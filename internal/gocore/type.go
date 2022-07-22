// Copyright 2017 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gocore

import (
	"fmt"
	"golang.org/x/debug/internal/core"
	"regexp"
	"strings"
)

// A Type is the representation of the type of a Go object.
// Types are not necessarily canonical.
// Names are opaque; do not depend on the format of the returned name.
type Type struct {
	Name string
	Size int64
	Kind Kind

	// Fields only valid for a subset of kinds.
	Count  int64   // for kind == KindArray
	Elem   *Type   // for kind == Kind{Ptr,Array,Slice,String}. nil for unsafe.Pointer. Always uint8 for KindString.
	Fields []Field // for kind == KindStruct
}

type Kind uint8

const (
	KindNone Kind = iota
	KindBool
	KindInt
	KindUint
	KindFloat
	KindComplex
	KindArray
	KindPtr // includes chan, map, unsafe.Pointer
	KindIface
	KindEface
	KindSlice
	KindString
	KindStruct
	KindFunc
)

func (k Kind) String() string {
	return [...]string{
		"KindNone",
		"KindBool",
		"KindInt",
		"KindUint",
		"KindFloat",
		"KindComplex",
		"KindArray",
		"KindPtr",
		"KindIface",
		"KindEface",
		"KindSlice",
		"KindString",
		"KindStruct",
		"KindFunc",
	}[k]
}

// A Field represents a single field of a struct type.
type Field struct {
	Name string
	Off  int64
	Type *Type
}

func (t *Type) String() string {
	return t.Name
}

func (t *Type) field(name string) *Field {
	if t.Kind != KindStruct {
		panic("asking for field of non-struct")
	}
	for i := range t.Fields {
		f := &t.Fields[i]
		if f.Name == name {
			return f
		}
	}
	return nil
}

// DynamicType returns the concrete type stored in the interface type t at address a.
// If the interface is nil, returns nil.
func (p *Process) DynamicType(t *Type, a core.Address) *Type {
	switch t.Kind {
	default:
		panic("asking for the dynamic type of a non-interface")
	case KindEface:
		x := p.proc.ReadPtr(a)
		if x == 0 {
			return nil
		}
		return p.runtimeType2Type(x, a.Add(p.proc.PtrSize()))
	case KindIface:
		x := p.proc.ReadPtr(a)
		if x == 0 {
			return nil
		}
		// Read type out of itab.
		x = p.proc.ReadPtr(x.Add(p.proc.PtrSize()))
		return p.runtimeType2Type(x, a.Add(p.proc.PtrSize()))
	}
}

func readNameLen(p *Process, a core.Address) (int64, int64) {
	if p.minorVersion >= 17 {
		v := 0
		for i := 0; ; i++ {
			x := p.proc.ReadUint8(a.Add(int64(i + 1)))
			v += int(x&0x7f) << (7 * i)
			if x&0x80 == 0 {
				return int64(i + 1), int64(v)
			}
		}
	} else {
		n1 := p.proc.ReadUint8(a.Add(1))
		n2 := p.proc.ReadUint8(a.Add(2))
		n := uint16(n1)<<8 + uint16(n2)
		return 2, int64(n)
	}
}

