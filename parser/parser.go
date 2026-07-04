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
	"unicode/utf8"

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

// tabWidth is the tab stop width used for column numbers and caret rendering;
// see kTabWidth in googlesql/public/parse_location.cc.
const tabWidth = 8

// LineCol returns the 1-based line and column of the error. Following
// ParseLocationTranslator::GetLineAndColumnFromByteOffset in
// googlesql/public/parse_location.cc, each UTF-8 character counts as one
// column and a tab advances the column to one past the next multiple of
// tabWidth.
func (e *Error) LineCol() (line, col int) {
	lineStart, lineText, line := lineAtOffset(e.SQL, e.Offset)
	col = 1
	for i := 0; i < e.Offset-lineStart && i < len(lineText); {
		if lineText[i] == '\t' {
			col = (col+tabWidth-1)/tabWidth*tabWidth + 1
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(lineText[i:])
		i += size
		col++
	}
	return line, col
}

// lineAtOffset returns the start offset, text (without line terminator) and
// 1-based number of the line containing byte offset. Lines are terminated by
// "\n", "\r" or "\r\n", matching
// ParseLocationTranslator::CalculateLineOffsets.
func lineAtOffset(sql string, offset int) (start int, text string, num int) {
	start, num = 0, 1
	i := 0
	for i < len(sql) {
		c := sql[i]
		if c != '\n' && c != '\r' {
			i++
			continue
		}
		end := i
		i++
		if c == '\r' && i < len(sql) && sql[i] == '\n' {
			i++
		}
		if offset < i {
			return start, sql[start:end], num
		}
		start = i
		num++
	}
	return start, sql[start:], num
}

// expandTabs replaces each tab with spaces up to the next multiple of
// tabWidth bytes, matching ParseLocationTranslator::ExpandTabs.
func expandTabs(s string) string {
	if !strings.Contains(s, "\t") {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\t' {
			out.WriteString(strings.Repeat(" ", tabWidth-out.Len()%tabWidth))
		} else {
			out.WriteByte(s[i])
		}
	}
	return out.String()
}

// Caret renders the error in ZetaSQL's test format: the message with
// location, the offending source line (tabs expanded), and a caret marking
// the column.
func (e *Error) Caret() string {
	line, col := e.LineCol()
	_, srcLine, _ := lineAtOffset(e.SQL, e.Offset)
	srcLine = expandTabs(srcLine)
	caret := col
	if caret > len(srcLine)+1 {
		caret = len(srcLine) + 1
	}
	return fmt.Sprintf("%s [at %d:%d]\n%s\n%s^", e.Message, line, col, srcLine, strings.Repeat(" ", caret-1))
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

// Feature names a GoogleSQL language feature that gates optional syntax; see
// LanguageFeature in googlesql/public/options.proto. Feature names omit the
// FEATURE_ prefix, matching the language_features option used by the parser
// test suite.
type Feature string

// FeatureWithGroupRows gates "WITH name() AS GROUP ROWS" entries in WITH
// clauses (FEATURE_WITH_GROUP_ROWS).
const FeatureWithGroupRows Feature = "WITH_GROUP_ROWS"

// featureInMaximum records whether each gated feature is enabled by
// language_features=MAXIMUM, i.e. whether it is ideally enabled and not in
// development; see LanguageOptions::EnableMaximumLanguageFeatures and the
// language_feature_options annotations in googlesql/public/options.proto.
var featureInMaximum = map[Feature]bool{
	FeatureWithGroupRows: false, // in_development
}

// FeatureSet is a set of enabled language features. The zero value has no
// features enabled; a nil *FeatureSet enables every feature.
type FeatureSet struct {
	maximum   bool
	overrides map[Feature]bool
}

// ParseFeatureSet parses a language_features test option value such as
// "MAXIMUM,+WITH_GROUP_ROWS": a comma-separated list of feature names to
// enable, where MAXIMUM enables the maximum supported features and +NAME /
// -NAME add or remove a feature relative to that.
func ParseFeatureSet(spec string) *FeatureSet {
	fs := &FeatureSet{overrides: map[Feature]bool{}}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		switch {
		case part == "":
		case strings.EqualFold(part, "MAXIMUM"):
			fs.maximum = true
		case part[0] == '+':
			fs.overrides[Feature(part[1:])] = true
		case part[0] == '-':
			fs.overrides[Feature(part[1:])] = false
		default:
			fs.overrides[Feature(part)] = true
		}
	}
	return fs
}

// Enabled reports whether the feature is enabled. A nil FeatureSet enables
// every feature.
func (f *FeatureSet) Enabled(feat Feature) bool {
	if f == nil {
		return true
	}
	if v, ok := f.overrides[feat]; ok {
		return v
	}
	return f.maximum && featureInMaximum[feat]
}

// Options controls optional parser behavior.
type Options struct {
	// Features is the set of enabled language features; nil enables all.
	Features *FeatureSet
}

// ParseStatement parses a single SQL statement, allowing an optional
// trailing semicolon. All language features are enabled.
func ParseStatement(sql string) (ast.Statement, error) {
	return ParseStatementWithOptions(sql, Options{})
}

// ParseStatementWithOptions parses a single SQL statement, allowing an
// optional trailing semicolon.
func ParseStatementWithOptions(sql string, opts Options) (ast.Statement, error) {
	toks, err := lexer.Lex(sql)
	if err != nil {
		var lerr *lexer.Error
		if errors.As(err, &lerr) {
			return nil, &Error{Message: lerr.Message, Offset: lerr.Offset, SQL: sql}
		}
		return nil, err
	}
	p := &parser{sql: sql, toks: toks, features: opts.Features}
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
	sql      string
	toks     []token.Token
	pos      int
	features *FeatureSet
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
	"ALL": true, "AND": true, "ARRAY": true, "AS": true, "ASC": true,
	"BETWEEN": true,
	"BY":      true, "CASE": true, "CROSS": true, "DESC": true, "DISTINCT": true,
	"ELSE": true, "END": true, "EXCEPT": true, "FALSE": true, "FOR": true,
	"FROM": true,
	"FULL": true, "GROUP": true, "HAVING": true, "IN": true, "INNER": true,
	"INTERSECT": true, "IS": true, "JOIN": true, "LEFT": true, "LIKE": true,
	"LIMIT": true, "NOT": true, "NULL": true, "NULLS": true, "ON": true,
	"OR": true, "ORDER": true, "OUTER": true, "OVER": true, "PARTITION": true,
	"RIGHT": true, "SELECT": true, "SET": true,
	"TRUE": true, "UNION": true, "UNNEST": true, "USING": true, "WHERE": true,
	"WITH": true,
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
	case isKeyword(tok, "SELECT"), isKeyword(tok, "FROM"), isKeyword(tok, "WITH"),
		tok.Kind == token.LPAREN:
		query, err := p.parseQuery()
		if err != nil {
			return nil, err
		}
		// The statement's location covers all consumed tokens, which can
		// exceed the query node's own location: a parenthesized query keeps
		// the location of the query inside the parentheses.
		return &ast.QueryStatement{Span: span(tok.Pos, p.prevEnd()), Query: query}, nil
	case isKeyword(tok, "ALTER"):
		return p.parseAlterStatement()
	case isKeyword(tok, "CREATE"):
		return p.parseCreateStatement()
	}
	return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
}

