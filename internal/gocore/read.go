package gocore

import (
	"fmt"
	"golang.org/x/debug/internal/core"
	"strings"
)

var SymbolNameMap = map[string]string{
	"*http.connPool":               "gitlab.alipay-ant-http.connPool",
	"*stream.client":               "gitlab.alipay-ant-stream.client",
	"*http.clientStreamConnection": "gitlab.alipay-ant-http.clientStreamConnection",
	"*http.serverStreamConnection": "gitlab.alipay-ant-http.serverStreamConnection",
	"*network.connection":          "gitlab.alipay-ant-network.connection",
	"*proxy.downStream":            "gitlab.alipay-ant-proxy.downStream",
	"*proxy.proxy":                 "gitlab.alipay-ant-proxy.proxy",
	"*server.activeConnection":     "gitlab.alipay-ant-server.activeConnection",
	"*http.activeClient":           "gitlab.alipay-ant-http.activeClient",
	"*cluster.clusterSnapshot":     "gitlab.alipay-ant-cluster.clusterSnapshot",
	"*cluster.hostSet":             "gitlab.alipay-ant-cluster.hostSet",
	"*context.valueCtx":            "gitlab.alipay-ant-context.valueCtx",
	"context.valueCtx":             "gitlab.alipay-ant-context.valueCtx",
}

func ReFindType(t *Type, p *Process) *Type {
	return t
	name := t.Name
	if _, ok := SymbolNameMap[name]; t.Kind == KindStruct && ok {
		newName := "gitlab.alipay-ant-" + strings.TrimPrefix(name, "*")
		s := p.runtimeNameMap[newName]
		if len(s) > 0 {
			typ := p.findType(newName)
			if typ != nil {
				// fmt.Printf("old name: %v, kind: %v, new name: %v, kind: %v\n", typ.Name, typ.Kind, t.Name, t.Kind)
				return typ
			}
		}
	}
	return t
}

func ReadEface(a core.Address, t *Type, r reader, p *Process, level int) {
	if t != nil && t.Kind != KindEface {
		panic("invalid type")
	}
	fmt.Printf("Level(%d), Empty interface, address: 0x%x\n", level, a)

	ptrSize := p.proc.PtrSize()
	typPtr := r.ReadPtr(a)
	if typPtr == 0 { // nil interface
		fmt.Println("nil interface")
		return
	}
	typ := p.runtimeType2Type(typPtr, a.Add(ptrSize))
	data := a.Add(ptrSize)
	dataPtr := r.ReadPtr(data)
	fmt.Printf("type, name: %v, kind: %v, data: 0x%x, data ptr: 0x%x\n", typ.Name, typ.Kind, data, dataPtr)

	if typ.Kind == KindString {
		ReadString(dataPtr, typ, r, p)
	}

	if typ.Kind == KindPtr {
		ReadPtr(data, typ, r, p, level+1)
	}

	typ = ReFindType(typ, p)
	if typ.Kind == KindStruct {
		/*
			name := typ.Name
			if newTypName, ok := SymbolNameMap[name]; ok {
				typ = p.findType(newTypName)
				if typ == nil {
					panic(fmt.Sprintf("not found type: %v", newTypName))
				}
			}
			fmt.Printf("new type, name: %v, kind: %v\n", typ.Name, typ.Kind)
		*/
		ReadStruct(dataPtr, typ, r, p, level+1)
	}

	if typ.Kind == KindUint {
		ReadUint(data, typ, r, p)
		ReadUint(dataPtr, typ, r, p)
	}
}