// Convert the address of a runtime._type to a *Type.
// Guaranteed to return a non-nil *Type.
func (p *Process) runtimeType2Type(a core.Address, d core.Address) *Type {
	if t := p.runtimeMap[a]; t != nil {
		return t
	}

	if a == 21696736 {
		fmt.Println("hit")
	}

	// Read runtime._type.size
	r := region{p: p, a: a, typ: p.findType("runtime._type")}
	size := int64(r.Field("size").Uintptr())

	// Find module this type is in.
	var m *module
	for _, x := range p.modules {
		if x.types <= a && a < x.etypes {
			m = x
			break
		}
	}

	// Read information out of the runtime._type.
	var name string
	if m != nil {
		x := m.types.Add(int64(r.Field("str").Int32()))
		n1 := p.proc.ReadUint8(x.Add(1))
		n2 := p.proc.ReadUint8(x.Add(2))
		n := uint16(n1)<<8 + uint16(n2)
		b := make([]byte, n)
		p.proc.ReadAt(b, x.Add(3))
		name = string(b)
		if r.Field("tflag").Uint8()&uint8(p.rtConstants["tflagExtraStar"]) != 0 {
			name = name[1:]
		}
	} else {
		// A reflect-generated type.
		// TODO: The actual name is in the runtime.reflectOffs map.
		// Too hard to look things up in maps here, just allocate a placeholder for now.
		name = fmt.Sprintf("reflect.generatedType%x", a)
	}
	if m != nil {
		x := m.types.Add(int64(r.Field("str").Int32()))
		i, l := readNameLen(p, x)
		b := make([]byte, l)
		p.proc.ReadAt(b, x.Add(i+1))
		name = string(b)
		if r.Field("tflag").Uint8()&uint8(p.rtConstants["tflagExtraStar"]) != 0 {
			name = name[1:]
		}
	}

	// Read ptr/nonptr bits
	ptrSize := p.proc.PtrSize()
	nptrs := int64(r.Field("ptrdata").Uintptr()) / ptrSize

	// hack nptrs, it may be very big, no reason yet.
	if nptrs > 1024*10 {
		nptrs = 10
	}

	var ptrs []int64
	if r.Field("kind").Uint8()&uint8(p.rtConstants["kindGCProg"]) == 0 {
		gcdata := r.Field("gcdata").Address()
		for i := int64(0); i < nptrs; i++ {
			if p.proc.ReadUint8(gcdata.Add(i/8))>>uint(i%8)&1 != 0 {
				ptrs = append(ptrs, i*ptrSize)
			}
		}
	} else {
		// TODO: run GC program to get ptr indexes
	}

	// Find a Type that matches this type.
	// (The matched type will be one constructed from DWARF info.)
	// It must match name, size, and pointer bits.
	var candidates []*Type
	for _, t := range p.runtimeNameMap[name] {
		if size == t.Size && equal(ptrs, t.ptrs()) {
			candidates = append(candidates, t)
		}
	}
	/*
		if len(candidates) == 0 {
			name1 := strings.TrimPrefix(name, "*")
			if _, ok := SymbolNameMap[name]; ok {
				for key, list := range p.runtimeNameMap {
					if strings.HasPrefix(key, "*gitlab.alipay-ant-") && strings.HasSuffix(key, name1) {
						for _, t := range list {
							if len(ptrs) > 0 && size == t.Size && equal(ptrs, t.ptrs()) {
								fmt.Printf("Match suffix, %v => %v\n", key, name)
								candidates = append(candidates, t)
							}
						}
					}
				}
			}
		}
	*/
	var t *Type
	if len(candidates) > 0 {
		// If a runtime type matches more than one DWARF type,
		// pick one arbitrarily.
		// This looks mostly harmless. DWARF has some redundant entries.
		// For example, [32]uint8 appears twice.
		// TODO: investigate the reason for this duplication.

		if len(candidates) > 1 {
			for i, t := range candidates {
				fmt.Printf("#%d, typPtr, %v, name: %v, kind: %v, size: %v\n", i, a, t.Name, t.Kind, t.Size)
			}

			// got two candicates, ptr types.
			if candidates[0].Size == ptrSize && nptrs == 1 {
				o := p.proc.ReadPtr(d)
				size := p.Size(Object(o))
				var tmp []*Type
				for _, t := range candidates {
					if t.Elem != nil && t.Elem.Size == size {
						tmp = append(tmp, t)
					}
				}
				if len(tmp) == 1 {
					fmt.Printf("reduced candidates by matching elem object size\n")
				}
				if len(tmp) > 0 {
					candidates = tmp
				}
			}
		}
		t = candidates[0]

	} else {
		// There's no corresponding DWARF type.  Make our own.
		t = &Type{Name: name, Size: size, Kind: KindStruct}
		n := t.Size / ptrSize

		// Types to use for ptr/nonptr fields of runtime types which
		// have no corresponding DWARF type.
		ptr := p.findType("unsafe.Pointer")
		nonptr := p.findType("uintptr")
		if ptr == nil || nonptr == nil {
			panic("ptr / nonptr standins missing")
		}

		for i := int64(0); i < n; i++ {
			typ := nonptr
			if len(ptrs) > 0 && ptrs[0] == i*ptrSize {
				typ = ptr
				ptrs = ptrs[1:]
			}
			t.Fields = append(t.Fields, Field{
				Name: fmt.Sprintf("f%d", i),
				Off:  i * ptrSize,
				Type: typ,
			})

		}
		if t.Size%ptrSize != 0 {
			// TODO: tail of <ptrSize data.
		}
	}
	if t.Name == "*http.connPool" {
		fmt.Printf("type, kind: %v\n", t.Kind)
	}

	if t.Name == "*gitlab.alipay-inc.com/ant-mesh/mosn/vendor/mosn.io/mosn/pkg/stream/http.httpBuffers" ||
		t.Name == "*gitlab.alipay-inc.com/ant-mesh/mosn/vendor/mosn.io/mosn/pkg/stream/http.serverStream" {
		l := p.runtimeNameMap[name]
		fmt.Printf("type, kind: %v, l.len: %d\n", t.Kind, len(l))
	}
	// Memoize.
	p.runtimeMap[a] = t

	return t
}

