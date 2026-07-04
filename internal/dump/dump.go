// Package dump renders an AST in the same format as ZetaSQL's
// ASTNode::DebugString (github.com/google/googlesql,
// googlesql/parser/parse_tree.cc), so output can be compared byte-for-byte
// against the upstream parser test suite.
package dump

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/sqlc-dev/zetajones/ast"
	"github.com/sqlc-dev/zetajones/token"
)

// Options controls the debug output format.
type Options struct {
	// SQL is the original query text, used to render the source snippet for
	// each node. If ShowLocationText is false it is unused.
	SQL string
	// ShowLocationText appends the summarized source text of each node, e.g.
	// "Identifier(col) [7-10] [col]". This matches ZetaSQL's default in the
	// parser test suite; tests with no_show_parse_location_text disable it.
	ShowLocationText bool
}

// Tree renders the debug string for the AST rooted at n. The output has no
// trailing newline.
func Tree(n ast.Node, opts Options) string {
	var b strings.Builder
	walk(&b, n, 0, opts)
	return strings.TrimSuffix(b.String(), "\n")
}

func walk(b *strings.Builder, n ast.Node, depth int, opts Options) {
	for i := 0; i < depth; i++ {
		b.WriteString("  ")
	}
	b.WriteString(nodeString(n))
	fmt.Fprintf(b, " [%d-%d]", n.Pos(), n.End())
	if opts.ShowLocationText && n.Pos() >= 0 && n.End() >= n.Pos() && n.End() <= len(opts.SQL) {
		summary, ok := summaryString(opts.SQL[n.Pos():n.End()], 30)
		if ok {
			fmt.Fprintf(b, " [%s]", summary)
		}
	}
	b.WriteString("\n")
	for _, child := range n.Children() {
		walk(b, child, depth+1, opts)
	}
}