func ReadIface(a core.Address, t *Type, r reader, p *Process, level int) {
	if t.Kind != KindIface {
		panic("invalid type")
	}
	fmt.Printf("normal interface, address: 0x%x\n", a)

	ptrSize := p.proc.PtrSize()
	typPtr := r.ReadPtr(a)
	if typPtr == 0 { // nil interface
		fmt.Println("nil interface")
		return
	}

	typPtr = p.proc.ReadPtr(typPtr.Add(p.findType("runtime.itab").field("_type").Off))
	if typPtr == 0 { // nil interface
		fmt.Println("nil interface")
		return
	}

	data := a.Add(ptrSize)
	typ := p.runtimeType2Type(typPtr, data)
	dataPtr := r.ReadPtr(data)
	fmt.Printf("type, name: %v, kind: %v, data: 0x%x, data ptr: 0x%x\n", typ.Name, typ.Kind, data, dataPtr)

	if typ.Kind == KindString {
		ReadString(dataPtr, typ, r, p)
	}

	if typ.Kind == KindPtr {
		ReadPtr(data, typ, r, p, level+1)
	}
	/*
		for name, _ := range p.runtimeNameMap {
			fmt.Printf("%v\n", name)
		}
	*/

	typ = ReFindType(typ, p)
	if typ.Kind == KindStruct {
		/*
			name := typ.Name
			if newTypName, ok := SymbolNameMap[name]; ok {
				typ = p.findType(newTypName)
				if typ == nil {
					panic(fmt.Sprintf("not found type: %v", newTypName))
				}
			}
		*/
	}

	if typ.Kind == KindStruct && dataPtr != 0 {
		ReadStruct(dataPtr, typ, r, p, level+1)
	}
}

func ReadPtr(a core.Address, t *Type, r reader, p *Process, level int) {
	if t.Kind != KindPtr {
		panic("invalid type")
	}

	ptr := r.ReadPtr(a)

	typ := t.Elem
	if typ == nil {
		fmt.Printf("unsafe.pointer, address: 0x%x, ptr: 0x%x\n", a, ptr)
	} else {
		fmt.Printf("pointer, address: 0x%x, ptr: 0x%x, type name: %v, kind: %v\n", a, ptr, typ.Name, typ.Kind)

		// typ = ReFindType(typ, p)

		// hack for 0xffffffff, 4294967295
		if ptr != 0 && ptr != 4294967295 {
			ReadObj(ptr, typ, r, p, level+1)
		}
	}
}

var ReadedObjs = make(map[string]bool)

func ReadObj(a core.Address, t *Type, r reader, p *Process, level int) {
	if level == 0 {
		for name, l := range p.runtimeNameMap {
			fmt.Printf("%v\n", name)
			for i, t := range l {
				fmt.Printf("%v, %d: %v\n", name, i, t.Kind)
				if t.Kind == KindStruct {
					for j, f := range t.Fields {
						fmt.Printf("-- %d: %v\n", j, f.Name)
					}
				}
			}
		}
		level++
	}
	if level > 20 {
		fmt.Printf("skipping level(%d)\n", level)
		return
	}
	key := fmt.Sprintf("0x%x-%v-%v", a, t.Name, t.Kind)
	if v, ok := ReadedObjs[key]; ok && v {
		fmt.Printf("skipping the readed obj, address: 0x%x\n", a)
		return
	}
	ReadedObjs[key] = true
	switch t.Kind {
	case KindString:
		ReadString(a, t, r, p)
	case KindUint:
		ReadUint(a, t, r, p)
	case KindInt:
		ReadInt(a, t, r, p)
	case KindBool:
		ReadBool(a, t, r, p)
	case KindSlice:
		ReadSlice(a, t, r, p, level)
	case KindPtr:
		ReadPtr(a, t, r, p, level)
	case KindStruct:
		ReadStruct(a, t, r, p, level)
	case KindIface:
		ReadIface(a, t, r, p, level)
	case KindEface:
		ReadEface(a, t, r, p, level)
	case KindArray:
		ReadArray(a, t, r, p, level)
	default:
		fmt.Printf("unsupported type, kind: %v, name: %v, address: 0x%x\n", t.Kind, t.Name, a)
	}
}

