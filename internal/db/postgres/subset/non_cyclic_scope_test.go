package subset

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/greenmaskio/greenmask/internal/db/postgres/entries"
	"github.com/greenmaskio/greenmask/pkg/toolkit"
)

// newTestTable builds a minimal *entries.Table usable by the query generator.
func newTestTable(oid toolkit.Oid, schema, name string, pk []string) *entries.Table {
	return &entries.Table{
		Table: &toolkit.Table{
			Oid:        oid,
			Schema:     schema,
			Name:       name,
			PrimaryKey: pk,
		},
	}
}

// TestGenerateQueryForScc_NonCyclicChildScope reproduces greenmask issue #329:
// a path rooted at a CYCLIC component (here a self-referencing table) that also
// contains a NON-cyclic child scope (a single table) used to panic in
// getOneCycleGroup. The fix routes the non-cyclic scope to
// generateQueryForNonCyclicScope, which emits a "schema__table__ids" CTE item.
func TestGenerateQueryForScc_NonCyclicChildScope(t *testing.T) {
	// Cyclic root component: public.parts (oid 10) with a self FK (parts.parent_id -> parts.id)
	parts := newTestTable(10, "public", "parts", []string{"id"})
	parts.SubsetConds = []string{`"public"."parts"."deleted" = false`}
	// Non-cyclic child component: public.kinds (oid 20). parts.kind_id -> kinds.id
	kinds := newTestTable(20, "public", "kinds", []string{"id"})
	kinds.SubsetConds = []string{`"public"."kinds"."active" = true`}

	const (
		partsIdx = 0
		kindsIdx = 1
	)

	// Self edge for parts (cycle of length 1).
	selfEdge := NewEdge(
		1, partsIdx, false,
		NewTableLink(partsIdx, parts, NewKeysByColumn([]string{"parent_id"}), nil),
		NewTableLink(partsIdx, parts, NewKeysByColumn([]string{"id"}), nil),
	)

	// Cyclic component for parts.
	cyclicComp := NewComponent(
		0,
		map[int][]*Edge{partsIdx: {selfEdge}},
		map[int]*entries.Table{partsIdx: parts},
	)
	require.True(t, cyclicComp.hasCycle(), "parts component must be cyclic")
	require.Len(t, cyclicComp.groupedCycles, 1)

	// Non-cyclic component for kinds (single table, no edges).
	nonCyclicComp := NewComponent(
		1,
		map[int][]*Edge{kindsIdx: nil},
		map[int]*entries.Table{kindsIdx: kinds},
	)
	require.False(t, nonCyclicComp.hasCycle(), "kinds component must be non-cyclic")

	// Edge parts -> kinds (parts.kind_id references kinds.id).
	partsToKinds := NewEdge(
		2, kindsIdx, false,
		NewTableLink(partsIdx, parts, NewKeysByColumn([]string{"kind_id"}), nil),
		NewTableLink(kindsIdx, kinds, NewKeysByColumn([]string{"id"}), nil),
	)
	condEdge := NewCondensedEdge(
		0,
		NewComponentLink(0, cyclicComp),
		NewComponentLink(1, nonCyclicComp),
		partsToKinds,
	)

	g := &Graph{
		scc: []*Component{cyclicComp, nonCyclicComp},
	}

	// Build the path: root scope (cyclic) plus a child scope holding the non-cyclic kinds.
	path := NewPath(0)
	path.scopeEdges = map[int][]*CondensedEdge{
		rootScopeId: nil,
		1:           {condEdge},
	}
	path.scopeEdgesNullable = map[int]map[int]bool{
		rootScopeId: {},
		1:           {},
	}
	childScopeEdge := &ScopeEdge{
		scopeId:               1,
		originalCondensedEdge: condEdge,
		isNullable:            false,
	}
	path.scopeGraph = map[int][]*ScopeEdge{
		rootScopeId: {childScopeEdge},
		1:           nil,
	}

	cq := newCteQuery(cyclicComp)

	require.NotPanics(t, func() {
		g.generateQueriesSccDfs(cq, path, nil)
	})

	// The non-cyclic kinds table must have produced its "__ids" CTE item.
	var kindsItem *cteItem
	for _, item := range cq.items {
		if item.name == "public__kinds__ids" {
			kindsItem = item
		}
	}
	require.NotNil(t, kindsItem, "expected public__kinds__ids CTE item for the non-cyclic scope")
	require.NotEmpty(t, kindsItem.query)

	// Well-formedness checks on the generated non-cyclic __ids item.
	q := kindsItem.query
	require.True(t, strings.HasPrefix(q, "SELECT DISTINCT"), "got: %s", q)
	require.Contains(t, q, `FROM "public"."kinds"`)
	require.Contains(t, q, `"public"."kinds"."id" AS "id"`)
	require.Contains(t, q, `"public"."kinds"."active" = true`) // own subset cond

	// The full query for parts must assemble without panicking and reference kinds ids.
	require.NotPanics(t, func() {
		_ = cq.generateQuery(parts)
	})
	full := cq.generateQuery(parts)
	require.Contains(t, full, "public__kinds__ids")
	t.Logf("non-cyclic __ids item:\n%s", q)
	t.Logf("assembled parts query:\n%s", full)
}
