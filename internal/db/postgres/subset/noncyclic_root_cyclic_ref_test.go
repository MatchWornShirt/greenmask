package subset

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/greenmaskio/greenmask/internal/db/postgres/entries"
)

// TestNonCyclicRootReferencingCycle reproduces the greenmask #329 follow-up:
// dumping a NON-cyclic table (public.OrderDetails) that REFERENCES a
// cyclic component (public.Orders, modelled here as a self-FK cycle) used to take
// the plain (non-CTE) generator, which joined the real "public"."Orders" table and
// passed an EMPTY overridden-tables map -> the executing DB returned
// `missing FROM-clause entry for table "Orders"`.
//
// The fix routes such a path to the SCC/CTE machinery (isPathForScc now inspects
// the whole closure), so:
//   - Orders is represented by its recursive __ids CTE,
//   - the OrderDetails -> Orders join references that CTE via an alias
//     (overriddenJoinAlias), never the real public.Orders table,
//   - the root OrderDetails__ids CTE is emitted and the final assembly
//     filters OrderDetails by it.
func TestNonCyclicRootReferencingCycle(t *testing.T) {
	// Cyclic component: public.Orders (oid 30) with a self FK (Orders.ParentOrderId -> Orders.Id).
	orders := newTestTable(30, "public", "Orders", []string{"Id"})
	orders.SubsetConds = []string{`"public"."Orders"."Status" = 'open'`}

	// Non-cyclic root: public.OrderDetails (oid 40). FK OrderId -> Orders.Id.
	pbd := newTestTable(40, "public", "OrderDetails", []string{"Id"})
	pbd.SubsetConds = []string{`"public"."OrderDetails"."Deleted" = false`}

	const (
		ordersIdx = 0
		pbdIdx    = 1
	)

	// Non-nullable self edge: genuinely forces a cycle (nullable self-loops are not cycle-forming).
	ordersSelfEdge := NewEdge(
		1, ordersIdx, false,
		NewTableLink(ordersIdx, orders, NewKeysByColumn([]string{"ParentOrderId"}), nil),
		NewTableLink(ordersIdx, orders, NewKeysByColumn([]string{"Id"}), nil),
	)
	cyclicOrders := NewComponent(
		0,
		map[int][]*Edge{ordersIdx: {ordersSelfEdge}},
		map[int]*entries.Table{ordersIdx: orders},
	)
	require.True(t, cyclicOrders.hasCycle(), "Orders component must be cyclic")
	require.Len(t, cyclicOrders.groupedCycles, 1)

	// Non-cyclic root component: OrderDetails (single table, no edges).
	nonCyclicPbd := NewComponent(
		1,
		map[int][]*Edge{pbdIdx: nil},
		map[int]*entries.Table{pbdIdx: pbd},
	)
	require.False(t, nonCyclicPbd.hasCycle(), "OrderDetails component must be non-cyclic")

	// Edge OrderDetails -> Orders (OrderDetails.OrderId references Orders.Id).
	pbdToOrders := NewEdge(
		2, ordersIdx, false,
		NewTableLink(pbdIdx, pbd, NewKeysByColumn([]string{"OrderId"}), nil),
		NewTableLink(ordersIdx, orders, NewKeysByColumn([]string{"Id"}), nil),
	)
	condEdge := NewCondensedEdge(
		0,
		NewComponentLink(1, nonCyclicPbd), // from: non-cyclic root
		NewComponentLink(0, cyclicOrders), // to: cyclic Orders
		pbdToOrders,
	)

	g := &Graph{
		scc: []*Component{cyclicOrders, nonCyclicPbd},
	}

	// Path rooted at the NON-cyclic OrderDetails (rootVertex = scc idx 1),
	// with a child scope holding the cyclic Orders. Edges to a cyclic component fork
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

	// The closure contains a cycle (Orders) even though the root is non-cyclic,
	// so the path MUST be routed to the SCC/CTE generator.
	require.True(t, isPathForScc(path, g), "non-cyclic root referencing a cycle must use the SCC generator")

	// Generate the query for the root table via the CTE machinery; must not panic.
	require.NotPanics(t, func() {
		g.generateAndSetQueryForScc(path)
	})

	full := pbd.Query
	require.NotEmpty(t, full, "expected a generated query for OrderDetails")
	t.Logf("assembled OrderDetails query:\n%s", full)

	// Root __ids CTE for the non-cyclic table must be present and drive the final filter.
	require.Contains(t, full, "public__OrderDetails__ids")
	require.Contains(t, full, `SELECT * FROM "public"."OrderDetails" WHERE`)
	require.Contains(t, full, `"public"."OrderDetails"."Deleted" = false`)

	// The recursive Orders CTE must be present (cyclic component represented by its CTE).
	require.Contains(t, full, "WITH RECURSIVE")
	require.Contains(t, full, "public__Orders__ids")

	// CRITICAL: the OrderDetails -> Orders join must reference the overridden
	// __ids CTE via the per-edge alias, NOT the real public.Orders table.
	overrideName := "public__Orders__ids"
	alias := overriddenJoinAlias(pbdToOrders, overrideName)
	require.Contains(t, full, alias, "join to Orders must use the overridden __ids alias")

	// The real Orders table must never appear as a direct JOIN/FROM target in the
	// non-cyclic root scope (it would be the "missing FROM-clause entry" bug). It
	// is legitimate for it to appear inside the recursive Orders CTE body, so we only
	// assert it is not joined directly in the OrderDetails __ids item.
	var pbdIds string
	cq := newCteQuery(nonCyclicPbd)
	g.generateQueriesSccDfs(cq, path, nil)
	for _, it := range cq.items {
		if it.name == "public__OrderDetails__ids" {
			pbdIds = it.query
		}
	}
	require.NotEmpty(t, pbdIds, "expected public__OrderDetails__ids item")
	require.NotContains(t, pbdIds, `JOIN "public"."Orders" ON`,
		"root scope must not join the real Orders table; got: %s", pbdIds)
	require.Contains(t, pbdIds, alias, "root scope join must use the __ids alias")

	// The real, qualified public.Orders table must not be referenced ANYWHERE in the
	// root __ids item (not in FROM, JOIN, nor a leaked SubsetCond). Orders is only
	// reachable through its __ids CTE here. This guards the regression where the
	// overridden TO-table's SubsetConds leaked `"public"."Orders"."Status" = 'open'`
	// into the join condition, referencing a table not in FROM.
	require.NotContains(t, pbdIds, `"public"."Orders".`,
		"root scope must not reference the real public.Orders table; got: %s", pbdIds)

	// Sanity: the alias-based join references the override CTE name.
	require.True(t, strings.Contains(pbdIds, overrideName),
		"root scope must join the Orders __ids CTE; got: %s", pbdIds)
}
