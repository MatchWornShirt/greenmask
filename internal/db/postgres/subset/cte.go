package subset

import (
	"fmt"
	"slices"
	"strings"

	"github.com/greenmaskio/greenmask/internal/db/postgres/entries"
)

type cteQuery struct {
	items []*cteItem
	c     *Component
}

func newCteQuery(c *Component) *cteQuery {
	return &cteQuery{
		c: c,
	}
}

func (c *cteQuery) addItem(name, query string) {
	c.items = append(c.items, &cteItem{
		name:  name,
		query: query,
	})
}

func (c *cteQuery) generateQuery(targetTable *entries.Table) string {
	var queries []string
	var excludedCteQueries []string
	// A non-cyclic root component (path rooted at a non-cyclic table that merely
	// REFERENCES a cycle, e.g. OrderDetails -> Orders) has no cycles[0] to
	// inspect. The cycle-based exclusion below only applies to a cyclic root,
	// where sibling cycle members share one set of __ids CTEs and the non-target
	// members must be excluded. For a non-cyclic root the target table's own
	// __ids CTE is emitted by generateQueryForNonCyclicScope and there is nothing
	// to exclude, so emit every collected CTE item.
	if c.c.hasCycle() {
		if len(c.c.groupedCycles) > 1 {
			panic("FIXME: found more than one grouped cycle")
		}
		for _, edge := range c.c.cycles[0] {
			if edge.from.table.Oid == targetTable.Oid {
				continue
			}
			excludedCteQuery := fmt.Sprintf("%s__%s__ids", edge.from.table.Schema, edge.from.table.Name)
			excludedCteQueries = append(excludedCteQueries, excludedCteQuery)
		}
	}

	var emitted []*cteItem
	for _, item := range c.items {
		if slices.Contains(excludedCteQueries, item.name) {
			continue
		}
		emitted = append(emitted, item)
	}
	// In a WITH [RECURSIVE] list a CTE may only reference CTEs defined earlier
	// (plus itself, if it is recursive). The items are appended in generation
	// order (parent cyclic CTEs first, child "__ids" CTEs later), which can place
	// a referenced CTE after the item that references it -> forward reference.
	// Topologically sort so every item appears AFTER the items it depends on.
	emitted = topoSortCteItems(emitted)
	for _, item := range emitted {
		queries = append(queries, fmt.Sprintf(` "%s" AS (%s)`, item.name, item.query))
	}
	var leftTableKeys, rightTableKeys []string
	rightTableName := fmt.Sprintf("%s__%s__ids", targetTable.Schema, targetTable.Name)
	for _, key := range targetTable.PrimaryKey {
		leftTableKeys = append(leftTableKeys, fmt.Sprintf(`"%s"."%s"."%s"`, targetTable.Schema, targetTable.Name, key))
		rightTableKeys = append(rightTableKeys, fmt.Sprintf(`"%s"."%s"`, rightTableName, key))
	}

	resultingQuery := fmt.Sprintf(
		`SELECT * FROM "%s"."%s" WHERE %s IN (SELECT %s FROM "%s")`,
		targetTable.Schema,
		targetTable.Name,
		fmt.Sprintf("(%s)", strings.Join(leftTableKeys, ",")),
		strings.Join(rightTableKeys, ","),
		rightTableName,
	)
	res := fmt.Sprintf("WITH RECURSIVE %s %s", strings.Join(queries, ","), resultingQuery)
	return res
}

type cteItem struct {
	name  string
	query string
}

// topoSortCteItems orders the CTE items so that any item appears AFTER every
// other item it references in its query body. "A references B" iff B.name
// occurs as a substring in A.query and B != A. An item's reference to itself
// (e.g. the recursive self-reference inside a `WITH RECURSIVE` member) is
// ignored, so recursive CTEs stay valid.
//
// The sort is a stable DFS (post-order) over the dependency edges, visiting
// items in their original slice order; this preserves the existing relative
// ordering for items that have no dependency relationship between them.
func topoSortCteItems(items []*cteItem) []*cteItem {
	const (
		stateUnvisited = 0
		stateVisiting  = 1
		stateVisited   = 2
	)
	state := make([]int, len(items))
	result := make([]*cteItem, 0, len(items))

	// dependsOn reports whether item a references item b (b must be emitted first).
	dependsOn := func(a, b *cteItem) bool {
		if a == b {
			return false
		}
		return strings.Contains(a.query, b.name)
	}

	var visit func(i int)
	visit = func(i int) {
		if state[i] == stateVisited {
			return
		}
		if state[i] == stateVisiting {
			// Cycle between distinct CTE items. Postgres itself only allows
			// self-recursion, so a genuine inter-item cycle would be invalid
			// SQL regardless of ordering; bail out to preserve progress rather
			// than loop forever.
			return
		}
		state[i] = stateVisiting
		for j := range items {
			if dependsOn(items[i], items[j]) {
				visit(j)
			}
		}
		state[i] = stateVisited
		result = append(result, items[i])
	}

	for i := range items {
		visit(i)
	}
	return result
}
