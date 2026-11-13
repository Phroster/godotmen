package main

// Memoize only CNS expressions made of explicitly listed runtime numeric
// triggers/operators. Command names, strings, constants, maps, local variables,
// ZSS and all unknown identifiers go through the original compiler every time.
// Each returned bytecode is a fresh slice: callers append operators to it.
import (
	"fmt"
	"strings"
	"sync"
)

var ovCompileCacheOn, ovCompileCacheVerify bool

type ovExpressionKey struct {
	text, previous                          string
	valueType                               ValueType
	norange, reverse, ignoreErrors, inBlock bool
	ikemen                                  [3]uint16
	mugen                                   [2]uint16
}
type ovExpressionValue struct {
	code                  BytecodeExp
	rest, previous, token string
	norange, reverse      bool
}

func ovSameBytecode(a, b BytecodeExp) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var ovExpressions = struct {
	sync.Mutex
	entries                map[ovExpressionKey]ovExpressionValue
	hits, misses, verified uint64
}{entries: make(map[ovExpressionKey]ovExpressionValue)}

var ovNumericTriggers = func() map[string]bool {
	m := make(map[string]bool)
	for _, name := range strings.Fields(`true false pi e s c a l u n h i
		abs ceil floor sin cos tan asin acos atan exp ln log ifelse cond
		var fvar sysvar sysfvar time gametime roundstate roundno roundsExisted
		life lifemax power powermax ctrl stateno prevstateno statetype movetype
		anim animtime animelem animelemno animelemtime animexist selfanimexist
		pos vel x y z facing id alive random canrecover ishelper
		numhelper numtarget numexplod numproj numprojid numenemy numpartner
		movehit moveguarded movecontact movereversed hitcount uniqhitcount
		hitpausetime hitshakeover hitfall hitover inguarddist gethitvar
		p2stateno p2statetype p2movetype p2life p2dist p2bodydist
		root parent helper target enemy enemynear playerid
		fall yvel xvel zvel hittime guarded animtype airtype groundtype
		damage recover hitshaketime fallcount hitid chainid
		win lose draw matchover tickspersecond`) {
		m[strings.ToLower(name)] = true
	}
	return m
}()

func ovNumericExpression(text string) bool {
	for i := 0; i < len(text); {
		b := text[i]
		if b == '"' || b == '\'' || b == '#' || b >= 128 {
			return false
		}
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b == '_' {
			start := i
			i++
			for i < len(text) {
				b = text[i]
				if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '.') {
					break
				}
				i++
			}
			if !ovNumericTriggers[strings.ToLower(text[start:i])] {
				return false
			}
		} else {
			i++
		}
	}
	return true
}

func ovCompileExpression(c *CharCompiler, in *string, vt ValueType) (BytecodeExp, error) {
	if !ovCompileCacheOn || c.zssMode || len(c.vars) > 0 || !ovNumericExpression(*in) {
		return c.fullExpressionUncached(in, vt)
	}
	gi := &sys.cgi[c.playerNo]
	key := ovExpressionKey{text: *in, previous: c.previousOperator, valueType: vt, norange: c.norange,
		reverse: c.reverseOrder, ignoreErrors: sys.ignoreMostErrors, inBlock: c.block != nil,
		ikemen: gi.ikemenver, mugen: gi.mugenver}
	ovExpressions.Lock()
	entry, hit := ovExpressions.entries[key]
	ovExpressions.Unlock()
	if hit {
		if ovCompileCacheVerify {
			code, err := c.fullExpressionUncached(in, vt)
			if err != nil || !ovSameBytecode(code, entry.code) || *in != entry.rest || c.token != entry.token ||
				c.previousOperator != entry.previous || c.norange != entry.norange || c.reverseOrder != entry.reverse {
				return nil, fmt.Errorf("numeric expression cache differs from original compiler: %q (original error %v)", key.text, err)
			}
			ovExpressions.Lock()
			ovExpressions.hits++
			ovExpressions.verified++
			ovExpressions.Unlock()
			return code, nil
		}
		*in, c.previousOperator, c.token, c.norange, c.reverseOrder = entry.rest, entry.previous, entry.token, entry.norange, entry.reverse
		ovExpressions.Lock()
		ovExpressions.hits++
		ovExpressions.Unlock()
		return append(BytecodeExp(nil), entry.code...), nil
	}
	poolSize := len(sys.stringPool[c.playerNo].List)
	code, err := c.fullExpressionUncached(in, vt)
	if err != nil {
		return code, err
	}
	// An additional conservative guard against accidentally broadening the list.
	if len(sys.stringPool[c.playerNo].List) != poolSize {
		return code, nil
	}
	key.text = strings.Clone(key.text)
	key.previous = strings.Clone(key.previous)
	entry = ovExpressionValue{code: append(BytecodeExp(nil), code...), rest: strings.Clone(*in),
		previous: strings.Clone(c.previousOperator), token: strings.Clone(c.token), norange: c.norange, reverse: c.reverseOrder}
	ovExpressions.Lock()
	if len(ovExpressions.entries) >= 32768 {
		ovExpressions.entries = make(map[ovExpressionKey]ovExpressionValue)
	}
	ovExpressions.entries[key] = entry
	ovExpressions.misses++
	ovExpressions.Unlock()
	return code, nil
}

func ovExpressionCounts() map[string]uint64 {
	ovExpressions.Lock()
	defer ovExpressions.Unlock()
	return map[string]uint64{"hits": ovExpressions.hits, "misses": ovExpressions.misses,
		"verified_equal": ovExpressions.verified, "entries": uint64(len(ovExpressions.entries))}
}
