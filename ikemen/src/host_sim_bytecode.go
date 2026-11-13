package main

import "unsafe"

// Extend the existing VM's literal-consumer fusion. The original byte stream,
// redirects, variable accessors, error behaviour and RNG remain authoritative.
func (be BytecodeExp) intComparison(value int32, at int) (bool, int) {
	var literal int32
	var cmp OpCode
	n := 0
	if at+2 < len(be) && be[at] == OC_int8 {
		literal, cmp, n = int32(int8(be[at+1])), be[at+2], 3
	} else if at+5 < len(be) && be[at] == OC_int {
		literal = *(*int32)(unsafe.Pointer(&be[at+1]))
		cmp, n = be[at+5], 6
	}
	if n == 0 {
		return false, 0
	}
	switch cmp {
	case OC_eq:
		return value == literal, n
	case OC_ne:
		return value != literal, n
	case OC_gt:
		return value > literal, n
	case OC_ge:
		return value >= literal, n
	case OC_lt:
		return value < literal, n
	case OC_le:
		return value <= literal, n
	}
	return false, 0
}

// CNS triggerall uses an exit jump followed by pop. A producer can consume
// these without materialising a stack value. A jump exits the expression with
// exactly that value; a fall-through discards it and continues the byte stream.
func (be BytecodeExp) booleanExit(result bool, at *int) (exit, consumed bool) {
	i := *at
	if i+2 >= len(be) || (be[i] != OC_jz8 && be[i] != OC_jnz8) || be[i+1] != 0 || be[i+2] != OC_pop {
		return false, false
	}
	if result != (be[i] == OC_jz8) {
		return true, true
	}
	*at += 3
	return false, true
}

// booleanExit by value: runVM's cursor must never be address-taken, or Go
// keeps it in memory for the whole interpreter loop.
func (be BytecodeExp) booleanExitI(result bool, i int) (exit, consumed bool, next int) {
	if i+2 >= len(be) || (be[i] != OC_jz8 && be[i] != OC_jnz8) || be[i+1] != 0 || be[i+2] != OC_pop {
		return false, false, i
	}
	if result != (be[i] == OC_jz8) {
		return true, true, i
	}
	return false, true, i + 3
}
