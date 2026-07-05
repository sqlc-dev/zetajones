package parser

// GoogleSQL graph query (GQL / GRAPH_TABLE) parsing.
//
// This file implements the foundational graph query nodes: the GRAPH
// statement (gql_statement / gql_query), the GRAPH_TABLE(...) table expression
// (graph_table_query), the linear GQL operator list (MATCH / OPTIONAL MATCH /
// LET / FILTER / RETURN), and the core graph pattern grammar (graph_pattern,
// graph_path_pattern, graph_node_pattern, graph_edge_pattern,
// graph_element_pattern_filler, and label expressions). It mirrors the graph_*
// productions in googlesql/parser/googlesql.tm and the ASTGql*/ASTGraph*
// node debug strings in googlesql/parser/parse_tree.cc.
//
// More advanced graph sub-features (quantified paths, parenthesized path
// patterns, path search prefixes / modes, path variables, WITH / FOR / SAMPLE
// / CALL operators, order-by/page, aggregation, graph set operations, graph
// subqueries) are intentionally left for later work.

import (
	"github.com/sqlc-dev/zetajones/ast"
	"github.com/sqlc-dev/zetajones/token"
)

// parseGraphStatement parses "GRAPH <name> <operation_block>" as a query
// statement; see gql_statement / gql_query in googlesql.tm. The result is a
// QueryStatement wrapping Query > GqlQuery > GraphTableQuery.
func (p *parser) parseGraphStatement() (ast.Statement, error) {
	graphTok := p.advance() // GRAPH
	graph, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	ops, err := p.parseGraphOperationBlock(nil)
	if err != nil {
		return nil, err
	}
	end := ops.End()
	gt := &ast.GraphTableQuery{Span: span(graphTok.Pos, end), Graph: graph, Op: ops}
	gq := &ast.GqlQuery{Span: span(graphTok.Pos, end), Query: gt}
	q := &ast.Query{Span: span(graphTok.Pos, end), QueryExpr: gq}
	return &ast.QueryStatement{Span: span(graphTok.Pos, end), Query: q}, nil
}

