// Package ast defines the GoogleSQL abstract syntax tree.
//
// The node structure mirrors the parse tree defined in
// github.com/google/googlesql (googlesql/parser/parse_tree.h and ast_node.h)
// so that trees can be rendered byte-for-byte identical to ZetaSQL's
// ASTNode::DebugString output for verification against the upstream parser
// test suite.
package ast

import "reflect"

// Node is implemented by every AST node.
type Node interface {
	// Pos returns the byte offset of the first byte of the node in the input.
	Pos() int
	// End returns the byte offset one past the last byte of the node.
	End() int
	// Children returns the node's children in source order. Nil children are
	// not included.
	Children() []Node
}

// Span records the source location of a node. It is embedded in every node.
type Span struct {
	Start int `json:"start"`
	Stop  int `json:"end"`
}

func (s Span) Pos() int { return s.Start }
func (s Span) End() int { return s.Stop }

func children(nodes ...Node) []Node {
	var out []Node
	for _, n := range nodes {
		if n == nil {
			continue
		}
		// A nil typed pointer (e.g. a nil *Alias field) still produces a
		// non-nil Node interface; filter those out too.
		if v := reflect.ValueOf(n); v.Kind() == reflect.Pointer && v.IsNil() {
			continue
		}
		out = append(out, n)
	}
	return out
}

// Statement is implemented by all statement nodes.
type Statement interface {
	Node
	statementNode()
}

// QueryStatement is a query used as a statement.
type QueryStatement struct {
	Span
	Query *Query `json:"query"`
}

func (n *QueryStatement) statementNode() {}
func (n *QueryStatement) Children() []Node {
	return children(n.Query)
}

// CallStatement is a "CALL procedure(args)" statement; see ASTCallStatement
// in googlesql/parser/parse_tree.h.
type CallStatement struct {
	Span
	Procedure *PathExpression `json:"procedure"`
	Args      []*TVFArgument  `json:"args,omitempty"`
}

func (n *CallStatement) statementNode() {}
func (n *CallStatement) Children() []Node {
	out := children(n.Procedure)
	for _, a := range n.Args {
		out = append(out, a)
	}
	return out
}

// ExecuteImmediateStatement is "EXECUTE IMMEDIATE <expr> [INTO ...] [USING ...]";
// see ASTExecuteImmediateStatement in googlesql/parser/parse_tree.h.
type ExecuteImmediateStatement struct {
	Span
	Sql   Node                `json:"sql"`
	Into  *ExecuteIntoClause  `json:"into,omitempty"`
	Using *ExecuteUsingClause `json:"using,omitempty"`
}

func (n *ExecuteImmediateStatement) statementNode() {}
func (n *ExecuteImmediateStatement) Children() []Node {
	out := children(n.Sql)
	if n.Into != nil {
		out = append(out, n.Into)
	}
	if n.Using != nil {
		out = append(out, n.Using)
	}
	return out
}

// ExecuteIntoClause is "INTO <identifier_list>"; see ASTExecuteIntoClause in
// googlesql/parser/parse_tree.h.
type ExecuteIntoClause struct {
	Span
	Identifiers *IdentifierList `json:"identifiers"`
}

func (n *ExecuteIntoClause) Children() []Node { return children(n.Identifiers) }

// IdentifierList is a comma-separated list of identifiers; see
// ASTIdentifierList in googlesql/parser/parse_tree.h.
type IdentifierList struct {
	Span
	Identifiers []*Identifier `json:"identifiers"`
}

func (n *IdentifierList) Children() []Node {
	out := make([]Node, 0, len(n.Identifiers))
	for _, id := range n.Identifiers {
		out = append(out, id)
	}
	return out
}

// ExecuteUsingClause is "USING <argument_list>"; see ASTExecuteUsingClause in
// googlesql/parser/parse_tree.h.
type ExecuteUsingClause struct {
	Span
	Arguments []*ExecuteUsingArgument `json:"arguments"`
}

func (n *ExecuteUsingClause) Children() []Node {
	out := make([]Node, 0, len(n.Arguments))
	for _, a := range n.Arguments {
		out = append(out, a)
	}
	return out
}

// ExecuteUsingArgument is a single USING argument, optionally aliased with AS;
// see ASTExecuteUsingArgument in googlesql/parser/parse_tree.h.
type ExecuteUsingArgument struct {
	Span
	Expr  Node   `json:"expr"`
	Alias *Alias `json:"alias,omitempty"`
}

func (n *ExecuteUsingArgument) Children() []Node {
	out := children(n.Expr)
	if n.Alias != nil {
		out = append(out, n.Alias)
	}
	return out
}

// HintedStatement wraps a statement preceded by a "@{...}" hint; see
// ASTHintedStatement in googlesql/parser/parse_tree.h.
type HintedStatement struct {
	Span
	Hint      *Hint     `json:"hint"`
	Statement Statement `json:"statement"`
}

func (n *HintedStatement) statementNode() {}
func (n *HintedStatement) Children() []Node {
	return children(n.Hint, n.Statement)
}

// DeleteStatement is a DELETE statement; see ASTDeleteStatement in
// googlesql/parser/parse_tree.h.
type DeleteStatement struct {
	Span
	Target             *PathExpression     `json:"target"`
	Offset             *WithOffset         `json:"offset,omitempty"`
	Where              Node                `json:"where,omitempty"`
	AssertRowsModified *AssertRowsModified `json:"assert_rows_modified,omitempty"`
	Returning          *ReturningClause    `json:"returning,omitempty"`
}

func (n *DeleteStatement) statementNode() {}
func (n *DeleteStatement) Children() []Node {
	return children(n.Target, n.Offset, n.Where, n.AssertRowsModified, n.Returning)
}

// InsertStatement is an INSERT statement; see ASTInsertStatement in
// googlesql/parser/parse_tree.h. InsertMode is "", "IGNORE", "REPLACE", or
// "UPDATE".
type InsertStatement struct {
	Span
	InsertMode         string               `json:"insert_mode,omitempty"`
	Target             *PathExpression      `json:"target"`
	Columns            *ColumnList          `json:"columns,omitempty"`
	Rows               *InsertValuesRowList `json:"rows,omitempty"`
	Query              *Query               `json:"query,omitempty"`
	AssertRowsModified *AssertRowsModified  `json:"assert_rows_modified,omitempty"`
	Returning          *ReturningClause     `json:"returning,omitempty"`
}

func (n *InsertStatement) statementNode() {}
func (n *InsertStatement) Children() []Node {
	out := children(n.Target, n.Columns)
	if n.Rows != nil {
		out = append(out, n.Rows)
	}
	if n.Query != nil {
		out = append(out, n.Query)
	}
	return append(out, children(n.AssertRowsModified, n.Returning)...)
}

// InsertValuesRowList is the VALUES list of an INSERT statement; see
// ASTInsertValuesRowList in googlesql/parser/parse_tree.h.
type InsertValuesRowList struct {
	Span
	Rows []*InsertValuesRow `json:"rows"`
}

func (n *InsertValuesRowList) Children() []Node {
	var out []Node
	for _, r := range n.Rows {
		out = append(out, r)
	}
	return out
}

// InsertValuesRow is a single "( expr, ... )" row in a VALUES list; see
// ASTInsertValuesRow in googlesql/parser/parse_tree.h.
type InsertValuesRow struct {
	Span
	Values []Node `json:"values"`
}

func (n *InsertValuesRow) Children() []Node {
	return children(n.Values...)
}

// UpdateStatement is an UPDATE statement; see ASTUpdateStatement in
// googlesql/parser/parse_tree.h.
type UpdateStatement struct {
	Span
	Target             *PathExpression     `json:"target"`
	Offset             *WithOffset         `json:"offset,omitempty"`
	UpdateItemList     *UpdateItemList     `json:"update_item_list"`
	From               *FromClause         `json:"from,omitempty"`
	Where              Node                `json:"where,omitempty"`
	AssertRowsModified *AssertRowsModified `json:"assert_rows_modified,omitempty"`
	Returning          *ReturningClause    `json:"returning,omitempty"`
}

func (n *UpdateStatement) statementNode() {}
func (n *UpdateStatement) Children() []Node {
	return children(n.Target, n.Offset, n.UpdateItemList, n.From, n.Where, n.AssertRowsModified, n.Returning)
}

// UpdateItemList is the SET item list of an UPDATE statement; see
// ASTUpdateItemList in googlesql/parser/parse_tree.h.
type UpdateItemList struct {
	Span
	Items []*UpdateItem `json:"items"`
}

func (n *UpdateItemList) Children() []Node {
	var out []Node
	for _, it := range n.Items {
		out = append(out, it)
	}
	return out
}

// UpdateItem is a single item in an UPDATE SET list: either a set-value
// assignment or a nested INSERT/UPDATE/DELETE statement; see ASTUpdateItem in
// googlesql/parser/parse_tree.h.
type UpdateItem struct {
	Span
	SetValue  *UpdateSetValue `json:"set_value,omitempty"`
	Statement Statement       `json:"statement,omitempty"`
}

func (n *UpdateItem) Children() []Node {
	if n.SetValue != nil {
		return children(n.SetValue)
	}
	return children(n.Statement)
}

// UpdateSetValue is a "path = value" assignment in an UPDATE SET list; see
// ASTUpdateSetValue in googlesql/parser/parse_tree.h.
type UpdateSetValue struct {
	Span
	Path  Node `json:"path"`
	Value Node `json:"value"`
}

func (n *UpdateSetValue) Children() []Node {
	return children(n.Path, n.Value)
}

// MergeStatement is a MERGE statement; see ASTMergeStatement in
// googlesql/parser/parse_tree.h.
type MergeStatement struct {
	Span
	Target         *PathExpression      `json:"target"`
	Alias          *Alias               `json:"alias,omitempty"`
	Source         Node                 `json:"source"` // *TablePathExpression or *TableSubquery
	MergeCondition Node                 `json:"merge_condition"`
	WhenClauseList *MergeWhenClauseList `json:"when_clause_list"`
}

func (n *MergeStatement) statementNode() {}
func (n *MergeStatement) Children() []Node {
	return children(n.Target, n.Alias, n.Source, n.MergeCondition, n.WhenClauseList)
}

// MergeWhenClauseList is the list of WHEN clauses of a MERGE statement; see
// ASTMergeWhenClauseList in googlesql/parser/parse_tree.h.
type MergeWhenClauseList struct {
	Span
	Clauses []*MergeWhenClause `json:"clauses"`
}

func (n *MergeWhenClauseList) Children() []Node {
	var out []Node
	for _, c := range n.Clauses {
		out = append(out, c)
	}
	return out
}

// MergeWhenClause is a single WHEN clause of a MERGE statement; see
// ASTMergeWhenClause in googlesql/parser/parse_tree.h. MatchType is
// "MATCHED", "NOT_MATCHED_BY_SOURCE", or "NOT_MATCHED_BY_TARGET".
type MergeWhenClause struct {
	Span
	MatchType       string       `json:"match_type"`
	SearchCondition Node         `json:"search_condition,omitempty"`
	Action          *MergeAction `json:"action"`
}

func (n *MergeWhenClause) Children() []Node {
	return children(n.SearchCondition, n.Action)
}

// MergeAction is the action of a MERGE WHEN clause; see ASTMergeAction in
// googlesql/parser/parse_tree.h. ActionType is "INSERT", "UPDATE", or
// "DELETE".
type MergeAction struct {
	Span
	ActionType       string           `json:"action_type"`
	InsertColumnList *ColumnList      `json:"insert_column_list,omitempty"`
	InsertRow        *InsertValuesRow `json:"insert_row,omitempty"`
	UpdateItemList   *UpdateItemList  `json:"update_item_list,omitempty"`
}

func (n *MergeAction) Children() []Node {
	return children(n.InsertColumnList, n.InsertRow, n.UpdateItemList)
}

// AssertRowsModified is the ASSERT_ROWS_MODIFIED clause on a DML statement;
// see ASTAssertRowsModified in googlesql/parser/parse_tree.h.
type AssertRowsModified struct {
	Span
	Value Node `json:"value"`
}

