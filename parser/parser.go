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
// location, the offending source line (tabs expanded, truncated to 80
// columns around the error), and a caret marking the column; see
// GetErrorStringWithCaret and GetTruncatedInputStringInfo in
// googlesql/public/error_helpers.cc.
func (e *Error) Caret() string {
	line, col := e.LineCol()
	_, srcLine, _ := lineAtOffset(e.SQL, e.Offset)
	srcLine = expandTabs(srcLine)
	const maxWidth = 80
	// errorColumn is 0-based; col may be one off the end of the line for
	// end-of-input errors.
	errorColumn := max(1, min(len(srcLine)+1, col)) - 1
	// If the error line is longer than maxWidth, give a substring of up to
	// maxWidth characters with the caret near the middle of it.
	if len(srcLine) > maxWidth {
		oneHalf := maxWidth / 2
		oneThird := maxWidth / 3
		// If the error is near the start, just use a prefix of the string.
		if errorColumn > maxWidth-oneThird {
			// Otherwise, try to find a word boundary to start the string on
			// that puts the caret in the middle third of the output line.
			foundStart := -1
			for startColumn := max(0, errorColumn-2*oneThird); startColumn < max(0, errorColumn-oneThird); startColumn++ {
				if isWordStart(srcLine, startColumn) {
					foundStart = startColumn
					break
				}
			}
			if foundStart == -1 {
				// Didn't find a good separator. Just split in the middle.
				foundStart = max(errorColumn-oneHalf, 0)
			}
			// Add the "..." prefix if necessary.
			if foundStart < 3 {
				foundStart = 0
			} else {
				srcLine = "..." + srcLine[foundStart:]
				errorColumn -= foundStart - 3
			}
		}
		srcLine = prettyTruncate(srcLine, maxWidth)
	}
	return fmt.Sprintf("%s [at %d:%d]\n%s\n%s^", e.Message, line, col, srcLine, strings.Repeat(" ", errorColumn))
}

// isWordStart reports whether the 0-based column in s starts a word; see
// IsWordStart in googlesql/public/error_helpers.cc.
func isWordStart(s string, column int) bool {
	if column == 0 || column >= len(s) {
		return true
	}
	return !isWordByte(s[column-1]) && isWordByte(s[column])
}

func isWordByte(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}

// prettyTruncate truncates s to at most maxBytes bytes, appending "..." when
// it truncates and avoiding splitting a UTF-8 character; see
// PrettyTruncateUTF8 in googlesql/common/utf_util.cc.
func prettyTruncate(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	if maxBytes <= 3 {
		return s[:maxBytes]
	}
	newWidth := maxBytes - 3
	// Back up to the start of the code point containing byte newWidth.
	for newWidth > 0 && s[newWidth]&0xC0 == 0x80 {
		newWidth--
	}
	return s[:newWidth] + "..."
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

// FeaturePipes gates pipe query syntax (FEATURE_PIPES).
const FeaturePipes Feature = "PIPES"

// FeatureAllowConsecutiveOn gates consecutive ON/USING clauses in join
// expressions (FEATURE_ALLOW_CONSECUTIVE_ON).
const FeatureAllowConsecutiveOn Feature = "ALLOW_CONSECUTIVE_ON"

// FeatureIsDistinct gates "IS [NOT] DISTINCT FROM" comparisons
// (FEATURE_IS_DISTINCT).
const FeatureIsDistinct Feature = "IS_DISTINCT"

// featureInMaximum records whether each gated feature is enabled by
// language_features=MAXIMUM, i.e. whether it is ideally enabled and not in
// development; see LanguageOptions::EnableMaximumLanguageFeatures and the
// language_feature_options annotations in googlesql/public/options.proto.
var featureInMaximum = map[Feature]bool{
	FeatureWithGroupRows:      false, // in_development
	FeaturePipes:              true,
	FeatureAllowConsecutiveOn: true,
	FeatureIsDistinct:         true,
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
		if err := p.exceptClashError(); err != nil {
			return nil, err
		}
		if isKeyword(p.peek(), "OVER") {
			// When the OVER keyword is used in the wrong place, the
			// reference parser tells the user exactly where it can be used;
			// see MakeSyntaxError in parser_internal.cc.
			return nil, p.errorf(p.peek().Pos, "Syntax error: OVER keyword must follow a function call")
		}
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected end of input but got %s", describeToken(p.peek()))
	}
	return stmt, nil
}

type parser struct {
	sql      string
	toks     []token.Token
	pos      int
	features *FeatureSet
	// extents records the full token extent of expressions that were
	// parenthesized. In ZetaSQL's parse tree a parenthesized expression
	// keeps the location of the inner expression, but any enclosing
	// production's location (@$ in the LALR grammar) spans all consumed
	// tokens, including the parentheses. Keys are the inner expression
	// nodes; values are the [start, end) offsets including parentheses.
	extents map[ast.Node][2]int
	// allowDotStar is set while parsing a select column expression, where a
	// trailing ".*" may follow (see select_column_dot_star in googlesql.tm).
	// It is cleared while parsing any nested expression (and restored
	// afterwards), so that ".*" only ends a postfix expression of the select
	// column itself.
	allowDotStar bool
	// dotStarTarget records the postfix expression that stopped in front of
	// ".*" while allowDotStar was set. Grammar-wise ".*" binds more tightly
	// than any binary operator (select_column_dot_star takes an
	// expression_higher_prec_than_and with %prec "."), so the ".*" is only
	// valid when the whole select column expression is exactly this postfix
	// expression (e.g. "1+x.*" is an error).
	dotStarTarget ast.Node
}

// setExtent records that node n's full token extent is [start, end), wider
// than its own location because of wrapping parentheses.
func (p *parser) setExtent(n ast.Node, start, end int) {
	if p.extents == nil {
		p.extents = make(map[ast.Node][2]int)
	}
	p.extents[n] = [2]int{start, end}
}

// extStart returns the start offset of n's full token extent, including any
// wrapping parentheses not covered by the node's own location.
func (p *parser) extStart(n ast.Node) int {
	if ext, ok := p.extents[n]; ok {
		return ext[0]
	}
	return n.Pos()
}

// extEnd returns the end offset of n's full token extent, including any
// wrapping parentheses not covered by the node's own location.
func (p *parser) extEnd(n ast.Node) int {
	if ext, ok := p.extents[n]; ok {
		return ext[1]
	}
	return n.End()
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
	case token.SYSTEM_VARIABLE:
		// The lexer folds "@@name" into one token, but the reference lexes
		// "@@" separately and reports just that; see MakeSyntaxErrorAtToken
		// in googlesql/parser/parser_internal.cc.
		return `"@@"`
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
	"FULL": true, "GROUP": true, "HASH": true, "HAVING": true, "IN": true,
	"INNER":     true,
	"INTERSECT": true, "IS": true, "JOIN": true, "LATERAL": true, "LEFT": true,
	"LIKE":  true,
	"LIMIT": true, "LOOKUP": true, "NATURAL": true, "NOT": true, "NULL": true,
	"NULLS": true, "ON": true,
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
		if err := p.exceptClashError(); err != nil {
			return token.Token{}, err
		}
		return token.Token{}, p.errorf(p.peek().Pos, "Syntax error: Expected keyword %s but got %s", kw, describeToken(p.peek()))
	}
	return p.advance(), nil
}

func (p *parser) expect(kind token.Kind, what string) (token.Token, error) {
	if p.peek().Kind != kind {
		if err := p.exceptClashError(); err != nil {
			return token.Token{}, err
		}
		return token.Token{}, p.errorf(p.peek().Pos, "Syntax error: Expected %s but got %s", what, describeToken(p.peek()))
	}
	return p.advance(), nil
}

// exceptClashError returns the dedicated EXCEPT error if the parser is
// stopped at an EXCEPT keyword that is not followed by ALL, DISTINCT, "(", or
// a hint. Such an EXCEPT lexes as KW_EXCEPT_IN_UNEXPECTED_CONTEXT in the
// reference, and any syntax error at it produces this message; see
// MakeSyntaxErrorAtToken in googlesql/parser/parser_internal.cc and the
// KW_EXCEPT case in googlesql/parser/lookahead_transformer.cc.
func (p *parser) exceptClashError() error {
	tok := p.peek()
	if !isKeyword(tok, "EXCEPT") {
		return nil
	}
	next := p.peekAt(1)
	if isKeyword(next, "ALL") || isKeyword(next, "DISTINCT") || next.Kind == token.LPAREN {
		return nil
	}
	if next.Kind == token.ATSIGN {
		if k := p.peekAt(2).Kind; k == token.INT || k == token.LBRACE {
			return nil
		}
	}
	// No "Syntax error: " prefix, matching the reference.
	return p.errorf(tok.Pos, `EXCEPT must be followed by ALL, DISTINCT, or "("`)
}

