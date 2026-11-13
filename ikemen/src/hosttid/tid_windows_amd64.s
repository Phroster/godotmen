#include "textflag.h"

// func ID() uint64
// GetCurrentThreadId itself: gs:[0x30] is the TEB's own address (NT_TIB.Self), [TEB+0x48] its ClientId.UniqueThread.
TEXT ·ID(SB),NOSPLIT|NOFRAME,$0-8
	MOVQ	$0x30, CX
	MOVQ	0(CX)(GS*1), AX
	MOVL	0x48(AX), AX
	MOVQ	AX, ret+0(FP)
	RET