func (n *AssertRowsModified) Children() []Node {
	return children(n.Value)
}

// ReturningClause is the THEN RETURN clause on a DML statement; see
// ASTReturningClause in googlesql/parser/parse_tree.h. ActionAlias records the
// "WITH ACTION [AS alias]" column name (defaulting to "ACTION").
type ReturningClause struct {
	Span
	SelectList  *SelectList `json:"select_list"`
	ActionAlias *Alias      `json:"action_alias,omitempty"`
}

func (n *ReturningClause) Children() []Node {
	return children(n.SelectList, n.ActionAlias)
}

// DotGeneralizedField is "expression . ( path )" generalized field access;
// see ASTDotGeneralizedField in googlesql/parser/parse_tree.h.
type DotGeneralizedField struct {
	Span
	Expr Node            `json:"expr"`
	Path *PathExpression `json:"path"`
}

func (n *DotGeneralizedField) Children() []Node {
	return children(n.Expr, n.Path)
}

// Query is a query expression with an optional WITH clause, optional ORDER
// BY and LIMIT/OFFSET, optionally followed by |> pipe operators.
type Query struct {
	Span
	WithClause    *WithClause  `json:"with_clause,omitempty"`
	QueryExpr     Node         `json:"query_expr"` // *Select, *FromQuery, *SetOperation, or parenthesized *Query
	OrderBy       *OrderBy     `json:"order_by,omitempty"`
	Limit         *LimitOffset `json:"limit,omitempty"`
	LockMode      *LockMode    `json:"lock_mode,omitempty"`
	PipeOperators []Node       `json:"pipe_operators,omitempty"`
	// Parenthesized records that the query was written inside parentheses.
	// It does not appear in the debug output but affects how trailing pipe
	// operators nest; see the query rule in googlesql.tm.
	Parenthesized bool `json:"parenthesized,omitempty"`
	// IsPivotInput records that this query is the input to a PIVOT clause; it
	// renders as " (pivot input)" in the debug string (see ASTQuery in
	// parse_tree.cc).
	IsPivotInput bool `json:"is_pivot_input,omitempty"`
}

func (n *Query) Children() []Node {
	out := children(n.WithClause, n.QueryExpr, n.OrderBy, n.Limit, n.LockMode)
	return append(out, n.PipeOperators...)
}

// WithClause is a WITH clause holding one or more common table expression
// entries; see ASTWithClause in googlesql/parser/parse_tree.h.
type WithClause struct {
	Span
	Recursive bool               `json:"recursive,omitempty"`
	Entries   []*WithClauseEntry `json:"entries"`
}

func (n *WithClause) Children() []Node {
	var out []Node
	for _, e := range n.Entries {
		out = append(out, e)
	}
	return out
}

// WithClauseEntry is a single entry in a WITH clause, wrapping either an
// aliased query or an aliased GROUP ROWS entry; see ASTWithClauseEntry in
// googlesql/parser/parse_tree.h. Exactly one of AliasedQuery and
// AliasedGroupRows is set.
type WithClauseEntry struct {
	Span
	AliasedQuery     *AliasedQuery     `json:"aliased_query,omitempty"`
	AliasedGroupRows *AliasedGroupRows `json:"aliased_group_rows,omitempty"`
}

func (n *WithClauseEntry) Children() []Node {
	return children(n.AliasedQuery, n.AliasedGroupRows)
}

// AliasedGroupRows is "name() AS GROUP ROWS" in a WITH clause; see
// ASTAliasedGroupRows in googlesql/parser/parse_tree.h.
type AliasedGroupRows struct {
	Span
	Identifier *Identifier `json:"identifier"`
}

func (n *AliasedGroupRows) Children() []Node {
	return children(n.Identifier)
}

// AliasedQuery is "name AS ( query )"; see ASTAliasedQuery in
// googlesql/parser/parse_tree.h. The query's location includes the
// parentheses.
type AliasedQuery struct {
	Span
	Identifier *Identifier `json:"identifier"`
	Query      *Query      `json:"query"`
}

func (n *AliasedQuery) Children() []Node {
	return children(n.Identifier, n.Query)
}

// LockMode is a FOR UPDATE locking clause on a query; see ASTLockMode in
// googlesql/parser/parse_tree.h. The only strength is UPDATE.
type LockMode struct {
	Span
}

func (n *LockMode) Children() []Node { return nil }

// Select is a SELECT clause with its associated clauses.
type Select struct {
	Span
	Distinct   bool          `json:"distinct,omitempty"`
	SelectList *SelectList   `json:"select_list"`
	From       *FromClause   `json:"from,omitempty"`
	Where      *WhereClause  `json:"where,omitempty"`
	GroupBy    *GroupBy      `json:"group_by,omitempty"`
	Having     *Having       `json:"having,omitempty"`
	Qualify    *Qualify      `json:"qualify,omitempty"`
	Window     *WindowClause `json:"window,omitempty"`
}

func (n *Select) Children() []Node {
	return children(n.SelectList, n.From, n.Where, n.GroupBy, n.Having, n.Qualify, n.Window)
}

// SelectList is the list of expressions being selected.
type SelectList struct {
	Span
	Columns []*SelectColumn `json:"columns"`
}

func (n *SelectList) Children() []Node {
	var out []Node
	for _, c := range n.Columns {
		out = append(out, c)
	}
	return out
}

// SelectColumn is a single item in a select list.
type SelectColumn struct {
	Span
	Expr  Node   `json:"expr"`
	Alias *Alias `json:"alias,omitempty"`
}

func (n *SelectColumn) Children() []Node {
	return children(n.Expr, n.Alias)
}

// Alias is an [AS] name alias.
type Alias struct {
	Span
	Identifier *Identifier `json:"identifier"`
}

func (n *Alias) Children() []Node {
	return children(n.Identifier)
}

// Star is the * in SELECT *.
type Star struct {
	Span
	Image string `json:"image"`
}

func (n *Star) Children() []Node { return nil }

// StarWithModifiers is "* EXCEPT (...) REPLACE (...)" in a select list; see
// ASTStarWithModifiers in googlesql/parser/parse_tree.h.
type StarWithModifiers struct {
	Span
	Modifiers *StarModifiers `json:"modifiers"`
}

func (n *StarWithModifiers) Children() []Node { return children(n.Modifiers) }

// DotStar is "expression . *" as a select list item; see ASTDotStar in
// googlesql/parser/parse_tree.h.
type DotStar struct {
	Span
	Expr Node `json:"expr"`
}

func (n *DotStar) Children() []Node { return children(n.Expr) }

// DotStarWithModifiers is "expression . * EXCEPT (...) REPLACE (...)"; see
// ASTDotStarWithModifiers in googlesql/parser/parse_tree.h.
type DotStarWithModifiers struct {
	Span
	Expr      Node           `json:"expr"`
	Modifiers *StarModifiers `json:"modifiers"`
}

func (n *DotStarWithModifiers) Children() []Node { return children(n.Expr, n.Modifiers) }

// StarModifiers holds the EXCEPT column list and REPLACE items following "*"
// or ".*"; see ASTStarModifiers in googlesql/parser/parse_tree.h.
type StarModifiers struct {
	Span
	ExceptList   *StarExceptList    `json:"except_list"`
	ReplaceItems []*StarReplaceItem `json:"replace_items"`
}

func (n *StarModifiers) Children() []Node {
	out := children(n.ExceptList)
	for _, item := range n.ReplaceItems {
		out = append(out, item)
	}
	return out
}

// StarExceptList is the "EXCEPT ( identifiers )" part of star modifiers; see
// ASTStarExceptList in googlesql/parser/parse_tree.h.
type StarExceptList struct {
	Span
	Identifiers []*Identifier `json:"identifiers"`
}

func (n *StarExceptList) Children() []Node {
	var out []Node
	for _, id := range n.Identifiers {
		out = append(out, id)
	}
	return out
}

// StarReplaceItem is one "expression AS identifier" inside "REPLACE (...)";
// see ASTStarReplaceItem in googlesql/parser/parse_tree.h.
type StarReplaceItem struct {
	Span
	Expr  Node        `json:"expr"`
	Alias *Identifier `json:"alias"`
}

func (n *StarReplaceItem) Children() []Node { return children(n.Expr, n.Alias) }

// FromClause holds the FROM clause contents.
type FromClause struct {
	Span
	TableExpression Node `json:"table_expression"`
}

func (n *FromClause) Children() []Node {
	return children(n.TableExpression)
}

// TablePathExpression is a table reference by (possibly dotted) path or an
// UNNEST expression; exactly one of Path and UnnestExpr is set.
type TablePathExpression struct {
	Span
	Path       *PathExpression   `json:"path,omitempty"`
	UnnestExpr *UnnestExpression `json:"unnest_expr,omitempty"`
	// Hint is an optional hint (@{...}) between the path and the alias.
	Hint   *Hint       `json:"hint,omitempty"`
	Alias  *Alias      `json:"alias,omitempty"`
	Offset *WithOffset `json:"offset,omitempty"`
	// ForSystemTime is the optional FOR SYSTEM TIME AS OF clause.
	ForSystemTime *ForSystemTime `json:"for_system_time,omitempty"`
	// PostfixOperators holds trailing postfix table operators such as
	// MATCH_RECOGNIZE(...); see ASTPostfixTableOperator in
	// googlesql/parser/parse_tree.h.
	PostfixOperators []Node `json:"postfix_operators,omitempty"`
}

func (n *TablePathExpression) Children() []Node {
	out := children(n.Path, n.UnnestExpr, n.Hint, n.Alias, n.Offset, n.ForSystemTime)
	return append(out, n.PostfixOperators...)
}

// TableSubquery is a parenthesized query used as a table in a FROM clause,
// with an optional alias; see ASTTableSubquery in
// googlesql/parser/parse_tree.h. The span includes the parentheses and the
// alias, but the query's own location excludes the parentheses.
type TableSubquery struct {
	Span
	Query *Query `json:"query"`
	Alias *Alias `json:"alias,omitempty"`
	// IsLateral is true when the subquery is preceded by the LATERAL
	// keyword; the span then includes the keyword.
	IsLateral bool `json:"is_lateral,omitempty"`
	// PostfixOperators holds trailing postfix table operators such as
	// MATCH_RECOGNIZE(...).
	PostfixOperators []Node `json:"postfix_operators,omitempty"`
}

func (n *TableSubquery) Children() []Node {
	out := children(n.Query, n.Alias)
	return append(out, n.PostfixOperators...)
}

// TVF is a call to a table-valued function in a FROM clause, e.g.
// "FROM tvf(arg1, arg2)"; see ASTTVF in googlesql/parser/parse_tree.h. The
// span includes the closing parenthesis and the alias when present.
type TVF struct {
	Span
	Name  *PathExpression `json:"name"`
	Args  []*TVFArgument  `json:"args,omitempty"`
	Alias *Alias          `json:"alias,omitempty"`
	// IsLateral is true when the call is preceded by the LATERAL keyword;
	// the span then includes the keyword.
	IsLateral bool `json:"is_lateral,omitempty"`
	// PostfixOperators holds trailing postfix table operators such as
	// MATCH_RECOGNIZE(...).
	PostfixOperators []Node `json:"postfix_operators,omitempty"`
}

func (n *TVF) Children() []Node {
	out := children(n.Name)
	for _, a := range n.Args {
		out = append(out, a)
	}
	out = append(out, children(n.Alias)...)
	return append(out, n.PostfixOperators...)
}

// TVFArgument is a single argument to a table-valued function call; see
// ASTTVFArgument in googlesql/parser/parse_tree.h.
type TVFArgument struct {
	Span
	Expr Node `json:"expr"`
}

func (n *TVFArgument) Children() []Node {
	return children(n.Expr)
}

// UnnestExpression is UNNEST(expr [AS alias], ...); see ASTUnnestExpression
// in googlesql/parser/parse_tree.h. The span includes the UNNEST keyword and
// the closing parenthesis.
type UnnestExpression struct {
	Span
	Expressions []*ExpressionWithOptAlias `json:"expressions"`
	// ArrayZipMode is an optional trailing named argument, e.g. the
	// "mode => 'pad'" in UNNEST([1], mode => 'pad'); see unnest_expression in
	// googlesql.tm.
	ArrayZipMode *NamedArgument `json:"array_zip_mode,omitempty"`
}

