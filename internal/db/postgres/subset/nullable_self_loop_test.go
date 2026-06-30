package subset

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/greenmaskio/greenmask/internal/db/postgres/entries"
)

// TestNullableSelfLoopIsNonCyclic verifies that a table whose ONLY cyclic edge
// is a NULLABLE self-referential FK (parent_id -> id) is treated as a non-cyclic
// single-table component: hasCycle() is false and the generated query is the
// plain join form with no WITH RECURSIVE. The self-FK column is still part of
// the table and dumped via SELECT *.
func TestNullableSelfLoopIsNonCyclic(t *testing.T) {
	const nodeIdx = 0
	node := newTestTable(10, "public", "Node", []string{"id"})
	node.SubsetConds = []string{`"public"."Node"."deleted" = false`}

	// Nullable self FK: Node.parent_id -> Node.id.
	selfEdge := NewEdge(
		1, nodeIdx, true,
		NewTableLink(nodeIdx, node, NewKeysByColumn([]string{"parent_id"}), nil),
		NewTableLink(nodeIdx, node, NewKeysByColumn([]string{"id"}), nil),
	)

	comp := NewComponent(
		0,
		map[int][]*Edge{nodeIdx: {selfEdge}},
		map[int]*entries.Table{nodeIdx: node},
	)
	require.False(t, comp.hasCycle(), "nullable self-loop must NOT make the component cyclic")
	require.Empty(t, comp.cycles)

	// A non-cyclic single-table path generates the plain query.
	g := &Graph{scc: []*Component{comp}}
	path := NewPath(0)
	path.scopeEdges = map[int][]*CondensedEdge{rootScopeId: nil}
	path.scopeEdgesNullable = map[int]map[int]bool{rootScopeId: {}}
	path.scopeGraph = map[int][]*ScopeEdge{rootScopeId: nil}

	require.False(t, isPathForScc(path, g), "path with only a nullable self-loop must not need the SCC generator")

	g.generateAndSetQueryForTable(path)
	q := node.Query
	require.NotEmpty(t, q)
	require.NotContains(t, strings.ToUpper(q), "WITH RECURSIVE",
		"nullable self-loop table must not produce a recursive CTE; got: %s", q)
	require.Contains(t, q, `SELECT "public"."Node".*`)
	require.Contains(t, q, `"public"."Node"."deleted" = false`)
}

// TestNullableSelfLoopOnlyForcingEdge models the target shape: an Owner table
// (with a subset condition) and a Node table with a NOT NULL FK -> Owner and a
// NULLABLE self-FK (parent_id -> Node.id). Node's only cyclic edge is the
// nullable self-loop, so Node is non-cyclic and its query (derived from the
// incoming Owner FK) has no WITH RECURSIVE.
func TestNullableSelfLoopOnlyForcingEdge(t *testing.T) {
	const (
		ownerIdx = 0
		nodeIdx  = 1
	)
	owner := newTestTable(10, "public", "Owner", []string{"id"})
	owner.SubsetConds = []string{`"public"."Owner"."id" = 1`}
	node := newTestTable(20, "public", "Node", []string{"id"})

	ownerComp := NewComponent(
		0,
		map[int][]*Edge{ownerIdx: nil},
		map[int]*entries.Table{ownerIdx: owner},
	)
	require.False(t, ownerComp.hasCycle())

	// Node has a NOT NULL FK to Owner and a NULLABLE self FK.
	nodeToOwner := NewEdge(
		1, ownerIdx, false,
		NewTableLink(nodeIdx, node, NewKeysByColumn([]string{"owner_id"}), nil),
		NewTableLink(ownerIdx, owner, NewKeysByColumn([]string{"id"}), nil),
	)
	nodeSelf := NewEdge(
		2, nodeIdx, true,
		NewTableLink(nodeIdx, node, NewKeysByColumn([]string{"parent_id"}), nil),
		NewTableLink(nodeIdx, node, NewKeysByColumn([]string{"id"}), nil),
	)
	nodeComp := NewComponent(
		1,
		map[int][]*Edge{nodeIdx: {nodeToOwner, nodeSelf}},
		map[int]*entries.Table{nodeIdx: node},
	)
	require.False(t, nodeComp.hasCycle(), "Node's only cyclic edge is a nullable self-loop -> non-cyclic")
	require.Empty(t, nodeComp.cycles)

	// Build the path Owner(root) -> Node via the reversed FK so Node is subset by Owner.
	condEdge := NewCondensedEdge(
		0,
		NewComponentLink(0, ownerComp),
		NewComponentLink(1, nodeComp),
		NewEdge(
			3, nodeIdx, false,
			NewTableLink(ownerIdx, owner, NewKeysByColumn([]string{"id"}), nil),
			NewTableLink(nodeIdx, node, NewKeysByColumn([]string{"owner_id"}), nil),
		),
	)

	g := &Graph{scc: []*Component{ownerComp, nodeComp}}
	path := NewPath(0)
	path.scopeEdges = map[int][]*CondensedEdge{rootScopeId: {condEdge}}
	path.scopeEdgesNullable = map[int]map[int]bool{rootScopeId: {nodeIdx: false}}
	path.scopeGraph = map[int][]*ScopeEdge{rootScopeId: nil}

	require.False(t, isPathForScc(path, g), "no cyclic component in closure -> plain generator")

	g.generateAndSetQueryForTable(path)
	q := owner.Query
	require.NotEmpty(t, q)
	require.NotContains(t, strings.ToUpper(q), "WITH RECURSIVE",
		"path must not produce a recursive CTE; got: %s", q)
}

// TestNonNullableSelfLoopStaysCyclic is the contrasting case: a NON-nullable
// self-FK genuinely needs closure, so the component must remain cyclic and the
// generated query must still use the recursive-CTE machinery.
func TestNonNullableSelfLoopStaysCyclic(t *testing.T) {
	const nodeIdx = 0
	node := newTestTable(10, "public", "Node", []string{"id"})
	node.SubsetConds = []string{`"public"."Node"."deleted" = false`}

	// NON-nullable self FK.
	selfEdge := NewEdge(
		1, nodeIdx, false,
		NewTableLink(nodeIdx, node, NewKeysByColumn([]string{"parent_id"}), nil),
		NewTableLink(nodeIdx, node, NewKeysByColumn([]string{"id"}), nil),
	)
	comp := NewComponent(
		0,
		map[int][]*Edge{nodeIdx: {selfEdge}},
		map[int]*entries.Table{nodeIdx: node},
	)
	require.True(t, comp.hasCycle(), "non-nullable self-loop must remain cyclic")
	require.Len(t, comp.groupedCycles, 1)

	g := &Graph{scc: []*Component{comp}}
	path := NewPath(0)
	path.scopeEdges = map[int][]*CondensedEdge{rootScopeId: nil}
	path.scopeEdgesNullable = map[int]map[int]bool{rootScopeId: {}}
	path.scopeGraph = map[int][]*ScopeEdge{rootScopeId: nil}

	require.True(t, isPathForScc(path, g), "non-nullable self-loop must route to the SCC generator")

	g.generateAndSetQueryForScc(path)
	require.Contains(t, node.Query, "WITH RECURSIVE",
		"non-nullable self-loop must still produce a recursive CTE; got: %s", node.Query)
}
