// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Skip aix and plan9 for now: github.com/chzyer/readline doesn't support them.
// (https://golang.org/issue/32839)
//
//go:build !aix && !plan9
// +build !aix,!plan9

// The viewcore tool is a command-line tool for exploring the state of a Go process
// that has dumped core.
// Run "viewcore help" for a list of commands.
package main

import (
	"fmt"
	"io"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime/debug"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"unicode"

	"github.com/chzyer/readline"
	"github.com/spf13/cobra"
	"golang.org/x/debug/internal/core"
	"golang.org/x/debug/internal/gocore"
)

// Top-level command.
var cmdRoot = &cobra.Command{
	Use:   "viewcore <corefile>",
	Short: "viewcore is a set of tools for analyzing core dumped from Go process",
	Long: `
viewcore is a set of tools for analyzing core dumped from Go process.

The following command starts an interactive shell for analysis of the
specified core.

  viewcore <corefile>

When provided a command in the following form, viewcore invokes the
command directly rather than starting in interactive mode.

  viewcore <corefile> <command>

Example:

  viewcore mycore overview

For available analysis tools, run the following command.

  viewcore help
`,
	PersistentPreRun:  func(cmd *cobra.Command, args []string) { startProfile() },
	PersistentPostRun: func(cmd *cobra.Command, args []string) { endProfile() },

	Args: cobra.ExactArgs(0), // either empty, <corefile> or help <subcommand>
	Run:  runRoot,
}

// Subcommands
var (
	cmdOverview = &cobra.Command{
		Use:   "overview",
		Short: "print a few overall statistics",
		Args:  cobra.ExactArgs(0),
		Run:   runOverview,
	}

	cmdMappings = &cobra.Command{
		Use:   "mappings",
		Short: "print virtual memory mappings",
		Args:  cobra.ExactArgs(0),
		Run:   runMappings,
	}

	cmdGoroutines = &cobra.Command{
		Use:   "goroutines",
		Short: "list goroutines",
		Args:  cobra.ExactArgs(0),
		Run:   runGoroutines,
	}

	cmdHistogram = &cobra.Command{
		Use:     "histogram",
		Aliases: []string{"histo"},
		Short:   "print histogram of heap memory use by Go type",
		Long: "print histogram of heap memory use by Go type.\n" +
			"If N is specified, it will reports only the top N buckets\n" +
			"based on the total bytes.",
		Args: cobra.ExactArgs(0),
		Run:  runHistogram,
	}

	cmdBreakdown = &cobra.Command{
		Use:   "breakdown",
		Short: "print memory use by class",
		Args:  cobra.ExactArgs(0),
		Run:   runBreakdown,
	}

	cmdObjects = &cobra.Command{
		Use:   "objects",
		Short: "print a list of all live objects",
		Args:  cobra.ExactArgs(0),
		Run:   runObjects,
	}

	cmdObjgraph = &cobra.Command{
		Use:   "objgraph <output_filename>",
		Short: "dump object graph (dot)",
		Args:  cobra.ExactArgs(1),
		Run:   runObjgraph,
	}

	cmdObjref = &cobra.Command{
		Use:   "objref <output_filename>",
		Short: "dump object reference",
		Args:  cobra.ExactArgs(1),
		Run:   runObjref,
	}

	cmdReadObj = &cobra.Command{
		Use:   "readobj <address>",
		Short: "read a gc object from address",
		Args:  cobra.ExactArgs(1),
		Run:   runReadObj,
	}

	cmdReadEface = &cobra.Command{
		Use:   "readEface <address>",
		Short: "read a map from address",
		Args:  cobra.ExactArgs(1),
		Run:   runReadEface,
	}

	cmdReachable = &cobra.Command{
		Use:   "reachable <address>",
		Short: "find path from root to an object",
		Args:  cobra.ExactArgs(1),
		Run:   runReachable,
	}

	cmdHTML = &cobra.Command{
		Use:   "html",
		Short: "start an http server for browsing core file data on the port specified with -port",
		Args:  cobra.ExactArgs(0),
		Run:   runHTML,
	}

	cmdRead = &cobra.Command{
		Use:   "read <address> [<size>]",
		Short: "read a chunk of memory", // oh very helpful!
		Args:  cobra.RangeArgs(1, 2),
		Run:   runRead,
	}

	visitedNodes   = make(map[core.Address]*GCNode)
	allGCNodes     = make(map[core.Address]*GCNode)
	rootGCNodesMap = make(map[core.Address]*GCNode)
	rootGCNodes    = []*GCNode{}
)

type config struct {
	interactive bool

	// Set based on os.Args[1]
	corefile string

	// flags
	base    string
	exePath string
	cpuprof string // TODO: move to subcommand config.
}