// parseCreateStatement parses CREATE [OR REPLACE] [TEMP|TEMPORARY|PUBLIC|
// PRIVATE] TABLE [IF NOT EXISTS] <path> [AS query]; see
// create_table_statement in googlesql.tm. Other CREATE object kinds and the
// remaining optional clauses (table elements, options, PARTITION BY, ...)
// are not implemented yet.
func (p *parser) parseCreateStatement() (ast.Statement, error) {
	createTok := p.advance() // CREATE
	stmt := &ast.CreateTableStatement{Span: span(createTok.Pos, 0)}
	if isKeyword(p.peek(), "OR") && isKeyword(p.peekAt(1), "REPLACE") {
		p.advance()
		p.advance()
		stmt.IsOrReplace = true
	}
	switch {
	case isKeyword(p.peek(), "TEMP"), isKeyword(p.peek(), "TEMPORARY"):
		p.advance()
		stmt.Scope = "TEMP"
	case isKeyword(p.peek(), "PUBLIC"):
		p.advance()
		stmt.Scope = "PUBLIC"
	case isKeyword(p.peek(), "PRIVATE"):
		p.advance()
		stmt.Scope = "PRIVATE"
	}
	if _, err := p.expectKeyword("TABLE"); err != nil {
		return nil, err
	}
	if isKeyword(p.peek(), "IF") && isKeyword(p.peekAt(1), "NOT") && isKeyword(p.peekAt(2), "EXISTS") {
		p.advance()
		p.advance()
		p.advance()
		stmt.IsIfNotExists = true
	}
	name, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	stmt.Name = name
	stmt.Stop = name.End()
	if isKeyword(p.peek(), "AS") {
		p.advance()
		query, err := p.parseQuery()
		if err != nil {
			return nil, err
		}
		stmt.Query = query
		// The statement covers all consumed tokens, which can exceed the
		// query node's end: a parenthesized query keeps the location of the
		// query inside the parentheses.
		stmt.Stop = p.prevEnd()
	}
	return stmt, nil
}

// prevEnd returns the end offset of the most recently consumed token.
func (p *parser) prevEnd() int { return p.toks[p.pos-1].End }

// parseAlterStatement parses ALTER <schema object kind> [IF EXISTS] <path>
// <alter action list>; see alter_statement in googlesql.tm. Object kinds the
// reference grammar recognizes but does not support for ALTER (for example
// ALTER FUNCTION) are diagnosed only after the whole statement parses,
// matching the reference, which raises the error in the rule's reduce action.
func (p *parser) parseAlterStatement() (ast.Statement, error) {
	alterTok := p.advance() // ALTER
	kindTok := p.peek()
	var nodeName string    // parse tree node name for supported kinds
	var unsupported string // schema object kind name for unsupported kinds
	consumeSecond := func() { p.advance(); p.advance() }
	second := p.peekAt(1)
	switch {
	case isKeyword(kindTok, "TABLE") && isKeyword(second, "FUNCTION"):
		consumeSecond()
		unsupported = "TABLE FUNCTION"
	case isKeyword(kindTok, "TABLE"):
		p.advance()
		nodeName = "AlterTableStatement"
	case isKeyword(kindTok, "VIEW"):
		p.advance()
		nodeName = "AlterViewStatement"
	case isKeyword(kindTok, "MATERIALIZED") && isKeyword(second, "VIEW"):
		consumeSecond()
		nodeName = "AlterMaterializedViewStatement"
	case isKeyword(kindTok, "APPROX") && isKeyword(second, "VIEW"):
		consumeSecond()
		nodeName = "AlterApproxViewStatement"
	case isKeyword(kindTok, "MODEL"):
		p.advance()
		nodeName = "AlterModelStatement"
	case isKeyword(kindTok, "DATABASE"):
		p.advance()
		nodeName = "AlterDatabaseStatement"
	case isKeyword(kindTok, "SCHEMA"):
		p.advance()
		nodeName = "AlterSchemaStatement"
	case isKeyword(kindTok, "EXTERNAL") && isKeyword(second, "SCHEMA"):
		consumeSecond()
		nodeName = "AlterExternalSchemaStatement"
	case isKeyword(kindTok, "EXTERNAL") && isKeyword(second, "TABLE"):
		consumeSecond()
		unsupported = "EXTERNAL TABLE"
	case isKeyword(kindTok, "SEQUENCE"):
		p.advance()
		nodeName = "AlterSequenceStatement"
	case isKeyword(kindTok, "CONNECTION"):
		p.advance()
		nodeName = "AlterConnectionStatement"
	case isKeyword(kindTok, "AGGREGATE") && isKeyword(second, "FUNCTION"):
		consumeSecond()
		unsupported = "AGGREGATE FUNCTION"
	case isKeyword(kindTok, "CONSTANT"):
		p.advance()
		unsupported = "CONSTANT"
	case isKeyword(kindTok, "FUNCTION"):
		p.advance()
		unsupported = "FUNCTION"
	case isKeyword(kindTok, "INDEX"):
		p.advance()
		unsupported = "INDEX"
	case isKeyword(kindTok, "PROCEDURE"):
		p.advance()
		unsupported = "PROCEDURE"
	case isKeyword(kindTok, "PROPERTY") && isKeyword(second, "GRAPH"):
		consumeSecond()
		unsupported = "PROPERTY GRAPH"
	default:
		return nil, p.errorf(kindTok.Pos, "Syntax error: Unexpected %s", describeToken(kindTok))
	}

	stmt := &ast.AlterStatement{Span: span(alterTok.Pos, 0), NodeName: nodeName}
	if isKeyword(p.peek(), "IF") && isKeyword(p.peekAt(1), "EXISTS") {
		p.advance()
		p.advance()
		stmt.IsIfExists = true
	}
	if tok := p.peek(); tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	path, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	stmt.Path = path
	actions, err := p.parseAlterActionList()
	if err != nil {
		return nil, err
	}
	stmt.Actions = actions
	stmt.Stop = actions.End()
	if unsupported != "" {
		// No "Syntax error: " prefix; see alter_statement in googlesql.tm.
		return nil, p.errorf(kindTok.Pos, "ALTER %s is not supported", unsupported)
	}
	return stmt, nil
}

// parseAlterActionList parses one or more comma-separated alter actions.
func (p *parser) parseAlterActionList() (*ast.AlterActionList, error) {
	first, err := p.parseAlterAction()
	if err != nil {
		return nil, err
	}
	list := &ast.AlterActionList{Span: span(first.Pos(), first.End()), Actions: []ast.Node{first}}
	for p.peek().Kind == token.COMMA {
		p.advance()
		action, err := p.parseAlterAction()
		if err != nil {
			return nil, err
		}
		list.Actions = append(list.Actions, action)
		list.Stop = action.End()
	}
	return list, nil
}

