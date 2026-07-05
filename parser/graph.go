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
	"strings"

	"github.com/sqlc-dev/zetajones/ast"
	"github.com/sqlc-dev/zetajones/token"
)

// parseGraphStatement parses "GRAPH <name> <operation_block>" as a query
// statement; see gql_statement / gql_query in googlesql.tm. The result is a
// QueryStatement wrapping Query > GqlQuery > GraphTableQuery.
func (p *parser) parseGraphStatement() (ast.Statement, error) {
	q, err := p.parseGraphQuery()
	if err != nil {
		return nil, err
	}
	return &ast.QueryStatement{Span: span(q.Pos(), q.End()), Query: q}, nil
}

// parseGraphQuery parses "GRAPH <name> <operation_block>" and returns the
// Query > GqlQuery > GraphTableQuery wrapper; see gql_query in googlesql.tm.
// It is shared by the top-level GQL statement and the DDL/EXPORT
// query_after_as position (query_after_as: query | gql_query).
func (p *parser) parseGraphQuery() (*ast.Query, error) {
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
	return &ast.Query{Span: span(graphTok.Pos, end), QueryExpr: gq}, nil
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
			ops, err := p.parseGraphOperationBlock([]ast.Node{match})
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
func (p *parser) parseGraphOperationBlock(firstOps []ast.Node) (*ast.GqlOperatorList, error) {
	block, err := p.parseGraphCompositeQueryBlock(firstOps)
	if err != nil {
		return nil, err
	}
	blocks := []ast.Node{block}
	for isKeyword(p.peek(), "NEXT") {
		p.advance() // NEXT
		block, err := p.parseGraphCompositeQueryBlock(nil)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	return &ast.GqlOperatorList{Span: span(blocks[0].Pos(), blocks[len(blocks)-1].End()), Operators: blocks}, nil
}

// parseGraphCompositeQueryBlock parses a single NEXT-separated block: either a
// lone linear query operation or a composite query (a set operation between two
// or more linear query operations); see graph_composite_query_block /
// graph_composite_query_prefix in googlesql.tm. firstOp, if non-nil, is a
// linear operator already consumed for the leftmost operand.
func (p *parser) parseGraphCompositeQueryBlock(firstOps []ast.Node) (ast.Node, error) {
	left, err := p.parseGraphLinearQueryOperation(firstOps)
	if err != nil {
		return nil, err
	}
	if !p.atSetOpMetadataStart() {
		return left, nil
	}

	var metas []*ast.SetOperationMetadata
	inputs := []ast.Node{left}
	for p.atSetOpMetadataStart() {
		md, err := p.parseGraphSetOperationMetadata()
		if err != nil {
			return nil, err
		}
		metas = append(metas, md)
		right, err := p.parseGraphLinearQueryOperation(nil)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, right)
	}
	mdl := &ast.SetOperationMetadataList{
		Span:    span(metas[0].Pos(), metas[len(metas)-1].End()),
		Entries: metas,
	}
	return &ast.GqlSetOperation{
		Span:     span(inputs[0].Pos(), inputs[len(inputs)-1].End()),
		Metadata: mdl,
		Inputs:   inputs,
	}, nil
}

// parseGraphSetOperationMetadata parses one GQL set operator:
// "[outer_mode] (UNION|INTERSECT|EXCEPT) (ALL|DISTINCT)"; see
// graph_set_operation_metadata in googlesql.tm. Unlike the SQL set operation
// metadata, GQL does not allow hints, STRICT, or a column-match suffix.
func (p *parser) parseGraphSetOperationMetadata() (*ast.SetOperationMetadata, error) {
	start := p.peek().Pos

	var outerMode *ast.SetOperationColumnPropagationMode
	tok := p.peek()
	switch {
	case isKeyword(tok, "FULL"), isKeyword(tok, "LEFT"):
		p.advance()
		end := tok.End
		if isKeyword(p.peek(), "OUTER") {
			end = p.advance().End
		}
		value := "FULL"
		if isKeyword(tok, "LEFT") {
			value = "LEFT"
		}
		outerMode = &ast.SetOperationColumnPropagationMode{Span: span(tok.Pos, end), Value: value}
	case isKeyword(tok, "OUTER"):
		p.advance()
		outerMode = &ast.SetOperationColumnPropagationMode{Span: span(tok.Pos, tok.End), Value: "FULL"}
	case isKeyword(tok, "INNER"):
		p.advance()
		outerMode = &ast.SetOperationColumnPropagationMode{Span: span(tok.Pos, tok.End), Value: "INNER"}
	}

	opTok := p.advance() // UNION, INTERSECT, or EXCEPT
	md := &ast.SetOperationMetadata{
		Span:                  span(start, 0),
		OpType:                &ast.SetOperationType{Span: span(opTok.Pos, opTok.End), Op: strings.ToUpper(opTok.Image)},
		ColumnPropagationMode: outerMode,
	}

	tok = p.peek()
	var value string
	switch {
	case isKeyword(tok, "ALL"):
		value = "ALL"
	case isKeyword(tok, "DISTINCT"):
		value = "DISTINCT"
	default:
		return nil, p.errorf(tok.Pos, "Syntax error: Expected keyword ALL or keyword DISTINCT but got %s", describeToken(tok))
	}
	p.advance()
	md.AllOrDistinct = &ast.SetOperationAllOrDistinct{Span: span(tok.Pos, tok.End), Value: value}
	md.Stop = tok.End
	return md, nil
}

// parseGraphLinearQueryOperation parses a sequence of linear operators
// terminated by a mandatory RETURN, wrapping them in a GqlOperatorList; see
// graph_linear_query_operation in googlesql.tm.
func (p *parser) parseGraphLinearQueryOperation(firstOps []ast.Node) (*ast.GqlOperatorList, error) {
	rawOps := append([]ast.Node(nil), firstOps...)
	for !isKeyword(p.peek(), "RETURN") {
		if !p.startsGraphLinearOp() {
			// At the very start of a linear query operation (nothing parsed
			// yet), an unexpected token yields the generic "Unexpected" error
			// because many operators could begin here; once at least one
			// operator has been parsed, only RETURN can complete the operation.
			if len(rawOps) == 0 {
				return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
			}
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword RETURN but got %s", describeToken(p.peek()))
		}
		op, err := p.parseGraphLinearOp()
		if err != nil {
			return nil, err
		}
		rawOps = append(rawOps, op)
	}
	ops := combineGraphLinearOps(rawOps)
	ret, err := p.parseGqlReturn()
	if err != nil {
		return nil, err
	}
	start := ret.Pos()
	if len(ops) > 0 {
		start = ops[0].Pos()
	}
	ops = append(ops, ret)
	return &ast.GqlOperatorList{Span: span(start, ret.End()), Operators: ops}, nil
}

// combineGraphLinearOps folds consecutive OFFSET/LIMIT (or SKIP/LIMIT) page
// operators into GqlOrderByAndPage(GqlPage(...)) nodes, mirroring the small
// state machine in the reduce action of graph_linear_operator_list in
// googlesql.tm. A LIMIT immediately following an OFFSET is merged into the same
// GqlPage; otherwise each page clause becomes its own GqlOrderByAndPage.
func combineGraphLinearOps(rawOps []ast.Node) []ast.Node {
	var out []ast.Node
	prevWasOffset := false
	var page *ast.GqlPage
	for _, thisOp := range rawOps {
		if lim, ok := thisOp.(*ast.GqlPageLimit); ok && prevWasOffset {
			page.Limit = lim
			page.Stop = lim.End()
			last := out[len(out)-1].(*ast.GqlOrderByAndPage)
			last.Stop = lim.End()
			page = nil
			prevWasOffset = false
			continue
		}
		_, prevWasOffset = thisOp.(*ast.GqlPageOffset)
		switch op := thisOp.(type) {
		case *ast.GqlPageOffset:
			page = &ast.GqlPage{Span: span(op.Pos(), op.End()), Offset: op}
			out = append(out, &ast.GqlOrderByAndPage{Span: span(op.Pos(), op.End()), Page: page})
		case *ast.GqlPageLimit:
			page = &ast.GqlPage{Span: span(op.Pos(), op.End()), Limit: op}
			out = append(out, &ast.GqlOrderByAndPage{Span: span(op.Pos(), op.End()), Page: page})
		default:
			out = append(out, thisOp)
		}
	}
	return out
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
	case isKeyword(p.peek(), "TABLESAMPLE"):
		return true
	case isKeyword(p.peek(), "WITH"):
		return true
	case isKeyword(p.peek(), "FOR"):
		return true
	case isKeyword(p.peek(), "CALL"):
		return true
	case isKeyword(p.peek(), "OPTIONAL") && isKeyword(p.peekAt(1), "CALL"):
		return true
	case isKeyword(p.peek(), "ORDER"):
		return true
	case isKeyword(p.peek(), "OFFSET"), isKeyword(p.peek(), "SKIP"):
		return true
	case isKeyword(p.peek(), "LIMIT"):
		return true
	}
	return false
}