// nodeString returns the single-node debug string: the node kind name plus
// node-specific details, matching each node's SingleNodeDebugString in
// ZetaSQL's parse_tree.cc.
func nodeString(n ast.Node) string {
	switch t := n.(type) {
	case *ast.QueryStatement:
		return "QueryStatement"
	case *ast.HintedStatement:
		return "HintedStatement"
	case *ast.DeleteStatement:
		return "DeleteStatement"
	case *ast.InsertStatement:
		if t.InsertMode != "" {
			return "InsertStatement(insert_mode=" + t.InsertMode + ")"
		}
		return "InsertStatement"
	case *ast.InsertValuesRowList:
		return "InsertValuesRowList"
	case *ast.InsertValuesRow:
		return "InsertValuesRow"
	case *ast.UpdateStatement:
		return "UpdateStatement"
	case *ast.UpdateItemList:
		return "UpdateItemList"
	case *ast.UpdateItem:
		return "UpdateItem"
	case *ast.UpdateSetValue:
		return "UpdateSetValue"
	case *ast.MergeStatement:
		return "MergeStatement"
	case *ast.MergeWhenClauseList:
		return "MergeWhenClauseList"
	case *ast.MergeWhenClause:
		return "MergeWhenClause(match_type=" + t.MatchType + ")"
	case *ast.MergeAction:
		return "MergeAction(" + t.ActionType + ")"
	case *ast.AssertRowsModified:
		return "AssertRowsModified"
	case *ast.ReturningClause:
		return "ReturningClause"
	case *ast.DotGeneralizedField:
		return "DotGeneralizedField"
	case *ast.Query:
		if t.IsPivotInput {
			return "Query (pivot input)"
		}
		return "Query"
	case *ast.WithClause:
		if t.Recursive {
			return "WithClause (recursive)"
		}
		return "WithClause"
	case *ast.WithClauseEntry:
		return "WithClauseEntry"
	case *ast.AliasedQuery:
		return "AliasedQuery"
	case *ast.AliasedGroupRows:
		return "AliasedGroupRows"
	case *ast.TVF:
		return "TVF"
	case *ast.TVFArgument:
		return "TVFArgument"
	case *ast.Select:
		if t.Distinct {
			return "Select(distinct=true)"
		}
		return "Select"
	case *ast.SelectList:
		return "SelectList"
	case *ast.SelectColumn:
		return "SelectColumn"
	case *ast.Alias:
		return "Alias"
	case *ast.IntoAlias:
		return "IntoAlias"
	case *ast.ImportStatement:
		return "ImportStatement"
	case *ast.ModuleStatement:
		return "ModuleStatement"
	case *ast.SingleAssignment:
		return "SingleAssignment"
	case *ast.ParameterAssignment:
		return "ParameterAssignment"
	case *ast.SystemVariableAssignment:
		return "SystemVariableAssignment"
	case *ast.AssignmentFromStruct:
		return "AssignmentFromStruct"
	case *ast.Star:
		return fmt.Sprintf("Star(%s)", t.Image)
	case *ast.StarWithModifiers:
		return "StarWithModifiers"
	case *ast.DotStar:
		return "DotStar"
	case *ast.DotStarWithModifiers:
		return "DotStarWithModifiers"
	case *ast.StarModifiers:
		return "StarModifiers"
	case *ast.StarExceptList:
		return "StarExceptList"
	case *ast.StarReplaceItem:
		return "StarReplaceItem"
	case *ast.FromClause:
		return "FromClause"
	case *ast.TablePathExpression:
		return "TablePathExpression"
	case *ast.TableSubquery:
		return "TableSubquery"
	case *ast.UnnestExpression:
		return "UnnestExpression"
	case *ast.ExpressionWithOptAlias:
		return "ExpressionWithOptAlias"
	case *ast.WithOffset:
		return "WithOffset"
	case *ast.ForSystemTime:
		return "ForSystemTime"
	case *ast.ArrayConstructor:
		return "ArrayConstructor"
	case *ast.Join:
		// See ASTJoin::SingleNodeDebugString in parse_tree.cc: the details
		// are NATURAL, the join type (comma joins show as COMMA), and the
		// join hint, in that order.
		var attrs []string
		if t.Natural {
			attrs = append(attrs, "NATURAL")
		}
		if t.JoinType != "" {
			attrs = append(attrs, t.JoinType)
		}
		if t.JoinHint != "" {
			attrs = append(attrs, t.JoinHint)
		}
		if len(attrs) == 0 {
			return "Join"
		}
		return fmt.Sprintf("Join(%s)", strings.Join(attrs, ", "))
	case *ast.OnClause:
		return "OnClause"
	case *ast.UsingClause:
		return "UsingClause"
	case *ast.OnOrUsingClauseList:
		return "OnOrUsingClauseList"
	case *ast.ParenthesizedJoin:
		return "ParenthesizedJoin"
	case *ast.PipeJoin:
		return "PipeJoin"
	case *ast.PipeJoinLhsPlaceholder:
		return "PipeJoinLhsPlaceholder"
	case *ast.WhereClause:
		return "WhereClause"
	case *ast.GroupBy:
		return "GroupBy"
	case *ast.GroupByAll:
		return "GroupByAll"
	case *ast.GroupingItem:
		return "GroupingItem"
	case *ast.Having:
		return "Having"
	case *ast.Qualify:
		return "Qualify"
	case *ast.OrderBy:
		return "OrderBy"
	case *ast.OrderingExpression:
		if t.Descending {
			return "OrderingExpression(DESC)"
		}
		if t.HasAsc {
			return "OrderingExpression(ASC EXPLICITLY)"
		}
		return "OrderingExpression(ASC)"
	case *ast.Collate:
		return "Collate"
	case *ast.IndexItemList:
		return "IndexItemList"
	case *ast.IndexAllColumns:
		return "IndexAllColumns(" + t.Image + ")"
	case *ast.IndexUnnestExpressionList:
		return "IndexUnnestExpressionList"
	case *ast.UnnestExpressionWithOptAliasAndOffset:
		return "UnnestExpressionWithOptAliasAndOffset"
	case *ast.IndexStoringExpressionList:
		return "IndexStoringExpressionList"
	case *ast.NullOrder:
		if t.NullsFirst {
			return "NullOrder(NULLS FIRST)"
		}
		return "NullOrder(NULLS LAST)"
	case *ast.Hint:
		return "Hint"
	case *ast.HintEntry:
		return "HintEntry"
	case *ast.AnalyticFunctionCall:
		return "AnalyticFunctionCall"
	case *ast.WindowSpecification:
		return "WindowSpecification"
	case *ast.WindowClause:
		return "WindowClause"
	case *ast.WindowDefinition:
		return "WindowDefinition"
	case *ast.WindowFrame:
		return "WindowFrame(" + t.Unit + ")"
	case *ast.WindowFrameExpr:
		return "WindowFrameExpr(" + t.BoundaryType + ")"
	case *ast.PartitionBy:
		return "PartitionBy"
	case *ast.ClusterBy:
		return "ClusterBy"
	case *ast.LimitOffset:
		return "LimitOffset"
	case *ast.Limit:
		return "Limit"
	case *ast.LimitAll:
		return "LimitAll"
	case *ast.LockMode:
		return "LockMode"
	case *ast.AlterStatement:
		if t.IsIfExists {
			return t.NodeName + "(is_if_exists)"
		}
		return t.NodeName
	case *ast.DropStatement:
		// See ASTDropStatement / ASTDropFunctionStatement /
		// ASTDropSnapshotTableStatement etc. SingleNodeDebugString in
		// parse_tree.cc: the generic node prints the schema object kind name,
		// then any modifiers (is_if_exists, drop_mode) in parentheses.
		out := t.NodeName
		if t.ObjectKind != "" {
			out += " " + t.ObjectKind
		}
		var mods []string
		if t.IsIfExists {
			mods = append(mods, "is_if_exists")
		}
		if t.DropMode != "" {
			mods = append(mods, "drop_mode="+t.DropMode)
		}
		if len(mods) > 0 {
			out += "(" + strings.Join(mods, ", ") + ")"
		}
		return out
	case *ast.AlterRowAccessPolicyStatement:
		if t.IsIfExists {
			return "AlterRowAccessPolicyStatement(is_if_exists)"
		}
		return "AlterRowAccessPolicyStatement"
	case *ast.AlterAllRowAccessPoliciesStatement:
		return "AlterAllRowAccessPoliciesStatement"
	case *ast.AlterActionList:
		return "AlterActionList"
	case *ast.GrantToClause:
		return "GrantToClause"
	case *ast.RevokeFromClause:
		if t.IsRevokeFromAll {
			return "RevokeFromClause(is_revoke_from_all)"
		}
		return "RevokeFromClause"
	case *ast.FilterUsingClause:
		return "FilterUsingClause"
	case *ast.GranteeList:
		return "GranteeList"
	case *ast.RenameToClause:
		return "RenameToClause"
	case *ast.SetOptionsAction:
		return "SetOptionsAction"
	case *ast.AddColumnAction:
		if t.IsIfNotExists {
			return "AddColumnAction(is_if_not_exists)"
		}
		return "AddColumnAction"
	case *ast.ColumnPosition:
		return "ColumnPosition(" + t.Type + ")"
	case *ast.DropColumnAction:
		if t.IsIfExists {
			return "DropColumnAction(is_if_exists)"
		}
		return "DropColumnAction"
	case *ast.RenameColumnAction:
		if t.IsIfExists {
			return "RenameColumnAction(is_if_exists)"
		}
		return "RenameColumnAction"
	case *ast.AddConstraintAction:
		if t.IsIfNotExists {
			return "AddConstraintAction(is_if_not_exists)"
		}
		return "AddConstraintAction"
	case *ast.DropConstraintAction:
		if t.IsIfExists {
			return "DropConstraintAction(is_if_exists)"
		}
		return "DropConstraintAction"
	case *ast.DropPrimaryKeyAction:
		if t.IsIfExists {
			return "DropPrimaryKeyAction(is_if_exists)"
		}
		return "DropPrimaryKeyAction"
	case *ast.AlterConstraintEnforcementAction:
		if t.IsIfExists {
			return "AlterConstraintEnforcementAction(is_if_exists)"
		}
		return "AlterConstraintEnforcementAction"
	case *ast.AlterConstraintSetOptionsAction:
		if t.IsIfExists {
			return "AlterConstraintSetOptionsAction(is_if_exists)"
		}
		return "AlterConstraintSetOptionsAction"
	case *ast.AddTtlAction:
		return "AddTtlAction"
	case *ast.ReplaceTtlAction:
		return "ReplaceTtlAction"
	case *ast.DropTtlAction:
		return "DropTtlAction"
	case *ast.OptionsList:
		return "OptionsList"
	case *ast.OptionsEntry:
		return "OptionsEntry"
	case *ast.Identifier:
		return fmt.Sprintf("Identifier(%s)", identifierLiteral(t.Name))
	case *ast.PathExpression:
		return "PathExpression"
	case *ast.NullLiteral:
		return fmt.Sprintf("NullLiteral(%s)", t.Image)
	case *ast.BooleanLiteral:
		return fmt.Sprintf("BooleanLiteral(%s)", t.Image)
	case *ast.IntLiteral:
		return fmt.Sprintf("IntLiteral(%s)", t.Image)
	case *ast.FloatLiteral:
		return fmt.Sprintf("FloatLiteral(%s)", t.Image)
	case *ast.StringLiteral:
		return "StringLiteral"
	case *ast.StringLiteralComponent:
		return fmt.Sprintf("StringLiteralComponent(%s)", t.Image)
	case *ast.BytesLiteral:
		return "BytesLiteral"
	case *ast.BytesLiteralComponent:
		return fmt.Sprintf("BytesLiteralComponent(%s)", t.Image)
	case *ast.NumericLiteral:
		return "NumericLiteral"
	case *ast.BigNumericLiteral:
		return "BigNumericLiteral"
	case *ast.JSONLiteral:
		return "JSONLiteral"
	case *ast.DateOrTimeLiteral:
		return fmt.Sprintf("DateOrTimeLiteral(%s)", t.TypeKind)
	case *ast.RangeLiteral:
		return "RangeLiteral"
	case *ast.IntervalExpr:
		return "IntervalExpr"
	case *ast.UnaryExpression:
		return fmt.Sprintf("UnaryExpression(%s)", t.Op)
	case *ast.BinaryExpression:
		// See ASTBinaryExpression::GetSQLForOperator in parse_tree.cc for
		// how is_not combines with the operator name.
		op := t.Op
		if t.IsNot {
			switch t.Op {
			case "IS":
				op = "IS NOT"
			case "LIKE":
				op = "NOT LIKE"
			case "IS DISTINCT FROM":
				op = "IS NOT DISTINCT FROM"
			}
		}
		return fmt.Sprintf("BinaryExpression(%s)", op)
	case *ast.BitwiseShiftExpression:
		if t.IsLeftShift {
			return "BitwiseShiftExpression(<<)"
		}
		return "BitwiseShiftExpression(>>)"
	case *ast.InExpression:
		if t.IsNot {
			return "InExpression(NOT IN)"
		}
		return "InExpression(IN)"
	case *ast.InList:
		return "InList"
	case *ast.LikeExpression:
		if t.IsNot {
			return "LikeExpression(NOT LIKE)"
		}
		return "LikeExpression(LIKE)"
	case *ast.QuantifiedComparisonExpression:
		return fmt.Sprintf("QuantifiedComparisonExpression(%s)", t.Op)
	case *ast.AnySomeAllOp:
		return fmt.Sprintf("AnySomeAllOp(%s)", t.Op)
	case *ast.AndExpr:
		return "AndExpr"
	case *ast.OrExpr:
		return "OrExpr"
	case *ast.Location:
		return "Location"
	case *ast.BetweenExpression:
		if t.IsNot {
			return "BetweenExpression(NOT BETWEEN)"
		}
		return "BetweenExpression(BETWEEN)"
	case *ast.ClampedBetweenModifier:
		return "ClampedBetweenModifier"
	case *ast.HavingModifier:
		return "HavingModifier"
	case *ast.NamedArgument:
		return "NamedArgument"
	case *ast.Lambda:
		return "Lambda"
	case *ast.StructConstructorWithParens:
		return "StructConstructorWithParens"
	case *ast.StructConstructorWithKeyword:
		return "StructConstructorWithKeyword"
	case *ast.StructConstructorArg:
		return "StructConstructorArg"
	case *ast.NewConstructor:
		return "NewConstructor"
	case *ast.NewConstructorArg:
		return "NewConstructorArg"
	case *ast.BracedNewConstructor:
		return "BracedNewConstructor"
	case *ast.StructBracedConstructor:
		return "StructBracedConstructor"
	case *ast.BracedConstructor:
		return "BracedConstructor"
	case *ast.BracedConstructorField:
		return "BracedConstructorField"
	case *ast.BracedConstructorLhs:
		return "BracedConstructorLhs"
	case *ast.BracedConstructorFieldValue:
		return "BracedConstructorFieldValue"
	case *ast.FromQuery:
		return "FromQuery"
	case *ast.SetOperation:
		// The parenthesized detail is the first operator's SQL; see
		// ASTSetOperationMetadata::GetSQLForOperation in parse_tree.cc.
		md := t.Metadata.Entries[0]
		return fmt.Sprintf("SetOperation(%s %s)", md.OpType.Op, md.AllOrDistinct.Value)
	case *ast.SetOperationMetadataList:
		return "SetOperationMetadataList"
	case *ast.SetOperationMetadata:
		return "SetOperationMetadata"
	case *ast.SetOperationType:
		return "SetOperationType"
	case *ast.SetOperationAllOrDistinct:
		return "SetOperationAllOrDistinct"
	case *ast.SetOperationColumnMatchMode:
		return "SetOperationColumnMatchMode"
	case *ast.SetOperationColumnPropagationMode:
		return "SetOperationColumnPropagationMode"
	case *ast.ColumnList:
		return "ColumnList"
	case *ast.TableClause:
		return "TableClause"
	case *ast.ModelClause:
		return "ModelClause"
	case *ast.ConnectionClause:
		return "ConnectionClause"
	case *ast.DefaultLiteral:
		return "DefaultLiteral"
	case *ast.TableElementList:
		return "TableElementList"
	case *ast.ColumnDefinition:
		return "ColumnDefinition"
	case *ast.SimpleColumnSchema:
		return "SimpleColumnSchema"
	case *ast.ColumnAttributeList:
		return "ColumnAttributeList"
	case *ast.NotNullColumnAttribute:
		return "NotNullColumnAttribute"
	case *ast.PrimaryKey:
		if t.Enforced {
			return "PrimaryKey(ENFORCED)"
		}
		return "PrimaryKey(NOT ENFORCED)"
	case *ast.PrimaryKeyElementList:
		return "PrimaryKeyElementList"
	case *ast.PrimaryKeyElement:
		switch t.Ordering {
		case "DESC":
			return "PrimaryKeyElement(DESC)"
		case "ASC":
			return "PrimaryKeyElement(ASC EXPLICITLY)"
		default:
			return "PrimaryKeyElement(ASC)"
		}
	case *ast.CheckConstraint:
		if t.Enforced {
			return "CheckConstraint(ENFORCED)"
		}
		return "CheckConstraint(NOT ENFORCED)"
	case *ast.WithPartitionColumnsClause:
		return "WithPartitionColumnsClause"
	case *ast.CallStatement:
		return "CallStatement"
	case *ast.ExecuteImmediateStatement:
		return "ExecuteImmediateStatement"
	case *ast.ExecuteIntoClause:
		return "ExecuteIntoClause"
	case *ast.IdentifierList:
		return "IdentifierList"
	case *ast.ExecuteUsingClause:
		return "ExecuteUsingClause"
	case *ast.ExecuteUsingArgument:
		return "ExecuteUsingArgument"
	case *ast.CaseValueExpression:
		return "CaseValueExpression"
	case *ast.CaseNoValueExpression:
		return "CaseNoValueExpression"
	case *ast.DotIdentifier:
		return "DotIdentifier"
	case *ast.ArrayElement:
		return "ArrayElement"
	case *ast.ParameterExpr:
		// Positional parameters show their 1-based position; see
		// ASTParameterExpr::SingleNodeDebugString.
		if t.Name == nil {
			return fmt.Sprintf("ParameterExpr(%d)", t.Position)
		}
		return "ParameterExpr"
	case *ast.SystemVariableExpr:
		return "SystemVariableExpr"
	case *ast.PipeSetOperation:
		return "PipeSetOperation"
	case *ast.Subpipeline:
		return "Subpipeline"
	case *ast.PipeLog:
		return "PipeLog"
	case *ast.PipeSelect:
		return "PipeSelect"
	case *ast.PipeExtend:
		return "PipeExtend"
	case *ast.PipeWindow:
		return "PipeWindow"
	case *ast.PipeLimitOffset:
		return "PipeLimitOffset"
	case *ast.PipeDistinct:
		return "PipeDistinct"
	case *ast.PipeMatchRecognize:
		return "PipeMatchRecognize"
	case *ast.MatchRecognizeClause:
		return "MatchRecognizeClause"
	case *ast.SampleClause:
		return "SampleClause"
	case *ast.SampleSize:
		return "SampleSize"
	case *ast.SampleSuffix:
		return "SampleSuffix"
	case *ast.WithWeight:
		return "WithWeight"
	case *ast.RepeatableClause:
		return "RepeatableClause"
	case *ast.AfterMatchSkipClause:
		return "AfterMatchSkipClause"
	case *ast.PathExpressionList:
		return "PathExpressionList"
	case *ast.PivotClause:
		return "PivotClause"
	case *ast.PivotExpressionList:
		return "PivotExpressionList"
	case *ast.PivotExpression:
		return "PivotExpression"
	case *ast.PivotValueList:
		return "PivotValueList"
	case *ast.PivotValue:
		return "PivotValue"
	case *ast.UnpivotClause:
		// See ASTUnpivotClause::SingleNodeDebugString in parse_tree.cc.
		if t.NullFilter != "" {
			return fmt.Sprintf("UnpivotClause(%s)", t.NullFilter)
		}
		return "UnpivotClause"
	case *ast.UnpivotInItemList:
		return "UnpivotInItemList"
	case *ast.UnpivotInItem:
		return "UnpivotInItem"
	case *ast.UnpivotInItemLabel:
		return "UnpivotInItemLabel"
	case *ast.RowPatternOperation:
		return "RowPatternOperation"
	case *ast.EmptyRowPattern:
		// See ASTEmptyRowPattern::SingleNodeDebugString.
		if t.Parenthesized {
			return "EmptyRowPattern(parenthesized=true)"
		}
		return "EmptyRowPattern"
	case *ast.RowPatternVariable:
		return "RowPatternVariable"
	case *ast.RowPatternAnchor:
		return "RowPatternAnchor"
	case *ast.RowPatternQuantification:
		return "RowPatternQuantification"
	case *ast.SymbolQuantifier:
		// See ASTQuantifier::SingleNodeDebugString.
		if t.IsReluctant {
			return "SymbolQuantifier(is_reluctant=true)"
		}
		return "SymbolQuantifier"
	case *ast.FixedQuantifier:
		return "FixedQuantifier"
	case *ast.BoundedQuantifier:
		if t.IsReluctant {
			return "BoundedQuantifier(is_reluctant=true)"
		}
		return "BoundedQuantifier"
	case *ast.QuantifierBound:
		return "QuantifierBound"
	case *ast.PipeAggregate:
		return "PipeAggregate"
	case *ast.PipeWhere:
		return "PipeWhere"
	case *ast.PipeTablesample:
		return "PipeTablesample"
	case *ast.PipeOrderBy:
		return "PipeOrderBy"
	case *ast.PipeSet:
		return "PipeSet"
	case *ast.PipeSetItem:
		return "PipeSetItem"
	case *ast.FunctionCall:
		if t.Distinct {
			return "FunctionCall(distinct=true)"
		}
		return "FunctionCall"
	case *ast.CastExpression:
		// See ASTCastExpression::SingleNodeDebugString in parse_tree.cc.
		if t.IsSafeCast {
			return "CastExpression(return_null_on_error=true)"
		}
		return "CastExpression"
	case *ast.ExtractExpression:
		return "ExtractExpression"
	case *ast.FormatClause:
		return "FormatClause"
	case *ast.SimpleType:
		return "SimpleType"
	case *ast.ArrayType:
		return "ArrayType"
	case *ast.StructType:
		return "StructType"
	case *ast.StructField:
		return "StructField"
	case *ast.RangeType:
		return "RangeType"
	case *ast.MapType:
		return "MapType"
	case *ast.FunctionType:
		return "FunctionType"
	case *ast.FunctionTypeArgList:
		return "FunctionTypeArgList"
	case *ast.TypeParameterList:
		return "TypeParameterList"
	case *ast.MaxLiteral:
		// ASTMaxLiteral is a printable leaf with an empty image, so the
		// reference prints an empty parenthesized detail.
		return "MaxLiteral()"
	case *ast.ExpressionSubquery:
		if t.Modifier != "" {
			return fmt.Sprintf("ExpressionSubquery(modifier=%s)", t.Modifier)
		}
		return "ExpressionSubquery"
	case *ast.CreateTableStatement:
		var mods []string
		switch t.Scope {
		case "PRIVATE":
			mods = append(mods, "is_private")
		case "PUBLIC":
			mods = append(mods, "is_public")
		case "TEMP":
			mods = append(mods, "is_temp")
		}
		if t.IsOrReplace {
			mods = append(mods, "is_or_replace")
		}
		if t.IsIfNotExists {
			mods = append(mods, "is_if_not_exists")
		}
		if len(mods) == 0 {
			return "CreateTableStatement"
		}
		return fmt.Sprintf("CreateTableStatement(%s)", strings.Join(mods, ", "))
	case *ast.CreateExternalTableStatement:
		var mods []string
		switch t.Scope {
		case "PRIVATE":
			mods = append(mods, "is_private")
		case "PUBLIC":
			mods = append(mods, "is_public")
		case "TEMP":
			mods = append(mods, "is_temp")
		}
		if t.IsOrReplace {
			mods = append(mods, "is_or_replace")
		}
		if t.IsIfNotExists {
			mods = append(mods, "is_if_not_exists")
		}
		if len(mods) == 0 {
			return "CreateExternalTableStatement"
		}
		return fmt.Sprintf("CreateExternalTableStatement(%s)", strings.Join(mods, ", "))
	case *ast.CreateViewStatement:
		// The modifier order matches ASTCreateViewStatementBase::CollectModifiers
		// in parse_tree.cc: scope, is_or_replace, is_if_not_exists (from the
		// base ASTCreateStatement), then SQL SECURITY, then recursive.
		var name string
		switch t.ViewKind {
		case "MATERIALIZED":
			name = "CreateMaterializedViewStatement"
		case "APPROX":
			name = "CreateApproxViewStatement"
		default:
			name = "CreateViewStatement"
		}
		var mods []string
		switch t.Scope {
		case "PRIVATE":
			mods = append(mods, "is_private")
		case "PUBLIC":
			mods = append(mods, "is_public")
		case "TEMP":
			mods = append(mods, "is_temp")
		}
		if t.IsOrReplace {
			mods = append(mods, "is_or_replace")
		}
		if t.IsIfNotExists {
			mods = append(mods, "is_if_not_exists")
		}
		if t.SqlSecurity != "" {
			mods = append(mods, "SQL SECURITY "+t.SqlSecurity)
		}
		if t.Recursive {
			mods = append(mods, "recursive")
		}
		if len(mods) == 0 {
			return name
		}
		return fmt.Sprintf("%s(%s)", name, strings.Join(mods, ", "))
	case *ast.CreateConstantStatement:
		// Modifier order matches the base ASTCreateStatement modifiers:
		// scope, is_or_replace, is_if_not_exists.
		var mods []string
		switch t.Scope {
		case "PRIVATE":
			mods = append(mods, "is_private")
		case "PUBLIC":
			mods = append(mods, "is_public")
		case "TEMP":
			mods = append(mods, "is_temp")
		}
		if t.IsOrReplace {
			mods = append(mods, "is_or_replace")
		}
		if t.IsIfNotExists {
			mods = append(mods, "is_if_not_exists")
		}
		if len(mods) == 0 {
			return "CreateConstantStatement"
		}
		return fmt.Sprintf("CreateConstantStatement(%s)", strings.Join(mods, ", "))
	case *ast.CreateModelStatement:
		// The modifier order matches the base ASTCreateStatement modifiers:
		// scope, is_or_replace, is_if_not_exists. is_remote is not shown.
		var mods []string
		switch t.Scope {
		case "PRIVATE":
			mods = append(mods, "is_private")
		case "PUBLIC":
			mods = append(mods, "is_public")
		case "TEMP":
			mods = append(mods, "is_temp")
		}
		if t.IsOrReplace {
			mods = append(mods, "is_or_replace")
		}
		if t.IsIfNotExists {
			mods = append(mods, "is_if_not_exists")
		}
		if len(mods) == 0 {
			return "CreateModelStatement"
		}
		return fmt.Sprintf("CreateModelStatement(%s)", strings.Join(mods, ", "))
	case *ast.InputOutputClause:
		return "InputOutputClause"
	case *ast.TransformClause:
		return "TransformClause"
	case *ast.AliasedQueryList:
		return "AliasedQueryList"
	case *ast.ColumnWithOptionsList:
		return "ColumnWithOptionsList"
	case *ast.ColumnWithOptions:
		return "ColumnWithOptions"
	case *ast.CreateIndexStatement:
		// The debug detail lists UNIQUE, then SEARCH or VECTOR, comma-separated;
		// see ASTCreateIndexStatement::SingleNodeDebugString in parse_tree.cc.
		if t.IsUnique || t.IsSearch || t.IsVector {
			var parts []string
			if t.IsUnique {
				parts = append(parts, "UNIQUE")
			}
			if t.IsSearch {
				parts = append(parts, "SEARCH")
			}
			if t.IsVector {
				parts = append(parts, "VECTOR")
			}
			return "CreateIndexStatement(" + strings.Join(parts, ",") + ")"
		}
		return "CreateIndexStatement"
	case *ast.CreateRowAccessPolicyStatement:
		var mods []string
		if t.IsOrReplace {
			mods = append(mods, "is_or_replace")
		}
		if t.IsIfNotExists {
			mods = append(mods, "is_if_not_exists")
		}
		if len(mods) == 0 {
			return "CreateRowAccessPolicyStatement"
		}
		return fmt.Sprintf("CreateRowAccessPolicyStatement(%s)", strings.Join(mods, ", "))
	case *ast.CreateTableFunctionStatement:
		// The SQL SECURITY clause is parsed but, matching the reference's
		// ASTCreateTableFunctionStatement::SingleNodeDebugString (which defers
		// to the plain create-statement modifier list), is not shown.
		var mods []string
		switch t.Scope {
		case "PRIVATE":
			mods = append(mods, "is_private")
		case "PUBLIC":
			mods = append(mods, "is_public")
		case "TEMP":
			mods = append(mods, "is_temp")
		}
		if t.IsOrReplace {
			mods = append(mods, "is_or_replace")
		}
		if t.IsIfNotExists {
			mods = append(mods, "is_if_not_exists")
		}
		if len(mods) == 0 {
			return "CreateTableFunctionStatement"
		}
		return fmt.Sprintf("CreateTableFunctionStatement(%s)", strings.Join(mods, ", "))
	case *ast.CreateFunctionStatement:
		// The base create-statement modifiers come first as one group; the
		// function-specific aggregate, determinism, and SQL SECURITY modifiers
		// follow as their own parenthesized groups (matching
		// ASTCreateFunctionStmtBase::SingleNodeDebugString in
		// googlesql/parser/parse_tree.cc).
		var mods []string
		switch t.Scope {
		case "PRIVATE":
			mods = append(mods, "is_private")
		case "PUBLIC":
			mods = append(mods, "is_public")
		case "TEMP":
			mods = append(mods, "is_temp")
		}
		if t.IsOrReplace {
			mods = append(mods, "is_or_replace")
		}
		if t.IsIfNotExists {
			mods = append(mods, "is_if_not_exists")
		}
		out := "CreateFunctionStatement"
		if len(mods) > 0 {
			out += "(" + strings.Join(mods, ", ") + ")"
		}
		if t.IsAggregate {
			out += "(is_aggregate=true)"
		}
		if t.Determinism != "" {
			out += "(" + t.Determinism + ")"
		}
		if t.SqlSecurity != "" {
			out += "(SQL SECURITY " + t.SqlSecurity + ")"
		}
		return out
	case *ast.CreateProcedureStatement:
		// Base ASTCreateStatement modifiers come first as one group, then the
		// external-security modifier as its own parenthesized group; see
		// ASTCreateProcedureStatement::SingleNodeDebugString in
		// googlesql/parser/parse_tree.cc.
		var mods []string
		switch t.Scope {
		case "PRIVATE":
			mods = append(mods, "is_private")
		case "PUBLIC":
			mods = append(mods, "is_public")
		case "TEMP":
			mods = append(mods, "is_temp")
		}
		if t.IsOrReplace {
			mods = append(mods, "is_or_replace")
		}
		if t.IsIfNotExists {
			mods = append(mods, "is_if_not_exists")
		}
		out := "CreateProcedureStatement"
		if len(mods) > 0 {
			out += "(" + strings.Join(mods, ", ") + ")"
		}
		if t.ExternalSecurity != "" {
			out += "(EXTERNAL SECURITY " + t.ExternalSecurity + ")"
		}
		return out
	case *ast.Script:
		return "Script"
	case *ast.StatementList:
		return "StatementList"
	case *ast.BeginEndBlock:
		return "BeginEndBlock"
	case *ast.VariableDeclaration:
		return "VariableDeclaration"
	case *ast.IfStatement:
		return "IfStatement"
	case *ast.ElseifClause:
		return "ElseifClause"
	case *ast.ElseifClauseList:
		return "ElseifClauseList"
	case *ast.ReturnStatement:
		return "ReturnStatement"
	case *ast.SqlFunctionBody:
		return "SqlFunctionBody"
	case *ast.TemplatedParameterType:
		return "TemplatedParameterType"
	case *ast.WithConnectionClause:
		return "WithConnectionClause"
	case *ast.FunctionDeclaration:
		return "FunctionDeclaration"
	case *ast.FunctionParameters:
		return "FunctionParameters"
	case *ast.FunctionParameter:
		// Modifier order matches ASTFunctionParameter::SingleNodeDebugString in
		// googlesql/parser/parse_tree.cc: is_not_aggregate, mode, default_value.
		var mods []string
		if t.IsNotAggregate {
			mods = append(mods, "is_not_aggregate=true")
		}
		if t.Mode != "" {
			mods = append(mods, "mode="+t.Mode)
		}
		if t.DefaultValue != nil {
			mods = append(mods, "default_value=("+nodeString(t.DefaultValue)+")")
		}
		if len(mods) == 0 {
			return "FunctionParameter"
		}
		return "FunctionParameter(" + strings.Join(mods, ", ") + ")"
	case *ast.TVFSchema:
		return "TVFSchema"
	case *ast.TVFSchemaColumn:
		return "TVFSchemaColumn"
	default:
		return fmt.Sprintf("UNKNOWN_NODE(%T)", n)
	}
}