func (n *UnnestExpression) Children() []Node {
	var out []Node
	for _, e := range n.Expressions {
		out = append(out, e)
	}
	if n.ArrayZipMode != nil {
		out = append(out, n.ArrayZipMode)
	}
	return out
}

// ExpressionWithOptAlias is an expression with an optional "AS alias" (the
// AS keyword is required when the alias is present).
type ExpressionWithOptAlias struct {
	Span
	Expr  Node   `json:"expr"`
	Alias *Alias `json:"alias,omitempty"`
}

func (n *ExpressionWithOptAlias) Children() []Node {
	return children(n.Expr, n.Alias)
}

// WithOffset is the WITH OFFSET [[AS] alias] clause on a FROM-clause table
// expression.
type WithOffset struct {
	Span
	Alias *Alias `json:"alias,omitempty"`
}

func (n *WithOffset) Children() []Node {
	return children(n.Alias)
}

// ForSystemTime is a FOR SYSTEM TIME AS OF <expression> clause on a
// FROM-clause table expression; see ASTForSystemTime in
// googlesql/parser/parse_tree.h.
type ForSystemTime struct {
	Span
	Expr Node `json:"expr"`
}

func (n *ForSystemTime) Children() []Node {
	return children(n.Expr)
}

// SampleClause is a TABLESAMPLE clause: "TABLESAMPLE <method> ( <size> )
// [<suffix>]"; see ASTSampleClause in googlesql/parser/parse_tree.h.
type SampleClause struct {
	Span
	Method *Identifier   `json:"method"`
	Size   *SampleSize   `json:"size"`
	Suffix *SampleSuffix `json:"suffix,omitempty"`
}

func (n *SampleClause) Children() []Node {
	return children(n.Method, n.Size, n.Suffix)
}

// SampleSize is the size portion of a TABLESAMPLE clause: "<value> ROWS" or
// "<value> PERCENT", with an optional PARTITION BY; see ASTSampleSize in
// googlesql/parser/parse_tree.h. Unit is stored but not shown in the debug
// string.
type SampleSize struct {
	Span
	Value       Node         `json:"value"`
	Unit        string       `json:"unit"` // "ROWS" or "PERCENT"
	PartitionBy *PartitionBy `json:"partition_by,omitempty"`
}

func (n *SampleSize) Children() []Node {
	return children(n.Value, n.PartitionBy)
}

// SampleSuffix is the optional suffix of a TABLESAMPLE clause: a REPEATABLE
// clause and/or a WITH WEIGHT clause; see ASTSampleSuffix in
// googlesql/parser/parse_tree.h.
type SampleSuffix struct {
	Span
	Weight     *WithWeight       `json:"weight,omitempty"`
	Repeatable *RepeatableClause `json:"repeatable,omitempty"`
}

func (n *SampleSuffix) Children() []Node {
	return children(n.Weight, n.Repeatable)
}

// WithWeight is the "WITH WEIGHT [[AS] alias]" part of a TABLESAMPLE suffix;
// see ASTWithWeight in googlesql/parser/parse_tree.h.
type WithWeight struct {
	Span
	Alias *Alias `json:"alias,omitempty"`
}

func (n *WithWeight) Children() []Node {
	return children(n.Alias)
}

// RepeatableClause is the "REPEATABLE ( <value> )" part of a TABLESAMPLE
// suffix; see ASTRepeatableClause in googlesql/parser/parse_tree.h.
type RepeatableClause struct {
	Span
	Value Node `json:"value"`
}

func (n *RepeatableClause) Children() []Node {
	return children(n.Value)
}

// PathExpressionList is a comma-separated list of path expressions; see
// ASTPathExpressionList in googlesql/parser/parse_tree.h.
type PathExpressionList struct {
	Span
	Paths []*PathExpression `json:"paths"`
}

func (n *PathExpressionList) Children() []Node {
	out := make([]Node, 0, len(n.Paths))
	for _, p := range n.Paths {
		out = append(out, p)
	}
	return out
}

// PivotClause is a "PIVOT(...)" postfix table operator; see ASTPivotClause in
// googlesql/parser/parse_tree.h. Child order matches the reference:
// PivotExpressionList, the FOR expression, PivotValueList, then an optional
// output alias.
type PivotClause struct {
	Span
	Expressions *PivotExpressionList `json:"expressions"`
	ForExpr     Node                 `json:"for_expr"`
	Values      *PivotValueList      `json:"values"`
	Alias       *Alias               `json:"alias,omitempty"`
}

func (n *PivotClause) Children() []Node {
	return children(n.Expressions, n.ForExpr, n.Values, n.Alias)
}

// PivotExpressionList is the list of pivot expressions in a PIVOT clause; see
// ASTPivotExpressionList in googlesql/parser/parse_tree.h.
type PivotExpressionList struct {
	Span
	Expressions []*PivotExpression `json:"expressions"`
}

func (n *PivotExpressionList) Children() []Node {
	out := make([]Node, 0, len(n.Expressions))
	for _, e := range n.Expressions {
		out = append(out, e)
	}
	return out
}

// PivotExpression is a single pivot expression with an optional alias; see
// ASTPivotExpression in googlesql/parser/parse_tree.h.
type PivotExpression struct {
	Span
	Expr  Node   `json:"expr"`
	Alias *Alias `json:"alias,omitempty"`
}

func (n *PivotExpression) Children() []Node {
	return children(n.Expr, n.Alias)
}

// PivotValueList is the list of pivot values in a PIVOT clause; see
// ASTPivotValueList in googlesql/parser/parse_tree.h.
type PivotValueList struct {
	Span
	Values []*PivotValue `json:"values"`
}

func (n *PivotValueList) Children() []Node {
	out := make([]Node, 0, len(n.Values))
	for _, v := range n.Values {
		out = append(out, v)
	}
	return out
}

// PivotValue is a single pivot value with an optional alias; see
// ASTPivotValue in googlesql/parser/parse_tree.h.
type PivotValue struct {
	Span
	Value Node   `json:"value"`
	Alias *Alias `json:"alias,omitempty"`
}

func (n *PivotValue) Children() []Node {
	return children(n.Value, n.Alias)
}

// UnpivotClause is an "UNPIVOT(...)" postfix table operator; see
// ASTUnpivotClause in googlesql/parser/parse_tree.h. Child order matches the
// reference: the unpivot column list, the FOR path expression, the IN item
// list, then an optional output alias.
type UnpivotClause struct {
	Span
	// NullFilter is "", "EXCLUDE NULLS", or "INCLUDE NULLS".
	NullFilter string              `json:"null_filter,omitempty"`
	Columns    *PathExpressionList `json:"columns"`
	ForExpr    *PathExpression     `json:"for_expr"`
	InItems    *UnpivotInItemList  `json:"in_items"`
	Alias      *Alias              `json:"alias,omitempty"`
}

func (n *UnpivotClause) Children() []Node {
	return children(n.Columns, n.ForExpr, n.InItems, n.Alias)
}

// UnpivotInItemList is the list of IN items in an UNPIVOT clause; see
// ASTUnpivotInItemList in googlesql/parser/parse_tree.h. Its span includes
// the surrounding parentheses.
type UnpivotInItemList struct {
	Span
	Items []*UnpivotInItem `json:"items"`
}

func (n *UnpivotInItemList) Children() []Node {
	out := make([]Node, 0, len(n.Items))
	for _, it := range n.Items {
		out = append(out, it)
	}
	return out
}

// UnpivotInItem is a single IN item in an UNPIVOT clause: a column list with
// an optional label; see ASTUnpivotInItem in googlesql/parser/parse_tree.h.
type UnpivotInItem struct {
	Span
	Columns *PathExpressionList `json:"columns"`
	Label   *UnpivotInItemLabel `json:"label,omitempty"`
}

func (n *UnpivotInItem) Children() []Node {
	return children(n.Columns, n.Label)
}

// UnpivotInItemLabel is the "[AS] (string|integer) literal" label of an
// UNPIVOT IN item; see ASTUnpivotInItemLabel in
// googlesql/parser/parse_tree.h.
type UnpivotInItemLabel struct {
	Span
	Label Node `json:"label"`
}

func (n *UnpivotInItemLabel) Children() []Node {
	return children(n.Label)
}

// Join is a JOIN between two table expressions, including comma joins; see
// ASTJoin in googlesql/parser/parse_tree.h. Child order matches the
// reference: Lhs, Hint, JoinLocation, Rhs, then the ON/USING clause (or the
// clause list before consecutive-ON transformation).
type Join struct {
	Span
	// JoinType is "" for a plain JOIN, or "COMMA", "CROSS", "FULL", "INNER",
	// "LEFT", or "RIGHT".
	JoinType string `json:"join_type,omitempty"`
	// JoinHint is "" or the join hint keyword "HASH" or "LOOKUP".
	JoinHint string `json:"join_hint,omitempty"`
	Natural  bool   `json:"natural,omitempty"`
	Lhs      Node   `json:"lhs"`
	Hint     *Hint  `json:"hint,omitempty"`
	// JoinLocation covers the join keywords (e.g. "LEFT OUTER JOIN"), or the
	// comma of a comma join.
	JoinLocation *Location `json:"join_location"`
	Rhs          Node      `json:"rhs"`
	// OnOrUsingClause is the *OnClause or *UsingClause, if any.
	OnOrUsingClause Node `json:"on_or_using_clause,omitempty"`
	// ClauseList holds two or more consecutive ON/USING clauses before the
	// join transformation dissolves them; see ASTOnOrUsingClauseList and
	// googlesql/parser/join_processor.cc.
	ClauseList *OnOrUsingClauseList `json:"clause_list,omitempty"`

	// The fields below are parser bookkeeping used by the consecutive
	// ON/USING clause transformation (join_processor.cc); they are not part
	// of the parse tree.
	UnmatchedJoinCount   int             `json:"-"`
	TransformationNeeded bool            `json:"-"`
	ContainsCommaJoin    bool            `json:"-"`
	ParseError           *JoinParseError `json:"-"`
}

func (n *Join) Children() []Node {
	return children(n.Lhs, n.Hint, n.JoinLocation, n.Rhs, n.OnOrUsingClause, n.ClauseList)
}

// JoinParseError is a deferred error recorded on a Join when there are more
// ON/USING clauses than joins that need one; see ASTJoin::ParseError in
// googlesql/parser/parse_tree.h.
type JoinParseError struct {
	ErrorNode Node
	Message   string
}

// OnClause is the ON condition of a join; see ASTOnClause in
// googlesql/parser/parse_tree.h.
type OnClause struct {
	Span
	Expr Node `json:"expr"`
}

func (n *OnClause) Children() []Node {
	return children(n.Expr)
}

// UsingClause is the USING (column, ...) clause of a join; see
// ASTUsingClause in googlesql/parser/parse_tree.h. The span includes the
// USING keyword and the parentheses.
type UsingClause struct {
	Span
	Keys []*Identifier `json:"keys"`
}

func (n *UsingClause) Children() []Node {
	var out []Node
	for _, k := range n.Keys {
		out = append(out, k)
	}
	return out
}

// OnOrUsingClauseList holds consecutive ON/USING clauses of a join before
// the join transformation; see ASTOnOrUsingClauseList in
// googlesql/parser/parse_tree.h.
type OnOrUsingClauseList struct {
	Span
	Clauses []Node `json:"clauses"` // *OnClause or *UsingClause
}

func (n *OnOrUsingClauseList) Children() []Node {
	return append([]Node(nil), n.Clauses...)
}

// ParenthesizedJoin is "( join )" used as a table primary; see
// ASTParenthesizedJoin in googlesql/parser/parse_tree.h. The span includes
// the parentheses.
type ParenthesizedJoin struct {
	Span
	Join Node `json:"join"`
	// PostfixOperators holds trailing postfix table operators such as
	// MATCH_RECOGNIZE(...).
	PostfixOperators []Node `json:"postfix_operators,omitempty"`
}

