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
	Distinct   bool         `json:"distinct,omitempty"`
	SelectList *SelectList  `json:"select_list"`
	From       *FromClause  `json:"from,omitempty"`
	Where      *WhereClause `json:"where,omitempty"`
	GroupBy    *GroupBy     `json:"group_by,omitempty"`
	Having     *Having      `json:"having,omitempty"`
}

func (n *Select) Children() []Node {
	return children(n.SelectList, n.From, n.Where, n.GroupBy, n.Having)
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
	Alias      *Alias            `json:"alias,omitempty"`
	Offset     *WithOffset       `json:"offset,omitempty"`
}

func (n *TablePathExpression) Children() []Node {
	return children(n.Path, n.UnnestExpr, n.Alias, n.Offset)
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
}

func (n *TableSubquery) Children() []Node {
	return children(n.Query, n.Alias)
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
}

func (n *TVF) Children() []Node {
	out := children(n.Name)
	for _, a := range n.Args {
		out = append(out, a)
	}
	return append(out, children(n.Alias)...)
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
}

func (n *UnnestExpression) Children() []Node {
	var out []Node
	for _, e := range n.Expressions {
		out = append(out, e)
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
}

func (n *ParenthesizedJoin) Children() []Node {
	return children(n.Join)
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
	Elements []Node `json:"elements"`
}

func (n *ArrayConstructor) Children() []Node {
	return append([]Node(nil), n.Elements...)
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
	Items []*GroupingItem `json:"items"`
}

func (n *GroupBy) Children() []Node {
	var out []Node
	for _, item := range n.Items {
		out = append(out, item)
	}
	return out
}

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
	Query    *Query `json:"query"`
}

func (n *ExpressionSubquery) Children() []Node {
	return children(n.Query)
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

// FunctionCall is a function call expression.
type FunctionCall struct {
	Span
	Function *PathExpression `json:"function"`
	Args     []Node          `json:"args"`
	Distinct bool            `json:"distinct,omitempty"`
}

func (n *FunctionCall) Children() []Node {
	out := children(n.Function)
	out = append(out, n.Args...)
	return out
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
}

func (n *WindowSpecification) Children() []Node {
	return children(n.Name, n.PartitionBy, n.OrderBy)
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