// parseAlterAction parses a single alter action; see alter_action in
// googlesql.tm. Only SET OPTIONS and RENAME TO are implemented so far.
func (p *parser) parseAlterAction() (ast.Node, error) {
	tok := p.peek()
	if isKeyword(tok, "RENAME") {
		renameTok := p.advance()
		next := p.peek()
		if !isKeyword(next, "TO") {
			return nil, p.errorf(next.Pos, "Syntax error: Expected keyword COLUMN or keyword TO but got %s", describeToken(next))
		}
		p.advance() // TO
		path, err := p.parsePathExpression()
		if err != nil {
			return nil, err
		}
		return &ast.RenameToClause{Span: span(renameTok.Pos, path.End()), NewName: path}, nil
	}
	if !isKeyword(tok, "SET") {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	setTok := p.advance()
	next := p.peek()
	if !isKeyword(next, "OPTIONS") {
		return nil, p.errorf(next.Pos, "Syntax error: Expected keyword AS or keyword DEFAULT or keyword ON or keyword OPTIONS but got %s", describeToken(next))
	}
	p.advance() // OPTIONS
	opts, err := p.parseOptionsList()
	if err != nil {
		return nil, err
	}
	return &ast.SetOptionsAction{Span: span(setTok.Pos, opts.End()), Options: opts}, nil
}

// parseOptionsList parses "( [options_entry, ...] )".
func (p *parser) parseOptionsList() (*ast.OptionsList, error) {
	lparen, err := p.expect(token.LPAREN, `"("`)
	if err != nil {
		return nil, err
	}
	list := &ast.OptionsList{Span: span(lparen.Pos, 0)}
	if p.peek().Kind != token.RPAREN {
		for {
			entry, err := p.parseOptionsEntry()
			if err != nil {
				return nil, err
			}
			list.Entries = append(list.Entries, entry)
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
	list.Stop = rparen.End
	return list, nil
}

// parseOptionsEntry parses "identifier (=|+=|-=) expression".
func (p *parser) parseOptionsEntry() (*ast.OptionsEntry, error) {
	tok := p.peek()
	if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	name := p.parseIdentifierToken(p.advance())
	var op string
	switch {
	case p.peek().Kind == token.EQ:
		p.advance()
		op = "="
	// The lexer has no dedicated += / -= tokens yet; recognize the adjacent
	// two-token forms.
	case p.peek().Kind == token.PLUS && p.peekAt(1).Kind == token.EQ && p.peek().End == p.peekAt(1).Pos:
		p.advance()
		p.advance()
		op = "+="
	case p.peek().Kind == token.MINUS && p.peekAt(1).Kind == token.EQ && p.peek().End == p.peekAt(1).Pos:
		p.advance()
		p.advance()
		op = "-="
	default:
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "=" but got %s`, describeToken(p.peek()))
	}
	value, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	return &ast.OptionsEntry{Span: span(name.Pos(), value.End()), Name: name, Op: op, Value: value}, nil
}

// parseQuery parses "[WITH ...] query_primary [ORDER BY] [LIMIT] [FOR
// UPDATE]" followed by any pipe operators; see query and
// query_without_pipe_operators in googlesql.tm.
func (p *parser) parseQuery() (*ast.Query, error) {
	start := p.peek().Pos
	var with *ast.WithClause
	if isKeyword(p.peek(), "WITH") {
		w, err := p.parseWithClause()
		if err != nil {
			return nil, err
		}
		with = w
	}

	tok := p.peek()
	var primary ast.Node
	var primaryEnd int // end of the primary's tokens, including any parens
	switch {
	case isKeyword(tok, "FROM"):
		return p.parseFromQueryTail(start, with)
	case isKeyword(tok, "SELECT"):
		sel, err := p.parseSelect()
		if err != nil {
			return nil, err
		}
		primary, primaryEnd = sel, sel.End()
	case tok.Kind == token.LPAREN:
		inner, parenEnd, err := p.parseParenthesizedQuery()
		if err != nil {
			return nil, err
		}
		primary, primaryEnd = inner, parenEnd
	default:
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}

	var orderBy *ast.OrderBy
	var limit *ast.LimitOffset
	var lockMode *ast.LockMode
	end := primaryEnd
	if isKeyword(p.peek(), "ORDER") {
		ob, err := p.parseOrderBy()
		if err != nil {
			return nil, err
		}
		orderBy = ob
		end = ob.End()
	}
	if isKeyword(p.peek(), "LIMIT") {
		lo, err := p.parseLimitOffset()
		if err != nil {
			return nil, err
		}
		limit = lo
		end = lo.End()
	}
	// FOR UPDATE lock mode clause; the reference lexer only produces
	// KW_FOR_BEFORE_LOCK_MODE when FOR is immediately followed by UPDATE
	// (see the lookahead transformer), so require both keywords here.
	if isKeyword(p.peek(), "FOR") && isKeyword(p.peekAt(1), "UPDATE") {
		forTok := p.advance()    // FOR
		updateTok := p.advance() // UPDATE
		lockMode = &ast.LockMode{Span: span(forTok.Pos, updateTok.End)}
		end = lockMode.End()
	}

	var query *ast.Query
	inner, isParenQuery := primary.(*ast.Query)
	switch {
	case with != nil:
		query = &ast.Query{Span: span(start, end), WithClause: with, QueryExpr: primary,
			OrderBy: orderBy, Limit: limit, LockMode: lockMode}
	case isParenQuery && orderBy == nil && limit == nil && lockMode == nil:
		// A parenthesized query with no trailing clauses: wrapping it would
		// be semantically useless, so reuse the inner query node directly;
		// see query_without_pipe_operators in googlesql.tm.
		query = inner
	default:
		query = &ast.Query{Span: span(start, end), QueryExpr: primary,
			OrderBy: orderBy, Limit: limit, LockMode: lockMode}
	}
	return p.parsePipeOperators(query, start)
}

// parseParenthesizedQuery parses "( query )" with the opening parenthesis as
// the next token. The returned query keeps the location of the query inside
// the parentheses; parenEnd is the end offset of the closing parenthesis for
// callers that need the parenthesized range. See parenthesized_query in
// googlesql.tm.
func (p *parser) parseParenthesizedQuery() (query *ast.Query, parenEnd int, err error) {
	p.advance() // (
	query, err = p.parseQuery()
	if err != nil {
		return nil, 0, err
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, 0, err
	}
	query.Parenthesized = true
	return query, rparen.End, nil
}

// parseWithClause parses "WITH [RECURSIVE] name AS ( query ) [, ...]"; see
// with_clause in googlesql.tm.
func (p *parser) parseWithClause() (*ast.WithClause, error) {
	withTok := p.advance() // WITH
	wc := &ast.WithClause{Span: span(withTok.Pos, withTok.End)}
	if isKeyword(p.peek(), "RECURSIVE") {
		p.advance()
		wc.Recursive = true
	}
	for {
		entry, err := p.parseWithClauseEntry()
		if err != nil {
			return nil, err
		}
		wc.Entries = append(wc.Entries, entry)
		wc.Stop = entry.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance() // ,
		next := p.peek()
		if isKeyword(next, "SELECT") || isKeyword(next, "FROM") {
			// See with_clause_with_trailing_comma in googlesql.tm.
			return nil, p.errorf(next.Pos, "Syntax error: Trailing comma after the WITH clause before the main query is not allowed")
		}
	}
	if p.peek().Kind == token.PIPE_INPUT {
		// See the with_clause "|>" alternative of
		// query_without_pipe_operators in googlesql.tm.
		return nil, p.errorf(p.peek().Pos, "Syntax error: A pipe operator cannot follow the WITH clause before the main query; The main query usually starts with SELECT or FROM here")
	}
	return wc, nil
}

// parseWithClauseEntry parses "identifier AS ( query )"; see aliased_query
// and with_clause_entry in googlesql.tm.
func (p *parser) parseWithClauseEntry() (*ast.WithClauseEntry, error) {
	tok := p.peek()
	if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	if isReserved(tok) {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected keyword %s", strings.ToUpper(tok.Image))
	}
	ident := p.parseIdentifierToken(p.advance())
	if p.peek().Kind == token.LPAREN {
		// "identifier ( ) AS GROUP ROWS"; see the second alternative of
		// with_clause_entry in googlesql.tm.
		p.advance() // (
		if _, err := p.expect(token.RPAREN, `")"`); err != nil {
			return nil, err
		}
		if _, err := p.expectKeyword("AS"); err != nil {
			return nil, err
		}
		groupTok, err := p.expectKeyword("GROUP")
		if err != nil {
			return nil, err
		}
		rowsTok, err := p.expectKeyword("ROWS")
		if err != nil {
			return nil, err
		}
		if !p.features.Enabled(FeatureWithGroupRows) {
			// No "Syntax error: " prefix; see with_clause_entry in
			// googlesql.tm.
			return nil, p.errorf(groupTok.Pos, "GROUP ROWS is not supported.")
		}
		agr := &ast.AliasedGroupRows{Span: span(ident.Pos(), rowsTok.End), Identifier: ident}
		return &ast.WithClauseEntry{Span: agr.Span, AliasedGroupRows: agr}, nil
	}
	if _, err := p.expectKeyword("AS"); err != nil {
		return nil, err
	}
	lparen := p.peek()
	if lparen.Kind != token.LPAREN {
		return nil, p.errorf(lparen.Pos, `Syntax error: Expected "(" but got %s`, describeToken(lparen))
	}
	query, parenEnd, err := p.parseParenthesizedQuery()
	if err != nil {
		return nil, err
	}
	// The aliased query's location includes the parentheses; see
	// aliased_query_with_overridden_next_token_lookback in googlesql.tm.
	query.Start, query.Stop = lparen.Pos, parenEnd
	aq := &ast.AliasedQuery{Span: span(ident.Pos(), query.End()), Identifier: ident, Query: query}
	return &ast.WithClauseEntry{Span: aq.Span, AliasedQuery: aq}, nil
}

// parsePipeOperators parses any trailing "|> operator" sequence onto query.
// A pipe operator after a parenthesized query nests the query rather than
// extending it, to represent how the pipes bind; see the query rule in
// googlesql.tm. start is the offset of the query's first token (which can
// precede query's own location when it is parenthesized).
func (p *parser) parsePipeOperators(query *ast.Query, start int) (*ast.Query, error) {
	for p.peek().Kind == token.PIPE_INPUT {
		op, err := p.parsePipeOperator()
		if err != nil {
			return nil, err
		}
		if query.Parenthesized {
			query.Parenthesized = false
			query = &ast.Query{Span: span(start, op.End()), QueryExpr: query,
				PipeOperators: []ast.Node{op}}
		} else {
			query.PipeOperators = append(query.PipeOperators, op)
			query.Stop = op.End()
		}
	}
	return query, nil
}

// parseFromQueryTail parses a standalone FROM clause used as a query,
// optionally preceded by an already-parsed WITH clause (starting at start)
// and optionally followed by a lock mode clause and pipe operators; see the
// from_clause alternative of query_without_pipe_operators in googlesql.tm.
// Clauses that would be valid after a FROM clause in a normal query produce
// dedicated errors suggesting pipe operators.
func (p *parser) parseFromQueryTail(start int, with *ast.WithClause) (*ast.Query, error) {
	from, err := p.parseFromClause()
	if err != nil {
		return nil, err
	}
	tok := p.peek()
	switch {
	case isKeyword(tok, "WHERE"):
		return nil, p.badKeywordAfterFromQuery(tok, "WHERE", "WHERE", false)
	case isKeyword(tok, "SELECT"):
		return nil, p.badKeywordAfterFromQuery(tok, "SELECT", "SELECT", false)
	case isKeyword(tok, "GROUP"):
		return nil, p.badKeywordAfterFromQuery(tok, "GROUP BY", "AGGREGATE", false)
	case isKeyword(tok, "ORDER"):
		return nil, p.badKeywordAfterFromQuery(tok, "ORDER BY", "ORDER BY", true)
	case isKeyword(tok, "UNION"):
		return nil, p.badKeywordAfterFromQuery(tok, "UNION", "UNION", true)
	case isKeyword(tok, "INTERSECT"):
		return nil, p.badKeywordAfterFromQuery(tok, "INTERSECT", "INTERSECT", true)
	case isKeyword(tok, "LIMIT"):
		return nil, p.badKeywordAfterFromQuery(tok, "LIMIT", "LIMIT", true)
	// EXCEPT only lexes as a set operation keyword when followed by ALL or
	// DISTINCT (KW_EXCEPT_IN_SET_OP in the reference lookahead transformer).
	case isKeyword(tok, "EXCEPT") && (isKeyword(p.peekAt(1), "ALL") || isKeyword(p.peekAt(1), "DISTINCT")):
		return nil, p.badKeywordAfterFromQuery(tok, "EXCEPT", "EXCEPT", true)
	}
	fromQuery := &ast.FromQuery{Span: span(from.Pos(), from.End()), From: from}
	query := &ast.Query{Span: span(start, fromQuery.End()), WithClause: with, QueryExpr: fromQuery}
	if isKeyword(p.peek(), "FOR") && isKeyword(p.peekAt(1), "UPDATE") {
		forTok := p.advance()    // FOR
		updateTok := p.advance() // UPDATE
		query.LockMode = &ast.LockMode{Span: span(forTok.Pos, updateTok.End)}
		query.Stop = query.LockMode.End()
	}
	return p.parsePipeOperators(query, start)
}

// badKeywordAfterFromQuery builds the error for a clause keyword that is not
// allowed after a FROM query; see bad_keyword_after_from_query and
// bad_keyword_after_from_query_allows_parens in googlesql.tm.
func (p *parser) badKeywordAfterFromQuery(tok token.Token, keyword, pipeOp string, allowsParens bool) error {
	suffix := ""
	if allowsParens {
		suffix = " or parentheses around the FROM query"
	}
	return p.errorf(tok.Pos, "Syntax error: %s not supported after FROM query; Consider using pipe operator `|> %s`%s", keyword, pipeOp, suffix)
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
	case isKeyword(tok, "LOG"):
		logTok := p.advance()
		node := &ast.PipeLog{Span: span(pipeTok.Pos, logTok.End)}
		if p.peek().Kind == token.LPAREN {
			sub, err := p.parseSubpipeline()
			if err != nil {
				return nil, err
			}
			node.Subpipeline = sub
			node.Stop = sub.End()
		}
		return node, nil
	case isKeyword(tok, "AGGREGATE"):
		return p.parsePipeAggregate(pipeTok)
	case isKeyword(tok, "SELECT"):
		sel, err := p.parseSelectClause()
		if err != nil {
			return nil, err
		}
		return &ast.PipeSelect{Span: span(pipeTok.Pos, sel.End()), Select: sel}, nil
	case isKeyword(tok, "EXTEND"):
		return p.parsePipeExtend(pipeTok)
	case isKeyword(tok, "LIMIT"):
		limitOffset, err := p.parseLimitOffset()
		if err != nil {
			return nil, err
		}
		return &ast.PipeLimitOffset{Span: span(pipeTok.Pos, limitOffset.End()), LimitOffset: limitOffset}, nil
	case isKeyword(tok, "DISTINCT"):
		distinctTok := p.advance()
		return &ast.PipeDistinct{Span: span(pipeTok.Pos, distinctTok.End)}, nil
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

// parseSubpipeline parses a parenthesized subpipeline "( |> op ... )" with
// the opening parenthesis as the next token; see subpipeline_with_parens and
// subpipeline_prefix_invalid in googlesql.tm.
func (p *parser) parseSubpipeline() (*ast.Subpipeline, error) {
	lparen := p.advance() // (
	sub := &ast.Subpipeline{Span: span(lparen.Pos, 0)}
	// Dedicated errors when the parenthesized text does not start with |>;
	// see subpipeline_bad_prefix_subquery and
	// subpipeline_bad_prefix_not_subquery in googlesql.tm.
	tok := p.peek()
	switch {
	case isKeyword(tok, "SELECT"), isKeyword(tok, "FROM"), isKeyword(tok, "WITH"):
		return nil, p.errorf(tok.Pos, "Syntax error: Expected subpipeline starting with |>, not a subquery")
	case tok.Kind == token.LPAREN,
		tok.Kind == token.QUOTED_IDENT,
		tok.Kind == token.IDENT && !isReserved(tok),
		isKeyword(tok, "WHERE"), isKeyword(tok, "LIMIT"), isKeyword(tok, "JOIN"),
		isKeyword(tok, "ORDER"), isKeyword(tok, "GROUP"):
		return nil, p.errorf(tok.Pos, "Syntax error: Expected subpipeline starting with |>")
	}
	for p.peek().Kind == token.PIPE_INPUT {
		op, err := p.parsePipeOperator()
		if err != nil {
			return nil, err
		}
		sub.PipeOperators = append(sub.PipeOperators, op)
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	sub.Stop = rparen.End
	return sub, nil
}

// parsePipeAggregate parses "AGGREGATE [expression [AS alias], ...]
// [GROUP BY ...]" after a |> token. The aggregate list and GROUP BY are
// represented as a Select node; see pipe_aggregate in googlesql.tm.
func (p *parser) parsePipeAggregate(pipeTok token.Token) (ast.Node, error) {
	aggTok := p.advance() // AGGREGATE
	sel := &ast.Select{Span: span(aggTok.Pos, aggTok.End)}
	// An empty aggregate list is an empty SelectList located at the end of
	// the AGGREGATE keyword.
	list := &ast.SelectList{Span: span(aggTok.End, aggTok.End)}
	if startsExpression(p.peek()) {
		for {
			col, err := p.parseSelectColumnExpr()
			if err != nil {
				return nil, err
			}
			if len(list.Columns) == 0 {
				list.Start = col.Pos()
			}
			list.Columns = append(list.Columns, col)
			list.Stop = col.End()
			if p.peek().Kind != token.COMMA {
				break
			}
			comma := p.advance()
			if !startsExpression(p.peek()) {
				// Trailing comma; it is included in the list's location.
				list.Stop = comma.End
				break
			}
		}
	}
	sel.SelectList = list
	sel.Stop = list.End()
	if isKeyword(p.peek(), "GROUP") {
		groupBy, err := p.parseGroupBy()
		if err != nil {
			return nil, err
		}
		sel.GroupBy = groupBy
		sel.Stop = groupBy.End()
	}
	return &ast.PipeAggregate{Span: span(pipeTok.Pos, sel.End()), Select: sel}, nil
}

// parsePipeExtend parses "EXTEND pipe_selection_item_list" after a |> token.
// The selection list is represented as a Select node whose location starts at
// the EXTEND keyword; see pipe_extend in googlesql.tm.
func (p *parser) parsePipeExtend(pipeTok token.Token) (ast.Node, error) {
	extendTok := p.advance() // EXTEND
	list, err := p.parsePipeSelectionItemList()
	if err != nil {
		return nil, err
	}
	sel := &ast.Select{Span: span(extendTok.Pos, list.End()), SelectList: list}
	return &ast.PipeExtend{Span: span(pipeTok.Pos, sel.End()), Select: sel}, nil
}

// parsePipeSelectionItemList parses one or more comma-separated selection
// items with an optional trailing comma; see pipe_selection_item_list in
// googlesql.tm.
func (p *parser) parsePipeSelectionItemList() (*ast.SelectList, error) {
	first, err := p.parseSelectColumnExpr()
	if err != nil {
		return nil, err
	}
	list := &ast.SelectList{Span: span(first.Pos(), first.End()), Columns: []*ast.SelectColumn{first}}
	for p.peek().Kind == token.COMMA {
		comma := p.advance()
		if !startsExpression(p.peek()) {
			// Trailing comma; it is included in the list's location.
			list.Stop = comma.End
			break
		}
		col, err := p.parseSelectColumnExpr()
		if err != nil {
			return nil, err
		}
		list.Columns = append(list.Columns, col)
		list.Stop = col.End()
	}
	return list, nil
}

// parseSelectColumnExpr parses "expression [[AS] alias]" (a select list item
// without the * forms); see select_column_expr in googlesql.tm.
func (p *parser) parseSelectColumnExpr() (*ast.SelectColumn, error) {
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if err := p.checkAttachedAlias(); err != nil {
		return nil, err
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

// startsExpression reports whether tok can begin an expression.
func startsExpression(tok token.Token) bool {
	switch tok.Kind {
	case token.INT, token.FLOAT, token.STRING, token.BYTES,
		token.LBRACKET, token.LPAREN, token.MINUS, token.PLUS, token.TILDE,
		token.QUOTED_IDENT:
		return true
	case token.IDENT:
		if !isReserved(tok) {
			return true
		}
		switch strings.ToUpper(tok.Image) {
		case "NULL", "TRUE", "FALSE", "NOT", "ARRAY", "CASE":
			return true
		}
	}
	return false
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

// parseSelectClause parses "SELECT [ALL|DISTINCT] select_list" with none of
// the clauses after the select list; see select_clause in googlesql.tm.
func (p *parser) parseSelectClause() (*ast.Select, error) {
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
	return sel, nil
}

func (p *parser) parseSelect() (*ast.Select, error) {
	sel, err := p.parseSelectClause()
	if err != nil {
		return nil, err
	}

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
		havingTok := p.advance()
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		sel.Having = &ast.Having{Span: span(havingTok.Pos, expr.End()), Expr: expr}
		sel.Stop = expr.End()
	}
	return sel, nil
}

func (p *parser) parseSelectList() (*ast.SelectList, error) {
	first, err := p.parseSelectColumn()
	if err != nil {
		return nil, err
	}
	list := &ast.SelectList{Span: span(first.Pos(), first.End()), Columns: []*ast.SelectColumn{first}}
	for p.peek().Kind == token.COMMA {
		comma := p.advance()
		next := p.peek()
		if next.Kind != token.STAR && !startsExpression(next) {
			// Trailing comma; it is included in the list's location. See
			// select_list in googlesql.tm.
			list.Stop = comma.End
			break
		}
		col, err := p.parseSelectColumn()
		if err != nil {
			return nil, err
		}
		list.Columns = append(list.Columns, col)
		list.Stop = col.End()
	}
	return list, nil
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
	if err := p.checkAttachedAlias(); err != nil {
		return nil, err
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

// checkAttachedAlias reports the reference implementation's ATTACHED_ALIAS
// error: an integer or floating point literal immediately followed, with no
// whitespace in between, by an unquoted identifier or keyword, as in
// `SELECT 123abc`. See IsLiteralBeforeAdjacentUnquotedIdentifier in
// googlesql/parser/lookahead_transformer.cc and the select_column_expr rule
// in googlesql/parser/googlesql.tm.
func (p *parser) checkAttachedAlias() error {
	tok := p.peek()
	if tok.Kind != token.IDENT || p.pos == 0 {
		return nil
	}
	prev := p.toks[p.pos-1]
	if (prev.Kind != token.INT && prev.Kind != token.FLOAT) || prev.End != tok.Pos {
		return nil
	}
	// Inputs like "123.abc" tokenize as the float "123." followed by the
	// identifier "abc" and remain valid, mirroring the reference lexer.
	if strings.HasSuffix(prev.Image, ".") {
		return nil
	}
	return p.errorf(tok.Pos, "Syntax error: Missing whitespace between literal and alias")
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
	table, err := p.parseTablePrimary()
	if err != nil {
		return nil, err
	}
	return &ast.FromClause{Span: span(fromTok.Pos, table.End()), TableExpression: table}, nil
}

// parseTablePrimary parses a single table item in a FROM clause: either a
// parenthesized query used as a table subquery or a table path expression;
// see table_primary in googlesql.tm.
func (p *parser) parseTablePrimary() (ast.Node, error) {
	if p.peek().Kind == token.LPAREN {
		return p.parseTableSubquery()
	}
	return p.parseTablePathExpression()
}

// parseTableSubquery parses "( query ) [[AS] alias]" in a FROM clause; see
// table_subquery in googlesql.tm. The node's location includes the
// parentheses and the alias, while the inner query's location does not
// include the parentheses.
func (p *parser) parseTableSubquery() (*ast.TableSubquery, error) {
	lparen := p.peek()
	query, parenEnd, err := p.parseParenthesizedQuery()
	if err != nil {
		return nil, err
	}
	node := &ast.TableSubquery{Span: span(lparen.Pos, parenEnd), Query: query}
	alias, err := p.parseOptionalAlias()
	if err != nil {
		return nil, err
	}
	if alias != nil {
		node.Alias = alias
		node.Stop = alias.End()
	}
	return node, nil
}

func (p *parser) parseTablePathExpression() (ast.Node, error) {
	var table *ast.TablePathExpression
	if isKeyword(p.peek(), "UNNEST") {
		unnest, err := p.parseUnnestExpression()
		if err != nil {
			return nil, err
		}
		table = &ast.TablePathExpression{Span: span(unnest.Pos(), unnest.End()), UnnestExpr: unnest}
	} else {
		path, err := p.parsePathExpression()
		if err != nil {
			return nil, err
		}
		if p.peek().Kind == token.LPAREN {
			return p.parseTVFRest(path)
		}
		table = &ast.TablePathExpression{Span: span(path.Pos(), path.End()), Path: path}
	}
	alias, err := p.parseOptionalAlias()
	if err != nil {
		return nil, err
	}
	if alias != nil {
		table.Alias = alias
		table.Stop = alias.End()
	}
	// WITH OFFSET [[AS] alias]; see with_offset_and_alias in googlesql.tm.
	if isKeyword(p.peek(), "WITH") && isKeyword(p.peekAt(1), "OFFSET") {
		withTok := p.advance()   // WITH
		offsetTok := p.advance() // OFFSET
		offset := &ast.WithOffset{Span: span(withTok.Pos, offsetTok.End)}
		offsetAlias, err := p.parseOptionalAlias()
		if err != nil {
			return nil, err
		}
		if offsetAlias != nil {
			offset.Alias = offsetAlias
			offset.Stop = offsetAlias.End()
		}
		table.Offset = offset
		table.Stop = offset.End()
	}
	return table, nil
}

// parseTVFRest parses the argument list and optional alias of a
// table-valued function call in a FROM clause, after the function's path
// expression has already been parsed; see tvf in googlesql.tm. The opening
// parenthesis is the next token.
func (p *parser) parseTVFRest(path *ast.PathExpression) (*ast.TVF, error) {
	p.advance() // (
	tvf := &ast.TVF{Span: span(path.Pos(), 0), Name: path}
	if p.peek().Kind != token.RPAREN {
		for {
			expr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			arg := &ast.TVFArgument{Span: span(expr.Pos(), expr.End()), Expr: expr}
			tvf.Args = append(tvf.Args, arg)
			if p.peek().Kind != token.COMMA {
				break
			}
			p.advance() // ,
		}
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	tvf.Stop = rparen.End
	alias, err := p.parseOptionalAlias()
	if err != nil {
		return nil, err
	}
	if alias != nil {
		tvf.Alias = alias
		tvf.Stop = alias.End()
	}
	return tvf, nil
}

// parseUnnestExpression parses "UNNEST ( expression [AS alias] [, ...] )";
// see unnest_expression in googlesql.tm.
func (p *parser) parseUnnestExpression() (*ast.UnnestExpression, error) {
	unnestTok := p.advance() // UNNEST
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	if isKeyword(p.peek(), "SELECT") {
		// No "Syntax error: " prefix; see unnest_expression in googlesql.tm.
		return nil, p.errorf(p.peek().Pos, "The argument to UNNEST is an expression, not a query; to use a query as an expression, the query must be wrapped with additional parentheses to make it a scalar subquery expression")
	}
	node := &ast.UnnestExpression{Span: span(unnestTok.Pos, 0)}
	for {
		expr, err := p.parseExpressionWithOptAlias()
		if err != nil {
			return nil, err
		}
		node.Expressions = append(node.Expressions, expr)
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance()
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	node.Stop = rparen.End
	return node, nil
}

// parseExpressionWithOptAlias parses an expression with an optional alias
// that requires the AS keyword; see expression_with_opt_alias in
// googlesql.tm.
func (p *parser) parseExpressionWithOptAlias() (*ast.ExpressionWithOptAlias, error) {
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	node := &ast.ExpressionWithOptAlias{Span: span(expr.Pos(), expr.End()), Expr: expr}
	if isKeyword(p.peek(), "AS") {
		asTok := p.advance()
		tok := p.peek()
		if tok.Kind != token.QUOTED_IDENT && (tok.Kind != token.IDENT || isReserved(tok)) {
			return nil, p.errorf(tok.Pos, "Syntax error: Expected identifier but got %s", describeToken(tok))
		}
		ident := p.parseIdentifierToken(p.advance())
		node.Alias = &ast.Alias{Span: span(asTok.Pos, ident.End()), Identifier: ident}
		node.Stop = ident.End()
	}
	return node, nil
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
	hint, err := p.parseOptionalHint()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("BY"); err != nil {
		return nil, err
	}
	orderBy := &ast.OrderBy{Span: span(orderTok.Pos, orderTok.End), Hint: hint}
	for {
		item, err := p.parseOrderingExpression()
		if err != nil {
			return nil, err
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

// parseOrderingExpression parses "expression [COLLATE collation] [ASC|DESC]
// [NULLS FIRST|NULLS LAST]"; see ordering_expression in googlesql.tm.
func (p *parser) parseOrderingExpression() (*ast.OrderingExpression, error) {
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	item := &ast.OrderingExpression{Span: span(expr.Pos(), expr.End()), Expr: expr}
	if isKeyword(p.peek(), "COLLATE") {
		collate, err := p.parseCollate()
		if err != nil {
			return nil, err
		}
		item.Collate = collate
		item.Stop = collate.End()
	}
	if isKeyword(p.peek(), "ASC") {
		tok := p.advance()
		item.HasAsc = true
		item.Stop = tok.End
	} else if isKeyword(p.peek(), "DESC") {
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

// parseCollate parses "COLLATE <string literal>" with the COLLATE keyword as
// the next token; see collate_clause in googlesql.tm. Parameters and system
// variables as collation names are not implemented yet.
func (p *parser) parseCollate() (*ast.Collate, error) {
	collateTok := p.advance() // COLLATE
	if p.peek().Kind != token.STRING {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "@" or "@@" or string literal but got %s`, describeToken(p.peek()))
	}
	name, err := p.parseStringLiteral()
	if err != nil {
		return nil, err
	}
	return &ast.Collate{Span: span(collateTok.Pos, name.End()), Name: name}, nil
}

// parseOptionalHint parses a "@{name=value, ...}" hint if one starts at the
// current position; see hint in googlesql.tm. Integer hints ("@<int>") are
// not implemented yet.
func (p *parser) parseOptionalHint() (*ast.Hint, error) {
	if p.peek().Kind != token.ATSIGN {
		return nil, nil
	}
	at := p.advance() // @
	if _, err := p.expect(token.LBRACE, `"{"`); err != nil {
		return nil, err
	}
	hint := &ast.Hint{Span: span(at.Pos, 0)}
	for {
		entry, err := p.parseHintEntry()
		if err != nil {
			return nil, err
		}
		hint.Entries = append(hint.Entries, entry)
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance()
	}
	rbrace, err := p.expect(token.RBRACE, `"}"`)
	if err != nil {
		return nil, err
	}
	hint.Stop = rbrace.End
	return hint, nil
}

// parseHintEntry parses "[qualifier.]name = expression"; see hint_entry in
// googlesql.tm.
func (p *parser) parseHintEntry() (*ast.HintEntry, error) {
	name, err := p.parseIdentifierInHints()
	if err != nil {
		return nil, err
	}
	entry := &ast.HintEntry{Span: span(name.Pos(), 0), Name: name}
	if p.peek().Kind == token.DOT {
		p.advance()
		second, err := p.parseIdentifierInHints()
		if err != nil {
			return nil, err
		}
		entry.Qualifier = name
		entry.Name = second
	}
	if _, err := p.expect(token.EQ, `"="`); err != nil {
		return nil, err
	}
	value, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	entry.Value = value
	entry.Stop = value.End()
	return entry, nil
}

// parseIdentifierInHints parses a hint name identifier. The reserved keywords
// HASH, PROTO, and PARTITION are also allowed as hint names; see
// identifier_in_hints in googlesql.tm.
func (p *parser) parseIdentifierInHints() (*ast.Identifier, error) {
	tok := p.peek()
	if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
		return nil, p.errorf(tok.Pos, "Syntax error: Expected identifier but got %s", describeToken(tok))
	}
	if isReserved(tok) && !isKeyword(tok, "HASH") && !isKeyword(tok, "PROTO") && !isKeyword(tok, "PARTITION") {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected keyword %s", strings.ToUpper(tok.Image))
	}
	return p.parseIdentifierToken(p.advance()), nil
}

func (p *parser) parseLimitOffset() (*ast.LimitOffset, error) {
	limitTok, err := p.expectKeyword("LIMIT")
	if err != nil {
		return nil, err
	}
	// The LIMIT keyword is included in the wrapping Limit node's location;
	// see limit_expression and limit_all in googlesql.tm.
	var limit *ast.Limit
	if isKeyword(p.peek(), "ALL") {
		allTok := p.advance()
		all := &ast.LimitAll{Span: span(allTok.Pos, allTok.End)}
		limit = &ast.Limit{Span: span(limitTok.Pos, allTok.End), Expr: all}
	} else {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		limit = &ast.Limit{Span: span(limitTok.Pos, expr.End()), Expr: expr}
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
	case token.LBRACKET:
		return p.parseArrayConstructor(tok.Pos)
	case token.LPAREN:
		if p.lparenStartsQuery() {
			save := p.pos
			lparen := p.peek()
			query, parenEnd, qerr := p.parseParenthesizedQuery()
			if qerr == nil {
				// The subquery's ExpressionSubquery node covers the
				// parentheses; the inner query is not marked parenthesized
				// because the subquery node already accounts for them (see
				// expression_higher_prec_than_and in googlesql.tm).
				query.Parenthesized = false
				return &ast.ExpressionSubquery{Span: span(lparen.Pos, parenEnd), Query: query}, nil
			}
			// A parenthesized expression can also start with a nested
			// subquery (e.g. "((SELECT 1) + 2)"), so retry as an ordinary
			// parenthesized expression and keep whichever error got further.
			p.pos = save
			expr, eerr := p.parseParenthesizedExpression()
			if eerr != nil {
				return nil, furthestError(qerr, eerr)
			}
			return expr, nil
		}
		return p.parseParenthesizedExpression()
	case token.IDENT, token.QUOTED_IDENT:
		switch {
		case isKeyword(tok, "NULL"):
			p.advance()
			return &ast.NullLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image}, nil
		case isKeyword(tok, "TRUE"), isKeyword(tok, "FALSE"):
			p.advance()
			return &ast.BooleanLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image, Value: isKeyword(tok, "TRUE")}, nil
		case isKeyword(tok, "ARRAY") && p.peekAt(1).Kind == token.LBRACKET:
			p.advance() // ARRAY; the constructor's span starts at the keyword.
			return p.parseArrayConstructor(tok.Pos)
		case isKeyword(tok, "ARRAY") && p.peekAt(1).Kind == token.LPAREN:
			p.advance() // ARRAY; the subquery's span starts at the keyword.
			return p.parseModifiedSubquery(tok.Pos, "ARRAY")
		case isKeyword(tok, "EXISTS") && p.peekAt(1).Kind == token.LPAREN:
			p.advance() // EXISTS; the subquery's span starts at the keyword.
			return p.parseModifiedSubquery(tok.Pos, "EXISTS")
		case isReserved(tok):
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected keyword %s", strings.ToUpper(tok.Image))
		}
		return p.parsePathOrCall()
	}
	return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
}