func (n *ParenthesizedJoin) Children() []Node {
	out := children(n.Join)
	return append(out, n.PostfixOperators...)
}

// PipeJoin is a |> JOIN pipe operator; see ASTPipeJoin in
// googlesql/parser/parse_tree.h. The contained Join's Lhs is a
// PipeJoinLhsPlaceholder because the left input is the pipe input.
type PipeJoin struct {
	Span
	Join Node `json:"join"`
}

func (n *PipeJoin) Children() []Node {
	return children(n.Join)
}

// PipeJoinLhsPlaceholder stands in for the missing left input of a pipe
// JOIN; see ASTPipeJoinLhsPlaceholder in googlesql/parser/parse_tree.h.
type PipeJoinLhsPlaceholder struct {
	Span
}

func (n *PipeJoinLhsPlaceholder) Children() []Node { return nil }

// ArrayConstructor is "[...]" or "ARRAY[...]"; see ASTArrayConstructor in
// googlesql/parser/parse_tree.h.
type ArrayConstructor struct {
	Span
	Type     *ArrayType `json:"type,omitempty"`
	Elements []Node     `json:"elements"`
}

func (n *ArrayConstructor) Children() []Node {
	return append(children(n.Type), n.Elements...)
}

// WhereClause holds the WHERE clause expression.
type WhereClause struct {
	Span
	Expr Node `json:"expr"`
}

func (n *WhereClause) Children() []Node {
	return children(n.Expr)
}

// GroupBy is a GROUP BY clause.
type GroupBy struct {
	Span
	Hint  *Hint           `json:"hint,omitempty"`
	All   *GroupByAll     `json:"all,omitempty"`
	Items []*GroupingItem `json:"items"`
}

func (n *GroupBy) Children() []Node {
	out := children(n.Hint)
	if n.All != nil {
		out = append(out, n.All)
	}
	for _, item := range n.Items {
		out = append(out, item)
	}
	return out
}

// GroupByAll is the ALL keyword in "GROUP BY ALL".
type GroupByAll struct {
	Span
}

func (n *GroupByAll) Children() []Node { return nil }

// GroupingItem is a single GROUP BY item.
type GroupingItem struct {
	Span
	Expr Node `json:"expr"`
}

func (n *GroupingItem) Children() []Node {
	return children(n.Expr)
}

// Having is a HAVING clause.
type Having struct {
	Span
	Expr Node `json:"expr"`
}

func (n *Having) Children() []Node {
	return children(n.Expr)
}

// Qualify is a QUALIFY clause.
type Qualify struct {
	Span
	Expr Node `json:"expr"`
}

func (n *Qualify) Children() []Node {
	return children(n.Expr)
}

// OrderBy is an ORDER BY clause.
type OrderBy struct {
	Span
	Hint  *Hint                 `json:"hint,omitempty"`
	Items []*OrderingExpression `json:"items"`
}

func (n *OrderBy) Children() []Node {
	out := children(n.Hint)
	for _, item := range n.Items {
		out = append(out, item)
	}
	return out
}

// OrderingExpression is a single ORDER BY item.
type OrderingExpression struct {
	Span
	Expr       Node       `json:"expr"`
	Collate    *Collate   `json:"collate,omitempty"`
	NullOrder  *NullOrder `json:"null_order,omitempty"`
	Descending bool       `json:"descending,omitempty"`
	HasAsc     bool       `json:"has_asc,omitempty"` // explicit ASC keyword present
}

func (n *OrderingExpression) Children() []Node {
	return children(n.Expr, n.Collate, n.NullOrder)
}

// Collate is a "COLLATE <string literal or parameter>" clause; see ASTCollate
// in googlesql/parser/parse_tree.h. The span includes the COLLATE keyword.
type Collate struct {
	Span
	Name Node `json:"name"`
}

func (n *Collate) Children() []Node {
	return children(n.Name)
}

// NullOrder is a "NULLS FIRST" or "NULLS LAST" clause on an ordering
// expression; see ASTNullOrder in googlesql/parser/parse_tree.h.
type NullOrder struct {
	Span
	NullsFirst bool `json:"nulls_first"`
}

func (n *NullOrder) Children() []Node { return nil }

// Hint is a "@{name=value, ...}" hint annotation, optionally preceded by an
// integer shard count as in "@4"; see ASTHint in
// googlesql/parser/parse_tree.h. The span starts at the "@".
type Hint struct {
	Span
	NumShardsHint Node         `json:"num_shards_hint,omitempty"` // integer literal in @<int>
	Entries       []*HintEntry `json:"entries,omitempty"`
}

func (n *Hint) Children() []Node {
	out := children(n.NumShardsHint)
	for _, e := range n.Entries {
		out = append(out, e)
	}
	return out
}

// HintEntry is a single "[qualifier.]name = value" entry in a hint; see
// ASTHintEntry in googlesql/parser/parse_tree.h.
type HintEntry struct {
	Span
	Qualifier *Identifier `json:"qualifier,omitempty"`
	Name      *Identifier `json:"name"`
	Value     Node        `json:"value"`
}

func (n *HintEntry) Children() []Node {
	return children(n.Qualifier, n.Name, n.Value)
}

// LimitOffset is a LIMIT [OFFSET] clause.
type LimitOffset struct {
	Span
	Limit  *Limit `json:"limit"`
	Offset Node   `json:"offset,omitempty"`
}

func (n *LimitOffset) Children() []Node {
	return children(n.Limit, n.Offset)
}

// Limit wraps the expression (or ALL) of a LIMIT clause, including the LIMIT
// keyword in its location.
type Limit struct {
	Span
	Expr Node `json:"expr"` // expression, or *LimitAll for LIMIT ALL
}

func (n *Limit) Children() []Node {
	return children(n.Expr)
}

// LimitAll is the ALL keyword in LIMIT ALL.
type LimitAll struct {
	Span
}

func (n *LimitAll) Children() []Node { return nil }

// Identifier is a single (possibly quoted) identifier.
type Identifier struct {
	Span
	Name string `json:"name"` // unquoted name
}

func (n *Identifier) Children() []Node { return nil }

// PathExpression is a dotted path of identifiers.
type PathExpression struct {
	Span
	Names []*Identifier `json:"names"`
}

func (n *PathExpression) Children() []Node {
	var out []Node
	for _, name := range n.Names {
		out = append(out, name)
	}
	return out
}

// NullLiteral is the NULL literal.
type NullLiteral struct {
	Span
	Image string `json:"image"`
}

func (n *NullLiteral) Children() []Node { return nil }

// BooleanLiteral is TRUE or FALSE.
type BooleanLiteral struct {
	Span
	Image string `json:"image"`
	Value bool   `json:"value"`
}

func (n *BooleanLiteral) Children() []Node { return nil }

// IntLiteral is an integer literal.
type IntLiteral struct {
	Span
	Image string `json:"image"`
}

func (n *IntLiteral) Children() []Node { return nil }

// FloatLiteral is a floating point literal.
type FloatLiteral struct {
	Span
	Image string `json:"image"`
}

func (n *FloatLiteral) Children() []Node { return nil }

// StringLiteral is a string literal, made of one or more concatenated
// components.
type StringLiteral struct {
	Span
	Components []*StringLiteralComponent `json:"components"`
}

func (n *StringLiteral) Children() []Node {
	var out []Node
	for _, c := range n.Components {
		out = append(out, c)
	}
	return out
}

// StringLiteralComponent is a single quoted piece of a string literal.
type StringLiteralComponent struct {
	Span
	Image string `json:"image"` // raw text including quotes and any prefix
}

func (n *StringLiteralComponent) Children() []Node { return nil }

// BytesLiteral is a bytes literal, made of one or more concatenated
// components.
type BytesLiteral struct {
	Span
	Components []*BytesLiteralComponent `json:"components"`
}

func (n *BytesLiteral) Children() []Node {
	var out []Node
	for _, c := range n.Components {
		out = append(out, c)
	}
	return out
}

// BytesLiteralComponent is a single quoted piece of a bytes literal.
type BytesLiteralComponent struct {
	Span
	Image string `json:"image"` // raw text including quotes and prefix
}

func (n *BytesLiteralComponent) Children() []Node { return nil }

// NumericLiteral is a NUMERIC/DECIMAL typed literal: NUMERIC '123'.
type NumericLiteral struct {
	Span
	Value *StringLiteral `json:"value"`
}

func (n *NumericLiteral) Children() []Node { return children(n.Value) }

// BigNumericLiteral is a BIGNUMERIC/BIGDECIMAL typed literal.
type BigNumericLiteral struct {
	Span
	Value *StringLiteral `json:"value"`
}

func (n *BigNumericLiteral) Children() []Node { return children(n.Value) }

// JSONLiteral is a JSON typed literal: JSON '{"a": 1}'.
type JSONLiteral struct {
	Span
	Value *StringLiteral `json:"value"`
}

func (n *JSONLiteral) Children() []Node { return children(n.Value) }

// DateOrTimeLiteral is a DATE/DATETIME/TIME/TIMESTAMP typed literal. TypeKind
// is one of the ZetaSQL type kind names, e.g. "TYPE_DATE".
type DateOrTimeLiteral struct {
	Span
	TypeKind string         `json:"type_kind"`
	Value    *StringLiteral `json:"value"`
}

func (n *DateOrTimeLiteral) Children() []Node { return children(n.Value) }

// IntervalExpr is an INTERVAL expression: INTERVAL <value> <datepart> with an
// optional "TO <datepart>" range; see ASTIntervalExpr / interval_expression in
// googlesql.tm.
type IntervalExpr struct {
	Span
	Value      Node        `json:"value"`
	DatePart   *Identifier `json:"date_part"`
	DatePartTo *Identifier `json:"date_part_to,omitempty"`
}

func (n *IntervalExpr) Children() []Node {
	return children(n.Value, n.DatePart, n.DatePartTo)
}

// RangeLiteral is a RANGE<type> typed literal: RANGE<DATE> '[a, b)'.
type RangeLiteral struct {
	Span
	Type  *RangeType     `json:"type"`
	Value *StringLiteral `json:"value"`
}

func (n *RangeLiteral) Children() []Node { return children(n.Type, n.Value) }

// UnaryExpression is a unary operator applied to an expression.
type UnaryExpression struct {
	Span
	Op      string `json:"op"` // "-", "+", "~", "NOT"
	Operand Node   `json:"operand"`
}

func (n *UnaryExpression) Children() []Node {
	return children(n.Operand)
}

// BinaryExpression is a binary operator applied to two expressions.
type BinaryExpression struct {
	Span
	Op    string `json:"op"` // "+", "-", "*", "/", "=", "!=", "<", ...
	IsNot bool   `json:"is_not,omitempty"`
	Left  Node   `json:"left"`
	Right Node   `json:"right"`
}

func (n *BinaryExpression) Children() []Node {
	return children(n.Left, n.Right)
}

// BitwiseShiftExpression is <lhs> (<< | >>) <rhs>. Unlike other binary
// operators, ZetaSQL gives the shift operators a dedicated node with an
// explicit Location child covering the operator token; see
// ASTBitwiseShiftExpression in googlesql/parser/parse_tree.h.
type BitwiseShiftExpression struct {
	Span
	IsLeftShift      bool      `json:"is_left_shift"`
	Lhs              Node      `json:"lhs"`
	OperatorLocation *Location `json:"operator_location"`
	Rhs              Node      `json:"rhs"`
}

func (n *BitwiseShiftExpression) Children() []Node {
	return children(n.Lhs, n.OperatorLocation, n.Rhs)
}

// AndExpr is an AND expression with two or more conjuncts, flattened.
type AndExpr struct {
	Span
	Conjuncts []Node `json:"conjuncts"`
}

func (n *AndExpr) Children() []Node {
	return append([]Node(nil), n.Conjuncts...)
}

// OrExpr is an OR expression with two or more disjuncts, flattened.
type OrExpr struct {
	Span
	Disjuncts []Node `json:"disjuncts"`
}