func span(start, end int) ast.Span { return ast.Span{Start: start, Stop: end} }

func (p *parser) parseStatement() (ast.Statement, error) {
	tok := p.peek()
	switch {
	case isKeyword(tok, "SELECT"), isKeyword(tok, "FROM"), isKeyword(tok, "WITH"),
		isKeyword(tok, "TABLE"), tok.Kind == token.LPAREN:
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
	case isKeyword(tok, "CALL"):
		return p.parseCallStatement()
	case isKeyword(tok, "CREATE"):
		return p.parseCreateStatement()
	}
	return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
}

// parseCallStatement parses "CALL path ( [tvf_argument, ...] )"; see
// call_statement in googlesql.tm.
func (p *parser) parseCallStatement() (ast.Statement, error) {
	callTok := p.advance() // CALL
	proc, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	// parsePathExpression stops at anything other than ".", so a missing
	// argument list reports both continuations.
	if p.peek().Kind != token.LPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" or "." but got %s`, describeToken(p.peek()))
	}
	p.advance() // (
	stmt := &ast.CallStatement{Span: span(callTok.Pos, 0), Procedure: proc}
	if p.peek().Kind != token.RPAREN {
		for {
			arg, err := p.parseTVFArgument()
			if err != nil {
				return nil, err
			}
			stmt.Args = append(stmt.Args, arg)
			if p.peek().Kind != token.COMMA {
				break
			}
			p.advance() // ,
		}
	}
	if tok := p.peek(); tok.Kind != token.RPAREN {
		return nil, p.errorf(tok.Pos, `Syntax error: Expected ")" or "," but got %s`, describeToken(tok))
	}
	stmt.Stop = p.advance().End // )
	return stmt, nil
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
	case isKeyword(tok, "TABLE"):
		tc, err := p.parseTableClause()
		if err != nil {
			return nil, err
		}
		// A TABLE clause used as a query primary is wrapped in a Query node;
		// see the table_clause_reserved alternative of query_primary in
		// googlesql.tm.
		primary = &ast.Query{Span: span(tc.Pos(), tc.End()), QueryExpr: tc}
		primaryEnd = tc.End()
	case tok.Kind == token.LPAREN:
		inner, parenEnd, err := p.parseParenthesizedQuery()
		if err != nil {
			return nil, err
		}
		primary, primaryEnd = inner, parenEnd
	default:
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}

	if p.atSetOpMetadataStart() {
		setOp, err := p.parseSetOperationRest(primary, tok.Pos)
		if err != nil {
			return nil, err
		}
		primary, primaryEnd = setOp, setOp.End()
	}

	var orderBy *ast.OrderBy
	var limit *ast.LimitOffset
	var lockMode *ast.LockMode
	end := primaryEnd
	if isKeyword(p.peek(), "ORDER") {
		ob, err := p.parseOrderBy(false)
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

// atSetOpMetadataStart reports whether the tokens at the current position
// begin set operation metadata: an optional FULL/LEFT/INNER/OUTER outer mode
// prefix followed by UNION, INTERSECT, or an EXCEPT that lexes as a set
// operator. FULL/LEFT/INNER only lex as set operation keywords when followed
// by (OUTER)? UNION/INTERSECT/EXCEPT; see the KW_FULL/KW_LEFT/KW_INNER cases
// in googlesql/parser/lookahead_transformer.cc.
func (p *parser) atSetOpMetadataStart() bool {
	i := 0
	tok := p.peek()
	switch {
	case isKeyword(tok, "FULL"), isKeyword(tok, "LEFT"):
		i = 1
		if isKeyword(p.peekAt(1), "OUTER") {
			i = 2
		}
	case isKeyword(tok, "INNER"), isKeyword(tok, "OUTER"):
		i = 1
	}
	op := p.peekAt(i)
	if isKeyword(op, "UNION") || isKeyword(op, "INTERSECT") {
		return true
	}
	return isKeyword(op, "EXCEPT") && p.exceptIsSetOp(i)
}

// exceptIsSetOp reports whether the EXCEPT keyword at offset i from the
// current position lexes as a set operator (KW_EXCEPT_IN_SET_OP): it must be
// followed by ALL, DISTINCT, or a hint; see the KW_EXCEPT case in
// googlesql/parser/lookahead_transformer.cc.
func (p *parser) exceptIsSetOp(i int) bool {
	next := p.peekAt(i + 1)
	if isKeyword(next, "ALL") || isKeyword(next, "DISTINCT") {
		return true
	}
	if next.Kind == token.ATSIGN {
		if k := p.peekAt(i + 2).Kind; k == token.INT || k == token.LBRACE {
			return true
		}
	}
	return false
}

// parseSetOperationRest parses "(set_operation_metadata query_primary)+"
// following an already-parsed left query primary whose tokens start at
// firstStart; see query_set_operation_prefix in googlesql.tm. All metadata
// entries collect into one list and all operand queries become flat inputs.
func (p *parser) parseSetOperationRest(first ast.Node, firstStart int) (*ast.SetOperation, error) {
	mdl := &ast.SetOperationMetadataList{}
	setOp := &ast.SetOperation{Span: span(firstStart, 0), Metadata: mdl, Inputs: []ast.Node{first}}
	for p.atSetOpMetadataStart() {
		md, err := p.parseSetOperationMetadata()
		if err != nil {
			return nil, err
		}
		if len(mdl.Entries) == 0 {
			mdl.Start = md.Pos()
		}
		mdl.Entries = append(mdl.Entries, md)
		mdl.Stop = md.End()
		rhs, err := p.parseQueryPrimary()
		if err != nil {
			return nil, err
		}
		setOp.Inputs = append(setOp.Inputs, rhs)
		// The end covers all consumed tokens, which can exceed the operand
		// node's end when it is a parenthesized query.
		setOp.Stop = p.prevEnd()
	}
	return setOp, nil
}

// parseSetOperationMetadata parses one set operator: an optional outer mode
// prefix, the operator keyword, an optional hint, ALL or DISTINCT, an
// optional STRICT, and an optional column match suffix; see
// set_operation_metadata in googlesql.tm.
func (p *parser) parseSetOperationMetadata() (*ast.SetOperationMetadata, error) {
	start := p.peek().Pos

	// opt_corresponding_outer_mode.
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

	hint, err := p.parseOptionalHint()
	if err != nil {
		return nil, err
	}
	md.Hint = hint

	// all_or_distinct is required.
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

	// opt_strict.
	var strict *ast.SetOperationColumnPropagationMode
	if isKeyword(p.peek(), "STRICT") {
		stok := p.advance()
		strict = &ast.SetOperationColumnPropagationMode{Span: span(stok.Pos, stok.End), Value: "STRICT"}
		md.Stop = stok.End
	}

	// opt_column_match_suffix.
	switch {
	case isKeyword(p.peek(), "CORRESPONDING"):
		ctok := p.advance()
		if isKeyword(p.peek(), "BY") {
			btok := p.advance()
			md.ColumnMatchMode = &ast.SetOperationColumnMatchMode{Span: span(ctok.Pos, btok.End), Value: "CORRESPONDING_BY"}
			cols, err := p.parseColumnList()
			if err != nil {
				return nil, err
			}
			md.ColumnList = cols
			md.Stop = cols.End()
		} else {
			md.ColumnMatchMode = &ast.SetOperationColumnMatchMode{Span: span(ctok.Pos, ctok.End), Value: "CORRESPONDING"}
			md.Stop = ctok.End
		}
	case isKeyword(p.peek(), "BY") && isKeyword(p.peekAt(1), "NAME"):
		btok := p.advance()
		ntok := p.advance()
		if isKeyword(p.peek(), "ON") {
			otok := p.advance()
			md.ColumnMatchMode = &ast.SetOperationColumnMatchMode{Span: span(btok.Pos, otok.End), Value: "BY_NAME_ON"}
			cols, err := p.parseColumnList()
			if err != nil {
				return nil, err
			}
			md.ColumnList = cols
			md.Stop = cols.End()
		} else {
			md.ColumnMatchMode = &ast.SetOperationColumnMatchMode{Span: span(btok.Pos, ntok.End), Value: "BY_NAME"}
			md.Stop = ntok.End
		}
	}

	if strict != nil {
		// See the reduce action of set_operation_metadata in googlesql.tm.
		if outerMode != nil {
			return nil, p.errorf(strict.Pos(), "Syntax error: STRICT cannot be used with outer mode in set operations")
		}
		if md.ColumnMatchMode != nil && (md.ColumnMatchMode.Value == "BY_NAME" || md.ColumnMatchMode.Value == "BY_NAME_ON") {
			return nil, p.errorf(strict.Pos(), "Syntax error: STRICT cannot be used with BY NAME in set operations")
		}
		md.ColumnPropagationMode = strict
	}
	return md, nil
}

// parseColumnList parses "( identifier, ... )"; see column_list in
// googlesql.tm. The list's location includes the parentheses.
func (p *parser) parseColumnList() (*ast.ColumnList, error) {
	lparen, err := p.expect(token.LPAREN, `"("`)
	if err != nil {
		return nil, err
	}
	list := &ast.ColumnList{Span: span(lparen.Pos, 0)}
	for {
		tok := p.peek()
		if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
		}
		if isReserved(tok) {
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected keyword %s", strings.ToUpper(tok.Image))
		}
		list.Identifiers = append(list.Identifiers, p.parseIdentifierToken(p.advance()))
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance()
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	list.Stop = rparen.End
	return list, nil
}

// parseQueryPrimary parses one operand of a set operation: a SELECT, a TABLE
// clause, or a parenthesized query; see query_primary in googlesql.tm.
func (p *parser) parseQueryPrimary() (ast.Node, error) {
	tok := p.peek()
	switch {
	case isKeyword(tok, "SELECT"):
		return p.parseSelect()
	case isKeyword(tok, "TABLE"):
		tc, err := p.parseTableClause()
		if err != nil {
			return nil, err
		}
		return &ast.Query{Span: span(tc.Pos(), tc.End()), QueryExpr: tc}, nil
	case isKeyword(tok, "FROM"):
		// See the "FROM" alternatives of query_set_operation_prefix in
		// googlesql.tm.
		if p.features.Enabled(FeaturePipes) {
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected FROM; FROM queries following a set operation must be parenthesized")
		}
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected FROM")
	case tok.Kind == token.LPAREN:
		query, _, err := p.parseParenthesizedQuery()
		if err != nil {
			return nil, err
		}
		return query, nil
	}
	return nil, p.errorf(tok.Pos, `Syntax error: Expected "(" or keyword SELECT or keyword TABLE but got %s`, describeToken(tok))
}

// parseTableClause parses "TABLE path"; see table_clause in googlesql.tm.
// Table-valued function calls after TABLE are not implemented yet.
func (p *parser) parseTableClause() (*ast.TableClause, error) {
	tableTok := p.advance() // TABLE
	path, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	return &ast.TableClause{Span: span(tableTok.Pos, path.End()), Path: path}, nil
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
		orderBy, err := p.parseOrderBy(true)
		if err != nil {
			return nil, err
		}
		return &ast.PipeOrderBy{Span: span(pipeTok.Pos, orderBy.End()), OrderBy: orderBy}, nil
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
	case isKeyword(tok, "WINDOW"):
		return p.parsePipeWindow(pipeTok)
	case isKeyword(tok, "LIMIT"):
		limitOffset, err := p.parseLimitOffset()
		if err != nil {
			return nil, err
		}
		return &ast.PipeLimitOffset{Span: span(pipeTok.Pos, limitOffset.End()), LimitOffset: limitOffset}, nil
	case isKeyword(tok, "DISTINCT"):
		distinctTok := p.advance()
		return &ast.PipeDistinct{Span: span(pipeTok.Pos, distinctTok.End)}, nil
	case p.atSetOpMetadataStart():
		return p.parsePipeSetOperation(pipeTok)
	}
	if err := p.exceptClashError(); err != nil {
		return nil, err
	}
	// The last alternative is pipe_join; an unrecognized pipe operator gets
	// its "Expected keyword JOIN" error from the JOIN inside pipe_join.
	return p.parsePipeJoin(pipeTok)
}

