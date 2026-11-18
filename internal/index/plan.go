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

// Change is one step of a plan. Index is the new definition, or for Drop the
// existing index; Old is the index a Replace drops.
type Change struct {
	Action Action
	Index  Index
	Old    Index
}

// Plan is the set of changes that brings a database's indexes in line with the desired ones.
type Plan struct {
	Changes   []Change // drops, then replacements, then creations
	Unmanaged []Index  // in the database but not desired; kept unless pruning
}

// Empty reports whether the plan changes nothing.
func (p Plan) Empty() bool { return len(p.Changes) == 0 }

// Prune says which undeclared indexes a plan drops.
type Prune int

// The prune modes. A UNIQUE index enforces a constraint, so it needs its own.
const (
	PruneNone      Prune = iota
	PruneNonUnique       // drop undeclared indexes that aren't UNIQUE
	PruneAll             // drop every undeclared index
)

// Diff plans the changes that turn the actual indexes into the desired ones,
// matching them by name. Unmatched actual indexes are dropped as prune says.
func Diff(desired, actual []Index, prune Prune) Plan {
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
			p.Changes = append(p.Changes, Change{Action: Create, Index: want})
		case !have.Equal(want):
			p.Changes = append(p.Changes, Change{Action: Replace, Index: want, Old: have})
		}
	}
	for _, ix := range existing {
		if prune == PruneAll || prune == PruneNonUnique && !ix.Unique {
			p.Changes = append(p.Changes, Change{Action: Drop, Index: ix})
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