func (p *parser) parseGraphLinearOp() (ast.Node, error) {
	switch {
	case isKeyword(p.peek(), "OPTIONAL") && isKeyword(p.peekAt(1), "CALL"):
		return p.parseGqlCall()
	case isKeyword(p.peek(), "OPTIONAL"):
		return p.parseGqlOptionalMatch()
	case isKeyword(p.peek(), "MATCH"):
		return p.parseGqlMatch()
	case isKeyword(p.peek(), "LET"):
		return p.parseGqlLet()
	case isKeyword(p.peek(), "FILTER"):
		return p.parseGqlFilter()
	case isKeyword(p.peek(), "WITH"):
		return p.parseGqlWith()
	case isKeyword(p.peek(), "FOR"):
		return p.parseGqlFor()
	case isKeyword(p.peek(), "CALL"):
		return p.parseGqlCall()
	case isKeyword(p.peek(), "TABLESAMPLE"):
		return p.parseGqlSample()
	case isKeyword(p.peek(), "ORDER"):
		return p.parseGraphOrderByOperator()
	case isKeyword(p.peek(), "OFFSET"), isKeyword(p.peek(), "SKIP"):
		return p.parseGraphOffsetClause()
	case isKeyword(p.peek(), "LIMIT"):
		return p.parseGraphLimitClause()
	}
	return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
}

// parseGraphOrderByOperator parses "ORDER [hint] BY <ordering_list>" as a
// standalone GQL linear operator, wrapping the OrderBy in a GqlOrderByAndPage
// (with no page); see graph_order_by_operator in googlesql.tm.
func (p *parser) parseGraphOrderByOperator() (*ast.GqlOrderByAndPage, error) {
	ob, err := p.parseGraphOrderBy()
	if err != nil {
		return nil, err
	}
	return &ast.GqlOrderByAndPage{Span: span(ob.Pos(), ob.End()), OrderBy: ob}, nil
}

// parseGraphOrderBy parses "ORDER [hint] BY <graph_ordering_expression_list>";
// see graph_order_by_clause in googlesql.tm. Unlike the SQL ORDER BY, each
// ordering expression also accepts the ASCENDING / DESCENDING keywords.
func (p *parser) parseGraphOrderBy() (*ast.OrderBy, error) {
	orderTok := p.advance() // ORDER
	hint, err := p.parseOptionalHint()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("BY"); err != nil {
		return nil, err
	}
	ob := &ast.OrderBy{Span: span(orderTok.Pos, orderTok.End), Hint: hint}
	for {
		item, err := p.parseGraphOrderingExpression()
		if err != nil {
			return nil, err
		}
		ob.Items = append(ob.Items, item)
		ob.Stop = item.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance() // ,
	}
	return ob, nil
}

// parseGraphOrderingExpression parses "expression [COLLATE c]
// [ASC|ASCENDING|DESC|DESCENDING] [NULLS FIRST|LAST]"; see
// graph_ordering_expression / opt_graph_asc_or_desc in googlesql.tm.
func (p *parser) parseGraphOrderingExpression() (*ast.OrderingExpression, error) {
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	item := &ast.OrderingExpression{Span: span(p.extStart(expr), p.extEnd(expr)), Expr: expr}
	if isKeyword(p.peek(), "COLLATE") {
		collate, err := p.parseCollate()
		if err != nil {
			return nil, err
		}
		item.Collate = collate
		item.Stop = collate.End()
	}
	switch {
	case isKeyword(p.peek(), "ASC"), isKeyword(p.peek(), "ASCENDING"):
		tok := p.advance()
		item.HasAsc = true
		item.Stop = tok.End
	case isKeyword(p.peek(), "DESC"), isKeyword(p.peek(), "DESCENDING"):
		tok := p.advance()
		item.Descending = true
		item.Stop = tok.End
	}
	if isKeyword(p.peek(), "NULLS") {
		nullsTok := p.advance()
		var nullsFirst bool
		switch {
		case isKeyword(p.peek(), "FIRST"):
			nullsFirst = true
		case isKeyword(p.peek(), "LAST"):
			nullsFirst = false
		default:
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword FIRST or keyword LAST but got %s", describeToken(p.peek()))
		}
		endTok := p.advance()
		item.NullOrder = &ast.NullOrder{Span: span(nullsTok.Pos, endTok.End), NullsFirst: nullsFirst}
		item.Stop = endTok.End
	}
	return item, nil
}

// parseGraphOffsetClause parses "(OFFSET|SKIP) <value>" into a raw GqlPageOffset
// node; see graph_offset_clause in googlesql.tm.
func (p *parser) parseGraphOffsetClause() (*ast.GqlPageOffset, error) {
	tok := p.advance() // OFFSET or SKIP
	val, err := p.parsePossiblyCastIntLiteralOrParameter()
	if err != nil {
		return nil, err
	}
	return &ast.GqlPageOffset{Span: span(tok.Pos, val.End()), Value: val}, nil
}

// parseGraphLimitClause parses "LIMIT <value>" into a raw GqlPageLimit node;
// see graph_limit_clause in googlesql.tm.
func (p *parser) parseGraphLimitClause() (*ast.GqlPageLimit, error) {
	tok := p.advance() // LIMIT
	val, err := p.parsePossiblyCastIntLiteralOrParameter()
	if err != nil {
		return nil, err
	}
	return &ast.GqlPageLimit{Span: span(tok.Pos, val.End()), Value: val}, nil
}

