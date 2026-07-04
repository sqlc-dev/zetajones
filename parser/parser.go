// Package parser implements a hand-written recursive descent parser for
// GoogleSQL, producing an AST that matches the parse tree of
// github.com/google/googlesql.
package parser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/sqlc-dev/zetajones/ast"
	"github.com/sqlc-dev/zetajones/lexer"
	"github.com/sqlc-dev/zetajones/token"
)

// Error is a parse error with a byte offset into the input.
type Error struct {
	Message string // e.g. "Syntax error: Unexpected end of statement"
	Offset  int    // byte offset of the error
	SQL     string
}

func (e *Error) Error() string {
	line, col := e.LineCol()
	return fmt.Sprintf("%s [at %d:%d]", e.Message, line, col)
}

// LineCol returns the 1-based line and column of the error.
func (e *Error) LineCol() (line, col int) {
	line, col = 1, 1
	for i := 0; i < e.Offset && i < len(e.SQL); i++ {
		if e.SQL[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

// Caret renders the error in ZetaSQL's test format: the message with
// location, the offending source line, and a caret marking the column.
func (e *Error) Caret() string {
	line, col := e.LineCol()
	var srcLine string
	if lines := strings.Split(e.SQL, "\n"); line-1 < len(lines) {
		srcLine = lines[line-1]
	}
	return fmt.Sprintf("%s [at %d:%d]\n%s\n%s^", e.Message, line, col, srcLine, strings.Repeat(" ", col-1))
}

// Parse reads SQL from r and parses it as a single statement.
func Parse(ctx context.Context, r io.Reader) ([]ast.Statement, error) {
	sql, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	stmt, err := ParseStatement(string(sql))
	if err != nil {
		return nil, err
	}
	return []ast.Statement{stmt}, nil
}

// ParseStatement parses a single SQL statement, allowing an optional
// trailing semicolon.
func ParseStatement(sql string) (ast.Statement, error) {
	toks, err := lexer.Lex(sql)
	if err != nil {
		var lerr *lexer.Error
		if errors.As(err, &lerr) {
			return nil, &Error{Message: lerr.Message, Offset: lerr.Offset, SQL: sql}
		}
		return nil, err
	}
	p := &parser{sql: sql, toks: toks}
	stmt, err := p.parseStatement()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind == token.SEMICOLON {
		p.advance()
	}
	if p.peek().Kind != token.EOF {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected end of input but got %s", describeToken(p.peek()))
	}
	return stmt, nil
}

type parser struct {
	sql  string
	toks []token.Token
	pos  int
}

func (p *parser) peek() token.Token { return p.toks[p.pos] }
func (p *parser) peekAt(n int) token.Token {
	if p.pos+n < len(p.toks) {
		return p.toks[p.pos+n]
	}
	return p.toks[len(p.toks)-1]
}
func (p *parser) advance() token.Token {
	tok := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return tok
}

func (p *parser) errorf(offset int, format string, args ...any) error {
	return &Error{Message: fmt.Sprintf(format, args...), Offset: offset, SQL: p.sql}
}

// describeToken renders a token for an error message the same way the
// reference implementation does; see MakeSyntaxErrorAtToken in
// googlesql/parser/parser_internal.cc.
func describeToken(tok token.Token) string {
	switch tok.Kind {
	case token.EOF:
		return "end of statement"
	case token.IDENT:
		if keywordNames[strings.ToLower(tok.Image)] {
			return "keyword " + strings.ToUpper(tok.Image)
		}
		return fmt.Sprintf("identifier \"%s\"", tok.Image)
	case token.QUOTED_IDENT:
		// Don't put extra quotes around an already-backquoted identifier.
		return "identifier " + tok.Image
	case token.INT:
		return fmt.Sprintf("integer literal \"%s\"", tok.Image)
	case token.FLOAT:
		return fmt.Sprintf("floating point literal \"%s\"", tok.Image)
	case token.STRING:
		return "string literal " + escapeTokenNewlines(tok.Image)
	case token.BYTES:
		return "bytes literal " + escapeTokenNewlines(tok.Image)
	}
	return fmt.Sprintf("%q", tok.Image)
}

// escapeTokenNewlines escapes physical newlines to avoid multi-line error
// messages, matching the reference implementation.
func escapeTokenNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r", `\r`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

// isKeyword reports whether tok is the given keyword (case-insensitive).
func isKeyword(tok token.Token, kw string) bool {
	return tok.Kind == token.IDENT && strings.EqualFold(tok.Image, kw)
}

// reservedKeywords is the subset of GoogleSQL reserved keywords the parser
// currently needs to recognize to know where expressions and clauses end.
// See googlesql/parser/keywords.cc for the full list.
var reservedKeywords = map[string]bool{
	"ALL": true, "AND": true, "AS": true, "ASC": true, "BETWEEN": true,
	"BY": true, "CASE": true, "CROSS": true, "DESC": true, "DISTINCT": true,
	"ELSE": true, "END": true, "EXCEPT": true, "FALSE": true, "FROM": true,
	"FULL": true, "GROUP": true, "HAVING": true, "IN": true, "INNER": true,
	"INTERSECT": true, "IS": true, "JOIN": true, "LEFT": true, "LIKE": true,
	"LIMIT": true, "NOT": true, "NULL": true, "ON": true, "OR": true,
	"ORDER": true, "OUTER": true, "RIGHT": true, "SELECT": true, "SET": true,
	"TRUE": true, "UNION": true, "USING": true, "WHERE": true, "WITH": true,
}

func isReserved(tok token.Token) bool {
	return tok.Kind == token.IDENT && reservedKeywords[strings.ToUpper(tok.Image)]
}

// expectKeyword consumes the given keyword or returns an error.
func (p *parser) expectKeyword(kw string) (token.Token, error) {
	if !isKeyword(p.peek(), kw) {
		return token.Token{}, p.errorf(p.peek().Pos, "Syntax error: Expected keyword %s but got %s", kw, describeToken(p.peek()))
	}
	return p.advance(), nil
}

func (p *parser) expect(kind token.Kind, what string) (token.Token, error) {
	if p.peek().Kind != kind {
		return token.Token{}, p.errorf(p.peek().Pos, "Syntax error: Expected %s but got %s", what, describeToken(p.peek()))
	}
	return p.advance(), nil
}

func span(start, end int) ast.Span { return ast.Span{Start: start, Stop: end} }

func (p *parser) parseStatement() (ast.Statement, error) {
	tok := p.peek()
	switch {
	case isKeyword(tok, "SELECT") || tok.Kind == token.LPAREN:
		query, err := p.parseQuery()
		if err != nil {
			return nil, err
		}
		return &ast.QueryStatement{Span: span(query.Pos(), query.End()), Query: query}, nil
	}
	return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
}

func (p *parser) parseQuery() (*ast.Query, error) {
	sel, err := p.parseSelect()
	if err != nil {
		return nil, err
	}
	query := &ast.Query{Span: span(sel.Pos(), sel.End()), QueryExpr: sel}

	if isKeyword(p.peek(), "ORDER") {
		orderBy, err := p.parseOrderBy()
		if err != nil {
			return nil, err
		}
		query.OrderBy = orderBy
		query.Stop = orderBy.End()
	}
	if isKeyword(p.peek(), "LIMIT") {
		limit, err := p.parseLimitOffset()
		if err != nil {
			return nil, err
		}
		query.Limit = limit
		query.Stop = limit.End()
	}
	for p.peek().Kind == token.PIPE_INPUT {
		op, err := p.parsePipeOperator()
		if err != nil {
			return nil, err
		}
		query.PipeOperators = append(query.PipeOperators, op)
		query.Stop = op.End()
	}
	return query, nil
}

// parsePipeOperator parses one "|> <operator>" pipe operator.
func (p *parser) parsePipeOperator() (ast.Node, error) {
	pipeTok := p.advance() // |>
	tok := p.peek()
	switch {
	case isKeyword(tok, "WHERE"):
		where, err := p.parseWhereClause()
		if err != nil {
			return nil, err
		}
		return &ast.PipeWhere{Span: span(pipeTok.Pos, where.End()), Where: where}, nil
	case isKeyword(tok, "ORDER"):
		orderBy, err := p.parseOrderBy()
		if err != nil {
			return nil, err
		}
		node := &ast.PipeOrderBy{Span: span(pipeTok.Pos, orderBy.End()), OrderBy: orderBy}
		// Pipe ORDER BY allows a trailing comma.
		if p.peek().Kind == token.COMMA {
			comma := p.advance()
			node.Stop = comma.End
		}
		return node, nil
	case isKeyword(tok, "SET"):
		return p.parsePipeSet(pipeTok)
	}
	// The reference grammar's recovery point for an unrecognized pipe
	// operator is the JOIN inside pipe_join.
	return nil, p.errorf(tok.Pos, "Syntax error: Expected keyword JOIN but got %s", describeToken(tok))
}

// parsePipeSet parses "SET column = expression, ..." after a |> token,
// including an optional trailing comma.
func (p *parser) parsePipeSet(pipeTok token.Token) (ast.Node, error) {
	p.advance() // SET
	node := &ast.PipeSet{Span: span(pipeTok.Pos, 0)}
	for {
		tok := p.peek()
		if (tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT) || isReserved(tok) {
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
		}
		ident := p.parseIdentifierToken(p.advance())
		if p.peek().Kind == token.DOT {
			return nil, p.errorf(ident.Pos(), "Syntax error: Pipe SET can only update columns by column name alone; Setting columns under table aliases or fields under paths is not supported")
		}
		if p.peek().Kind != token.EQ {
			return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "." or "=" but got %s`, describeToken(p.peek()))
		}
		p.advance() // =
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		item := &ast.PipeSetItem{Span: span(ident.Pos(), expr.End()), Column: ident, Expr: expr}
		node.Items = append(node.Items, item)
		node.Stop = item.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		comma := p.advance()
		next := p.peek()
		if (next.Kind != token.IDENT && next.Kind != token.QUOTED_IDENT) || isReserved(next) {
			// Trailing comma; it is included in the operator's location.
			node.Stop = comma.End
			break
		}
	}
	return node, nil
}

// parseWhereClause parses "WHERE expression"; the WHERE keyword is included
// in the clause's location.
func (p *parser) parseWhereClause() (*ast.WhereClause, error) {
	whereTok, err := p.expectKeyword("WHERE")
	if err != nil {
		return nil, err
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	return &ast.WhereClause{Span: span(whereTok.Pos, expr.End()), Expr: expr}, nil
}

func (p *parser) parseSelect() (*ast.Select, error) {
	selectTok, err := p.expectKeyword("SELECT")
	if err != nil {
		return nil, err
	}
	sel := &ast.Select{Span: span(selectTok.Pos, selectTok.End)}

	if isKeyword(p.peek(), "DISTINCT") {
		p.advance()
		sel.Distinct = true
	} else if isKeyword(p.peek(), "ALL") {
		p.advance()
	}

	list, err := p.parseSelectList()
	if err != nil {
		return nil, err
	}
	sel.SelectList = list
	sel.Stop = list.End()

	if isKeyword(p.peek(), "FROM") {
		from, err := p.parseFromClause()
		if err != nil {
			return nil, err
		}
		sel.From = from
		sel.Stop = from.End()
	}
	if isKeyword(p.peek(), "WHERE") {
		where, err := p.parseWhereClause()
		if err != nil {
			return nil, err
		}
		sel.Where = where
		sel.Stop = where.End()
	}
	if isKeyword(p.peek(), "GROUP") {
		groupBy, err := p.parseGroupBy()
		if err != nil {
			return nil, err
		}
		sel.GroupBy = groupBy
		sel.Stop = groupBy.End()
	}
	if isKeyword(p.peek(), "HAVING") {
		p.advance()
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		sel.Having = &ast.Having{Span: span(expr.Pos(), expr.End()), Expr: expr}
		sel.Stop = expr.End()
	}
	return sel, nil
}

func (p *parser) parseSelectList() (*ast.SelectList, error) {
	var cols []*ast.SelectColumn
	for {
		col, err := p.parseSelectColumn()
		if err != nil {
			return nil, err
		}
		cols = append(cols, col)
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance()
	}
	return &ast.SelectList{
		Span:    span(cols[0].Pos(), cols[len(cols)-1].End()),
		Columns: cols,
	}, nil
}

func (p *parser) parseSelectColumn() (*ast.SelectColumn, error) {
	var expr ast.Node
	var err error
	if p.peek().Kind == token.STAR {
		star := p.advance()
		expr = &ast.Star{Span: span(star.Pos, star.End), Image: star.Image}
	} else {
		expr, err = p.parseExpression()
		if err != nil {
			return nil, err
		}
	}
	col := &ast.SelectColumn{Span: span(expr.Pos(), expr.End()), Expr: expr}
	alias, err := p.parseOptionalAlias()
	if err != nil {
		return nil, err
	}
	if alias != nil {
		col.Alias = alias
		col.Stop = alias.End()
	}
	return col, nil
}

// parseOptionalAlias parses [AS] identifier if present.
func (p *parser) parseOptionalAlias() (*ast.Alias, error) {
	start := p.peek().Pos
	hasAs := false
	if isKeyword(p.peek(), "AS") {
		p.advance()
		hasAs = true
	}
	tok := p.peek()
	if tok.Kind == token.QUOTED_IDENT || (tok.Kind == token.IDENT && !isReserved(tok)) {
		ident := p.parseIdentifierToken(p.advance())
		return &ast.Alias{Span: span(start, ident.End()), Identifier: ident}, nil
	}
	if hasAs {
		return nil, p.errorf(tok.Pos, "Syntax error: Expected identifier but got %s", describeToken(tok))
	}
	return nil, nil
}

func (p *parser) parseIdentifierToken(tok token.Token) *ast.Identifier {
	name := tok.Image
	if tok.Kind == token.QUOTED_IDENT {
		name = unquoteBackquoted(tok.Image)
	}
	return &ast.Identifier{Span: span(tok.Pos, tok.End), Name: name}
}

func unquoteBackquoted(image string) string {
	s := strings.TrimPrefix(strings.TrimSuffix(image, "`"), "`")
	s = strings.ReplaceAll(s, "\\`", "`")
	return s
}

func (p *parser) parseFromClause() (*ast.FromClause, error) {
	fromTok, err := p.expectKeyword("FROM")
	if err != nil {
		return nil, err
	}
	table, err := p.parseTablePathExpression()
	if err != nil {
		return nil, err
	}
	return &ast.FromClause{Span: span(fromTok.Pos, table.End()), TableExpression: table}, nil
}

func (p *parser) parseTablePathExpression() (*ast.TablePathExpression, error) {
	path, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	table := &ast.TablePathExpression{Span: span(path.Pos(), path.End()), Path: path}
	alias, err := p.parseOptionalAlias()
	if err != nil {
		return nil, err
	}
	if alias != nil {
		table.Alias = alias
		table.Stop = alias.End()
	}
	return table, nil
}

func (p *parser) parseGroupBy() (*ast.GroupBy, error) {
	groupTok, err := p.expectKeyword("GROUP")
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("BY"); err != nil {
		return nil, err
	}
	groupBy := &ast.GroupBy{Span: span(groupTok.Pos, groupTok.End)}
	for {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		item := &ast.GroupingItem{Span: span(expr.Pos(), expr.End()), Expr: expr}
		groupBy.Items = append(groupBy.Items, item)
		groupBy.Stop = item.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance()
	}
	return groupBy, nil
}

func (p *parser) parseOrderBy() (*ast.OrderBy, error) {
	orderTok, err := p.expectKeyword("ORDER")
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("BY"); err != nil {
		return nil, err
	}
	orderBy := &ast.OrderBy{Span: span(orderTok.Pos, orderTok.End)}
	for {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		item := &ast.OrderingExpression{Span: span(expr.Pos(), expr.End()), Expr: expr}
		if isKeyword(p.peek(), "ASC") {
			tok := p.advance()
			item.HasAsc = true
			item.Stop = tok.End
		} else if isKeyword(p.peek(), "DESC") {
			tok := p.advance()
			item.Descending = true
			item.Stop = tok.End
		}
		orderBy.Items = append(orderBy.Items, item)
		orderBy.Stop = item.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance()
	}
	return orderBy, nil
}

func (p *parser) parseLimitOffset() (*ast.LimitOffset, error) {
	limitTok, err := p.expectKeyword("LIMIT")
	if err != nil {
		return nil, err
	}
	limit, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	node := &ast.LimitOffset{Span: span(limitTok.Pos, limit.End()), Limit: limit}
	if isKeyword(p.peek(), "OFFSET") {
		p.advance()
		offset, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		node.Offset = offset
		node.Stop = offset.End()
	}
	return node, nil
}

// Expression parsing. Precedence, from lowest to highest binding:
//
//	OR
//	AND
//	NOT (unary)
//	comparison: = != <> < > <= >= [NOT] BETWEEN, [NOT] LIKE, [NOT] IN, IS [NOT]
//	|
//	^
//	&
//	<< >>
//	+ -
//	* / ||
//	unary - ~ +
//	primary
func (p *parser) parseExpression() (ast.Node, error) {
	return p.parseOr()
}

func (p *parser) parseOr() (ast.Node, error) {
	first, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	if !isKeyword(p.peek(), "OR") {
		return first, nil
	}
	disjuncts := []ast.Node{first}
	for isKeyword(p.peek(), "OR") {
		p.advance()
		next, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		disjuncts = append(disjuncts, next)
	}
	return &ast.OrExpr{
		Span:      span(first.Pos(), disjuncts[len(disjuncts)-1].End()),
		Disjuncts: disjuncts,
	}, nil
}

func (p *parser) parseAnd() (ast.Node, error) {
	first, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	if !isKeyword(p.peek(), "AND") {
		return first, nil
	}
	conjuncts := []ast.Node{first}
	for isKeyword(p.peek(), "AND") {
		p.advance()
		next, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		conjuncts = append(conjuncts, next)
	}
	return &ast.AndExpr{
		Span:      span(first.Pos(), conjuncts[len(conjuncts)-1].End()),
		Conjuncts: conjuncts,
	}, nil
}

func (p *parser) parseNot() (ast.Node, error) {
	if isKeyword(p.peek(), "NOT") {
		notTok := p.advance()
		operand, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &ast.UnaryExpression{
			Span:    span(notTok.Pos, operand.End()),
			Op:      "NOT",
			Operand: operand,
		}, nil
	}
	return p.parseComparison()
}

func (p *parser) parseComparison() (ast.Node, error) {
	lhs, err := p.parseBitwiseOr()
	if err != nil {
		return nil, err
	}

	// [NOT] BETWEEN
	notTok := token.Token{Pos: -1}
	if isKeyword(p.peek(), "NOT") && isKeyword(p.peekAt(1), "BETWEEN") {
		notTok = p.advance()
	}
	if isKeyword(p.peek(), "BETWEEN") {
		betweenTok := p.advance()
		low, err := p.parseBitwiseOr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expectKeyword("AND"); err != nil {
			return nil, err
		}
		high, err := p.parseBitwiseOr()
		if err != nil {
			return nil, err
		}
		if isKeyword(p.peek(), "BETWEEN") {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expression in BETWEEN must be parenthesized")
		}
		return &ast.BetweenExpression{
			Span:            span(lhs.Pos(), high.End()),
			IsNot:           notTok.Pos >= 0,
			Lhs:             lhs,
			BetweenLocation: &ast.Location{Span: span(betweenTok.Pos, betweenTok.End)},
			Low:             low,
			High:            high,
		}, nil
	}
	if notTok.Pos >= 0 {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected BETWEEN")
	}

	// IS [NOT] NULL / TRUE / FALSE
	if isKeyword(p.peek(), "IS") {
		p.advance()
		isNot := false
		if isKeyword(p.peek(), "NOT") {
			p.advance()
			isNot = true
		}
		tok := p.peek()
		var rhs ast.Node
		switch {
		case isKeyword(tok, "NULL"):
			p.advance()
			rhs = &ast.NullLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image}
		case isKeyword(tok, "TRUE"), isKeyword(tok, "FALSE"):
			p.advance()
			rhs = &ast.BooleanLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image, Value: isKeyword(tok, "TRUE")}
		default:
			return nil, p.errorf(tok.Pos, "Syntax error: Expected NULL, TRUE, or FALSE after IS")
		}
		return &ast.BinaryExpression{
			Span:  span(lhs.Pos(), rhs.End()),
			Op:    "IS",
			IsNot: isNot,
			Left:  lhs,
			Right: rhs,
		}, nil
	}

	// Simple comparison operators.
	var op string
	switch p.peek().Kind {
	case token.EQ:
		op = "="
	case token.NEQ:
		op = p.peek().Image
	case token.LT:
		op = "<"
	case token.GT:
		op = ">"
	case token.LTE:
		op = "<="
	case token.GTE:
		op = ">="
	default:
		return lhs, nil
	}
	p.advance()
	rhs, err := p.parseBitwiseOr()
	if err != nil {
		return nil, err
	}
	return &ast.BinaryExpression{
		Span:  span(lhs.Pos(), rhs.End()),
		Op:    op,
		Left:  lhs,
		Right: rhs,
	}, nil
}

// parseBinaryLevel parses a left-associative binary operator level.
func (p *parser) parseBinaryLevel(matches func(token.Token) (string, bool), next func() (ast.Node, error)) (ast.Node, error) {
	lhs, err := next()
	if err != nil {
		return nil, err
	}
	for {
		op, ok := matches(p.peek())
		if !ok {
			return lhs, nil
		}
		p.advance()
		rhs, err := next()
		if err != nil {
			return nil, err
		}
		lhs = &ast.BinaryExpression{
			Span: span(lhs.Pos(), rhs.End()),
			Op:   op,
			Left: lhs, Right: rhs,
		}
	}
}

func (p *parser) parseBitwiseOr() (ast.Node, error) {
	return p.parseBinaryLevel(func(tok token.Token) (string, bool) {
		if tok.Kind == token.PIPE {
			return "|", true
		}
		return "", false
	}, p.parseBitwiseXor)
}

func (p *parser) parseBitwiseXor() (ast.Node, error) {
	return p.parseBinaryLevel(func(tok token.Token) (string, bool) {
		if tok.Kind == token.CARET {
			return "^", true
		}
		return "", false
	}, p.parseBitwiseAnd)
}

func (p *parser) parseBitwiseAnd() (ast.Node, error) {
	return p.parseBinaryLevel(func(tok token.Token) (string, bool) {
		if tok.Kind == token.AMP {
			return "&", true
		}
		return "", false
	}, p.parseShift)
}

func (p *parser) parseShift() (ast.Node, error) {
	return p.parseBinaryLevel(func(tok token.Token) (string, bool) {
		switch tok.Kind {
		case token.LSHIFT:
			return "<<", true
		case token.RSHIFT:
			return ">>", true
		}
		return "", false
	}, p.parseAdditive)
}

func (p *parser) parseAdditive() (ast.Node, error) {
	return p.parseBinaryLevel(func(tok token.Token) (string, bool) {
		switch tok.Kind {
		case token.PLUS:
			return "+", true
		case token.MINUS:
			return "-", true
		}
		return "", false
	}, p.parseMultiplicative)
}

func (p *parser) parseMultiplicative() (ast.Node, error) {
	return p.parseBinaryLevel(func(tok token.Token) (string, bool) {
		switch tok.Kind {
		case token.STAR:
			return "*", true
		case token.SLASH:
			return "/", true
		case token.CONCAT:
			return "||", true
		}
		return "", false
	}, p.parseUnary)
}

func (p *parser) parseUnary() (ast.Node, error) {
	tok := p.peek()
	switch tok.Kind {
	case token.MINUS, token.PLUS, token.TILDE:
		p.advance()
		operand, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &ast.UnaryExpression{
			Span:    span(tok.Pos, operand.End()),
			Op:      tok.Image,
			Operand: operand,
		}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (ast.Node, error) {
	tok := p.peek()
	switch tok.Kind {
	case token.INT:
		p.advance()
		return &ast.IntLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image}, nil
	case token.FLOAT:
		p.advance()
		return &ast.FloatLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image}, nil
	case token.STRING:
		return p.parseStringLiteral()
	case token.BYTES:
		return p.parseBytesLiteral()
	case token.LPAREN:
		p.advance()
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(token.RPAREN, `")"`); err != nil {
			return nil, err
		}
		// Parenthesized expressions keep the span of the inner expression in
		// ZetaSQL's parse tree; the parentheses only affect grouping.
		return expr, nil
	case token.IDENT, token.QUOTED_IDENT:
		switch {
		case isKeyword(tok, "NULL"):
			p.advance()
			return &ast.NullLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image}, nil
		case isKeyword(tok, "TRUE"), isKeyword(tok, "FALSE"):
			p.advance()
			return &ast.BooleanLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image, Value: isKeyword(tok, "TRUE")}, nil
		case isReserved(tok):
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected keyword %s", strings.ToUpper(tok.Image))
		}
		return p.parsePathOrCall()
	}
	return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
}

// parsePathOrCall parses a path expression, possibly followed by a function
// call argument list.
func (p *parser) parsePathOrCall() (ast.Node, error) {
	path, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != token.LPAREN {
		return path, nil
	}
	p.advance() // consume (
	call := &ast.FunctionCall{Span: span(path.Pos(), 0), Function: path}
	if isKeyword(p.peek(), "DISTINCT") {
		p.advance()
		call.Distinct = true
	}
	if p.peek().Kind != token.RPAREN {
		for {
			var arg ast.Node
			if p.peek().Kind == token.STAR {
				star := p.advance()
				arg = &ast.Star{Span: span(star.Pos, star.End), Image: star.Image}
			} else {
				arg, err = p.parseExpression()
				if err != nil {
					return nil, err
				}
			}
			call.Args = append(call.Args, arg)
			if p.peek().Kind != token.COMMA {
				break
			}
			p.advance()
		}
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	call.Stop = rparen.End
	return call, nil
}

func (p *parser) parsePathExpression() (*ast.PathExpression, error) {
	tok := p.peek()
	if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
		return nil, p.errorf(tok.Pos, "Syntax error: Expected identifier but got %s", describeToken(tok))
	}
	if isReserved(tok) {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected keyword %s", strings.ToUpper(tok.Image))
	}
	first := p.parseIdentifierToken(p.advance())
	path := &ast.PathExpression{Span: span(first.Pos(), first.End()), Names: []*ast.Identifier{first}}
	for p.peek().Kind == token.DOT {
		p.advance()
		tok := p.peek()
		if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
			return nil, p.errorf(tok.Pos, "Syntax error: Expected identifier after \".\" but got %s", describeToken(tok))
		}
		ident := p.parseIdentifierToken(p.advance())
		path.Names = append(path.Names, ident)
		path.Stop = ident.End()
	}
	return path, nil
}

func (p *parser) parseStringLiteral() (ast.Node, error) {
	tok := p.advance()
	component := &ast.StringLiteralComponent{Span: span(tok.Pos, tok.End), Image: tok.Image}
	lit := &ast.StringLiteral{
		Span:       span(tok.Pos, tok.End),
		Components: []*ast.StringLiteralComponent{component},
	}
	// Adjacent string literals concatenate into one literal with multiple
	// components.
	for p.peek().Kind == token.STRING {
		tok := p.advance()
		component := &ast.StringLiteralComponent{Span: span(tok.Pos, tok.End), Image: tok.Image}
		lit.Components = append(lit.Components, component)
		lit.Stop = tok.End
	}
	return lit, nil
}

func (p *parser) parseBytesLiteral() (ast.Node, error) {
	tok := p.advance()
	component := &ast.BytesLiteralComponent{Span: span(tok.Pos, tok.End), Image: tok.Image}
	lit := &ast.BytesLiteral{
		Span:       span(tok.Pos, tok.End),
		Components: []*ast.BytesLiteralComponent{component},
	}
	for p.peek().Kind == token.BYTES {
		tok := p.advance()
		component := &ast.BytesLiteralComponent{Span: span(tok.Pos, tok.End), Image: tok.Image}
		lit.Components = append(lit.Components, component)
		lit.Stop = tok.End
	}
	return lit, nil
}