var cfg config

func init() {
	cmdRoot.PersistentFlags().StringVar(&cfg.base, "base", "", "root directory to find core dump file references")
	cmdRoot.PersistentFlags().StringVar(&cfg.exePath, "exe", "", "main executable file")
	cmdRoot.PersistentFlags().StringVar(&cfg.cpuprof, "prof", "", "write cpu profile of viewcore to this file for viewcore's developers")

	// subcommand flags
	cmdHTML.Flags().IntP("port", "p", 8080, "port for http server")

	cmdHistogram.Flags().Int("top", 0, "reports only top N entries if N>0")

	cmdRoot.AddCommand(
		cmdOverview,
		cmdMappings,
		cmdGoroutines,
		cmdHistogram,
		cmdBreakdown,
		cmdObjects,
		cmdObjgraph,
		cmdObjref,
		cmdReadObj,
		cmdReadEface,
		cmdReachable,
		cmdHTML,
		cmdRead)

	// customize the usage template - viewcore's command structure
	// is not typical of cobra-based command line tool.
	cobra.AddTemplateFunc("viewcoreUseLine", useLine)
	cmdRoot.SetUsageTemplate(usageTmpl)
}

// useLine is like cobra.Command.UseLine but tweaked to use commandPath.
func useLine(c *cobra.Command) string {
	var useline string
	if c.HasParent() {
		useline = commandPath(c.Parent()) + " " + c.Use
	} else {
		useline = c.Use
	}
	if c.DisableFlagsInUseLine {
		return useline
	}
	if c.HasAvailableFlags() && !strings.Contains(useline, "[flags]") {
		useline += " [flags]"
	}
	return useline
}

// commandPath is like cobra.Command.CommandPath but tweaked to
// use c.Use instead of c.Name for the root command so it works
// with viewcore's unusual command structure.
func commandPath(c *cobra.Command) string {
	if c.HasParent() {
		return commandPath(c) + " " + c.Name()
	}
	return c.Use
}