func (n *OrExpr) Children() []Node {
	return append([]Node(nil), n.Disjuncts...)
}

// Location marks the source location of a piece of syntax that has no
// dedicated node, such as the BETWEEN keyword inside a BetweenExpression.
type Location struct {
	Span
}

func (n *Location) Children() []Node { return nil }

// BetweenExpression is <lhs> [NOT] BETWEEN <low> AND <high>.
type BetweenExpression struct {
	Span
	IsNot           bool      `json:"is_not,omitempty"`
	Lhs             Node      `json:"lhs"`
	BetweenLocation *Location `json:"between_location"`
	Low             Node      `json:"low"`
	High            Node      `json:"high"`
}

func (n *BetweenExpression) Children() []Node {
	return children(n.Lhs, n.BetweenLocation, n.Low, n.High)
}

// ClampedBetweenModifier is the "CLAMPED BETWEEN <low> AND <high>" modifier
// on aggregate function calls; see ASTClampedBetweenModifier in
// googlesql/parser/parse_tree.h.
type ClampedBetweenModifier struct {
	Span
	Low  Node `json:"low"`
	High Node `json:"high"`
}

func (n *ClampedBetweenModifier) Children() []Node {
	return children(n.Low, n.High)
}

// InList is the parenthesized value list of an IN or LIKE ANY/SOME/ALL
// expression; see ASTInList in googlesql/parser/parse_tree.h. Its location
// covers the expressions but not the enclosing parentheses (except for a
// single extra-parenthesized subquery element).
type InList struct {
	Span
	Exprs []Node `json:"exprs"`
}

func (n *InList) Children() []Node { return n.Exprs }

// InExpression is <lhs> [NOT] IN <rhs> where rhs is exactly one of a value
// list, a subquery, or an UNNEST expression; see ASTInExpression in
// googlesql/parser/parse_tree.h.
type InExpression struct {
	Span
	IsNot      bool              `json:"is_not,omitempty"`
	Lhs        Node              `json:"lhs"`
	InLocation *Location         `json:"in_location"`
	Query      *Query            `json:"query,omitempty"`
	List       *InList           `json:"in_list,omitempty"`
	UnnestExpr *UnnestExpression `json:"unnest_expr,omitempty"`
}

func (n *InExpression) Children() []Node {
	return children(n.Lhs, n.InLocation, n.Query, n.List, n.UnnestExpr)
}

// AnySomeAllOp is the ANY, SOME, or ALL quantifier of a LIKE or comparison
// expression; see ASTAnySomeAllOp in googlesql/parser/parse_tree.h.
type AnySomeAllOp struct {
	Span
	Op string `json:"op"` // "ANY", "SOME", or "ALL"
}

func (n *AnySomeAllOp) Children() []Node { return nil }

// LikeExpression is <lhs> [NOT] LIKE ANY|SOME|ALL <rhs> where rhs is exactly
// one of a value list, a subquery, or an UNNEST expression; see
// ASTLikeExpression in googlesql/parser/parse_tree.h. Plain "<lhs> LIKE
// <rhs>" is a BinaryExpression instead.
type LikeExpression struct {
	Span
	IsNot        bool              `json:"is_not,omitempty"`
	Lhs          Node              `json:"lhs"`
	LikeLocation *Location         `json:"like_location"`
	Op           *AnySomeAllOp     `json:"op"`
	Query        *Query            `json:"query,omitempty"`
	List         *InList           `json:"in_list,omitempty"`
	UnnestExpr   *UnnestExpression `json:"unnest_expr,omitempty"`
}

func (n *LikeExpression) Children() []Node {
	return children(n.Lhs, n.LikeLocation, n.Op, n.Query, n.List, n.UnnestExpr)
}

// StructConstructorWithParens is "(expr1, expr2 [, ... ])" with at least two
// expressions; see ASTStructConstructorWithParens in parse_tree.h. The
// single-expression form is a parenthesized expression, not a struct.
type StructConstructorWithParens struct {
	Span
	FieldExpressions []Node `json:"field_expressions"`
}

func (n *StructConstructorWithParens) Children() []Node {
	return children(n.FieldExpressions...)
}

// StructConstructorArg is one "expression [AS alias]" argument of a
// keyword-form struct constructor; see ASTStructConstructorArg in
// parse_tree.h. The alias requires the AS keyword (see
// as_alias_with_required_as in googlesql.tm).
type StructConstructorArg struct {
	Span
	Expression Node   `json:"expression"`
	Alias      *Alias `json:"alias,omitempty"`
}

func (n *StructConstructorArg) Children() []Node {
	return children(n.Expression, n.Alias)
}

// StructConstructorWithKeyword is "STRUCT(...)" or "STRUCT<...>(...)"; see
// ASTStructConstructorWithKeyword in parse_tree.h. StructType is nil for the
// typeless "STRUCT(...)" form.
type StructConstructorWithKeyword struct {
	Span
	StructType *StructType             `json:"struct_type,omitempty"`
	Fields     []*StructConstructorArg `json:"fields,omitempty"`
}

func (n *StructConstructorWithKeyword) Children() []Node {
	out := children(n.StructType)
	for _, f := range n.Fields {
		out = append(out, f)
	}
	return out
}

// NewConstructor is "NEW type_name(arg, ...)"; see ASTNewConstructor in
// parse_tree.h.
type NewConstructor struct {
	Span
	TypeName *SimpleType          `json:"type_name"`
	Args     []*NewConstructorArg `json:"args,omitempty"`
}

func (n *NewConstructor) Children() []Node {
	out := children(n.TypeName)
	for _, a := range n.Args {
		out = append(out, a)
	}
	return out
}

// NewConstructorArg is one "expression [AS identifier | AS (path)]" argument
// of a NEW constructor; see ASTNewConstructorArg in parse_tree.h. At most
// one of OptionalIdentifier and OptionalPathExpression is set.
type NewConstructorArg struct {
	Span
	Expression             Node            `json:"expression"`
	OptionalIdentifier     *Identifier     `json:"optional_identifier,omitempty"`
	OptionalPathExpression *PathExpression `json:"optional_path_expression,omitempty"`
}

func (n *NewConstructorArg) Children() []Node {
	return children(n.Expression, n.OptionalIdentifier, n.OptionalPathExpression)
}

// BracedNewConstructor is "NEW type_name { ... }"; see
// ASTBracedNewConstructor in parse_tree.h.
type BracedNewConstructor struct {
	Span
	TypeName    *SimpleType        `json:"type_name"`
	Constructor *BracedConstructor `json:"constructor"`
}

func (n *BracedNewConstructor) Children() []Node {
	return children(n.TypeName, n.Constructor)
}

// BracedConstructor is a braced proto/struct constructor body
// "{ field [, field ...] }"; see ASTBracedConstructor in parse_tree.h.
type BracedConstructor struct {
	Span
	Fields []*BracedConstructorField `json:"fields,omitempty"`
}

func (n *BracedConstructor) Children() []Node {
	out := make([]Node, 0, len(n.Fields))
	for _, f := range n.Fields {
		out = append(out, f)
	}
	return out
}

// BracedConstructorField is one "lhs: value" or "lhs { ... }" entry of a
// braced constructor; see ASTBracedConstructorField in parse_tree.h.
// CommaSeparated records whether the field was preceded by a comma (fields
// may also be separated by whitespace only, proto text-format style).
type BracedConstructorField struct {
	Span
	Lhs            *BracedConstructorLhs        `json:"lhs"`
	Value          *BracedConstructorFieldValue `json:"value"`
	CommaSeparated bool                         `json:"comma_separated,omitempty"`
}

func (n *BracedConstructorField) Children() []Node {
	return children(n.Lhs, n.Value)
}

// BracedConstructorLhs is the field name of a braced constructor field: a
// path expression, or a parenthesized extension path "(path.to.extension)";
// see ASTBracedConstructorLhs in parse_tree.h.
type BracedConstructorLhs struct {
	Span
	Expression Node `json:"expression"`
}

func (n *BracedConstructorLhs) Children() []Node {
	return children(n.Expression)
}

// BracedConstructorFieldValue is the value of a braced constructor field;
// see ASTBracedConstructorFieldValue in parse_tree.h. ColonPrefixed is true
// for "lhs: value" and false for the sub-message form "lhs { ... }".
type BracedConstructorFieldValue struct {
	Span
	Expression    Node `json:"expression"`
	ColonPrefixed bool `json:"colon_prefixed,omitempty"`
}

func (n *BracedConstructorFieldValue) Children() []Node {
	return children(n.Expression)
}

// StructBracedConstructor is "STRUCT { ... }" or "STRUCT<...> { ... }"; see
// ASTStructBracedConstructor in parse_tree.h. StructType is nil for the
// typeless form.
type StructBracedConstructor struct {
	Span
	StructType  *StructType        `json:"struct_type,omitempty"`
	Constructor *BracedConstructor `json:"constructor"`
}

func (n *StructBracedConstructor) Children() []Node {
	return children(n.StructType, n.Constructor)
}

// FromQuery is a standalone FROM clause used as a query, e.g. "FROM t";
// see ASTFromQuery in googlesql/parser/parse_tree.h.
type FromQuery struct {
	Span
	From *FromClause `json:"from"`
}

func (n *FromQuery) Children() []Node {
	return children(n.From)
}

// Subpipeline is a parenthesized sequence of pipe operators, e.g.
// "(|> WHERE x)"; see ASTSubpipeline in googlesql/parser/parse_tree.h. The
// span includes the parentheses.
type Subpipeline struct {
	Span
	PipeOperators []Node `json:"pipe_operators,omitempty"`
}

func (n *Subpipeline) Children() []Node {
	return append([]Node(nil), n.PipeOperators...)
}

// PipeLog is a |> LOG pipe operator with an optional subpipeline.
type PipeLog struct {
	Span
	Subpipeline *Subpipeline `json:"subpipeline,omitempty"`
}

func (n *PipeLog) Children() []Node {
	return children(n.Subpipeline)
}

// PipeSelect is a |> SELECT pipe operator. The select list is represented
// as an ASTSelect for resolver code sharing in the reference implementation.
type PipeSelect struct {
	Span
	Select *Select `json:"select"`
}

func (n *PipeSelect) Children() []Node {
	return children(n.Select)
}

// PipeExtend is a |> EXTEND pipe operator. The selection item list is
// represented as an ASTSelect for resolver code sharing in the reference
// implementation.
type PipeExtend struct {
	Span
	Select *Select `json:"select"`
}

func (n *PipeExtend) Children() []Node {
	return children(n.Select)
}

// PipeWindow is a |> WINDOW pipe operator. The selection item list is
// represented as an ASTSelect for resolver code sharing in the reference
// implementation.
type PipeWindow struct {
	Span
	Select *Select `json:"select"`
}

func (n *PipeWindow) Children() []Node {
	return children(n.Select)
}

// PipeLimitOffset is a |> LIMIT [OFFSET] pipe operator.
type PipeLimitOffset struct {
	Span
	LimitOffset *LimitOffset `json:"limit_offset"`
}

func (n *PipeLimitOffset) Children() []Node {
	return children(n.LimitOffset)
}

// PipeDistinct is a |> DISTINCT pipe operator; it has no children.
type PipeDistinct struct {
	Span
}

func (n *PipeDistinct) Children() []Node { return nil }

// PipeAggregate is a |> AGGREGATE pipe operator. The aggregate list and
// optional GROUP BY are represented as an ASTSelect for resolver code
// sharing in the reference implementation.
type PipeAggregate struct {
	Span
	Select *Select `json:"select"`
}

func (n *PipeAggregate) Children() []Node {
	return children(n.Select)
}

// PipeWhere is a |> WHERE pipe operator.
type PipeWhere struct {
	Span
	Where *WhereClause `json:"where"`
}

func (n *PipeWhere) Children() []Node {
	return children(n.Where)
}

// PipeTablesample is a |> TABLESAMPLE pipe operator; see ASTPipeTablesample
// in googlesql/parser/parse_tree.h.
type PipeTablesample struct {
	Span
	Sample *SampleClause `json:"sample"`
}

