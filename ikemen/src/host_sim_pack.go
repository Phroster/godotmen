package main

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"sync/atomic"
	"unsafe"
)

// Compiled state code layout.
//
// Every CNS controller compiles to its own heap objects: the frozen block
// header, its trigger bytecode and the one-element controller list, allocated
// in compile order between every other compiler allocation. Running a state
// walks those headers and triggers one after the other, so in a crowded fight
// the first touch of almost every block is a cache/TLB miss (the profile's
// hottest single instruction is the load of the block header's first field).
//
// simPackStates copies one character's finished code into three contiguous
// slabs - block headers, controller lists and expression bytecode - in the
// order the interpreter walks them. Only the memory layout changes: every
// block keeps the same field values, every list the same elements in the same
// order, every expression the same bytes (cap == len, so no append can write
// into a neighbour). Compiled code is immutable after compilation (loop
// bounds are copied per invocation, StateBlock.run), and nothing compares
// block identities, so execution is unchanged.
type simPack struct {
	blocks []StateBlock
	fused  []simFusedBlock
	tails  []simCtrlTail
	ctrls  []StateController
	code   []OpCode
	groups []simCNSGroup
	gblks  []compiledStateBlock
	// counting pass
	nBlocks, nFused, nTails, nCtrls, nCode, nGroups, nGblks int
	counting                                                bool
}

// Almost every CNS block holds exactly one controller. Such a block, its
// one-element controller list and (for a byte-slice controller) that
// controller's slice header share one allocation, so running the block
// touches one place instead of four.
type simFusedBlock struct {
	StateBlock
	ctrl [1]StateController
	hdr  []byte
}

// Split layout (IKEMEN_SIM_PACK_FUSE=2): headers stay a compact slab; the one
// controller and its slice header sit in a parallel slab, in the same order.
type simCtrlTail struct {
	ctrl [1]StateController
	hdr  []byte
}

type simIfaceWords struct{ tab, data unsafe.Pointer }

var simPackCode = os.Getenv("IKEMEN_SIM_PACK") != "0" // IKEMEN_SIM_PACK=0: compile-order layout (diagnostics)

// One-controller blocks (IKEMEN_SIM_PACK_FUSE): 0 header slab + controller-list
// slab (default: measured best, native 3.50 vs 4.02 ms median over 5
// interleaved rounds, a large fight 23.8 vs 26.3 us CPU per character-tick), 1 fused
// (header+controller+slice header), 2 split (compact headers + parallel
// controller/slice-header slab).
var simPackFuse = simPackFuseMode()

func simPackFuseMode() int {
	switch os.Getenv("IKEMEN_SIM_PACK_FUSE") {
	case "1":
		return 1
	case "2":
		return 2
	}
	return 0
}

func (p *simPack) bytes(e BytecodeExp) BytecodeExp {
	if len(e) == 0 {
		return e
	}
	if p.counting {
		p.nCode += len(e)
		return e
	}
	r := p.code[:len(e):len(e)]
	p.code = p.code[len(e):]
	copy(r, e)
	return r
}

func (p *simPack) block(b *StateBlock) *StateBlock {
	if p.counting {
		switch {
		case len(b.ctrls) == 1 && simPackFuse == 1:
			p.nFused++
		case len(b.ctrls) == 1 && simPackFuse == 2:
			p.nBlocks++
			p.nTails++
		default:
			p.nBlocks++
		}
		p.bytes(b.trigger)
		if len(b.ctrls) == 1 && simPackFuse != 0 {
			p.elem(b.ctrls[0], nil, nil)
		} else {
			p.list(b.ctrls)
		}
		if b.elseBlock != nil {
			p.block(b.elseBlock)
		}
		return b
	}
	var nb *StateBlock
	if len(b.ctrls) == 1 && simPackFuse == 2 {
		nb = &p.blocks[0]
		p.blocks = p.blocks[1:]
		*nb = *b
		nb.trigger = p.bytes(b.trigger)
		t := &p.tails[0]
		p.tails = p.tails[1:]
		t.ctrl[0] = p.elem(b.ctrls[0], &t.ctrl[0], &t.hdr)
		nb.ctrls = t.ctrl[:1:1]
	} else if len(b.ctrls) == 1 && simPackFuse == 1 {
		fb := &p.fused[0]
		p.fused = p.fused[1:]
		fb.StateBlock = *b
		nb = &fb.StateBlock
		nb.trigger = p.bytes(b.trigger)
		fb.ctrl[0] = p.elem(b.ctrls[0], &fb.ctrl[0], &fb.hdr)
		nb.ctrls = fb.ctrl[:1:1]
	} else {
		nb = &p.blocks[0]
		p.blocks = p.blocks[1:]
		*nb = *b
		nb.trigger = p.bytes(b.trigger)
		nb.ctrls = p.list(b.ctrls)
	}
	if b.elseBlock != nil {
		nb.elseBlock = p.block(b.elseBlock)
	}
	return nb
}