// lparenStartsQuery reports whether the "(" at the current position opens a
// parenthesized query rather than a parenthesized expression or struct
// constructor: after skipping consecutive "("s, the next token must start a
// query.
func (p *parser) lparenStartsQuery() bool {
	i := 0
	for p.peekAt(i).Kind == token.LPAREN {
		i++
	}
	tok := p.peekAt(i)
	return isKeyword(tok, "SELECT") || isKeyword(tok, "WITH") || isKeyword(tok, "FROM")
}

// furthestError returns whichever parse error consumed more input, so the
// message from the more successful of two alternative parses wins; a is
// preferred on ties.
func furthestError(a, b error) error {
	var ea, eb *Error
	if errors.As(a, &ea) && errors.As(b, &eb) && eb.Offset > ea.Offset {
		return b
	}
	return a
}

// parseModifiedSubquery parses the "( query )" following an ARRAY or EXISTS
// keyword (already consumed, starting at start); see
// expression_subquery_with_keyword in googlesql.tm.
func (p *parser) parseModifiedSubquery(start int, modifier string) (ast.Node, error) {
	query, parenEnd, err := p.parseParenthesizedQuery()
	if err != nil {
		return nil, err
	}
	query.Parenthesized = false
	return &ast.ExpressionSubquery{Span: span(start, parenEnd), Modifier: modifier, Query: query}, nil
}