// parsePipeSetOperation parses "<set_operation_metadata> (query|table_clause)
// [, ...][,]" after a |> token; see pipe_set_operation in googlesql.tm. Each
// operand is a parenthesized query or an unparenthesized TABLE clause. When
// the first operand is a parenthesized query the operator's location includes
// its closing parenthesis; operands appended after a comma extend the
// location only to the operand node's own end (which excludes the
// parentheses), and a trailing comma is not included.
func (p *parser) parsePipeSetOperation(pipeTok token.Token) (ast.Node, error) {
	md, err := p.parseSetOperationMetadata()
	if err != nil {
		return nil, err
	}
	node := &ast.PipeSetOperation{Span: span(pipeTok.Pos, 0), Metadata: md}
	for {
		tok := p.peek()
		switch {
		case tok.Kind == token.LPAREN:
			query, parenEnd, err := p.parseParenthesizedQuery()
			if err != nil {
				return nil, err
			}
			node.Inputs = append(node.Inputs, query)
			if len(node.Inputs) == 1 {
				node.Stop = parenEnd
			} else {
				node.Stop = query.End()
			}
		case isKeyword(tok, "TABLE"):
			tc, err := p.parseTableClause()
			if err != nil {
				return nil, err
			}
			node.Inputs = append(node.Inputs, tc)
			node.Stop = tc.End()
		default:
			return nil, p.errorf(tok.Pos, `Syntax error: Expected "(" or keyword TABLE but got %s`, describeToken(tok))
		}
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance() // ,
		next := p.peek()
		if next.Kind != token.LPAREN && !isKeyword(next, "TABLE") {
			// Trailing comma; see pipe_set_operation in googlesql.tm.
			break
		}
	}
	return node, nil
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
		item := &ast.PipeSetItem{Span: span(ident.Pos(), p.extEnd(expr)), Column: ident, Expr: expr}
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
			col, err := p.parseSelectColumnOrDotStar()
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

// parsePipeWindow parses "WINDOW pipe_selection_item_list" after a |> token.
// The selection list is represented as a Select node whose location starts at
// the WINDOW keyword; see pipe_window in googlesql.tm.
func (p *parser) parsePipeWindow(pipeTok token.Token) (ast.Node, error) {
	windowTok := p.advance() // WINDOW
	list, err := p.parsePipeSelectionItemList()
	if err != nil {
		return nil, err
	}
	sel := &ast.Select{Span: span(windowTok.Pos, list.End()), SelectList: list}
	return &ast.PipeWindow{Span: span(pipeTok.Pos, sel.End()), Select: sel}, nil
}

// parsePipeSelectionItemList parses one or more comma-separated selection
// items with an optional trailing comma; see pipe_selection_item_list in
// googlesql.tm.
func (p *parser) parsePipeSelectionItemList() (*ast.SelectList, error) {
	first, err := p.parseSelectColumnOrDotStar()
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
		col, err := p.parseSelectColumnOrDotStar()
		if err != nil {
			return nil, err
		}
		list.Columns = append(list.Columns, col)
		list.Stop = col.End()
	}
	return list, nil
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
	return &ast.WhereClause{Span: span(whereTok.Pos, p.extEnd(expr)), Expr: expr}, nil
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
		sel.Having = &ast.Having{Span: span(havingTok.Pos, p.extEnd(expr)), Expr: expr}
		sel.Stop = p.extEnd(expr)
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
	// "*" and "expression . *", with optional EXCEPT/REPLACE modifiers, are
	// select column forms that cannot take an alias; see select_column_star
	// and select_column_dot_star in googlesql.tm.
	if p.peek().Kind == token.STAR {
		star := p.advance()
		var expr ast.Node = &ast.Star{Span: span(star.Pos, star.End), Image: star.Image}
		mods, err := p.parseOptionalStarModifiers()
		if err != nil {
			return nil, err
		}
		if mods != nil {
			expr = &ast.StarWithModifiers{Span: span(star.Pos, mods.End()), Modifiers: mods}
		}
		return &ast.SelectColumn{Span: span(expr.Pos(), expr.End()), Expr: expr}, nil
	}
	return p.parseSelectColumnOrDotStar()
}

