package subset

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/greenmaskio/greenmask/internal/db/postgres/entries"
)

// TestNonCyclicRootReferencingCycle reproduces the greenmask #329 follow-up:
// dumping a NON-cyclic table (public.ProductBiddingDetails) that REFERENCES a
// cyclic component (public.Bids, modelled here as a self-FK cycle) used to take
// the plain (non-CTE) generator, which joined the real "public"."Bids" table and
// passed an EMPTY overridden-tables map -> the executing DB returned
// `missing FROM-clause entry for table "Bids"`.
//
// The fix routes such a path to the SCC/CTE machinery (isPathForScc now inspects
// the whole closure), so:
//   - Bids is represented by its recursive __ids CTE,
//   - the ProductBiddingDetails -> Bids join references that CTE via an alias
//     (overriddenJoinAlias), never the real public.Bids table,
//   - the root ProductBiddingDetails__ids CTE is emitted and the final assembly
//     filters ProductBiddingDetails by it.
func TestNonCyclicRootReferencingCycle(t *testing.T) {
	// Cyclic component: public.Bids (oid 30) with a self FK (Bids.ParentBidId -> Bids.Id).
	bids := newTestTable(30, "public", "Bids", []string{"Id"})
	bids.SubsetConds = []string{`"public"."Bids"."Status" = 'open'`}

	// Non-cyclic root: public.ProductBiddingDetails (oid 40). FK BidId -> Bids.Id.
	pbd := newTestTable(40, "public", "ProductBiddingDetails", []string{"Id"})
	pbd.SubsetConds = []string{`"public"."ProductBiddingDetails"."Deleted" = false`}

	const (
		bidsIdx = 0
		pbdIdx  = 1
	)

	// Self edge for Bids (cycle of length 1): Bids.ParentBidId -> Bids.Id.
	bidsSelfEdge := NewEdge(
		1, bidsIdx, true, // self FK is nullable (root bids have no parent)
		NewTableLink(bidsIdx, bids, NewKeysByColumn([]string{"ParentBidId"}), nil),
		NewTableLink(bidsIdx, bids, NewKeysByColumn([]string{"Id"}), nil),
	)
	cyclicBids := NewComponent(
		0,
		map[int][]*Edge{bidsIdx: {bidsSelfEdge}},
		map[int]*entries.Table{bidsIdx: bids},
	)
	require.True(t, cyclicBids.hasCycle(), "Bids component must be cyclic")
	require.Len(t, cyclicBids.groupedCycles, 1)

	// Non-cyclic root component: ProductBiddingDetails (single table, no edges).
	nonCyclicPbd := NewComponent(
		1,
		map[int][]*Edge{pbdIdx: nil},
		map[int]*entries.Table{pbdIdx: pbd},
	)
	require.False(t, nonCyclicPbd.hasCycle(), "ProductBiddingDetails component must be non-cyclic")

	// Edge ProductBiddingDetails -> Bids (ProductBiddingDetails.BidId references Bids.Id).
	pbdToBids := NewEdge(
		2, bidsIdx, false,
		NewTableLink(pbdIdx, pbd, NewKeysByColumn([]string{"BidId"}), nil),
		NewTableLink(bidsIdx, bids, NewKeysByColumn([]string{"Id"}), nil),
	)
	condEdge := NewCondensedEdge(
		0,
		NewComponentLink(1, nonCyclicPbd), // from: non-cyclic root
		NewComponentLink(0, cyclicBids),   // to: cyclic Bids
		pbdToBids,
	)

	g := &Graph{
		scc: []*Component{cyclicBids, nonCyclicPbd},
	}

	// Path rooted at the NON-cyclic ProductBiddingDetails (rootVertex = scc idx 1),
	// with a child scope holding the cyclic Bids. Edges to a cyclic component fork
	// into a child scope, mirroring how path.go builds it at runtime.
	path := NewPath(1)
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

	// The closure contains a cycle (Bids) even though the root is non-cyclic,
	// so the path MUST be routed to the SCC/CTE generator.
	require.True(t, isPathForScc(path, g), "non-cyclic root referencing a cycle must use the SCC generator")

	// Generate the query for the root table via the CTE machinery; must not panic.
	require.NotPanics(t, func() {
		g.generateAndSetQueryForScc(path)
	})

	full := pbd.Query
	require.NotEmpty(t, full, "expected a generated query for ProductBiddingDetails")
	t.Logf("assembled ProductBiddingDetails query:\n%s", full)

	// Root __ids CTE for the non-cyclic table must be present and drive the final filter.
	require.Contains(t, full, "public__ProductBiddingDetails__ids")
	require.Contains(t, full, `SELECT * FROM "public"."ProductBiddingDetails" WHERE`)
	require.Contains(t, full, `"public"."ProductBiddingDetails"."Deleted" = false`)

	// The recursive Bids CTE must be present (cyclic component represented by its CTE).
	require.Contains(t, full, "WITH RECURSIVE")
	require.Contains(t, full, "public__Bids__ids")

	// CRITICAL: the ProductBiddingDetails -> Bids join must reference the overridden
	// __ids CTE via the per-edge alias, NOT the real public.Bids table.
	overrideName := "public__Bids__ids"
	alias := overriddenJoinAlias(pbdToBids, overrideName)
	require.Contains(t, full, alias, "join to Bids must use the overridden __ids alias")

	// The real Bids table must never appear as a direct JOIN/FROM target in the
	// non-cyclic root scope (it would be the "missing FROM-clause entry" bug). It
	// is legitimate for it to appear inside the recursive Bids CTE body, so we only
	// assert it is not joined directly in the ProductBiddingDetails __ids item.
	var pbdIds string
	cq := newCteQuery(nonCyclicPbd)
	g.generateQueriesSccDfs(cq, path, nil)
	for _, it := range cq.items {
		if it.name == "public__ProductBiddingDetails__ids" {
			pbdIds = it.query
		}
	}
	require.NotEmpty(t, pbdIds, "expected public__ProductBiddingDetails__ids item")
	require.NotContains(t, pbdIds, `JOIN "public"."Bids" ON`,
		"root scope must not join the real Bids table; got: %s", pbdIds)
	require.Contains(t, pbdIds, alias, "root scope join must use the __ids alias")

	// The real, qualified public.Bids table must not be referenced ANYWHERE in the
	// root __ids item (not in FROM, JOIN, nor a leaked SubsetCond). Bids is only
	// reachable through its __ids CTE here. This guards the regression where the
	// overridden TO-table's SubsetConds leaked `"public"."Bids"."Status" = 'open'`
	// into the join condition, referencing a table not in FROM.
	require.NotContains(t, pbdIds, `"public"."Bids".`,
		"root scope must not reference the real public.Bids table; got: %s", pbdIds)

	// Sanity: the alias-based join references the override CTE name.
	require.True(t, strings.Contains(pbdIds, overrideName),
		"root scope must join the Bids __ids CTE; got: %s", pbdIds)
}