func (p *simPack) list(l []StateController) []StateController {
	if len(l) == 0 {
		return l
	}
	if p.counting {
		p.nCtrls += len(l)
		for _, sc := range l {
			p.elem(sc, nil, nil)
		}
		return l
	}
	nl := p.ctrls[:len(l):len(l)]
	p.ctrls = p.ctrls[len(l):]
	for i, sc := range l {
		nl[i] = p.elem(sc, nil, nil)
	}
	return nl
}

// elem returns the packed copy of one controller. With slot/hdr (a fused
// block), a byte-slice controller's slice header is placed in hdr and the
// returned interface points at it.
func (p *simPack) elem(sc StateController, slot *StateController, hdr *[]byte) StateController {
	switch b := sc.(type) {
	case compiledStateBlock:
		return compiledStateBlock{p.block(b.StateBlock)}
	case *simCNSGroup:
		return p.group(b)
	case StateBlock:
		v := b
		v.trigger = p.bytes(b.trigger)
		v.ctrls = p.list(b.ctrls)
		if b.elseBlock != nil {
			v.elseBlock = p.block(b.elseBlock)
		}
		if p.counting {
			return sc
		}
		return v
	case varAssign:
		va := b
		va.be = p.bytes(b.be)
		if p.counting {
			return sc
		}
		return va
	case CallFunction:
		// Same call, packed argument bytes, a lookup cache (simLookupFunc).
		cf := b
		cf.arg = p.bytes(b.arg)
		if p.counting {
			return sc
		}
		cf.cache = &simFuncCache{}
		return cf
	}
	// Controllers whose value is their parameter bytecode (StateControllerBase
	// and the other byte-slice controller types): the same dynamic type with
	// the same bytes, copied next to the triggers.
	v := reflect.ValueOf(sc)
	if v.Kind() != reflect.Slice || v.Type().Elem().Kind() != reflect.Uint8 || v.Len() == 0 {
		return sc
	}
	raw := v.Bytes()
	nb := p.bytes(*(*BytecodeExp)(unsafe.Pointer(&raw)))
	if p.counting {
		return sc
	}
	nbytes := *(*[]byte)(unsafe.Pointer(&nb))
	if slot != nil && hdr != nil {
		// Same itab (dynamic type), data pointing at the fused block's header.
		*hdr = nbytes
		var out StateController
		w := (*simIfaceWords)(unsafe.Pointer(&out))
		w.tab = (*simIfaceWords)(unsafe.Pointer(&sc)).tab
		w.data = unsafe.Pointer(hdr)
		return out
	}
	nv := reflect.New(v.Type()).Elem()
	nv.SetBytes(nbytes)
	if nsc, ok := nv.Interface().(StateController); ok {
		return nsc
	}
	return sc
}

func (p *simPack) group(g *simCNSGroup) *simCNSGroup {
	if p.counting {
		p.nGroups++
		p.nGblks += len(g.blocks)
		p.bytes(g.prefix)
		for _, b := range g.blocks {
			p.block(b.StateBlock)
		}
		return g
	}
	ng := &p.groups[0]
	p.groups = p.groups[1:]
	ng.prefix = p.bytes(g.prefix)
	ng.blocks = p.gblks[:len(g.blocks):len(g.blocks)]
	p.gblks = p.gblks[len(g.blocks):]
	for j, b := range g.blocks {
		ng.blocks[j] = compiledStateBlock{p.block(b.StateBlock)}
	}
	return ng
}

func (p *simPack) state(sb *StateBytecode) {
	sb.block.trigger = p.bytes(sb.block.trigger)
	sb.block.ctrls = p.list(sb.block.ctrls)
	if sb.block.elseBlock != nil {
		sb.block.elseBlock = p.block(sb.block.elseBlock)
	}
}