func ReadSlice(a core.Address, t *Type, r reader, p *Process, level int) {
	if t.Kind != KindSlice {
		panic("invalid type")
	}
	ptrSize := p.proc.PtrSize()
	ptr := r.ReadPtr(a)
	len := r.ReadInt(a.Add(ptrSize))
	cap := r.ReadInt(a.Add(ptrSize * 2))
	typ := t.Elem

	fmt.Printf("Slice, address: 0x%x, ptr: 0x%x, len: %d, cap: %d, type, name: %v, kind: %v\n", a, ptr, len, cap, typ.Name, typ.Kind)
	for i := int64(0); i < len; i++ {
		fmt.Printf("Slice elem (%d):\n", i+1)
		ReadObj(ptr.Add(typ.Size*i), typ, r, p, level+1)
	}
}

func ReadArray(a core.Address, t *Type, r reader, p *Process, level int) {
	if t.Kind != KindArray {
		panic("invalid type")
	}

	typ := t.Elem
	len := t.Count
	// len = 25

	fmt.Printf("Level(%d), Array, address: 0x%x, len: %d, type, name: %v, kind: %v\n", level, a, len, typ.Name, typ.Kind)
	for i := int64(0); i < len; i++ {
		ReadObj(a.Add(typ.Size*i), typ, r, p, level+1)
	}
}

func ReadInt(a core.Address, t *Type, r reader, p *Process) {
	if t.Kind != KindInt {
		panic("invalid type")
	}
	fmt.Printf("Int, value: %d\n", r.ReadInt(a))
}

func ReadUint(a core.Address, t *Type, r reader, p *Process) {
	if t.Kind != KindUint {
		panic("invalid type")
	}
	fmt.Printf("UInt, value: %d\n", uint64(r.ReadInt(a)))
}

func ReadBool(a core.Address, t *Type, r reader, p *Process) {
	if t.Kind != KindBool {
		panic("invalid type")
	}
	fmt.Printf("Bool, value: %d\n", r.ReadUint8(a))
}

func ReadStruct(a core.Address, t *Type, r reader, p *Process, level int) {
	if t.Kind != KindStruct {
		panic("invalid type")
	}

	fmt.Printf("Level(%d), Reading struct, address: 0x%x, type, name: %v, fields num: %v\n", level, a, t.Name, len(t.Fields))

	count := int64(0)
	if strings.HasPrefix(t.Name, "hash<") {
		for _, f := range t.Fields {
			if f.Name == "B" {
				count = 1 << p.proc.ReadUint8(a.Add(f.Off))
			}
		}
		fmt.Printf("count: %d\n", count)
	}

	for i, f := range t.Fields {
		fmt.Printf("Level(%d), Field, (%d), name: %v\n", level, i+1, f.Name)
		if f.Name == "pool" && t.Name == "gitlab.alipay-inc.com/ant-mesh/mosn/vendor/mosn.io/mosn/pkg/stream/http.activeClient" {
			fmt.Printf("dead loop, skipping\n")
		} else {
			ReadObj(a.Add(f.Off), f.Type, r, p, level+1)
		}
		if f.Name == "buckets" && count > 1 {
			for i := int64(1); i < count; i++ {
				fmt.Printf("Level(%d), bucket (%d)\n", level, i)
				ReadObj(p.proc.ReadPtr(a.Add(f.Off)).Add(f.Type.Elem.Size*i), f.Type.Elem, r, p, level+1)
			}
		}
	}
	fmt.Printf("Level(%d), Finished struct, address: 0x%x, type, name: %v, fields num: %v\n", level, a, t.Name, len(t.Fields))
}

func ReadString(a core.Address, t *Type, r reader, p *Process) {
	if t.Kind != KindString {
		panic("invalid type")
	}
	ptrSize := p.proc.PtrSize()
	data := r.ReadPtr(a)
	len := r.ReadInt(a.Add(ptrSize))

	fmt.Printf("string, len: %v, data: 0x%x\n", len, data)
	if data == 0 && len > 0 {
		fmt.Printf("Error: invalid string data pointer: 0x%x\n", data)
	} else {
		fmt.Printf("string value: \"")
		for i := int64(0); i < len; i++ {
			fmt.Printf("%c", r.ReadUint8(data.Add(i)))
		}
		fmt.Printf("\"\n")
	}
}
