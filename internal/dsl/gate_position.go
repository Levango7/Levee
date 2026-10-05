// Gate positions: the one table that decides WHEN a declared check runs.
//
// A gate declaration names a position keyword (leveelang-spec.md §2.2 modifier
// table: pre_apply / post_batch / post_apply) and the position picks which
// GateSpec slot holds the check. The slot is load-bearing, not cosmetic:
// internal/engine maps slot → verify.GatePhase, and RunPhase only executes the
// phases it visits. Before this table existed, convertGate ignored position
// entirely and filed every step `verify:` and `batches.gate` check in
// GateSpec.Post, so `position: post_batch` actually ran once at the end, and the
// Pre and Batch slots were unreachable from any YAML — while the engine's
// post_batch machinery was exercised only by hand-built ASTs in tests.
package dsl

import (
	"fmt"
	"sort"
	"strings"
)

// GateSlot is the position a check occupies inside a GateSpec.
type GateSlot int

const (
	// GateSlotPre runs before the change touches anything.
	GateSlotPre GateSlot = iota
	// GateSlotBatch runs after every batch, i.e. between batches.
	GateSlotBatch
	// GateSlotPost runs once, after the whole change.
	GateSlotPost
)

// gateSlotsByPosition is the authoritative position → slot mapping. Adding a
// keyword here is the only way to add a position: the parser, both declaration
// sites and the advisory that asks "did this workflow declare a between-batches
// check at all" all read this table.
var gateSlotsByPosition = map[string]GateSlot{
	"pre_apply":  GateSlotPre,
	"post_batch": GateSlotBatch,
	"post_apply": GateSlotPost,
}

// GatePositions lists the accepted position keywords in sorted order, for error
// text and for the cross-layer vocabulary guard.
func GatePositions() []string {
	out := make([]string, 0, len(gateSlotsByPosition))
	for k := range gateSlotsByPosition {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// IsGatePosition reports whether the keyword names a position at all.
func IsGatePosition(position string) bool {
	_, ok := gateSlotsByPosition[position]
	return ok
}

// AppendGateCheck routes one converted check into the slot its position names.
//
// An unknown keyword is refused (LE051) rather than defaulted: a check filed in
// the wrong slot still appears in the plan and in plan_hash, so the workflow
// would look gated while the engine visits a different phase.
//
// defaultSlot applies when the declaration carries no position, and each
// declaration site passes its own because their defaults are not the same thing:
// a step `verify:` block checks the step's outcome, so it is a post_apply check
// unless told otherwise, while `batches.gate` is by definition the
// between-batches check (§4.3 calls it "gate post_batch").
func AppendGateCheck(spec *GateSpec, position string, gc GateCheck, defaultSlot GateSlot) error {
	slot := defaultSlot
	if position != "" {
		known, ok := gateSlotsByPosition[position]
		if !ok {
			return newError("LE051", "gate.position", fmt.Sprintf(
				"unknown gate position %q (must be one of %s)", position, strings.Join(GatePositions(), "|")))
		}
		slot = known
	}
	switch slot {
	case GateSlotPre:
		spec.Pre = append(spec.Pre, gc)
	case GateSlotBatch:
		spec.Batch = append(spec.Batch, gc)
	case GateSlotPost:
		spec.Post = append(spec.Post, gc)
	}
	return nil
}

// HasBatchCheck reports whether any of these specs holds a between-batches
// check. It is what the LE052 advisory asks.
func HasBatchCheck(specs ...*GateSpec) bool {
	for _, s := range specs {
		if s != nil && len(s.Batch) > 0 {
			return true
		}
	}
	return false
}