func (n *PipeTablesample) Children() []Node {
	return children(n.Sample)
}

// PipeOrderBy is a |> ORDER BY pipe operator.
type PipeOrderBy struct {
	Span
	OrderBy *OrderBy `json:"order_by"`
}

func (n *PipeOrderBy) Children() []Node {
	return children(n.OrderBy)
}

// PipeSet is a |> SET pipe operator.
type PipeSet struct {
	Span
	Items []*PipeSetItem `json:"items"`
}

func (n *PipeSet) Children() []Node {
	var out []Node
	for _, item := range n.Items {
		out = append(out, item)
	}
	return out
}

// PipeSetItem is a single "column = expression" assignment in a pipe SET
// operator.
type PipeSetItem struct {
	Span
	Column *Identifier `json:"column"`
	Expr   Node        `json:"expr"`
}

func (n *PipeSetItem) Children() []Node {
	return children(n.Column, n.Expr)
}

// AlterStatement is an ALTER <object kind> statement. NodeName holds the
// per-object-kind parse tree node name (e.g. "AlterTableStatement",
// "AlterViewStatement"), matching the distinct ASTAlter*Statement node
// classes in the reference implementation.
type AlterStatement struct {
	Span
	NodeName   string           `json:"node_name"`
	IsIfExists bool             `json:"is_if_exists,omitempty"`
	Path       *PathExpression  `json:"path"`
	Actions    *AlterActionList `json:"actions"`
}

func (n *AlterStatement) statementNode() {}
func (n *AlterStatement) Children() []Node {
	return children(n.Path, n.Actions)
}

// AlterActionList is the comma-separated list of actions in an ALTER
// statement.
type AlterActionList struct {
	Span
	Actions []Node `json:"actions"`
}

func (n *AlterActionList) Children() []Node {
	return append([]Node(nil), n.Actions...)
}

// RenameToClause is a RENAME TO <path> alter action.
type RenameToClause struct {
	Span
	NewName *PathExpression `json:"new_name"`
}

func (n *RenameToClause) Children() []Node {
	return children(n.NewName)
}

// SetOptionsAction is a SET OPTIONS (...) alter action.
type SetOptionsAction struct {
	Span
	Options *OptionsList `json:"options"`
}

func (n *SetOptionsAction) Children() []Node {
	return children(n.Options)
}

// OptionsList is a parenthesized list of name = value options.
type OptionsList struct {
	Span
	Entries []*OptionsEntry `json:"entries"`
}

func (n *OptionsList) Children() []Node {
	var out []Node
	for _, e := range n.Entries {
		out = append(out, e)
	}
	return out
}

// OptionsEntry is a single "name <op> value" entry in an options list. Op is
// "=", "+=", or "-=".
type OptionsEntry struct {
	Span
	Name  *Identifier `json:"name"`
	Op    string      `json:"op"`
	Value Node        `json:"value"`
}

func (n *OptionsEntry) Children() []Node {
	return children(n.Name, n.Value)
}

// ExpressionSubquery is a subquery used as an expression: "( query )",
// "ARRAY( query )", or "EXISTS( query )"; see ASTExpressionSubquery in
// googlesql/parser/parse_tree.h. The span includes the parentheses (and the
// modifier keyword, if any); the inner query keeps the span of the query
// text inside the parentheses. Modifier is "", "ARRAY", "EXISTS", or
// "VALUE".
type ExpressionSubquery struct {
	Span
	Modifier string `json:"modifier,omitempty"`
	Hint     *Hint  `json:"hint,omitempty"`
	Query    *Query `json:"query"`
}

func (n *ExpressionSubquery) Children() []Node {
	return children(n.Hint, n.Query)
}

// CaseValueExpression is "CASE value WHEN ... THEN ... [ELSE ...] END"; see
// ASTCaseValueExpression in googlesql/parser/parse_tree.h. Arguments holds
// the value expression, then alternating WHEN/THEN expression pairs, then
// the optional ELSE expression.
type CaseValueExpression struct {
	Span
	Arguments []Node `json:"arguments"`
}

func (n *CaseValueExpression) Children() []Node {
	return children(n.Arguments...)
}

// CaseNoValueExpression is "CASE WHEN ... THEN ... [ELSE ...] END"; see
// ASTCaseNoValueExpression in googlesql/parser/parse_tree.h. Arguments holds
// alternating WHEN/THEN expression pairs, then the optional ELSE expression.
type CaseNoValueExpression struct {
	Span
	Arguments []Node `json:"arguments"`
}

func (n *CaseNoValueExpression) Children() []Node {
	return children(n.Arguments...)
}

// DotIdentifier is "expression . identifier" where the expression is not a
// plain path expression (e.g. a parenthesized expression or a CASE
// expression); see ASTDotIdentifier in googlesql/parser/parse_tree.h.
type DotIdentifier struct {
	Span
	Expr Node        `json:"expr"`
	Name *Identifier `json:"name"`
}

func (n *DotIdentifier) Children() []Node {
	return children(n.Expr, n.Name)
}

// ArrayElement is "expression [ position ]"; see ASTArrayElement in
// googlesql/parser/parse_tree.h. BracketLocation covers the "[" token.
type ArrayElement struct {
	Span
	Array           Node      `json:"array"`
	BracketLocation *Location `json:"bracket_location"`
	Position        Node      `json:"position"`
}

func (n *ArrayElement) Children() []Node {
	return children(n.Array, n.BracketLocation, n.Position)
}

// CreateTableStatement is a CREATE TABLE statement, optionally with an AS
// query; see ASTCreateTableStatement in googlesql/parser/parse_tree.h.
// Scope is "", "TEMP", "PUBLIC", or "PRIVATE" (TEMPORARY normalizes to
// "TEMP").
type CreateTableStatement struct {
	Span
	Scope         string          `json:"scope,omitempty"`
	IsOrReplace   bool            `json:"is_or_replace,omitempty"`
	IsIfNotExists bool            `json:"is_if_not_exists,omitempty"`
	Name          *PathExpression `json:"name"`
	Query         *Query          `json:"query,omitempty"`
}

func (n *CreateTableStatement) statementNode() {}
func (n *CreateTableStatement) Children() []Node {
	return children(n.Name, n.Query)
}

// CreateTableFunctionStatement is a CREATE TABLE FUNCTION statement; see
// ASTCreateTableFunctionStatement in googlesql/parser/parse_tree.h. Scope is
// "", "TEMP", "PUBLIC", or "PRIVATE" (TEMPORARY normalizes to "TEMP").
// SqlSecurity ("", "INVOKER", or "DEFINER") is parsed but, matching the
// reference, is not shown in the debug tree. Children appear in a fixed order
// regardless of the source order of the options and language clauses.
type CreateTableFunctionStatement struct {
	Span
	Scope         string               `json:"scope,omitempty"`
	IsOrReplace   bool                 `json:"is_or_replace,omitempty"`
	IsIfNotExists bool                 `json:"is_if_not_exists,omitempty"`
	SqlSecurity   string               `json:"sql_security,omitempty"`
	Declaration   *FunctionDeclaration `json:"declaration"`
	Returns       *TVFSchema           `json:"returns,omitempty"`
	Options       *OptionsList         `json:"options,omitempty"`
	Language      *Identifier          `json:"language,omitempty"`
	Query         *Query               `json:"query,omitempty"`
}

func (n *CreateTableFunctionStatement) statementNode() {}
func (n *CreateTableFunctionStatement) Children() []Node {
	return children(n.Declaration, n.Returns, n.Options, n.Language, n.Query)
}

// CreateFunctionStatement is a CREATE FUNCTION statement (scalar or aggregate
// function); see ASTCreateFunctionStatement in googlesql/parser/parse_tree.h.
// Scope is "", "TEMP", "PUBLIC", or "PRIVATE" (TEMPORARY normalizes to
// "TEMP"). Determinism is "", "DETERMINISTIC", "NOT DETERMINISTIC",
// "IMMUTABLE", "STABLE", or "VOLATILE". SqlSecurity is "", "INVOKER", or
// "DEFINER" and, unlike CreateTableFunctionStatement, IS shown in the debug
// tree. IsRemote records the REMOTE keyword, which is parsed but not shown.
// Children appear in a fixed order (declaration, return type, language,
// with-connection, body, options) regardless of source order.
type CreateFunctionStatement struct {
	Span
	Scope          string                `json:"scope,omitempty"`
	IsOrReplace    bool                  `json:"is_or_replace,omitempty"`
	IsIfNotExists  bool                  `json:"is_if_not_exists,omitempty"`
	IsAggregate    bool                  `json:"is_aggregate,omitempty"`
	Determinism    string                `json:"determinism,omitempty"`
	SqlSecurity    string                `json:"sql_security,omitempty"`
	IsRemote       bool                  `json:"is_remote,omitempty"`
	Declaration    *FunctionDeclaration  `json:"declaration"`
	ReturnType     Node                  `json:"return_type,omitempty"`
	Language       *Identifier           `json:"language,omitempty"`
	WithConnection *WithConnectionClause `json:"with_connection,omitempty"`
	Body           Node                  `json:"body,omitempty"`
	Options        *OptionsList          `json:"options,omitempty"`
}

func (n *CreateFunctionStatement) statementNode() {}
func (n *CreateFunctionStatement) Children() []Node {
	return children(n.Declaration, n.ReturnType, n.Language, n.WithConnection, n.Body, n.Options)
}

// SqlFunctionBody is the "(expression)" body of a SQL-defined function; see
// ASTSqlFunctionBody in googlesql/parser/parse_tree.h. Its span includes the
// enclosing parentheses; its single child is the body expression.
type SqlFunctionBody struct {
	Span
	Expression Node `json:"expression"`
}

func (n *SqlFunctionBody) Children() []Node {
	return children(n.Expression)
}

// TemplatedParameterType is a templated function-argument type such as
// "ANY TYPE"; see ASTTemplatedParameterType in googlesql/parser/parse_tree.h.
// It has no children.
type TemplatedParameterType struct {
	Span
	Kind string `json:"kind,omitempty"`
}

func (n *TemplatedParameterType) Children() []Node { return nil }

// WithConnectionClause is "WITH CONNECTION <connection>"; see
// ASTWithConnectionClause in googlesql/parser/parse_tree.h. Its single child
// is a ConnectionClause.
type WithConnectionClause struct {
	Span
	Connection *ConnectionClause `json:"connection"`
}

func (n *WithConnectionClause) Children() []Node {
	return children(n.Connection)
}

// FunctionDeclaration is "path(params)" in a CREATE FUNCTION-family statement;
// see ASTFunctionDeclaration in googlesql/parser/parse_tree.h.
type FunctionDeclaration struct {
	Span
	Name       *PathExpression     `json:"name"`
	Parameters *FunctionParameters `json:"parameters"`
}

func (n *FunctionDeclaration) Children() []Node {
	return children(n.Name, n.Parameters)
}

// FunctionParameters is the parenthesized parameter list of a function
// declaration; see ASTFunctionParameters in googlesql/parser/parse_tree.h.
type FunctionParameters struct {
	Span
	Parameters []*FunctionParameter `json:"parameters,omitempty"`
}

func (n *FunctionParameters) Children() []Node {
	var out []Node
	for _, p := range n.Parameters {
		out = append(out, p)
	}
	return out
}

// FunctionParameter is a single "[name] type [AS alias] [DEFAULT expr]
// [NOT AGGREGATE]" function parameter; see ASTFunctionParameter in
// googlesql/parser/parse_tree.h. DefaultValue and IsNotAggregate surface in
// the node's debug string; Alias and DefaultValue are children.
type FunctionParameter struct {
	Span
	Name           *Identifier `json:"name,omitempty"`
	Type           Node        `json:"type,omitempty"`
	Alias          *Alias      `json:"alias,omitempty"`
	DefaultValue   Node        `json:"default_value,omitempty"`
	IsNotAggregate bool        `json:"is_not_aggregate,omitempty"`
}

func (n *FunctionParameter) Children() []Node {
	return children(n.Name, n.Type, n.Alias, n.DefaultValue)
}