// summaryString is a port of GetSummaryString from ZetaSQL
// (googlesql/common/utf_util.cc). It normalizes whitespace and, when the text
// exceeds maxCodePoints, elides the middle with "...", preferring to break at
// word boundaries. Returns ok=false for invalid UTF-8 (ZetaSQL omits the
// snippet in that case).
func summaryString(s string, maxCodePoints int) (string, bool) {
	// Normalize: strip leading/trailing whitespace, replace \r\n with a
	// single space, then replace every whitespace character with ' '.
	s = strings.TrimFunc(s, isASCIISpace)
	s = strings.ReplaceAll(s, "\r\n", " ")
	norm := []byte(s)
	for i := range norm {
		if isASCIISpaceByte(norm[i]) {
			norm[i] = ' '
		}
	}
	if !utf8.Valid(norm) {
		return "", false
	}
	runes := []rune(string(norm))
	if len(runes) <= maxCodePoints {
		return string(runes), true
	}

	minPrefixSuffix := (maxCodePoints - 3) / 2
	if maxCodePoints/3 < minPrefixSuffix {
		minPrefixSuffix = maxCodePoints / 3
	}

	prefix, prefixLen := summaryPrefix(runes, minPrefixSuffix, maxCodePoints)
	suffix := summarySuffix(runes, minPrefixSuffix, maxCodePoints-prefixLen)
	return prefix + "..." + suffix, true
}