// parseSelectColumnOrDotStar parses a select column that is either
// "expression [[AS] alias]" or "expression . *" with optional EXCEPT/REPLACE
// modifiers (which cannot take an alias); see select_column_expr and
// select_column_dot_star in googlesql.tm. This is also the pipe selection
// item form, which excludes the plain "*" select column; see
// pipe_selection_item in googlesql.tm.
func (p *parser) parseSelectColumnOrDotStar() (*ast.SelectColumn, error) {
	p.allowDotStar = true
	p.dotStarTarget = nil
	expr, err := p.parseOr()
	p.allowDotStar = false
	if err != nil {
		return nil, err
	}
	if p.peek().Kind == token.DOT && p.peekAt(1).Kind == token.STAR {
		if expr != p.dotStarTarget {
			// ".*" binds more tightly than any binary operator, so it
			// cannot apply to a larger expression (e.g. "1+x.*").
			return nil, p.errorf(p.peekAt(1).Pos, `Syntax error: Unexpected "*"`)
		}
		p.advance() // .
		star := p.advance()
		start := p.extStart(expr)
		var dotStar ast.Node = &ast.DotStar{Span: span(start, star.End), Expr: expr}
		mods, err := p.parseOptionalStarModifiers()
		if err != nil {
			return nil, err
		}
		if mods != nil {
			dotStar = &ast.DotStarWithModifiers{Span: span(start, mods.End()), Expr: expr, Modifiers: mods}
		}
		return &ast.SelectColumn{Span: span(dotStar.Pos(), dotStar.End()), Expr: dotStar}, nil
	}
	if err := p.checkAttachedAlias(); err != nil {
		return nil, err
	}
	col := &ast.SelectColumn{Span: span(p.extStart(expr), p.extEnd(expr)), Expr: expr}
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

// parseOptionalStarModifiers parses [EXCEPT "(" identifier, ... ")"]
// [REPLACE "(" expression AS identifier, ... ")"] after "*" or ".*",
// returning nil when neither modifier is present; see star_modifiers in
// googlesql.tm. EXCEPT only starts a modifier list when directly followed by
// "(", mirroring the set operation disambiguation in the reference lexer
// (see the KW_EXCEPT case in googlesql/parser/lookahead_transformer.cc).
func (p *parser) parseOptionalStarModifiers() (*ast.StarModifiers, error) {
	hasExcept := isKeyword(p.peek(), "EXCEPT") && p.peekAt(1).Kind == token.LPAREN
	hasReplace := isKeyword(p.peek(), "REPLACE") && p.peekAt(1).Kind == token.LPAREN
	if !hasExcept && !hasReplace {
		return nil, nil
	}
	mods := &ast.StarModifiers{Span: span(p.peek().Pos, 0)}
	if hasExcept {
		exceptTok := p.advance()
		p.advance() // (
		list := &ast.StarExceptList{Span: span(exceptTok.Pos, 0)}
		for {
			tok := p.peek()
			if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
				return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
			}
			if isReserved(tok) {
				return nil, p.errorf(tok.Pos, "Syntax error: Unexpected keyword %s", strings.ToUpper(tok.Image))
			}
			list.Identifiers = append(list.Identifiers, p.parseIdentifierToken(p.advance()))
			if p.peek().Kind != token.COMMA {
				break
			}
			p.advance()
		}
		rparen, err := p.expect(token.RPAREN, `")" or ","`)
		if err != nil {
			return nil, err
		}
		list.Stop = rparen.End
		mods.ExceptList = list
		mods.Stop = rparen.End
	}
	if isKeyword(p.peek(), "REPLACE") && p.peekAt(1).Kind == token.LPAREN {
		p.advance() // REPLACE
		p.advance() // (
		for {
			expr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			if !isKeyword(p.peek(), "AS") {
				return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword AS but got %s", describeToken(p.peek()))
			}
			p.advance() // AS
			tok := p.peek()
			if tok.Kind != token.QUOTED_IDENT && (tok.Kind != token.IDENT || isReserved(tok)) {
				return nil, p.errorf(tok.Pos, "Syntax error: Expected identifier but got %s", describeToken(tok))
			}
			alias := p.parseIdentifierToken(p.advance())
			mods.ReplaceItems = append(mods.ReplaceItems, &ast.StarReplaceItem{
				Span:  span(p.extStart(expr), alias.End()),
				Expr:  expr,
				Alias: alias,
			})
			if p.peek().Kind != token.COMMA {
				break
			}
			p.advance()
		}
		rparen, err := p.expect(token.RPAREN, `")" or ","`)
		if err != nil {
			return nil, err
		}
		mods.Stop = rparen.End
	}
	return mods, nil
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
	contents, err := p.parseFromClauseContents()
	if err != nil {
		return nil, err
	}
	// Consecutive ON/USING clauses rewrite the join tree; see from_clause in
	// googlesql.tm.
	table, err := p.transformJoinExpression(contents)
	if err != nil {
		return nil, err
	}
	return &ast.FromClause{Span: span(fromTok.Pos, p.prevEnd()), TableExpression: table}, nil
}

// parseTablePrimary parses a single table item in a FROM clause: a
// parenthesized query used as a table subquery, a parenthesized join, or a
// table path expression; see table_primary in googlesql.tm.
func (p *parser) parseTablePrimary() (ast.Node, error) {
	if isKeyword(p.peek(), "LATERAL") {
		return p.parseLateralTablePrimary()
	}
	if p.peek().Kind == token.LPAREN {
		if p.lparenStartsQuery() {
			return p.parseTableSubquery()
		}
		return p.parseParenthesizedJoin()
	}
	return p.parseTablePathExpression()
}

