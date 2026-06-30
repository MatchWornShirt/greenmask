package subset

import (
	"fmt"
	"strings"

	"github.com/greenmaskio/greenmask/internal/db/postgres/entries"
	"github.com/greenmaskio/greenmask/pkg/toolkit"
)

const (
	joinTypeInner = "INNER"
	joinTypeLeft  = "LEFT"
)

// overriddenJoinAlias derives a deterministic, per-edge alias for an overridden
// table's __ids CTE join. The same overridden table can be reached via multiple
// edges within one query (e.g. Bids -> ProductListings directly AND
// Bids -> MaxBids -> ProductListings); joining the same CTE name twice in one
// FROM is invalid SQL, so each edge gets a distinct alias. The scheme is stable
// and depends only on the edge + override name, so generateJoinClauseV2 and
// generateIntegrityChecksForNullableEdges derive an identical alias without
// sharing state.
// maxIdentifierLen is Postgres's NAMEDATALEN-1; longer identifiers are silently
// truncated (emitting a NOTICE that aborts greenmask's dump worker), and a
// truncated alias can also collide. Keep generated aliases within this bound.
const maxIdentifierLen = 63

func overriddenJoinAlias(edge *Edge, override string) string {
	// edge.id is unique per edge within the graph, so "<override>__e<id>" is a
	// deterministic, collision-free alias that both generateJoinClauseV2 and
	// generateIntegrityChecksForNullableEdges derive identically. The descriptive
	// from-table/key scheme produced identifiers over Postgres's 63-byte limit.
	suffix := fmt.Sprintf("__e%d", edge.id)
	base := override
	if len(base)+len(suffix) > maxIdentifierLen {
		base = base[:maxIdentifierLen-len(suffix)]
	}
	return sanitizeIdentifier(base + suffix)
}

// sanitizeIdentifier replaces characters that are not safe inside a double-quoted
// SQL identifier (notably embedded double quotes) so the alias is always a valid
// quoted identifier. It is deterministic for a given input.
func sanitizeIdentifier(s string) string {
	return strings.ReplaceAll(s, `"`, "_")
}

func generateJoinClauseForDroppedEdge(edge *Edge, initTableName string) string {
	var conds []string

	var leftTableKeys []string
	table := edge.from.table
	for _, key := range edge.from.keys {
		leftTableKeys = append(leftTableKeys, fmt.Sprintf(`%s__%s__%s`, table.Schema, table.Name, key.Name))
	}

	rightTable := edge.to
	for idx := 0; idx < len(edge.to.keys); idx++ {

		leftPart := fmt.Sprintf(
			`"%s"."%s"`,
			initTableName,
			leftTableKeys[idx],
		)

		rightPart := edge.to.keys[idx].GetKeyReference(rightTable.table)
		conds = append(conds, fmt.Sprintf(`%s = %s`, leftPart, rightPart))
	}
	if len(edge.from.polymorphicExprs) > 0 {
		conds = append(conds, edge.from.polymorphicExprs...)
	}
	if len(edge.to.polymorphicExprs) > 0 {
		conds = append(conds, edge.to.polymorphicExprs...)
	}

	rightTableName := fmt.Sprintf(`"%s"."%s"`, edge.to.table.Schema, edge.to.table.Name)

	joinClause := fmt.Sprintf(
		`JOIN %s ON %s`,
		rightTableName,
		strings.Join(conds, " AND "),
	)
	return joinClause
}

func generateJoinClauseV2(edge *Edge, joinType string, overriddenTables map[toolkit.Oid]string) string {
	if joinType != joinTypeInner && joinType != joinTypeLeft {
		panic(fmt.Sprintf("invalid join type: %s", joinType))
	}

	var conds []string

	leftTable, rightTable := edge.from.table, edge.to.table
	override, isOverridden := overriddenTables[rightTable.Oid]
	var alias string
	if isOverridden {
		alias = overriddenJoinAlias(edge, override)
	}
	for idx := 0; idx < len(edge.from.keys); idx++ {

		leftPart := edge.from.keys[idx].GetKeyReference(leftTable)
		rightPart := edge.to.keys[idx].GetKeyReference(rightTable)

		if isOverridden {
			rightPart = fmt.Sprintf(
				`"%s"."%s"`,
				alias,
				edge.to.keys[idx].Name,
			)
		}

		conds = append(conds, fmt.Sprintf(`%s = %s`, leftPart, rightPart))
		// When the TO table is overridden it is no longer in FROM — it has been
		// replaced by its "schema__table__ids" CTE (joined by alias). Its own
		// SubsetConds are already enforced INSIDE that CTE, so re-emitting them
		// here would both reference a table not in FROM (-> "missing FROM-clause
		// entry") and be redundant. Skip them for overridden tables.
		if !isOverridden && len(edge.to.table.SubsetConds) > 0 {
			conds = append(conds, edge.to.table.SubsetConds...)
		}
	}

	if len(edge.from.polymorphicExprs) > 0 {
		conds = append(conds, edge.from.polymorphicExprs...)
	}
	if len(edge.to.polymorphicExprs) > 0 {
		conds = append(conds, edge.to.polymorphicExprs...)
	}

	rightTableName := fmt.Sprintf(`"%s"."%s"`, rightTable.Schema, rightTable.Name)
	if isOverridden {
		rightTableName = fmt.Sprintf(`"%s" AS "%s"`, override, alias)
	}

	joinClause := fmt.Sprintf(
		`%s JOIN %s ON %s`,
		joinType,
		rightTableName,
		strings.Join(conds, " AND "),
	)
	return joinClause
}

func generateWhereClause(subsetConds []string) string {
	if len(subsetConds) == 0 {
		return "WHERE TRUE"
	}
	escapedConds := make([]string, 0, len(subsetConds))
	for _, cond := range subsetConds {
		escapedConds = append(escapedConds, fmt.Sprintf(`( %s )`, cond))
	}
	return "WHERE " + strings.Join(escapedConds, " AND ")
}

func generateSelectByPrimaryKey(table *entries.Table, pk []string) string {
	var keys []string
	for _, key := range pk {
		keys = append(keys, fmt.Sprintf(`"%s"."%s"."%s"`, table.Schema, table.Name, key))
	}
	return fmt.Sprintf(
		`SELECT %s`,
		strings.Join(keys, ", "),
	)
}