// parseGraphTableQuery parses "GRAPH_TABLE( <name> <match> [COLUMNS(...)] )" or
// "GRAPH_TABLE( <name> <operation_block> )" with an optional alias; see
// graph_table_query in googlesql.tm.
func (p *parser) parseGraphTableQuery() (ast.Node, error) {
	gtTok := p.advance() // GRAPH_TABLE
	p.advance()          // (
	graph, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}

	var op ast.Node
	var shape *ast.SelectList
	isBlock := false
	if isKeyword(p.peek(), "MATCH") {
		// Could be the single-match COLUMNS form or the first operator of a
		// linear operation block.
		match, err := p.parseGqlMatch()
		if err != nil {
			return nil, err
		}
		switch {
		case isKeyword(p.peek(), "COLUMNS"):
			shape, err = p.parseGraphShapeClause()
			if err != nil {
				return nil, err
			}
			op = match
		case p.peek().Kind == token.RPAREN:
			op = match
		default:
			ops, err := p.parseGraphOperationBlock(match)
			if err != nil {
				return nil, err
			}
			op = ops
			isBlock = true
		}
	} else {
		ops, err := p.parseGraphOperationBlock(nil)
		if err != nil {
			return nil, err
		}
		op = ops
		isBlock = true
	}

	if p.peek().Kind != token.RPAREN {
		if isBlock {
			return nil, p.errorf(p.peek().Pos, `Syntax error: Expected ")" or keyword NEXT but got %s`, describeToken(p.peek()))
		}
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected ")" but got %s`, describeToken(p.peek()))
	}
	rparen := p.advance() // )

	gt := &ast.GraphTableQuery{Span: span(gtTok.Pos, rparen.End), Graph: graph, Op: op, Shape: shape}
	if !p.atPivotOrUnpivotClauseStart() {
		alias, err := p.parseOptionalAlias()
		if err != nil {
			return nil, err
		}
		if alias != nil {
			gt.Alias = alias
			gt.Stop = alias.End()
		}
	}
	return gt, nil
}

// parseGraphShapeClause parses "COLUMNS ( select_list )"; see
// graph_shape_clause in googlesql.tm. It returns the select list node with its
// own location (excluding the COLUMNS keyword and parentheses).
func (p *parser) parseGraphShapeClause() (*ast.SelectList, error) {
	p.advance() // COLUMNS
	if p.peek().Kind != token.LPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" but got %s`, describeToken(p.peek()))
	}
	p.advance() // (
	list, err := p.parseSelectList()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != token.RPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected ")" but got %s`, describeToken(p.peek()))
	}
	p.advance() // )
	return list, nil
}

// parseGraphOperationBlock parses one or more NEXT-separated linear query
// operations, wrapping them in a top-level GqlOperatorList; see
// graph_operation_block in googlesql.tm. firstOp, if non-nil, is a linear
// operator already consumed for the first block (used when disambiguating the
// GRAPH_TABLE single-match / operation-block forms).
func (p *parser) parseGraphOperationBlock(firstOp ast.Node) (*ast.GqlOperatorList, error) {
	block, err := p.parseGraphLinearQueryOperation(firstOp)
	if err != nil {
		return nil, err
	}
	blocks := []ast.Node{block}
	for isKeyword(p.peek(), "NEXT") {
		p.advance() // NEXT
		block, err := p.parseGraphLinearQueryOperation(nil)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	return &ast.GqlOperatorList{Span: span(blocks[0].Pos(), blocks[len(blocks)-1].End()), Operators: blocks}, nil
}

// parseGraphLinearQueryOperation parses a sequence of linear operators
// terminated by a mandatory RETURN, wrapping them in a GqlOperatorList; see
// graph_linear_query_operation in googlesql.tm.
func (p *parser) parseGraphLinearQueryOperation(firstOp ast.Node) (*ast.GqlOperatorList, error) {
	var ops []ast.Node
	start := -1
	if firstOp != nil {
		ops = append(ops, firstOp)
		start = firstOp.Pos()
	}
	for !isKeyword(p.peek(), "RETURN") {
		if !p.startsGraphLinearOp() {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword RETURN but got %s", describeToken(p.peek()))
		}
		op, err := p.parseGraphLinearOp()
		if err != nil {
			return nil, err
		}
		if start < 0 {
			start = op.Pos()
		}
		ops = append(ops, op)
	}
	ret, err := p.parseGqlReturn()
	if err != nil {
		return nil, err
	}
	if start < 0 {
		start = ret.Pos()
	}
	ops = append(ops, ret)
	return &ast.GqlOperatorList{Span: span(start, ret.End()), Operators: ops}, nil
}

// startsGraphLinearOp reports whether the next token begins a linear GQL
// operator that this parser implements; see graph_linear_op in googlesql.tm.
func (p *parser) startsGraphLinearOp() bool {
	switch {
	case isKeyword(p.peek(), "MATCH"):
		return true
	case isKeyword(p.peek(), "OPTIONAL") && isKeyword(p.peekAt(1), "MATCH"):
		return true
	case isKeyword(p.peek(), "LET"):
		return true
	case isKeyword(p.peek(), "FILTER"):
		return true
	}
	return false
}

func (p *parser) parseGraphLinearOp() (ast.Node, error) {
	switch {
	case isKeyword(p.peek(), "OPTIONAL"):
		return p.parseGqlOptionalMatch()
	case isKeyword(p.peek(), "MATCH"):
		return p.parseGqlMatch()
	case isKeyword(p.peek(), "LET"):
		return p.parseGqlLet()
	case isKeyword(p.peek(), "FILTER"):
		return p.parseGqlFilter()
	}
	return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
}

// parseGqlMatch parses "MATCH <graph_pattern>"; see graph_match_operator in
// googlesql.tm.
func (p *parser) parseGqlMatch() (*ast.GqlMatch, error) {
	matchTok := p.advance() // MATCH
	pattern, err := p.parseGraphPattern()
	if err != nil {
		return nil, err
	}
	return &ast.GqlMatch{Span: span(matchTok.Pos, pattern.End()), Pattern: pattern}, nil
}

// parseGqlOptionalMatch parses "OPTIONAL MATCH <graph_pattern>"; see
// graph_optional_match_operator in googlesql.tm.
func (p *parser) parseGqlOptionalMatch() (*ast.GqlMatch, error) {
	optTok := p.advance() // OPTIONAL
	p.advance()           // MATCH
	pattern, err := p.parseGraphPattern()
	if err != nil {
		return nil, err
	}
	return &ast.GqlMatch{Span: span(optTok.Pos, pattern.End()), Pattern: pattern, Optional: true}, nil
}

// parseGqlLet parses "LET <definition_list>"; see graph_let_operator in
// googlesql.tm.
func (p *parser) parseGqlLet() (*ast.GqlLet, error) {
	letTok := p.advance() // LET
	def, err := p.parseGqlLetVariableDefinition()
	if err != nil {
		return nil, err
	}
	defs := []*ast.GqlLetVariableDefinition{def}
	for p.peek().Kind == token.COMMA {
		p.advance() // ,
		def, err := p.parseGqlLetVariableDefinition()
		if err != nil {
			return nil, err
		}
		defs = append(defs, def)
	}
	list := &ast.GqlLetVariableDefinitionList{
		Span:        span(defs[0].Pos(), defs[len(defs)-1].End()),
		Definitions: defs,
	}
	return &ast.GqlLet{Span: span(letTok.Pos, list.End()), Definitions: list}, nil
}

func (p *parser) parseGqlLetVariableDefinition() (*ast.GqlLetVariableDefinition, error) {
	name, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != token.EQ {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "=" but got %s`, describeToken(p.peek()))
	}
	p.advance() // =
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	return &ast.GqlLetVariableDefinition{Span: span(name.Pos(), p.extEnd(expr)), Name: name, Expr: expr}, nil
}