// parseLateralTablePrimary parses "LATERAL table_subquery" or "LATERAL tvf
// [[AS] alias]"; see the LATERAL rules under table_primary in googlesql.tm.
// LATERAL applies only to table subqueries and TVF calls, and the resulting
// node's location starts at the LATERAL keyword.
func (p *parser) parseLateralTablePrimary() (ast.Node, error) {
	latTok := p.advance() // LATERAL
	if p.peek().Kind == token.LPAREN {
		node, err := p.parseTableSubquery()
		if err != nil {
			return nil, err
		}
		node.IsLateral = true
		node.Start = latTok.Pos
		return node, nil
	}
	// Anything other than a subquery must be a TVF call: a path expression
	// followed by an argument list.
	if tok := p.peek(); (tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT) || isReserved(tok) {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	path, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != token.LPAREN {
		if err := p.exceptClashError(); err != nil {
			return nil, err
		}
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" or "." but got %s`, describeToken(p.peek()))
	}
	tvf, err := p.parseTVFRest(path)
	if err != nil {
		return nil, err
	}
	tvf.IsLateral = true
	tvf.Start = latTok.Pos
	return tvf, nil
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
		// A table primary reports plain "Unexpected" errors rather than the
		// path expression's "Expected identifier" ones.
		if tok := p.peek(); tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
		}
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
			arg, err := p.parseTVFArgument()
			if err != nil {
				return nil, err
			}
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

// parseTVFArgument parses a single table-valued function (or CALL statement)
// argument: an expression, or a TABLE, MODEL, or CONNECTION clause; see
// tvf_argument in googlesql.tm. The keyword forms apply only when the
// keyword is followed by a token that can start the clause's operand, so a
// plain column reference named "table" still parses as an expression.
func (p *parser) parseTVFArgument() (*ast.TVFArgument, error) {
	tok := p.peek()
	isPathStart := func(t token.Token) bool {
		return (t.Kind == token.IDENT || t.Kind == token.QUOTED_IDENT) && !isReserved(t)
	}
	switch {
	case isKeyword(tok, "TABLE") && isPathStart(p.peekAt(1)):
		p.advance() // TABLE
		path, err := p.parsePathExpression()
		if err != nil {
			return nil, err
		}
		clause := &ast.TableClause{Span: span(tok.Pos, path.End()), Path: path}
		return &ast.TVFArgument{Span: clause.Span, Expr: clause}, nil
	case isKeyword(tok, "MODEL") && isPathStart(p.peekAt(1)):
		p.advance() // MODEL
		path, err := p.parsePathExpression()
		if err != nil {
			return nil, err
		}
		clause := &ast.ModelClause{Span: span(tok.Pos, path.End()), Path: path}
		return &ast.TVFArgument{Span: clause.Span, Expr: clause}, nil
	case isKeyword(tok, "CONNECTION") && (isPathStart(p.peekAt(1)) || isKeyword(p.peekAt(1), "DEFAULT")):
		p.advance() // CONNECTION
		var path ast.Node
		if def := p.peek(); isKeyword(def, "DEFAULT") {
			p.advance()
			path = &ast.DefaultLiteral{Span: span(def.Pos, def.End)}
		} else {
			pathExpr, err := p.parsePathExpression()
			if err != nil {
				return nil, err
			}
			path = pathExpr
		}
		clause := &ast.ConnectionClause{Span: span(tok.Pos, path.End()), Path: path}
		return &ast.TVFArgument{Span: clause.Span, Expr: clause}, nil
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	return &ast.TVFArgument{Span: span(p.extStart(expr), p.extEnd(expr)), Expr: expr}, nil
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
	node := &ast.ExpressionWithOptAlias{Span: span(p.extStart(expr), p.extEnd(expr)), Expr: expr}
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
		item := &ast.GroupingItem{Span: span(p.extStart(expr), p.extEnd(expr)), Expr: expr}
		groupBy.Items = append(groupBy.Items, item)
		groupBy.Stop = item.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance()
	}
	return groupBy, nil
}

// parseOrderBy parses "ORDER [hint] BY ordering_expression, ...". When
// allowTrailingComma is true (pipe ORDER BY), a trailing comma is accepted
// and included in the clause's location; see order_by_clause and
// order_by_clause_with_opt_comma in googlesql.tm.
func (p *parser) parseOrderBy(allowTrailingComma bool) (*ast.OrderBy, error) {
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
		comma := p.advance()
		if allowTrailingComma && !startsExpression(p.peek()) {
			// Trailing comma; it is included in the clause's location.
			orderBy.Stop = comma.End
			break
		}
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
	item := &ast.OrderingExpression{Span: span(p.extStart(expr), p.extEnd(expr)), Expr: expr}
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

// parseOptionalHint parses a "@<int>" and/or "@{name=value, ...}" hint if one
// starts at the current position; see hint in googlesql.tm.
func (p *parser) parseOptionalHint() (*ast.Hint, error) {
	if p.peek().Kind != token.ATSIGN {
		return nil, nil
	}
	at := p.advance() // @
	hint := &ast.Hint{Span: span(at.Pos, 0)}
	if p.peek().Kind == token.INT {
		it := p.advance()
		hint.NumShardsHint = &ast.IntLiteral{Span: span(it.Pos, it.End), Image: it.Image}
		hint.Stop = it.End
		if p.peek().Kind != token.ATSIGN || p.peekAt(1).Kind != token.LBRACE {
			return hint, nil
		}
		p.advance() // @
	}
	if _, err := p.expect(token.LBRACE, `"{"`); err != nil {
		return nil, err
	}
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
	// A nested expression (function argument, parenthesized expression,
	// subscript, ...) can never end at a select column's ".*". The flag is
	// restored afterwards so that a parenthesized expression can itself be
	// followed by ".*" (e.g. "select (1+x).*").
	saved := p.allowDotStar
	p.allowDotStar = false
	expr, err := p.parseOr()
	p.allowDotStar = saved
	return expr, err
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
		Span:      span(p.extStart(first), p.extEnd(disjuncts[len(disjuncts)-1])),
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
		Span:      span(p.extStart(first), p.extEnd(conjuncts[len(conjuncts)-1])),
		Conjuncts: conjuncts,
	}, nil
}

func (p *parser) parseNot() (ast.Node, error) {
	if isKeyword(p.peek(), "NOT") {
		notTok := p.advance()
		p.allowDotStar = false
		operand, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &ast.UnaryExpression{
			Span:    span(notTok.Pos, p.extEnd(operand)),
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

	// The reference lexes a NOT followed by BETWEEN, IN, LIKE, or DISTINCT
	// as KW_NOT_SPECIAL (see lookahead_transformer.cc); after an expression
	// it must introduce NOT BETWEEN, NOT IN, or NOT LIKE (NOT DISTINCT is
	// only valid after IS, handled below). Any other postfix NOT is left
	// for the caller to report.
	notTok := token.Token{Pos: -1}
	if isKeyword(p.peek(), "NOT") {
		next := p.peekAt(1)
		switch {
		case isKeyword(next, "BETWEEN"), isKeyword(next, "IN"), isKeyword(next, "LIKE"):
			notTok = p.advance()
		case isKeyword(next, "DISTINCT"):
			return nil, p.errorf(next.Pos, "Syntax error: Expected keyword BETWEEN or keyword IN or keyword LIKE but got %s", describeToken(next))
		}
	}

	// [NOT] BETWEEN
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
		return p.finishComparison(&ast.BetweenExpression{
			Span:            span(p.extStart(lhs), p.extEnd(high)),
			IsNot:           notTok.Pos >= 0,
			Lhs:             lhs,
			BetweenLocation: &ast.Location{Span: span(betweenTok.Pos, betweenTok.End)},
			Low:             low,
			High:            high,
		})
	}

	// [NOT] IN; see the in_operator alternatives of
	// expression_higher_prec_than_and in googlesql.tm.
	if isKeyword(p.peek(), "IN") {
		inTok := p.advance()
		in := &ast.InExpression{
			IsNot:      notTok.Pos >= 0,
			Lhs:        lhs,
			InLocation: &ast.Location{Span: span(inTok.Pos, inTok.End)},
		}
		var end int
		switch {
		case isKeyword(p.peek(), "UNNEST"):
			unnest, err := p.parseUnnestExpression()
			if err != nil {
				return nil, err
			}
			in.UnnestExpr = unnest
			end = unnest.End()
		case p.peek().Kind == token.LPAREN:
			query, list, rhsEnd, err := p.parseInRhs(false)
			if err != nil {
				return nil, err
			}
			in.Query, in.List = query, list
			end = rhsEnd
		default:
			return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
		}
		in.Span = span(p.extStart(lhs), end)
		return p.finishComparison(in)
	}

	// [NOT] LIKE, either the plain binary operator or the quantified
	// LIKE ANY/SOME/ALL form; see the like_operator alternatives of
	// expression_higher_prec_than_and in googlesql.tm.
	if isKeyword(p.peek(), "LIKE") {
		likeTok := p.advance()
		if isKeyword(p.peek(), "ANY") || isKeyword(p.peek(), "SOME") || isKeyword(p.peek(), "ALL") {
			opTok := p.advance()
			like := &ast.LikeExpression{
				IsNot:        notTok.Pos >= 0,
				Lhs:          lhs,
				LikeLocation: &ast.Location{Span: span(likeTok.Pos, likeTok.End)},
				Op:           &ast.AnySomeAllOp{Span: span(opTok.Pos, opTok.End), Op: strings.ToUpper(opTok.Image)},
			}
			var end int
			switch {
			case isKeyword(p.peek(), "UNNEST"):
				unnest, err := p.parseUnnestExpression()
				if err != nil {
					return nil, err
				}
				like.UnnestExpr = unnest
				end = unnest.End()
			case p.peek().Kind == token.LPAREN:
				query, list, rhsEnd, err := p.parseInRhs(true)
				if err != nil {
					return nil, err
				}
				like.Query, like.List = query, list
				end = rhsEnd
			default:
				return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
			}
			like.Span = span(p.extStart(lhs), end)
			return p.finishComparison(like)
		}
		rhs, err := p.parseBitwiseOr()
		if err != nil {
			return nil, err
		}
		return p.finishComparison(&ast.BinaryExpression{
			Span:  span(p.extStart(lhs), p.extEnd(rhs)),
			Op:    "LIKE",
			IsNot: notTok.Pos >= 0,
			Left:  lhs,
			Right: rhs,
		})
	}

	// IS [NOT] NULL / TRUE / FALSE / DISTINCT FROM
	if isKeyword(p.peek(), "IS") {
		isTok := p.advance()
		isNot := false
		if isKeyword(p.peek(), "NOT") {
			p.advance()
			isNot = true
		}
		tok := p.peek()
		var rhs ast.Node
		switch {
		case isKeyword(tok, "DISTINCT"):
			// IS [NOT] DISTINCT FROM; error messages point at the DISTINCT
			// for the NOT form and at the IS otherwise (see
			// distinct_operator in googlesql.tm).
			if !p.features.Enabled(FeatureIsDistinct) {
				pos := isTok.Pos
				if isNot {
					pos = tok.Pos
				}
				// No "Syntax error: " prefix; see the distinct_operator
				// alternative of expression_higher_prec_than_and.
				return nil, p.errorf(pos, "IS DISTINCT FROM is not supported")
			}
			p.advance() // DISTINCT
			if !isKeyword(p.peek(), "FROM") {
				return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
			}
			p.advance() // FROM
			rhs, err := p.parseBitwiseOr()
			if err != nil {
				return nil, err
			}
			return p.finishComparison(&ast.BinaryExpression{
				Span:  span(p.extStart(lhs), p.extEnd(rhs)),
				Op:    "IS DISTINCT FROM",
				IsNot: isNot,
				Left:  lhs,
				Right: rhs,
			})
		case isKeyword(tok, "NULL"):
			p.advance()
			rhs = &ast.NullLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image}
		case isKeyword(tok, "TRUE"), isKeyword(tok, "FALSE"):
			p.advance()
			rhs = &ast.BooleanLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image, Value: isKeyword(tok, "TRUE")}
		default:
			return nil, p.errorf(tok.Pos, "Syntax error: Expected NULL, TRUE, or FALSE after IS")
		}
		return p.finishComparison(&ast.BinaryExpression{
			Span:  span(p.extStart(lhs), p.extEnd(rhs)),
			Op:    "IS",
			IsNot: isNot,
			Left:  lhs,
			Right: rhs,
		})
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
	return p.finishComparison(&ast.BinaryExpression{
		Span:  span(p.extStart(lhs), p.extEnd(rhs)),
		Op:    op,
		Left:  lhs,
		Right: rhs,
	})
}

// finishComparison enforces non-associativity of the comparison level. A
// LIKE after a comparison result gets a dedicated message because the
// reference shifts it and then rejects the lhs (IsAllowedInComparison is
// false; see the like_operator alternatives of
// expression_higher_prec_than_and in googlesql.tm); the other comparison
// operators fail immediately on the %nonassoc conflict.
func (p *parser) finishComparison(n ast.Node) (ast.Node, error) {
	tok := p.peek()
	switch {
	case isKeyword(tok, "LIKE"):
		return nil, p.errorf(tok.Pos, "Syntax error: Expression to the left of LIKE must be parenthesized")
	case isKeyword(tok, "IN"), isKeyword(tok, "IS"), isKeyword(tok, "BETWEEN"):
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected keyword %s", strings.ToUpper(tok.Image))
	}
	return n, nil
}

// parseInRhs parses the parenthesized right-hand side of an IN or (with
// quantified set) a LIKE ANY/SOME/ALL expression, with the opening
// parenthesis as the next token. Exactly one of query and list is returned,
// along with the end offset of the closing parenthesis; see
// parenthesized_in_rhs and parenthesized_anysomeall_list_rhs in
// googlesql.tm.
func (p *parser) parseInRhs(quantified bool) (query *ast.Query, list *ast.InList, end int, err error) {
	lparen := p.peek()
	var qerr error
	if p.lparenStartsQuery() {
		save := p.pos
		p.advance() // (
		inner, ierr := p.parseQuery()
		var rparen token.Token
		if ierr == nil {
			rparen, ierr = p.expect(token.RPAREN, `")"`)
		}
		if ierr == nil {
			if quantified && inner.Parenthesized {
				// An extra-parenthesized subquery is a single scalar
				// subquery expression element (see case 4 of
				// parenthesized_anysomeall_list_rhs).
				inner.Parenthesized = false
				sub := &ast.ExpressionSubquery{Span: span(lparen.Pos, rparen.End), Query: inner}
				list = &ast.InList{Span: span(lparen.Pos, rparen.End), Exprs: []ast.Node{sub}}
				return nil, list, rparen.End, nil
			}
			if quantified {
				// The subquery rhs of a quantified expression gets an extra
				// Query node spanning the parentheses.
				return &ast.Query{Span: span(lparen.Pos, rparen.End), QueryExpr: inner}, nil, rparen.End, nil
			}
			inner.Parenthesized = true
			return inner, nil, rparen.End, nil
		}
		// Not a parenthesized query after all (e.g. "((select 1), x)");
		// retry as an expression list and keep whichever error got further.
		qerr = ierr
		p.pos = save
	}
	// "( expression [, ...] )" is an in-list; its location spans the
	// expressions but not the parentheses (see in_list_two_or_more_prefix
	// in googlesql.tm).
	p.advance() // (
	var exprs []ast.Node
	for {
		expr, eerr := p.parseExpression()
		if eerr != nil {
			return nil, nil, 0, p.preferError(qerr, eerr)
		}
		exprs = append(exprs, expr)
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance() // ,
	}
	rparen, rerr := p.expect(token.RPAREN, `")"`)
	if rerr != nil {
		return nil, nil, 0, p.preferError(qerr, rerr)
	}
	list = &ast.InList{
		Span:  span(p.extStart(exprs[0]), p.extEnd(exprs[len(exprs)-1])),
		Exprs: exprs,
	}
	return nil, list, rparen.End, nil
}

// preferError combines the error from an abandoned query parse with the
// error from the expression-list parse of the same input, keeping whichever
// consumed more input.
func (p *parser) preferError(qerr, eerr error) error {
	if qerr == nil {
		return eerr
	}
	return furthestError(qerr, eerr)
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
			Span: span(p.extStart(lhs), p.extEnd(rhs)),
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
		p.allowDotStar = false
		operand, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &ast.UnaryExpression{
			Span:    span(tok.Pos, p.extEnd(operand)),
			Op:      tok.Image,
			Operand: operand,
		}, nil
	}
	return p.parsePostfix()
}

