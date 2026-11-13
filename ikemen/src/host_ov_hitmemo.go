package main

// Vestigial: the collision pass used to memoize a per-slot hit gate here. No gate is left in this build, so the pass
// takes nothing and every pair may hit. Remove together with its callers (CharList.collisionDetection in char.go and
// ovMayHit in host_ov_core.go).

var ovHitMemo struct{ on bool }

func ovHitMemoBegin() bool { return false }

func ovHitMemoEnd() {}

func ovMayHitMemo(attackerPn, getterPn int) bool { return true }
