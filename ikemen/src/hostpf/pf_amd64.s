#include "textflag.h"

// func Lines(p unsafe.Pointer, offs *uint16, n int)
// PREFETCHT0 of p+offs[i] for i < n: a hint, never a fault, no architectural effect.
TEXT ·Lines(SB),NOSPLIT|NOFRAME,$0-24
	MOVQ	p+0(FP), AX
	MOVQ	offs+8(FP), BX
	MOVQ	n+16(FP), CX
	TESTQ	AX, AX
	JZ	done
	TESTQ	CX, CX
	JLE	done
loop:
	MOVWQZX	(BX), DX
	PREFETCHT0	(AX)(DX*1)
	ADDQ	$2, BX
	DECQ	CX
	JNZ	loop
done:
	RET

// func T0x4(a, b, c, d unsafe.Pointer)
// PREFETCHT0 of four addresses (nil allowed: a prefetch never faults).
TEXT ·T0x4(SB),NOSPLIT|NOFRAME,$0-32
	MOVQ	a+0(FP), AX
	PREFETCHT0	(AX)
	MOVQ	b+8(FP), AX
	PREFETCHT0	(AX)
	MOVQ	c+16(FP), AX
	PREFETCHT0	(AX)
	MOVQ	d+24(FP), AX
	PREFETCHT0	(AX)
	RET