// ptrs returns a sorted list of pointer offsets in t.
func (t *Type) ptrs() []int64 {
	return t.ptrs1(nil, 0)
}
func (t *Type) ptrs1(s []int64, off int64) []int64 {
	switch t.Kind {
	case KindPtr, KindFunc, KindSlice, KindString:
		s = append(s, off)
	case KindIface, KindEface:
		s = append(s, off, off+t.Size/2)
	case KindArray:
		if t.Count > 10000 {
			// Be careful about really large types like [1e9]*byte.
			// To process such a type we'd make a huge ptrs list.
			// The ptrs list here is only used for matching
			// a runtime type with a dwarf type, and for making
			// fields for types with no dwarf type.
			// Both uses can fail with no terrible repercussions.
			// We still will scan the whole object during markObjects, for example.
			// TODO: make this more robust somehow.
			break
		}
		for i := int64(0); i < t.Count; i++ {
			s = t.Elem.ptrs1(s, off)
			off += t.Elem.Size
		}
	case KindStruct:
		for _, f := range t.Fields {
			s = f.Type.ptrs1(s, off+f.Off)
		}
	default:
		// no pointers
	}
	return s
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i, x := range a {
		if x != b[i] {
			return false
		}
	}
	return true
}

// A typeInfo contains information about the type of an object.
// A slice of these hold the results of typing the heap.
type typeInfo struct {
	// This object has an effective type of [r]t.
	// Parts of the object beyond the first r*t.Size bytes have unknown type.
	// If t == nil, the type is unknown. (TODO: provide access to ptr/nonptr bits in this case.)
	t *Type
	r int64
}

// A typeChunk records type information for a portion of an object.
// Similar to a typeInfo, but it has an offset so it can be used for interior typings.
type typeChunk struct {
	off int64
	t   *Type
	r   int64
}

func (c typeChunk) min() int64 {
	return c.off
}
func (c typeChunk) max() int64 {
	return c.off + c.r*c.t.Size
}
func (c typeChunk) size() int64 {
	return c.r * c.t.Size
}
func (c typeChunk) matchingAlignment(d typeChunk) bool {
	if c.t != d.t {
		panic("can't check alignment of differently typed chunks")
	}
	return (c.off-d.off)%c.t.Size == 0
}

func (c typeChunk) merge(d typeChunk) typeChunk {
	t := c.t
	if t != d.t {
		panic("can't merge chunks with different types")
	}
	size := t.Size
	if (c.off-d.off)%size != 0 {
		panic("can't merge poorly aligned chunks")
	}
	min := c.min()
	max := c.max()
	if max < d.min() || min > d.max() {
		panic("can't merge chunks which don't overlap or abut")
	}
	if x := d.min(); x < min {
		min = x
	}
	if x := d.max(); x > max {
		max = x
	}
	return typeChunk{off: min, t: t, r: (max - min) / size}
}
func (c typeChunk) String() string {
	return fmt.Sprintf("%x[%d]%s", c.off, c.r, c.t)
}