// parseGqlFilter parses "FILTER <where_clause>" or "FILTER <expression>"; see
// graph_filter_operator in googlesql.tm. In the expression form a WhereClause
// node is synthesized spanning the whole FILTER clause.
func (p *parser) parseGqlFilter() (*ast.GqlFilter, error) {
	filterTok := p.advance() // FILTER
	if isKeyword(p.peek(), "WHERE") {
		where, err := p.parseWhereClause()
		if err != nil {
			return nil, err
		}
		return &ast.GqlFilter{Span: span(filterTok.Pos, where.End()), Where: where}, nil
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	where := &ast.WhereClause{Span: span(filterTok.Pos, p.extEnd(expr)), Expr: expr}
	return &ast.GqlFilter{Span: span(filterTok.Pos, where.End()), Where: where}, nil
}

// parseGqlReturn parses "RETURN <return_item_list>"; see
// graph_return_operator in googlesql.tm. It builds a Select holding the item
// list. Advanced clauses (DISTINCT, GROUP BY, ORDER BY, OFFSET, LIMIT) are not
// yet supported.
func (p *parser) parseGqlReturn() (*ast.GqlReturn, error) {
	returnTok := p.advance() // RETURN
	first, err := p.parseGqlReturnItem()
	if err != nil {
		return nil, err
	}
	cols := []*ast.SelectColumn{first}
	for p.peek().Kind == token.COMMA {
		p.advance() // ,
		col, err := p.parseGqlReturnItem()
		if err != nil {
			return nil, err
		}
		cols = append(cols, col)
	}
	list := &ast.SelectList{Span: span(cols[0].Pos(), cols[len(cols)-1].End()), Columns: cols}
	sel := &ast.Select{Span: span(list.Pos(), list.End()), SelectList: list}
	return &ast.GqlReturn{Span: span(returnTok.Pos, list.End()), Select: sel}, nil
}

// parseGqlReturnItem parses a single return item: "*", "expression", or
// "expression AS identifier"; see graph_return_item in googlesql.tm. Unlike a
// general select column, an implicit (AS-less) alias is not allowed.
func (p *parser) parseGqlReturnItem() (*ast.SelectColumn, error) {
	if p.peek().Kind == token.STAR {
		star := p.advance()
		expr := &ast.Star{Span: span(star.Pos, star.End), Image: star.Image}
		return &ast.SelectColumn{Span: span(star.Pos, star.End), Expr: expr}, nil
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	col := &ast.SelectColumn{Span: span(p.extStart(expr), p.extEnd(expr)), Expr: expr}
	if isKeyword(p.peek(), "AS") {
		asTok := p.advance() // AS
		ident, err := p.parseIdentifier()
		if err != nil {
			return nil, err
		}
		col.Alias = &ast.Alias{Span: span(asTok.Pos, ident.End()), Identifier: ident}
		col.Stop = ident.End()
	}
	return col, nil
}

// parseGraphPattern parses a comma-separated list of path patterns with an
// optional trailing WHERE clause; see graph_pattern / graph_path_pattern_list
// in googlesql.tm.
func (p *parser) parseGraphPattern() (*ast.GraphPattern, error) {
	first, err := p.parseGraphPathPattern()
	if err != nil {
		return nil, err
	}
	paths := []*ast.GraphPathPattern{first}
	for p.peek().Kind == token.COMMA {
		p.advance() // ,
		next, err := p.parseGraphPathPattern()
		if err != nil {
			return nil, err
		}
		paths = append(paths, next)
	}
	gp := &ast.GraphPattern{Paths: paths}
	end := paths[len(paths)-1].End()
	if isKeyword(p.peek(), "WHERE") {
		where, err := p.parseWhereClause()
		if err != nil {
			return nil, err
		}
		gp.Where = where
		end = where.End()
	}
	gp.Span = span(paths[0].Pos(), end)
	return gp, nil
}

// parseGraphPathPattern parses a sequence of node/edge path factors; see
// graph_path_pattern / graph_path_pattern_expr in googlesql.tm. A leading
// "identifier =" path-variable assignment prefix is recognized only to
// reproduce the reference's error behavior; the assignment itself is not yet
// modeled.
func (p *parser) parseGraphPathPattern() (*ast.GraphPathPattern, error) {
	// A path pattern may begin with a "graph_identifier =" path variable
	// assignment. Any leading bare identifier here must be such a prefix,
	// since a path factor always starts with "(", "-", "<", or "->".
	if t := p.peek(); (t.Kind == token.IDENT && !isReserved(t)) || t.Kind == token.QUOTED_IDENT {
		p.advance() // path variable name
		if p.peek().Kind != token.EQ {
			return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "=" but got %s`, describeToken(p.peek()))
		}
		p.advance() // =
	}

	var factors []ast.Node
	for p.startsGraphPathFactor() {
		f, err := p.parseGraphPathFactor()
		if err != nil {
			return nil, err
		}
		factors = append(factors, f)
	}
	if len(factors) == 0 {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
	}
	return &ast.GraphPathPattern{
		Span:    span(factors[0].Pos(), factors[len(factors)-1].End()),
		Factors: factors,
	}, nil
}

// startsGraphPathFactor reports whether the next token begins a graph path
// factor (a node pattern "(" or an edge pattern "-", "<", or "->").
func (p *parser) startsGraphPathFactor() bool {
	switch p.peek().Kind {
	case token.LPAREN, token.MINUS, token.LT, token.ARROW:
		return true
	}
	return false
}

func (p *parser) parseGraphPathFactor() (ast.Node, error) {
	if p.peek().Kind == token.LPAREN {
		return p.parseGraphNodePattern()
	}
	return p.parseGraphEdgePattern()
}

// parseGraphNodePattern parses "( <filler> )"; see graph_node_pattern in
// googlesql.tm.
func (p *parser) parseGraphNodePattern() (*ast.GraphNodePattern, error) {
	lparen := p.advance() // (
	filler, err := p.parseGraphElementPatternFiller()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != token.RPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected ")" but got %s`, describeToken(p.peek()))
	}
	rparen := p.advance() // )
	return &ast.GraphNodePattern{Span: span(lparen.Pos, rparen.End), Filler: filler}, nil
}

// parseGraphEdgePattern parses an edge pattern in any of its full or
// abbreviated forms; see graph_edge_pattern in googlesql.tm.
func (p *parser) parseGraphEdgePattern() (*ast.GraphEdgePattern, error) {
	start := p.peek()
	switch start.Kind {
	case token.ARROW: // ->
		p.advance()
		return &ast.GraphEdgePattern{Span: span(start.Pos, start.End), Orientation: "RIGHT"}, nil
	case token.LT: // <- or <-[...]-
		p.advance() // <
		if p.peek().Kind != token.MINUS {
			return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "-" but got %s`, describeToken(p.peek()))
		}
		minus := p.advance() // -
		if p.peek().Kind == token.LBRACKET {
			filler, endTok, err := p.parseGraphBracketedFiller(token.MINUS)
			if err != nil {
				return nil, err
			}
			return &ast.GraphEdgePattern{Span: span(start.Pos, endTok.End), Filler: filler, Orientation: "LEFT"}, nil
		}
		return &ast.GraphEdgePattern{Span: span(start.Pos, minus.End), Orientation: "LEFT"}, nil
	case token.MINUS: // - or -[...]- or -[...]->
		p.advance() // -
		if p.peek().Kind == token.LBRACKET {
			filler, endTok, err := p.parseGraphBracketedFiller(token.MINUS, token.ARROW)
			if err != nil {
				return nil, err
			}
			orient := "ANY"
			if endTok.Kind == token.ARROW {
				orient = "RIGHT"
			}
			return &ast.GraphEdgePattern{Span: span(start.Pos, endTok.End), Filler: filler, Orientation: orient}, nil
		}
		return &ast.GraphEdgePattern{Span: span(start.Pos, start.End), Orientation: "ANY"}, nil
	}
	return nil, p.errorf(start.Pos, "Syntax error: Unexpected %s", describeToken(start))
}

// parseGraphBracketedFiller parses "[ <filler> ]" followed by one of the given
// closing token kinds, returning the filler and the closing token.
func (p *parser) parseGraphBracketedFiller(closers ...token.Kind) (*ast.GraphElementPatternFiller, token.Token, error) {
	p.advance() // [
	filler, err := p.parseGraphElementPatternFiller()
	if err != nil {
		return nil, token.Token{}, err
	}
	if p.peek().Kind != token.RBRACKET {
		return nil, token.Token{}, p.errorf(p.peek().Pos, `Syntax error: Expected "]" but got %s`, describeToken(p.peek()))
	}
	p.advance() // ]
	for _, k := range closers {
		if p.peek().Kind == k {
			return filler, p.advance(), nil
		}
	}
	return nil, token.Token{}, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
}

// parseGraphElementPatternFiller parses the optional variable name, label
// filter, and WHERE clause inside a node or edge pattern; see
// graph_element_pattern_filler in googlesql.tm. Property specifications, cost,
// and hints are not yet supported.
func (p *parser) parseGraphElementPatternFiller() (*ast.GraphElementPatternFiller, error) {
	startPos := p.peek().Pos
	var name *ast.Identifier
	var label *ast.GraphLabelFilter
	var where *ast.WhereClause
	end := startPos

	if t := p.peek(); (t.Kind == token.IDENT && !isReserved(t)) || t.Kind == token.QUOTED_IDENT {
		name = p.parseIdentifierToken(p.advance())
		end = name.End()
	}
	if isKeyword(p.peek(), "IS") || p.peek().Kind == token.COLON {
		lf, err := p.parseGraphLabelFilter()
		if err != nil {
			return nil, err
		}
		label = lf
		end = lf.End()
	}
	if isKeyword(p.peek(), "WHERE") {
		w, err := p.parseWhereClause()
		if err != nil {
			return nil, err
		}
		where = w
		end = w.End()
	}
	return &ast.GraphElementPatternFiller{Span: span(startPos, end), Name: name, Label: label, Where: where}, nil
}

// parseGraphLabelFilter parses "( IS | : ) <label_expression>"; see
// is_label_expression in googlesql.tm.
func (p *parser) parseGraphLabelFilter() (*ast.GraphLabelFilter, error) {
	tok := p.advance() // IS or :
	expr, err := p.parseGraphLabelOr()
	if err != nil {
		return nil, err
	}
	return &ast.GraphLabelFilter{Span: span(tok.Pos, expr.End()), Expr: expr}, nil
}

// Label expressions are parsed with precedence "|" < "&" < "!"/primary; see
// label_expression in googlesql.tm. Adjacent operands of the same operator are
// flattened into a single n-ary GraphLabelOperation unless parenthesized,
// matching MakeOrCombineGraphLabelOperation in parser_internal.h.

func (p *parser) parseGraphLabelOr() (ast.Node, error) {
	left, err := p.parseGraphLabelAnd()
	if err != nil {
		return nil, err
	}
	for p.peek().Kind == token.PIPE {
		p.advance() // |
		right, err := p.parseGraphLabelAnd()
		if err != nil {
			return nil, err
		}
		left = combineGraphLabelOperation("OR", left, right)
	}
	return left, nil
}

func (p *parser) parseGraphLabelAnd() (ast.Node, error) {
	left, err := p.parseGraphLabelNot()
	if err != nil {
		return nil, err
	}
	for p.peek().Kind == token.AMP {
		p.advance() // &
		right, err := p.parseGraphLabelNot()
		if err != nil {
			return nil, err
		}
		left = combineGraphLabelOperation("AND", left, right)
	}
	return left, nil
}

func (p *parser) parseGraphLabelNot() (ast.Node, error) {
	if p.peek().Kind == token.EXCL {
		notTok := p.advance() // !
		operand, err := p.parseGraphLabelNot()
		if err != nil {
			return nil, err
		}
		return &ast.GraphLabelOperation{
			Span:     span(notTok.Pos, operand.End()),
			Op:       "NOT",
			Operands: []ast.Node{operand},
		}, nil
	}
	return p.parseGraphLabelPrimary()
}

func (p *parser) parseGraphLabelPrimary() (ast.Node, error) {
	switch {
	case p.peek().Kind == token.PERCENT:
		tok := p.advance() // %
		return &ast.GraphWildcardLabel{Span: span(tok.Pos, tok.End)}, nil
	case p.peek().Kind == token.LPAREN:
		p.advance() // (
		inner, err := p.parseGraphLabelOr()
		if err != nil {
			return nil, err
		}
		if p.peek().Kind != token.RPAREN {
			return nil, p.errorf(p.peek().Pos, `Syntax error: Expected ")" but got %s`, describeToken(p.peek()))
		}
		p.advance() // )
		// Parentheses do not extend the node's location, but they prevent
		// flattening of same-operator operands.
		if op, ok := inner.(*ast.GraphLabelOperation); ok {
			op.Parenthesized = true
		}
		return inner, nil
	case p.peek().Kind == token.IDENT && !isReserved(p.peek()), p.peek().Kind == token.QUOTED_IDENT:
		ident := p.parseIdentifierToken(p.advance())
		return &ast.GraphElementLabel{Span: span(ident.Pos(), ident.End()), Name: ident}, nil
	}
	return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
}

// combineGraphLabelOperation combines left and right under op, flattening into
// left when it is an unparenthesized operation of the same op; see
// MakeOrCombineGraphLabelOperation in parser_internal.h.
func combineGraphLabelOperation(op string, left, right ast.Node) ast.Node {
	if lo, ok := left.(*ast.GraphLabelOperation); ok && lo.Op == op && !lo.Parenthesized {
		lo.Operands = append(lo.Operands, right)
		lo.Stop = right.End()
		return lo
	}
	return &ast.GraphLabelOperation{
		Span:     span(left.Pos(), right.End()),
		Op:       op,
		Operands: []ast.Node{left, right},
	}
}