// simPackStates rewrites states and funcs in place (map values) with packed
// copies. Negative (every-tick) states first, then ascending state numbers,
// then functions by name.
func simPackStates(states map[int32]StateBytecode, funcs map[string]BytecodeFunction) {
	if !simPackCode {
		return
	}
	nums := make([]int32, 0, len(states))
	for sn := range states {
		nums = append(nums, sn)
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
	names := make([]string, 0, len(funcs))
	for name := range funcs {
		names = append(names, name)
	}
	sort.Strings(names)

	p := &simPack{counting: true}
	walk := func() {
		for _, sn := range nums {
			sb := states[sn]
			p.state(&sb)
			if !p.counting {
				states[sn] = sb
			}
		}
		for _, name := range names {
			bf := funcs[name]
			bf.ctrls = p.list(bf.ctrls)
			if !p.counting {
				funcs[name] = bf
			}
		}
	}
	walk()
	if os.Getenv("IKEMEN_SIM_PACK_LOG") == "1" {
		fmt.Fprintln(os.Stderr, fmt.Sprintf("IKEMEN_PACK states=%d funcs=%d blocks=%d fused=%d ctrls=%d code=%d groups=%d",
			len(nums), len(names), p.nBlocks, p.nFused, p.nCtrls, p.nCode, p.nGroups))
	}
	p.counting = false
	p.blocks = make([]StateBlock, p.nBlocks)
	p.fused = make([]simFusedBlock, p.nFused)
	p.tails = make([]simCtrlTail, p.nTails)
	p.ctrls = make([]StateController, p.nCtrls)
	p.code = make([]OpCode, p.nCode)
	p.groups = make([]simCNSGroup, p.nGroups)
	p.gblks = make([]compiledStateBlock, p.nGblks)
	walk()
	if len(p.blocks) != 0 || len(p.fused) != 0 || len(p.tails) != 0 || len(p.ctrls) != 0 || len(p.code) != 0 || len(p.groups) != 0 || len(p.gblks) != 0 {
		panic("simPackStates: slab size mismatch")
	}
}

// A ZSS function call site remembers the callFuncs map it last resolved
// against and the result. A callFuncs map is filled by its compiler and only
// then published (sys.cgi[pn].callFuncs = c.funcs at the end of Compile); it
// is never written afterwards. So a lookup in the same map always gives the
// same answer; a different map (another slot's code, a reload) is looked up
// again. The cached entry references the map, so it cannot be freed and its
// address reused while cached.
type simFuncCache struct {
	hit atomic.Pointer[simFuncHit]
}

type simFuncHit struct {
	m  map[string]BytecodeFunction
	bf BytecodeFunction
	ok bool
}

func simSameMap(a, b map[string]BytecodeFunction) bool {
	return *(*unsafe.Pointer)(unsafe.Pointer(&a)) == *(*unsafe.Pointer)(unsafe.Pointer(&b))
}

func simLookupFunc(cf *CallFunction, m map[string]BytecodeFunction) (BytecodeFunction, bool) {
	if cf.cache == nil {
		bf, ok := m[cf.name]
		return bf, ok
	}
	if h := cf.cache.hit.Load(); h != nil && simSameMap(h.m, m) {
		return h.bf, h.ok
	}
	bf, ok := m[cf.name]
	cf.cache.hit.Store(&simFuncHit{m: m, bf: bf, ok: ok})
	return bf, ok
}

// const(name) for a name outside the built-in list reads the character's
// constants map with the pool string (a string hash and compare per read). A
// constants map is filled while its character loads (CharGlobalInfo gets a
// new map at the start of every load) and never written afterwards, so the
// same map gives the same value for the same name: each pool index remembers
// the map it last read and that value. A different map (a redirect to
// another player, a reload) is read again; the cell keeps its map alive.
type simConstCell struct {
	hit atomic.Pointer[simConstHit]
}

type simConstHit struct {
	m map[string]float32
	v float32
}

func simConstLookup(pool *StringPool, idx int32, m map[string]float32) float32 {
	if idx >= 0 && int(idx) < len(pool.consts) {
		cell := &pool.consts[idx]
		if h := cell.hit.Load(); h != nil &&
			*(*unsafe.Pointer)(unsafe.Pointer(&h.m)) == *(*unsafe.Pointer)(unsafe.Pointer(&m)) {
			return h.v
		}
		v := m[pool.List[idx]]
		cell.hit.Store(&simConstHit{m: m, v: v})
		return v
	}
	return m[pool.List[idx]]
}

// The command trigger checks its compile-time position hint by comparing the
// pooled name with the hinted group's first command name. Equal strings with
// different backing arrays compare byte by byte; a pool entry that names one
// of this character's command groups is replaced by that group's own name
// string (the same characters), so the comparison is a pointer check.
func simInternCommandNames(pool *StringPool, cl *CommandList) {
	if cl == nil {
		return
	}
	for k, s := range pool.List {
		if i, ok := cl.Names[s]; ok && i >= 0 && i < len(cl.Commands) && len(cl.Commands[i]) > 0 &&
			cl.Commands[i][0].name == s {
			pool.List[k] = cl.Commands[i][0].name
		}
	}
}