// typeHeap tries to label all the heap objects with types.
func (p *Process) typeHeap() {
	p.initTypeHeap.Do(func() {
		// Type info for the start of each object. a.k.a. "0 offset" typings.
		p.types = make([]typeInfo, p.nObj)

		// Type info for the interior of objects, a.k.a. ">0 offset" typings.
		// Type information is arranged in chunks. Chunks are stored in an
		// arbitrary order, and are guaranteed to not overlap. If types are
		// equal, chunks are also guaranteed not to abut.
		// Interior typings are kept separate because they hopefully are rare.
		// TODO: They aren't really that rare. On some large heaps I tried
		// ~50% of objects have an interior pointer into them.
		// Keyed by object index.
		interior := map[int][]typeChunk{}

		// Typings we know about but haven't scanned yet.
		type workRecord struct {
			a core.Address
			t *Type
			r int64
		}
		var work []workRecord

		appendWork := func(w workRecord) {
			if w.a == 825365401144 {
				fmt.Printf("hit L2\n")
			}
			work = append(work, w)
		}

		// add records the fact that we know the object at address a has
		// r copies of type t.
		add := func(a core.Address, t *Type, r int64) {
			// fmt.Printf("add, address: 0x%x, type name: %v, type kind: %v\n", a, t.Name, t.Kind)
			/*
				addr := fmt.Sprintf("0x%x", a)
				if addr == "0xc000096090" || addr == "0xc0000980d0" || addr == "0xc00000e010" {
					fmt.Printf("foo\n")
				}
				if addr == "0xc008c2a068" {
					fmt.Printf("bar\n")
				}
			*/
			if a == 0xc000d94000 {
				fmt.Printf("hit\n")
			}
			if a == 0xc320980000 {
				fmt.Printf("hit unk\n")
			}
			if a == 0 { // nil pointer
				return
			}
			i, off := p.findObjectIndex(a)
			if i < 0 { // pointer doesn't point to an object in the Go heap
				return
			}
			if a == 0xc05b71aa80 {
				fmt.Printf("hit\n")
			}
			if off == 0 {
				obj, _ := p.FindObject(a)
				objSize := p.Size(obj)
				if r*t.Size > objSize {
					fmt.Printf("ERROR: calculated size(%d * %d = %d) bigger than object size(%d), skipping it ...\n", r, t.Size, r*t.Size, objSize)
					return
				}
				// We have a 0-offset typing. Replace existing 0-offset typing
				// if the new one is larger.
				ot := p.types[i].t
				or := p.types[i].r
				if ot == nil || r*t.Size > or*ot.Size {
					if t == ot {
						// Scan just the new section.
						appendWork(workRecord{
							a: a.Add(or * ot.Size),
							t: t,
							r: r - or,
						})
					} else {
						// Rescan the whole typing using the updated type.
						appendWork(workRecord{
							a: a,
							t: t,
							r: r,
						})
					}
					p.types[i].t = t
					p.types[i].r = r
				}
				return
			}

			// Add an interior typing to object #i.
			c := typeChunk{off: off, t: t, r: r}

			// Merge the given typing into the chunks we already know.
			// TODO: this could be O(n) per insert if there are lots of internal pointers.
			chunks := interior[i]
			newchunks := chunks[:0]
			addWork := true
			for _, d := range chunks {
				if c.max() <= d.min() || c.min() >= d.max() {
					// c does not overlap with d.
					if c.t == d.t && (c.max() == d.min() || c.min() == d.max()) {
						// c and d abut and share the same base type. Merge them.
						c = c.merge(d)
						continue
					}
					// Keep existing chunk d.
					// Overwrites chunks slice, but we're only merging chunks so it
					// can't overwrite to-be-processed elements.
					newchunks = append(newchunks, d)
					continue
				}
				// There is some overlap. There are a few possibilities:
				// 1) One is completely contained in the other.
				// 2) Both are slices of a larger underlying array.
				// 3) Some unsafe trickery has happened. Non-containing overlap
				//    can only happen in safe Go via case 2.
				if c.min() >= d.min() && c.max() <= d.max() {
					// 1a: c is contained within the existing chunk d.
					// Note that there can be a type mismatch between c and d,
					// but we don't care. We use the larger chunk regardless.
					c = d
					addWork = false // We've already scanned all of c.
					continue
				}
				if d.min() >= c.min() && d.max() <= c.max() {
					// 1b: existing chunk d is completely covered by c.
					continue
				}
				if c.t == d.t && c.matchingAlignment(d) {
					// Union two regions of the same base type. Case 2 above.
					c = c.merge(d)
					continue
				}
				if c.size() < d.size() {
					// Keep the larger of the two chunks.
					c = d
					addWork = false
				}
			}
			// Add new chunk to list of chunks for object.
			newchunks = append(newchunks, c)
			interior[i] = newchunks
			// Also arrange to scan the new chunk. Note that if we merged
			// with an existing chunk (or chunks), those will get rescanned.
			// Duplicate work, but that's ok. TODO: but could be expensive.
			if addWork {
				appendWork(workRecord{
					a: a.Add(c.off - off),
					t: c.t,
					r: c.r,
				})
			}
		}

		// Get typings starting at roots.
		fr := &frameReader{p: p}
		p.ForEachRoot(func(r *Root) bool {
			if r.Frame != nil {
				fr.live = r.Frame.Live
				p.typeObject(r.Addr, r.Type, fr, add)
			} else {
				p.typeObject(r.Addr, r.Type, p.proc, add)
			}
			return true
		})

		// Propagate typings through the heap.
		for len(work) > 0 {
			c := work[len(work)-1]
			work = work[:len(work)-1]
			switch c.t.Kind {
			case KindBool, KindInt, KindUint, KindFloat, KindComplex:
				// Don't do O(n) function calls for big primitive slices
				continue
			}
			for i := int64(0); i < c.r; i++ {
				if c.a == 0xc000d94000 {
					fmt.Printf("hit\n")
				}
				p.typeObject(c.a.Add(i*c.t.Size), c.t, p.proc, add)
			}
		}

		// Merge any interior typings with the 0-offset typing.
		for i, chunks := range interior {
			t := p.types[i].t
			r := p.types[i].r
			if t == nil {
				continue // We have no type info at offset 0.
			}
			for _, c := range chunks {
				if c.max() <= r*t.Size {
					// c is completely contained in the 0-offset typing. Ignore it.
					continue
				}
				if c.min() <= r*t.Size {
					// Typings overlap or abut. Extend if we can.
					if c.t == t && c.min()%t.Size == 0 {
						r = c.max() / t.Size
						p.types[i].r = r
					}
					continue
				}
				// Note: at this point we throw away any interior typings that weren't
				// merged with the 0-offset typing.  TODO: make more use of this info.
			}
		}
	})
}