// summaryPrefix computes the text before the "...", extending past
// minChars to avoid breaking in the middle of a word, up to
// maxTotal-3-minChars characters.
func summaryPrefix(runes []rune, minChars, maxTotal int) (string, int) {
	maxPrefixChars := maxTotal - 3 - minChars
	var prefix []rune
	inWord := false
	trailingSpaces := 0
	for _, r := range runes {
		prevInWord := inWord
		inWord = isWordChar(r)
		if len(prefix) >= minChars && (!prevInWord || !inWord) {
			break
		}
		prefix = append(prefix, r)
		if r == ' ' {
			trailingSpaces++
		} else {
			trailingSpaces = 0
		}
		if len(prefix) >= maxPrefixChars {
			break
		}
	}
	charLen := len(prefix) - trailingSpaces
	out := strings.TrimRight(string(prefix), " ")
	return out, charLen
}

// summarySuffix computes the text after the "...", walking backwards.
func summarySuffix(runes []rune, minChars, maxTotal int) string {
	maxSuffixChars := maxTotal - 3
	var suffix []rune
	inWord := false
	for i := len(runes) - 1; i >= 0; i-- {
		r := runes[i]
		prevInWord := inWord
		inWord = isWordChar(r)
		if len(suffix) >= minChars && (!prevInWord || !inWord) {
			break
		}
		suffix = append([]rune{r}, suffix...)
		if len(suffix) >= maxSuffixChars {
			break
		}
	}
	return strings.TrimLeft(string(suffix), " ")
}

