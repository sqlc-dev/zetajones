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

// Query is a query expression with optional ORDER BY and LIMIT/OFFSET,
// optionally followed by |> pipe operators.
type Query struct {
	Span
	QueryExpr     Node         `json:"query_expr"` // *Select, *SetOperation, or parenthesized *Query
	OrderBy       *OrderBy     `json:"order_by,omitempty"`
	Limit         *LimitOffset `json:"limit,omitempty"`
	PipeOperators []Node       `json:"pipe_operators,omitempty"`
}

func (n *Query) Children() []Node {
	out := children(n.QueryExpr, n.OrderBy, n.Limit)
	return append(out, n.PipeOperators...)
}

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

// TablePathExpression is a table reference by (possibly dotted) path.
type TablePathExpression struct {
	Span
	Path  *PathExpression `json:"path"`
	Alias *Alias          `json:"alias,omitempty"`
}

func (n *TablePathExpression) Children() []Node {
	return children(n.Path, n.Alias)
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
	Items []*OrderingExpression `json:"items"`
}

func (n *OrderBy) Children() []Node {
	var out []Node
	for _, item := range n.Items {
		out = append(out, item)
	}
	return out
}

// OrderingExpression is a single ORDER BY item.
type OrderingExpression struct {
	Span
	Expr       Node `json:"expr"`
	Descending bool `json:"descending,omitempty"`
	HasAsc     bool `json:"has_asc,omitempty"` // explicit ASC keyword present
}

func (n *OrderingExpression) Children() []Node {
	return children(n.Expr)
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