// TVFSchema is the "TABLE<col, ...>" return schema of a table-valued function;
// see ASTTVFSchema in googlesql/parser/parse_tree.h.
type TVFSchema struct {
	Span
	Columns []*TVFSchemaColumn `json:"columns"`
}

func (n *TVFSchema) Children() []Node {
	var out []Node
	for _, c := range n.Columns {
		out = append(out, c)
	}
	return out
}

// TVFSchemaColumn is a single "[name] type" column in a TVFSchema; see
// ASTTVFSchemaColumn in googlesql/parser/parse_tree.h.
type TVFSchemaColumn struct {
	Span
	Name *Identifier `json:"name,omitempty"`
	Type Node        `json:"type"`
}

func (n *TVFSchemaColumn) Children() []Node {
	return children(n.Name, n.Type)
}

// FunctionCall is a function call expression.
type FunctionCall struct {
	Span
	Function *PathExpression `json:"function"`
	Args     []Node          `json:"args"`
	Distinct bool            `json:"distinct,omitempty"`
	// NullHandling is "IGNORE_NULLS" or "RESPECT_NULLS" when the call has an
	// "IGNORE NULLS" or "RESPECT NULLS" modifier; empty otherwise. It is not
	// shown in the debug tree; see ASTFunctionCall::NullHandlingModifier.
	NullHandling   string                  `json:"null_handling_modifier,omitempty"`
	Having         *HavingModifier         `json:"having_modifier,omitempty"`
	ClampedBetween *ClampedBetweenModifier `json:"clamped_between_modifier,omitempty"`
	OrderBy        *OrderBy                `json:"order_by,omitempty"`
	LimitOffset    *LimitOffset            `json:"limit_offset,omitempty"`
}

func (n *FunctionCall) Children() []Node {
	out := children(n.Function)
	out = append(out, n.Args...)
	if n.Having != nil {
		out = append(out, n.Having)
	}
	if n.ClampedBetween != nil {
		out = append(out, n.ClampedBetween)
	}
	if n.OrderBy != nil {
		out = append(out, n.OrderBy)
	}
	if n.LimitOffset != nil {
		out = append(out, n.LimitOffset)
	}
	return out
}

// NamedArgument is a "name => value" argument in a function or table-valued
// function call; see ASTNamedArgument in googlesql/parser/parse_tree.h.
type NamedArgument struct {
	Span
	Name  *Identifier `json:"name"`
	Value Node        `json:"value"`
}

func (n *NamedArgument) Children() []Node {
	return children(n.Name, n.Value)
}

// Lambda is a lambda argument "param -> body" or "(a, b) -> body" passed to a
// function or table-valued function call; see ASTLambda in
// googlesql/parser/parse_tree.h. Params is a PathExpression (single argument)
// or a StructConstructorWithParens (parenthesized argument list).
type Lambda struct {
	Span
	Params Node `json:"params"`
	Body   Node `json:"body"`
}

func (n *Lambda) Children() []Node {
	return children(n.Params, n.Body)
}

// HavingModifier is the "HAVING MAX <expr>" or "HAVING MIN <expr>" modifier
// on aggregate function calls; see ASTHavingModifier in
// googlesql/parser/parse_tree.h.
type HavingModifier struct {
	Span
	Kind string `json:"kind"` // "MAX" or "MIN"
	Expr Node   `json:"expr"`
}

func (n *HavingModifier) Children() []Node {
	return children(n.Expr)
}

// AnalyticFunctionCall is a function call followed by an OVER clause; see
// ASTAnalyticFunctionCall in googlesql/parser/parse_tree.h.
type AnalyticFunctionCall struct {
	Span
	Expr       Node                 `json:"expr"` // the *FunctionCall
	WindowSpec *WindowSpecification `json:"window_spec"`
}

func (n *AnalyticFunctionCall) Children() []Node {
	return children(n.Expr, n.WindowSpec)
}

// WindowSpecification is the window after OVER: either a base window name, or
// "( [base window name] [PARTITION BY ...] [ORDER BY ...] [frame] )"; see
// ASTWindowSpecification in googlesql/parser/parse_tree.h. For the
// parenthesized form the span includes the parentheses.
type WindowSpecification struct {
	Span
	Name        *Identifier  `json:"name,omitempty"`
	PartitionBy *PartitionBy `json:"partition_by,omitempty"`
	OrderBy     *OrderBy     `json:"order_by,omitempty"`
	WindowFrame *WindowFrame `json:"window_frame,omitempty"`
}

func (n *WindowSpecification) Children() []Node {
	return children(n.Name, n.PartitionBy, n.OrderBy, n.WindowFrame)
}

// WindowClause is a "WINDOW name AS (...), ..." named window clause on a
// SELECT; see ASTWindowClause in googlesql/parser/parse_tree.h.
type WindowClause struct {
	Span
	Windows []*WindowDefinition `json:"windows"`
}

func (n *WindowClause) Children() []Node {
	var out []Node
	for _, w := range n.Windows {
		out = append(out, w)
	}
	return out
}

// WindowDefinition is a single "name AS window_specification" entry in a
// WINDOW clause; see ASTWindowDefinition in googlesql/parser/parse_tree.h.
type WindowDefinition struct {
	Span
	Name       *Identifier          `json:"name"`
	WindowSpec *WindowSpecification `json:"window_spec"`
}

func (n *WindowDefinition) Children() []Node {
	return children(n.Name, n.WindowSpec)
}

// WindowFrame is a "ROWS|RANGE ..." window frame clause; see ASTWindowFrame
// in googlesql/parser/parse_tree.h. EndExpr is only set for the
// "BETWEEN low AND high" form.
type WindowFrame struct {
	Span
	Unit      string           `json:"unit"` // "ROWS" or "RANGE"
	StartExpr *WindowFrameExpr `json:"start_expr"`
	EndExpr   *WindowFrameExpr `json:"end_expr,omitempty"`
}

func (n *WindowFrame) Children() []Node {
	return children(n.StartExpr, n.EndExpr)
}

// WindowFrameExpr is a window frame boundary; see ASTWindowFrameExpr in
// googlesql/parser/parse_tree.h. Expression is only set for the
// "expression PRECEDING/FOLLOWING" (OFFSET) forms.
type WindowFrameExpr struct {
	Span
	// BoundaryType is one of "UNBOUNDED PRECEDING", "OFFSET PRECEDING",
	// "CURRENT ROW", "OFFSET FOLLOWING" or "UNBOUNDED FOLLOWING".
	BoundaryType string `json:"boundary_type"`
	Expression   Node   `json:"expression,omitempty"`
}

func (n *WindowFrameExpr) Children() []Node {
	return children(n.Expression)
}

// PartitionBy is a "PARTITION [hint] BY expr, ..." clause; see ASTPartitionBy
// in googlesql/parser/parse_tree.h.
type PartitionBy struct {
	Span
	Hint        *Hint  `json:"hint,omitempty"`
	Expressions []Node `json:"expressions"`
}

func (n *PartitionBy) Children() []Node {
	out := children(n.Hint)
	return append(out, n.Expressions...)
}

// SetOperation is a chain of query primaries combined with the same set
// operation (UNION/INTERSECT/EXCEPT), e.g. "q1 UNION ALL q2 UNION ALL q3";
// see ASTSetOperation in googlesql/parser/parse_tree.h. Metadata holds one
// entry per operator; Inputs holds the operand queries in order.
type SetOperation struct {
	Span
	Metadata *SetOperationMetadataList `json:"metadata"`
	Inputs   []Node                    `json:"inputs"`
}

func (n *SetOperation) Children() []Node {
	out := children(n.Metadata)
	return append(out, n.Inputs...)
}

// SetOperationMetadataList holds the metadata of each set operator in a
// SetOperation; see ASTSetOperationMetadataList in
// googlesql/parser/parse_tree.h.
type SetOperationMetadataList struct {
	Span
	Entries []*SetOperationMetadata `json:"entries"`
}

func (n *SetOperationMetadataList) Children() []Node {
	var out []Node
	for _, e := range n.Entries {
		out = append(out, e)
	}
	return out
}

// SetOperationMetadata describes one set operator: its type, ALL/DISTINCT
// modifier, optional hint, and optional column match/propagation modes; see
// ASTSetOperationMetadata in googlesql/parser/parse_tree.h. Children keep the
// constructor order of the reference (not source order): type, ALL/DISTINCT,
// hint, column match mode, column propagation mode, column list.
type SetOperationMetadata struct {
	Span
	OpType                *SetOperationType                  `json:"op_type"`
	AllOrDistinct         *SetOperationAllOrDistinct         `json:"all_or_distinct"`
	Hint                  *Hint                              `json:"hint,omitempty"`
	ColumnMatchMode       *SetOperationColumnMatchMode       `json:"column_match_mode,omitempty"`
	ColumnPropagationMode *SetOperationColumnPropagationMode `json:"column_propagation_mode,omitempty"`
	ColumnList            *ColumnList                        `json:"column_list,omitempty"`
}

func (n *SetOperationMetadata) Children() []Node {
	return children(n.OpType, n.AllOrDistinct, n.Hint, n.ColumnMatchMode,
		n.ColumnPropagationMode, n.ColumnList)
}

// SetOperationType is the operator keyword of a set operation; Op is "UNION",
// "INTERSECT", or "EXCEPT". See ASTSetOperationType in
// googlesql/parser/parse_tree.h.
type SetOperationType struct {
	Span
	Op string `json:"op"`
}

func (n *SetOperationType) Children() []Node { return nil }

// SetOperationAllOrDistinct is the ALL or DISTINCT modifier of a set
// operation; Value is "ALL" or "DISTINCT". See ASTSetOperationAllOrDistinct
// in googlesql/parser/parse_tree.h.
type SetOperationAllOrDistinct struct {
	Span
	Value string `json:"value"`
}

func (n *SetOperationAllOrDistinct) Children() []Node { return nil }

// SetOperationColumnMatchMode is a CORRESPONDING / CORRESPONDING BY /
// BY NAME / BY NAME ON modifier on a set operation; Value is
// "CORRESPONDING", "CORRESPONDING_BY", "BY_NAME", or "BY_NAME_ON". See
// ASTSetOperationColumnMatchMode in googlesql/parser/parse_tree.h.
type SetOperationColumnMatchMode struct {
	Span
	Value string `json:"value"`
}

func (n *SetOperationColumnMatchMode) Children() []Node { return nil }

// SetOperationColumnPropagationMode is a FULL/LEFT/INNER outer mode prefix or
// a STRICT modifier on a set operation; Value is "FULL", "LEFT", "INNER", or
// "STRICT". See ASTSetOperationColumnPropagationMode in
// googlesql/parser/parse_tree.h.
type SetOperationColumnPropagationMode struct {
	Span
	Value string `json:"value"`
}

func (n *SetOperationColumnPropagationMode) Children() []Node { return nil }

// ColumnList is a parenthesized list of column name identifiers, e.g. "(a,
// b)"; see ASTColumnList in googlesql/parser/parse_tree.h. The span includes
// the parentheses.
type ColumnList struct {
	Span
	Identifiers []*Identifier `json:"identifiers"`
}

func (n *ColumnList) Children() []Node {
	var out []Node
	for _, id := range n.Identifiers {
		out = append(out, id)
	}
	return out
}

// TableClause is a "TABLE path" clause used as a query; see ASTTableClause
// in googlesql/parser/parse_tree.h.
type TableClause struct {
	Span
	Path *PathExpression `json:"path"`
}

func (n *TableClause) Children() []Node {
	return children(n.Path)
}

// ModelClause is a "MODEL path" table-valued function argument; see
// ASTModelClause in googlesql/parser/parse_tree.h.
type ModelClause struct {
	Span
	Path *PathExpression `json:"path"`
}

func (n *ModelClause) Children() []Node {
	return children(n.Path)
}

// ConnectionClause is a "CONNECTION {path | DEFAULT}" table-valued function
// argument; see ASTConnectionClause in googlesql/parser/parse_tree.h. Path
// is either a *PathExpression or a *DefaultLiteral.
type ConnectionClause struct {
	Span
	Path Node `json:"path"`
}