type reader interface {
	ReadPtr(core.Address) core.Address
	ReadInt(core.Address) int64
	ReadUint8(core.Address) uint8
}

// A frameReader reads data out of a stack frame.
// Any pointer slots marked as dead will read as nil instead of their real value.
type frameReader struct {
	p    *Process
	live map[core.Address]bool
}

func (fr *frameReader) ReadPtr(a core.Address) core.Address {
	if !fr.live[a] {
		return 0
	}
	return fr.p.proc.ReadPtr(a)
}
func (fr *frameReader) ReadInt(a core.Address) int64 {
	return fr.p.proc.ReadInt(a)
}
func (fr *frameReader) ReadUint8(a core.Address) uint8 {
	return fr.p.proc.ReadUint8(a)
}

//go:noinline
func debugHit() {
	fmt.Printf("hit!\n")
}

// main.(*Bar).started-fm
var methodRegexp = regexp.MustCompile(`([\w]+)\.\(\*([\w]+)\)\.[\w-]+$`)

// typeObject takes an address and a type for the data at that address.
// For each pointer it finds in the memory at that address, it calls add with the pointer
// and the type + repeat count of the thing that it points to.
func (p *Process) typeObject(a core.Address, t *Type, r reader, add func(core.Address, *Type, int64)) {
	ptrSize := p.proc.PtrSize()
	if a == 0xc000d94000 {
		fmt.Printf("hit\n")
	}
	if a == 0xc320980000 {
		fmt.Printf("hit unk\n")
	}

	switch t.Kind {
	case KindBool, KindInt, KindUint, KindFloat, KindComplex:
		// Nothing to do
	case KindEface, KindIface:
		// interface. Use the type word to determine the type
		// of the pointed-to object.
		if a == 0 {
			return
		}
		typPtr := r.ReadPtr(a)
		if typPtr == 0 { // nil interface
			return
		}
		data := a.Add(ptrSize)
		dataPtr := r.ReadPtr(data)
		if dataPtr == 0xc320980000 {
			fmt.Printf("hit unk\n")
		}
		if t.Kind == KindIface {
			typPtr = p.proc.ReadPtr(typPtr.Add(p.findType("runtime.itab").field("_type").Off))
		}
		if typPtr == 0 { // nil interface
			return
		}
		// hack for invalid typePtr
		m := p.proc.FindMapping(typPtr)
		if m == nil {
			fmt.Printf("typPtr out of memory address: %v\n", typPtr)
			return
		}
		if typPtr == 4294967295 { // hack 0xffffffff
			return
		}
		// TODO: for KindEface, type typPtr. It might point to the heap
		// if the type was allocated with reflect.
		typ := p.runtimeType2Type(typPtr, a.Add(ptrSize))
		// size := p.Size(Object(dataPtr))
		// fmt.Printf("interface, addr: 0x%x, data: 0x%x, dataPtr: 0x%x, typPtr: 0x%x, type name: %s, typ kind: %v, typ size: %v, data obj size: %v\n", a, data, dataPtr, typPtr, typ.Name, typ.Kind, typ.Size, size)
		/*
			if dataPtr == 0xc0004bc008 {
				typeName := "*internal.InformersMap"
				s := p.runtimeNameMap[typeName]
				if len(s) == 0 {
					fmt.Printf("not found type(%v)\n", typeName)
				} else {
					styp := s[0]
					typ.Fields[0].Type = styp
				}
				add(dataPtr, typ, 1)
				ptr2 := r.ReadPtr(dataPtr)
				size2 := p.Size(Object(ptr2))
				fmt.Printf("size2: %d, typ field 0 size: %d\n", size2, typ.Fields[0].Type.Size)
				debugHit()
			}
			if dataPtr == 0xc00043c468 {
				typeName := "*cache.cache"
				s := p.runtimeNameMap[typeName]
				stype := s[0]
				add(data, stype, 1)

				fmt.Printf("type name: %s, typ kind: %v, typ size: %v\n", stype.Name, stype.Kind, stype.Size)
				return
			}
			if dataPtr == 0xc00022e770 {
				typeName := "*k8sstore.myThreadSafeStore"
				s := p.runtimeNameMap[typeName]
				stype := s[0]
				add(data, stype, 1)

				fmt.Printf("type name: %s, typ kind: %v, typ size: %v\n", stype.Name, stype.Kind, stype.Size)
				return
			}
			if dataPtr == 0xc0008d3d40 {
				typeName := "*cache.threadSafeMap"
				s := p.runtimeNameMap[typeName]
				stype := s[0]
				add(data, stype, 1)

				fmt.Printf("type name: %s, typ kind: %v, typ size: %v\n", stype.Name, stype.Kind, stype.Size)
				return
			}
		*/
		typr := region{p: p, a: typPtr, typ: p.findType("runtime._type")}
		if typr.Field("kind").Uint8()&uint8(p.rtConstants["kindDirectIface"]) == 0 {
			// Indirect interface: the interface introduced a new
			// level of indirection, not reflected in the type.
			// Read through it.
			add(r.ReadPtr(data), typ, 1)
			return
		} else {
			// fmt.Printf("foo\n")
		}

		// Direct interface: the contained type is a single pointer.
		// Figure out what it is and type it. See isdirectiface() for the rules.
		directTyp := typ
	findDirect:
		for {
			if newTyp := ReFindType(directTyp, p); newTyp != directTyp {
				directTyp = newTyp
				data = r.ReadPtr(data)
				break
			}
			/*
				name := directTyp.Name
				if newTypName, ok := SymbolNameMap[name]; ok {
					directTyp = p.findType(newTypName)
					if directTyp == nil {
						panic(fmt.Sprintf("not found type: %v", newTypName))
					}
					data = r.ReadPtr(data)
					break
				}
			*/
			/*
				if directTyp.Kind == KindStruct {
					// FIXME: hack type
					if directTyp.Name == "*http.connPool" {
						directTyp = p.findType("gitlab.alipay-ant-http.connPool")
						if directTyp == nil {
							panic("not found type: gitlab.alipay-ant-http.connPool")
						}
						data = r.ReadPtr(data)
						break
					}

					if directTyp.Name == "*stream.client" {
						directTyp = p.findType("gitlab.alipay-ant-stream.client")
						if directTyp == nil {
							panic("not found type: gitlab.alipay-ant-stream.client")
						}
						data = r.ReadPtr(data)
						break
					}
				}
			*/

			if directTyp.Kind == KindArray {
				directTyp = typ.Elem
				continue findDirect
			}
			if directTyp.Kind == KindStruct {
				for _, f := range directTyp.Fields {
					if f.Type.Size != 0 {
						directTyp = f.Type
						continue findDirect
					}
				}
			}
			if directTyp.Kind != KindFunc && directTyp.Kind != KindPtr {
				panic(fmt.Sprintf("type of direct interface, originally %s (kind %s), isn't a pointer: %s (kind %s)", typ, typ.Kind, directTyp, directTyp.Kind))
			}
			break
		}
		// fmt.Printf("interface, addr: 0x%x, typPtr: 0x%x, type name: %v, directTyp name: %v\n", a, typPtr, typ.Name, directTyp.Name)
		add(data, directTyp, 1)
	case KindString:
		ptr := r.ReadPtr(a)
		len := r.ReadInt(a.Add(ptrSize))
		add(ptr, t.Elem, len)
	case KindSlice:
		ptr := r.ReadPtr(a)
		cap := r.ReadInt(a.Add(2 * ptrSize))
		add(ptr, t.Elem, cap)
	case KindPtr:
		addr := fmt.Sprintf("0x%x", r.ReadPtr(a))
		if addr == "0xc0000100f0" {
			fmt.Printf("hit")
		}
		if t.Elem != nil { // unsafe.Pointer has a nil Elem field.
			add(r.ReadPtr(a), t.Elem, 1)
		} else {
			if addr == "0xc008c2a068" {
				fmt.Printf("bar\n")
			}
			// fmt.Printf("unsafe.Pointer: 0x%x, address: 0x%x\n", a, r.ReadPtr(a))
		}
	case KindFunc:
		// The referent is a closure. We don't know much about the
		// type of the referent. Its first entry is a code pointer.
		// The runtime._type we want exists in the binary (for all
		// heap-allocated closures, anyway) but it would be hard to find
		// just given the pc.
		closure := r.ReadPtr(a)
		if closure == 0 {
			break
		}
		pc := p.proc.ReadPtr(closure)
		f := p.funcTab.find(pc)
		if f == nil {
			panic(fmt.Sprintf("can't find func for closure pc %x", pc))
		}
		ft := f.closure
		if ft == nil {
			ft = &Type{Name: "closure for " + f.name, Size: ptrSize, Kind: KindPtr}
			// For now, treat a closure like an unsafe.Pointer.
			// TODO: better value for size?
			f.closure = ft
		}
		if matches := methodRegexp.FindStringSubmatch(f.name); len(matches) == 3 {
			typeName := "*" + matches[1] + "." + matches[2]
			s := p.runtimeNameMap[typeName]
			if len(s) == 0 {
				fmt.Printf("not found type(%v) for method(%v)\n", typeName, f.name)
			} else {
				typ := s[0]
				ptr := closure.Add(p.proc.PtrSize())
				p.typeObject(ptr, typ, r, add)
			}
		} else {
			fmt.Printf("func name (%v) does not looks like a method\n", f.name)
		}
		/*
			if f.name == "main.(*Bar).started-fm" {
				ptr := closure.Add(p.proc.PtrSize())
				o := r.ReadPtr(ptr)
				fmt.Printf("ptr: 0x%x, obj: 0x%x\n", a.Add(p.proc.PtrSize()), o)
				size := p.Size(Object(o))
				fmt.Printf("size: %d\n", size)
				typ := p.findType("*main.Bar")
				p.typeObject(ptr, typ, r, add)
			}
		*/
		p.typeObject(closure, ft, r, add)
	case KindArray:
		n := t.Elem.Size
		for i := int64(0); i < t.Count; i++ {
			p.typeObject(a.Add(i*n), t.Elem, r, add)
		}
	case KindStruct:
		if strings.HasPrefix(t.Name, "hash<") {
			// Special case - maps have a pointer to the first bucket
			// but it really types all the buckets (like a slice would).
			var bPtr core.Address
			var bTyp *Type
			var n int64
			for _, f := range t.Fields {
				if f.Name == "buckets" {
					bPtr = p.proc.ReadPtr(a.Add(f.Off))
					bTyp = f.Type.Elem
				}
				if f.Name == "B" {
					n = int64(1) << p.proc.ReadUint8(a.Add(f.Off))
				}
			}
			if bPtr == 0xc000d94000 {
				fmt.Printf("hit\n")
				typeName := "v1.Pod"
				s := p.runtimeNameMap[typeName]
				typ := s[0]
				addr := 0xc320980000
				size := p.Size(Object(addr))
				n := size / typ.Size
				add(core.Address(addr), typ, n)
			}
			if bPtr == 0xc320980000 {
				fmt.Printf("hit unk\n")
			}
			add(bPtr, bTyp, n)
			// TODO: also oldbuckets
		}
		// TODO: also special case for channels?
		for _, f := range t.Fields {
			if t.Name == "sync.entry" && f.Name == "p" && f.Type.Name == "unsafe.Pointer" {
				// f.Type.Name = "unsafe.Pointer<interface{}>"
				iface := &Type{
					Name: "sync.entry<interface{}>",
					Kind: KindEface,
				}
				// fmt.Printf("sync.entry, addr: 0x%x, ptr: 0x%x\n", a.Add(f.Off), r.ReadPtr(a.Add(f.Off)))
				p.typeObject(r.ReadPtr(a.Add(f.Off)), iface, r, add)

			} else if t.Name == "sync.Pool" && f.Name == "local" && f.Type.Name == "unsafe.Pointer" {
				// TODO: also handle victim
				var size uint64
				for _, f := range t.Fields {
					if f.Name == "localSize" {
						size = p.proc.ReadUint64(a.Add(f.Off))
					}
					// fmt.Printf("field: %v, a: %v, f.Off: %v, size: %v\n", f.Name, a, f.Off, size)
				}
				// fmt.Printf("Pool.local, t.name: %v\n", t.Name)
				// fmt.Printf("Array count: %v\n", array.Count)
				typ := p.findType("sync.poolLocal")
				// fmt.Printf("sync.poolLocal type, name: %v, kind: %v, size: %v, count: %v\n", typ.Name, typ.Kind, typ.Size, typ.Count)

				array := &Type{
					Name:  "[P]poolLocal",
					Kind:  KindArray,
					Count: int64(size),
					Size:  int64(size) * typ.Size,
					Elem:  typ,
				}
				ptr := &Type{
					Name: "*[P]poolLocal",
					Kind: KindPtr,
					Elem: array,
				}
				p.typeObject(a.Add(f.Off), ptr, r, add)

			} else {
				p.typeObject(a.Add(f.Off), f.Type, r, add)
			}
		}
	default:
		panic(fmt.Sprintf("unknown type kind %s\n", t.Kind))
	}
}
