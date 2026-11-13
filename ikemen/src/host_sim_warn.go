package main

import "strconv"

// Runtime warnings. Pack state code reaches redirect
// warnings ("has no target ...", "has no root", "found no player ...") many
// times a tick; each built its text with fmt.Sprintf on the state worker.
// These build the identical text (%v of an int/int32 is its decimal form, of
// a string the string itself) with strconv appends.

// fmt.Sprintf("%v: WARNING: %v (%v) in state %v: ", sys.tickCount, c.name, c.id, c.ss.no)
func simWarnPrefix(c *Char) string {
	var buf [128]byte
	b := strconv.AppendInt(buf[:0], int64(sys.tickCount), 10)
	b = append(b, ": WARNING: "...)
	b = append(b, c.name...)
	b = append(b, " ("...)
	b = strconv.AppendInt(b, int64(c.id), 10)
	b = append(b, ") in state "...)
	b = strconv.AppendInt(b, int64(c.ss.no), 10)
	b = append(b, ": "...)
	return string(b)
}

// prefix + fmt.Sprintf(format with %v at the two places, a, b), for the
// "<text> %v <text> %v" redirect warnings: head + a + mid + b.
func simWarn2(prefix, head string, a int64, mid string, b int64) string {
	var buf [192]byte
	s := append(buf[:0], prefix...)
	s = append(s, head...)
	s = strconv.AppendInt(s, a, 10)
	s = append(s, mid...)
	s = strconv.AppendInt(s, b, 10)
	return string(s)
}

func simWarn1(prefix, head string, a int64) string {
	var buf [160]byte
	s := append(buf[:0], prefix...)
	s = append(s, head...)
	s = strconv.AppendInt(s, a, 10)
	return string(s)
}