func (n *ConnectionClause) Children() []Node {
	return children(n.Path)
}

// DefaultLiteral is the DEFAULT keyword used in place of a path expression;
// see ASTDefaultLiteral in googlesql/parser/parse_tree.h.
type DefaultLiteral struct {
	Span
}

func (n *DefaultLiteral) Children() []Node { return nil }

// ParameterExpr is a named query parameter "@name" or a positional query
// parameter "?"; see ASTParameterExpr in googlesql/parser/parse_tree.h. For
// positional parameters Name is nil and Position is the 1-based ordinal of
// the "?" in the statement.
type ParameterExpr struct {
	Span
	Name     *Identifier `json:"name,omitempty"`
	Position int         `json:"position,omitempty"`
}

func (n *ParameterExpr) Children() []Node {
	return children(n.Name)
}

// SystemVariableExpr is a system variable reference "@@path"; see
// ASTSystemVariableExpr in googlesql/parser/parse_tree.h.
type SystemVariableExpr struct {
	Span
	Path *PathExpression `json:"path"`
}

func (n *SystemVariableExpr) Children() []Node {
	return children(n.Path)
}

// PipeSetOperation is a "|> UNION ALL (query), ..." pipe operator; see
// ASTPipeSetOperation in googlesql/parser/parse_tree.h.
type PipeSetOperation struct {
	Span
	Metadata *SetOperationMetadata `json:"metadata"`
	Inputs   []Node                `json:"inputs"`
}

func (n *PipeSetOperation) Children() []Node {
	out := children(n.Metadata)
	return append(out, n.Inputs...)
}

// CastExpression is "CAST(expr AS type [FORMAT ...])" or "SAFE_CAST(...)";
// see ASTCastExpression in googlesql/parser/parse_tree.h.
type CastExpression struct {
	Span
	Expr       Node          `json:"expr"`
	Type       Node          `json:"type"`
	Format     *FormatClause `json:"format,omitempty"`
	IsSafeCast bool          `json:"is_safe_cast,omitempty"`
}

func (n *CastExpression) Children() []Node {
	return children(n.Expr, n.Type, n.Format)
}

// FormatClause is the "FORMAT expr [AT TIME ZONE expr]" clause of a cast;
// see ASTFormatClause in googlesql/parser/parse_tree.h.
type FormatClause struct {
	Span
	Format   Node `json:"format"`
	TimeZone Node `json:"time_zone,omitempty"`
}

func (n *FormatClause) Children() []Node {
	return children(n.Format, n.TimeZone)
}

// SimpleType is a named type like "int64" or "a.b.c"; see ASTSimpleType in
// googlesql/parser/parse_tree.h.
type SimpleType struct {
	Span
	Name           *PathExpression    `json:"name"`
	TypeParameters *TypeParameterList `json:"type_parameters,omitempty"`
	Collate        *Collate           `json:"collate,omitempty"`
}

func (n *SimpleType) Children() []Node {
	return children(n.Name, n.TypeParameters, n.Collate)
}

// ArrayType is "ARRAY<type>"; see ASTArrayType in
// googlesql/parser/parse_tree.h.
type ArrayType struct {
	Span
	ElementType    Node               `json:"element_type"`
	TypeParameters *TypeParameterList `json:"type_parameters,omitempty"`
	Collate        *Collate           `json:"collate,omitempty"`
}

func (n *ArrayType) Children() []Node {
	return children(n.ElementType, n.TypeParameters, n.Collate)
}

// StructType is "STRUCT<field, ...>"; see ASTStructType in
// googlesql/parser/parse_tree.h.
type StructType struct {
	Span
	Fields         []*StructField     `json:"fields"`
	TypeParameters *TypeParameterList `json:"type_parameters,omitempty"`
	Collate        *Collate           `json:"collate,omitempty"`
}

func (n *StructType) Children() []Node {
	var out []Node
	for _, f := range n.Fields {
		out = append(out, f)
	}
	return append(out, children(n.TypeParameters, n.Collate)...)
}

// StructField is one "[name] type" entry in a STRUCT type; see
// ASTStructField in googlesql/parser/parse_tree.h.
type StructField struct {
	Span
	Name *Identifier `json:"name,omitempty"`
	Type Node        `json:"type"`
}

func (n *StructField) Children() []Node {
	return children(n.Name, n.Type)
}

// RangeType is "RANGE<type>"; see ASTRangeType in
// googlesql/parser/parse_tree.h.
type RangeType struct {
	Span
	ElementType    Node               `json:"element_type"`
	TypeParameters *TypeParameterList `json:"type_parameters,omitempty"`
	Collate        *Collate           `json:"collate,omitempty"`
}

func (n *RangeType) Children() []Node {
	return children(n.ElementType, n.TypeParameters, n.Collate)
}

// FunctionType is "FUNCTION<argtypes -> returntype>"; see ASTFunctionType in
// googlesql/parser/parse_tree.h. Children are the argument list, the return
// type, and then any trailing type parameters and collation.
type FunctionType struct {
	Span
	ArgList        *FunctionTypeArgList `json:"arg_list"`
	ReturnType     Node                 `json:"return_type"`
	TypeParameters *TypeParameterList   `json:"type_parameters,omitempty"`
	Collate        *Collate             `json:"collate,omitempty"`
}

func (n *FunctionType) Children() []Node {
	return children(n.ArgList, n.ReturnType, n.TypeParameters, n.Collate)
}

// FunctionTypeArgList is the argument-type list of a FunctionType; see
// ASTFunctionTypeArgList in googlesql/parser/parse_tree.h.
type FunctionTypeArgList struct {
	Span
	Args []Node `json:"args,omitempty"`
}

func (n *FunctionTypeArgList) Children() []Node {
	return append([]Node(nil), n.Args...)
}

// TypeParameterList is the "(param, ...)" suffix of a parameterized type
// like STRING(10); see ASTTypeParameterList in
// googlesql/parser/parse_tree.h. Its span covers "(" through the last
// parameter, excluding the closing ")", matching the reference grammar.
type TypeParameterList struct {
	Span
	Parameters []Node `json:"parameters"`
}

func (n *TypeParameterList) Children() []Node {
	return append([]Node(nil), n.Parameters...)
}

// MaxLiteral is the special "MAX" type parameter, e.g. NUMERIC(MAX); see
// ASTMaxLiteral in googlesql/parser/parse_tree.h.
type MaxLiteral struct {
	Span
}

func (n *MaxLiteral) Children() []Node { return nil }

// MatchRecognizeClause is a "MATCH_RECOGNIZE(...)" postfix table operator;
// see ASTMatchRecognizeClause in googlesql/parser/parse_tree.h. Child order
// matches the reference node: options, partition by, order by, measures,
// after match skip, pattern, definitions, alias.
type MatchRecognizeClause struct {
	Span
	Options        *OptionsList          `json:"options,omitempty"`
	PartitionBy    *PartitionBy          `json:"partition_by,omitempty"`
	OrderBy        *OrderBy              `json:"order_by"`
	Measures       *SelectList           `json:"measures"`
	AfterMatchSkip *AfterMatchSkipClause `json:"after_match_skip,omitempty"`
	Pattern        Node                  `json:"pattern"`
	Definitions    *SelectList           `json:"definitions"`
	Alias          *Alias                `json:"alias,omitempty"`
}

func (n *MatchRecognizeClause) Children() []Node {
	return children(n.Options, n.PartitionBy, n.OrderBy, n.Measures,
		n.AfterMatchSkip, n.Pattern, n.Definitions, n.Alias)
}

// AfterMatchSkipClause is the "AFTER MATCH SKIP ..." clause inside
// MATCH_RECOGNIZE; see ASTAfterMatchSkipClause in
// googlesql/parser/parse_tree.h. TargetType is "PAST_LAST_ROW" or
// "TO_NEXT_ROW". Its span covers only the skip target (e.g. "TO NEXT ROW").
type AfterMatchSkipClause struct {
	Span
	TargetType string `json:"target_type"`
}

func (n *AfterMatchSkipClause) Children() []Node { return nil }

// PipeMatchRecognize is a |> MATCH_RECOGNIZE pipe operator; see
// ASTPipeMatchRecognize in googlesql/parser/parse_tree.h.
type PipeMatchRecognize struct {
	Span
	Clause *MatchRecognizeClause `json:"clause"`
}

func (n *PipeMatchRecognize) Children() []Node {
	return children(n.Clause)
}

// RowPatternOperation is an alternation or concatenation of row pattern
// expressions inside PATTERN(...); see ASTRowPatternOperation in
// googlesql/parser/parse_tree.h. OpType is "ALTERNATE" or "CONCAT".
type RowPatternOperation struct {
	Span
	OpType        string `json:"op_type"`
	Parenthesized bool   `json:"parenthesized,omitempty"`
	Operands      []Node `json:"operands"`
}

func (n *RowPatternOperation) Children() []Node {
	return append([]Node(nil), n.Operands...)
}

// EmptyRowPattern is an empty row pattern, e.g. in "PATTERN ()" or between
// alternation bars; see ASTEmptyRowPattern in googlesql/parser/parse_tree.h.
type EmptyRowPattern struct {
	Span
	Parenthesized bool `json:"parenthesized,omitempty"`
}

func (n *EmptyRowPattern) Children() []Node { return nil }

// RowPatternVariable is a pattern variable reference inside PATTERN(...);
// see ASTRowPatternVariable in googlesql/parser/parse_tree.h.
type RowPatternVariable struct {
	Span
	Name *Identifier `json:"name"`
}

func (n *RowPatternVariable) Children() []Node {
	return children(n.Name)
}

// RowPatternAnchor is a "^" (START) or "$" (END) anchor inside
// PATTERN(...); see ASTRowPatternAnchor in googlesql/parser/parse_tree.h.
type RowPatternAnchor struct {
	Span
	Anchor string `json:"anchor"`
}

func (n *RowPatternAnchor) Children() []Node { return nil }

// RowPatternQuantification is a quantified row pattern primary, e.g. "A+"
// or "(A B){1,2}"; see ASTRowPatternQuantification in
// googlesql/parser/parse_tree.h.
type RowPatternQuantification struct {
	Span
	Primary    Node `json:"primary"`
	Quantifier Node `json:"quantifier"`
}

func (n *RowPatternQuantification) Children() []Node {
	return children(n.Primary, n.Quantifier)
}

// SymbolQuantifier is a "?", "+", or "*" quantifier; see ASTSymbolQuantifier
// in googlesql/parser/parse_tree.h. Symbol is "QUESTION_MARK", "PLUS", or
// "STAR".
type SymbolQuantifier struct {
	Span
	Symbol      string `json:"symbol"`
	IsReluctant bool   `json:"is_reluctant,omitempty"`
}

func (n *SymbolQuantifier) Children() []Node { return nil }

// FixedQuantifier is a "{n}" quantifier; see ASTFixedQuantifier in
// googlesql/parser/parse_tree.h.
type FixedQuantifier struct {
	Span
	Bound Node `json:"bound"`
}

func (n *FixedQuantifier) Children() []Node {
	return children(n.Bound)
}

// QuantifierBound is one bound of a bounded quantifier; its span covers the
// whole "{lo,hi}" range and its child is the bound expression, if any. See
// ASTQuantifierBound in googlesql/parser/parse_tree.h.
type QuantifierBound struct {
	Span
	Bound Node `json:"bound,omitempty"`
}

func (n *QuantifierBound) Children() []Node {
	return children(n.Bound)
}

// BoundedQuantifier is a "{lo,hi}" quantifier where either bound may be
// omitted; see ASTBoundedQuantifier in googlesql/parser/parse_tree.h.
type BoundedQuantifier struct {
	Span
	LowerBound  *QuantifierBound `json:"lower_bound"`
	UpperBound  *QuantifierBound `json:"upper_bound"`
	IsReluctant bool             `json:"is_reluctant,omitempty"`
}

func (n *BoundedQuantifier) Children() []Node {
	return children(n.LowerBound, n.UpperBound)
}