// parsePostfix parses a primary expression followed by postfix operators:
// ". identifier" (generalized field access) and "[ expression ]" (array
// element access); see the expression_higher_prec_than_and rules in
// googlesql.tm.
func (p *parser) parsePostfix() (ast.Node, error) {
	expr, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		switch p.peek().Kind {
		case token.DOT:
			next := p.peekAt(1)
			if next.Kind != token.IDENT && next.Kind != token.QUOTED_IDENT {
				if next.Kind == token.STAR && p.allowDotStar {
					// Stop in front of a select column's ".*" and record
					// which expression it binds to; see
					// select_column_dot_star in googlesql.tm.
					p.dotStarTarget = expr
				}
				return expr, nil
			}
			p.advance() // .
			ident := p.parseIdentifierToken(p.advance())
			// A non-parenthesized path expression is extended in place;
			// anything else becomes a generalized DotIdentifier (see the
			// expression_higher_prec_than_and "." identifier rule in
			// googlesql.tm).
			if path, ok := expr.(*ast.PathExpression); ok {
				if _, parenthesized := p.extents[path]; !parenthesized {
					path.Names = append(path.Names, ident)
					path.Stop = ident.End()
					continue
				}
			}
			expr = &ast.DotIdentifier{
				Span: span(p.extStart(expr), ident.End()),
				Expr: expr,
				Name: ident,
			}
		case token.LPAREN:
			// "expression ( ... )" is a function call, which requires a
			// (generalized) path expression; chained calls on paths are
			// handled elsewhere, and anything else is an error (see
			// function_call_expression_base in googlesql.tm).
			switch expr.(type) {
			case *ast.PathExpression, *ast.DotIdentifier, *ast.FunctionCall:
				return expr, nil
			}
			return nil, p.errorf(p.peek().Pos, "Syntax error: Function call cannot be applied to this expression. Function calls require a path, e.g. a.b.c()")
		case token.LBRACKET:
			// "expression [ expression ]" is array element access; the
			// Location child covers the "[" token (see the
			// expression_higher_prec_than_and "[" rule in googlesql.tm).
			lbracket := p.advance()
			position, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			rbracket, err := p.expect(token.RBRACKET, `"]"`)
			if err != nil {
				return nil, err
			}
			expr = &ast.ArrayElement{
				Span:            span(p.extStart(expr), rbracket.End),
				Array:           expr,
				BracketLocation: &ast.Location{Span: span(lbracket.Pos, lbracket.End)},
				Position:        position,
			}
		default:
			return expr, nil
		}
	}
}

