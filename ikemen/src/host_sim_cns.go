package main

import "math"

func simU32(be BytecodeExp, i int) uint32 {
	return uint32(be[i]) | uint32(be[i+1])<<8 | uint32(be[i+2])<<16 | uint32(be[i+3])<<24
}

func simSamePrefix(a, b BytecodeExp, n int) bool {
	if len(a) < n || len(b) < n {
		return false
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Consecutive CNS controllers often share the same pure triggerall prefix.
// When it is false no controller can fire or change the prefix's inputs. Check
// it once, then retain each original block's exact persistent/hitpause entry.
// A true prefix uses the original blocks, including all original evaluations.
type simCNSGroup struct {
	blocks []compiledStateBlock
	prefix BytecodeExp
}

var simTriggerGroups = true
var simCNSFalseBlocks uint64

func simPurePrefix(be BytecodeExp) bool {
	constant, value := false, int32(0)
	for i := 0; i < len(be); {
		pc, op := i, be[i]
		i++
		previous, priorValue := constant, value
		constant = false
		switch op {
		case OC_int8:
			if i >= len(be) {
				return false
			}
			constant, value = true, int32(int8(be[i]))
			i++
		case OC_int:
			if i+4 > len(be) {
				return false
			}
			constant, value = true, int32(simU32(be, i))
			i += 4
		case OC_float:
			if i+4 > len(be) {
				return false
			}
			i += 4
		case OC_var, OC_sysvar, OC_fvar, OC_sysfvar:
			// Negative/dynamic indices can log a warning: they are not pure.
			if !previous || priorValue < 0 {
				return false
			}
		case OC_statetype, OC_movetype:
			if i >= len(be) {
				return false
			}
			i++
		case OC_jz8, OC_jnz8, OC_jmp8:
			if i >= len(be) || be[i] == 0 || pc+2+int(be[i]) > len(be) {
				return false
			}
			i++
		case OC_jz, OC_jnz, OC_jmp:
			if i+4 > len(be) {
				return false
			}
			n := int32(simU32(be, i))
			if n < 0 || pc+5+int(n) > len(be) {
				return false
			}
			i += 4
		case OC_eq, OC_ne, OC_gt, OC_ge, OC_lt, OC_le, OC_pop, OC_dup, OC_swap,
			OC_neg, OC_not, OC_blnot, OC_bland, OC_blxor, OC_blor, OC_and, OC_or, OC_xor,
			OC_add, OC_sub, OC_mul, OC_alive, OC_ctrl, OC_ailevel, OC_time, OC_stateno,
			OC_prevstateno, OC_anim, OC_life, OC_lifemax, OC_hitpausetime, OC_id,
			OC_roundstate, OC_gametime, OC_facing, OC_inguarddist, OC_rdreset:
		default:
			// Random, assignments, functions, redirects, command contexts,
			// division/modulo/power diagnostics and unrecognised ops stay stock.
			return false
		}
	}
	return len(be) > 0
}

func simCompileCNSGroups(ctrls []StateController) []StateController {
	result := make([]StateController, 0, len(ctrls))
	for i := 0; i < len(ctrls); {
		first, ok := ctrls[i].(compiledStateBlock)
		if !ok || first.simPrefixLen == 0 || first.loopBlock || first.elseBlock != nil {
			result = append(result, ctrls[i])
			i++
			continue
		}
		n := int(first.simPrefixLen)
		end := i + 1
		for end < len(ctrls) {
			next, ok := ctrls[end].(compiledStateBlock)
			if !ok || next.loopBlock || next.elseBlock != nil || int(next.simPrefixLen) != n ||
				!simSamePrefix(first.trigger, next.trigger, n) {
				break
			}
			end++
		}
		if end-i < 3 {
			result = append(result, ctrls[i])
			i++
			continue
		}
		group := &simCNSGroup{prefix: first.trigger[:n-1]}
		for j := i; j < end; j++ {
			group.blocks = append(group.blocks, ctrls[j].(compiledStateBlock))
		}
		result = append(result, group)
		i = end
	}
	return result
}

func (g *simCNSGroup) Run(c *Char, ps []int32) bool {
	if ovFFA && simTriggerGroups && !g.prefix.evalB(c) {
		for _, block := range g.blocks {
			block.skipFalseCNS(c, ps)
		}
		simCountFalseCNS(c, uint64(len(g.blocks)))
		return false
	}
	for _, block := range g.blocks {
		if block.Run(c, ps) {
			return true
		}
	}
	return false
}

func (b compiledStateBlock) skipFalseCNS(c *Char, ps []int32) {
	c.currentSctrlIndex = b.persistentIndex
	if c.hitPause() {
		if c.hitStateChangeIdx >= 0 && b.persistentIndex == c.hitStateChangeIdx &&
			c.stWgi().ikemenver[0] == 0 && c.stWgi().ikemenver[1] == 0 {
			return
		}
		if b.ignorehitpause < -1 {
			return
		}
	}
	if b.persistentIndex >= 0 {
		if ps[b.persistentIndex] != math.MaxInt32 {
			ps[b.persistentIndex]--
		}
		if ps[b.persistentIndex] > 0 {
			return
		}
	}
	c.vm().workingChar = c
}

// The existing hitpause ChangeState correction inspects immediate CNS blocks.
// Groups are only an execution container; expose their original block order.
func simCNSFlat(ctrls []StateController) []StateController {
	grouped := false
	for _, ctrl := range ctrls {
		if _, ok := ctrl.(*simCNSGroup); ok {
			grouped = true
			break
		}
	}
	if !grouped {
		return ctrls
	}
	flat := make([]StateController, 0, len(ctrls))
	for _, ctrl := range ctrls {
		if group, ok := ctrl.(*simCNSGroup); ok {
			for _, b := range group.blocks {
				flat = append(flat, b)
			}
		} else {
			flat = append(flat, ctrl)
		}
	}
	return flat
}