func isWordChar(r rune) bool {
	return r == '_' ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9')
}

func isASCIISpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f'
}

func isASCIISpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

// identifierLiteral renders an identifier name the way ZetaSQL's
// ToIdentifierLiteral (googlesql/public/strings.cc, Apache 2.0) does: names
// that are valid unquoted identifiers — and not keywords whose meaning would
// change without quoting — print as-is; everything else is backquoted with
// C-style escaping.
func identifierLiteral(name string) string {
	if isValidUnquotedIdentifier(name) && !token.NonReservedIdentifierMustBeBackquoted(name) {
		return name
	}
	return "`" + escapeIdentifier(name) + "`"
}

// isValidUnquotedIdentifier ports IsValidUnquotedIdentifier from
// googlesql/public/strings.cc with all reservable keywords enabled and
// allow_reserved_keywords=false, which is how ToIdentifierLiteral calls it.
func isValidUnquotedIdentifier(s string) bool {
	if s == "" {
		return false
	}
	if !isIdentStartByte(s[0]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isIdentStartByte(c) && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return !token.IsReservableKeyword(s)
}

func isIdentStartByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// escapeIdentifier ports CEscapeInternal from googlesql/public/strings.cc
// (Apache 2.0) with utf8_safe=true and '`' as the quote char to escape.
func escapeIdentifier(s string) string {
	var b strings.Builder
	lastHexEscape := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		isHexEscape := false
		switch c {
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\\':
			b.WriteString(`\\`)
		case '`':
			b.WriteString("\\`")
		default:
			isPrint := c >= 0x20 && c < 0x7f
			isXDigit := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if c < 0x80 && (!isPrint || (lastHexEscape && isXDigit)) {
				fmt.Fprintf(&b, `\x%02X`, c)
				isHexEscape = true
			} else {
				b.WriteByte(c)
			}
		}
		lastHexEscape = isHexEscape
	}
	return b.String()
}