// parseParenthesizedExpression parses "( expression )" or a struct
// constructor "(expr, expr, ...)" with the opening parenthesis as the next
// token; see struct_constructor and parenthesized_expression_not_a_query in
// googlesql.tm.
func (p *parser) parseParenthesizedExpression() (ast.Node, error) {
	lparen := p.advance()
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	// "(expr, expr, ...)" is a struct constructor; see struct_constructor
	// in googlesql.tm.
	if p.peek().Kind == token.COMMA {
		s := &ast.StructConstructorWithParens{
			Span:             span(lparen.Pos, 0),
			FieldExpressions: []ast.Node{expr},
		}
		for p.peek().Kind == token.COMMA {
			p.advance()
			field, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			s.FieldExpressions = append(s.FieldExpressions, field)
		}
		rparen, err := p.expect(token.RPAREN, `")" or ","`)
		if err != nil {
			return nil, err
		}
		s.Stop = rparen.End
		return s, nil
	}
	// After "( expression", the reference LALR parser's only live item on
	// an unexpected token is the struct constructor's "," continuation, so
	// its error suggests "," rather than ")".
	if p.peek().Kind != token.RPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "," but got %s`, describeToken(p.peek()))
	}
	p.advance()
	// Parenthesized expressions keep the span of the inner expression in
	// ZetaSQL's parse tree; the parentheses only affect grouping.
	return expr, nil
}

// parseArrayConstructor parses "[ [expression, ...] ]" with the opening
// bracket as the next token; start is the start offset of the constructor
// (the "[" itself, or an ARRAY keyword already consumed by the caller). See
// array_constructor in googlesql.tm.
func (p *parser) parseArrayConstructor(start int) (ast.Node, error) {
	p.advance() // [
	arr := &ast.ArrayConstructor{Span: span(start, 0)}
	if p.peek().Kind != token.RBRACKET {
		for {
			expr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			arr.Elements = append(arr.Elements, expr)
			if p.peek().Kind != token.COMMA {
				break
			}
			p.advance()
		}
	}
	rbracket, err := p.expect(token.RBRACKET, `"]"`)
	if err != nil {
		return nil, err
	}
	arr.Stop = rbracket.End
	return arr, nil
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
	if isKeyword(p.peek(), "OVER") {
		p.advance()
		windowSpec, err := p.parseWindowSpecification()
		if err != nil {
			return nil, err
		}
		return &ast.AnalyticFunctionCall{
			Span:       span(call.Pos(), windowSpec.End()),
			Expr:       call,
			WindowSpec: windowSpec,
		}, nil
	}
	return call, nil
}

// parseWindowSpecification parses the window after OVER: a base window name,
// or "( [name] [PARTITION BY ...] [ORDER BY ...] )"; see window_specification
// in googlesql.tm. Window frame clauses (ROWS/RANGE) are not implemented yet.
func (p *parser) parseWindowSpecification() (*ast.WindowSpecification, error) {
	tok := p.peek()
	if tok.Kind == token.IDENT || tok.Kind == token.QUOTED_IDENT {
		if isReserved(tok) {
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected keyword %s", strings.ToUpper(tok.Image))
		}
		ident := p.parseIdentifierToken(p.advance())
		return &ast.WindowSpecification{Span: span(ident.Pos(), ident.End()), Name: ident}, nil
	}
	if tok.Kind != token.LPAREN {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	lparen := p.advance()
	windowSpec := &ast.WindowSpecification{Span: span(lparen.Pos, 0)}
	tok = p.peek()
	if (tok.Kind == token.IDENT || tok.Kind == token.QUOTED_IDENT) && !isReserved(tok) &&
		!isKeyword(tok, "PARTITION") && !isKeyword(tok, "ORDER") {
		windowSpec.Name = p.parseIdentifierToken(p.advance())
	}
	if isKeyword(p.peek(), "PARTITION") {
		partitionBy, err := p.parsePartitionBy()
		if err != nil {
			return nil, err
		}
		windowSpec.PartitionBy = partitionBy
	}
	if isKeyword(p.peek(), "ORDER") {
		orderBy, err := p.parseOrderBy()
		if err != nil {
			return nil, err
		}
		windowSpec.OrderBy = orderBy
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	windowSpec.Stop = rparen.End
	return windowSpec, nil
}

// parsePartitionBy parses "PARTITION [hint] BY expression, ..."; see
// partition_by_clause_prefix in googlesql.tm.
func (p *parser) parsePartitionBy() (*ast.PartitionBy, error) {
	partitionTok := p.advance() // PARTITION
	hint, err := p.parseOptionalHint()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("BY"); err != nil {
		return nil, err
	}
	partitionBy := &ast.PartitionBy{Span: span(partitionTok.Pos, 0), Hint: hint}
	for {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		partitionBy.Expressions = append(partitionBy.Expressions, expr)
		partitionBy.Stop = expr.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance()
	}
	return partitionBy, nil
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