// parseGqlMatch parses "MATCH <graph_pattern>"; see graph_match_operator in
// googlesql.tm.
func (p *parser) parseGqlMatch() (*ast.GqlMatch, error) {
	matchTok := p.advance() // MATCH
	hint, err := p.parseOptionalHint()
	if err != nil {
		return nil, err
	}
	pattern, err := p.parseGraphPattern()
	if err != nil {
		return nil, err
	}
	return &ast.GqlMatch{Span: span(matchTok.Pos, pattern.End()), Pattern: pattern, Hint: hint}, nil
}

// parseGqlOptionalMatch parses "OPTIONAL MATCH <graph_pattern>"; see
// graph_optional_match_operator in googlesql.tm.
func (p *parser) parseGqlOptionalMatch() (*ast.GqlMatch, error) {
	optTok := p.advance() // OPTIONAL
	p.advance()           // MATCH
	hint, err := p.parseOptionalHint()
	if err != nil {
		return nil, err
	}
	pattern, err := p.parseGraphPattern()
	if err != nil {
		return nil, err
	}
	return &ast.GqlMatch{Span: span(optTok.Pos, pattern.End()), Pattern: pattern, Hint: hint, Optional: true}, nil
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

// parseGqlWith parses "WITH [ALL|DISTINCT] [hint] <items> [GROUP BY ...]"; see
// graph_with_operator in googlesql.tm. It reuses the RETURN item list and
// builds a Select (with the same folding of hint / set quantifier / group by).
func (p *parser) parseGqlWith() (*ast.GqlWith, error) {
	withTok := p.advance() // WITH
	distinct, setQuantStart := p.parseGqlSetQuantifier()
	hint, err := p.parseOptionalHint()
	if err != nil {
		return nil, err
	}
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
	var groupBy *ast.GroupBy
	if isKeyword(p.peek(), "GROUP") {
		groupBy, err = p.parseGroupBy(groupByRegular)
		if err != nil {
			return nil, err
		}
	}
	sel := p.buildGqlSelect(list, hint, distinct, setQuantStart, groupBy)
	return &ast.GqlWith{Span: span(withTok.Pos, sel.End()), Select: sel}, nil
}

// parseGqlFor parses "FOR <name> IN <expr> [WITH OFFSET [AS alias]]"; see
// graph_for_operator in googlesql.tm. Once a WITH follows the IN expression the
// parser commits to a WITH OFFSET clause (matching the reference's greedy shift
// resolving the WITH OFFSET / WITH-operator ambiguity).
func (p *parser) parseGqlFor() (*ast.GqlFor, error) {
	forTok := p.advance() // FOR
	name, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("IN"); err != nil {
		return nil, err
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	node := &ast.GqlFor{Span: span(forTok.Pos, p.extEnd(expr)), Name: name, Expr: expr}
	if isKeyword(p.peek(), "WITH") {
		withTok := p.advance() // WITH
		offsetTok, err := p.expectKeyword("OFFSET")
		if err != nil {
			return nil, err
		}
		off := &ast.WithOffset{Span: span(withTok.Pos, offsetTok.End)}
		if isKeyword(p.peek(), "AS") {
			alias, err := p.parseRequiredAsAlias()
			if err != nil {
				return nil, err
			}
			off.Alias = alias
			off.Stop = alias.End()
		}
		node.WithOffset = off
		node.Stop = off.End()
	}
	return node, nil
}

// parseGqlCall parses a GQL CALL operator in any of its forms; see
// graph_call_operator / graph_call_operator_core in googlesql.tm. It handles
// the optional leading OPTIONAL, an optional PER capture list, and then either
// a named TVF (with optional YIELD), an inline braced subquery, or a
// parenthesized capture list followed by a braced subquery.
func (p *parser) parseGqlCall() (ast.Node, error) {
	start := p.peek().Pos
	optional := false
	if isKeyword(p.peek(), "OPTIONAL") {
		optional = true
		p.advance() // OPTIONAL
	}
	p.advance() // CALL

	var per *ast.IdentifierList
	isPartitioned := false
	if isKeyword(p.peek(), "PER") {
		p.advance() // PER
		list, err := p.parseParenthesizedIdentifierList()
		if err != nil {
			return nil, err
		}
		per = list
		isPartitioned = true
	}

	// Inline subquery: "CALL [PER(...)] { subquery }".
	if p.peek().Kind == token.LBRACE {
		subquery, err := p.parseBracedGraphSubquery()
		if err != nil {
			return nil, err
		}
		return &ast.GqlInlineSubqueryCall{
			Span: span(start, subquery.End()), Optional: optional,
			IsPartitioned: isPartitioned, Subquery: subquery, Captures: per,
		}, nil
	}

	// Capture list form: "CALL (captures) { subquery }" (no PER allowed).
	if per == nil && p.peek().Kind == token.LPAREN {
		captures, err := p.parseParenthesizedIdentifierList()
		if err != nil {
			return nil, err
		}
		if p.peek().Kind != token.LBRACE {
			return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "{" but got %s`, describeToken(p.peek()))
		}
		subquery, err := p.parseBracedGraphSubquery()
		if err != nil {
			return nil, err
		}
		return &ast.GqlInlineSubqueryCall{
			Span: span(start, subquery.End()), Optional: optional,
			Subquery: subquery, Captures: captures,
		}, nil
	}

	// Named call: "CALL [PER(...)] tvf [YIELD ...]".
	path, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != token.LPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" but got %s`, describeToken(p.peek()))
	}
	tvf, err := p.parseTVFCore(path)
	if err != nil {
		return nil, err
	}
	end := tvf.End()
	var yield *ast.YieldItemList
	if isKeyword(p.peek(), "YIELD") {
		yield, err = p.parseYieldClause()
		if err != nil {
			return nil, err
		}
		end = yield.End()
	}
	return &ast.GqlNamedCall{
		Span: span(start, end), Optional: optional, IsPartitioned: isPartitioned,
		TVF: tvf, Yield: yield, Per: per,
	}, nil
}

// parseParenthesizedIdentifierList parses "( [identifier_list] )"; see
// parenthesized_identifier_list in googlesql.tm. The resulting IdentifierList
// spans the parentheses.
func (p *parser) parseParenthesizedIdentifierList() (*ast.IdentifierList, error) {
	lparen := p.advance() // (
	if p.peek().Kind == token.RPAREN {
		rparen := p.advance() // )
		return &ast.IdentifierList{Span: span(lparen.Pos, rparen.End)}, nil
	}
	list, err := p.parseIdentifierList()
	if err != nil {
		return nil, err
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	list.Span = span(lparen.Pos, rparen.End)
	return list, nil
}

// parseYieldClause parses "YIELD <item>, ..."; see opt_yield_clause /
// yield_item_list in googlesql.tm. The list spans from the YIELD keyword.
func (p *parser) parseYieldClause() (*ast.YieldItemList, error) {
	yieldTok := p.advance() // YIELD
	first, err := p.parseYieldItem()
	if err != nil {
		return nil, err
	}
	items := []*ast.ExpressionWithOptAlias{first}
	for p.peek().Kind == token.COMMA {
		p.advance() // ,
		item, err := p.parseYieldItem()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return &ast.YieldItemList{Span: span(yieldTok.Pos, items[len(items)-1].End()), Items: items}, nil
}

// parseYieldItem parses "identifier [AS alias]"; see yield_item in
// googlesql.tm.
func (p *parser) parseYieldItem() (*ast.ExpressionWithOptAlias, error) {
	id, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	item := &ast.ExpressionWithOptAlias{Span: span(id.Pos(), id.End()), Expr: id}
	if isKeyword(p.peek(), "AS") {
		alias, err := p.parseRequiredAsAlias()
		if err != nil {
			return nil, err
		}
		item.Alias = alias
		item.Stop = alias.End()
	}
	return item, nil
}

// parseBracedGraphSubquery parses "{ [GRAPH g] graph_operation_block }"; see
// braced_graph_subquery in googlesql.tm. It builds the Query > GqlQuery >
// GraphTableQuery wrapper (all three spanning the braces).
func (p *parser) parseBracedGraphSubquery() (*ast.Query, error) {
	lbrace := p.advance() // {
	var graph *ast.PathExpression
	if isKeyword(p.peek(), "GRAPH") {
		p.advance() // GRAPH
		g, err := p.parsePathExpression()
		if err != nil {
			return nil, err
		}
		graph = g
	}
	ops, err := p.parseGraphOperationBlock(nil)
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != token.RBRACE {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "}" or keyword NEXT but got %s`, describeToken(p.peek()))
	}
	rbrace := p.advance() // }
	loc := span(lbrace.Pos, rbrace.End)
	gt := &ast.GraphTableQuery{Span: loc, Graph: graph, Op: ops}
	gq := &ast.GqlQuery{Span: loc, Query: gt}
	return &ast.Query{Span: loc, QueryExpr: gq}, nil
}

// parseBracedGraphExpressionSubquery parses the braced graph subquery following
// an ARRAY or VALUE modifier keyword (already consumed, starting at startPos),
// with an optional hint already parsed; see the "ARRAY"/"VALUE"
// braced_graph_subquery alternatives of expression_subquery_with_keyword in
// googlesql.tm.
func (p *parser) parseBracedGraphExpressionSubquery(startPos int, modifier string, hint *ast.Hint) (ast.Node, error) {
	q, err := p.parseBracedGraphSubquery()
	if err != nil {
		return nil, err
	}
	return &ast.ExpressionSubquery{Span: span(startPos, q.End()), Modifier: modifier, Hint: hint, Query: q}, nil
}

// startsGraphPattern reports whether the next tokens begin a graph pattern (as
// opposed to a graph linear operator or RETURN). A path pattern begins with a
// path factor, a path mode keyword, a path search prefix, or an "ident ="
// path-variable assignment; see graph_pattern / graph_path_pattern in
// googlesql.tm.
func (p *parser) startsGraphPattern() bool {
	if p.startsGraphPathFactor() {
		return true
	}
	if isGraphPathModeKeyword(p.peek()) {
		return true
	}
	if p.startsGraphSearchPrefix() {
		return true
	}
	t := p.peek()
	if ((t.Kind == token.IDENT && !p.isReserved(t)) || t.Kind == token.QUOTED_IDENT) && p.peekAt(1).Kind == token.EQ {
		return true
	}
	return false
}

// parseExistsGraphSubquery parses the graph subquery following an EXISTS
// keyword (already consumed, starting at existsPos), with the hint already
// parsed and the next token being "{". It handles all three graph forms: a
// bare graph pattern (GqlGraphPatternQuery), a linear operator list with no
// RETURN (GqlLinearOpsQuery), and a full operation block ending in RETURN
// (braced graph subquery). See exists_graph_subquery in googlesql.tm.
func (p *parser) parseExistsGraphSubquery(existsPos int, hint *ast.Hint) (ast.Node, error) {
	lbrace := p.advance() // {
	var graph *ast.PathExpression
	if isKeyword(p.peek(), "GRAPH") {
		p.advance() // GRAPH
		g, err := p.parsePathExpression()
		if err != nil {
			return nil, err
		}
		graph = g
	}

	// Bare graph pattern: EXISTS { [GRAPH g] graph_pattern }.
	if p.startsGraphPattern() {
		pattern, err := p.parseGraphPattern()
		if err != nil {
			return nil, err
		}
		rbrace, err := p.expect(token.RBRACE, `"}"`)
		if err != nil {
			return nil, err
		}
		loc := span(existsPos, rbrace.End)
		gp := &ast.GqlGraphPatternQuery{Span: loc, Graph: graph, Pattern: pattern}
		q := &ast.Query{Span: loc, QueryExpr: gp}
		return &ast.ExpressionSubquery{Span: loc, Modifier: "EXISTS", Hint: hint, Query: q}, nil
	}

	// Otherwise a sequence of linear operators, terminated either by RETURN
	// (an operation block) or by "}" (a linear-ops-only subquery).
	var rawOps []ast.Node
	for p.startsGraphLinearOp() {
		op, err := p.parseGraphLinearOp()
		if err != nil {
			return nil, err
		}
		rawOps = append(rawOps, op)
	}

	if isKeyword(p.peek(), "RETURN") {
		ops, err := p.parseGraphOperationBlock(rawOps)
		if err != nil {
			return nil, err
		}
		rbrace, err := p.expect(token.RBRACE, `"}"`)
		if err != nil {
			return nil, err
		}
		bracedLoc := span(lbrace.Pos, rbrace.End)
		gt := &ast.GraphTableQuery{Span: bracedLoc, Graph: graph, Op: ops}
		gq := &ast.GqlQuery{Span: bracedLoc, Query: gt}
		q := &ast.Query{Span: bracedLoc, QueryExpr: gq}
		loc := span(existsPos, rbrace.End)
		return &ast.ExpressionSubquery{Span: loc, Modifier: "EXISTS", Hint: hint, Query: q}, nil
	}

	if len(rawOps) == 0 {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
	}
	rbrace, err := p.expect(token.RBRACE, `"}"`)
	if err != nil {
		return nil, err
	}
	opsList := &ast.GqlOperatorList{
		Span:      span(rawOps[0].Pos(), rawOps[len(rawOps)-1].End()),
		Operators: combineGraphLinearOps(rawOps),
	}
	loc := span(existsPos, rbrace.End)
	lq := &ast.GqlLinearOpsQuery{Span: loc, Graph: graph, Ops: opsList}
	q := &ast.Query{Span: loc, QueryExpr: lq}
	return &ast.ExpressionSubquery{Span: loc, Modifier: "EXISTS", Hint: hint, Query: q}, nil
}

// parseGqlReturn parses "RETURN <return_item_list> [ORDER BY ...] [OFFSET ...]
// [LIMIT ...]"; see graph_return_operator in googlesql.tm. It builds a Select
// holding the item list; a trailing ORDER BY / OFFSET / LIMIT (offset before
// limit, at most one each) is folded into a single GqlOrderByAndPage. Advanced
// clauses (DISTINCT, GROUP BY) are not yet supported.
// parseGqlSetQuantifier consumes an optional leading ALL or DISTINCT set
// quantifier (opt_all_or_distinct in googlesql.tm). It returns whether DISTINCT
// was seen and the start position of the consumed keyword (-1 if none).
func (p *parser) parseGqlSetQuantifier() (distinct bool, start int) {
	switch {
	case isKeyword(p.peek(), "DISTINCT"):
		return true, p.advance().Pos
	case isKeyword(p.peek(), "ALL"):
		return false, p.advance().Pos
	}
	return false, -1
}

// buildGqlSelect assembles the Select node shared by the RETURN and WITH graph
// operators from an item list, an optional hint, a set quantifier, and an
// optional GROUP BY. The Select's location starts at the leftmost of the hint /
// set quantifier / item list and ends at the GROUP BY (if any); see the
// ExtendNode / WithStartLocation logic in graph_return_operator /
// graph_with_operator in googlesql.tm.
func (p *parser) buildGqlSelect(list *ast.SelectList, hint *ast.Hint, distinct bool, setQuantStart int, groupBy *ast.GroupBy) *ast.Select {
	start := list.Pos()
	if setQuantStart >= 0 && setQuantStart < start {
		start = setQuantStart
	}
	if hint != nil && hint.Pos() < start {
		start = hint.Pos()
	}
	end := list.End()
	if groupBy != nil {
		end = groupBy.End()
	}
	return &ast.Select{Span: span(start, end), Hint: hint, Distinct: distinct, SelectList: list, GroupBy: groupBy}
}

func (p *parser) parseGqlReturn() (*ast.GqlReturn, error) {
	returnTok := p.advance() // RETURN
	hint, err := p.parseOptionalHint()
	if err != nil {
		return nil, err
	}
	distinct, setQuantStart := p.parseGqlSetQuantifier()
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
	var groupBy *ast.GroupBy
	if isKeyword(p.peek(), "GROUP") {
		groupBy, err = p.parseGroupBy(groupByRegular)
		if err != nil {
			return nil, err
		}
	}
	sel := p.buildGqlSelect(list, hint, distinct, setQuantStart, groupBy)

	// Location where an absent ORDER BY would sit (end of the item list or the
	// GROUP BY clause), used as the start of a page-only GqlOrderByAndPage; see
	// the MakeLocationRange(@order_by, @$) in graph_return_operator.
	afterItems := p.prevEnd()

	var ob *ast.OrderBy
	if isKeyword(p.peek(), "ORDER") {
		ob, err = p.parseGraphOrderBy()
		if err != nil {
			return nil, err
		}
	}
	var offset *ast.GqlPageOffset
	if isKeyword(p.peek(), "OFFSET") || isKeyword(p.peek(), "SKIP") {
		offset, err = p.parseGraphOffsetClause()
		if err != nil {
			return nil, err
		}
	}
	var limit *ast.GqlPageLimit
	if isKeyword(p.peek(), "LIMIT") {
		limit, err = p.parseGraphLimitClause()
		if err != nil {
			return nil, err
		}
	}

	ret := &ast.GqlReturn{Span: span(returnTok.Pos, sel.End()), Select: sel}
	var page *ast.GqlPage
	if offset != nil || limit != nil {
		pageStart, pageEnd := afterItems, afterItems
		switch {
		case offset != nil && limit != nil:
			pageStart, pageEnd = offset.Pos(), limit.End()
		case offset != nil:
			pageStart, pageEnd = offset.Pos(), offset.End()
		case limit != nil:
			pageStart, pageEnd = limit.Pos(), limit.End()
		}
		page = &ast.GqlPage{Span: span(pageStart, pageEnd), Offset: offset, Limit: limit}
	}
	if ob != nil || page != nil {
		start := afterItems
		if ob != nil {
			start = ob.Pos()
		}
		end := afterItems
		if page != nil {
			end = page.End()
		} else if ob != nil {
			end = ob.End()
		}
		ret.OrderByPage = &ast.GqlOrderByAndPage{Span: span(start, end), OrderBy: ob, Page: page}
		ret.Stop = end
	}
	return ret, nil
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

// parseGqlSample parses a "TABLESAMPLE ..." linear operator; see
// graph_sample_operator / graph_sample_clause in googlesql.tm. The graph
// sample suffix requires "AS" before a WITH WEIGHT alias to avoid ambiguity
// with the next graph operator.
func (p *parser) parseGqlSample() (*ast.GqlSample, error) {
	sample, err := p.parseSampleClause(true)
	if err != nil {
		return nil, err
	}
	return &ast.GqlSample{Span: span(sample.Pos(), sample.End()), Sample: sample}, nil
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
		// A path in the list (after the comma) may be prefixed with a hint,
		// which is attached to the front of the path pattern.
		var hint *ast.Hint
		if p.peek().Kind == token.ATSIGN {
			hint, err = p.parseOptionalHint()
			if err != nil {
				return nil, err
			}
		}
		next, err := p.parseGraphPathPattern()
		if err != nil {
			return nil, err
		}
		if hint != nil {
			prependGraphPathFactors(next, hint)
			next.Start = hint.Pos()
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

// parseGraphPathPattern parses an optional "graph_identifier =" path-variable
// assignment, an optional path search prefix, an optional path mode, and a
// sequence of node/edge path factors; see graph_path_pattern in googlesql.tm.
func (p *parser) parseGraphPathPattern() (*ast.GraphPathPattern, error) {
	startPos := p.peek().Pos
	var pathName *ast.Identifier
	var searchPrefix *ast.GraphPathSearchPrefix
	// (graph_identifier "=")? — a leading bare identifier is a path-variable
	// assignment unless it is a path-mode keyword or begins a search prefix. A
	// path factor never starts with an identifier, so an identifier that is
	// neither must be an assignment target (which requires "=").
	if t := p.peek(); ((t.Kind == token.IDENT && !p.isReserved(t)) || t.Kind == token.QUOTED_IDENT) && !isGraphPathModeKeyword(t) {
		if p.peekAt(1).Kind == token.EQ || !p.startsGraphSearchPrefix() {
			pathName = p.parseIdentifierToken(p.advance())
			if p.peek().Kind != token.EQ {
				return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "=" but got %s`, describeToken(p.peek()))
			}
			p.advance() // =
		}
	}
	// graph_search_prefix?
	if p.startsGraphSearchPrefix() {
		sp, err := p.parseGraphSearchPrefix()
		if err != nil {
			return nil, err
		}
		searchPrefix = sp
	}
	// graph_path_mode?
	var mode *ast.GraphPathMode
	if isGraphPathModeKeyword(p.peek()) {
		mode = p.parseGraphPathMode()
	}
	pattern, err := p.parseGraphPathPatternExpr()
	if err != nil {
		return nil, err
	}
	if pathName == nil && searchPrefix == nil && mode == nil {
		return pattern, nil
	}
	// A parenthesized pattern is normally returned unwrapped; wrap it again so
	// the prefix (assignment / search prefix / mode) has a dedicated
	// ASTGraphPathPattern.
	if pattern.Parenthesized {
		pattern = &ast.GraphPathPattern{Span: pattern.Span, Factors: []ast.Node{pattern}}
	}
	pattern.PathName = pathName
	pattern.SearchPrefix = searchPrefix
	if mode != nil {
		pattern.Factors = append([]ast.Node{mode}, pattern.Factors...)
	}
	pattern.Start = startPos
	return pattern, nil
}

// parseGraphPathPatternExpr parses a concatenation of graph path factors,
// returning an ASTGraphPathPattern; see graph_path_pattern_expr in
// googlesql.tm. A single parenthesized subpath is returned unwrapped.
func (p *parser) parseGraphPathPatternExpr() (*ast.GraphPathPattern, error) {
	first, err := p.parseGraphPathFactor()
	if err != nil {
		return nil, err
	}
	var path *ast.GraphPathPattern
	if pp, ok := first.(*ast.GraphPathPattern); ok {
		path = pp
	} else {
		path = &ast.GraphPathPattern{Span: span(first.Pos(), first.End()), Factors: []ast.Node{first}}
	}
	for p.startsGraphPathFactor() {
		// When concatenating onto a parenthesized subpath, wrap it so the
		// wrapper (not the parenthesized node) grows to hold the new factor.
		if path.Parenthesized {
			path = &ast.GraphPathPattern{Span: path.Span, Factors: []ast.Node{path}}
		}
		next, err := p.parseGraphPathFactor()
		if err != nil {
			return nil, err
		}
		path.Factors = append(path.Factors, next)
		path.Start = path.Factors[0].Pos()
		path.Stop = next.End()
	}
	return path, nil
}

// startsGraphSearchPrefix reports whether the next tokens begin a graph path
// search prefix; see graph_search_prefix in googlesql.tm. ANY and ALL always
// begin a prefix, while SHORTEST and CHEAPEST require a following
// int_literal_or_parameter (otherwise the keyword is a path-variable name).
func (p *parser) startsGraphSearchPrefix() bool {
	return tokensStartGraphSearchPrefix(p.peek(), p.peekAt(1))
}

// tokensStartGraphSearchPrefix reports whether the token t0 (with lookahead t1)
// begins a graph path search prefix; see graph_search_prefix in googlesql.tm.
// ANY and ALL always begin a prefix, while SHORTEST and CHEAPEST require a
// following int_literal_or_parameter (otherwise the keyword is an identifier).
func tokensStartGraphSearchPrefix(t0, t1 token.Token) bool {
	switch {
	case isKeyword(t0, "ANY"), isKeyword(t0, "ALL"):
		return true
	case isKeyword(t0, "SHORTEST"), isKeyword(t0, "CHEAPEST"):
		return startsIntLiteralOrParameter(t1)
	}
	return false
}

// startsIntLiteralOrParameter reports whether tok begins an
// int_literal_or_parameter (integer literal, @parameter, ? parameter, or
// @@system variable); see int_literal_or_parameter in googlesql.tm.
func startsIntLiteralOrParameter(tok token.Token) bool {
	switch tok.Kind {
	case token.INT, token.PARAM, token.QUESTION, token.SYSTEM_VARIABLE:
		return true
	}
	return false
}

// parseGraphSearchPrefix parses a graph path search prefix; see
// graph_search_prefix in googlesql.tm. The count node, when present, spans the
// whole prefix (keyword through the count expression), matching the reference.
func (p *parser) parseGraphSearchPrefix() (*ast.GraphPathSearchPrefix, error) {
	start := p.advance() // ANY / ALL / SHORTEST / CHEAPEST
	switch {
	case isKeyword(start, "ANY"):
		switch {
		case isKeyword(p.peek(), "SHORTEST"):
			end := p.advance()
			return &ast.GraphPathSearchPrefix{Span: span(start.Pos, end.End), Type: "SHORTEST"}, nil
		case isKeyword(p.peek(), "CHEAPEST"):
			end := p.advance()
			return &ast.GraphPathSearchPrefix{Span: span(start.Pos, end.End), Type: "CHEAPEST"}, nil
		case startsIntLiteralOrParameter(p.peek()):
			count, err := p.parseGraphPathSearchPrefixCount(start.Pos)
			if err != nil {
				return nil, err
			}
			return &ast.GraphPathSearchPrefix{Span: span(start.Pos, count.End()), Type: "ANY", Count: count}, nil
		default:
			return &ast.GraphPathSearchPrefix{Span: span(start.Pos, start.End), Type: "ANY"}, nil
		}
	case isKeyword(start, "ALL"):
		switch {
		case isKeyword(p.peek(), "SHORTEST"):
			end := p.advance()
			return &ast.GraphPathSearchPrefix{Span: span(start.Pos, end.End), Type: "ALL_SHORTEST"}, nil
		case isKeyword(p.peek(), "CHEAPEST"):
			end := p.advance()
			return &ast.GraphPathSearchPrefix{Span: span(start.Pos, end.End), Type: "ALL_CHEAPEST"}, nil
		default:
			return &ast.GraphPathSearchPrefix{Span: span(start.Pos, start.End), Type: "ALL"}, nil
		}
	case isKeyword(start, "SHORTEST"):
		count, err := p.parseGraphPathSearchPrefixCount(start.Pos)
		if err != nil {
			return nil, err
		}
		return &ast.GraphPathSearchPrefix{Span: span(start.Pos, count.End()), Type: "SHORTEST", Count: count}, nil
	default: // CHEAPEST
		count, err := p.parseGraphPathSearchPrefixCount(start.Pos)
		if err != nil {
			return nil, err
		}
		return &ast.GraphPathSearchPrefix{Span: span(start.Pos, count.End()), Type: "CHEAPEST", Count: count}, nil
	}
}

// parseGraphPathSearchPrefixCount parses the int_literal_or_parameter path
// count; the resulting node spans from prefixStart (the search keyword) through
// the count expression, matching @$ in the reference grammar.
func (p *parser) parseGraphPathSearchPrefixCount(prefixStart int) (*ast.GraphPathSearchPrefixCount, error) {
	expr, err := p.parseIntLiteralOrParameter()
	if err != nil {
		return nil, err
	}
	return &ast.GraphPathSearchPrefixCount{Span: span(prefixStart, expr.End()), PathCount: expr}, nil
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

// parseGraphPathFactor parses a graph path primary optionally followed by a
// quantifier; see graph_path_factor / graph_quantified_path_primary in
// googlesql.tm.
func (p *parser) parseGraphPathFactor() (ast.Node, error) {
	primary, err := p.parseGraphPathPrimary()
	if err != nil {
		return nil, err
	}
	if !p.startsGraphQuantifier() {
		return primary, nil
	}
	if _, ok := primary.(*ast.GraphNodePattern); ok {
		return nil, p.errorf(primary.Pos(), "Quantifier cannot be used on a node pattern")
	}
	quant, err := p.parseGraphQuantifier()
	if err != nil {
		return nil, err
	}
	// @$ of graph_quantified_path_primary spans every consumed token, including
	// a trailing "}" that is not part of the FixedQuantifier node's location.
	quantEnd := p.prevEnd()
	var container *ast.GraphPathPattern
	if edge, ok := primary.(*ast.GraphEdgePattern); ok {
		container = &ast.GraphPathPattern{Factors: []ast.Node{edge}, Parenthesized: true}
	} else {
		container = primary.(*ast.GraphPathPattern)
	}
	// The quantifier is prepended leftmost; see ExtendNodeLeft in
	// graph_quantified_path_primary in googlesql.tm.
	prependGraphPathFactors(container, quant)
	container.Span = span(primary.Pos(), quantEnd)
	return container, nil
}

// prependGraphPathFactors prepends decorations to the front of a path
// pattern's child order. Children() emits the PathName and SearchPrefix fields
// before the factor list, so any such fields are demoted into the factor list
// (after the new decorations) to keep the decorations leftmost, matching the
// ExtendNodeLeft prepends in the graph pattern productions in googlesql.tm.
func prependGraphPathFactors(pattern *ast.GraphPathPattern, decorations ...ast.Node) {
	prefix := append([]ast.Node(nil), decorations...)
	if pattern.PathName != nil {
		prefix = append(prefix, pattern.PathName)
		pattern.PathName = nil
	}
	if pattern.SearchPrefix != nil {
		prefix = append(prefix, pattern.SearchPrefix)
		pattern.SearchPrefix = nil
	}
	pattern.Factors = append(prefix, pattern.Factors...)
}

// startsGraphQuantifier reports whether the next token begins a graph
// quantifier ("{", "+", or "*").
func (p *parser) startsGraphQuantifier() bool {
	switch p.peek().Kind {
	case token.LBRACE, token.PLUS, token.STAR:
		return true
	}
	return false
}

// parseGraphPathPrimary parses a node pattern, an edge pattern, or a
// parenthesized subpath; see graph_path_primary in googlesql.tm.
func (p *parser) parseGraphPathPrimary() (ast.Node, error) {
	if p.peek().Kind != token.LPAREN {
		return p.parseGraphEdgePattern()
	}
	if p.parenStartsNodePattern() {
		return p.parseGraphNodePattern()
	}
	return p.parseGraphParenthesizedPathPattern()
}

// parenStartsNodePattern reports whether a "(" begins a node pattern (as
// opposed to a parenthesized subpath). The interior of a node pattern is an
// element filler, which never begins with a path factor, a path mode keyword,
// or a "identifier =" path-variable assignment.
func (p *parser) parenStartsNodePattern() bool {
	t1 := p.peekAt(1)
	switch t1.Kind {
	case token.LPAREN, token.MINUS, token.LT, token.ARROW:
		return false
	}
	if isGraphPathModeKeyword(t1) {
		return false
	}
	if ((t1.Kind == token.IDENT && !p.isReserved(t1)) || t1.Kind == token.QUOTED_IDENT) && p.peekAt(2).Kind == token.EQ {
		return false
	}
	// A parenthesized pattern whose content begins with a path search prefix
	// (ANY / ALL / SHORTEST k / CHEAPEST k) is a subpath, not a node pattern
	// (a filler never begins with a search prefix).
	if tokensStartGraphSearchPrefix(t1, p.peekAt(2)) {
		return false
	}
	return true
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

// parseGraphParenthesizedPathPattern parses "( hint? path_pattern where? )";
// see graph_parenthesized_path_pattern in googlesql.tm.
func (p *parser) parseGraphParenthesizedPathPattern() (*ast.GraphPathPattern, error) {
	lparen := p.advance() // (
	if p.peek().Kind == token.ATSIGN {
		return nil, p.errorf(p.peek().Pos, "Hint cannot be used at beginning of path pattern")
	}
	inner, err := p.parseGraphPathPattern()
	if err != nil {
		return nil, err
	}
	var where *ast.WhereClause
	if isKeyword(p.peek(), "WHERE") {
		where, err = p.parseWhereClause()
		if err != nil {
			return nil, err
		}
	}
	if p.peek().Kind != token.RPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected ")" but got %s`, describeToken(p.peek()))
	}
	rparen := p.advance() // )
	if where != nil {
		ret := inner
		if inner.Parenthesized {
			ret = &ast.GraphPathPattern{Factors: []ast.Node{inner}}
		}
		ret.Parenthesized = true
		prependGraphPathFactors(ret, where)
		ret.Span = span(lparen.Pos, rparen.End)
		return ret, nil
	}
	inner.Parenthesized = true
	inner.Span = span(lparen.Pos, rparen.End)
	return inner, nil
}

// parseGraphPathMode parses a "WALK"/"TRAIL"/"SIMPLE"/"ACYCLIC" path mode
// keyword; see graph_path_mode in googlesql.tm.
func (p *parser) parseGraphPathMode() *ast.GraphPathMode {
	tok := p.advance()
	return &ast.GraphPathMode{Span: span(tok.Pos, tok.End), Mode: strings.ToUpper(tok.Image)}
}

// isGraphPathModeKeyword reports whether tok is a graph path mode keyword.
func isGraphPathModeKeyword(tok token.Token) bool {
	return isKeyword(tok, "WALK") || isKeyword(tok, "TRAIL") ||
		isKeyword(tok, "SIMPLE") || isKeyword(tok, "ACYCLIC")
}

// parseGraphQuantifier parses "{lo,hi}", "{n}", "+", or "*"; see
// graph_quantifier in googlesql.tm. Unlike the row-pattern quantifier, each
// bound node's location covers only the bound itself.
func (p *parser) parseGraphQuantifier() (ast.Node, error) {
	tok := p.peek()
	switch tok.Kind {
	case token.PLUS, token.STAR:
		p.advance()
		symbol := "PLUS"
		if tok.Kind == token.STAR {
			symbol = "STAR"
		}
		return &ast.SymbolQuantifier{Span: span(tok.Pos, tok.End), Symbol: symbol}, nil
	}
	lbrace := p.advance() // {
	afterBrace := p.prevEnd()
	lower, err := p.parseOptionalQuantifierBound()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind == token.COMMA {
		p.advance() // ,
		afterComma := p.prevEnd()
		upper, err := p.parseOptionalQuantifierBound()
		if err != nil {
			return nil, err
		}
		if p.peek().Kind != token.RBRACE {
			return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "}" but got %s`, describeToken(p.peek()))
		}
		rbrace := p.advance() // }
		return &ast.BoundedQuantifier{
			Span:       span(lbrace.Pos, rbrace.End),
			LowerBound: graphQuantifierBound(lower, afterBrace),
			UpperBound: graphQuantifierBound(upper, afterComma),
		}, nil
	}
	// Not a comma: must be the fixed "{ n }" form.
	if lower != nil && p.peek().Kind == token.RBRACE {
		p.advance() // }
		return &ast.FixedQuantifier{Span: span(lower.Pos(), lower.End()), Bound: lower}, nil
	}
	return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "," but got %s`, describeToken(p.peek()))
}

// graphQuantifierBound builds an ASTQuantifierBound whose location covers only
// the bound expression, or is an empty point at emptyPos when the bound is
// omitted.
func graphQuantifierBound(bound ast.Node, emptyPos int) *ast.QuantifierBound {
	if bound == nil {
		return &ast.QuantifierBound{Span: span(emptyPos, emptyPos)}
	}
	return &ast.QuantifierBound{Span: span(bound.Pos(), bound.End()), Bound: bound}
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
		lt := p.advance() // <
		if p.peek().Kind != token.MINUS {
			return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "-" but got %s`, describeToken(p.peek()))
		}
		minus := p.advance() // -
		// Edge pattern delimiters are multi-tokens: no whitespace allowed.
		if err := p.validateNoWhitespace(lt, "<", minus, "-"); err != nil {
			return nil, err
		}
		if p.peek().Kind == token.LBRACKET {
			lbracket := p.peek()
			if err := p.validateNoWhitespace(minus, "-", lbracket, "["); err != nil {
				return nil, err
			}
			filler, endTok, err := p.parseGraphBracketedFiller(true, token.MINUS)
			if err != nil {
				return nil, err
			}
			return &ast.GraphEdgePattern{Span: span(start.Pos, endTok.End), Filler: filler, Orientation: "LEFT"}, nil
		}
		return &ast.GraphEdgePattern{Span: span(start.Pos, minus.End), Orientation: "LEFT"}, nil
	case token.MINUS: // - or -[...]- or -[...]->
		minus := p.advance() // -
		if p.peek().Kind == token.LBRACKET {
			lbracket := p.peek()
			if err := p.validateNoWhitespace(minus, "-", lbracket, "["); err != nil {
				return nil, err
			}
			filler, endTok, err := p.parseGraphBracketedFiller(false, token.MINUS, token.ARROW)
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

// validateNoWhitespace reports a "Unexpected whitespace between ..." error at
// the left token's position when the two tokens are not directly adjacent; see
// ValidateNoWhitespace in googlesql/parser/parser_internal.cc.
func (p *parser) validateNoWhitespace(left token.Token, leftImage string, right token.Token, rightImage string) error {
	if left.End != right.Pos {
		return p.errorf(left.Pos, `Syntax error: Unexpected whitespace between "%s" and "%s"`, leftImage, rightImage)
	}
	return nil
}

// parseGraphBracketedFiller parses "[ <filler> ]" followed by one of the given
// closing token kinds, returning the filler and the closing token. The closing
// bracket must be directly adjacent to the closing token (no whitespace). When
// leftOnly is set (the "<-[...]-" form), a non-"-" closer yields the reference
// "Expected \"-\"" error.
func (p *parser) parseGraphBracketedFiller(leftOnly bool, closers ...token.Kind) (*ast.GraphElementPatternFiller, token.Token, error) {
	p.advance() // [
	filler, err := p.parseGraphElementPatternFiller()
	if err != nil {
		return nil, token.Token{}, err
	}
	if p.peek().Kind != token.RBRACKET {
		return nil, token.Token{}, p.errorf(p.peek().Pos, `Syntax error: Expected "]" but got %s`, describeToken(p.peek()))
	}
	rbracket := p.advance() // ]
	for _, k := range closers {
		if p.peek().Kind == k {
			closer := p.peek()
			img := "-"
			if k == token.ARROW {
				img = "->"
			}
			if err := p.validateNoWhitespace(rbracket, "]", closer, img); err != nil {
				return nil, token.Token{}, err
			}
			return filler, p.advance(), nil
		}
	}
	if leftOnly {
		return nil, token.Token{}, p.errorf(p.peek().Pos, `Syntax error: Expected "-" but got %s`, describeToken(p.peek()))
	}
	return nil, token.Token{}, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
}

// parseGraphElementPatternFiller parses the optional variable name, label
// filter, and WHERE clause inside a node or edge pattern; see
// graph_element_pattern_filler in googlesql.tm. Property specifications, cost,
// and hints are not yet supported.
func (p *parser) parseGraphElementPatternFiller() (*ast.GraphElementPatternFiller, error) {
	startPos := p.peek().Pos
	var hint *ast.Hint
	var name *ast.Identifier
	var label *ast.GraphLabelFilter
	var propSpec *ast.GraphPropertySpecification
	var where *ast.WhereClause
	end := startPos

	if p.peek().Kind == token.ATSIGN {
		h, err := p.parseOptionalHint()
		if err != nil {
			return nil, err
		}
		hint = h
		end = hint.End()
	}
	if t := p.peek(); (t.Kind == token.IDENT && !p.isReserved(t)) || t.Kind == token.QUOTED_IDENT {
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
	if p.peek().Kind == token.LBRACE {
		ps, err := p.parseGraphPropertySpecification()
		if err != nil {
			return nil, err
		}
		propSpec = ps
		end = ps.End()
	}
	if isKeyword(p.peek(), "WHERE") {
		w, err := p.parseWhereClause()
		if err != nil {
			return nil, err
		}
		if propSpec != nil {
			return nil, p.errorf(w.Pos(), "WHERE clause cannot be used together with property specification")
		}
		where = w
		end = w.End()
	}
	// opt_graph_cost: "COST" expression. The element name (an unreserved
	// keyword "cost") is already consumed above, so a remaining COST keyword
	// here introduces the cost expression. The filler location (@$) extends to
	// the last consumed token, which may be past the cost expression's own end
	// (e.g. a trailing ")" of a parenthesized cost).
	var cost ast.Node
	if isKeyword(p.peek(), "COST") {
		p.advance() // COST
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		cost = expr
		end = p.prevEnd()
	}
	return &ast.GraphElementPatternFiller{Span: span(startPos, end), Name: name, Label: label, PropSpec: propSpec, Where: where, Hint: hint, Cost: cost}, nil
}

// parseGraphPropertySpecification parses "{ name: value, ... }"; see
// graph_property_specification in googlesql.tm.
func (p *parser) parseGraphPropertySpecification() (*ast.GraphPropertySpecification, error) {
	lbrace := p.advance() // {
	first, err := p.parseGraphPropertyNameAndValue()
	if err != nil {
		return nil, err
	}
	props := []*ast.GraphPropertyNameAndValue{first}
	for p.peek().Kind == token.COMMA {
		p.advance() // ,
		prop, err := p.parseGraphPropertyNameAndValue()
		if err != nil {
			return nil, err
		}
		props = append(props, prop)
	}
	if p.peek().Kind != token.RBRACE {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "}" but got %s`, describeToken(p.peek()))
	}
	rbrace := p.advance() // }
	return &ast.GraphPropertySpecification{Span: span(lbrace.Pos, rbrace.End), Properties: props}, nil
}

func (p *parser) parseGraphPropertyNameAndValue() (*ast.GraphPropertyNameAndValue, error) {
	name, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != token.COLON {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected ":" but got %s`, describeToken(p.peek()))
	}
	p.advance() // :
	value, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	return &ast.GraphPropertyNameAndValue{Span: span(name.Pos(), p.extEnd(value)), Name: name, Value: value}, nil
}

// parseGraphLabelFilter parses "( IS | : ) <label_expression>"; see
// is_label_expression in googlesql.tm.
func (p *parser) parseGraphLabelFilter() (*ast.GraphLabelFilter, error) {
	tok := p.advance() // IS or :
	expr, err := p.parseGraphLabelOr()
	if err != nil {
		return nil, err
	}
	// The is_label_expression location (@$) spans every consumed token,
	// including a trailing ")" of a parenthesized label expression, even though
	// the label expression node's own location excludes the parentheses.
	return &ast.GraphLabelFilter{Span: span(tok.Pos, p.prevEnd()), Expr: expr}, nil
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
		left = combineGraphLabelOperation("OR", left, right, p.prevEnd())
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
		left = combineGraphLabelOperation("AND", left, right, p.prevEnd())
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
		// @$ spans the operand's tokens including a trailing ")".
		return &ast.GraphLabelOperation{
			Span:     span(notTok.Pos, p.prevEnd()),
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
	case p.peek().Kind == token.IDENT && !p.isReserved(p.peek()), p.peek().Kind == token.QUOTED_IDENT:
		ident := p.parseIdentifierToken(p.advance())
		return &ast.GraphElementLabel{Span: span(ident.Pos(), ident.End()), Name: ident}, nil
	}
	return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
}

// combineGraphLabelOperation combines left and right under op, flattening into
// left when it is an unparenthesized operation of the same op; see
// MakeOrCombineGraphLabelOperation in parser_internal.h.
func combineGraphLabelOperation(op string, left, right ast.Node, end int) ast.Node {
	// end is @$.end(), the end of the last consumed token, which includes a
	// trailing ")" when the right operand is parenthesized.
	if lo, ok := left.(*ast.GraphLabelOperation); ok && lo.Op == op && !lo.Parenthesized {
		lo.Operands = append(lo.Operands, right)
		lo.Stop = end
		return lo
	}
	return &ast.GraphLabelOperation{
		Span:     span(left.Pos(), end),
		Op:       op,
		Operands: []ast.Node{left, right},
	}
}
