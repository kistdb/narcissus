package index

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/kistdb/narcissus/internal/sqltext"
)

// Action is what a Change does to an index.
type Action int

// The actions, in the order a plan applies them: drops first, so names are free.
const (
	Drop Action = iota + 1
	Replace
	Create
)

func (a Action) String() string {
	switch a {
	case Drop:
		return "drop"
	case Replace:
		return "replace"
	case Create:
		return "create"
	default:
		return fmt.Sprintf("Action(%d)", int(a))
	}
}

// Change is one step of a plan. Index is the new definition, or for Drop the existing index.
type Change struct {
	Action Action
	Index  Index
}

// Plan is the set of changes that brings a database's indexes in line with the desired ones.
type Plan struct {
	Changes   []Change // drops, then replacements, then creations
	Unmanaged []Index  // in the database but not desired; kept unless pruning
}

// Empty reports whether the plan changes nothing.
func (p Plan) Empty() bool { return len(p.Changes) == 0 }

// Diff plans the changes that turn the actual indexes into the desired ones,
// matching them by name. Unmatched actual indexes are dropped only if prune is set.
func Diff(desired, actual []Index, prune bool) Plan {
	existing := make(map[string]Index, len(actual))
	for _, ix := range actual {
		existing[ix.key()] = ix
	}
	var p Plan
	for _, want := range desired {
		have, ok := existing[want.key()]
		delete(existing, want.key())
		switch {
		case !ok:
			p.Changes = append(p.Changes, Change{Create, want})
		case !have.Equal(want):
			p.Changes = append(p.Changes, Change{Replace, want})
		}
	}
	for _, ix := range existing {
		if prune {
			p.Changes = append(p.Changes, Change{Drop, ix})
		} else {
			p.Unmanaged = append(p.Unmanaged, ix)
		}
	}
	slices.SortFunc(p.Changes, func(a, b Change) int {
		return cmp.Or(cmp.Compare(a.Action, b.Action), Compare(a.Index, b.Index))
	})
	slices.SortFunc(p.Unmanaged, Compare)
	return p
}

// Compare orders indexes by table, then name, ignoring case.
func Compare(a, b Index) int {
	return cmp.Or(
		cmp.Compare(sqltext.Fold(a.Table), sqltext.Fold(b.Table)),
		cmp.Compare(a.key(), b.key()),
	)
}