// parseCaseExpression parses "CASE [value] WHEN expr THEN expr ...
// [ELSE expr] END" with the CASE keyword as the next token; see
// case_no_value_expression_prefix and case_value_expression_prefix in
// googlesql.tm.
func (p *parser) parseCaseExpression() (ast.Node, error) {
	caseTok := p.advance() // CASE
	var value ast.Node
	if !isKeyword(p.peek(), "WHEN") {
		var err error
		value, err = p.parseExpression()
		if err != nil {
			return nil, err
		}
	}
	var args []ast.Node
	for {
		if _, err := p.expectKeyword("WHEN"); err != nil {
			return nil, err
		}
		when, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		if _, err := p.expectKeyword("THEN"); err != nil {
			return nil, err
		}
		then, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		args = append(args, when, then)
		if !isKeyword(p.peek(), "WHEN") {
			break
		}
	}
	if isKeyword(p.peek(), "ELSE") {
		p.advance()
		elseExpr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		args = append(args, elseExpr)
	}
	end, err := p.expectKeyword("END")
	if err != nil {
		return nil, err
	}
	if value != nil {
		return &ast.CaseValueExpression{
			Span:      span(caseTok.Pos, end.End),
			Arguments: append([]ast.Node{value}, args...),
		}, nil
	}
	return &ast.CaseNoValueExpression{
		Span:      span(caseTok.Pos, end.End),
		Arguments: args,
	}, nil
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
	case token.QUESTION:
		// Positional parameters are numbered left to right; see
		// parameter_expression in googlesql.tm.
		p.advance()
		return &ast.ParameterExpr{
			Span:     span(tok.Pos, tok.End),
			Position: p.positionalParameterOrdinal(),
		}, nil
	case token.PARAM:
		// The token image is "@name"; the identifier starts after "@". See
		// named_parameter_expression in googlesql.tm.
		p.advance()
		name := &ast.Identifier{Span: span(tok.Pos+1, tok.End), Name: tok.Image[1:]}
		return &ast.ParameterExpr{Span: span(tok.Pos, tok.End), Name: name}, nil
	case token.SYSTEM_VARIABLE:
		return p.parseSystemVariableExpr()
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
		case isKeyword(tok, "CAST"),
			isKeyword(tok, "SAFE_CAST") && p.peekAt(1).Kind == token.LPAREN:
			// SAFE_CAST is non-reserved: it is only the cast keyword when
			// followed by "(" (see keywords.cc); otherwise it falls through
			// to the identifier cases below.
			return p.parseCastExpression()
		case isKeyword(tok, "ARRAY") && p.peekAt(1).Kind == token.LBRACKET:
			p.advance() // ARRAY; the constructor's span starts at the keyword.
			return p.parseArrayConstructor(tok.Pos)
		case isKeyword(tok, "ARRAY") && p.peekAt(1).Kind == token.LT:
			// "ARRAY<type>[...]" is an array constructor with an explicit
			// element type; see array_constructor_prefix_no_expressions in
			// googlesql.tm.
			return p.parseTypedArrayConstructor()
		case isKeyword(tok, "ARRAY") && p.peekAt(1).Kind == token.LPAREN:
			p.advance() // ARRAY; the subquery's span starts at the keyword.
			return p.parseModifiedSubquery(tok.Pos, "ARRAY")
		case isKeyword(tok, "EXISTS") && p.peekAt(1).Kind == token.LPAREN:
			p.advance() // EXISTS; the subquery's span starts at the keyword.
			return p.parseModifiedSubquery(tok.Pos, "EXISTS")
		case isKeyword(tok, "CASE"):
			return p.parseCaseExpression()
		case isReserved(tok):
			if err := p.exceptClashError(); err != nil {
				return nil, err
			}
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
	return isKeyword(tok, "SELECT") || isKeyword(tok, "WITH") || isKeyword(tok, "FROM") ||
		isKeyword(tok, "TABLE")
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
	rparen := p.advance()
	// Parenthesized expressions keep the span of the inner expression in
	// ZetaSQL's parse tree; the parentheses only affect grouping, but
	// enclosing productions span them (see parser.extents).
	p.setExtent(expr, lparen.Pos, rparen.End)
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
		!isKeyword(tok, "PARTITION") && !isKeyword(tok, "ORDER") &&
		!isKeyword(tok, "ROWS") && !isKeyword(tok, "RANGE") {
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
		orderBy, err := p.parseOrderBy(false)
		if err != nil {
			return nil, err
		}
		windowSpec.OrderBy = orderBy
	}
	if isKeyword(p.peek(), "ROWS") || isKeyword(p.peek(), "RANGE") {
		frame, err := p.parseWindowFrame()
		if err != nil {
			return nil, err
		}
		windowSpec.WindowFrame = frame
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	windowSpec.Stop = rparen.End
	return windowSpec, nil
}

// parseWindowFrame parses "ROWS|RANGE window_frame_bound" or
// "ROWS|RANGE BETWEEN window_frame_bound AND window_frame_bound"; see
// window_frame_clause in googlesql.tm.
func (p *parser) parseWindowFrame() (*ast.WindowFrame, error) {
	unitTok := p.advance() // ROWS or RANGE
	frame := &ast.WindowFrame{Span: span(unitTok.Pos, 0), Unit: strings.ToUpper(unitTok.Image)}
	if isKeyword(p.peek(), "BETWEEN") {
		p.advance()
		low, err := p.parseWindowFrameExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expectKeyword("AND"); err != nil {
			return nil, err
		}
		high, err := p.parseWindowFrameExpr()
		if err != nil {
			return nil, err
		}
		frame.StartExpr, frame.EndExpr = low, high
		frame.Stop = high.End()
	} else {
		bound, err := p.parseWindowFrameExpr()
		if err != nil {
			return nil, err
		}
		frame.StartExpr = bound
		frame.Stop = bound.End()
	}
	return frame, nil
}

// parseWindowFrameExpr parses a window frame boundary: "UNBOUNDED
// PRECEDING/FOLLOWING", "CURRENT ROW" or "expression PRECEDING/FOLLOWING";
// see window_frame_bound in googlesql.tm.
func (p *parser) parseWindowFrameExpr() (*ast.WindowFrameExpr, error) {
	tok := p.peek()
	switch {
	case isKeyword(tok, "UNBOUNDED"):
		p.advance()
		dir, err := p.parsePrecedingOrFollowing()
		if err != nil {
			return nil, err
		}
		return &ast.WindowFrameExpr{
			Span:         span(tok.Pos, dir.End),
			BoundaryType: "UNBOUNDED " + strings.ToUpper(dir.Image),
		}, nil
	case isKeyword(tok, "CURRENT"):
		p.advance()
		rowTok, err := p.expectKeyword("ROW")
		if err != nil {
			return nil, err
		}
		return &ast.WindowFrameExpr{
			Span:         span(tok.Pos, rowTok.End),
			BoundaryType: "CURRENT ROW",
		}, nil
	default:
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		dir, err := p.parsePrecedingOrFollowing()
		if err != nil {
			return nil, err
		}
		return &ast.WindowFrameExpr{
			Span:         span(p.extStart(expr), dir.End),
			BoundaryType: "OFFSET " + strings.ToUpper(dir.Image),
			Expression:   expr,
		}, nil
	}
}

// parsePrecedingOrFollowing parses the PRECEDING or FOLLOWING keyword after
// a window frame boundary; see preceding_or_following in googlesql.tm.
func (p *parser) parsePrecedingOrFollowing() (token.Token, error) {
	tok := p.peek()
	if !isKeyword(tok, "PRECEDING") && !isKeyword(tok, "FOLLOWING") {
		return token.Token{}, p.errorf(tok.Pos, "Syntax error: Expected keyword FOLLOWING or keyword PRECEDING but got %s", describeToken(tok))
	}
	return p.advance(), nil
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
		partitionBy.Stop = p.extEnd(expr)
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
		// In a select column a path may stop before a ".*"; parsePostfix
		// records it as the dot-star target. A path that is not the whole
		// column expression fails the target check there instead.
		if p.allowDotStar && p.peekAt(1).Kind == token.STAR {
			return path, nil
		}
		p.advance()
		tok := p.peek()
		if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
		}
		ident := p.parseIdentifierToken(p.advance())
		path.Names = append(path.Names, ident)
		path.Stop = ident.End()
	}
	return path, nil
}

// parseSystemVariableExpr parses a system variable reference "@@path"; see
// system_variable_expression in googlesql.tm. The lexer emits "@@" plus the
// first name as one token; subsequent ".name" segments extend the path
// ("@@a.b" is a single system variable named "a.b").
func (p *parser) parseSystemVariableExpr() (ast.Node, error) {
	tok := p.advance() // @@name
	first := &ast.Identifier{Span: span(tok.Pos+2, tok.End), Name: tok.Image[2:]}
	path := &ast.PathExpression{Span: span(first.Pos(), first.End()), Names: []*ast.Identifier{first}}
	for p.peek().Kind == token.DOT {
		p.advance()
		next := p.peek()
		if next.Kind != token.IDENT && next.Kind != token.QUOTED_IDENT {
			return nil, p.errorf(next.Pos, "Syntax error: Unexpected %s", describeToken(next))
		}
		ident := p.parseIdentifierToken(p.advance())
		path.Names = append(path.Names, ident)
		path.Stop = ident.End()
	}
	return &ast.SystemVariableExpr{Span: span(tok.Pos, path.End()), Path: path}, nil
}