const usageTmpl = `Usage:{{if .Runnable}}
  {{viewcoreUseLine .}}{{end}}{{if .HasAvailableSubCommands}}
  {{viewcoreUseLine .}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}

Available Commands:{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] != "help" && !strings.HasPrefix(args[0], "-") {
		cfg.corefile = args[0]
		args = args[1:]
	}
	cmdRoot.SetArgs(args)
	cmdRoot.Execute()
}

var coreCache = &struct {
	// copy of params used to generate p.
	cfg config

	coreP   *core.Process
	gocoreP *gocore.Process
	err     error
}{}

// readCore reads corefile and returns core and gocore process states.
func readCore() (*core.Process, *gocore.Process, error) {
	cc := coreCache
	if cc.cfg == cfg {
		return cc.coreP, cc.gocoreP, cc.err
	}
	c, err := core.Core(cfg.corefile, cfg.base, cfg.exePath)
	if err != nil {
		return nil, nil, err
	}
	p, err := gocore.Core(c)
	if os.IsNotExist(err) && cfg.exePath == "" {
		return nil, nil, fmt.Errorf("%v; consider specifying the --exe flag", err)
	}
	if err != nil {
		return nil, nil, err
	}
	for _, w := range c.Warnings() {
		fmt.Fprintf(os.Stderr, "WARNING: %s\n", w)
	}
	cc.cfg = cfg
	cc.coreP = c
	cc.gocoreP = p
	cc.err = nil
	return c, p, nil
}

func runRoot(cmd *cobra.Command, args []string) {
	if cfg.corefile == "" {
		cmd.Usage()
		return
	}
	// Interactive mode.
	cfg.interactive = true

	p, _, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}

	// Create a dummy root to run in shell.
	root := &cobra.Command{}
	// Make all subcommands of viewcore available in the shell.
	for _, subcmd := range cmd.Commands() {
		if subcmd.Name() == "help" {
			root.SetHelpCommand(subcmd)
			continue
		}
		root.AddCommand(subcmd)
	}
	// Also, add exit command to terminate the shell.
	root.AddCommand(&cobra.Command{
		Use:     "exit",
		Aliases: []string{"quit", "bye"},
		Short:   "exit from interactive mode",
		Run: func(*cobra.Command, []string) {
			os.Exit(0)
		},
	})

	rootCompleter := readline.NewPrefixCompleter()
	for _, child := range root.Commands() {
		cmdToCompleter(rootCompleter, child)
	}

	shell, err := readline.NewEx(&readline.Config{
		Prompt:       "(viewcore) ",
		AutoComplete: rootCompleter,
		EOFPrompt:    "\n",
	})
	if err != nil {
		panic(err)
	}
	defer shell.Close()

	// nice welcome message.
	fmt.Fprintln(shell.Terminal)
	if args := p.Args(); args != "" {
		fmt.Fprintf(shell.Terminal, "Core %q was generated by %q\n", cfg.corefile, args)
	}
	fmt.Fprintf(shell.Terminal, "Entering interactive mode (type 'help' for commands)\n")

	for {
		l, err := shell.Readline()
		if err != nil {
			if err != io.EOF {
				fmt.Printf("Error: %v\n", err)
			}
			break
		}

		err = capturePanic(func() {
			root.ResetFlags()
			root.SetArgs(strings.Fields(l))
			root.Execute()
		})
		if err != nil {
			fmt.Printf("Error while trying to run command %q: %v", l, err)
		}
	}
}

func capturePanic(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v\nStack: %s\n", r, debug.Stack())
		}
	}()

	fn()
	return nil
}

func cmdToCompleter(parent readline.PrefixCompleterInterface, c *cobra.Command) {
	completer := readline.PcItem(c.Name())
	parent.SetChildren(append(parent.GetChildren(), completer))
	for _, child := range c.Commands() {
		cmdToCompleter(completer, child)
	}
}

func runOverview(cmd *cobra.Command, args []string) {
	p, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}

	t := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
	fmt.Fprintf(t, "arch\t%s\n", p.Arch())
	fmt.Fprintf(t, "runtime\t%s\n", c.BuildVersion())
	var total int64
	for _, m := range p.Mappings() {
		total += m.Max().Sub(m.Min())
	}
	fmt.Fprintf(t, "memory\t%.1f MB\n", float64(total)/(1<<20))
	t.Flush()
}

func runMappings(cmd *cobra.Command, args []string) {
	p, _, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}
	t := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', tabwriter.AlignRight)
	fmt.Fprintf(t, "min\tmax\tperm\tsource\toriginal\t\n")
	for _, m := range p.Mappings() {
		perm := ""
		if m.Perm()&core.Read != 0 {
			perm += "r"
		} else {
			perm += "-"
		}
		if m.Perm()&core.Write != 0 {
			perm += "w"
		} else {
			perm += "-"
		}
		if m.Perm()&core.Exec != 0 {
			perm += "x"
		} else {
			perm += "-"
		}
		file, off := m.Source()
		fmt.Fprintf(t, "%x\t%x\t%s\t%s@%x\t", m.Min(), m.Max(), perm, file, off)
		if m.CopyOnWrite() {
			file, off = m.OrigSource()
			fmt.Fprintf(t, "%s@%x", file, off)
		}
		fmt.Fprintf(t, "\t\n")
	}
	t.Flush()
}

func runGoroutines(cmd *cobra.Command, args []string) {
	_, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}
	for _, g := range c.Goroutines() {
		fmt.Printf("G stacksize=%x\n", g.Stack())
		for _, f := range g.Frames() {
			pc := f.PC()
			entry := f.Func().Entry()
			var adj string
			switch {
			case pc == entry:
				adj = ""
			case pc < entry:
				adj = fmt.Sprintf("-%d", entry.Sub(pc))
			default:
				adj = fmt.Sprintf("+%d", pc.Sub(entry))
			}
			fmt.Printf("  %016x %016x %s%s\n", f.Min(), f.Max(), f.Func().Name(), adj)
		}
	}
}

func runHistogram(cmd *cobra.Command, args []string) {
	topN, err := cmd.Flags().GetInt("top")
	if err != nil {
		exitf("%v\n", err)
	}
	_, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}
	// Produce an object histogram (bytes per type).
	type bucket struct {
		name  string
		size  int64
		count int64
	}
	var buckets []*bucket
	m := map[string]*bucket{}
	c.ForEachObject(func(x gocore.Object) bool {
		name := typeName(c, x)
		b := m[name]
		if b == nil {
			b = &bucket{name: name, size: c.Size(x)}
			buckets = append(buckets, b)
			m[name] = b
		}
		b.count++
		return true
	})
	sort.Slice(buckets, func(i, j int) bool {
		return buckets[i].size*buckets[i].count > buckets[j].size*buckets[j].count
	})

	// report only top N if requested
	if topN > 0 && len(buckets) > topN {
		buckets = buckets[:topN]
	}

	t := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', tabwriter.AlignRight)
	fmt.Fprintf(t, "%s\t%s\t%s\t %s\n", "count", "size", "bytes", "type")
	for _, e := range buckets {
		fmt.Fprintf(t, "%d\t%d\t%d\t %s\n", e.count, e.size, e.count*e.size, e.name)
	}
	t.Flush()
}

func runBreakdown(cmd *cobra.Command, args []string) {
	_, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}
	t := tabwriter.NewWriter(os.Stdout, 0, 8, 1, ' ', tabwriter.AlignRight)
	all := c.Stats().Size
	var printStat func(*gocore.Stats, string)
	printStat = func(s *gocore.Stats, indent string) {
		comment := ""
		switch s.Name {
		case "bss":
			comment = "(grab bag, includes OS thread stacks, ...)"
		case "manual spans":
			comment = "(Go stacks)"
		case "retained":
			comment = "(kept for reuse by Go)"
		case "released":
			comment = "(given back to the OS)"
		}
		fmt.Fprintf(t, "%s\t%d\t%6.2f%%\t %s\n", fmt.Sprintf("%-20s", indent+s.Name), s.Size, float64(s.Size)*100/float64(all), comment)
		for _, c := range s.Children {
			printStat(c, indent+"  ")
		}
	}
	printStat(c.Stats(), "")
	t.Flush()
}

type GCRef struct {
	link string
	node *GCNode
}

type GCNode struct {
	addr core.Address
	name string
	size int64
	refs []*GCRef
}

func addUniqueChildNodes(nodes []*GCNode) {
	// width first visit
	cnodes := []*GCNode{}

	for _, node := range nodes {
		if _, ok := visitedNodes[node.addr]; !ok {
			panic("add children for not visited node")
			return
		}

		refs := allGCNodes[node.addr].refs // all refered child nodes
		for _, ref := range refs {
			if _, ok := visitedNodes[ref.node.addr]; ok {
				continue
			}
			newNode := nodeCopy(ref.node)
			newRef := &GCRef{
				node: newNode,
				link: ref.link,
			}

			node.refs = append(node.refs, newRef)
			visitedNodes[newNode.addr] = newNode

			cnodes = append(cnodes, newNode)
		}
	}
	if len(cnodes) > 0 {
		addUniqueChildNodes(cnodes)
	}
}

func (node *GCNode) appendChild(cNode *GCNode, link string) {
	ref := &GCRef{
		link: link,
		node: cNode,
	}
	node.refs = append(node.refs, ref)
}

func findOrCreateGCNode(name string, addr core.Address, size int64) *GCNode {
	if node, ok := allGCNodes[addr]; ok {
		if node.size != size {
			fmt.Fprintf(os.Stderr, "same address: %v, old size: %v, new size: %v\n", addr, node.size, size)
		}
		if node.name != name && strings.HasPrefix(node.name, "unk") && !strings.HasPrefix(name, "unk") {
			node.name = name
		}
		return node
	}
	node := &GCNode{
		name: name,
		addr: addr,
		size: size,
	}
	allGCNodes[addr] = node
	return node
}

func checkGCNodeCreated(name string, addr core.Address, size int64) *GCNode {
	if _, ok := allGCNodes[addr]; !ok {
		// TODO
		// fmt.Fprintf(os.Stderr, "uncreated gc node, address: %v, name: %v, size: %v\n", addr, name, size)
	}
	return findOrCreateGCNode(name, addr, size)
}

func calcTreeSize(node *GCNode) int64 {
	// fmt.Printf("node: %x\n", node.addr)
	size := int64(0)
	for _, ref := range node.refs {
		size += calcTreeSize(ref.node)
	}
	node.size = size + node.size
	return node.size
}

func nodeCopy(node *GCNode) *GCNode {
	newNode := *node
	newNode.refs = []*GCRef{}
	return &newNode
}

func addGCRoot(node *GCNode, delay bool) {
	if n, ok := rootGCNodesMap[node.addr]; ok {
		// there may have empty slices
		if node.size == n.size && n.size == 0 {
			return
		}
		err := fmt.Sprintf("duplicated GC root node: %v, existing: %v", node, n)
		panic(err)
	}

	n := nodeCopy(node)
	if !delay {
		rootGCNodes = append(rootGCNodes, n)
		rootGCNodesMap[node.addr] = node
	}

	// mark visited for root node
	visitedNodes[n.addr] = n
}

func findObject(addr core.Address, c *gocore.Process) gocore.Object {
	var target gocore.Object
	c.ForEachObject(func(x gocore.Object) bool {
		if c.Addr(x) == addr {
			target = x
			return false
		}
		return true
	})
	return target
}

func runReadEface(cmd *cobra.Command, args []string) {
	p, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}
	arg, err := strconv.ParseInt(args[0], 16, 64)
	if err != nil {
		exitf("invalid address %v, err: %v\n", args[0], err)
	}
	addr := core.Address(uint64(arg))
	target := findObject(addr, c)
	if target == 0 {
		fmt.Println("not found")
		return
	}
	fmt.Printf("found target Eface: 0x%x, type: %v, size: %v\n", c.Addr(target), typeName(c, target), c.Size(target))
	typ, repeat := c.Type(target)
	if typ != nil {
		fmt.Printf("type, name: %v, repeat: %v\n", typ.Name, repeat)
	} else {
		fmt.Printf("no type found\n")
	}
	gocore.ReadEface(c.Addr(target), typ, p, c, 1)
}

func runReadObj(cmd *cobra.Command, args []string) {
	go func() {
		http.ListenAndServe(":6060", nil)
	}()
	p, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}
	arg, err := strconv.ParseInt(args[0], 16, 64)
	if err != nil {
		exitf("invalid address %v, err: %v\n", args[0], err)
	}
	addr := core.Address(uint64(arg))
	var target gocore.Object
	c.ForEachObject(func(x gocore.Object) bool {
		if c.Addr(x) == addr {
			target = x
			return false
		}
		return true
	})
	if target == 0 {
		fmt.Println("not found")
		return
	}

	typ, repeat := c.Type(target)
	fmt.Printf("found target: 0x%x, repeat: %v, size: %v\n", c.Addr(target), repeat, c.Size(target))

	if typ == nil {
		fmt.Printf("not found type\n")
		gocore.ReadEface(addr, typ, p, c, 1)
	} else {
		fmt.Printf("type, name: %v, kind: %v\n", typ.Name, typ.Kind)
		gocore.ReadObj(addr, typ, p, c, 1)
	}

	/*
		if !strings.HasPrefix(typ.Name, "hash<") {
			fmt.Printf("not hash type")
			return
		}

		var bPtr core.Address
		var bTyp *gocore.Type
		var n int64
		for _, f := range typ.Fields {
			if f.Name == "buckets" {
				bPtr = p.ReadPtr(addr.Add(f.Off))
				bTyp = f.Type.Elem
			}
			if f.Name == "B" {
				n = int64(1) << p.ReadUint8(addr.Add(f.Off))
			}
		}
		fmt.Printf("buckets, addr: 0x%x, type name: %v, kind: %v, num: %v\n", bPtr, bTyp.Name, bTyp.Kind, n)

		var keyPtr, valuePtr core.Address
		var keyTyp, valueTyp *gocore.Type
		for _, f := range bTyp.Fields {
			if f.Name == "keys" {
				keyPtr = bPtr.Add(f.Off)
				keyTyp = f.Type
			}
			if f.Name == "values" {
				valuePtr = bPtr.Add(f.Off)
				valueTyp = f.Type
			}
		}
		for j := int64(0); j < n; j++ {
			nKeyPtr := keyPtr.Add(bTyp.Size * j)
			nValuePtr := valuePtr.Add(bTyp.Size * j)

			fmt.Printf("n: %d, keys, addr: 0x%x, type name: %v, kind: %v, num: %v\n", j+1, nKeyPtr, keyTyp.Name, keyTyp.Kind, keyTyp.Count)
			fmt.Printf("n: %d, values, addr: 0x%x, type name: %v, kind: %v, num: %v\n", j+1, nValuePtr, valueTyp.Name, valueTyp.Kind, valueTyp.Count)

			for i := int64(0); i < keyTyp.Count; i++ {
				fmt.Printf("key %v:", j*8+i+1)
				gocore.ReadEface(nKeyPtr.Add(keyTyp.Elem.Size*i), keyTyp.Elem, p, c)
			}
		}
	*/
}

func runObjref(cmd *cobra.Command, args []string) {
	_, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}
	/*
		for _, r := range c.Globals() {
			size := c.Size(gocore.Object(r.Addr))
			fmt.Println("global addr: ", r.Addr, ", ", r.Name, ", ", size)
		}
	*/

	sumObjSize := int64(0)
	c.ForEachObject(func(x gocore.Object) bool {
		sumObjSize += c.Size(x)
		xNode := findOrCreateGCNode(typeName(c, x), c.Addr(x), c.Size(x))
		c.ForEachPtr(x, func(i int64, y gocore.Object, j int64) bool {
			yNode := findOrCreateGCNode(typeName(c, y), c.Addr(y), c.Size(y))
			xNode.appendChild(yNode, fieldName(c, x, i))
			return true
		})
		return true
	})
	fmt.Fprintf(os.Stderr, "sum object size %v\n", sumObjSize)

	// just check all root gc nodes and their children nodes have been created
	for _, r := range c.Globals() {
		size := c.Size(gocore.Object(r.Addr))
		rNode := checkGCNodeCreated(r.Name, r.Addr, size)
		addGCRoot(rNode, false)

		c.ForEachRootPtr(r, func(i int64, y gocore.Object, j int64) bool {
			cNode := checkGCNodeCreated(typeName(c, y), c.Addr(y), c.Size(y))
			rNode.appendChild(cNode, typeFieldName(r.Type, i))
			return true
		})
	}
	for _, g := range c.Goroutines() {
		gName := fmt.Sprintf("go%x", g.Addr())
		gNode := checkGCNodeCreated(gName, g.Addr(), c.Size(gocore.Object(g.Addr())))
		addGCRoot(gNode, true)

		for fi, f := range g.Frames() {
			fNode := findOrCreateGCNode(f.Func().Name(), f.Max(), 0)
			gNode.appendChild(fNode, fmt.Sprintf("frame-%d", fi))
			for ri, r := range f.Roots() {
				rNode := checkGCNodeCreated(r.Name, r.Addr, c.Size(gocore.Object(r.Addr)))
				// really useful?
				fNode.appendChild(rNode, fmt.Sprintf("local-var-%d", ri))

				c.ForEachRootPtr(r, func(i int64, y gocore.Object, j int64) bool {
					cNode := checkGCNodeCreated(typeName(c, y), c.Addr(y), c.Size(y))
					rNode.appendChild(cNode, typeFieldName(r.Type, i))
					return true
				})
			}
		}
	}

	// unique ref: only need one ref for one gc obj
	// order: globals first
	addUniqueChildNodes(rootGCNodes)
	for _, g := range c.Goroutines() {
		gName := fmt.Sprintf("go%x", g.Addr())
		gNode := checkGCNodeCreated(gName, g.Addr(), c.Size(gocore.Object(g.Addr())))
		addGCRoot(gNode, false)
	}
	// next, goroutines
	addUniqueChildNodes(rootGCNodes)

	allObjSize := int64(0)
	for _, node := range allGCNodes {
		allObjSize += node.size
	}
	fmt.Fprintf(os.Stderr, "all object size %v\n", allObjSize)

	total := int64(0)
	for _, rNode := range rootGCNodes {
		total += calcTreeSize(rNode)
	}
	fmt.Fprintf(os.Stderr, "total size %v\n", total)

	fname := args[0]
	// Dump object graph to output file.
	w, err := os.Create(fname)
	if err != nil {
		panic(err)
	}
	debug := false
	if debug {
		for _, node := range allGCNodes {
			var childs []string
			for _, ref := range node.refs {
				childs = append(childs, fmt.Sprintf("0x%x", ref.node.addr))
			}
			fmt.Fprintf(w, "0x%x(%v): %s\n", node.addr, node.size, strings.Join(childs, ", "))
		}
	} else {
		var path []string

		printedSize := int64(0)
		for _, rNode := range rootGCNodes {
			printedSize += printRefPath(w, path, total, rNode)
		}
		fmt.Fprintf(os.Stderr, "printed size: %v\n", printedSize)
	}
	w.Close()
	fmt.Fprintf(os.Stderr, "wrote the object reference to %q\n", fname)
}

func genRefPath(slice []string) string {
	// 1. reverse order.
	var reverse = make([]string, len(slice))

	for index, value := range slice {
		// 2. remove unprintable
		newValue := strings.Map(func(r rune) rune {
			if r == '+' || r == '?' {
				return '.'
			}
			if unicode.IsPrint(r) {
				return r
			}
			return -1
		}, value)
		reverse[len(reverse)-1-index] = newValue
	}

	return strings.Join(reverse, "\n")
}

var minPrintRatio = float64(0.0001)

// return the printed size
func printRefPath(w *os.File, path []string, total int64, node *GCNode) int64 {
	if float64(node.size)/float64(total) < minPrintRatio {
		return 0
	}
	printedSize := int64(0)
	path = append(path, fmt.Sprintf("%v 0x%x", node.name, node.addr))
	for _, ref := range node.refs {
		rPath := path
		if ref.link != "" {
			rPath = append(rPath, ref.link)
		}
		printedSize += printRefPath(w, rPath, total, ref.node)
	}
	if float64(node.size-printedSize)/float64(total) < minPrintRatio {
		return printedSize
	}
	// fmt.Printf("%v\n\t%d\n", strings.Join(path, "\n"), node.size)
	ref := genRefPath(path)
	fmt.Fprintf(w, "%v\n\t%d\n", ref, node.size-printedSize)

	return node.size
}

func runObjgraph(cmd *cobra.Command, args []string) {
	_, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}

	fname := args[0]

	// Dump object graph to output file.
	w, err := os.Create(fname)
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(w, "digraph {\n")
	for k, r := range c.Globals() {
		printed := false
		c.ForEachRootPtr(r, func(i int64, y gocore.Object, j int64) bool {
			if !printed {
				fmt.Fprintf(w, "r%d [label=\"%s\n%s\",shape=hexagon]\n", k, r.Name, r.Type)
				printed = true
			}
			fmt.Fprintf(w, "r%d -> o%x [label=\"%s\"", k, c.Addr(y), typeFieldName(r.Type, i))
			if j != 0 {
				fmt.Fprintf(w, " ,headlabel=\"+%d\"", j)
			}
			fmt.Fprintf(w, "]\n")
			return true
		})
	}
	for _, g := range c.Goroutines() {
		last := fmt.Sprintf("o%x", g.Addr())
		for _, f := range g.Frames() {
			frame := fmt.Sprintf("f%x", f.Max())
			fmt.Fprintf(w, "%s [label=\"%s\",shape=rectangle]\n", frame, f.Func().Name())
			fmt.Fprintf(w, "%s -> %s [style=dotted]\n", last, frame)
			last = frame
			for _, r := range f.Roots() {
				c.ForEachRootPtr(r, func(i int64, y gocore.Object, j int64) bool {
					fmt.Fprintf(w, "%s -> o%x [label=\"%s%s\"", frame, c.Addr(y), r.Name, typeFieldName(r.Type, i))
					if j != 0 {
						fmt.Fprintf(w, " ,headlabel=\"+%d\"", j)
					}
					fmt.Fprintf(w, "]\n")
					return true
				})
			}
		}
	}
	c.ForEachObject(func(x gocore.Object) bool {
		addr := c.Addr(x)
		size := c.Size(x)
		fmt.Fprintf(w, "o%x [label=\"%s\\n%d\"]\n", addr, typeName(c, x), size)
		c.ForEachPtr(x, func(i int64, y gocore.Object, j int64) bool {
			fmt.Fprintf(w, "o%x -> o%x [label=\"%s\"", addr, c.Addr(y), fieldName(c, x, i))
			if j != 0 {
				fmt.Fprintf(w, ",headlabel=\"+%d\"", j)
			}
			fmt.Fprintf(w, "]\n")
			return true
		})
		return true
	})
	fmt.Fprintf(w, "}")
	w.Close()
	fmt.Fprintf(os.Stderr, "wrote the object graph to %q\n", fname)
}

func runObjects(cmd *cobra.Command, args []string) {
	_, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}
	c.ForEachObject(func(x gocore.Object) bool {
		fmt.Printf("%16x %s\n", c.Addr(x), typeName(c, x))
		return true
	})

}

func runReachable(cmd *cobra.Command, args []string) {
	_, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}
	n, err := strconv.ParseInt(args[0], 16, 64)
	if err != nil {
		exitf("can't parse %q as an object address\n", args[1])
	}
	a := core.Address(n)
	obj, _ := c.FindObject(a)
	if obj == 0 {
		exitf("can't find object at address %s\n", args[1])
	}

	// Breadth-first search backwards until we reach a root.
	type hop struct {
		i int64         // offset in "from" object (the key in the path map) where the pointer is
		x gocore.Object // the "to" object
		j int64         // the offset in the "to" object
	}
	depth := map[gocore.Object]int{}
	depth[obj] = 0
	q := []gocore.Object{obj}
	done := false
	for !done {
		if len(q) == 0 {
			panic("can't find a root that can reach the object")
		}
		y := q[0]
		q = q[1:]
		c.ForEachReversePtr(y, func(x gocore.Object, r *gocore.Root, i, j int64) bool {
			if r != nil {
				// found it.
				if r.Frame == nil {
					// Print global
					fmt.Printf("%s", r.Name)
				} else {
					// Print stack up to frame in question.
					var frames []*gocore.Frame
					for f := r.Frame.Parent(); f != nil; f = f.Parent() {
						frames = append(frames, f)
					}
					for k := len(frames) - 1; k >= 0; k-- {
						fmt.Printf("%s\n", frames[k].Func().Name())
					}
					// Print frame + variable in frame.
					fmt.Printf("%s.%s", r.Frame.Func().Name(), r.Name)
				}
				fmt.Printf("%s → \n", typeFieldName(r.Type, i))

				z := y
				for {
					fmt.Printf("%x %s", c.Addr(z), typeName(c, z))
					if z == obj {
						fmt.Println()
						break
					}
					// Find an edge out of z which goes to an object
					// closer to obj.
					c.ForEachPtr(z, func(i int64, w gocore.Object, j int64) bool {
						if d, ok := depth[w]; ok && d < depth[z] {
							fmt.Printf(" %s → %s", objField(c, z, i), objRegion(c, w, j))
							z = w
							return false
						}
						return true
					})
					fmt.Println()
				}
				done = true
				return false
			}
			if _, ok := depth[x]; ok {
				// we already found a shorter path to this object.
				return true
			}
			depth[x] = depth[y] + 1
			q = append(q, x)
			return true
		})
	}
}

// httpServer is the singleton http server, initialized by
// the first call to runHTML.
var httpServer struct {
	sync.Mutex
	port int
}

func runHTML(cmd *cobra.Command, args []string) {
	httpServer.Lock()
	defer httpServer.Unlock()
	if httpServer.port != 0 {
		fmt.Printf("already serving on http://localhost:%d\n", httpServer.port)
		return
	}
	_, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}

	port, err := cmd.Flags().GetInt("port")
	if err != nil {
		exitf("%v\n", err)
	}
	serveHTML(c, port, cfg.interactive)
	httpServer.port = port
	// TODO: launch web browser
}

func runRead(cmd *cobra.Command, args []string) {
	p, _, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}
	n, err := strconv.ParseInt(args[0], 16, 64)
	if err != nil {
		exitf("can't parse %q as an object address\n", args[1])
	}
	a := core.Address(n)
	if len(args) < 2 {
		n = 256
	} else {
		n, err = strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			exitf("can't parse %q as a byte count\n", args[2])
		}
	}
	if !p.ReadableN(a, n) {
		exitf("address range [%x,%x] not readable\n", a, a.Add(n))
	}
	b := make([]byte, n)
	p.ReadAt(b, a)
	for i, x := range b {
		if i%16 == 0 {
			if i > 0 {
				fmt.Println()
			}
			fmt.Printf("%x:", a.Add(int64(i)))
		}
		fmt.Printf(" %02x", x)
	}
	fmt.Println()
}

// typeName returns a string representing the type of this object.
func typeName(c *gocore.Process, x gocore.Object) string {
	size := c.Size(x)
	typ, repeat := c.Type(x)
	if typ == nil {
		return fmt.Sprintf("unk#0x%x", x)
	}
	name := typ.String()
	n := size / typ.Size
	if n > 1 {
		if repeat < n {
			name = fmt.Sprintf("[%d+%d?]%s", repeat, n-repeat, name)
		} else {
			name = fmt.Sprintf("[%d]%s", repeat, name)
		}
	}
	return name + fmt.Sprintf(" 0x%x", x)
}

// fieldName returns the name of the field at offset off in x.
func fieldName(c *gocore.Process, x gocore.Object, off int64) string {
	size := c.Size(x)
	typ, repeat := c.Type(x)
	if typ == nil {
		return fmt.Sprintf("f%d", off)
	}
	n := size / typ.Size
	i := off / typ.Size
	if i == 0 && repeat == 1 {
		// Probably a singleton object, no need for array notation.
		return typeFieldName(typ, off)
	}
	if i >= n {
		// Partial space at the end of the object - the type can't be complete.
		return fmt.Sprintf("f%d", off)
	}
	q := ""
	if i >= repeat {
		// Past the known repeat section, add a ? because we're not sure about the type.
		q = "?"
	}
	return fmt.Sprintf("[%d]%s%s", i, typeFieldName(typ, off-i*typ.Size), q)
}

// typeFieldName returns the name of the field at offset off in t.
func typeFieldName(t *gocore.Type, off int64) string {
	switch t.Kind {
	case gocore.KindBool, gocore.KindInt, gocore.KindUint, gocore.KindFloat:
		return ""
	case gocore.KindComplex:
		if off == 0 {
			return ".real"
		}
		return ".imag"
	case gocore.KindIface, gocore.KindEface:
		if off == 0 {
			return ".type"
		}
		return ".data"
	case gocore.KindPtr, gocore.KindFunc:
		return ""
	case gocore.KindString:
		if off == 0 {
			return ".ptr"
		}
		return ".len"
	case gocore.KindSlice:
		if off == 0 {
			return ".ptr"
		}
		if off <= t.Size/2 {
			return ".len"
		}
		return ".cap"
	case gocore.KindArray:
		s := t.Elem.Size
		i := off / s
		return fmt.Sprintf("[%d]%s", i, typeFieldName(t.Elem, off-i*s))
	case gocore.KindStruct:
		for _, f := range t.Fields {
			if f.Off <= off && off < f.Off+f.Type.Size {
				return "." + f.Name + typeFieldName(f.Type, off-f.Off)
			}
		}
	}
	return ".???"
}

func startProfile() {
	if cfg.cpuprof != "" {
		f, err := os.Create(cfg.cpuprof)
		if err != nil {
			fmt.Fprintf(os.Stderr, "can't open profile file: %s\n", err)
			os.Exit(2)
		}
		pprof.StartCPUProfile(f)

	}
}

func endProfile() {
	if cfg.cpuprof != "" {
		pprof.StopCPUProfile()
	}
}

func exitf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format, args...)
	os.Exit(1)
}
