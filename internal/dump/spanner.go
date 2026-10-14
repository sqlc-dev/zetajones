package dump

import "github.com/sqlc-dev/zetajones/ast"

// spannerNodeString returns the debug string for the zetajones-only Spanner
// DDL nodes in ast/spanner.go, which have no SingleNodeDebugString in the
// reference. Like the reference nodes, they print the node kind name followed
// by any modifiers in parentheses.
func spannerNodeString(n ast.Node) (string, bool) {
	switch t := n.(type) {
	case *ast.PlacementKeyColumnAttribute:
		return "PlacementKeyColumnAttribute", true
	case *ast.AutoIncrementColumnAttribute:
		return "AutoIncrementColumnAttribute", true
	case *ast.ColumnOnUpdateClause:
		return "ColumnOnUpdateClause", true
	case *ast.IdentityColumnSkipRange:
		return "IdentityColumnSkipRange", true
	case *ast.IdentityColumnRestartCounterWith:
		return "IdentityColumnRestartCounterWith", true
	case *ast.AlterColumnIdentityAction:
		if t.NoSkipRange {
			return "AlterColumnIdentityAction(no_skip_range)", true
		}
		return "AlterColumnIdentityAction", true
	case *ast.TableSynonym:
		return "TableSynonym", true
	case *ast.AddStoredColumnAction:
		return "AddStoredColumnAction", true
	case *ast.DropStoredColumnAction:
		return "DropStoredColumnAction", true
	case *ast.CreateQueueStatement:
		if t.IsIfNotExists {
			return "CreateQueueStatement(is_if_not_exists)", true
		}
		return "CreateQueueStatement", true
	case *ast.CreateChangeStreamStatement:
		if t.IsIfNotExists {
			return "CreateChangeStreamStatement(is_if_not_exists)", true
		}
		return "CreateChangeStreamStatement", true
	case *ast.ChangeStreamForClause:
		if t.All {
			return "ChangeStreamForClause(all)", true
		}
		return "ChangeStreamForClause", true
	case *ast.ChangeStreamTrackedTable:
		return "ChangeStreamTrackedTable", true
	case *ast.AlterChangeStreamStatement:
		if t.IsIfExists {
			return "AlterChangeStreamStatement(is_if_exists)", true
		}
		return "AlterChangeStreamStatement", true
	case *ast.SetChangeStreamForAction:
		return "SetChangeStreamForAction", true
	case *ast.DropChangeStreamForAllAction:
		return "DropChangeStreamForAllAction", true
	case *ast.AlterLocalityGroupStatement:
		if t.IsIfExists {
			return "AlterLocalityGroupStatement(is_if_exists)", true
		}
		return "AlterLocalityGroupStatement", true
	case *ast.CreateProtoBundleStatement:
		return "CreateProtoBundleStatement", true
	case *ast.AlterProtoBundleStatement:
		return "AlterProtoBundleStatement", true
	case *ast.ProtoBundleAction:
		return "ProtoBundleAction(" + t.Op + ")", true
	case *ast.DropProtoBundleStatement:
		return "DropProtoBundleStatement", true
	case *ast.GrantToRoleStatement:
		name := "GrantToRoleStatement"
		if t.IsRevoke {
			name = "RevokeFromRoleStatement"
		}
		if t.AllInSchema {
			name += "(all_in_schema)"
		}
		return name, true
	case *ast.GrantRoleStatement:
		if t.IsRevoke {
			return "RevokeRoleStatement", true
		}
		return "GrantRoleStatement", true
	}
	return "", false
}