// positionalParameterOrdinal returns the 1-based ordinal of the "?"
// parameter token just consumed. Ordinals count "?" tokens left to right in
// the token stream (see parameter_expression in googlesql.tm); deriving the
// ordinal from token positions keeps it stable across parser backtracking.
func (p *parser) positionalParameterOrdinal() int {
	n := 0
	for _, tok := range p.toks[:p.pos] {
		if tok.Kind == token.QUESTION {
			n++
		}
	}
	return n
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

// parseCastExpression parses "CAST(expr AS type [FORMAT ...])" or
// "SAFE_CAST(...)"; see cast_expression in googlesql.tm.
func (p *parser) parseCastExpression() (ast.Node, error) {
	kw := p.advance() // CAST or SAFE_CAST
	isSafe := strings.EqualFold(kw.Image, "SAFE_CAST")
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	if isKeyword(p.peek(), "SELECT") {
		name := "CAST"
		if isSafe {
			name = "SAFE_CAST"
		}
		// Dedicated error without the "Syntax error: " prefix, matching the
		// reference grammar rule.
		return nil, p.errorf(p.peek().Pos, "The argument to %s is an expression, not a query; to use a query as an expression, the query must be wrapped with additional parentheses to make it a scalar subquery expression", name)
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("AS"); err != nil {
		return nil, err
	}
	typ, err := p.parseType()
	if err != nil {
		return nil, err
	}
	var format *ast.FormatClause
	if isKeyword(p.peek(), "FORMAT") {
		format, err = p.parseFormatClause()
		if err != nil {
			return nil, err
		}
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	return &ast.CastExpression{
		Span:       span(kw.Pos, rparen.End),
		Expr:       expr,
		Type:       typ,
		Format:     format,
		IsSafeCast: isSafe,
	}, nil
}

// parseFormatClause parses "FORMAT expr [AT TIME ZONE expr]"; see format and
// at_time_zone in googlesql.tm.
func (p *parser) parseFormatClause() (*ast.FormatClause, error) {
	formatTok := p.advance() // FORMAT
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	fc := &ast.FormatClause{Span: span(formatTok.Pos, p.extEnd(expr)), Format: expr}
	if isKeyword(p.peek(), "AT") {
		p.advance() // AT
		if _, err := p.expectKeyword("TIME"); err != nil {
			return nil, err
		}
		if _, err := p.expectKeyword("ZONE"); err != nil {
			return nil, err
		}
		tz, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		fc.TimeZone = tz
		fc.Stop = tz.End()
	}
	return fc, nil
}

// startsType reports whether tok can begin a type; used to disambiguate
// "name type" from a bare type in struct fields.
func startsType(tok token.Token) bool {
	if tok.Kind == token.QUOTED_IDENT {
		return true
	}
	if tok.Kind != token.IDENT {
		return false
	}
	if !isReserved(tok) {
		return true
	}
	return isKeyword(tok, "ARRAY") || isKeyword(tok, "STRUCT") ||
		isKeyword(tok, "RANGE") || isKeyword(tok, "INTERVAL")
}

// parseType parses a type with optional type parameters and collation; see
// the type rule in googlesql.tm: raw_type type_parameters? collate_clause?.
func (p *parser) parseType() (ast.Node, error) {
	raw, err := p.parseRawType()
	if err != nil {
		return nil, err
	}
	var params *ast.TypeParameterList
	if p.peek().Kind == token.LPAREN {
		params, err = p.parseTypeParameterList()
		if err != nil {
			return nil, err
		}
	}
	var collate *ast.Collate
	if isKeyword(p.peek(), "COLLATE") {
		collate, err = p.parseCollate()
		if err != nil {
			return nil, err
		}
	}
	if params == nil && collate == nil {
		return raw, nil
	}
	// The reference extends the raw type node through the type parameters
	// and collation (ExtendNodeRight with @$.end() in the type rule), so
	// the closing ")" of the parameter list is included in the type's span
	// even though TypeParameterList itself excludes it. prevEnd is only
	// valid here because a parameter list or collation was consumed; right
	// after a split ">>" token the previous token's end would be stale.
	end := p.prevEnd()
	switch t := raw.(type) {
	case *ast.SimpleType:
		t.TypeParameters, t.Collate, t.Stop = params, collate, end
	case *ast.ArrayType:
		t.TypeParameters, t.Collate, t.Stop = params, collate, end
	case *ast.StructType:
		t.TypeParameters, t.Collate, t.Stop = params, collate, end
	case *ast.RangeType:
		t.TypeParameters, t.Collate, t.Stop = params, collate, end
	}
	return raw, nil
}

// parseRawType parses a type without parameters or collation; see raw_type
// in googlesql.tm.
func (p *parser) parseRawType() (ast.Node, error) {
	tok := p.peek()
	switch {
	case isKeyword(tok, "ARRAY"):
		return p.parseArrayType()
	case isKeyword(tok, "STRUCT"):
		return p.parseStructType()
	case isKeyword(tok, "RANGE"):
		return p.parseRangeType()
	case isKeyword(tok, "INTERVAL"):
		// INTERVAL is a reserved keyword but still names a type; see
		// type_name in googlesql.tm.
		id := p.parseIdentifierToken(p.advance())
		path := &ast.PathExpression{Span: span(tok.Pos, tok.End), Names: []*ast.Identifier{id}}
		return &ast.SimpleType{Span: span(tok.Pos, tok.End), Name: path}, nil
	}
	if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	path, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	return &ast.SimpleType{Span: span(path.Pos(), path.End()), Name: path}, nil
}

// expectTemplateOpen consumes the "<" opening a template type.
func (p *parser) expectTemplateOpen() (token.Token, error) {
	return p.expect(token.LT, `"<"`)
}

// expectTemplateClose consumes the ">" closing a template type. A ">>" token
// (as in "ARRAY<STRUCT<int64>>") is split so its second ">" can close the
// enclosing template type; the reference lexer does this with lookback
// overrides (see template_type_close in googlesql.tm).
func (p *parser) expectTemplateClose() (token.Token, error) {
	tok := p.peek()
	if tok.Kind == token.RSHIFT {
		p.toks[p.pos] = token.Token{Kind: token.GT, Image: ">", Pos: tok.Pos + 1, End: tok.End}
		return token.Token{Kind: token.GT, Image: ">", Pos: tok.Pos, End: tok.Pos + 1}, nil
	}
	return p.expect(token.GT, `">"`)
}

// parseArrayType parses "ARRAY<type>"; see array_type in googlesql.tm.
func (p *parser) parseArrayType() (*ast.ArrayType, error) {
	arrayTok := p.advance() // ARRAY
	if _, err := p.expectTemplateOpen(); err != nil {
		return nil, err
	}
	elem, err := p.parseType()
	if err != nil {
		return nil, err
	}
	closeTok, err := p.expectTemplateClose()
	if err != nil {
		return nil, err
	}
	return &ast.ArrayType{Span: span(arrayTok.Pos, closeTok.End), ElementType: elem}, nil
}

// parseRangeType parses "RANGE<type>"; see range_type in googlesql.tm.
func (p *parser) parseRangeType() (*ast.RangeType, error) {
	rangeTok := p.advance() // RANGE
	if _, err := p.expectTemplateOpen(); err != nil {
		return nil, err
	}
	elem, err := p.parseType()
	if err != nil {
		return nil, err
	}
	closeTok, err := p.expectTemplateClose()
	if err != nil {
		return nil, err
	}
	return &ast.RangeType{Span: span(rangeTok.Pos, closeTok.End), ElementType: elem}, nil
}

// parseStructType parses "STRUCT<field, ...>" (possibly empty); see
// struct_type in googlesql.tm.
func (p *parser) parseStructType() (*ast.StructType, error) {
	structTok := p.advance() // STRUCT
	if _, err := p.expectTemplateOpen(); err != nil {
		return nil, err
	}
	st := &ast.StructType{Span: span(structTok.Pos, 0)}
	if p.peek().Kind != token.GT && p.peek().Kind != token.RSHIFT {
		for {
			field, err := p.parseStructField()
			if err != nil {
				return nil, err
			}
			st.Fields = append(st.Fields, field)
			if p.peek().Kind != token.COMMA {
				break
			}
			p.advance()
		}
	}
	closeTok, err := p.expectTemplateClose()
	if err != nil {
		return nil, err
	}
	st.Stop = closeTok.End
	return st, nil
}

// parseStructField parses one "[name] type" struct field; see struct_field
// in googlesql.tm.
func (p *parser) parseStructField() (*ast.StructField, error) {
	tok := p.peek()
	named := (tok.Kind == token.QUOTED_IDENT || (tok.Kind == token.IDENT && !isReserved(tok))) &&
		startsType(p.peekAt(1))
	var name *ast.Identifier
	if named {
		name = p.parseIdentifierToken(p.advance())
	}
	typ, err := p.parseType()
	if err != nil {
		return nil, err
	}
	start := typ.Pos()
	if name != nil {
		start = name.Pos()
	}
	return &ast.StructField{Span: span(start, typ.End()), Name: name, Type: typ}, nil
}

// parseTypeParameterList parses "(param, ...)" after a type name; see
// type_parameters in googlesql.tm. The node's span excludes the closing ")".
func (p *parser) parseTypeParameterList() (*ast.TypeParameterList, error) {
	lparen := p.advance() // (
	list := &ast.TypeParameterList{Span: span(lparen.Pos, 0)}
	for {
		param, err := p.parseTypeParameter()
		if err != nil {
			return nil, err
		}
		list.Parameters = append(list.Parameters, param)
		if p.peek().Kind != token.COMMA {
			break
		}
		comma := p.advance()
		if p.peek().Kind == token.RPAREN {
			return nil, p.errorf(comma.Pos, "Syntax error: Trailing comma in type parameter list is not allowed.")
		}
	}
	list.Stop = list.Parameters[len(list.Parameters)-1].End()
	if _, err := p.expect(token.RPAREN, `")"`); err != nil {
		return nil, err
	}
	return list, nil
}

// parseTypeParameter parses one literal type parameter; see type_parameter
// in googlesql.tm.
func (p *parser) parseTypeParameter() (ast.Node, error) {
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
	}
	switch {
	case isKeyword(tok, "MAX"):
		p.advance()
		return &ast.MaxLiteral{Span: span(tok.Pos, tok.End)}, nil
	case isKeyword(tok, "TRUE"), isKeyword(tok, "FALSE"):
		p.advance()
		return &ast.BooleanLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image, Value: isKeyword(tok, "TRUE")}, nil
	}
	return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
}

// parseTypedArrayConstructor parses "ARRAY<type>[...]"; the ArrayType
// becomes the constructor's first child (see array_constructor_prefix_...
// in googlesql.tm).
func (p *parser) parseTypedArrayConstructor() (ast.Node, error) {
	start := p.peek().Pos
	typ, err := p.parseArrayType()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != token.LBRACKET {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "[" but got %s`, describeToken(p.peek()))
	}
	arr, err := p.parseArrayConstructor(start)
	if err != nil {
		return nil, err
	}
	arr.(*ast.ArrayConstructor).Type = typ
	return arr, nil
}
