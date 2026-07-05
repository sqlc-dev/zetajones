// Package parser implements a hand-written recursive descent parser for
// GoogleSQL, producing an AST that matches the parse tree of
// github.com/google/googlesql.
package parser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
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

// FeatureStatementWithPipeOperators gates attaching pipe-operator suffixes to
// non-query statements that can produce a table (SHOW, DESCRIBE, EXECUTE
// IMMEDIATE, RUN, CALL), forming an ASTStatementWithPipeOperators
// (FEATURE_STATEMENT_WITH_PIPE_OPERATORS).
const FeatureStatementWithPipeOperators Feature = "STATEMENT_WITH_PIPE_OPERATORS"

// FeatureAllowConsecutiveOn gates consecutive ON/USING clauses in join
// expressions (FEATURE_ALLOW_CONSECUTIVE_ON).
const FeatureAllowConsecutiveOn Feature = "ALLOW_CONSECUTIVE_ON"

// FeatureIsDistinct gates "IS [NOT] DISTINCT FROM" comparisons
// (FEATURE_IS_DISTINCT).
const FeatureIsDistinct Feature = "IS_DISTINCT"

// FeatureBracedProtoConstructors gates braced constructors "{ field: value }"
// (FEATURE_BRACED_PROTO_CONSTRUCTORS), including the "STRUCT { ... }" form.
const FeatureBracedProtoConstructors Feature = "BRACED_PROTO_CONSTRUCTORS"

// FeatureQualify gates the QUALIFY clause (FEATURE_QUALIFY).
const FeatureQualify Feature = "QUALIFY"

// FeatureForUpdate gates the FOR UPDATE lock mode clause in SELECT queries
// (FEATURE_FOR_UPDATE).
const FeatureForUpdate Feature = "FOR_UPDATE"

// FeatureRemoteFunction gates the REMOTE keyword and the WITH CONNECTION
// clause in CREATE FUNCTION statements (FEATURE_REMOTE_FUNCTION).
const FeatureRemoteFunction Feature = "REMOTE_FUNCTION"

// FeatureCreateFunctionLanguageWithConnection gates the WITH CONNECTION clause
// on non-remote CREATE FUNCTION statements
// (FEATURE_CREATE_FUNCTION_LANGUAGE_WITH_CONNECTION).
const FeatureCreateFunctionLanguageWithConnection Feature = "CREATE_FUNCTION_LANGUAGE_WITH_CONNECTION"

// FeatureAllowDashesInTableName gates dashes in the first component of a
// multi-part table name (e.g. crafty-tractor-287.dataset.table); see
// FEATURE_ALLOW_DASHES_IN_TABLE_NAME and maybe_dashed_path_expression in
// googlesql.tm. When off, such a name reports "Table name contains '-'
// character".
const FeatureAllowDashesInTableName Feature = "ALLOW_DASHES_IN_TABLE_NAME"

// FeatureAllowSlashPaths gates table names that start with "/" and contain
// non-adjacent "/", "-", and ":" separators before the first dot
// (e.g. /span/db/my-grp:db.Table); see FEATURE_ALLOW_SLASH_PATHS and
// slashed_path_expression / maybe_slashed_or_dashed_path_expression in
// googlesql.tm. When off, such a name reports "Table name contains '/'
// character. ... needs to be quoted".
const FeatureAllowSlashPaths Feature = "ALLOW_SLASH_PATHS"

// FeatureOrderedPrimaryKeys gates ASC/DESC and NULLS ordering in a PRIMARY KEY
// element list (FEATURE_ORDERED_PRIMARY_KEYS); see primary_key_element in
// googlesql.tm. When off, ordering reports "Ordering for primary keys is not
// supported".
const FeatureOrderedPrimaryKeys Feature = "ORDERED_PRIMARY_KEYS"

// FeatureTtl gates ROW DELETION POLICY alter actions (FEATURE_TTL); see the
// ADD/REPLACE/DROP ROW DELETION POLICY productions in alter_action in
// googlesql.tm. When off, they report "... ROW DELETION POLICY clause is not
// supported.".
const FeatureTtl Feature = "TTL"

// FeatureRepeat gates the "REPEAT ... UNTIL ... END REPEAT" script statement
// (FEATURE_REPEAT); see repeat_statement in googlesql.tm. When off, it reports
// "REPEAT is not supported".
const FeatureRepeat Feature = "REPEAT"

// FeatureForIn gates the "FOR ... IN (...) DO ... END FOR" script statement
// (FEATURE_FOR_IN); see for_in_statement in googlesql.tm. When off, it reports
// "FOR...IN is not supported".
const FeatureForIn Feature = "FOR_IN"

// FeatureEnableAlterArrayOptions gates the "+=" and "-=" options assignment
// operators (FEATURE_ENABLE_ALTER_ARRAY_OPTIONS). The grammar always accepts
// them, but when the feature is off the reference drops them from the set of
// expected tokens in a syntax error message; see the expectations_set.erase
// calls in googlesql/parser/parser_internal.cc.
const FeatureEnableAlterArrayOptions Feature = "ENABLE_ALTER_ARRAY_OPTIONS"

// FeatureSpannerLegacyDDL gates the Cloud Spanner legacy DDL extensions
// (FEATURE_SPANNER_LEGACY_DDL): PRIMARY KEY / INTERLEAVE table options, the
// NULL_FILTERED index modifier and index INTERLEAVE clause, and the SET ON
// DELETE and ALTER COLUMN <schema> spanner alter actions; see the
// spanner_* productions in googlesql.tm. It is not enabled by MAXIMUM.
const FeatureSpannerLegacyDDL Feature = "SPANNER_LEGACY_DDL"

// FeatureSqlGraph gates GoogleSQL graph queries: the GRAPH statement, the
// GRAPH_TABLE(...) table expression, and graph pattern syntax
// (FEATURE_SQL_GRAPH).
const FeatureSqlGraph Feature = "SQL_GRAPH"

// featureInMaximum records whether each gated feature is enabled by
// language_features=MAXIMUM, i.e. whether it is ideally enabled and not in
// development; see LanguageOptions::EnableMaximumLanguageFeatures and the
// language_feature_options annotations in googlesql/public/options.proto.
var featureInMaximum = map[Feature]bool{
	FeatureWithGroupRows:                        false, // in_development
	FeaturePipes:                                true,
	FeatureAllowConsecutiveOn:                   true,
	FeatureIsDistinct:                           true,
	FeatureBracedProtoConstructors:              true,
	FeatureQualify:                              true,
	FeatureForUpdate:                            true,
	FeatureRemoteFunction:                       true,
	FeatureCreateFunctionLanguageWithConnection: true,
	FeatureAllowDashesInTableName:               true,
	FeatureAllowSlashPaths:                      true,
	FeatureOrderedPrimaryKeys:                   false, // in_development
	FeatureTtl:                                  true,
	FeatureRepeat:                               true,
	FeatureForIn:                                true,
	FeatureEnableAlterArrayOptions:              true,
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
	// SupportedGenericEntityTypes lists the object types accepted by generic
	// entity DDL (CREATE/ALTER/DROP <type>); see supported_generic_entity_types
	// in run_parser_test.cc. Matching is case-insensitive.
	SupportedGenericEntityTypes []string
	// SupportedGenericSubEntityTypes lists the nested object types accepted by
	// generic sub-entity alter actions (ADD/ALTER/DROP <sub_type>); see
	// supported_generic_sub_entity_types in run_parser_test.cc. Matching is
	// case-insensitive.
	SupportedGenericSubEntityTypes []string
	// MacroExpansionMode selects how macro constructs are handled: "none"
	// (the default; DEFINE MACRO and "$"-macro tokens are rejected), "lenient",
	// or "strict". See macro_expansion_mode in run_parser_test.cc and
	// ParserOptions in googlesql/parser/parser.h.
	MacroExpansionMode string
	// ReserveGraphTable makes GRAPH_TABLE a reserved keyword, enabling the
	// GRAPH_TABLE(...) graph table expression in a FROM clause; see
	// reserve_graph_table in run_parser_test.cc and KW_GRAPH_TABLE_RESERVED in
	// googlesql.tm.
	ReserveGraphTable bool
}

// stringSet builds a case-insensitive (upper-cased) lookup set from a list.
func stringSet(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[strings.ToUpper(it)] = true
	}
	return m
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
	p := &parser{sql: sql, toks: toks, features: opts.Features, entityTypes: stringSet(opts.SupportedGenericEntityTypes), subEntityTypes: stringSet(opts.SupportedGenericSubEntityTypes), macroMode: opts.MacroExpansionMode, reserveGraphTable: opts.ReserveGraphTable}
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
		if isKeyword(p.peek(), "OVER") {
			// See the KW_OVER special case in
			// googlesql/parser/parser_internal.cc.
			return nil, p.errorf(p.peek().Pos, "Syntax error: OVER keyword must follow a function call")
		}
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected end of input but got %s", describeToken(p.peek()))
	}
	return stmt, nil
}

// ParseScript parses a whole script: a sequence of statements separated by ";"
// with an optional trailing ";", wrapped in a Script node. It corresponds to
// the reference driver's "mode=script" test mode; see the script rule in
// googlesql.tm.
func ParseScript(sql string) (ast.Node, error) {
	return ParseScriptWithOptions(sql, Options{})
}

// ParseScriptWithOptions parses a whole script; see ParseScript.
func ParseScriptWithOptions(sql string, opts Options) (ast.Node, error) {
	toks, err := lexer.Lex(sql)
	if err != nil {
		var lerr *lexer.Error
		if errors.As(err, &lerr) {
			return nil, &Error{Message: lerr.Message, Offset: lerr.Offset, SQL: sql}
		}
		return nil, err
	}
	// The reference tokenizer terminates a script with a sentinel whose
	// syntax-error wording is "end of script"; stash that in the EOF token so
	// describeToken reports it. See GetParserModeName in
	// googlesql/parser/parser_internal.cc.
	if n := len(toks); n > 0 && toks[n-1].Kind == token.EOF {
		toks[n-1].Image = "end of script"
	}
	// Script mode force-emits SCRIPT_LABEL for "<name> : <block-keyword>" at a
	// statement-start position; see LookaheadTransformer::IsCurrentTokenScriptLabel
	// in googlesql/parser/lookahead_transformer.cc.
	markScriptLabels(toks)
	p := &parser{sql: sql, toks: toks, features: opts.Features, entityTypes: stringSet(opts.SupportedGenericEntityTypes), subEntityTypes: stringSet(opts.SupportedGenericSubEntityTypes), macroMode: opts.MacroExpansionMode, reserveGraphTable: opts.ReserveGraphTable}
	// An empty script resolves to a Script wrapping an empty statement list.
	if p.peek().Kind == token.EOF {
		empty := &ast.StatementList{Span: span(0, 0)}
		return &ast.Script{Span: span(0, 0), Statements: empty}, nil
	}
	list := &ast.StatementList{Span: span(p.peek().Pos, 0)}
	for {
		stmt, err := p.parseScriptStatement()
		if err != nil {
			return nil, err
		}
		list.Statements = append(list.Statements, stmt)
		list.Stop = stmt.End()
		if p.peek().Kind == token.SEMICOLON {
			// The trailing ";" extends the statement list (WithEndLocation);
			// an internal ";" between statements does not, since the next
			// statement re-extends the list to its own end.
			semi := p.advance()
			list.Stop = semi.End
			if p.peek().Kind == token.EOF {
				break
			}
			continue
		}
		if p.peek().Kind == token.EOF {
			break
		}
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected end of input but got %s", describeToken(p.peek()))
	}
	return &ast.Script{Span: span(list.Start, list.Stop), Statements: list}, nil
}

// markScriptLabels retags qualifying identifier tokens as SCRIPT_LABEL, as the
// reference lookahead transformer does in script mode. A token becomes a
// SCRIPT_LABEL when it is an identifier or keyword immediately followed by ":"
// and one of the block-opening keywords BEGIN/WHILE/LOOP/REPEAT/FOR, and it
// sits at a statement-start position (the very start, after ";", after
// ELSE/THEN, after a statement-list-opening keyword, or after a statement-level
// hint). See LookaheadTransformer::IsCurrentTokenScriptLabel in
// googlesql/parser/lookahead_transformer.cc (Apache 2.0).
func markScriptLabels(toks []token.Token) {
	for i := 0; i+2 < len(toks); i++ {
		if toks[i].Kind != token.IDENT {
			continue
		}
		if toks[i+1].Kind != token.COLON {
			continue
		}
		n2 := toks[i+2]
		if !isKeyword(n2, "BEGIN") && !isKeyword(n2, "WHILE") && !isKeyword(n2, "LOOP") &&
			!isKeyword(n2, "REPEAT") && !isKeyword(n2, "FOR") {
			continue
		}
		if scriptLabelLookbackOK(toks, i) {
			toks[i].Kind = token.SCRIPT_LABEL
		}
	}
}

// scriptLabelLookbackOK reports whether position i sits at a script
// statement-start position, i.e. the lookback token is one of those permitted
// before a SCRIPT_LABEL. See IsCurrentTokenScriptLabel in
// googlesql/parser/lookahead_transformer.cc.
func scriptLabelLookbackOK(toks []token.Token, i int) bool {
	if i == 0 {
		return true
	}
	prev := toks[i-1]
	switch {
	case prev.Kind == token.SEMICOLON:
		return true
	case isKeyword(prev, "ELSE"), isKeyword(prev, "THEN"):
		return true
	case isStatementListOpener(prev):
		return true
	case isStatementHintEnd(toks, i-1):
		return true
	}
	return false
}

// isStatementListOpener reports whether tok is a keyword that opens a script
// statement list (so the following token is at a statement-start position):
// BEGIN (block), LOOP, REPEAT, and DO. These carry the LB_OPEN_STATEMENT_BLOCK
// lookback override in googlesql.tm.
func isStatementListOpener(tok token.Token) bool {
	return isKeyword(tok, "BEGIN") || isKeyword(tok, "LOOP") ||
		isKeyword(tok, "REPEAT") || isKeyword(tok, "DO")
}

// isStatementHintEnd reports whether toks[k] is the final token of a
// statement-level hint ("@int" or "@[int]{...}") that itself begins a
// statement. Such a hint carries the LB_END_OF_STATEMENT_LEVEL_HINT lookback
// override in googlesql.tm.
func isStatementHintEnd(toks []token.Token, k int) bool {
	if k < 1 {
		return false
	}
	switch toks[k].Kind {
	case token.INT:
		// "@int": ATSIGN INT.
		return toks[k-1].Kind == token.ATSIGN && atStatementStart(toks, k-1)
	case token.RBRACE:
		// "@{...}" or "@int{...}": find the matching "{".
		depth := 0
		for j := k; j >= 0; j-- {
			switch toks[j].Kind {
			case token.RBRACE:
				depth++
			case token.LBRACE:
				depth--
				if depth == 0 {
					if j >= 1 && toks[j-1].Kind == token.ATSIGN && atStatementStart(toks, j-1) {
						return true
					}
					if j >= 2 && toks[j-1].Kind == token.INT && toks[j-2].Kind == token.ATSIGN &&
						atStatementStart(toks, j-2) {
						return true
					}
					return false
				}
			}
		}
	}
	return false
}

// atStatementStart reports whether toks[idx] is the first token of a script
// statement (used to confirm a leading hint really begins a statement).
func atStatementStart(toks []token.Token, idx int) bool {
	if idx == 0 {
		return true
	}
	prev := toks[idx-1]
	return prev.Kind == token.SEMICOLON || isKeyword(prev, "ELSE") ||
		isKeyword(prev, "THEN") || isStatementListOpener(prev)
}

// ParseType parses a single standalone type (outside of any query), allowing
// an optional trailing semicolon. It corresponds to the reference driver's
// "mode=type" test mode.
func ParseType(sql string) (ast.Node, error) {
	return ParseTypeWithOptions(sql, Options{})
}

// ParseTypeWithOptions parses a single standalone type; see ParseType.
func ParseTypeWithOptions(sql string, opts Options) (ast.Node, error) {
	toks, err := lexer.Lex(sql)
	if err != nil {
		var lerr *lexer.Error
		if errors.As(err, &lerr) {
			return nil, &Error{Message: lerr.Message, Offset: lerr.Offset, SQL: sql}
		}
		return nil, err
	}
	// The reference tokenizer terminates a standalone type with a sentinel
	// whose syntax-error wording is "end of type"; stash that in the EOF
	// token so describeToken reports it.
	if n := len(toks); n > 0 && toks[n-1].Kind == token.EOF {
		toks[n-1].Image = "end of type"
	}
	p := &parser{sql: sql, toks: toks, features: opts.Features, entityTypes: stringSet(opts.SupportedGenericEntityTypes), subEntityTypes: stringSet(opts.SupportedGenericSubEntityTypes), macroMode: opts.MacroExpansionMode, reserveGraphTable: opts.ReserveGraphTable}
	typ, err := p.parseType()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind == token.SEMICOLON {
		p.advance()
	}
	if p.peek().Kind != token.EOF {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected end of input but got %s", describeToken(p.peek()))
	}
	return typ, nil
}

// ParseExpression parses a single standalone expression (outside of any
// query). It corresponds to the reference driver's "mode=expression" test
// mode.
func ParseExpression(sql string) (ast.Node, error) {
	return ParseExpressionWithOptions(sql, Options{})
}

// ParseExpressionWithOptions parses a single standalone expression; see
// ParseExpression.
func ParseExpressionWithOptions(sql string, opts Options) (ast.Node, error) {
	toks, err := lexer.Lex(sql)
	if err != nil {
		var lerr *lexer.Error
		if errors.As(err, &lerr) {
			return nil, &Error{Message: lerr.Message, Offset: lerr.Offset, SQL: sql}
		}
		return nil, err
	}
	// The reference tokenizer terminates a standalone expression with a
	// sentinel whose syntax-error wording is "end of expression"; stash that
	// in the EOF token so describeToken reports it.
	if n := len(toks); n > 0 && toks[n-1].Kind == token.EOF {
		toks[n-1].Image = "end of expression"
	}
	p := &parser{sql: sql, toks: toks, features: opts.Features, entityTypes: stringSet(opts.SupportedGenericEntityTypes), subEntityTypes: stringSet(opts.SupportedGenericSubEntityTypes), macroMode: opts.MacroExpansionMode, reserveGraphTable: opts.ReserveGraphTable}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != token.EOF {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected end of input but got %s", describeToken(p.peek()))
	}
	return expr, nil
}

type parser struct {
	sql      string
	toks     []token.Token
	pos      int
	features *FeatureSet
	// entityTypes and subEntityTypes are the case-insensitive sets of object
	// types accepted by generic entity DDL and generic sub-entity alter
	// actions; see Options.SupportedGenericEntityTypes.
	entityTypes    map[string]bool
	subEntityTypes map[string]bool
	// macroMode is the effective macro expansion mode ("none", "lenient", or
	// "strict"); an empty value means "none". It gates DEFINE MACRO statements.
	macroMode string
	// reserveGraphTable makes GRAPH_TABLE a reserved keyword and enables the
	// GRAPH_TABLE(...) table expression; see Options.ReserveGraphTable.
	reserveGraphTable bool
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
	// inTablePath is set while parsing the path expression of a FROM-clause
	// table item, where "." followed by "(" reports the dedicated generalized
	// field access error; see table_path_expression_base in googlesql.tm.
	inTablePath bool
	// allowGeneralizedField is set while parsing the leading path of an
	// expression, where "." followed by "(" is a generalized field access
	// handled by the postfix layer rather than an error; see the "." "("
	// path_expression ")" rule in googlesql.tm.
	allowGeneralizedField bool
	// allowDashes is set while parsing a table-name path expression, where the
	// first component may be a "dashed identifier" like my-project; see
	// maybe_dashed_path_expression in googlesql.tm.
	allowDashes bool
	// quantifierQuestions records the byte positions of "?" tokens consumed
	// as row pattern quantifiers (or reluctant markers) inside
	// MATCH_RECOGNIZE patterns, so positional query parameter numbering
	// skips them.
	quantifierQuestions map[int]bool
	// suppressTopLevelIn tells the next (outermost) comparison parse not to
	// consume a top-level IN operator. It is used for a PIVOT clause's FOR
	// expression, where the following IN introduces the pivot value list
	// rather than an IN expression; see the pivot_clause rule and
	// expression_higher_prec_than_and in googlesql.tm.
	suppressTopLevelIn bool
	// inPipeCreateTable is set while parsing the create_table_statement_prefix
	// of a |> CREATE TABLE pipe operator, where a trailing AS query is a
	// dedicated error rather than a valid clause; see pipe_create_table in
	// googlesql.tm.
	inPipeCreateTable bool
	// inFromQuery is set while parsing the FROM clause of a standalone FROM
	// query (the from_query alternative of query_primary_or_from_query in
	// googlesql.tm). In that context a trailing "QUALIFY expression" is not a
	// postfix table operator on the table primary; the LALR grammar leaves the
	// QUALIFY unshifted so it surfaces as a top-level "Expected end of input"
	// (or, without pipes, the "Unexpected FROM" error). It is reset while
	// parsing any nested query so select-clause QUALIFY still works.
	inFromQuery bool
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

// setNodeEnd sets the end (Stop) of a node's embedded ast.Span, mimicking
// ZetaSQL's WithEndLocation. Every AST node embeds ast.Span, so reflection can
// locate and update it. This is used where the reference grammar extends an
// existing node's location (see parseGroupingSet).
func setNodeEnd(n ast.Node, end int) {
	v := reflect.ValueOf(n)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return
	}
	f := v.Elem().FieldByName("Span")
	if f.IsValid() && f.CanSet() {
		f.FieldByName("Stop").SetInt(int64(end))
	}
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
		// In standalone-type mode the reference driver describes the end
		// sentinel as "end of type" rather than "end of statement"; the
		// entry point stashes that wording in the EOF token's Image.
		if tok.Image != "" {
			return tok.Image
		}
		return "end of statement"
	case token.IDENT:
		if keywordNames[strings.ToLower(tok.Image)] {
			return "keyword " + strings.ToUpper(tok.Image)
		}
		return fmt.Sprintf("identifier \"%s\"", tok.Image)
	case token.QUOTED_IDENT:
		// Don't put extra quotes around an already-backquoted identifier.
		return "identifier " + tok.Image
	case token.SCRIPT_LABEL:
		// A SCRIPT_LABEL token is reported as just the quoted image, without
		// the "identifier" qualifier: it is neither IDENTIFIER nor a keyword to
		// MakeSyntaxErrorAtToken (googlesql/parser/parser_internal.cc), so it
		// falls through to the plain-quoted default.
		return `"` + tok.Image + `"`
	case token.INT:
		return fmt.Sprintf("integer literal \"%s\"", tok.Image)
	case token.FLOAT:
		return fmt.Sprintf("floating point literal \"%s\"", tok.Image)
	case token.STRING:
		return "string literal " + shortenStringLiteralForError(escapeTokenNewlines(tok.Image))
	case token.BYTES:
		return "bytes literal " + shortenBytesLiteralForError(escapeTokenNewlines(tok.Image))
	case token.SYSTEM_VARIABLE:
		// The lexer folds "@@name" into one token, but the reference lexes
		// "@@" separately and reports just that; see MakeSyntaxErrorAtToken
		// in googlesql/parser/parser_internal.cc.
		return `"@@"`
	case token.PARAM:
		// The lexer folds "@name" into one token, but the reference lexes the
		// "@" (ATSIGN) separately from the identifier and reports just the
		// "@"; see MakeSyntaxErrorAtToken in googlesql/parser/parser_internal.cc.
		return `"@"`
	}
	// Wrap the token image in literal double quotes without Go %q escaping, so
	// that e.g. a lone backslash renders as "\" rather than "\\". This matches
	// MakeSyntaxErrorAtToken in googlesql/parser/parser_internal.cc.
	return `"` + tok.Image + `"`
}

// escapeTokenNewlines escapes physical newlines to avoid multi-line error
// messages, matching the reference implementation.
func escapeTokenNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r", `\r`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

// maxErrorLiteralLength is the longest literal value (in bytes, including the
// quotes) that we're willing to echo in an error message. Matches
// kMaxErrorLiteralLength in googlesql/parser/parser_internal.cc.
const maxErrorLiteralLength = 50

func startsWithFoldPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if len(s) >= len(p) && strings.EqualFold(s[:len(p)], p) {
			return true
		}
	}
	return false
}

// shortenStringLiteralForError shortens a too-long string literal for display
// in an error message, inserting "..." before the final quotes. Ported from
// ShortenStringLiteralForError in googlesql/parser/parser_internal.cc.
func shortenStringLiteralForError(literal string) string {
	if len(literal) <= maxErrorLiteralLength {
		return literal
	}
	numEndQuotes := 1
	if startsWithFoldPrefix(literal, `"""`, `'''`, `r"""`, `r'''`) {
		numEndQuotes = 3
	}
	excerptSize := maxErrorLiteralLength - numEndQuotes
	// If we can't remove at least four bytes in addition to the quotes, keep
	// the original.
	if excerptSize > len(literal)-numEndQuotes-4 {
		return literal
	}
	// Don't cut the string off in the middle of a multibyte character.
	for excerptSize > 0 && !utf8.ValidString(literal[:excerptSize]) {
		excerptSize--
	}
	return literal[:excerptSize] + "..." + literal[len(literal)-numEndQuotes:]
}

// shortenBytesLiteralForError shortens a too-long bytes literal for display in
// an error message. Ported from ShortenBytesLiteralForError in
// googlesql/parser/parser_internal.cc.
func shortenBytesLiteralForError(literal string) string {
	if len(literal) < maxErrorLiteralLength {
		return literal
	}
	numEndQuotes := 1
	if startsWithFoldPrefix(literal, `b"""`, `rb"""`, `br"""`, `b'''`, `rb'''`, `br'''`) {
		numEndQuotes = 3
	}
	excerptSize := maxErrorLiteralLength - numEndQuotes
	if excerptSize > len(literal)-numEndQuotes-4 {
		return literal
	}
	return literal[:excerptSize] + "..." + literal[len(literal)-numEndQuotes:]
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
	"ASSERT_ROWS_MODIFIED": true,
	"BETWEEN":              true,
	"BY":                   true, "CASE": true, "CAST": true, "COLLATE": true, "CROSS": true, "CURRENT": true,
	"DEFAULT": true,
	"DESC":    true, "DISTINCT": true,
	"ELSE": true, "END": true, "ENUM": true, "EXCEPT": true, "EXISTS": true, "FALSE": true, "FOR": true,
	"FROM": true,
	"FULL": true, "GROUP": true, "GROUPING": true, "GROUPS": true, "HASH": true, "HAVING": true,
	"IF":     true,
	"IGNORE": true, "IN": true,
	"INNER":     true,
	"INTERSECT": true, "IS": true, "JOIN": true, "LATERAL": true, "LEFT": true,
	"LIKE":  true,
	"LIMIT": true, "LOOKUP": true,
	// MATCH_RECOGNIZE is conditionally reserved; the parser tests reserve it
	// by default (see reserve_match_recognize in run_parser_test.cc).
	"MATCH_RECOGNIZE": true,
	"NATURAL":         true, "NOT": true, "NULL": true,
	"NULLS": true, "ON": true,
	"OR": true, "ORDER": true, "OUTER": true, "OVER": true, "PARTITION": true,
	"PROTO":   true,
	"RESPECT": true, "RIGHT": true, "ROWS": true, "SELECT": true, "SET": true, "STRUCT": true,
	"TABLESAMPLE": true,
	"THEN":        true,
	"TO":          true,
	"TRUE":        true, "UNION": true, "UNNEST": true, "USING": true, "WHERE": true,
	"WINDOW": true, "WITH": true,
}

// isReservedStatic reports whether tok is an unconditionally reserved keyword.
// The parser method isReserved additionally honors the conditionally reserved
// GRAPH_TABLE (see reserve_graph_table); a few context-detection helpers that
// have no parser receiver use this static form.
func isReservedStatic(tok token.Token) bool {
	return tok.Kind == token.IDENT && reservedKeywords[strings.ToUpper(tok.Image)]
}

func (p *parser) isReserved(tok token.Token) bool {
	if tok.Kind != token.IDENT {
		return false
	}
	if reservedKeywords[strings.ToUpper(tok.Image)] {
		return true
	}
	// GRAPH_TABLE is conditionally reserved: when reserve_graph_table is on it
	// becomes a reserved keyword and cannot be used as an identifier. See
	// KW_GRAPH_TABLE_RESERVED / kConditionallyReserved in
	// googlesql/parser/keywords.cc.
	if p.reserveGraphTable && strings.EqualFold(tok.Image, "GRAPH_TABLE") {
		return true
	}
	return false
}

// optionsNameOK reports whether tok can start an options-list or hint entry
// name; see identifier_in_hints in googlesql.tm. The name is any identifier
// (unreserved keyword or plain identifier) plus the reserved keywords HASH,
// PROTO, and PARTITION, which are explicitly allowed.
func optionsNameOK(tok token.Token) bool {
	if tok.Kind == token.QUOTED_IDENT {
		return true
	}
	if tok.Kind != token.IDENT {
		return false
	}
	switch strings.ToUpper(tok.Image) {
	case "HASH", "PROTO", "PARTITION":
		return true
	}
	return !reservedKeywords[strings.ToUpper(tok.Image)]
}

// reservedFunctionNameKeywords are the function_name_from_keyword entries that
// are also reserved keywords: in expression position they name a function call
// (requiring "(") rather than being an ordinary path. See
// function_name_from_keyword in googlesql.tm. (RANGE is also in that rule but
// has its own dedicated handling for range literals.)
var reservedFunctionNameKeywords = map[string]bool{
	"COLLATE": true, "GROUPING": true, "IF": true, "LEFT": true, "RIGHT": true,
}

func isReservedFunctionNameKeyword(tok token.Token) bool {
	return tok.Kind == token.IDENT && reservedFunctionNameKeywords[strings.ToUpper(tok.Image)]
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
	// A statement may be preceded by a "@{...}" (or "@n{...}") hint; see
	// hinted_statement in googlesql.tm.
	if tok.Kind == token.ATSIGN && (p.peekAt(1).Kind == token.LBRACE || p.peekAt(1).Kind == token.INT) {
		hint, err := p.parseOptionalHint()
		if err != nil {
			return nil, err
		}
		if isKeyword(p.peek(), "DEFINE") && isKeyword(p.peekAt(1), "MACRO") {
			// Hints are not allowed on DEFINE MACRO statements; the error points
			// at the hint. See the statement_level_hint "DEFINE" "MACRO" rule in
			// googlesql.tm.
			return nil, p.errorf(hint.Pos(), "Hints are not allowed on DEFINE MACRO statements.")
		}
		inner, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		return &ast.HintedStatement{Span: span(hint.Pos(), inner.End()), Hint: hint, Statement: inner}, nil
	}
	// A statement may itself be a standalone subpipeline "|> op ..."; see
	// subpipeline_statement in googlesql.tm.
	if tok.Kind == token.PIPE_INPUT {
		sub, err := p.parseSubpipelineNoParens()
		if err != nil {
			return nil, err
		}
		return &ast.SubpipelineStatement{Span: span(sub.Pos(), sub.End()), Subpipeline: sub}, nil
	}
	switch {
	case isKeyword(tok, "GRAPH"):
		// A statement beginning with GRAPH is a GQL graph query statement;
		// see gql_statement / gql_query in googlesql.tm. The grammar does not
		// gate this on FEATURE_SQL_GRAPH (that check happens in the analyzer),
		// so graph statements parse regardless of the enabled language
		// features.
		return p.parseGraphStatement()
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
		stmt, err := p.parseCallStatement()
		if err != nil {
			return nil, err
		}
		return p.maybeStatementWithPipeOperators(stmt)
	case isKeyword(tok, "CREATE"):
		return p.parseCreateStatement()
	case isKeyword(tok, "DEFINE"):
		if isKeyword(p.peekAt(1), "MACRO") {
			return p.parseDefineMacroStatement()
		}
		return p.parseDefineTableStatement()
	case isKeyword(tok, "DELETE"):
		return p.parseDeleteStatement()
	case isKeyword(tok, "DROP"):
		return p.parseDropStatement()
	case isKeyword(tok, "GRANT"):
		return p.parseGrantOrRevokeStatement(false)
	case isKeyword(tok, "REVOKE"):
		return p.parseGrantOrRevokeStatement(true)
	case isKeyword(tok, "SHOW"):
		stmt, err := p.parseShowStatement()
		if err != nil {
			return nil, err
		}
		return p.maybeStatementWithPipeOperators(stmt)
	case isKeyword(tok, "DESCRIBE"), isKeyword(tok, "DESC"):
		stmt, err := p.parseDescribeStatement()
		if err != nil {
			return nil, err
		}
		return p.maybeStatementWithPipeOperators(stmt)
	case isKeyword(tok, "EXECUTE"):
		stmt, err := p.parseExecuteImmediateStatement()
		if err != nil {
			return nil, err
		}
		return p.maybeStatementWithPipeOperators(stmt)
	case isKeyword(tok, "EXPORT") && isKeyword(p.peekAt(1), "DATA"):
		return p.parseExportDataStatement()
	case isKeyword(tok, "EXPORT") && isKeyword(p.peekAt(1), "MODEL"):
		return p.parseExportModelStatement()
	case isKeyword(tok, "IMPORT"):
		return p.parseImportStatement()
	case isKeyword(tok, "MODULE"):
		return p.parseModuleStatement()
	case isKeyword(tok, "RENAME"):
		return p.parseRenameStatement()
	case isKeyword(tok, "INSERT"):
		return p.parseInsertStatement()
	case isKeyword(tok, "MERGE"):
		return p.parseMergeStatement()
	case isKeyword(tok, "TRUNCATE"):
		return p.parseTruncateStatement()
	case isKeyword(tok, "CLONE"):
		return p.parseCloneDataStatement()
	case isKeyword(tok, "UPDATE"):
		return p.parseUpdateStatement()
	case isKeyword(tok, "SET"):
		return p.parseSetStatement()
	case isKeyword(tok, "BEGIN"):
		return p.parseBeginStatement()
	case isKeyword(tok, "START"):
		if isKeyword(p.peekAt(1), "BATCH") {
			return p.parseStartBatchStatement()
		}
		return p.parseBeginStatement()
	case isKeyword(tok, "COMMIT"):
		return p.parseCommitStatement()
	case isKeyword(tok, "ROLLBACK"):
		return p.parseRollbackStatement()
	case isKeyword(tok, "RUN"):
		return p.parseRunBatchStatement()
	case isKeyword(tok, "ABORT"):
		return p.parseAbortBatchStatement()
	case isKeyword(tok, "ANALYZE"):
		return p.parseAnalyzeStatement()
	case isKeyword(tok, "ASSERT"):
		return p.parseAssertStatement()
	case isKeyword(tok, "EXPLAIN"):
		// "EXPLAIN <statement>" wraps another (non-script) SQL statement; see
		// explain_statement in googlesql.tm. The inner parseStatement rejects
		// script-only statements (e.g. IF) with an "Unexpected" error.
		explainTok := p.advance() // EXPLAIN
		inner, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		return &ast.ExplainStatement{Span: span(explainTok.Pos, inner.End()), Statement: inner}, nil
	}
	return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
}

// parseAssertStatement parses "ASSERT expression [AS description]"; see
// assert_statement in googlesql.tm.
func (p *parser) parseAssertStatement() (ast.Statement, error) {
	assertTok := p.advance() // ASSERT
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	stmt := &ast.AssertStatement{Span: span(assertTok.Pos, p.prevEnd()), Expression: expr}
	if isKeyword(p.peek(), "AS") {
		p.advance() // AS
		if p.peek().Kind != token.STRING {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected string literal but got %s", describeToken(p.peek()))
		}
		desc, err := p.parseStringLiteralValue()
		if err != nil {
			return nil, err
		}
		stmt.Description = desc
		stmt.Stop = desc.End()
	}
	return stmt, nil
}

// parseAnalyzeStatement parses "ANALYZE [OPTIONS(...)]
// [table_and_column_info, ...]"; see analyze_statement in googlesql.tm.
func (p *parser) parseAnalyzeStatement() (ast.Statement, error) {
	analyzeTok := p.advance() // ANALYZE
	stmt := &ast.AnalyzeStatement{Span: span(analyzeTok.Pos, analyzeTok.End)}
	// options_opt: an OPTIONS keyword immediately after ANALYZE commits to the
	// options list (shift is favored over reducing OPTIONS as an identifier;
	// see AMBIGUOUS CASE 8 in googlesql.tm), so a missing "(" is an error.
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		stmt.Options = opts
		stmt.Stop = opts.End()
	}
	// (table_and_column_info separator ",")*
	if isTableAndColumnInfoStart(p.peek()) {
		list := &ast.TableAndColumnInfoList{Span: span(p.peek().Pos, 0)}
		for {
			info, err := p.parseTableAndColumnInfo()
			if err != nil {
				return nil, err
			}
			list.Infos = append(list.Infos, info)
			list.Stop = info.End()
			if p.peek().Kind != token.COMMA {
				break
			}
			p.advance() // ,
		}
		stmt.TableInfo = list
		stmt.Stop = list.End()
	}
	return stmt, nil
}

// isTableAndColumnInfoStart reports whether tok can begin a
// table_and_column_info (a maybe_dashed_path_expression, i.e. an identifier).
func isTableAndColumnInfoStart(tok token.Token) bool {
	return tok.Kind == token.IDENT || tok.Kind == token.QUOTED_IDENT
}

// parseTableAndColumnInfo parses "maybe_dashed_path_expression
// opt_column_list"; see table_and_column_info in googlesql.tm.
func (p *parser) parseTableAndColumnInfo() (*ast.TableAndColumnInfo, error) {
	table, err := p.parseMaybeDashedPathExpression()
	if err != nil {
		return nil, err
	}
	info := &ast.TableAndColumnInfo{Span: span(table.Pos(), table.End()), Table: table}
	if p.peek().Kind == token.LPAREN {
		cols, err := p.parseInsertColumnList()
		if err != nil {
			return nil, err
		}
		info.Columns = cols
		info.Stop = cols.End()
	}
	return info, nil
}

// parseSetStatement parses a "SET" assignment statement; see set_statement in
// googlesql.tm. It handles single-variable, named-parameter, system-variable,
// and struct (parenthesized variable list) assignments.
func (p *parser) parseSetStatement() (ast.Statement, error) {
	setTok := p.advance() // SET
	tok := p.peek()
	switch {
	case isKeyword(tok, "TRANSACTION") && p.beginsTransactionMode(p.peekAt(1)):
		// SET TRANSACTION (transaction_mode separator ",")+. If "TRANSACTION"
		// is not followed by a transaction mode it is instead treated as an
		// ordinary identifier (see the default case below), matching the
		// set_statement grammar in googlesql.tm.
		p.advance() // TRANSACTION
		modes, err := p.parseTransactionModeList()
		if err != nil {
			return nil, err
		}
		return &ast.SetTransactionStatement{Span: span(setTok.Pos, p.prevEnd()), ModeList: modes}, nil
	case tok.Kind == token.LPAREN:
		// SET "(" identifier_list ")" "=" expression
		p.advance() // (
		if p.peek().Kind == token.RPAREN {
			// Improved error for an empty variable list; see set_statement in
			// googlesql.tm.
			return nil, p.errorf(p.peek().Pos, "Parenthesized SET statement requires a variable list")
		}
		list, err := p.parseIdentifierList()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(token.RPAREN, `")"`); err != nil {
			return nil, err
		}
		if _, err := p.expect(token.EQ, `"="`); err != nil {
			return nil, err
		}
		value, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		return &ast.AssignmentFromStruct{Span: span(setTok.Pos, p.prevEnd()), Variables: list, Value: value}, nil
	case tok.Kind == token.PARAM || tok.Kind == token.ATSIGN:
		// SET named_parameter_expression "=" expression
		param, err := p.parseNamedParameterExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(token.EQ, `"="`); err != nil {
			return nil, err
		}
		value, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		return &ast.ParameterAssignment{Span: span(setTok.Pos, p.prevEnd()), Parameter: param, Value: value}, nil
	case tok.Kind == token.SYSTEM_VARIABLE:
		// SET system_variable_expression "=" expression
		svNode, err := p.parseSystemVariableExpr()
		if err != nil {
			return nil, err
		}
		sv := svNode.(*ast.SystemVariableExpr)
		if _, err := p.expect(token.EQ, `"="`); err != nil {
			return nil, err
		}
		value, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		return &ast.SystemVariableAssignment{Span: span(setTok.Pos, p.prevEnd()), SystemVariable: sv, Value: value}, nil
	default:
		// SET identifier "=" expression
		ident, err := p.parseIdentifier()
		if err != nil {
			return nil, err
		}
		switch p.peek().Kind {
		case token.EQ:
			p.advance() // =
			value, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			return &ast.SingleAssignment{Span: span(setTok.Pos, p.prevEnd()), Variable: ident, Value: value}, nil
		case token.COMMA:
			// "SET identifier "," identifier_list "=" ...": the improved error
			// for a list of multiple variables without the required
			// parentheses only applies once the whole list and "=" have been
			// consumed; otherwise the normal syntax error wins. See
			// set_statement in googlesql.tm.
			p.advance() // ,
			if _, err := p.parseIdentifierList(); err != nil {
				return nil, err
			}
			if _, err := p.expect(token.EQ, `"="`); err != nil {
				return nil, err
			}
			return nil, p.errorf(ident.Pos(), "Using SET with multiple variables requires parentheses around the variable list")
		default:
			return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "," or "=" but got %s`, describeToken(p.peek()))
		}
	}
}

// parseNamedParameterExpr parses a named query parameter "@name"; see
// named_parameter_expression in googlesql.tm. The lexer emits "@name" as a
// single PARAM token, while a bare "@" (ATSIGN) must be followed by an
// identifier.
func (p *parser) parseNamedParameterExpr() (*ast.ParameterExpr, error) {
	tok := p.peek()
	if tok.Kind == token.PARAM {
		p.advance()
		name := &ast.Identifier{Span: span(tok.Pos+1, tok.End), Name: tok.Image[1:]}
		return &ast.ParameterExpr{Span: span(tok.Pos, tok.End), Name: name}, nil
	}
	at := p.advance() // bare @
	ident, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	return &ast.ParameterExpr{Span: span(at.Pos, ident.End()), Name: ident}, nil
}

// parseImportStatement parses "IMPORT MODULE|PROTO path_or_string
// [AS/INTO alias] [OPTIONS(...)]"; see import_statement in googlesql.tm.
func (p *parser) parseImportStatement() (ast.Statement, error) {
	importTok := p.advance() // IMPORT
	// import_type: MODULE or PROTO.
	if !isKeyword(p.peek(), "MODULE") && !isKeyword(p.peek(), "PROTO") {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword MODULE or keyword PROTO but got %s", describeToken(p.peek()))
	}
	p.advance() // MODULE / PROTO
	// path_expression_or_string. The reference LALR parser accepts either an
	// identifier or a string literal here, so a token that starts neither is
	// reported generically as unexpected rather than "Expected identifier".
	if k := p.peek().Kind; k != token.STRING && k != token.IDENT && k != token.QUOTED_IDENT {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
	}
	var name ast.Node
	var err error
	if p.peek().Kind == token.STRING {
		name, err = p.parseStringLiteral()
	} else {
		name, err = p.parsePathExpression()
	}
	if err != nil {
		return nil, err
	}
	stmt := &ast.ImportStatement{Span: span(importTok.Pos, p.extEnd(name)), Name: name}
	// opt_as_or_into_alias.
	switch {
	case isKeyword(p.peek(), "AS"):
		alias, err := p.parseRequiredAsAlias()
		if err != nil {
			return nil, err
		}
		stmt.Alias = alias
		stmt.Stop = alias.End()
	case isKeyword(p.peek(), "INTO"):
		into, err := p.parseIntoAlias()
		if err != nil {
			return nil, err
		}
		stmt.Into = into
		stmt.Stop = into.End()
	}
	// opt_options_list.
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance()
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		stmt.Options = opts
		stmt.Stop = opts.End()
	}
	return stmt, nil
}

// parseRenameStatement parses "RENAME identifier path_expression TO
// path_expression"; see rename_statement in googlesql.tm.
func (p *parser) parseRenameStatement() (ast.Statement, error) {
	renameTok := p.advance() // RENAME
	ident, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	oldName, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	// After the source path the LALR parser expects either "." (to extend the
	// path, already consumed by parsePathExpression) or the "TO" keyword.
	if !isKeyword(p.peek(), "TO") {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected \".\" or keyword TO but got %s", describeToken(p.peek()))
	}
	p.advance() // TO
	newName, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	return &ast.RenameStatement{
		Span:       span(renameTok.Pos, p.prevEnd()),
		Identifier: ident,
		OldName:    oldName,
		NewName:    newName,
	}, nil
}

// parseModuleStatement parses "MODULE path_expression [OPTIONS(...)]"; see
// module_statement in googlesql.tm.
func (p *parser) parseModuleStatement() (ast.Statement, error) {
	moduleTok := p.advance() // MODULE
	name, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	stmt := &ast.ModuleStatement{Span: span(moduleTok.Pos, name.End()), Name: name}
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance()
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		stmt.Options = opts
		stmt.Stop = opts.End()
	}
	return stmt, nil
}

// parseRequiredAsAlias parses "AS identifier". The AS keyword must be present;
// the identifier may not be a reserved keyword.
func (p *parser) parseRequiredAsAlias() (*ast.Alias, error) {
	asTok := p.advance() // AS
	tok := p.peek()
	if tok.Kind == token.QUOTED_IDENT || (tok.Kind == token.IDENT && !p.isReserved(tok)) {
		ident := p.parseIdentifierToken(p.advance())
		return &ast.Alias{Span: span(asTok.Pos, ident.End()), Identifier: ident}, nil
	}
	return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
}

// parseIntoAlias parses "INTO identifier"; see opt_as_or_into_alias in
// googlesql.tm.
func (p *parser) parseIntoAlias() (*ast.IntoAlias, error) {
	intoTok := p.advance() // INTO
	tok := p.peek()
	if tok.Kind == token.QUOTED_IDENT || (tok.Kind == token.IDENT && !p.isReserved(tok)) {
		ident := p.parseIdentifierToken(p.advance())
		return &ast.IntoAlias{Span: span(intoTok.Pos, ident.End()), Identifier: ident}, nil
	}
	return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
}

// parseDMLTarget parses the target table path of a DML statement, allowing an
// optional leading FROM/INTO keyword handled by the caller.
func (p *parser) parseWithOffsetClause() (*ast.WithOffset, error) {
	withTok := p.advance() // WITH
	offsetTok, err := p.expectKeyword("OFFSET")
	if err != nil {
		return nil, err
	}
	offset := &ast.WithOffset{Span: span(withTok.Pos, offsetTok.End)}
	alias, err := p.parseOptionalAlias()
	if err != nil {
		return nil, err
	}
	if alias != nil {
		offset.Alias = alias
		offset.Stop = alias.End()
	}
	return offset, nil
}

// parseAssertRowsModified parses "ASSERT_ROWS_MODIFIED <expression>"; see
// opt_assert_rows_modified in googlesql.tm.
func (p *parser) parseAssertRowsModified() (*ast.AssertRowsModified, error) {
	kw := p.advance() // ASSERT_ROWS_MODIFIED
	expr, err := p.parsePossiblyCastIntLiteralOrParameter()
	if err != nil {
		return nil, err
	}
	return &ast.AssertRowsModified{Span: span(kw.Pos, p.extEnd(expr)), Value: expr}, nil
}

// parsePossiblyCastIntLiteralOrParameter parses an integer literal, a
// parameter, or a CAST of one of those (recursively); see
// possibly_cast_int_literal_or_parameter in googlesql.tm. Used for
// ASSERT_ROWS_MODIFIED and LIMIT-style clauses that only accept a restricted
// value.
func (p *parser) parsePossiblyCastIntLiteralOrParameter() (ast.Node, error) {
	if isKeyword(p.peek(), "CAST") || isKeyword(p.peek(), "SAFE_CAST") {
		kw := p.advance()
		isSafe := strings.EqualFold(kw.Image, "SAFE_CAST")
		if _, err := p.expect(token.LPAREN, `"("`); err != nil {
			return nil, err
		}
		// The CAST argument is not recursive: only an integer literal or a
		// parameter is allowed, not another cast.
		inner, err := p.parseIntLiteralOrParameter()
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
		return &ast.CastExpression{Span: span(kw.Pos, rparen.End), Expr: inner, Type: typ, Format: format, IsSafeCast: isSafe}, nil
	}
	tok := p.peek()
	if tok.Kind == token.INT || tok.Kind == token.PARAM || tok.Kind == token.QUESTION || tok.Kind == token.SYSTEM_VARIABLE {
		return p.parseIntLiteralOrParameter()
	}
	return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
}

// parseIntLiteralOrParameter parses an integer literal or a parameter; see
// int_literal_or_parameter in googlesql.tm.
func (p *parser) parseIntLiteralOrParameter() (ast.Node, error) {
	tok := p.peek()
	switch tok.Kind {
	case token.INT:
		p.advance()
		return &ast.IntLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image}, nil
	case token.PARAM:
		p.advance()
		name := &ast.Identifier{Span: span(tok.Pos+1, tok.End), Name: tok.Image[1:]}
		return &ast.ParameterExpr{Span: span(tok.Pos, tok.End), Name: name}, nil
	case token.QUESTION:
		p.advance()
		return &ast.ParameterExpr{Span: span(tok.Pos, tok.End), Position: p.positionalParameterOrdinal()}, nil
	case token.SYSTEM_VARIABLE:
		return p.parseSystemVariableExpr()
	}
	return nil, p.errorf(tok.Pos, `Syntax error: Expected "@" or "@@" or integer literal but got %s`, describeToken(tok))
}

// parseReturningClause parses "THEN RETURN [WITH ACTION [AS alias]]
// select_list"; see opt_returning_clause in googlesql.tm.
func (p *parser) parseReturningClause() (*ast.ReturningClause, error) {
	thenTok := p.advance() // THEN
	if _, err := p.expectKeyword("RETURN"); err != nil {
		return nil, err
	}
	rc := &ast.ReturningClause{Span: span(thenTok.Pos, 0)}
	var actionAlias *ast.Alias
	if isKeyword(p.peek(), "WITH") {
		p.advance() // WITH
		actionTok, err := p.expectKeyword("ACTION")
		if err != nil {
			return nil, err
		}
		// WITH ACTION [AS alias]: with no alias the column name defaults to
		// "ACTION" and takes the location of the ACTION keyword.
		alias := &ast.Alias{Identifier: &ast.Identifier{Span: span(actionTok.Pos, actionTok.End), Name: "ACTION"}}
		if isKeyword(p.peek(), "AS") {
			named, err := p.parseOptionalAlias()
			if err != nil {
				return nil, err
			}
			if named != nil {
				alias.Identifier = named.Identifier
			}
		}
		actionAlias = alias
	}
	list, err := p.parseSelectList()
	if err != nil {
		return nil, err
	}
	rc.SelectList = list
	rc.Stop = list.End()
	if actionAlias != nil {
		// The action alias node spans the whole returning clause.
		actionAlias.Span = span(rc.Pos(), rc.End())
		rc.ActionAlias = actionAlias
	}
	return rc, nil
}

// parseDeleteStatement parses a DELETE statement; see delete_statement in
// googlesql.tm.
func (p *parser) parseDeleteStatement() (ast.Statement, error) {
	deleteTok := p.advance() // DELETE
	if isKeyword(p.peek(), "FROM") {
		p.advance()
	}
	target, err := p.parseMaybeDashedGeneralizedPathExpression()
	if err != nil {
		return nil, err
	}
	stmt := &ast.DeleteStatement{Span: span(deleteTok.Pos, p.extEnd(target)), Target: target}
	// Optional table alias, before the WITH OFFSET / WHERE clauses; see
	// as_alias in delete_statement in googlesql.tm.
	alias, err := p.parseOptionalAlias()
	if err != nil {
		return nil, err
	}
	if alias != nil {
		stmt.Alias = alias
		stmt.Stop = alias.End()
	}
	if isKeyword(p.peek(), "WITH") {
		offset, err := p.parseWithOffsetClause()
		if err != nil {
			return nil, err
		}
		stmt.Offset = offset
		stmt.Stop = offset.End()
	}
	if isKeyword(p.peek(), "WHERE") {
		p.advance()
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		stmt.Where = expr
		stmt.Stop = p.extEnd(expr)
	}
	if isKeyword(p.peek(), "ASSERT_ROWS_MODIFIED") {
		arm, err := p.parseAssertRowsModified()
		if err != nil {
			return nil, err
		}
		stmt.AssertRowsModified = arm
		stmt.Stop = arm.End()
	}
	if isKeyword(p.peek(), "THEN") {
		rc, err := p.parseReturningClause()
		if err != nil {
			return nil, err
		}
		stmt.Returning = rc
		stmt.Stop = rc.End()
	}
	return stmt, nil
}

// parseInsertStatement parses an INSERT statement; see insert_statement in
// googlesql.tm.
func (p *parser) parseInsertStatement() (ast.Statement, error) {
	insertTok := p.advance() // INSERT
	stmt := &ast.InsertStatement{Span: span(insertTok.Pos, 0)}
	// INSERT [OR] {IGNORE|REPLACE|UPDATE]. Per insert_mode in googlesql.tm the
	// "OR" prefix requires one of the three mode keywords to follow.
	sawOr := isKeyword(p.peek(), "OR")
	if sawOr {
		p.advance()
	}
	// The mode keywords IGNORE/REPLACE/UPDATE are non-reserved, so a mode
	// keyword directly followed by "." (e.g. "INSERT replace.foo") is the start
	// of the target path, not an insert mode. After an explicit "OR" a mode
	// keyword is always required, so the "." lookahead only applies otherwise.
	// See insert_mode in googlesql.tm.
	treatAsMode := sawOr || p.peekAt(1).Kind != token.DOT
	switch {
	case treatAsMode && isKeyword(p.peek(), "IGNORE"):
		p.advance()
		stmt.InsertMode = "IGNORE"
	case treatAsMode && isKeyword(p.peek(), "REPLACE"):
		p.advance()
		stmt.InsertMode = "REPLACE"
	case treatAsMode && isKeyword(p.peek(), "UPDATE"):
		p.advance()
		stmt.InsertMode = "UPDATE"
	default:
		if sawOr {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword IGNORE or keyword REPLACE or keyword UPDATE but got %s", describeToken(p.peek()))
		}
	}
	if isKeyword(p.peek(), "INTO") {
		p.advance()
	}
	target, err := p.parseMaybeDashedGeneralizedPathExpression()
	if err != nil {
		return nil, err
	}
	stmt.Target = target
	stmt.Stop = p.extEnd(target)
	// Optional column list "( ident, ... )". A "(" that opens a query instead
	// (e.g. "insert into T (select 5)") is left for the source parser; see
	// identifierPreceedsParenthsizedQuery in googlesql.tm.
	if p.peek().Kind == token.LPAREN && !p.parenOpensInsertQuery() {
		cols, err := p.parseInsertColumnList()
		if err != nil {
			return nil, err
		}
		stmt.Columns = cols
		stmt.Stop = cols.End()
	}
	// The rows source: VALUES ... or a query. Per insert_source_and_opt_on_conflict
	// in googlesql.tm, an ON CONFLICT clause is only allowed after VALUES or a
	// parenthesized query, never after a bare query.
	var allowOnConflict bool
	switch {
	case isKeyword(p.peek(), "VALUES"):
		rows, err := p.parseInsertValuesRowList()
		if err != nil {
			return nil, err
		}
		stmt.Rows = rows
		stmt.Stop = rows.End()
		allowOnConflict = true
	default:
		allowOnConflict = p.peek().Kind == token.LPAREN
		query, err := p.parseQuery()
		if err != nil {
			return nil, err
		}
		stmt.Query = query
		// The statement covers all consumed tokens; for a parenthesized query
		// source this includes the wrapping parens, which the reused inner
		// Query node's own location excludes.
		stmt.Stop = p.prevEnd()
	}
	if allowOnConflict && isKeyword(p.peek(), "ON") && isKeyword(p.peekAt(1), "CONFLICT") {
		oc, err := p.parseOnConflictClause()
		if err != nil {
			return nil, err
		}
		stmt.OnConflict = oc
		stmt.Stop = oc.End()
	}
	if isKeyword(p.peek(), "ASSERT_ROWS_MODIFIED") {
		arm, err := p.parseAssertRowsModified()
		if err != nil {
			return nil, err
		}
		stmt.AssertRowsModified = arm
		stmt.Stop = arm.End()
	}
	if isKeyword(p.peek(), "THEN") {
		rc, err := p.parseReturningClause()
		if err != nil {
			return nil, err
		}
		stmt.Returning = rc
		stmt.Stop = rc.End()
	}
	return stmt, nil
}

// parseOnConflictClause parses "ON CONFLICT [conflict_target] DO
// (NOTHING | UPDATE SET update_item, ... [WHERE expr])"; see on_conflict_clause
// in googlesql.tm. The next tokens are "ON" "CONFLICT".
func (p *parser) parseOnConflictClause() (*ast.OnConflictClause, error) {
	onTok := p.advance() // ON
	p.advance()          // CONFLICT
	clause := &ast.OnConflictClause{Span: span(onTok.Pos, 0)}
	// Optional conflict_target: a column_list "(...)" or "ON UNIQUE CONSTRAINT
	// identifier".
	switch {
	case p.peek().Kind == token.LPAREN:
		cols, err := p.parseInsertColumnList()
		if err != nil {
			return nil, err
		}
		clause.ConflictTarget = cols
	case isKeyword(p.peek(), "ON"):
		p.advance() // ON
		if _, err := p.expectKeyword("UNIQUE"); err != nil {
			return nil, err
		}
		if _, err := p.expectKeyword("CONSTRAINT"); err != nil {
			return nil, err
		}
		ident, err := p.parseIdentifier()
		if err != nil {
			return nil, err
		}
		clause.ConflictTarget = ident
	}
	if _, err := p.expectKeyword("DO"); err != nil {
		return nil, err
	}
	switch {
	case isKeyword(p.peek(), "NOTHING"):
		nothingTok := p.advance()
		clause.ConflictAction = "NOTHING"
		clause.Stop = nothingTok.End
	case isKeyword(p.peek(), "UPDATE"):
		p.advance() // UPDATE
		clause.ConflictAction = "UPDATE"
		if _, err := p.expectKeyword("SET"); err != nil {
			return nil, err
		}
		items, err := p.parseUpdateItemList()
		if err != nil {
			return nil, err
		}
		clause.UpdateItemList = items
		clause.Stop = items.End()
		if isKeyword(p.peek(), "WHERE") {
			p.advance()
			expr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			clause.UpdateWhere = expr
			clause.Stop = p.extEnd(expr)
		}
	default:
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword NOTHING or keyword UPDATE but got %s", describeToken(p.peek()))
	}
	return clause, nil
}

// parenOpensInsertQuery reports whether a "(" following the INSERT target
// begins a parenthesized query (its source) rather than a column list. A
// column list starts with an identifier; a query primary starts with SELECT,
// WITH, FROM, TABLE, or another "(".
func (p *parser) parenOpensInsertQuery() bool {
	next := p.peekAt(1)
	return next.Kind == token.LPAREN ||
		isKeyword(next, "SELECT") || isKeyword(next, "WITH") ||
		isKeyword(next, "FROM") || isKeyword(next, "TABLE")
}

// parseInsertColumnList parses "( ident, ... )" with the opening parenthesis
// as the next token; see column_list in googlesql.tm.
func (p *parser) parseInsertColumnList() (*ast.ColumnList, error) {
	lparen := p.advance() // (
	list := &ast.ColumnList{Span: span(lparen.Pos, 0)}
	for {
		tok := p.peek()
		if (tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT) || p.isReserved(tok) {
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
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

// parseInsertValuesRowList parses "VALUES ( expr, ... ), ..."; see
// insert_values_list in googlesql.tm.
func (p *parser) parseInsertValuesRowList() (*ast.InsertValuesRowList, error) {
	valuesTok := p.advance() // VALUES
	list := &ast.InsertValuesRowList{Span: span(valuesTok.Pos, 0)}
	for {
		lparen, err := p.expect(token.LPAREN, `"("`)
		if err != nil {
			return nil, err
		}
		row := &ast.InsertValuesRow{Span: span(lparen.Pos, 0)}
		for {
			var value ast.Node
			if isKeyword(p.peek(), "DEFAULT") {
				def := p.advance()
				value = &ast.DefaultLiteral{Span: span(def.Pos, def.End)}
			} else {
				value, err = p.parseExpression()
				if err != nil {
					return nil, err
				}
			}
			row.Values = append(row.Values, value)
			if p.peek().Kind != token.COMMA {
				break
			}
			p.advance()
		}
		rparen, err := p.expect(token.RPAREN, `")"`)
		if err != nil {
			return nil, err
		}
		row.Stop = rparen.End
		list.Rows = append(list.Rows, row)
		list.Stop = row.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance()
	}
	return list, nil
}

// parseUpdateStatement parses an UPDATE statement; see update_statement in
// googlesql.tm.
func (p *parser) parseUpdateStatement() (ast.Statement, error) {
	updateTok := p.advance() // UPDATE
	target, err := p.parseMaybeDashedGeneralizedPathExpression()
	if err != nil {
		return nil, err
	}
	stmt := &ast.UpdateStatement{Span: span(updateTok.Pos, p.extEnd(target)), Target: target}
	// Optional table alias, before the WITH OFFSET / SET clauses; see as_alias
	// in update_statement in googlesql.tm.
	alias, err := p.parseOptionalAlias()
	if err != nil {
		return nil, err
	}
	if alias != nil {
		stmt.Alias = alias
		stmt.Stop = alias.End()
	}
	if isKeyword(p.peek(), "WITH") {
		offset, err := p.parseWithOffsetClause()
		if err != nil {
			return nil, err
		}
		stmt.Offset = offset
		stmt.Stop = offset.End()
	}
	// After the target and optional alias/offset the LALR parser can still shift
	// an alias identifier or WITH, so a non-SET token here is reported
	// generically as unexpected rather than "Expected keyword SET".
	if !isKeyword(p.peek(), "SET") {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
	}
	p.advance() // SET
	items, err := p.parseUpdateItemList()
	if err != nil {
		return nil, err
	}
	stmt.UpdateItemList = items
	stmt.Stop = items.End()
	if isKeyword(p.peek(), "FROM") {
		from, err := p.parseFromClause()
		if err != nil {
			return nil, err
		}
		stmt.From = from
		stmt.Stop = from.End()
	}
	if isKeyword(p.peek(), "WHERE") {
		p.advance()
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		stmt.Where = expr
		stmt.Stop = p.extEnd(expr)
	}
	if isKeyword(p.peek(), "ASSERT_ROWS_MODIFIED") {
		arm, err := p.parseAssertRowsModified()
		if err != nil {
			return nil, err
		}
		stmt.AssertRowsModified = arm
		stmt.Stop = arm.End()
	}
	if isKeyword(p.peek(), "THEN") {
		rc, err := p.parseReturningClause()
		if err != nil {
			return nil, err
		}
		stmt.Returning = rc
		stmt.Stop = rc.End()
	}
	return stmt, nil
}

// parseUpdateItemList parses "update_item, ..." for an UPDATE SET clause; see
// update_item_list in googlesql.tm.
func (p *parser) parseUpdateItemList() (*ast.UpdateItemList, error) {
	first, err := p.parseUpdateItem()
	if err != nil {
		return nil, err
	}
	list := &ast.UpdateItemList{Span: span(first.Pos(), first.End()), Items: []*ast.UpdateItem{first}}
	for p.peek().Kind == token.COMMA {
		p.advance()
		item, err := p.parseUpdateItem()
		if err != nil {
			return nil, err
		}
		list.Items = append(list.Items, item)
		list.Stop = item.End()
	}
	return list, nil
}

// parseUpdateItem parses a single UPDATE SET item: either a nested
// "( INSERT|UPDATE|DELETE ... )" statement or a "path = value" assignment; see
// update_item in googlesql.tm.
func (p *parser) parseUpdateItem() (*ast.UpdateItem, error) {
	// A "(" always begins a nested DML statement (INSERT/UPDATE/DELETE); a
	// generalized path may not begin with a parenthesized component. See
	// nested_dml_statement and the comment on update_item in googlesql.tm.
	if p.peek().Kind == token.LPAREN {
		lparen := p.advance() // (
		if !isKeyword(p.peek(), "INSERT") && !isKeyword(p.peek(), "UPDATE") && !isKeyword(p.peek(), "DELETE") {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword DELETE or keyword INSERT or keyword UPDATE but got %s", describeToken(p.peek()))
		}
		inner, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		rparen, err := p.expect(token.RPAREN, `")"`)
		if err != nil {
			return nil, err
		}
		return &ast.UpdateItem{Span: span(lparen.Pos, rparen.End), Statement: inner}, nil
	}
	// The set-value target is a generalized path expression. When it cannot
	// begin one, the LALR parser is still expecting the "(" of a nested DML, so
	// the error names "(" rather than "identifier".
	if tok := p.peek(); tok.Kind != token.QUOTED_IDENT && (tok.Kind != token.IDENT || p.isReserved(tok)) {
		return nil, p.errorf(tok.Pos, `Syntax error: Expected "(" but got %s`, describeToken(tok))
	}
	path, err := p.parseGeneralizedPathExpression()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(token.EQ, `"="`); err != nil {
		return nil, err
	}
	var value ast.Node
	if isKeyword(p.peek(), "DEFAULT") {
		def := p.advance()
		value = &ast.DefaultLiteral{Span: span(def.Pos, def.End)}
	} else {
		value, err = p.parseExpression()
		if err != nil {
			return nil, err
		}
	}
	setValue := &ast.UpdateSetValue{Span: span(path.Pos(), p.extEnd(value)), Path: path, Value: value}
	return &ast.UpdateItem{Span: setValue.Span, SetValue: setValue}, nil
}

// parseMergeStatement parses a MERGE statement; see merge_statement in
// googlesql.tm.
func (p *parser) parseMergeStatement() (ast.Statement, error) {
	mergeTok := p.advance() // MERGE
	if isKeyword(p.peek(), "INTO") {
		p.advance()
	}
	target, err := p.parseMaybeDashedPathExpression()
	if err != nil {
		return nil, err
	}
	stmt := &ast.MergeStatement{Span: span(mergeTok.Pos, 0), Target: target}
	alias, err := p.parseOptionalAlias()
	if err != nil {
		return nil, err
	}
	stmt.Alias = alias
	// After the target (and optional alias) the reference LALR parser is in a
	// state where several tokens are valid, so a non-USING token yields a
	// generic "Unexpected" error rather than "Expected keyword USING".
	if !isKeyword(p.peek(), "USING") {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
	}
	p.advance() // USING
	source, err := p.parseMergeSource()
	if err != nil {
		return nil, err
	}
	stmt.Source = source
	if _, err := p.expectKeyword("ON"); err != nil {
		return nil, err
	}
	cond, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	stmt.MergeCondition = cond
	whenList, err := p.parseMergeWhenClauseList()
	if err != nil {
		return nil, err
	}
	stmt.WhenClauseList = whenList
	stmt.Stop = whenList.End()
	return stmt, nil
}

// parseMergeSource parses the USING source of a MERGE statement: either a
// table path expression or a parenthesized subquery; see merge_source in
// googlesql.tm.
func (p *parser) parseMergeSource() (ast.Node, error) {
	if p.peek().Kind == token.LPAREN {
		return p.parseTableSubquery()
	}
	if tok := p.peek(); tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	// merge_source is a table_path_expression, whose base is a
	// maybe_slashed_or_dashed_path_expression; the first component may be a
	// dashed identifier. See table_path_expression_base in googlesql.tm.
	path, err := p.parseMaybeDashedPathExpression()
	if err != nil {
		return nil, err
	}
	return &ast.TablePathExpression{Span: span(path.Pos(), path.End()), Path: path}, nil
}

// parseMergeWhenClauseList parses one or more WHEN clauses of a MERGE
// statement; see merge_when_clause_list in googlesql.tm.
func (p *parser) parseMergeWhenClauseList() (*ast.MergeWhenClauseList, error) {
	first, err := p.parseMergeWhenClause()
	if err != nil {
		return nil, err
	}
	list := &ast.MergeWhenClauseList{Span: span(first.Pos(), first.End()), Clauses: []*ast.MergeWhenClause{first}}
	for isKeyword(p.peek(), "WHEN") {
		clause, err := p.parseMergeWhenClause()
		if err != nil {
			return nil, err
		}
		list.Clauses = append(list.Clauses, clause)
		list.Stop = clause.End()
	}
	return list, nil
}

// parseMergeWhenClause parses a single WHEN clause of a MERGE statement; see
// merge_when_clause in googlesql.tm.
func (p *parser) parseMergeWhenClause() (*ast.MergeWhenClause, error) {
	whenTok, err := p.expectKeyword("WHEN")
	if err != nil {
		return nil, err
	}
	clause := &ast.MergeWhenClause{Span: span(whenTok.Pos, 0)}
	switch {
	case isKeyword(p.peek(), "MATCHED"):
		p.advance()
		clause.MatchType = "MATCHED"
	case isKeyword(p.peek(), "NOT"):
		p.advance()
		if _, err := p.expectKeyword("MATCHED"); err != nil {
			return nil, err
		}
		clause.MatchType = "NOT_MATCHED_BY_TARGET"
		if isKeyword(p.peek(), "BY") {
			p.advance()
			switch {
			case isKeyword(p.peek(), "SOURCE"):
				p.advance()
				clause.MatchType = "NOT_MATCHED_BY_SOURCE"
			case isKeyword(p.peek(), "TARGET"):
				p.advance()
				clause.MatchType = "NOT_MATCHED_BY_TARGET"
			default:
				return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword SOURCE or keyword TARGET but got %s", describeToken(p.peek()))
			}
		}
	default:
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword MATCHED or keyword NOT but got %s", describeToken(p.peek()))
	}
	// Optional "AND <search condition>".
	if isKeyword(p.peek(), "AND") {
		p.advance()
		cond, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		clause.SearchCondition = cond
	}
	if _, err := p.expectKeyword("THEN"); err != nil {
		return nil, err
	}
	action, err := p.parseMergeAction()
	if err != nil {
		return nil, err
	}
	clause.Action = action
	clause.Stop = action.End()
	return clause, nil
}

// parseMergeAction parses the INSERT, UPDATE, or DELETE action of a MERGE WHEN
// clause; see merge_action in googlesql.tm.
func (p *parser) parseMergeAction() (*ast.MergeAction, error) {
	tok := p.peek()
	switch {
	case isKeyword(tok, "INSERT"):
		insertTok := p.advance()
		action := &ast.MergeAction{Span: span(insertTok.Pos, insertTok.End), ActionType: "INSERT"}
		if p.peek().Kind == token.LPAREN {
			cols, err := p.parseInsertColumnList()
			if err != nil {
				return nil, err
			}
			action.InsertColumnList = cols
			action.Stop = cols.End()
		}
		switch {
		case isKeyword(p.peek(), "VALUES"):
			p.advance()
			row, err := p.parseMergeInsertValuesRow()
			if err != nil {
				return nil, err
			}
			action.InsertRow = row
			action.Stop = row.End()
		case isKeyword(p.peek(), "ROW"):
			rowTok := p.advance()
			action.InsertRow = &ast.InsertValuesRow{Span: span(rowTok.Pos, rowTok.End)}
			action.Stop = rowTok.End
		default:
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword VALUES or keyword ROW but got %s", describeToken(p.peek()))
		}
		return action, nil
	case isKeyword(tok, "UPDATE"):
		updateTok := p.advance()
		action := &ast.MergeAction{Span: span(updateTok.Pos, updateTok.End), ActionType: "UPDATE"}
		if _, err := p.expectKeyword("SET"); err != nil {
			return nil, err
		}
		items, err := p.parseUpdateItemList()
		if err != nil {
			return nil, err
		}
		action.UpdateItemList = items
		action.Stop = items.End()
		return action, nil
	case isKeyword(tok, "DELETE"):
		deleteTok := p.advance()
		return &ast.MergeAction{Span: span(deleteTok.Pos, deleteTok.End), ActionType: "DELETE"}, nil
	default:
		return nil, p.errorf(tok.Pos, "Syntax error: Expected keyword DELETE or keyword INSERT or keyword UPDATE but got %s", describeToken(tok))
	}
}

// parseMergeInsertValuesRow parses a single "( expr, ... )" row following the
// VALUES keyword in a MERGE INSERT action; see insert_values_row in
// googlesql.tm.
func (p *parser) parseMergeInsertValuesRow() (*ast.InsertValuesRow, error) {
	lparen, err := p.expect(token.LPAREN, `"("`)
	if err != nil {
		return nil, err
	}
	row := &ast.InsertValuesRow{Span: span(lparen.Pos, 0)}
	for {
		var value ast.Node
		if isKeyword(p.peek(), "DEFAULT") {
			def := p.advance()
			value = &ast.DefaultLiteral{Span: span(def.Pos, def.End)}
		} else {
			value, err = p.parseExpression()
			if err != nil {
				return nil, err
			}
		}
		row.Values = append(row.Values, value)
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance()
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	row.Stop = rparen.End
	return row, nil
}

// parseTruncateStatement parses a "TRUNCATE TABLE <path> [WHERE <expr>]"
// statement; see truncate_statement in googlesql.tm.
func (p *parser) parseTruncateStatement() (ast.Statement, error) {
	truncateTok := p.advance() // TRUNCATE
	if _, err := p.expectKeyword("TABLE"); err != nil {
		return nil, err
	}
	target, err := p.parseMaybeDashedPathExpression()
	if err != nil {
		return nil, err
	}
	stmt := &ast.TruncateStatement{Span: span(truncateTok.Pos, target.End()), Target: target}
	if isKeyword(p.peek(), "WHERE") {
		p.advance()
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		stmt.Where = expr
		stmt.Stop = p.extEnd(expr)
	}
	return stmt, nil
}

// parseCloneDataStatement parses a "CLONE DATA INTO <path> FROM <source> [UNION
// ALL <source>]..." statement; see clone_data_statement in googlesql.tm.
func (p *parser) parseCloneDataStatement() (ast.Statement, error) {
	cloneTok := p.advance() // CLONE
	if _, err := p.expectKeyword("DATA"); err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("INTO"); err != nil {
		return nil, err
	}
	target, err := p.parseMaybeDashedPathExpression()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	list, err := p.parseCloneDataSourceList()
	if err != nil {
		return nil, err
	}
	return &ast.CloneDataStatement{Span: span(cloneTok.Pos, list.End()), Target: target, Sources: list}, nil
}

// parseCloneDataSourceList parses one or more clone data sources separated by
// "UNION ALL"; see clone_data_statement in googlesql.tm.
func (p *parser) parseCloneDataSourceList() (*ast.CloneDataSourceList, error) {
	first, err := p.parseCloneDataSource()
	if err != nil {
		return nil, err
	}
	list := &ast.CloneDataSourceList{Span: span(first.Pos(), first.End()), Sources: []*ast.CloneDataSource{first}}
	for isKeyword(p.peek(), "UNION") && isKeyword(p.peekAt(1), "ALL") {
		p.advance() // UNION
		p.advance() // ALL
		src, err := p.parseCloneDataSource()
		if err != nil {
			return nil, err
		}
		list.Sources = append(list.Sources, src)
		list.Stop = src.End()
	}
	return list, nil
}

// parseCloneDataSource parses a single clone data source: a table path with an
// optional FOR SYSTEM_TIME clause and WHERE clause; see clone_data_source in
// googlesql.tm.
func (p *parser) parseCloneDataSource() (*ast.CloneDataSource, error) {
	path, err := p.parseMaybeDashedPathExpression()
	if err != nil {
		return nil, err
	}
	src := &ast.CloneDataSource{Span: span(path.Pos(), path.End()), Path: path}
	if p.atForSystemTime() {
		fst, err := p.parseForSystemTime()
		if err != nil {
			return nil, err
		}
		src.ForSystemTime = fst
		src.Stop = fst.End()
	}
	if isKeyword(p.peek(), "WHERE") {
		whereTok := p.advance()
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		wc := &ast.WhereClause{Span: span(whereTok.Pos, p.extEnd(expr)), Expr: expr}
		src.Where = wc
		src.Stop = wc.End()
	}
	return src, nil
}

// parseGeneralizedPathExpression parses a path with optional "[expr]" array
// element, ".ident", and ".(path)" generalized field access; see
// generalized_path_expression in googlesql.tm.
func (p *parser) parseGeneralizedPathExpression() (ast.Node, error) {
	tok := p.peek()
	if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	if p.isReserved(tok) {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected keyword %s", strings.ToUpper(tok.Image))
	}
	first := p.parseIdentifierToken(p.advance())
	var expr ast.Node = &ast.PathExpression{Span: span(first.Pos(), first.End()), Names: []*ast.Identifier{first}}
	for {
		switch p.peek().Kind {
		case token.LBRACKET:
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
				Span:            span(expr.Pos(), rbracket.End),
				Array:           expr,
				BracketLocation: &ast.Location{Span: span(lbracket.Pos, lbracket.End)},
				Position:        position,
			}
		case token.DOT:
			if p.peekAt(1).Kind == token.LPAREN {
				p.advance() // .
				p.advance() // (
				inner, err := p.parsePathExpression()
				if err != nil {
					return nil, err
				}
				rparen, err := p.expect(token.RPAREN, `")"`)
				if err != nil {
					return nil, err
				}
				expr = &ast.DotGeneralizedField{Span: span(expr.Pos(), rparen.End), Expr: expr, Path: inner}
				continue
			}
			if p.peekAt(1).Kind != token.IDENT && p.peekAt(1).Kind != token.QUOTED_IDENT {
				return expr, nil
			}
			p.advance() // .
			ident := p.parseIdentifierToken(p.advance())
			if pe, ok := expr.(*ast.PathExpression); ok {
				pe.Names = append(pe.Names, ident)
				pe.Stop = ident.End()
				continue
			}
			expr = &ast.DotIdentifier{Span: span(expr.Pos(), ident.End()), Expr: expr, Name: ident}
		default:
			return expr, nil
		}
	}
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
			// CALL argument parsing preserves its existing "unexpected token"
			// diagnostics; the empty-list "Expected \")\"" rewrite applies only
			// to table-valued function calls.
			arg, err := p.parseTVFArgument(false)
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

// parseExecuteImmediateStatement parses "EXECUTE IMMEDIATE <expression>
// [INTO <identifier_list>] [USING <argument_list>]"; see execute_immediate in
// googlesql.tm.
func (p *parser) parseExecuteImmediateStatement() (ast.Statement, error) {
	execTok := p.advance() // EXECUTE
	if _, err := p.expectKeyword("IMMEDIATE"); err != nil {
		return nil, err
	}
	sql, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	stmt := &ast.ExecuteImmediateStatement{Span: span(execTok.Pos, p.extEnd(sql)), Sql: sql}
	if isKeyword(p.peek(), "INTO") {
		into, err := p.parseExecuteIntoClause()
		if err != nil {
			return nil, err
		}
		stmt.Into = into
		stmt.Stop = into.End()
	}
	if isKeyword(p.peek(), "USING") {
		using, err := p.parseExecuteUsingClause()
		if err != nil {
			return nil, err
		}
		stmt.Using = using
		stmt.Stop = using.End()
	}
	return stmt, nil
}

// parseExecuteIntoClause parses "INTO <identifier_list>"; see
// execute_into_clause in googlesql.tm.
func (p *parser) parseExecuteIntoClause() (*ast.ExecuteIntoClause, error) {
	intoTok := p.advance() // INTO
	list, err := p.parseIdentifierList()
	if err != nil {
		return nil, err
	}
	return &ast.ExecuteIntoClause{Span: span(intoTok.Pos, list.End()), Identifiers: list}, nil
}

// parseIdentifierList parses a comma-separated list of identifiers; see
// identifier_list in googlesql.tm.
func (p *parser) parseIdentifierList() (*ast.IdentifierList, error) {
	first, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	list := &ast.IdentifierList{Span: span(first.Pos(), first.End()), Identifiers: []*ast.Identifier{first}}
	for p.peek().Kind == token.COMMA {
		p.advance() // ,
		ident, err := p.parseIdentifier()
		if err != nil {
			return nil, err
		}
		list.Identifiers = append(list.Identifiers, ident)
		list.Stop = ident.End()
	}
	return list, nil
}

// parseIdentifier parses a single (possibly quoted) identifier; see identifier
// in googlesql.tm.
func (p *parser) parseIdentifier() (*ast.Identifier, error) {
	tok := p.peek()
	if tok.Kind != token.QUOTED_IDENT && (tok.Kind != token.IDENT || p.isReserved(tok)) {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	return p.parseIdentifierToken(p.advance()), nil
}

// parseExecuteUsingClause parses "USING <argument_list>"; see
// execute_using_clause in googlesql.tm.
func (p *parser) parseExecuteUsingClause() (*ast.ExecuteUsingClause, error) {
	usingTok := p.advance() // USING
	clause := &ast.ExecuteUsingClause{Span: span(usingTok.Pos, usingTok.End)}
	for {
		arg, err := p.parseExecuteUsingArgument()
		if err != nil {
			return nil, err
		}
		clause.Arguments = append(clause.Arguments, arg)
		clause.Stop = arg.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance() // ,
	}
	// The clause location starts at the first argument, matching the
	// execute_using_argument_list production (the "USING" keyword is not part
	// of the ASTExecuteUsingClause node).
	clause.Start = clause.Arguments[0].Pos()
	return clause, nil
}

// parseExecuteUsingArgument parses "<expression> [AS <identifier>]"; see
// execute_using_argument in googlesql.tm.
func (p *parser) parseExecuteUsingArgument() (*ast.ExecuteUsingArgument, error) {
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	arg := &ast.ExecuteUsingArgument{Span: span(p.extStart(expr), p.extEnd(expr)), Expr: expr}
	if isKeyword(p.peek(), "AS") {
		p.advance() // AS
		ident, err := p.parseIdentifier()
		if err != nil {
			return nil, err
		}
		arg.Alias = &ast.Alias{Span: span(ident.Pos(), ident.End()), Identifier: ident}
		arg.Stop = ident.End()
	}
	return arg, nil
}

// parseCreateStatement parses CREATE [OR REPLACE] [TEMP|TEMPORARY|PUBLIC|
// PRIVATE] TABLE [IF NOT EXISTS] <path> [AS query]; see
// create_table_statement in googlesql.tm. Other CREATE object kinds and the
// remaining optional clauses (table elements, options, PARTITION BY, ...)
// are not implemented yet.
func (p *parser) parseCreateStatement() (ast.Statement, error) {
	createTok := p.advance() // CREATE
	var isOrReplace bool
	if isKeyword(p.peek(), "OR") && isKeyword(p.peekAt(1), "REPLACE") {
		p.advance()
		p.advance()
		isOrReplace = true
	}
	// CREATE [OR REPLACE] [UNIQUE] [SEARCH|VECTOR] INDEX ...; see
	// create_index_statement in googlesql.tm. Index statements take no scope
	// modifier, so they are recognized before scope parsing.
	if isKeyword(p.peek(), "UNIQUE") || isKeyword(p.peek(), "NULL_FILTERED") ||
		isKeyword(p.peek(), "SEARCH") || isKeyword(p.peek(), "VECTOR") ||
		isKeyword(p.peek(), "INDEX") {
		return p.parseCreateIndexStatement(createTok, isOrReplace)
	}
	var scope string
	switch {
	case isKeyword(p.peek(), "TEMP"), isKeyword(p.peek(), "TEMPORARY"):
		p.advance()
		scope = "TEMP"
	case isKeyword(p.peek(), "PUBLIC"):
		p.advance()
		scope = "PUBLIC"
	case isKeyword(p.peek(), "PRIVATE"):
		p.advance()
		scope = "PRIVATE"
	}
	// OR REPLACE must precede the scope modifier, so a scope followed by OR is
	// reported as an unexpected OR keyword rather than a missing object type.
	if scope != "" && !isOrReplace && isKeyword(p.peek(), "OR") {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected keyword OR")
	}
	// A |> CREATE pipe operator uses the create_table_statement_prefix rule,
	// which only accepts TABLE as the object type; see pipe_create_table in
	// googlesql.tm. Any other (or missing) object type is reported as a missing
	// TABLE keyword rather than dispatching to the general object-type handlers.
	if p.inPipeCreateTable && !isKeyword(p.peek(), "TABLE") {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword TABLE but got %s", describeToken(p.peek()))
	}
	// CREATE DATABASE <name> [OPTIONS(...)]; see create_database_statement in
	// googlesql.tm. A database takes no OR REPLACE, scope, or IF NOT EXISTS
	// modifier, so any of those preceding DATABASE makes it an unexpected
	// keyword.
	if isKeyword(p.peek(), "DATABASE") {
		if isOrReplace || scope != "" {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected keyword DATABASE")
		}
		return p.parseCreateDatabaseStatement(createTok)
	}
	// CREATE [OR REPLACE] SCHEMA ...; see create_schema_statement in
	// googlesql.tm. A plain schema takes no scope modifier, so a scope
	// preceding SCHEMA makes it an unexpected keyword. EXTERNAL SCHEMA (which
	// does accept a scope) is handled separately below.
	if isKeyword(p.peek(), "SCHEMA") {
		if scope != "" {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected keyword SCHEMA")
		}
		return p.parseCreateSchemaStatement(createTok, isOrReplace)
	}
	// CREATE [OR REPLACE] SEQUENCE ...; see create_sequence_statement in
	// googlesql.tm. A sequence takes no scope modifier.
	if isKeyword(p.peek(), "SEQUENCE") {
		if scope != "" {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected keyword SEQUENCE")
		}
		return p.parseCreateSequenceStatement(createTok, isOrReplace)
	}
	// CREATE [OR REPLACE] [MATERIALIZED|APPROX] [RECURSIVE] VIEW ...; see
	// create_view_statement in googlesql.tm. MATERIALIZED and APPROX views do
	// not accept a scope modifier, so a scope followed by one is reported as
	// an unexpected keyword.
	if isKeyword(p.peek(), "MATERIALIZED") || isKeyword(p.peek(), "APPROX") {
		if scope != "" {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected keyword %s", strings.ToUpper(p.peek().Image))
		}
		viewKind := strings.ToUpper(p.peek().Image)
		p.advance()
		return p.parseCreateViewStatement(createTok, "", isOrReplace, viewKind)
	}
	if isKeyword(p.peek(), "RECURSIVE") || isKeyword(p.peek(), "VIEW") {
		return p.parseCreateViewStatement(createTok, scope, isOrReplace, "")
	}
	// CREATE [OR REPLACE] [scope] MODEL ...; see create_model_statement in
	// googlesql.tm.
	if isKeyword(p.peek(), "MODEL") {
		return p.parseCreateModelStatement(createTok, scope, isOrReplace)
	}
	// CREATE [OR REPLACE] [scope] CONSTANT [IF NOT EXISTS] <path> = <expr>; see
	// create_constant_statement in googlesql.tm.
	if isKeyword(p.peek(), "CONSTANT") {
		return p.parseCreateConstantStatement(createTok, scope, isOrReplace)
	}
	// CREATE [OR REPLACE] [scope] EXTERNAL TABLE FUNCTION is recognized only to
	// diagnose it; see create_external_table_function_statement in
	// googlesql.tm. The error points at the EXTERNAL keyword.
	if isKeyword(p.peek(), "EXTERNAL") && isKeyword(p.peekAt(1), "TABLE") && isKeyword(p.peekAt(2), "FUNCTION") {
		return nil, p.errorf(p.peek().Pos, "Syntax error: CREATE EXTERNAL TABLE FUNCTION is not supported")
	}
	// CREATE [OR REPLACE] [scope] EXTERNAL TABLE ...; see
	// create_external_table_statement in googlesql.tm.
	if isKeyword(p.peek(), "EXTERNAL") && isKeyword(p.peekAt(1), "TABLE") {
		return p.parseCreateExternalTableStatement(createTok, scope, isOrReplace)
	}
	// CREATE [OR REPLACE] [scope] EXTERNAL SCHEMA ...; see
	// create_external_schema_statement in googlesql.tm.
	if isKeyword(p.peek(), "EXTERNAL") && isKeyword(p.peekAt(1), "SCHEMA") {
		return p.parseCreateExternalSchemaStatement(createTok, scope, isOrReplace)
	}
	// CREATE [OR REPLACE] [scope] EXTERNAL must be followed by SCHEMA or TABLE;
	// there is no CREATE EXTERNAL MODEL. The error points at the token after
	// EXTERNAL.
	if isKeyword(p.peek(), "EXTERNAL") {
		p.advance() // EXTERNAL
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword SCHEMA or keyword TABLE but got %s", describeToken(p.peek()))
	}
	// CREATE [OR REPLACE] [scope] [AGGREGATE] FUNCTION is a scalar/aggregate
	// function; see create_function_statement in googlesql.tm.
	isAggregate := false
	if isKeyword(p.peek(), "AGGREGATE") && isKeyword(p.peekAt(1), "FUNCTION") {
		p.advance()
		isAggregate = true
	}
	if isKeyword(p.peek(), "FUNCTION") {
		return p.parseCreateFunctionStatement(createTok, scope, isOrReplace, isAggregate)
	}
	// CREATE [OR REPLACE] [scope] PROCEDURE ...; see
	// create_procedure_statement in googlesql.tm.
	if isKeyword(p.peek(), "PROCEDURE") {
		return p.parseCreateProcedureStatement(createTok, scope, isOrReplace)
	}
	// CREATE [OR REPLACE] ROW [ACCESS] POLICY ...; see
	// create_row_access_policy_statement in googlesql.tm. Scope modifiers
	// (TEMP/PUBLIC/PRIVATE) are not allowed here, so a scope followed by ROW is
	// reported as an unexpected ROW keyword.
	if isKeyword(p.peek(), "ROW") {
		if scope != "" {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
		}
		return p.parseCreateRowAccessPolicyStatement(createTok, isOrReplace)
	}
	// POLICY only follows ROW; on its own after CREATE it is unexpected rather
	// than a missing TABLE keyword.
	if isKeyword(p.peek(), "POLICY") {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
	}
	// MODULE is not a valid CREATE target; the reference parser reports it as
	// an unexpected keyword rather than a missing TABLE keyword.
	if isKeyword(p.peek(), "MODULE") {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected keyword %s", strings.ToUpper(p.peek().Image))
	}
	// A backtick-quoted object type is by design never a supported generic
	// entity type; the backticks are kept as part of the reported name. See
	// generic_entity_type_unchecked in googlesql.tm.
	if p.peek().Kind == token.QUOTED_IDENT {
		return nil, p.errorf(p.peek().Pos, "%s is not a supported object type", p.peek().Image)
	}
	// CREATE [OR REPLACE] generic_entity_type [IF NOT EXISTS] path ...; see
	// create_entity_statement in googlesql.tm. A generic entity type is a bare
	// identifier or the PROJECT keyword, and takes no scope modifier. When the
	// type is in the supported set the statement parses; otherwise it reports
	// "<type> is not a supported object type".
	if scope == "" && isGenericEntityTypeToken(p.peek()) {
		return p.parseCreateEntityStatement(createTok, isOrReplace)
	}
	// No recognized object-type keyword follows. Anything that is not TABLE is
	// simply unexpected here.
	if !isKeyword(p.peek(), "TABLE") {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
	}
	p.advance() // TABLE
	if isKeyword(p.peek(), "FUNCTION") {
		return p.parseCreateTableFunctionStatement(createTok, scope, isOrReplace)
	}
	stmt := &ast.CreateTableStatement{Span: span(createTok.Pos, 0), Scope: scope, IsOrReplace: isOrReplace}
	if isKeyword(p.peek(), "IF") && isKeyword(p.peekAt(1), "NOT") && isKeyword(p.peekAt(2), "EXISTS") {
		p.advance()
		p.advance()
		p.advance()
		stmt.IsIfNotExists = true
	}
	name, err := p.parseMaybeDashedPathExpression()
	if err != nil {
		return nil, err
	}
	stmt.Name = name
	stmt.Stop = name.End()

	// opt_table_element_list: "(column definitions and constraints)".
	if p.peek().Kind == token.LPAREN {
		tel, err := p.parseTableElementList()
		if err != nil {
			return nil, err
		}
		stmt.TableElementList = tel
		stmt.Stop = tel.End()
	}

	// opt_spanner_table_options: "PRIMARY KEY (...) [, INTERLEAVE IN PARENT ...]".
	if isKeyword(p.peek(), "PRIMARY") && isKeyword(p.peekAt(1), "KEY") {
		opts, err := p.parseSpannerTableOptions()
		if err != nil {
			return nil, err
		}
		stmt.SpannerOptions = opts
		stmt.Stop = opts.End()
	}

	// opt_like_path_expression: "LIKE table_name".
	if isKeyword(p.peek(), "LIKE") {
		p.advance()
		like, err := p.parseMaybeDashedPathExpression()
		if err != nil {
			return nil, err
		}
		stmt.LikeName = like
		stmt.Stop = like.End()
	}

	// opt_clone_table: "CLONE clone_data_source".
	if isKeyword(p.peek(), "CLONE") {
		p.advance() // CLONE
		clone, err := p.parseCloneDataSource()
		if err != nil {
			return nil, err
		}
		stmt.Clone = clone
		stmt.Stop = clone.End()
	}

	// opt_partition_by_clause_no_hint: "PARTITION BY expr, ...".
	if isKeyword(p.peek(), "PARTITION") {
		pb, err := p.parsePartitionBy()
		if err != nil {
			return nil, err
		}
		stmt.PartitionBy = pb
		stmt.Stop = pb.End()
	}

	// opt_cluster_by_clause_no_hint: "CLUSTER BY expr, ...".
	if isKeyword(p.peek(), "CLUSTER") {
		cb, err := p.parseClusterBy()
		if err != nil {
			return nil, err
		}
		stmt.ClusterBy = cb
		stmt.Stop = cb.End()
	}

	// opt_with_connection_clause: "WITH CONNECTION connection".
	if isKeyword(p.peek(), "WITH") {
		wc, err := p.parseWithConnectionClause()
		if err != nil {
			return nil, err
		}
		stmt.WithConnection = wc
		stmt.Stop = wc.End()
	}

	// opt_options_list: "OPTIONS(...)".
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance()
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		stmt.Options = opts
		stmt.Stop = opts.End()
	}

	// opt_as_query: "AS query". A |> CREATE TABLE pipe operator uses the
	// create_table_statement_prefix rule, which excludes the AS query; the AS
	// keyword is a dedicated error there. See pipe_create_table in googlesql.tm.
	if isKeyword(p.peek(), "AS") && p.inPipeCreateTable {
		return nil, p.errorf(p.peek().Pos, "Syntax error: AS query is not allowed on pipe CREATE TABLE")
	}
	if isKeyword(p.peek(), "AS") {
		p.advance()
		queryStart := p.peek().Pos
		query, err := p.parseQueryAfterAs()
		if err != nil {
			return nil, err
		}
		if hasLockMode(query) {
			return nil, p.errorf(queryStart, "Syntax error: Unexpected lock mode in query")
		}
		stmt.Query = query
		// The statement covers all consumed tokens, which can exceed the
		// query node's end: a parenthesized query keeps the location of the
		// query inside the parentheses.
		stmt.Stop = p.prevEnd()
	}
	return stmt, nil
}

// parseCreateDatabaseStatement parses the tail of "CREATE DATABASE <name>
// [OPTIONS(...)]"; see create_database_statement in googlesql.tm. DATABASE is
// the next token.
func (p *parser) parseCreateDatabaseStatement(createTok token.Token) (ast.Statement, error) {
	p.advance() // DATABASE
	name, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	stmt := &ast.CreateDatabaseStatement{Span: span(createTok.Pos, name.End()), Name: name}
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		stmt.Options = opts
		stmt.Stop = opts.End()
	}
	return stmt, nil
}

// parseCreateSchemaStatement parses the tail of "CREATE [OR REPLACE] SCHEMA
// [IF NOT EXISTS] <name> [DEFAULT COLLATE ...] [OPTIONS(...)]"; see
// create_schema_statement in googlesql.tm. SCHEMA is the next token.
func (p *parser) parseCreateSchemaStatement(createTok token.Token, isOrReplace bool) (ast.Statement, error) {
	p.advance() // SCHEMA
	stmt := &ast.CreateSchemaStatement{Span: span(createTok.Pos, 0), IsOrReplace: isOrReplace}
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
	// opt_default_collate_clause: "DEFAULT COLLATE <collation>".
	if isKeyword(p.peek(), "DEFAULT") {
		p.advance() // DEFAULT
		if !isKeyword(p.peek(), "COLLATE") {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword COLLATE but got %s", describeToken(p.peek()))
		}
		collate, err := p.parseCollate()
		if err != nil {
			return nil, err
		}
		stmt.Collate = collate
		stmt.Stop = collate.End()
	}
	// opt_options_list: "OPTIONS(...)".
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		stmt.Options = opts
		stmt.Stop = opts.End()
	}
	return stmt, nil
}

// parseCreateExternalSchemaStatement parses the tail of "CREATE [OR REPLACE]
// [scope] EXTERNAL SCHEMA [IF NOT EXISTS] <name> [WITH CONNECTION <connection>]
// OPTIONS(...)"; see create_external_schema_statement in googlesql.tm. EXTERNAL
// is the next token. The OPTIONS clause is required.
func (p *parser) parseCreateExternalSchemaStatement(createTok token.Token, scope string, isOrReplace bool) (ast.Statement, error) {
	p.advance() // EXTERNAL
	p.advance() // SCHEMA
	stmt := &ast.CreateExternalSchemaStatement{Span: span(createTok.Pos, 0), Scope: scope, IsOrReplace: isOrReplace}
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
	// opt_with_connection_clause: "WITH CONNECTION <connection>".
	if isKeyword(p.peek(), "WITH") {
		wc, err := p.parseWithConnectionClause()
		if err != nil {
			return nil, err
		}
		stmt.WithConnection = wc
		stmt.Stop = wc.End()
	}
	// options (required, not optional).
	if !isKeyword(p.peek(), "OPTIONS") {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword OPTIONS but got %s", describeToken(p.peek()))
	}
	p.advance() // OPTIONS
	opts, err := p.parseOptionsList()
	if err != nil {
		return nil, err
	}
	stmt.Options = opts
	stmt.Stop = opts.End()
	return stmt, nil
}

// parseCreateSequenceStatement parses the tail of "CREATE [OR REPLACE] SEQUENCE
// [IF NOT EXISTS] <name> [OPTIONS(...)]"; see create_sequence_statement in
// googlesql.tm. SEQUENCE is the next token.
func (p *parser) parseCreateSequenceStatement(createTok token.Token, isOrReplace bool) (ast.Statement, error) {
	p.advance() // SEQUENCE
	stmt := &ast.CreateSequenceStatement{Span: span(createTok.Pos, 0), IsOrReplace: isOrReplace}
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
	// opt_options_list: "OPTIONS(...)".
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		stmt.Options = opts
		stmt.Stop = opts.End()
	}
	return stmt, nil
}

// parseDefineTableStatement parses "DEFINE TABLE <name> (options)"; see
// define_table_statement in googlesql.tm. The options list is required and,
// unlike other statements, is written without a leading OPTIONS keyword.
func (p *parser) parseDefineTableStatement() (ast.Statement, error) {
	defineTok := p.advance() // DEFINE
	if _, err := p.expectKeyword("TABLE"); err != nil {
		return nil, err
	}
	name, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	// options_list is required and begins with "(". Because a path expression
	// could also continue with ".", the reference reports both alternatives.
	if p.peek().Kind != token.LPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" or "." but got %s`, describeToken(p.peek()))
	}
	opts, err := p.parseOptionsList()
	if err != nil {
		return nil, err
	}
	return &ast.DefineTableStatement{Span: span(defineTok.Pos, opts.End()), Name: name, Options: opts}, nil
}

// parseDefineMacroStatement parses "DEFINE MACRO <name> <body>"; see
// define_macro_statement in googlesql.tm. DEFINE and MACRO are the next two
// tokens. The macro name is the first raw body token; the body is the verbatim
// source text (whitespace and comments included) from the first body token to
// the last, terminated by ";" or end of input. Because our base lexer already
// respects string and comment boundaries, we recover the body as a raw text
// span over the already-tokenized stream rather than running a dedicated
// macro-body tokenizer mode.
func (p *parser) parseDefineMacroStatement() (ast.Statement, error) {
	startIdx := p.pos
	defineTok := p.advance() // DEFINE
	p.advance()              // MACRO
	// Macros must be enabled; otherwise the DEFINE keyword is reported as
	// unsupported. See ValidateMacroSupport in googlesql.tm. When macro
	// expansion is disabled the base tokenizer never marks DEFINE as the
	// special KW_DEFINE_FOR_MACROS, so this check comes first.
	if p.macroMode == "" || p.macroMode == "none" {
		return nil, p.errorf(defineTok.Pos, "Syntax error: DEFINE MACRO statements are not supported because macro expansions are disabled")
	}
	// The macro expander only marks DEFINE as KW_DEFINE_FOR_MACROS (an
	// "original" macro definition) when it is the first token of a statement,
	// i.e. at the start of the input or immediately after a ";" (ignoring
	// comments). A DEFINE MACRO anywhere else is treated as though it were
	// produced by expanding another macro and is rejected. See
	// LoadPotentiallySplicingTokens in googlesql/parser/macros/macro_expander.cc
	// and the "DEFINE" "MACRO" production in googlesql.tm.
	if startIdx != 0 && p.toks[startIdx-1].Kind != token.SEMICOLON {
		return nil, p.errorf(defineTok.Pos, "Syntax error: DEFINE MACRO statements cannot be composed from other expansions")
	}
	// The macro name is the first body token; it must be an identifier or
	// keyword. See IsIdentifierOrKeyword in googlesql.tm.
	nameTok := p.peek()
	if nameTok.Kind != token.IDENT && nameTok.Kind != token.QUOTED_IDENT {
		return nil, p.errorf(nameTok.Pos, "Syntax error: Expected macro name but got %s", describeToken(nameTok))
	}
	p.advance() // name
	name := p.parseIdentifierToken(nameTok)
	// The body runs from the token after the name up to the terminating ";" or
	// end of input, with all tokens in between forming MACRO_BODY_TOKENs.
	bodyFirst := p.pos
	for p.peek().Kind != token.SEMICOLON && p.peek().Kind != token.EOF {
		p.advance()
	}
	var body *ast.MacroBody
	if p.pos > bodyFirst {
		start := p.toks[bodyFirst].Pos
		end := p.toks[p.pos-1].End
		body = &ast.MacroBody{Span: span(start, end), Image: p.sql[start:end]}
	} else {
		// An empty macro body: an empty leaf at the position after the name.
		body = &ast.MacroBody{Span: span(nameTok.End, nameTok.End)}
	}
	return &ast.DefineMacroStatement{Span: span(defineTok.Pos, body.End()), Name: name, Body: body}, nil
}

// parseClusterBy parses "CLUSTER BY expression, ..."; see
// cluster_by_clause_prefix_no_hint in googlesql.tm. CLUSTER is the next token.
func (p *parser) parseClusterBy() (*ast.ClusterBy, error) {
	clusterTok := p.advance() // CLUSTER
	if _, err := p.expectKeyword("BY"); err != nil {
		return nil, err
	}
	clusterBy := &ast.ClusterBy{Span: span(clusterTok.Pos, 0)}
	for {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		clusterBy.Expressions = append(clusterBy.Expressions, expr)
		clusterBy.Stop = p.extEnd(expr)
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance()
	}
	return clusterBy, nil
}

// parseCreateExternalTableStatement parses the tail of "CREATE [OR REPLACE]
// [scope] EXTERNAL TABLE [IF NOT EXISTS] name [(table elements)]
// [WITH PARTITION COLUMNS [(...)]] [WITH CONNECTION conn] OPTIONS(...)"; see
// create_external_table_statement in googlesql.tm. EXTERNAL is the next token.
func (p *parser) parseCreateExternalTableStatement(createTok token.Token, scope string, isOrReplace bool) (ast.Statement, error) {
	p.advance() // EXTERNAL
	p.advance() // TABLE
	stmt := &ast.CreateExternalTableStatement{Span: span(createTok.Pos, 0), Scope: scope, IsOrReplace: isOrReplace}
	if isKeyword(p.peek(), "IF") && isKeyword(p.peekAt(1), "NOT") && isKeyword(p.peekAt(2), "EXISTS") {
		p.advance()
		p.advance()
		p.advance()
		stmt.IsIfNotExists = true
	}
	name, err := p.parseMaybeDashedPathExpression()
	if err != nil {
		return nil, err
	}
	stmt.Name = name
	stmt.Stop = name.End()

	hasTableElements := false
	if p.peek().Kind == token.LPAREN {
		tel, err := p.parseTableElementList()
		if err != nil {
			return nil, err
		}
		stmt.TableElementList = tel
		stmt.Stop = tel.End()
		hasTableElements = true
	}

	// Optional WITH PARTITION COLUMNS then WITH CONNECTION, in that order,
	// followed by the required OPTIONS clause.
	for isKeyword(p.peek(), "WITH") {
		next := p.peekAt(1)
		switch {
		case stmt.WithPartition == nil && stmt.WithConnection == nil && isKeyword(next, "PARTITION"):
			wp, err := p.parseWithPartitionColumnsClause()
			if err != nil {
				return nil, err
			}
			stmt.WithPartition = wp
			stmt.Stop = wp.End()
		case stmt.WithConnection == nil && isKeyword(next, "CONNECTION"):
			wc, err := p.parseWithConnectionClause()
			if err != nil {
				return nil, err
			}
			stmt.WithConnection = wc
			stmt.Stop = wc.End()
		case stmt.WithConnection != nil:
			// A connection clause was already parsed; only OPTIONS may follow.
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword OPTIONS but got %s", describeToken(p.peek()))
		case stmt.WithPartition != nil:
			// PARTITION COLUMNS already parsed; only CONNECTION may follow.
			return nil, p.errorf(next.Pos, "Syntax error: Expected keyword CONNECTION but got %s", describeToken(next))
		default:
			// No WITH clause parsed yet; both PARTITION and CONNECTION are valid.
			return nil, p.errorf(next.Pos, "Syntax error: Expected keyword CONNECTION or keyword PARTITION but got %s", describeToken(next))
		}
	}

	if !isKeyword(p.peek(), "OPTIONS") {
		tok := p.peek()
		switch {
		case stmt.WithConnection != nil:
			// A WITH CONNECTION clause was consumed; only OPTIONS may follow.
			return nil, p.errorf(tok.Pos, "Syntax error: Expected keyword OPTIONS but got %s", describeToken(tok))
		case stmt.WithPartition != nil:
			// After WITH PARTITION COLUMNS, WITH CONNECTION or OPTIONS may follow.
			return nil, p.errorf(tok.Pos, "Syntax error: Expected keyword OPTIONS or keyword WITH but got %s", describeToken(tok))
		case hasTableElements:
			return nil, p.errorf(tok.Pos, "Syntax error: Expected keyword DEFAULT or keyword LIKE or keyword OPTIONS or keyword WITH but got %s", describeToken(tok))
		default:
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
		}
	}
	p.advance() // OPTIONS
	opts, err := p.parseOptionsList()
	if err != nil {
		return nil, err
	}
	stmt.Options = opts
	stmt.Stop = opts.End()
	return stmt, nil
}

// parseWithPartitionColumnsClause parses "WITH PARTITION COLUMNS
// [(table elements)]"; see with_partition_columns_clause in googlesql.tm. WITH
// is the next token.
func (p *parser) parseWithPartitionColumnsClause() (*ast.WithPartitionColumnsClause, error) {
	withTok := p.advance() // WITH
	p.advance()            // PARTITION
	columnsTok, err := p.expectKeyword("COLUMNS")
	if err != nil {
		return nil, err
	}
	clause := &ast.WithPartitionColumnsClause{Span: span(withTok.Pos, columnsTok.End)}
	if p.peek().Kind == token.LPAREN {
		tel, err := p.parseTableElementList()
		if err != nil {
			return nil, err
		}
		clause.TableElementList = tel
		clause.Stop = tel.End()
	}
	return clause, nil
}

// parseTableElementList parses "(table_element [, table_element]...)"; see
// table_element_list in googlesql.tm. The opening parenthesis is the next
// token. An empty list "()" is an error unless FEATURE_SPANNER_LEGACY_DDL is
// enabled (which the parser does not support).
func (p *parser) parseTableElementList() (*ast.TableElementList, error) {
	lparen := p.advance() // (
	if p.peek().Kind == token.RPAREN {
		rparen := p.peek()
		// An empty "()" list is allowed only under FEATURE_SPANNER_LEGACY_DDL,
		// producing an empty TableElementList; see table_element_list in
		// googlesql.tm.
		if !p.features.Enabled(FeatureSpannerLegacyDDL) {
			return nil, p.errorf(rparen.Pos, "A table must define at least one column.")
		}
		p.advance() // )
		return &ast.TableElementList{Span: span(lparen.Pos, rparen.End)}, nil
	}
	list := &ast.TableElementList{Span: span(lparen.Pos, 0)}
	for {
		elem, err := p.parseTableElement()
		if err != nil {
			return nil, err
		}
		list.Elements = append(list.Elements, elem)
		switch p.peek().Kind {
		case token.COMMA:
			p.advance()
			// A trailing comma before ")" is allowed.
			if p.peek().Kind == token.RPAREN {
				rparen := p.advance()
				list.Stop = rparen.End
				return list, nil
			}
		case token.RPAREN:
			rparen := p.advance()
			list.Stop = rparen.End
			return list, nil
		default:
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected \")\" or \",\" but got %s", describeToken(p.peek()))
		}
	}
}

// parseTableElement parses a single column definition or table constraint; see
// table_element in googlesql.tm.
func (p *parser) parseTableElement() (ast.Node, error) {
	if isKeyword(p.peek(), "PRIMARY") && isKeyword(p.peekAt(1), "KEY") {
		return p.parsePrimaryKey()
	}
	if isKeyword(p.peek(), "CHECK") && p.peekAt(1).Kind == token.LPAREN {
		return p.parseCheckConstraint()
	}
	if isKeyword(p.peek(), "FOREIGN") && isKeyword(p.peekAt(1), "KEY") {
		return p.parseForeignKey()
	}
	// A named table constraint is "identifier identifier table_constraint_spec"
	// (the first identifier must be CONSTRAINT); see table_constraint_definition
	// in googlesql.tm. It is recognized by an "identifier identifier" prefix
	// followed by a table_constraint_spec start (CHECK "(" or FOREIGN "KEY").
	if isIdentToken(p.peek()) && isIdentToken(p.peekAt(1)) {
		third := p.peekAt(2)
		if (isKeyword(third, "CHECK") && p.peekAt(3).Kind == token.LPAREN) ||
			(isKeyword(third, "FOREIGN") && isKeyword(p.peekAt(3), "KEY")) {
			return p.parseNamedTableConstraint()
		}
	}
	return p.parseColumnDefinition()
}

// isIdentToken reports whether the token can be parsed as an identifier (a bare
// or backtick-quoted identifier).
func isIdentToken(tok token.Token) bool {
	return tok.Kind == token.IDENT || tok.Kind == token.QUOTED_IDENT
}

// parseNamedTableConstraint parses the "identifier identifier
// table_constraint_spec" form of table_constraint_definition; see googlesql.tm.
// The first identifier must be CONSTRAINT; otherwise the reference reports a
// spec-specific error. The constraint name is attached as the last child and
// the node's start is moved to the CONSTRAINT keyword.
func (p *parser) parseNamedTableConstraint() (ast.Node, error) {
	first := p.advance() // identifier (expected: CONSTRAINT)
	name, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	var spec ast.Node
	if isKeyword(p.peek(), "CHECK") {
		spec, err = p.parseCheckConstraint()
	} else {
		spec, err = p.parseForeignKey()
	}
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(first.Image, "CONSTRAINT") {
		switch spec.(type) {
		case *ast.CheckConstraint:
			return nil, p.errorf(first.Pos, "Syntax error: Expected CONSTRAINT for check constraint definition. Check constraints on columns are not supported. Define check constraints as table elements instead")
		case *ast.ForeignKey:
			return nil, p.errorf(first.Pos, "Syntax error: Expected CONSTRAINT for foreign key definition")
		}
	}
	switch c := spec.(type) {
	case *ast.CheckConstraint:
		c.ConstraintName = name
		c.Start = first.Pos
	case *ast.ForeignKey:
		c.ConstraintName = name
		c.Start = first.Pos
	}
	return spec, nil
}

// parseColumnDefinition parses "identifier column_schema [attributes]"; see
// table_column_definition in googlesql.tm.
func (p *parser) parseColumnDefinition() (*ast.ColumnDefinition, error) {
	name, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	schema, err := p.parseTableColumnSchema()
	if err != nil {
		return nil, err
	}
	// opt_options_list on a table_column_definition attaches to the schema node
	// (ExtendNodeRight in table_column_definition, googlesql.tm), extending its
	// end.
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		switch s := schema.(type) {
		case *ast.SimpleColumnSchema:
			s.Options, s.Stop = opts, opts.End()
		case *ast.ArrayColumnSchema:
			s.Options, s.Stop = opts, opts.End()
		case *ast.StructColumnSchema:
			s.Options, s.Stop = opts, opts.End()
		}
	}
	return &ast.ColumnDefinition{Span: span(name.Pos(), schema.End()), Name: name, Schema: schema}, nil
}

// parseTableColumnSchema parses a table_column_schema: "column_schema_inner
// opt_collate_clause opt_column_info" (a type, optional collation, and an
// optional DEFAULT or GENERATED/AS clause), followed by the column_attributes
// of the enclosing table_column_definition (parsed here so they attach to the
// schema node); see table_column_schema and table_column_definition in
// googlesql.tm.
func (p *parser) parseTableColumnSchema() (ast.Node, error) {
	schema, err := p.parseColumnSchemaInner()
	if err != nil {
		return nil, err
	}
	end := schema.End()
	// opt_collate_clause
	var collate *ast.Collate
	if isKeyword(p.peek(), "COLLATE") {
		c, err := p.parseCollate()
		if err != nil {
			return nil, err
		}
		collate = c
		end = c.End()
	}
	// opt_column_info: generated_column_info or default_column_info.
	genOrDefault, giEnd, err := p.parseOptColumnInfo()
	if err != nil {
		return nil, err
	}
	if genOrDefault != nil {
		end = giEnd
	}
	// column_attributes (from table_column_definition): NOT NULL, PRIMARY KEY.
	attrs := p.tryParseColumnAttributes()
	if attrs != nil {
		end = attrs.End()
	}
	switch s := schema.(type) {
	case *ast.SimpleColumnSchema:
		s.Collate, s.DefaultExpression, s.Attributes, s.Stop = collate, genOrDefault, attrs, end
	case *ast.ArrayColumnSchema:
		s.Collate, s.Attributes, s.Stop = collate, attrs, end
	case *ast.StructColumnSchema:
		s.Collate, s.Attributes, s.Stop = collate, attrs, end
	}
	return schema, nil
}

// parseOptColumnInfo parses opt_column_info: an optional generated_column_info
// ("[GENERATED [ALWAYS|BY DEFAULT]] AS ..." ) or default_column_info ("DEFAULT
// expression"); see opt_column_info in googlesql.tm. It returns the resulting
// node (a *GeneratedColumnInfo or an expression) and the offset just past it,
// or (nil, 0) when absent. Providing both is an error.
func (p *parser) parseOptColumnInfo() (ast.Node, int, error) {
	startsGenerated := func() bool {
		return isKeyword(p.peek(), "GENERATED") || isKeyword(p.peek(), "AS")
	}
	bothErr := `Syntax error: "DEFAULT" and "[GENERATED [ALWAYS | BY DEFAULT]] AS" clauses must not be both provided for the column`
	switch {
	case startsGenerated():
		info, err := p.parseGeneratedColumnInfo()
		if err != nil {
			return nil, 0, err
		}
		if isKeyword(p.peek(), "DEFAULT") {
			return nil, 0, p.errorf(p.peek().Pos, "%s", bothErr)
		}
		return info, info.End(), nil
	case isKeyword(p.peek(), "DEFAULT"):
		p.advance() // DEFAULT
		expr, err := p.parseExpression()
		if err != nil {
			return nil, 0, err
		}
		if startsGenerated() {
			return nil, 0, p.errorf(p.peek().Pos, "%s", bothErr)
		}
		return expr, p.extEnd(expr), nil
	}
	return nil, 0, nil
}

// parseGeneratedColumnInfo parses generated_column_info: "generated_mode (
// expression ) stored_mode" or "generated_mode identity_column_info"; see
// generated_column_info and generated_mode in googlesql.tm. generated_mode is
// "[GENERATED [ALWAYS | BY DEFAULT]] AS". The next token is GENERATED or AS.
func (p *parser) parseGeneratedColumnInfo() (*ast.GeneratedColumnInfo, error) {
	start := p.peek().Pos
	mode := "ALWAYS"
	if isKeyword(p.peek(), "GENERATED") {
		p.advance() // GENERATED
		switch {
		case isKeyword(p.peek(), "ALWAYS"):
			p.advance() // ALWAYS
			if _, err := p.expectKeyword("AS"); err != nil {
				return nil, err
			}
		case isKeyword(p.peek(), "BY"):
			p.advance() // BY
			if _, err := p.expectKeyword("DEFAULT"); err != nil {
				return nil, err
			}
			if _, err := p.expectKeyword("AS"); err != nil {
				return nil, err
			}
			mode = "BY_DEFAULT"
		default:
			if _, err := p.expectKeyword("AS"); err != nil {
				return nil, err
			}
		}
	} else {
		p.advance() // AS
	}
	if p.peek().Kind == token.LPAREN {
		p.advance() // (
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		rparen, err := p.expect(token.RPAREN, `")"`)
		if err != nil {
			return nil, err
		}
		info := &ast.GeneratedColumnInfo{Span: span(start, rparen.End), GeneratedMode: mode, Expression: expr}
		// stored_mode: "STORED" ["VOLATILE"] | %empty
		if isKeyword(p.peek(), "STORED") {
			storedTok := p.advance() // STORED
			if isKeyword(p.peek(), "VOLATILE") {
				volTok := p.advance()
				info.StoredMode, info.Stop = "STORED_VOLATILE", volTok.End
			} else {
				info.StoredMode, info.Stop = "STORED", storedTok.End
			}
		}
		return info, nil
	}
	// generated_mode identity_column_info
	identity, err := p.parseIdentityColumnInfo()
	if err != nil {
		return nil, err
	}
	return &ast.GeneratedColumnInfo{Span: span(start, identity.End()), GeneratedMode: mode, Identity: identity}, nil
}

// parseFieldSchema parses a field_schema: "column_schema_inner
// opt_collate_clause opt_field_attributes opt_options_list"; see field_schema
// in googlesql.tm. The only supported field attribute is NOT NULL, matching
// the grammar's opt_field_attributes.
func (p *parser) parseFieldSchema() (ast.Node, error) {
	schema, err := p.parseColumnSchemaInner()
	if err != nil {
		return nil, err
	}
	return p.parseColumnSchemaTail(schema, true)
}

// parseColumnSchemaTail parses the trailing "opt_collate_clause
// opt_field_attributes [opt_options_list]" of a field_schema (or struct
// column field) and attaches the results to schema, extending its span. When
// allowOptions is false, a trailing OPTIONS list is not consumed (unnamed
// struct fields cannot carry OPTIONS; see struct_column_field in
// googlesql.tm).
func (p *parser) parseColumnSchemaTail(schema ast.Node, allowOptions bool) (ast.Node, error) {
	var collate *ast.Collate
	if isKeyword(p.peek(), "COLLATE") {
		c, err := p.parseCollate()
		if err != nil {
			return nil, err
		}
		collate = c
	}
	var attrs *ast.ColumnAttributeList
	if isKeyword(p.peek(), "NOT") && isKeyword(p.peekAt(1), "NULL") {
		notTok := p.advance()  // NOT
		nullTok := p.advance() // NULL
		attr := &ast.NotNullColumnAttribute{Span: span(notTok.Pos, nullTok.End)}
		attrs = &ast.ColumnAttributeList{Span: span(notTok.Pos, nullTok.End), Attributes: []ast.Node{attr}}
	}
	var options *ast.OptionsList
	if allowOptions && isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		o, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		options = o
	}
	setColumnSchemaTail(schema, collate, attrs, options)
	return schema, nil
}

// setColumnSchemaTail attaches an optional collation, attribute list, and
// options list to a column schema node and extends its end location to the
// last present component.
func setColumnSchemaTail(schema ast.Node, collate *ast.Collate, attrs *ast.ColumnAttributeList, options *ast.OptionsList) {
	end := schema.End()
	if collate != nil {
		end = collate.End()
	}
	if attrs != nil {
		end = attrs.End()
	}
	if options != nil {
		end = options.End()
	}
	switch s := schema.(type) {
	case *ast.SimpleColumnSchema:
		s.Collate, s.Attributes, s.Options, s.Stop = collate, attrs, options, end
	case *ast.ArrayColumnSchema:
		s.Collate, s.Attributes, s.Options, s.Stop = collate, attrs, options, end
	case *ast.StructColumnSchema:
		s.Collate, s.Attributes, s.Options, s.Stop = collate, attrs, options, end
	}
}

// parseColumnSchemaInner parses "raw_column_schema_inner opt_type_parameters";
// see column_schema_inner in googlesql.tm.
func (p *parser) parseColumnSchemaInner() (ast.Node, error) {
	schema, err := p.parseRawColumnSchemaInner()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind == token.LPAREN {
		params, err := p.parseTypeParameterList()
		if err != nil {
			return nil, err
		}
		// TypeParameterList excludes its closing ")", which prevEnd supplies;
		// see the ExtendNodeRight in column_schema_inner in googlesql.tm.
		end := p.prevEnd()
		switch s := schema.(type) {
		case *ast.SimpleColumnSchema:
			s.TypeParameters, s.Stop = params, end
		case *ast.ArrayColumnSchema:
			s.TypeParameters, s.Stop = params, end
		case *ast.StructColumnSchema:
			s.TypeParameters, s.Stop = params, end
		}
	}
	return schema, nil
}

// parseRawColumnSchemaInner parses a simple, array, or struct column schema
// without trailing type parameters; see raw_column_schema_inner and
// simple_column_schema_inner in googlesql.tm.
func (p *parser) parseRawColumnSchemaInner() (ast.Node, error) {
	tok := p.peek()
	switch {
	case isKeyword(tok, "ARRAY"):
		return p.parseArrayColumnSchema()
	case isKeyword(tok, "STRUCT"):
		return p.parseStructColumnSchema()
	case isKeyword(tok, "INTERVAL"):
		// INTERVAL is a reserved keyword but still names a type; see
		// simple_column_schema_inner in googlesql.tm.
		id := p.parseIdentifierToken(p.advance())
		path := &ast.PathExpression{Span: span(tok.Pos, tok.End), Names: []*ast.Identifier{id}}
		return &ast.SimpleColumnSchema{Span: span(tok.Pos, tok.End), Type: path}, nil
	}
	// A column schema must begin with a type name (a path expression). A
	// non-identifier token here is reported as unexpected, matching the
	// reference's generic error at this LR state.
	if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	typePath, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	return &ast.SimpleColumnSchema{Span: span(typePath.Pos(), typePath.End()), Type: typePath}, nil
}

// parseArrayColumnSchema parses "ARRAY<field_schema>"; see
// array_column_schema_inner in googlesql.tm. ARRAY is the next token.
func (p *parser) parseArrayColumnSchema() (*ast.ArrayColumnSchema, error) {
	arrayTok := p.advance() // ARRAY
	// ARRAY has no empty form, so a "<>" token is not split; see parseArrayType.
	if _, err := p.expect(token.LT, `"<"`); err != nil {
		return nil, err
	}
	elem, err := p.parseFieldSchema()
	if err != nil {
		return nil, err
	}
	closeTok, err := p.expectTemplateClose()
	if err != nil {
		return nil, err
	}
	return &ast.ArrayColumnSchema{Span: span(arrayTok.Pos, closeTok.End), ElementSchema: elem}, nil
}

// parseStructColumnSchema parses "STRUCT<field, ...>" (possibly empty); see
// struct_column_schema_inner in googlesql.tm. STRUCT is the next token.
func (p *parser) parseStructColumnSchema() (*ast.StructColumnSchema, error) {
	structTok := p.advance() // STRUCT
	if _, err := p.expectTemplateOpen(); err != nil {
		return nil, err
	}
	st := &ast.StructColumnSchema{Span: span(structTok.Pos, 0)}
	if p.peek().Kind != token.GT && p.peek().Kind != token.RSHIFT && p.peek().Kind != token.EOF {
		for {
			field, err := p.parseStructColumnField()
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

// parseStructColumnField parses one struct column field: either "identifier
// field_schema" (named) or "column_schema_inner opt_collate_clause
// opt_field_attributes" (unnamed, which cannot carry OPTIONS); see
// struct_column_field in googlesql.tm.
func (p *parser) parseStructColumnField() (*ast.StructColumnField, error) {
	tok := p.peek()
	named := (tok.Kind == token.QUOTED_IDENT || (tok.Kind == token.IDENT && !p.isReserved(tok))) &&
		startsType(p.peekAt(1))
	if named {
		name := p.parseIdentifierToken(p.advance())
		schema, err := p.parseFieldSchema()
		if err != nil {
			return nil, err
		}
		return &ast.StructColumnField{Span: span(name.Pos(), schema.End()), Name: name, Schema: schema}, nil
	}
	schema, err := p.parseColumnSchemaInner()
	if err != nil {
		return nil, err
	}
	schema, err = p.parseColumnSchemaTail(schema, false)
	if err != nil {
		return nil, err
	}
	return &ast.StructColumnField{Span: span(schema.Pos(), schema.End()), Schema: schema}, nil
}

// tryParseColumnAttributes parses a run of column attributes trailing a column
// schema; see column_attributes in googlesql.tm. Only NOT NULL is implemented
// (the sole attribute exercised by the external-table tests). It returns nil
// when no attribute is present.
func (p *parser) tryParseColumnAttributes() *ast.ColumnAttributeList {
	startsAttr := func() bool {
		return (isKeyword(p.peek(), "NOT") && isKeyword(p.peekAt(1), "NULL")) ||
			(isKeyword(p.peek(), "PRIMARY") && isKeyword(p.peekAt(1), "KEY"))
	}
	if !startsAttr() {
		return nil
	}
	list := &ast.ColumnAttributeList{}
	for startsAttr() {
		if isKeyword(p.peek(), "PRIMARY") {
			primaryTok := p.advance() // PRIMARY
			keyTok := p.advance()     // KEY
			attr := &ast.PrimaryKeyColumnAttribute{Span: span(primaryTok.Pos, keyTok.End), Enforced: true}
			if len(list.Attributes) == 0 {
				list.Start = primaryTok.Pos
			}
			list.Stop = keyTok.End
			list.Attributes = append(list.Attributes, attr)
			// A trailing constraint_enforcement binds to the primary key
			// attribute, extending its (and the list's) end location; see the
			// "column_attributes constraint_enforcement" production in
			// googlesql.tm.
			if enforced, endPos, ok, err := p.tryParseConstraintEnforcement(); err == nil && ok {
				attr.Enforced = enforced
				attr.Stop = endPos
				list.Stop = endPos
			}
			continue
		}
		notTok := p.advance()  // NOT
		nullTok := p.advance() // NULL
		if len(list.Attributes) == 0 {
			list.Start = notTok.Pos
		}
		list.Stop = nullTok.End
		list.Attributes = append(list.Attributes, &ast.NotNullColumnAttribute{Span: span(notTok.Pos, nullTok.End)})
	}
	return list
}

// parsePrimaryKey parses "PRIMARY KEY (elements) [ENFORCED|NOT ENFORCED]
// [OPTIONS(...)]"; see primary_key_spec in googlesql.tm. PRIMARY is the next
// token.
func (p *parser) parsePrimaryKey() (*ast.PrimaryKey, error) {
	primaryTok := p.advance() // PRIMARY
	p.advance()               // KEY
	pk := &ast.PrimaryKey{Span: span(primaryTok.Pos, 0), Enforced: true}
	list, closeEnd, err := p.parsePrimaryKeyElementList()
	if err != nil {
		return nil, err
	}
	pk.ElementList = list
	pk.Stop = closeEnd
	if enforced, endPos, ok, err := p.tryParseConstraintEnforcement(); err != nil {
		return nil, err
	} else if ok {
		pk.Enforced = enforced
		pk.Stop = endPos
	}
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance()
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		pk.Options = opts
		pk.Stop = opts.End()
	}
	return pk, nil
}

// parseSpannerTableOptions parses opt_spanner_table_options: "PRIMARY KEY
// (elements) [, INTERLEAVE IN PARENT path [ON DELETE action]]"; see
// spanner_primary_key and opt_spanner_interleave_in_parent_clause in
// googlesql.tm. PRIMARY is the next token. Requires FEATURE_SPANNER_LEGACY_DDL.
func (p *parser) parseSpannerTableOptions() (*ast.SpannerTableOptions, error) {
	primaryPos := p.peek().Pos
	if !p.features.Enabled(FeatureSpannerLegacyDDL) {
		return nil, p.errorf(primaryPos, "PRIMARY KEY must be defined in the table element list as column attribute or constraint.")
	}
	pk, err := p.parsePrimaryKey()
	if err != nil {
		return nil, err
	}
	opts := &ast.SpannerTableOptions{Span: span(pk.Pos(), pk.End()), PrimaryKey: pk}
	// opt_spanner_interleave_in_parent_clause: ", INTERLEAVE IN PARENT path
	// [ON DELETE action]".
	if p.peek().Kind == token.COMMA && isKeyword(p.peekAt(1), "INTERLEAVE") &&
		isKeyword(p.peekAt(2), "IN") && isKeyword(p.peekAt(3), "PARENT") {
		commaTok := p.advance() // ,
		p.advance()             // INTERLEAVE
		p.advance()             // IN
		p.advance()             // PARENT
		path, err := p.parseMaybeDashedPathExpression()
		if err != nil {
			return nil, err
		}
		clause := &ast.SpannerInterleaveClause{Span: span(commaTok.Pos, path.End()), TableName: path, Type: "IN_PARENT", Action: "NO ACTION"}
		// opt_foreign_key_on_delete: "ON DELETE foreign_key_action".
		if isKeyword(p.peek(), "ON") && isKeyword(p.peekAt(1), "DELETE") {
			p.advance() // ON
			p.advance() // DELETE
			action, actEnd, err := p.parseForeignKeyAction()
			if err != nil {
				return nil, err
			}
			clause.Action = action
			clause.Stop = actEnd
		}
		opts.Interleave = clause
		opts.Stop = clause.End()
	}
	return opts, nil
}

// parsePrimaryKeyElementList parses "(primary_key_element [, ...])"; see
// primary_key_element_list in googlesql.tm. It returns the list (nil for an
// empty "()") and the offset just past the closing parenthesis.
func (p *parser) parsePrimaryKeyElementList() (*ast.PrimaryKeyElementList, int, error) {
	lparen, err := p.expect(token.LPAREN, "\"(\"")
	if err != nil {
		return nil, 0, err
	}
	if p.peek().Kind == token.RPAREN {
		rparen := p.advance()
		return nil, rparen.End, nil
	}
	list := &ast.PrimaryKeyElementList{Span: span(lparen.Pos, 0)}
	for {
		elem, err := p.parsePrimaryKeyElement()
		if err != nil {
			return nil, 0, err
		}
		list.Elements = append(list.Elements, elem)
		switch p.peek().Kind {
		case token.COMMA:
			p.advance()
		case token.RPAREN:
			rparen := p.advance()
			list.Stop = rparen.End
			return list, rparen.End, nil
		default:
			return nil, 0, p.errorf(p.peek().Pos, "Syntax error: Expected \")\" or \",\" but got %s", describeToken(p.peek()))
		}
	}
}

// parsePrimaryKeyElement parses "identifier [ASC|DESC]"; see
// primary_key_element in googlesql.tm. Ordering requires
// FEATURE_ORDERED_PRIMARY_KEYS; without it, an explicit ordering is an error.
func (p *parser) parsePrimaryKeyElement() (*ast.PrimaryKeyElement, error) {
	col, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	elem := &ast.PrimaryKeyElement{Span: span(col.Pos(), col.End()), Column: col}
	ordering := ""
	orderPos := -1
	if isKeyword(p.peek(), "ASC") {
		ordering = "ASC"
		orderPos = p.peek().Pos
		elem.Stop = p.advance().End
	} else if isKeyword(p.peek(), "DESC") {
		ordering = "DESC"
		orderPos = p.peek().Pos
		elem.Stop = p.advance().End
	}
	if ordering != "" && !p.features.Enabled(FeatureOrderedPrimaryKeys) {
		return nil, p.errorf(orderPos, "Ordering for primary keys is not supported")
	}
	elem.Ordering = ordering
	return elem, nil
}

// parseCheckConstraint parses "CHECK (expression) [ENFORCED|NOT ENFORCED]
// [OPTIONS(...)]"; see table_constraint_spec in googlesql.tm. CHECK is the
// next token.
func (p *parser) parseCheckConstraint() (*ast.CheckConstraint, error) {
	checkTok := p.advance() // CHECK
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	rparen, err := p.expect(token.RPAREN, "\")\"")
	if err != nil {
		return nil, err
	}
	cc := &ast.CheckConstraint{Span: span(checkTok.Pos, rparen.End), Enforced: true, Expression: expr}
	if enforced, endPos, ok, err := p.tryParseConstraintEnforcement(); err != nil {
		return nil, err
	} else if ok {
		cc.Enforced = enforced
		cc.Stop = endPos
	}
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance()
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		cc.Options = opts
		cc.Stop = opts.End()
	}
	return cc, nil
}

// parseForeignKey parses "FOREIGN KEY (columns) foreign_key_reference
// [ENFORCED|NOT ENFORCED] [OPTIONS(...)]"; see table_constraint_spec in
// googlesql.tm. FOREIGN is the next token. The reference's enforced flag is
// set from the trailing opt_constraint_enforcement.
func (p *parser) parseForeignKey() (*ast.ForeignKey, error) {
	foreignTok := p.advance() // FOREIGN
	if _, err := p.expectKeyword("KEY"); err != nil {
		return nil, err
	}
	cols, err := p.parseColumnListParen()
	if err != nil {
		return nil, err
	}
	ref, err := p.parseForeignKeyReference()
	if err != nil {
		return nil, err
	}
	fk := &ast.ForeignKey{Span: span(foreignTok.Pos, ref.End()), ColumnList: cols, Reference: ref}
	// opt_constraint_enforcement (default true) is applied to the reference and
	// extends the foreign key node's span.
	enforced, endPos, ok, err := p.tryParseConstraintEnforcement()
	if err != nil {
		return nil, err
	}
	ref.Enforced = enforced
	if ok {
		fk.Stop = endPos
	}
	// opt_options_list
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance()
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		fk.Options = opts
		fk.Stop = opts.End()
	}
	return fk, nil
}

// parseForeignKeyReference parses "REFERENCES path (columns)
// [MATCH mode] [actions]"; see foreign_key_reference in googlesql.tm. The
// enforced flag is left at its default (true) and set by the caller.
func (p *parser) parseForeignKeyReference() (*ast.ForeignKeyReference, error) {
	refTok, err := p.expectKeyword("REFERENCES")
	if err != nil {
		return nil, err
	}
	path, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	cols, err := p.parseColumnListParen()
	if err != nil {
		return nil, err
	}
	ref := &ast.ForeignKeyReference{Span: span(refTok.Pos, 0), Match: "SIMPLE", Enforced: true, Reference: path, ColumnList: cols}
	// opt_foreign_key_match
	matchEnd := cols.End()
	if isKeyword(p.peek(), "MATCH") {
		p.advance() // MATCH
		switch {
		case isKeyword(p.peek(), "SIMPLE"):
			ref.Match = "SIMPLE"
			matchEnd = p.advance().End
		case isKeyword(p.peek(), "FULL"):
			ref.Match = "FULL"
			matchEnd = p.advance().End
		case isKeyword(p.peek(), "NOT") && isKeyword(p.peekAt(1), "DISTINCT"):
			p.advance() // NOT
			ref.Match = "NOT DISTINCT"
			matchEnd = p.advance().End
		default:
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword FULL or keyword SIMPLE but got %s", describeToken(p.peek()))
		}
	}
	// opt_foreign_key_actions
	actions, actEnd, err := p.parseForeignKeyActions(matchEnd)
	if err != nil {
		return nil, err
	}
	ref.Actions = actions
	ref.Stop = actEnd
	return ref, nil
}

// parseForeignKeyActions parses opt_foreign_key_actions: an ON UPDATE and/or ON
// DELETE clause in either order; see opt_foreign_key_actions in googlesql.tm.
// emptyPos is the location used for the (always-present) actions node when no
// clause is given. It returns the actions node and the offset just past it.
func (p *parser) parseForeignKeyActions(emptyPos int) (*ast.ForeignKeyActions, int, error) {
	actions := &ast.ForeignKeyActions{UpdateAction: "NO ACTION", DeleteAction: "NO ACTION"}
	start := -1
	end := emptyPos
	for isKeyword(p.peek(), "ON") && (isKeyword(p.peekAt(1), "UPDATE") || isKeyword(p.peekAt(1), "DELETE")) {
		onTok := p.advance() // ON
		isUpdate := isKeyword(p.advance(), "UPDATE")
		act, actEnd, err := p.parseForeignKeyAction()
		if err != nil {
			return nil, 0, err
		}
		if isUpdate {
			actions.UpdateAction = act
		} else {
			actions.DeleteAction = act
		}
		if start < 0 {
			start = onTok.Pos
		}
		end = actEnd
	}
	if start < 0 {
		actions.Span = span(emptyPos, emptyPos)
	} else {
		actions.Span = span(start, end)
	}
	return actions, end, nil
}

// parseForeignKeyAction parses foreign_key_action: NO ACTION, RESTRICT,
// CASCADE, or SET NULL; see foreign_key_action in googlesql.tm. It returns the
// canonical action string and the offset just past the clause.
func (p *parser) parseForeignKeyAction() (string, int, error) {
	tok := p.peek()
	switch {
	case isKeyword(tok, "NO"):
		p.advance() // NO
		action, err := p.expectKeyword("ACTION")
		if err != nil {
			return "", 0, err
		}
		return "NO ACTION", action.End, nil
	case isKeyword(tok, "RESTRICT"):
		return "RESTRICT", p.advance().End, nil
	case isKeyword(tok, "CASCADE"):
		return "CASCADE", p.advance().End, nil
	case isKeyword(tok, "SET"):
		p.advance() // SET
		null, err := p.expectKeyword("NULL")
		if err != nil {
			return "", 0, err
		}
		return "SET NULL", null.End, nil
	}
	return "", 0, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
}

// parseColumnListParen parses a parenthesized column_list, requiring the
// opening parenthesis to be the next token; see column_list in googlesql.tm.
func (p *parser) parseColumnListParen() (*ast.ColumnList, error) {
	if p.peek().Kind != token.LPAREN {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected \"(\" but got %s", describeToken(p.peek()))
	}
	return p.parseInsertColumnList()
}

// tryParseConstraintEnforcement parses an optional "ENFORCED" or "NOT ENFORCED"
// clause; see constraint_enforcement in googlesql.tm. The third return value
// reports whether a clause was present; the second is the offset just past it.
func (p *parser) tryParseConstraintEnforcement() (bool, int, bool, error) {
	if isKeyword(p.peek(), "ENFORCED") {
		return true, p.advance().End, true, nil
	}
	if isKeyword(p.peek(), "NOT") && isKeyword(p.peekAt(1), "ENFORCED") {
		p.advance() // NOT
		return false, p.advance().End, true, nil
	}
	return true, 0, false, nil
}

// parseCreateViewStatement parses the tail of "CREATE [OR REPLACE] [scope]
// [MATERIALIZED|APPROX] [RECURSIVE] VIEW [IF NOT EXISTS] name
// [(column_with_options_list)] [SQL SECURITY {INVOKER|DEFINER}] [OPTIONS(...)]
// AS query"; see create_view_statement in googlesql.tm. viewKind is "",
// "MATERIALIZED", or "APPROX". The RECURSIVE keyword and VIEW keyword are not
// yet consumed.
func (p *parser) parseCreateViewStatement(createTok token.Token, scope string, isOrReplace bool, viewKind string) (ast.Statement, error) {
	stmt := &ast.CreateViewStatement{Span: span(createTok.Pos, 0), ViewKind: viewKind, Scope: scope, IsOrReplace: isOrReplace}
	if isKeyword(p.peek(), "RECURSIVE") {
		p.advance()
		stmt.Recursive = true
	}
	if _, err := p.expectKeyword("VIEW"); err != nil {
		return nil, err
	}
	if isKeyword(p.peek(), "IF") && isKeyword(p.peekAt(1), "NOT") && isKeyword(p.peekAt(2), "EXISTS") {
		p.advance()
		p.advance()
		p.advance()
		stmt.IsIfNotExists = true
	}
	name, err := p.parseMaybeDashedPathExpression()
	if err != nil {
		return nil, err
	}
	stmt.Name = name
	stmt.Stop = name.End()
	// column_with_options_list?
	if p.peek().Kind == token.LPAREN {
		cols, err := p.parseColumnWithOptionsList()
		if err != nil {
			return nil, err
		}
		stmt.Columns = cols
		stmt.Stop = cols.End()
	}
	// sql_security?
	if isKeyword(p.peek(), "SQL") {
		p.advance() // SQL
		if _, err := p.expectKeyword("SECURITY"); err != nil {
			return nil, err
		}
		switch {
		case isKeyword(p.peek(), "INVOKER"):
			p.advance()
			stmt.SqlSecurity = "INVOKER"
		case isKeyword(p.peek(), "DEFINER"):
			p.advance()
			stmt.SqlSecurity = "DEFINER"
		default:
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword DEFINER or keyword INVOKER but got %s", describeToken(p.peek()))
		}
	}
	// opt_partition_by_clause_no_hint and cluster_by_clause_no_hint apply only
	// to CREATE MATERIALIZED VIEW; see create_view_statement in googlesql.tm.
	if viewKind == "MATERIALIZED" {
		if isKeyword(p.peek(), "PARTITION") {
			pb, err := p.parsePartitionByNoHint()
			if err != nil {
				return nil, err
			}
			stmt.PartitionBy = pb
			stmt.Stop = pb.End()
		}
		if isKeyword(p.peek(), "CLUSTER") {
			cb, err := p.parseClusterBy()
			if err != nil {
				return nil, err
			}
			stmt.ClusterBy = cb
			stmt.Stop = cb.End()
		}
	}
	// options?
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		stmt.Options = opts
		stmt.Stop = opts.End()
	}
	if _, err := p.expectKeyword("AS"); err != nil {
		return nil, err
	}
	// A materialized view's body is either a query or "REPLICA OF <path>"; see
	// create_view_statement in googlesql.tm. Other view kinds take only a query.
	if viewKind == "MATERIALIZED" && isKeyword(p.peek(), "REPLICA") {
		p.advance() // REPLICA
		if _, err := p.expectKeyword("OF"); err != nil {
			return nil, err
		}
		src, err := p.parseMaybeDashedPathExpression()
		if err != nil {
			return nil, err
		}
		stmt.ReplicaSource = src
		stmt.Stop = src.End()
		return stmt, nil
	}
	queryStart := p.peek().Pos
	query, err := p.parseQueryAfterAs()
	if err != nil {
		return nil, err
	}
	if hasLockMode(query) {
		return nil, p.errorf(queryStart, "Syntax error: Unexpected lock mode in query")
	}
	stmt.Query = query
	stmt.Stop = p.prevEnd()
	return stmt, nil
}

// parseCreateModelStatement parses the tail of "CREATE [OR REPLACE] [scope]
// MODEL [IF NOT EXISTS] name [INPUT(...) OUTPUT(...)] [TRANSFORM(...)]
// [REMOTE] [WITH CONNECTION ...] [OPTIONS(...)] [AS query | AS
// (aliased_query_list)]"; see create_model_statement in googlesql.tm. MODEL is
// the next token.
// parseCreateConstantStatement parses the tail of "CREATE [OR REPLACE]
// [scope] CONSTANT [IF NOT EXISTS] <path> = <expression>"; see
// create_constant_statement in googlesql.tm. The CONSTANT keyword is the next
// token.
func (p *parser) parseCreateConstantStatement(createTok token.Token, scope string, isOrReplace bool) (ast.Statement, error) {
	p.advance() // CONSTANT
	stmt := &ast.CreateConstantStatement{Span: span(createTok.Pos, 0), Scope: scope, IsOrReplace: isOrReplace}
	if isKeyword(p.peek(), "IF") && isKeyword(p.peekAt(1), "NOT") && isKeyword(p.peekAt(2), "EXISTS") {
		p.advance()
		p.advance()
		p.advance()
		stmt.IsIfNotExists = true
	}
	// A leading token that cannot begin a path_expression identifier is
	// reported as "Unexpected" rather than "Expected identifier".
	if tok := p.peek(); tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	name, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	stmt.Name = name
	// The path_expression is followed by "="; a different token yields the
	// combined "." or "=" diagnostic because a path may still continue on ".".
	if p.peek().Kind != token.EQ {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "." or "=" but got %s`, describeToken(p.peek()))
	}
	p.advance() // =
	value, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	stmt.Value = value
	stmt.Stop = p.extEnd(value)
	return stmt, nil
}

func (p *parser) parseCreateModelStatement(createTok token.Token, scope string, isOrReplace bool) (ast.Statement, error) {
	p.advance() // MODEL
	stmt := &ast.CreateModelStatement{Span: span(createTok.Pos, 0), Scope: scope, IsOrReplace: isOrReplace}
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

	// input_output_clause?: "INPUT (columns) OUTPUT (columns)".
	if isKeyword(p.peek(), "INPUT") {
		inputTok := p.advance() // INPUT
		if p.peek().Kind != token.LPAREN {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected \"(\" but got %s", describeToken(p.peek()))
		}
		input, err := p.parseTableElementList()
		if err != nil {
			return nil, err
		}
		if tableElementListHasConstraints(input) {
			return nil, p.errorf(input.Pos(), "Syntax error: Element list contains unexpected constraint")
		}
		if _, err := p.expectKeyword("OUTPUT"); err != nil {
			return nil, err
		}
		if p.peek().Kind != token.LPAREN {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected \"(\" but got %s", describeToken(p.peek()))
		}
		output, err := p.parseTableElementList()
		if err != nil {
			return nil, err
		}
		if tableElementListHasConstraints(output) {
			return nil, p.errorf(output.Pos(), "Syntax error: Element list contains unexpected constraint")
		}
		stmt.InputOutput = &ast.InputOutputClause{Span: span(inputTok.Pos, output.End()), Input: input, Output: output}
		stmt.Stop = stmt.InputOutput.End()
	}

	// transform_clause?: "TRANSFORM ( select_list )".
	if isKeyword(p.peek(), "TRANSFORM") {
		transformTok := p.advance() // TRANSFORM
		if _, err := p.expect(token.LPAREN, `"("`); err != nil {
			return nil, err
		}
		sl, err := p.parseSelectList()
		if err != nil {
			return nil, err
		}
		rparen, err := p.expect(token.RPAREN, `")"`)
		if err != nil {
			return nil, err
		}
		stmt.Transform = &ast.TransformClause{Span: span(transformTok.Pos, rparen.End), SelectList: sl}
		stmt.Stop = stmt.Transform.End()
	}

	// remote_with_connection_clause?: "REMOTE [WITH CONNECTION ...]" or
	// "WITH CONNECTION ...".
	if isKeyword(p.peek(), "REMOTE") {
		if !p.features.Enabled(FeatureRemoteFunction) {
			return nil, p.errorf(p.peek().Pos, "Keyword REMOTE is not supported")
		}
		stmt.IsRemote = true
		stmt.Stop = p.advance().End
		if isKeyword(p.peek(), "WITH") && isKeyword(p.peekAt(1), "CONNECTION") {
			wc, err := p.parseWithConnectionClause()
			if err != nil {
				return nil, err
			}
			stmt.WithConnection = wc
			stmt.Stop = wc.End()
		}
	} else if isKeyword(p.peek(), "WITH") && isKeyword(p.peekAt(1), "CONNECTION") {
		if !p.features.Enabled(FeatureRemoteFunction) && !p.features.Enabled(FeatureCreateFunctionLanguageWithConnection) {
			return nil, p.errorf(p.peek().Pos, "WITH CONNECTION clause is not supported")
		}
		wc, err := p.parseWithConnectionClause()
		if err != nil {
			return nil, err
		}
		stmt.WithConnection = wc
		stmt.Stop = wc.End()
	}

	// options?
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		stmt.Options = opts
		stmt.Stop = opts.End()
	}

	// as_query_or_aliased_query_list?: "AS query" or "AS (aliased_query_list)".
	if isKeyword(p.peek(), "AS") {
		p.advance() // AS
		// "( identifier AS ..." begins an aliased query list; anything else is a
		// plain (possibly parenthesized) query. A query can never start with an
		// identifier, so this lookahead is unambiguous.
		if p.peek().Kind == token.LPAREN && isAliasedQueryStart(p.peekAt(1), p.peekAt(2)) {
			list, err := p.parseAliasedQueryList()
			if err != nil {
				return nil, err
			}
			stmt.AliasedQueries = list
			stmt.Stop = p.prevEnd()
		} else {
			queryStart := p.peek().Pos
			query, err := p.parseQueryAfterAs()
			if err != nil {
				return nil, err
			}
			if hasLockMode(query) {
				return nil, p.errorf(queryStart, "Syntax error: Unexpected lock mode in query")
			}
			stmt.Query = query
			stmt.Stop = p.prevEnd()
		}
	}
	return stmt, nil
}

// tableElementListHasConstraints reports whether the list holds any table
// constraint element (as opposed to a plain column definition); see
// ASTTableElementList::HasConstraints in parse_tree.cc.
func tableElementListHasConstraints(list *ast.TableElementList) bool {
	for _, elem := range list.Elements {
		if _, ok := elem.(*ast.ColumnDefinition); !ok {
			return true
		}
	}
	return false
}

// isAliasedQueryStart reports whether the two tokens after an opening "(" begin
// an aliased query ("identifier AS ..."); see aliased_query in googlesql.tm.
func isAliasedQueryStart(first, second token.Token) bool {
	if first.Kind != token.IDENT && first.Kind != token.QUOTED_IDENT {
		return false
	}
	if isReservedStatic(first) {
		return false
	}
	return isKeyword(second, "AS")
}

// parseAliasedQueryList parses "( aliased_query [, aliased_query]... )"; see
// aliased_query_list in googlesql.tm. The opening parenthesis is the next
// token. The list's location spans the aliased queries, excluding the
// surrounding parentheses.
func (p *parser) parseAliasedQueryList() (*ast.AliasedQueryList, error) {
	p.advance() // (
	list := &ast.AliasedQueryList{}
	for {
		aq, err := p.parseAliasedQuery()
		if err != nil {
			return nil, err
		}
		if len(list.Queries) == 0 {
			list.Start = aq.Pos()
		}
		list.Queries = append(list.Queries, aq)
		list.Stop = aq.End()
		if p.peek().Kind == token.COMMA {
			p.advance()
			continue
		}
		break
	}
	if _, err := p.expect(token.RPAREN, `")" or ","`); err != nil {
		return nil, err
	}
	return list, nil
}

// parseAliasedQuery parses "identifier AS ( query )"; see aliased_query in
// googlesql.tm. The aliased query's location includes the parentheses.
func (p *parser) parseAliasedQuery() (*ast.AliasedQuery, error) {
	tok := p.peek()
	if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	if p.isReserved(tok) {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected keyword %s", strings.ToUpper(tok.Image))
	}
	ident := p.parseIdentifierToken(p.advance())
	if _, err := p.expectKeyword("AS"); err != nil {
		return nil, err
	}
	lparen := p.peek()
	if lparen.Kind != token.LPAREN {
		return nil, p.errorf(lparen.Pos, "Syntax error: Expected \"(\" but got %s", describeToken(lparen))
	}
	query, parenEnd, err := p.parseParenthesizedQuery()
	if err != nil {
		return nil, err
	}
	query.Start, query.Stop = lparen.Pos, parenEnd
	return &ast.AliasedQuery{Span: span(ident.Pos(), query.End()), Identifier: ident, Query: query}, nil
}

// parseColumnWithOptionsList parses "(" identifier [OPTIONS(...)] {","
// identifier [OPTIONS(...)]} ")"; see column_with_options_list in
// googlesql.tm. The next token is "(".
func (p *parser) parseColumnWithOptionsList() (*ast.ColumnWithOptionsList, error) {
	lparen := p.advance() // (
	list := &ast.ColumnWithOptionsList{Span: span(lparen.Pos, 0)}
	for {
		tok := p.peek()
		if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
			return nil, p.errorf(tok.Pos, "Syntax error: Expected identifier but got %s", describeToken(tok))
		}
		nm := p.parseIdentifierToken(p.advance())
		col := &ast.ColumnWithOptions{Span: span(nm.Pos(), nm.End()), Name: nm}
		if isKeyword(p.peek(), "OPTIONS") {
			p.advance() // OPTIONS
			opts, err := p.parseOptionsList()
			if err != nil {
				return nil, err
			}
			col.Options = opts
			col.Stop = opts.End()
		}
		list.Columns = append(list.Columns, col)
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
	return list, nil
}

// parseCreateIndexStatement parses the tail of "CREATE [OR REPLACE] [UNIQUE]
// [SEARCH|VECTOR] INDEX [IF NOT EXISTS] name ON table [AS alias]
// [unnest_list] (index_items) [STORING (...)] [PARTITION BY ...] [OPTIONS(...)]";
// see create_index_statement in googlesql.tm. The next token is UNIQUE,
// SEARCH, VECTOR, or INDEX.
func (p *parser) parseCreateIndexStatement(createTok token.Token, isOrReplace bool) (ast.Statement, error) {
	stmt := &ast.CreateIndexStatement{Span: span(createTok.Pos, 0), IsOrReplace: isOrReplace}
	if isKeyword(p.peek(), "UNIQUE") {
		p.advance()
		stmt.IsUnique = true
	}
	// opt_spanner_null_filtered: "NULL_FILTERED"; see spanner_null_filtered in
	// googlesql.tm. Requires FEATURE_SPANNER_LEGACY_DDL; otherwise the keyword
	// is reported as an unsupported object type.
	if isKeyword(p.peek(), "NULL_FILTERED") {
		nfTok := p.advance()
		if !p.features.Enabled(FeatureSpannerLegacyDDL) {
			return nil, p.errorf(nfTok.Pos, "null_filtered is not a supported object type")
		}
		stmt.IsNullFiltered = true
	}
	// opt_index_type: at most one of SEARCH or VECTOR; see index_type in
	// googlesql.tm. After it, INDEX is required (so "SEARCH VECTOR" reports the
	// INDEX-expected error at VECTOR).
	if isKeyword(p.peek(), "SEARCH") {
		p.advance()
		stmt.IsSearch = true
	} else if isKeyword(p.peek(), "VECTOR") {
		p.advance()
		stmt.IsVector = true
	}
	if _, err := p.expectKeyword("INDEX"); err != nil {
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
	if _, err := p.expectKeyword("ON"); err != nil {
		return nil, err
	}
	tableName, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	stmt.TableName = tableName
	stmt.Stop = tableName.End()
	// Optional table alias (as_alias: "AS"? identifier).
	alias, err := p.parseOptionalAlias()
	if err != nil {
		return nil, err
	}
	if alias != nil {
		stmt.Alias = alias
		stmt.Stop = alias.End()
	}
	// Optional list of UNNEST expressions.
	if isKeyword(p.peek(), "UNNEST") {
		list := &ast.IndexUnnestExpressionList{}
		start := p.peek().Pos
		for isKeyword(p.peek(), "UNNEST") {
			uexpr, err := p.parseUnnestExpression()
			if err != nil {
				return nil, err
			}
			item := &ast.UnnestExpressionWithOptAliasAndOffset{Span: span(uexpr.Pos(), uexpr.End()), Expression: uexpr}
			ualias, err := p.parseOptionalAlias()
			if err != nil {
				return nil, err
			}
			if ualias != nil {
				item.Alias = ualias
				item.Stop = ualias.End()
			}
			if isKeyword(p.peek(), "WITH") {
				offset, err := p.parseWithOffsetClause()
				if err != nil {
					return nil, err
				}
				item.WithOffset = offset
				item.Stop = offset.End()
			}
			list.UnnestExpressions = append(list.UnnestExpressions, item)
			list.Stop = item.End()
		}
		list.Start = start
		stmt.UnnestExpressionList = list
		stmt.Stop = list.End()
	}
	// Required index item list (index_order_by_and_options).
	itemList, err := p.parseIndexOrderByAndOptions()
	if err != nil {
		return nil, err
	}
	stmt.IndexItemList = itemList
	stmt.Stop = itemList.End()
	// Optional STORING clause.
	if isKeyword(p.peek(), "STORING") {
		storing, err := p.parseIndexStoringList()
		if err != nil {
			return nil, err
		}
		stmt.Storing = storing
		stmt.Stop = storing.End()
	}
	// Optional suffix: PARTITION BY [OPTIONS] or OPTIONS.
	if isKeyword(p.peek(), "PARTITION") {
		pb, err := p.parseIndexPartitionBy()
		if err != nil {
			return nil, err
		}
		stmt.PartitionBy = pb
		stmt.Stop = pb.End()
		if isKeyword(p.peek(), "OPTIONS") {
			p.advance()
			opts, err := p.parseOptionsList()
			if err != nil {
				return nil, err
			}
			stmt.Options = opts
			stmt.Stop = opts.End()
		}
	} else if isKeyword(p.peek(), "OPTIONS") {
		p.advance()
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		stmt.Options = opts
		stmt.Stop = opts.End()
	}
	// spanner_index_interleave_clause: ", INTERLEAVE IN path"; see
	// create_index_statement_suffix in googlesql.tm. Requires
	// FEATURE_SPANNER_LEGACY_DDL; without it the trailing "," falls through to
	// the "Expected end of input" error.
	if p.features.Enabled(FeatureSpannerLegacyDDL) && stmt.PartitionBy == nil &&
		p.peek().Kind == token.COMMA && isKeyword(p.peekAt(1), "INTERLEAVE") &&
		isKeyword(p.peekAt(2), "IN") {
		commaTok := p.advance() // ,
		p.advance()             // INTERLEAVE
		p.advance()             // IN
		path, err := p.parseMaybeDashedPathExpression()
		if err != nil {
			return nil, err
		}
		stmt.SpannerInterleave = &ast.SpannerInterleaveClause{Span: span(commaTok.Pos, path.End()), TableName: path, Type: "IN"}
		stmt.Stop = path.End()
	}
	return stmt, nil
}

// parseIndexOrderByAndOptions parses "( ALL COLUMNS [WITH COLUMN OPTIONS
// (...)] )" or "( column_ordering_and_options_expr [, ...] )"; see
// index_order_by_and_options and index_all_columns in googlesql.tm.
func (p *parser) parseIndexOrderByAndOptions() (*ast.IndexItemList, error) {
	lparen, err := p.expect(token.LPAREN, `"("`)
	if err != nil {
		return nil, err
	}
	if isKeyword(p.peek(), "ALL") {
		p.advance() // ALL
		if _, err := p.expectKeyword("COLUMNS"); err != nil {
			return nil, err
		}
		var colOpts *ast.IndexItemList
		withConsumed := false
		if isKeyword(p.peek(), "WITH") {
			p.advance() // WITH
			if _, err := p.expectKeyword("COLUMN"); err != nil {
				return nil, err
			}
			if _, err := p.expectKeyword("OPTIONS"); err != nil {
				return nil, err
			}
			colOpts, err = p.parseIndexColumnList(false)
			if err != nil {
				return nil, err
			}
			withConsumed = true
		}
		if p.peek().Kind != token.RPAREN {
			if withConsumed {
				return nil, p.errorf(p.peek().Pos, `Syntax error: Expected ")" but got %s`, describeToken(p.peek()))
			}
			return nil, p.errorf(p.peek().Pos, `Syntax error: Expected ")" or keyword WITH but got %s`, describeToken(p.peek()))
		}
		rparen := p.advance()
		allCols := &ast.IndexAllColumns{Span: span(lparen.Pos, rparen.End), Image: "ALL COLUMNS", ColumnOptions: colOpts}
		oe := &ast.OrderingExpression{Span: span(lparen.Pos, rparen.End), Expr: allCols}
		return &ast.IndexItemList{Span: span(lparen.Pos, rparen.End), OrderingExpressions: []*ast.OrderingExpression{oe}}, nil
	}
	return p.parseIndexColumnListBody(lparen, true)
}

// parseIndexColumnList parses "( column_ordering_and_options_expr [, ...] )".
// If extendToClose is true the returned list's span includes the closing
// parenthesis (index_order_by_and_options); otherwise it ends at the last
// column (all_column_column_options, used by WITH COLUMN OPTIONS).
func (p *parser) parseIndexColumnList(extendToClose bool) (*ast.IndexItemList, error) {
	lparen, err := p.expect(token.LPAREN, `"("`)
	if err != nil {
		return nil, err
	}
	return p.parseIndexColumnListBody(lparen, extendToClose)
}

func (p *parser) parseIndexColumnListBody(lparen token.Token, extendToClose bool) (*ast.IndexItemList, error) {
	if p.peek().Kind == token.RPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Unexpected ")"`)
	}
	list := &ast.IndexItemList{Span: span(lparen.Pos, 0)}
	for {
		oe, err := p.parseIndexOrderingExpression()
		if err != nil {
			return nil, err
		}
		list.OrderingExpressions = append(list.OrderingExpressions, oe)
		list.Stop = oe.End()
		if p.peek().Kind == token.COMMA {
			p.advance()
			continue
		}
		break
	}
	if p.peek().Kind != token.RPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected ")" or "," but got %s`, describeToken(p.peek()))
	}
	rparen := p.advance()
	if extendToClose {
		list.Stop = rparen.End
	}
	return list, nil
}

// parseIndexOrderingExpression parses "expression [COLLATE ...] [ASC|DESC]
// [NULLS FIRST|LAST] [OPTIONS(...)]"; see column_ordering_and_options_expr in
// googlesql.tm.
func (p *parser) parseIndexOrderingExpression() (*ast.OrderingExpression, error) {
	oe, err := p.parseOrderingExpression()
	if err != nil {
		return nil, err
	}
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		oe.Options = opts
		oe.Stop = opts.End()
	}
	return oe, nil
}

// parseIndexStoringList parses "STORING ( expression [, ...] )"; see
// index_storing_list in googlesql.tm. The STORING keyword is the next token.
// The node span starts at the opening parenthesis.
func (p *parser) parseIndexStoringList() (*ast.IndexStoringExpressionList, error) {
	p.advance() // STORING
	lparen, err := p.expect(token.LPAREN, `"("`)
	if err != nil {
		return nil, err
	}
	if p.peek().Kind == token.RPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Unexpected ")"`)
	}
	list := &ast.IndexStoringExpressionList{Span: span(lparen.Pos, 0)}
	for {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		list.Expressions = append(list.Expressions, expr)
		if p.peek().Kind == token.COMMA {
			p.advance()
			continue
		}
		break
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	list.Stop = rparen.End
	return list, nil
}

// parseIndexPartitionBy parses "PARTITION BY expression [, ...]" with no hint;
// see partition_by_clause_prefix_no_hint in googlesql.tm. The PARTITION keyword
// is the next token.
func (p *parser) parseIndexPartitionBy() (*ast.PartitionBy, error) {
	partTok := p.advance() // PARTITION
	if _, err := p.expectKeyword("BY"); err != nil {
		return nil, err
	}
	pb := &ast.PartitionBy{Span: span(partTok.Pos, 0)}
	for {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		pb.Expressions = append(pb.Expressions, expr)
		pb.Stop = p.extEnd(expr)
		if p.peek().Kind == token.COMMA {
			p.advance()
			continue
		}
		break
	}
	return pb, nil
}

// prevEnd returns the end offset of the most recently consumed token.
func (p *parser) prevEnd() int { return p.toks[p.pos-1].End }

// parseCreateTableFunctionStatement parses the tail of "CREATE [OR REPLACE]
// [scope] TABLE FUNCTION [IF NOT EXISTS] path(params) [RETURNS TABLE<...>]
// [SQL SECURITY {INVOKER|DEFINER}] [<language>|<options>...] [AS query]"; see
// create_table_function_statement in googlesql.tm. The FUNCTION keyword is the
// next token. The debug tree lists children in the fixed grammar order
// (declaration, returns, options, language, body) regardless of whether the
// source used LANGUAGE before OPTIONS or vice versa.
func (p *parser) parseCreateTableFunctionStatement(createTok token.Token, scope string, isOrReplace bool) (ast.Statement, error) {
	p.advance() // FUNCTION
	stmt := &ast.CreateTableFunctionStatement{Span: span(createTok.Pos, 0), Scope: scope, IsOrReplace: isOrReplace}
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
	params, err := p.parseFunctionParameters()
	if err != nil {
		return nil, err
	}
	stmt.Declaration = &ast.FunctionDeclaration{
		Span:       span(name.Pos(), params.End()),
		Name:       name,
		Parameters: params,
	}
	stmt.Stop = stmt.Declaration.End()

	// RETURNS type_or_tvf_schema. For a table function this must be a TVF
	// schema; a plain type is diagnosed as "Expected keyword TABLE".
	if isKeyword(p.peek(), "RETURNS") {
		p.advance()
		if !isKeyword(p.peek(), "TABLE") {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword TABLE but got %s", describeToken(p.peek()))
		}
		schema, err := p.parseTVFSchema()
		if err != nil {
			return nil, err
		}
		stmt.Returns = schema
		stmt.Stop = schema.End()
	}

	// SQL SECURITY {INVOKER|DEFINER}. Parsed but not shown in the debug tree.
	if isKeyword(p.peek(), "SQL") && isKeyword(p.peekAt(1), "SECURITY") {
		p.advance() // SQL
		p.advance() // SECURITY
		switch {
		case isKeyword(p.peek(), "INVOKER"):
			stmt.SqlSecurity = "INVOKER"
		case isKeyword(p.peek(), "DEFINER"):
			stmt.SqlSecurity = "DEFINER"
		default:
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword DEFINER or keyword INVOKER but got %s", describeToken(p.peek()))
		}
		sec := p.advance()
		stmt.Stop = sec.End
	}

	// unordered_language_options: at most one LANGUAGE and one OPTIONS clause,
	// in either order. A second OPTIONS (or LANGUAGE) is left unconsumed so the
	// top-level "Expected end of input" check reports it.
	for {
		if isKeyword(p.peek(), "LANGUAGE") && stmt.Language == nil {
			p.advance() // LANGUAGE
			tok := p.peek()
			if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
				return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
			}
			stmt.Language = p.parseIdentifierToken(p.advance())
			stmt.Stop = stmt.Language.End()
			continue
		}
		if isKeyword(p.peek(), "OPTIONS") && stmt.Options == nil {
			p.advance() // OPTIONS
			opts, err := p.parseOptionsList()
			if err != nil {
				return nil, err
			}
			stmt.Options = opts
			stmt.Stop = opts.End()
			continue
		}
		break
	}

	// AS query (the string-literal body form is not exercised here).
	if isKeyword(p.peek(), "AS") {
		p.advance()
		queryStart := p.peek().Pos
		query, err := p.parseQuery()
		if err != nil {
			return nil, err
		}
		if hasLockMode(query) {
			return nil, p.errorf(queryStart, "Syntax error: Unexpected lock mode in query")
		}
		stmt.Query = query
		stmt.Stop = p.prevEnd()
	}
	return stmt, nil
}

// parseCreateFunctionStatement parses the tail of "CREATE [OR REPLACE] [scope]
// [AGGREGATE] FUNCTION [IF NOT EXISTS] path(params) [RETURNS type]
// [determinism] [SQL SECURITY ...] [REMOTE] [LANGUAGE id] [WITH CONNECTION ...]
// [OPTIONS ...] [AS body]"; see create_function_statement in googlesql.tm. The
// FUNCTION keyword is the next token. The trailing clauses may appear in any
// order (matching the reference's flexible grammar); the debug tree lists
// children in a fixed order regardless.
func (p *parser) parseCreateFunctionStatement(createTok token.Token, scope string, isOrReplace, isAggregate bool) (ast.Statement, error) {
	p.advance() // FUNCTION
	stmt := &ast.CreateFunctionStatement{Span: span(createTok.Pos, 0), Scope: scope, IsOrReplace: isOrReplace, IsAggregate: isAggregate}
	if isKeyword(p.peek(), "IF") && isKeyword(p.peekAt(1), "NOT") && isKeyword(p.peekAt(2), "EXISTS") {
		p.advance()
		p.advance()
		p.advance()
		stmt.IsIfNotExists = true
	}
	// A missing function name reports a generic "Unexpected" error rather than
	// "Expected identifier"; see create_function_statement in googlesql.tm.
	if tok := p.peek(); tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	name, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	// After the path, the parameter list must open with "("; the path could
	// also have continued with ".", so the error mentions both.
	if p.peek().Kind != token.LPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" or "." but got %s`, describeToken(p.peek()))
	}
	params, err := p.parseFunctionParameters()
	if err != nil {
		return nil, err
	}
	stmt.Declaration = &ast.FunctionDeclaration{
		Span:       span(name.Pos(), params.End()),
		Name:       name,
		Parameters: params,
	}
	stmt.Stop = stmt.Declaration.End()

	// RETURNS type. Templated types (ANY ...) are rejected here.
	if isKeyword(p.peek(), "RETURNS") {
		p.advance()
		if isKeyword(p.peek(), "ANY") {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Templated types are not allowed in the RETURNS clause")
		}
		typ, err := p.parseType()
		if err != nil {
			return nil, err
		}
		stmt.ReturnType = typ
		stmt.Stop = typ.End()
	}

	// Trailing clauses, accepted in any order (each at most once).
	for {
		switch {
		case stmt.Determinism == "" && isKeyword(p.peek(), "DETERMINISTIC"):
			stmt.Determinism = "DETERMINISTIC"
			stmt.Stop = p.advance().End
		case stmt.Determinism == "" && isKeyword(p.peek(), "NOT") && isKeyword(p.peekAt(1), "DETERMINISTIC"):
			p.advance()
			stmt.Determinism = "NOT DETERMINISTIC"
			stmt.Stop = p.advance().End
		case stmt.Determinism == "" && isKeyword(p.peek(), "IMMUTABLE"):
			stmt.Determinism = "IMMUTABLE"
			stmt.Stop = p.advance().End
		case stmt.Determinism == "" && isKeyword(p.peek(), "STABLE"):
			stmt.Determinism = "STABLE"
			stmt.Stop = p.advance().End
		case stmt.Determinism == "" && isKeyword(p.peek(), "VOLATILE"):
			stmt.Determinism = "VOLATILE"
			stmt.Stop = p.advance().End
		case stmt.SqlSecurity == "" && isKeyword(p.peek(), "SQL") && isKeyword(p.peekAt(1), "SECURITY"):
			p.advance() // SQL
			p.advance() // SECURITY
			switch {
			case isKeyword(p.peek(), "INVOKER"):
				stmt.SqlSecurity = "INVOKER"
			case isKeyword(p.peek(), "DEFINER"):
				stmt.SqlSecurity = "DEFINER"
			default:
				return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword DEFINER or keyword INVOKER but got %s", describeToken(p.peek()))
			}
			stmt.Stop = p.advance().End
		case !stmt.IsRemote && isKeyword(p.peek(), "REMOTE"):
			if !p.features.Enabled(FeatureRemoteFunction) {
				return nil, p.errorf(p.peek().Pos, "Keyword REMOTE is not supported")
			}
			stmt.IsRemote = true
			stmt.Stop = p.advance().End
		case stmt.Language == nil && isKeyword(p.peek(), "LANGUAGE"):
			p.advance() // LANGUAGE
			tok := p.peek()
			if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
				return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
			}
			stmt.Language = p.parseIdentifierToken(p.advance())
			stmt.Stop = stmt.Language.End()
		case stmt.WithConnection == nil && isKeyword(p.peek(), "WITH") && isKeyword(p.peekAt(1), "CONNECTION"):
			if !p.features.Enabled(FeatureRemoteFunction) && !p.features.Enabled(FeatureCreateFunctionLanguageWithConnection) {
				return nil, p.errorf(p.peek().Pos, "WITH CONNECTION clause is not supported")
			}
			wc, err := p.parseWithConnectionClause()
			if err != nil {
				return nil, err
			}
			stmt.WithConnection = wc
			stmt.Stop = wc.End()
		case stmt.Options == nil && isKeyword(p.peek(), "OPTIONS"):
			p.advance() // OPTIONS
			opts, err := p.parseOptionsList()
			if err != nil {
				return nil, err
			}
			stmt.Options = opts
			stmt.Stop = opts.End()
		case stmt.Body == nil && isKeyword(p.peek(), "AS"):
			p.advance() // AS
			body, err := p.parseSqlFunctionBodyOrString()
			if err != nil {
				return nil, err
			}
			stmt.Body = body
			stmt.Stop = body.End()
		default:
			return stmt, nil
		}
	}
}

// parseWithConnectionClause parses "WITH CONNECTION <connection>"; see
// opt_with_connection_clause in googlesql.tm. WITH is the next token.
func (p *parser) parseWithConnectionClause() (*ast.WithConnectionClause, error) {
	withTok := p.advance() // WITH
	connTok, err := p.expectKeyword("CONNECTION")
	if err != nil {
		return nil, err
	}
	var path ast.Node
	switch tok := p.peek(); {
	case isKeyword(tok, "DEFAULT"):
		p.advance()
		path = &ast.DefaultLiteral{Span: span(tok.Pos, tok.End)}
	case (tok.Kind == token.IDENT || tok.Kind == token.QUOTED_IDENT) && !p.isReserved(tok):
		pathExpr, err := p.parsePathExpression()
		if err != nil {
			return nil, err
		}
		path = pathExpr
	default:
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	conn := &ast.ConnectionClause{Span: span(connTok.Pos, path.End()), Path: path}
	return &ast.WithConnectionClause{Span: span(withTok.Pos, conn.End()), Connection: conn}, nil
}

// parseSqlFunctionBodyOrString parses a function body following AS: either a
// parenthesized SQL expression (SqlFunctionBody) or a string literal; see
// as_sql_function_body_or_string in googlesql.tm.
func (p *parser) parseSqlFunctionBodyOrString() (ast.Node, error) {
	if p.peek().Kind == token.LPAREN {
		lparen := p.advance() // (
		// The grammar's sql_function_body rule special-cases "(" "SELECT" to
		// reject a bare query body (which would otherwise parse as a scalar
		// subquery expression), directing the user to wrap it in an extra pair
		// of parentheses. The message deliberately omits the "Syntax error:"
		// prefix, matching MakeSyntaxError in googlesql.tm.
		if isKeyword(p.peek(), "SELECT") {
			return nil, p.errorf(p.peek().Pos, "The body of each CREATE FUNCTION statement is an expression, not a query; to use a query as an expression, the query must be wrapped with additional parentheses to make it a scalar subquery expression")
		}
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		rparen, err := p.expect(token.RPAREN, `")"`)
		if err != nil {
			return nil, err
		}
		body := &ast.SqlFunctionBody{Span: span(lparen.Pos, rparen.End), Expression: expr}
		// Queries defined in function bodies must not have lock modes, to avoid
		// unknowingly acquiring locks when executing them; see
		// as_sql_function_body_or_string in googlesql.tm.
		if hasLockMode(body) {
			return nil, p.errorf(lparen.Pos, "Syntax error: Unexpected lock mode in function body query")
		}
		return body, nil
	}
	return p.parseStringLiteralValue()
}

// parseFunctionParameters parses "( [function_parameter [, ...]] )"; see
// function_parameters in googlesql.tm. The opening parenthesis is the next
// token.
func (p *parser) parseFunctionParameters() (*ast.FunctionParameters, error) {
	lparen, err := p.expect(token.LPAREN, `"("`)
	if err != nil {
		return nil, err
	}
	fp := &ast.FunctionParameters{Span: span(lparen.Pos, 0)}
	if p.beginsFunctionParameter(p.peek()) {
		for {
			param, err := p.parseFunctionParameter()
			if err != nil {
				return nil, err
			}
			fp.Parameters = append(fp.Parameters, param)
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
	fp.Stop = rparen.End
	return fp, nil
}

// beginsFunctionParameter reports whether tok can begin a function parameter:
// a parameter name (identifier) or a type. The reserved keyword DEFAULT (used
// for default-argument syntax) cannot; a leading DEFAULT is reported as a
// missing ")".
func (p *parser) beginsFunctionParameter(tok token.Token) bool {
	if isKeyword(tok, "DEFAULT") {
		return false
	}
	return tok.Kind == token.IDENT || tok.Kind == token.QUOTED_IDENT
}

// beginsParameterType reports whether tok can begin a function-parameter type,
// including the templated "ANY ..." forms; see function_parameter and
// templated_parameter_type in googlesql.tm. Unlike beginsType it excludes
// reserved keywords (other than the type keywords ARRAY/STRUCT/RANGE/INTERVAL),
// so that a following reserved keyword such as AS is not mistaken for a type
// (which would wrongly make the preceding token a parameter name).
func (p *parser) beginsParameterType(tok token.Token) bool {
	switch {
	case tok.Kind == token.QUOTED_IDENT:
		return true
	case tok.Kind == token.IDENT && !p.isReserved(tok):
		return true
	case isKeyword(tok, "ARRAY"), isKeyword(tok, "STRUCT"), isKeyword(tok, "RANGE"), isKeyword(tok, "INTERVAL"), isKeyword(tok, "ANY"):
		return true
	}
	return false
}

// parseFunctionParameter parses "[name] type [AS alias] [DEFAULT expr]
// [NOT AGGREGATE]"; see function_parameter in googlesql.tm.
func (p *parser) parseFunctionParameter() (*ast.FunctionParameter, error) {
	var name *ast.Identifier
	// A parameter name must be an identifier (grammar: identifier? type...),
	// so a reserved keyword such as ANY cannot be a name: "ANY TYPE" with no
	// name parses as a templated_parameter_type rather than name ANY + type
	// TYPE. Uses the full reserved-keyword set (keywords.cc).
	if (p.peek().Kind == token.QUOTED_IDENT || (p.peek().Kind == token.IDENT && !token.IsReservedKeyword(p.peek().Image))) && p.beginsParameterType(p.peekAt(1)) {
		name = p.parseIdentifierToken(p.advance())
	}
	typ, err := p.parseFunctionParameterType()
	if err != nil {
		return nil, err
	}
	start := typ.Pos()
	if name != nil {
		start = name.Pos()
	}
	param := &ast.FunctionParameter{Span: span(start, typ.End()), Name: name, Type: typ}

	// Optional "AS alias" (the AS is required for the alias to bind here).
	if isKeyword(p.peek(), "AS") {
		alias, err := p.parseOptionalAlias()
		if err != nil {
			return nil, err
		}
		param.Alias = alias
		param.Stop = alias.End()
	}
	// Optional "DEFAULT expr".
	if isKeyword(p.peek(), "DEFAULT") {
		p.advance()
		def, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		param.DefaultValue = def
		param.Stop = p.extEnd(def)
	}
	// Optional "NOT AGGREGATE".
	if isKeyword(p.peek(), "NOT") && isKeyword(p.peekAt(1), "AGGREGATE") {
		p.advance()
		param.IsNotAggregate = true
		param.Stop = p.advance().End
	}
	return param, nil
}

// parseFunctionParameterType parses a function-parameter type, which may be a
// templated "ANY ..." type; see function_parameter in googlesql.tm.
func (p *parser) parseFunctionParameterType() (ast.Node, error) {
	switch {
	case isKeyword(p.peek(), "ANY"):
		return p.parseTemplatedParameterType()
	case isKeyword(p.peek(), "TABLE"):
		// A "TABLE<...>" TVF schema is accepted here; see type_or_tvf_schema in
		// googlesql.tm.
		return p.parseTVFSchema()
	}
	return p.parseType()
}

// parseTemplatedParameterType parses "ANY {TYPE|PROTO|ENUM|STRUCT|ARRAY|TABLE}";
// see templated_parameter_type in googlesql.tm. ANY is the next token.
func (p *parser) parseTemplatedParameterType() (*ast.TemplatedParameterType, error) {
	anyTok := p.advance() // ANY
	kindTok := p.peek()
	var kind string
	switch {
	case isKeyword(kindTok, "TYPE"):
		kind = "TYPE"
	case isKeyword(kindTok, "PROTO"):
		kind = "PROTO"
	case isKeyword(kindTok, "ENUM"):
		kind = "ENUM"
	case isKeyword(kindTok, "STRUCT"):
		kind = "STRUCT"
	case isKeyword(kindTok, "ARRAY"):
		kind = "ARRAY"
	case isKeyword(kindTok, "TABLE"):
		kind = "TABLE"
	case kindTok.Kind == token.QUOTED_IDENT || (kindTok.Kind == token.IDENT && !token.IsReservedKeyword(kindTok.Image)):
		// Grammar: templated_parameter_kind's identifier branch accepts only
		// TABLE/TYPE; any other (non-reserved) identifier (INTEGER, FLOAT, ...)
		// matches the branch but is rejected with this specific message. A
		// reserved keyword (AS) or non-identifier (")") never reaches the
		// branch and falls through to the generic error below. See
		// templated_parameter_kind in googlesql.tm.
		return nil, p.errorf(kindTok.Pos, "Syntax error: unexpected ANY template type")
	default:
		return nil, p.errorf(kindTok.Pos, "Syntax error: Unexpected %s", describeToken(kindTok))
	}
	p.advance()
	return &ast.TemplatedParameterType{Span: span(anyTok.Pos, p.prevEnd()), Kind: kind}, nil
}

// parseTVFSchema parses "TABLE< column [, ...] >"; see tvf_schema in
// googlesql.tm. The TABLE keyword is the next token.
func (p *parser) parseTVFSchema() (*ast.TVFSchema, error) {
	tableTok := p.advance() // TABLE
	if _, err := p.expectTemplateOpen(); err != nil {
		return nil, err
	}
	schema := &ast.TVFSchema{Span: span(tableTok.Pos, 0)}
	for {
		col, err := p.parseTVFSchemaColumn()
		if err != nil {
			return nil, err
		}
		schema.Columns = append(schema.Columns, col)
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance()
	}
	closeTok, err := p.expectTemplateClose()
	if err != nil {
		return nil, err
	}
	schema.Stop = closeTok.End
	return schema, nil
}

// parseTVFSchemaColumn parses "[name] type"; see tvf_schema_column in
// googlesql.tm. A leading identifier names the column only when it is followed
// by another token that can begin a type.
func (p *parser) parseTVFSchemaColumn() (*ast.TVFSchemaColumn, error) {
	var name *ast.Identifier
	if (p.peek().Kind == token.IDENT || p.peek().Kind == token.QUOTED_IDENT) && p.beginsType(p.peekAt(1)) {
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
	return &ast.TVFSchemaColumn{Span: span(start, typ.End()), Name: name, Type: typ}, nil
}

// parseCreateProcedureStatement parses the tail of "CREATE [OR REPLACE] [scope]
// PROCEDURE [IF NOT EXISTS] path(params) [EXTERNAL SECURITY ...]
// [WITH CONNECTION ...] [OPTIONS ...] (begin_end_block | LANGUAGE id [AS str])";
// see create_procedure_statement in googlesql.tm. The PROCEDURE keyword is the
// next token.
func (p *parser) parseCreateProcedureStatement(createTok token.Token, scope string, isOrReplace bool) (ast.Statement, error) {
	p.advance() // PROCEDURE
	stmt := &ast.CreateProcedureStatement{Span: span(createTok.Pos, 0), Scope: scope, IsOrReplace: isOrReplace}
	if isKeyword(p.peek(), "IF") && isKeyword(p.peekAt(1), "NOT") && isKeyword(p.peekAt(2), "EXISTS") {
		p.advance()
		p.advance()
		p.advance()
		stmt.IsIfNotExists = true
	}
	// A missing procedure name reports a generic "Unexpected" error rather than
	// "Expected identifier"; see create_procedure_statement in googlesql.tm.
	if tok := p.peek(); tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	name, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	stmt.Name = name
	// After the path, the parameter list must open with "("; the path could
	// also have continued with ".", so the error mentions both.
	if p.peek().Kind != token.LPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" or "." but got %s`, describeToken(p.peek()))
	}
	params, err := p.parseProcedureParameters()
	if err != nil {
		return nil, err
	}
	stmt.Parameters = params
	stmt.Stop = params.End()

	// EXTERNAL SECURITY (INVOKER | DEFINER).
	if isKeyword(p.peek(), "EXTERNAL") && isKeyword(p.peekAt(1), "SECURITY") {
		p.advance() // EXTERNAL
		p.advance() // SECURITY
		switch {
		case isKeyword(p.peek(), "INVOKER"):
			stmt.ExternalSecurity = "INVOKER"
		case isKeyword(p.peek(), "DEFINER"):
			stmt.ExternalSecurity = "DEFINER"
		default:
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword DEFINER or keyword INVOKER but got %s", describeToken(p.peek()))
		}
		stmt.Stop = p.advance().End
	}

	// Optional WITH CONNECTION clause.
	if isKeyword(p.peek(), "WITH") && isKeyword(p.peekAt(1), "CONNECTION") {
		wc, err := p.parseWithConnectionClause()
		if err != nil {
			return nil, err
		}
		stmt.WithConnection = wc
		stmt.Stop = wc.End()
	}

	// Optional OPTIONS list.
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		stmt.Options = opts
		stmt.Stop = opts.End()
	}

	// Body: either a BEGIN/END block (wrapped in a Script) or a
	// "LANGUAGE identifier [AS string]" external body.
	switch {
	case isKeyword(p.peek(), "BEGIN"):
		beb, err := p.parseBeginEndBlock()
		if err != nil {
			return nil, err
		}
		stmtList := &ast.StatementList{Span: span(beb.Pos(), beb.End()), Statements: []ast.Node{beb}}
		script := &ast.Script{Span: span(beb.Pos(), beb.End()), Statements: stmtList}
		stmt.Body = script
		stmt.Stop = script.End()
	case isKeyword(p.peek(), "LANGUAGE"):
		langKw := p.advance() // LANGUAGE
		tok := p.peek()
		if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
		}
		lang := p.parseIdentifierToken(p.advance())
		// The reference extends the language identifier's start to the LANGUAGE
		// keyword (WithStartLocation); see create_procedure_statement in
		// googlesql.tm.
		lang.Start = langKw.Pos
		stmt.Language = lang
		stmt.Stop = lang.End()
		if isKeyword(p.peek(), "AS") {
			p.advance() // AS
			code, err := p.parseStringLiteralValue()
			if err != nil {
				return nil, err
			}
			stmt.Code = code
			stmt.Stop = code.End()
		}
	default:
		return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
	}
	return stmt, nil
}

// parseProcedureParameters parses "( [procedure_parameter [, ...]] )"; see
// procedure_parameters in googlesql.tm. The opening parenthesis is the next
// token.
func (p *parser) parseProcedureParameters() (*ast.FunctionParameters, error) {
	lparen, err := p.expect(token.LPAREN, `"("`)
	if err != nil {
		return nil, err
	}
	fp := &ast.FunctionParameters{Span: span(lparen.Pos, 0)}
	if p.peek().Kind != token.RPAREN {
		for {
			param, err := p.parseProcedureParameter()
			if err != nil {
				return nil, err
			}
			fp.Parameters = append(fp.Parameters, param)
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
	fp.Stop = rparen.End
	return fp, nil
}

// parseProcedureParameter parses "[mode] identifier type_or_tvf_schema"; see
// procedure_parameter in googlesql.tm. The mode (IN/OUT/INOUT) is recognized
// only as an unquoted keyword.
func (p *parser) parseProcedureParameter() (*ast.FunctionParameter, error) {
	start := p.peek().Pos
	mode := ""
	switch {
	case isKeyword(p.peek(), "IN"):
		mode = "IN"
		p.advance()
	case isKeyword(p.peek(), "OUT"):
		mode = "OUT"
		p.advance()
	case isKeyword(p.peek(), "INOUT"):
		mode = "INOUT"
		p.advance()
	}
	name, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	// The type is required; hitting ")" or "," instead yields the reference's
	// tailored "end of parameter" diagnostic; see procedure_parameter in
	// googlesql.tm.
	if p.peek().Kind == token.RPAREN || p.peek().Kind == token.COMMA {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected end of parameter. Parameters should be in the format [<parameter mode>] <parameter name> <type>. If IN/OUT/INOUT is intended to be the name of a parameter, it must be escaped with backticks")
	}
	typ, err := p.parseProcedureParameterType()
	if err != nil {
		return nil, err
	}
	return &ast.FunctionParameter{Span: span(start, typ.End()), Name: name, Type: typ, Mode: mode}, nil
}

// parseProcedureParameterType parses a procedure-parameter type, which may be a
// templated "ANY ..." type or a "TABLE<...>" TVF schema in addition to an
// ordinary type; see type_or_tvf_schema in googlesql.tm.
func (p *parser) parseProcedureParameterType() (ast.Node, error) {
	switch {
	case isKeyword(p.peek(), "ANY"):
		return p.parseTemplatedParameterType()
	case isKeyword(p.peek(), "TABLE"):
		return p.parseTVFSchema()
	}
	return p.parseType()
}

// parseBeginEndBlock parses "BEGIN statement_list [exception_handler] END"; see
// begin_end_block in googlesql.tm. BEGIN is the next token.
func (p *parser) parseBeginEndBlock() (*ast.BeginEndBlock, error) {
	beginTok := p.advance() // BEGIN
	list, err := p.parseScriptStatementList(beginTok.End, func() bool {
		return isKeyword(p.peek(), "END") || isKeyword(p.peek(), "EXCEPTION")
	})
	if err != nil {
		return nil, err
	}
	var handlers *ast.ExceptionHandlerList
	if isKeyword(p.peek(), "EXCEPTION") {
		handlers, err = p.parseExceptionHandler()
		if err != nil {
			return nil, err
		}
	} else if p.peek().Kind == token.EOF {
		// In the block body the parser could still accept another statement,
		// EXCEPTION, or END, so an unexpected end of input here is reported
		// generically rather than as "Expected keyword END".
		return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
	}
	endTok, err := p.expectKeyword("END")
	if err != nil {
		return nil, err
	}
	return &ast.BeginEndBlock{Span: span(beginTok.Pos, endTok.End), Statements: list, Handlers: handlers}, nil
}

// parseExceptionHandler parses "EXCEPTION WHEN ERROR THEN statement_list",
// producing a single-element ASTExceptionHandlerList; see exception_handler in
// googlesql.tm. EXCEPTION is the next token. The handler node starts at the
// WHEN keyword (WithStartLocation in the reference), while the list starts at
// EXCEPTION.
func (p *parser) parseExceptionHandler() (*ast.ExceptionHandlerList, error) {
	excTok := p.advance() // EXCEPTION
	whenTok, err := p.expectKeyword("WHEN")
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("ERROR"); err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("THEN"); err != nil {
		return nil, err
	}
	// Stop the handler body at END as well as a stray WHEN/EXCEPTION so the
	// enclosing block reports "Expected keyword END but got ..." (the grammar
	// allows only a single handler); see exception_handler in googlesql.tm.
	body, err := p.parseScriptStatementList(p.prevEnd(), func() bool {
		return isKeyword(p.peek(), "END") || isKeyword(p.peek(), "WHEN") ||
			isKeyword(p.peek(), "EXCEPTION")
	})
	if err != nil {
		return nil, err
	}
	handler := &ast.ExceptionHandler{Span: span(whenTok.Pos, body.End()), Body: body}
	return &ast.ExceptionHandlerList{Span: span(excTok.Pos, body.End()), Handlers: []*ast.ExceptionHandler{handler}}, nil
}

// beginStartsTransaction reports whether a "BEGIN" at statement start begins a
// "BEGIN [TRANSACTION] ..." transaction statement rather than a begin/end
// block. The block continues with a statement, END, or EXCEPTION; the
// transaction statement is complete after BEGIN, optionally followed by
// TRANSACTION or a transaction mode, and can be terminated by ";" or end of
// input. See begin_statement and begin_end_block in googlesql.tm.
func (p *parser) beginStartsTransaction() bool {
	next := p.peekAt(1)
	switch {
	case isKeyword(next, "TRANSACTION"):
		return true
	case p.beginsTransactionMode(next):
		return true
	case next.Kind == token.SEMICOLON, next.Kind == token.EOF:
		return true
	}
	return false
}

// parseBeginStatement parses begin_statement: ("START" "TRANSACTION" | "BEGIN"
// "TRANSACTION"?) followed by an optional transaction mode list. The BEGIN or
// START keyword is the next token. See begin_statement in googlesql.tm.
func (p *parser) parseBeginStatement() (ast.Statement, error) {
	kw := p.advance() // BEGIN or START
	if isKeyword(kw, "START") {
		if _, err := p.expectKeyword("TRANSACTION"); err != nil {
			return nil, err
		}
	} else if isKeyword(p.peek(), "TRANSACTION") {
		p.advance() // TRANSACTION
	}
	modes, err := p.parseTransactionModeList()
	if err != nil {
		return nil, err
	}
	return &ast.BeginStatement{Span: span(kw.Pos, p.prevEnd()), ModeList: modes}, nil
}

// beginsTransactionMode reports whether tok can begin a transaction_mode:
// "READ" (ONLY/WRITE) or "ISOLATION" (LEVEL ...). See transaction_mode in
// googlesql.tm.
func (p *parser) beginsTransactionMode(tok token.Token) bool {
	return isKeyword(tok, "READ") || isKeyword(tok, "ISOLATION")
}

// parseTransactionModeList parses a comma-separated list of transaction modes,
// returning nil when no mode is present; see the (transaction_mode ...)* and
// (transaction_mode ...)+ productions in googlesql.tm.
func (p *parser) parseTransactionModeList() (*ast.TransactionModeList, error) {
	if !p.beginsTransactionMode(p.peek()) {
		return nil, nil
	}
	first, err := p.parseTransactionMode()
	if err != nil {
		return nil, err
	}
	list := &ast.TransactionModeList{Span: span(first.Pos(), first.End()), Modes: []ast.Node{first}}
	for p.peek().Kind == token.COMMA {
		p.advance() // ,
		mode, err := p.parseTransactionMode()
		if err != nil {
			return nil, err
		}
		list.Modes = append(list.Modes, mode)
		list.Stop = mode.End()
	}
	return list, nil
}

// parseTransactionMode parses a single transaction_mode: "READ ONLY",
// "READ WRITE", or "ISOLATION LEVEL identifier [identifier]"; see
// transaction_mode in googlesql.tm.
func (p *parser) parseTransactionMode() (ast.Node, error) {
	tok := p.peek()
	switch {
	case isKeyword(tok, "READ"):
		readTok := p.advance() // READ
		switch {
		case isKeyword(p.peek(), "ONLY"):
			onlyTok := p.advance()
			return &ast.TransactionReadWriteMode{Span: span(readTok.Pos, onlyTok.End), Mode: "READ_ONLY"}, nil
		case isKeyword(p.peek(), "WRITE"):
			writeTok := p.advance()
			return &ast.TransactionReadWriteMode{Span: span(readTok.Pos, writeTok.End), Mode: "READ_WRITE"}, nil
		}
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword ONLY or keyword WRITE but got %s", describeToken(p.peek()))
	default: // ISOLATION
		isoTok := p.advance() // ISOLATION
		if _, err := p.expectKeyword("LEVEL"); err != nil {
			return nil, err
		}
		id1, err := p.parseIdentifier()
		if err != nil {
			return nil, err
		}
		node := &ast.TransactionIsolationLevel{Span: span(isoTok.Pos, id1.End()), Identifier1: id1}
		if p.atIdentifier() {
			id2 := p.parseIdentifierToken(p.advance())
			node.Identifier2 = id2
			node.Stop = id2.End()
		}
		return node, nil
	}
}

// atIdentifier reports whether the next token is a bare identifier (a quoted
// identifier or a non-reserved keyword/identifier).
func (p *parser) atIdentifier() bool {
	tok := p.peek()
	return tok.Kind == token.QUOTED_IDENT || (tok.Kind == token.IDENT && !p.isReserved(tok))
}

// parseCommitStatement parses "COMMIT [TRANSACTION]"; see commit_statement in
// googlesql.tm. COMMIT is the next token.
func (p *parser) parseCommitStatement() (ast.Statement, error) {
	commitTok := p.advance() // COMMIT
	stop := commitTok.End
	if isKeyword(p.peek(), "TRANSACTION") {
		stop = p.advance().End
	}
	return &ast.CommitStatement{Span: span(commitTok.Pos, stop)}, nil
}

// parseRollbackStatement parses "ROLLBACK [TRANSACTION]"; see rollback_statement
// in googlesql.tm. ROLLBACK is the next token.
func (p *parser) parseRollbackStatement() (ast.Statement, error) {
	rollbackTok := p.advance() // ROLLBACK
	stop := rollbackTok.End
	if isKeyword(p.peek(), "TRANSACTION") {
		stop = p.advance().End
	}
	return &ast.RollbackStatement{Span: span(rollbackTok.Pos, stop)}, nil
}

// parseStartBatchStatement parses "START BATCH [batch_type]"; see
// start_batch_statement in googlesql.tm. START is the next token.
func (p *parser) parseStartBatchStatement() (ast.Statement, error) {
	startTok := p.advance() // START
	batchTok, err := p.expectKeyword("BATCH")
	if err != nil {
		return nil, err
	}
	stmt := &ast.StartBatchStatement{Span: span(startTok.Pos, batchTok.End)}
	if p.atIdentifier() {
		id := p.parseIdentifierToken(p.advance())
		stmt.BatchType = id
		stmt.Stop = id.End()
	}
	return stmt, nil
}

// parseRunBatchStatement parses "RUN BATCH"; see run_batch_statement in
// googlesql.tm. RUN is the next token.
func (p *parser) parseRunBatchStatement() (ast.Statement, error) {
	runTok := p.advance() // RUN
	batchTok, err := p.expectKeyword("BATCH")
	if err != nil {
		return nil, err
	}
	return &ast.RunBatchStatement{Span: span(runTok.Pos, batchTok.End)}, nil
}

// parseAbortBatchStatement parses "ABORT BATCH"; see abort_batch_statement in
// googlesql.tm. ABORT is the next token.
func (p *parser) parseAbortBatchStatement() (ast.Statement, error) {
	abortTok := p.advance() // ABORT
	batchTok, err := p.expectKeyword("BATCH")
	if err != nil {
		return nil, err
	}
	return &ast.AbortBatchStatement{Span: span(abortTok.Pos, batchTok.End)}, nil
}

// parseScriptStatementList parses a "statement_list": a sequence of statements
// each terminated by ";", stopping when isEnd reports the terminator keyword;
// see statement_list in googlesql.tm. An empty list is located at emptyPos.
func (p *parser) parseScriptStatementList(emptyPos int, isEnd func() bool) (*ast.StatementList, error) {
	if isEnd() {
		return &ast.StatementList{Span: span(emptyPos, emptyPos)}, nil
	}
	list := &ast.StatementList{Span: span(p.peek().Pos, 0)}
	for {
		stmt, err := p.parseScriptStatement()
		if err != nil {
			return nil, err
		}
		// A DEFINE MACRO statement is only allowed at the top level of a
		// script, not inside a block's statement list; see
		// unterminated_non_empty_statement_list in googlesql.tm.
		if _, ok := stmt.(*ast.DefineMacroStatement); ok {
			return nil, p.errorf(stmt.Pos(), "DEFINE MACRO statements cannot be nested under other statements or blocks.")
		}
		list.Statements = append(list.Statements, stmt)
		semi, err := p.expect(token.SEMICOLON, `";"`)
		if err != nil {
			return nil, err
		}
		list.Stop = semi.End
		if isEnd() || p.peek().Kind == token.EOF {
			break
		}
	}
	return list, nil
}

// parseScriptStatement parses a single statement that may appear in a script or
// block body: the script-only statements (DECLARE/IF/RETURN/BEGIN) plus any
// ordinary SQL statement; see unterminated_statement in googlesql.tm.
func (p *parser) parseScriptStatement() (ast.Statement, error) {
	switch {
	case isKeyword(p.peek(), "DECLARE"):
		return p.parseVariableDeclaration()
	case isKeyword(p.peek(), "IF"):
		return p.parseIfStatement()
	case isKeyword(p.peek(), "RETURN"):
		return p.parseReturnStatement()
	case isKeyword(p.peek(), "BEGIN"):
		// "BEGIN" starts either a begin/end block or a "BEGIN [TRANSACTION]"
		// statement. A block continues with a statement, END, or EXCEPTION; a
		// transaction statement is complete after "BEGIN" (optionally followed
		// by TRANSACTION and transaction modes). See begin_end_block and
		// begin_statement in googlesql.tm.
		if p.beginStartsTransaction() {
			return p.parseBeginStatement()
		}
		return p.parseBeginEndBlock()
	case isKeyword(p.peek(), "LOOP"):
		return p.parseLoopStatement()
	case isKeyword(p.peek(), "WHILE"):
		return p.parseWhileStatement()
	case isKeyword(p.peek(), "REPEAT"):
		return p.parseRepeatStatement()
	case isKeyword(p.peek(), "FOR"):
		return p.parseForInStatement()
	case isKeyword(p.peek(), "RAISE"):
		return p.parseRaiseStatement()
	case isKeyword(p.peek(), "BREAK"), isKeyword(p.peek(), "LEAVE"):
		return p.parseBreakStatement()
	case isKeyword(p.peek(), "CONTINUE"), isKeyword(p.peek(), "ITERATE"):
		return p.parseContinueStatement()
	}
	return p.parseStatement()
}

// parseRaiseStatement parses "RAISE" or "RAISE USING MESSAGE = expression"; see
// raise_statement in googlesql.tm. RAISE is the next token.
func (p *parser) parseRaiseStatement() (ast.Statement, error) {
	raiseTok := p.advance() // RAISE
	if !isKeyword(p.peek(), "USING") {
		return &ast.RaiseStatement{Span: span(raiseTok.Pos, raiseTok.End)}, nil
	}
	p.advance() // USING
	if _, err := p.expectKeyword("MESSAGE"); err != nil {
		return nil, err
	}
	if _, err := p.expect(token.EQ, `"="`); err != nil {
		return nil, err
	}
	msg, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	return &ast.RaiseStatement{Span: span(raiseTok.Pos, p.extEnd(msg)), Message: msg}, nil
}

// parseBreakStatement parses "BREAK" or "LEAVE"; both produce an
// ASTBreakStatement. See break_statement in googlesql.tm. The keyword is the
// next token.
func (p *parser) parseBreakStatement() (ast.Statement, error) {
	tok := p.advance() // BREAK / LEAVE
	return &ast.BreakStatement{Span: span(tok.Pos, tok.End), Keyword: strings.ToUpper(tok.Image)}, nil
}

// parseContinueStatement parses "CONTINUE" or "ITERATE"; both produce an
// ASTContinueStatement. See continue_statement in googlesql.tm. The keyword is
// the next token.
func (p *parser) parseContinueStatement() (ast.Statement, error) {
	tok := p.advance() // CONTINUE / ITERATE
	return &ast.ContinueStatement{Span: span(tok.Pos, tok.End), Keyword: strings.ToUpper(tok.Image)}, nil
}

// isLoopBodyEnd reports whether the next token terminates a loop/for/repeat
// body statement_list: the body's own END keyword or (for a REPEAT body) the
// UNTIL keyword. Stopping here lets the enclosing construct report the precise
// "Expected keyword END/UNTIL but got ..." error via expectKeyword.
func (p *parser) isLoopBodyEnd() bool {
	return isKeyword(p.peek(), "END") || isKeyword(p.peek(), "UNTIL")
}

// parseLoopStatement parses "LOOP statement_list END LOOP"; it produces an
// ASTWhileStatement with no condition. See loop_statement in googlesql.tm. LOOP
// is the next token.
func (p *parser) parseLoopStatement() (ast.Statement, error) {
	loopTok := p.advance() // LOOP
	body, err := p.parseScriptStatementList(loopTok.End, p.isLoopBodyEnd)
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("END"); err != nil {
		return nil, err
	}
	endTok, err := p.expectKeyword("LOOP")
	if err != nil {
		return nil, err
	}
	return &ast.WhileStatement{Span: span(loopTok.Pos, endTok.End), Body: body}, nil
}

// parseWhileStatement parses "WHILE expression DO statement_list END WHILE"; see
// while_statement in googlesql.tm. WHILE is the next token.
func (p *parser) parseWhileStatement() (ast.Statement, error) {
	whileTok := p.advance() // WHILE
	cond, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("DO"); err != nil {
		return nil, err
	}
	body, err := p.parseScriptStatementList(p.prevEnd(), p.isLoopBodyEnd)
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("END"); err != nil {
		return nil, err
	}
	endTok, err := p.expectKeyword("WHILE")
	if err != nil {
		return nil, err
	}
	return &ast.WhileStatement{Span: span(whileTok.Pos, endTok.End), Condition: cond, Body: body}, nil
}

// parseRepeatStatement parses "REPEAT statement_list until_clause END REPEAT";
// see repeat_statement in googlesql.tm. REPEAT is the next token.
func (p *parser) parseRepeatStatement() (ast.Statement, error) {
	repeatTok := p.advance() // REPEAT
	if !p.features.Enabled(FeatureRepeat) {
		// No "Syntax error: " prefix; see repeat_statement in googlesql.tm.
		return nil, p.errorf(repeatTok.Pos, "REPEAT is not supported")
	}
	body, err := p.parseScriptStatementList(repeatTok.End, p.isLoopBodyEnd)
	if err != nil {
		return nil, err
	}
	untilTok, err := p.expectKeyword("UNTIL")
	if err != nil {
		return nil, err
	}
	cond, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	until := &ast.UntilClause{Span: span(untilTok.Pos, p.extEnd(cond)), Condition: cond}
	if _, err := p.expectKeyword("END"); err != nil {
		return nil, err
	}
	endTok, err := p.expectKeyword("REPEAT")
	if err != nil {
		return nil, err
	}
	return &ast.RepeatStatement{Span: span(repeatTok.Pos, endTok.End), Body: body, Until: until}, nil
}

// parseForInStatement parses "FOR identifier IN ( query ) DO statement_list END
// FOR"; see for_in_statement in googlesql.tm. FOR is the next token.
func (p *parser) parseForInStatement() (ast.Statement, error) {
	forTok := p.advance() // FOR
	variable, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("IN"); err != nil {
		return nil, err
	}
	if p.peek().Kind != token.LPAREN {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected %s but got %s", `"("`, describeToken(p.peek()))
	}
	query, _, err := p.parseParenthesizedQuery()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("DO"); err != nil {
		return nil, err
	}
	body, err := p.parseScriptStatementList(p.prevEnd(), p.isLoopBodyEnd)
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("END"); err != nil {
		return nil, err
	}
	endTok, err := p.expectKeyword("FOR")
	if err != nil {
		return nil, err
	}
	if !p.features.Enabled(FeatureForIn) {
		// The FOR...IN feature is validated at rule reduction, i.e. after the
		// whole statement (including its body) has parsed, so an error inside
		// the body surfaces first. No "Syntax error: " prefix; see
		// for_in_statement in googlesql.tm.
		return nil, p.errorf(forTok.Pos, "FOR...IN is not supported")
	}
	return &ast.ForInStatement{Span: span(forTok.Pos, endTok.End), Variable: variable, Query: query, Body: body}, nil
}

// parseVariableDeclaration parses "DECLARE identifier_list (type [DEFAULT expr]
// | DEFAULT expr)"; see variable_declaration in googlesql.tm. DECLARE is the
// next token.
func (p *parser) parseVariableDeclaration() (ast.Statement, error) {
	declTok := p.advance() // DECLARE
	list, err := p.parseIdentifierList()
	if err != nil {
		return nil, err
	}
	vd := &ast.VariableDeclaration{Span: span(declTok.Pos, list.End()), Variables: list}
	if isKeyword(p.peek(), "DEFAULT") {
		p.advance()
		def, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		vd.DefaultValue = def
		vd.Stop = p.extEnd(def)
		return vd, nil
	}
	typ, err := p.parseType()
	if err != nil {
		return nil, err
	}
	vd.Type = typ
	vd.Stop = typ.End()
	if isKeyword(p.peek(), "DEFAULT") {
		p.advance()
		def, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		vd.DefaultValue = def
		vd.Stop = p.extEnd(def)
	}
	return vd, nil
}

// parseReturnStatement parses the "RETURN" script statement; see
// return_statement in googlesql.tm.
func (p *parser) parseReturnStatement() (ast.Statement, error) {
	tok := p.advance() // RETURN
	return &ast.ReturnStatement{Span: span(tok.Pos, tok.End)}, nil
}

// parseIfStatement parses "IF expr THEN stmts [ELSEIF ...] [ELSE stmts] END IF";
// see if_statement in googlesql.tm. IF is the next token.
func (p *parser) parseIfStatement() (ast.Statement, error) {
	ifTok := p.advance() // IF
	cond, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("THEN"); err != nil {
		return nil, err
	}
	isBodyEnd := func() bool {
		return isKeyword(p.peek(), "ELSEIF") || isKeyword(p.peek(), "ELSE") || isKeyword(p.peek(), "END")
	}
	thenList, err := p.parseScriptStatementList(p.prevEnd(), isBodyEnd)
	if err != nil {
		return nil, err
	}
	var elifList *ast.ElseifClauseList
	for isKeyword(p.peek(), "ELSEIF") {
		elifTok := p.advance() // ELSEIF
		econd, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		if _, err := p.expectKeyword("THEN"); err != nil {
			return nil, err
		}
		ebody, err := p.parseScriptStatementList(p.prevEnd(), isBodyEnd)
		if err != nil {
			return nil, err
		}
		clause := &ast.ElseifClause{Span: span(elifTok.Pos, ebody.End()), Condition: econd, Body: ebody}
		if elifList == nil {
			elifList = &ast.ElseifClauseList{Span: span(elifTok.Pos, ebody.End())}
		}
		elifList.Clauses = append(elifList.Clauses, clause)
		elifList.Stop = ebody.End()
	}
	var elseList *ast.StatementList
	if isKeyword(p.peek(), "ELSE") {
		elseTok := p.advance() // ELSE
		elseList, err = p.parseScriptStatementList(elseTok.End, isBodyEnd)
		if err != nil {
			return nil, err
		}
	}
	if _, err := p.expectKeyword("END"); err != nil {
		return nil, err
	}
	ifEnd, err := p.expectKeyword("IF")
	if err != nil {
		return nil, err
	}
	return &ast.IfStatement{
		Span:          span(ifTok.Pos, ifEnd.End),
		Condition:     cond,
		ThenList:      thenList,
		ElseifClauses: elifList,
		ElseList:      elseList,
	}, nil
}

// beginsType reports whether tok can begin a type; see raw_type in
// googlesql.tm.
func (p *parser) beginsType(tok token.Token) bool {
	switch {
	case tok.Kind == token.IDENT, tok.Kind == token.QUOTED_IDENT:
		return true
	case isKeyword(tok, "ARRAY"), isKeyword(tok, "STRUCT"), isKeyword(tok, "RANGE"), isKeyword(tok, "INTERVAL"):
		return true
	}
	return false
}

// parseDropEntityStatement parses "DROP generic_entity_type opt_if_exists
// path_expression"; see drop_statement in googlesql.tm. The entity type is the
// next token (already verified supported by the caller).
func (p *parser) parseDropEntityStatement(dropTok token.Token) (ast.Statement, error) {
	typeTok := p.advance() // entity type
	entType := p.parseIdentifierToken(typeTok)
	stmt := &ast.DropEntityStatement{Span: span(dropTok.Pos, typeTok.End), EntityType: entType}
	stmt.IsIfExists = p.tryParseIfExists()
	name, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	stmt.Name = name
	stmt.Stop = name.End()
	return stmt, nil
}

// parseDropStatement parses a DROP <object kind> [IF EXISTS] <path> statement;
// see drop_statement in googlesql.tm. Most object kinds share the generic
// ASTDropStatement node (which records the schema object kind name); FUNCTION,
// TABLE FUNCTION, MATERIALIZED VIEW and SNAPSHOT TABLE use their own node
// classes. Object kinds the reference grammar recognizes but does not support
// (AGGREGATE FUNCTION, EXTERNAL TABLE FUNCTION) are diagnosed here.
func (p *parser) parseDropStatement() (ast.Statement, error) {
	dropTok := p.advance() // DROP
	kindTok := p.peek()
	second := p.peekAt(1)
	third := p.peekAt(2)
	consumeSecond := func() { p.advance(); p.advance() }

	// nodeName is the parse tree node class; objectKind is the schema object
	// kind name shown by the generic DropStatement node and in drop_mode
	// errors; parsesDropMode reports whether opt_drop_mode is part of the
	// rule for this kind (TABLE FUNCTION and SNAPSHOT TABLE do not parse it,
	// so a trailing RESTRICT/CASCADE is reported as an unexpected token).
	nodeName := "DropStatement"
	var objectKind string
	parsesDropMode := true
	isSchema := false
	dashedName := false

	switch {
	case isKeyword(kindTok, "ROW"):
		// DROP ROW ACCESS POLICY ...; see drop_statement in googlesql.tm. Only
		// the unquoted keyword ROW triggers this; a quoted `row` object type
		// falls through to the generic handling below.
		return p.parseDropRowAccessPolicyStatement(dropTok)
	case isKeyword(kindTok, "ALL"):
		// DROP ALL ROW [ACCESS] POLICIES ON ...; see
		// drop_all_row_access_policies_statement in googlesql.tm.
		return p.parseDropAllRowAccessPoliciesStatement(dropTok)
	case isKeyword(kindTok, "EXTERNAL") && isKeyword(second, "TABLE") && isKeyword(third, "FUNCTION"):
		// No "Syntax error: " prefix; see drop_statement in googlesql.tm.
		return nil, p.errorf(kindTok.Pos, "EXTERNAL TABLE FUNCTION is not supported")
	case isKeyword(kindTok, "EXTERNAL") && isKeyword(second, "TABLE"):
		consumeSecond()
		objectKind = "EXTERNAL TABLE"
	case isKeyword(kindTok, "EXTERNAL") && isKeyword(second, "SCHEMA"):
		consumeSecond()
		objectKind = "EXTERNAL SCHEMA"
	case isKeyword(kindTok, "AGGREGATE") && isKeyword(second, "FUNCTION"):
		return nil, p.errorf(kindTok.Pos, "DROP AGGREGATE FUNCTION is not supported, use DROP FUNCTION")
	case isKeyword(kindTok, "MATERIALIZED") && isKeyword(second, "VIEW"):
		consumeSecond()
		nodeName = "DropMaterializedViewStatement"
		objectKind = "MATERIALIZED VIEW"
	case isKeyword(kindTok, "SNAPSHOT") && isKeyword(second, "TABLE"):
		consumeSecond()
		nodeName = "DropSnapshotTableStatement"
		parsesDropMode = false
		dashedName = true
	case isKeyword(kindTok, "TABLE") && isKeyword(second, "FUNCTION"):
		consumeSecond()
		nodeName = "DropTableFunctionStatement"
		parsesDropMode = false
		dashedName = true
	case isKeyword(kindTok, "FUNCTION"):
		p.advance()
		nodeName = "DropFunctionStatement"
		objectKind = "FUNCTION"
	case isKeyword(kindTok, "SCHEMA"):
		p.advance()
		objectKind = "SCHEMA"
		isSchema = true
	case isKeyword(kindTok, "TABLE"):
		// DROP TABLE has its own grammar rule (using a maybe-dashed path) and
		// does not accept opt_drop_mode, so a trailing RESTRICT/CASCADE is an
		// unexpected token rather than an "unsupported drop mode" error.
		p.advance()
		objectKind = "TABLE"
		parsesDropMode = false
		dashedName = true
	case isKeyword(kindTok, "SEQUENCE"), isKeyword(kindTok, "CONNECTION"),
		isKeyword(kindTok, "CONSTANT"), isKeyword(kindTok, "DATABASE"),
		isKeyword(kindTok, "INDEX"), isKeyword(kindTok, "MODEL"),
		isKeyword(kindTok, "PROCEDURE"), isKeyword(kindTok, "VIEW"):
		p.advance()
		objectKind = strings.ToUpper(kindTok.Image)
	case isGenericEntityTypeToken(kindTok):
		// DROP generic_entity_type [IF EXISTS] path; see drop_statement in
		// googlesql.tm. An unsupported type reports "<type> is not a supported
		// object type" from the generic_entity_type reduce action.
		if !p.entityTypes[strings.ToUpper(kindTok.Image)] {
			return nil, p.errorf(kindTok.Pos, "%s is not a supported object type", kindTok.Image)
		}
		return p.parseDropEntityStatement(dropTok)
	case kindTok.Kind == token.QUOTED_IDENT:
		// A backtick-quoted identifier is never a supported entity type; the
		// backticks are kept as part of the reported type name. See
		// generic_entity_type_unchecked in googlesql.tm.
		return nil, p.errorf(kindTok.Pos, "%s is not a supported object type", kindTok.Image)
	default:
		return nil, p.errorf(kindTok.Pos, "Syntax error: Unexpected %s", describeToken(kindTok))
	}

	// For DropFunctionStatement the generic node still needs the schema object
	// kind name for drop_mode errors, but it is not shown in the debug string.
	errKind := objectKind
	if nodeName == "DropFunctionStatement" {
		errKind = "FUNCTION"
		objectKind = ""
	} else if nodeName != "DropStatement" {
		objectKind = ""
	}

	stmt := &ast.DropStatement{Span: span(dropTok.Pos, 0), NodeName: nodeName, ObjectKind: objectKind}
	if isKeyword(p.peek(), "IF") {
		p.advance() // IF
		if _, err := p.expectKeyword("EXISTS"); err != nil {
			return nil, err
		}
		stmt.IsIfExists = true
	}
	if tok := p.peek(); tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	var path *ast.PathExpression
	var err error
	if dashedName {
		path, err = p.parseMaybeDashedPathExpression()
	} else {
		path, err = p.parsePathExpression()
	}
	if err != nil {
		return nil, err
	}
	stmt.Path = path
	stmt.Stop = path.End()

	// A function parameter list is only accepted for DROP FUNCTION; other
	// object kinds diagnose the "(" specifically. See drop_statement and
	// drop_function_statement in googlesql.tm.
	if p.peek().Kind == token.LPAREN {
		switch {
		case nodeName == "DropFunctionStatement":
			params, err := p.parseFunctionParameters()
			if err != nil {
				return nil, err
			}
			stmt.Parameters = params
			stmt.Stop = params.End()
		case nodeName == "DropTableFunctionStatement":
			return nil, p.errorf(p.peek().Pos, "Syntax error: Parameters are not supported for DROP TABLE FUNCTION because table functions don't support overloading")
		case objectKind == "TABLE", nodeName == "DropSnapshotTableStatement":
			// DROP TABLE (table_or_table_function rule) and DROP SNAPSHOT TABLE
			// have no function_parameters production; the "(" is unexpected.
			return nil, p.errorf(p.peek().Pos, `Syntax error: Unexpected "("`)
		default:
			// schema_object_kind rule: any function parameters are only accepted
			// for DROP FUNCTION. See drop_statement in googlesql.tm.
			return nil, p.errorf(p.peek().Pos, "Syntax error: Parameters are only supported for DROP FUNCTION")
		}
	}

	if parsesDropMode {
		if tok := p.peek(); isKeyword(tok, "RESTRICT") || isKeyword(tok, "CASCADE") {
			mode := strings.ToUpper(tok.Image)
			if isSchema {
				p.advance()
				stmt.DropMode = mode
				stmt.Stop = tok.End
			} else {
				return nil, p.errorf(tok.Pos, "Syntax error: '%s' is not supported for DROP %s", mode, errKind)
			}
		}
	}
	return stmt, nil
}

// parseDropRowAccessPolicyStatement parses
// "DROP ROW ACCESS POLICY [IF EXISTS] identifier ON path"; see the
// "DROP" "ROW" "ACCESS" "POLICY" ... alternative of drop_statement in
// googlesql.tm. The DROP keyword is already consumed; the parser is at ROW.
func (p *parser) parseDropRowAccessPolicyStatement(dropTok token.Token) (ast.Statement, error) {
	p.advance() // ROW
	if _, err := p.expectKeyword("ACCESS"); err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("POLICY"); err != nil {
		return nil, err
	}
	isIfExists := p.tryParseIfExists()
	name, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	// The policy name is a single identifier wrapped in a one-component path;
	// see MakeNode<ASTPathExpression>(@6, $6) in the reduce action.
	namePath := &ast.PathExpression{Span: span(name.Pos(), name.End()), Names: []*ast.Identifier{name}}
	if _, err := p.expectKeyword("ON"); err != nil {
		return nil, err
	}
	target, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	return &ast.DropRowAccessPolicyStatement{
		Span:       span(dropTok.Pos, target.End()),
		IsIfExists: isIfExists,
		Name:       namePath,
		Target:     target,
	}, nil
}

// parseDropAllRowAccessPoliciesStatement parses
// "DROP ALL ROW [ACCESS] POLICIES ON path"; see
// drop_all_row_access_policies_statement in googlesql.tm. The DROP keyword is
// already consumed; the parser is at ALL.
func (p *parser) parseDropAllRowAccessPoliciesStatement(dropTok token.Token) (ast.Statement, error) {
	p.advance() // ALL
	if _, err := p.expectKeyword("ROW"); err != nil {
		return nil, err
	}
	hasAccess := false
	if isKeyword(p.peek(), "ACCESS") {
		p.advance() // ACCESS
		hasAccess = true
		if !isKeyword(p.peek(), "POLICIES") {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword POLICIES but got %s", describeToken(p.peek()))
		}
		p.advance() // POLICIES
	} else {
		if !isKeyword(p.peek(), "POLICIES") {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword ACCESS or keyword POLICIES but got %s", describeToken(p.peek()))
		}
		p.advance() // POLICIES
	}
	if _, err := p.expectKeyword("ON"); err != nil {
		return nil, err
	}
	target, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	return &ast.DropAllRowAccessPoliciesStatement{
		Span:             span(dropTok.Pos, target.End()),
		HasAccessKeyword: hasAccess,
		Target:           target,
	}, nil
}

// parseGrantOrRevokeStatement parses a GRANT or REVOKE statement; see
// grant_statement and revoke_statement in googlesql.tm. The two share the same
// shape apart from the TO/FROM keyword and node class: privileges, an object
// (with zero, one, or two leading object-type identifiers such as "table" or
// "materialized view"), then the grantee list.
func (p *parser) parseGrantOrRevokeStatement(isRevoke bool) (ast.Statement, error) {
	startTok := p.advance() // GRANT or REVOKE
	privs, err := p.parsePrivileges()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("ON"); err != nil {
		return nil, err
	}

	// The object name is preceded by up to two object-type identifiers. An
	// identifier is a leading type word only when it is followed by another
	// identifier that could begin the path (or another type word); otherwise it
	// is the start of the object path_expression. See the three alternatives of
	// grant_statement/revoke_statement in googlesql.tm.
	var objectTypes []*ast.Identifier
	for {
		tok := p.peek()
		if !beginsObjectIdentifier(tok) {
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
		}
		if len(objectTypes) < 2 && beginsObjectIdentifier(p.peekAt(1)) {
			objectTypes = append(objectTypes, p.parseIdentifierToken(p.advance()))
			continue
		}
		break
	}
	path, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}

	// Expect the TO (GRANT) / FROM (REVOKE) keyword. The path may still continue
	// with ".", so the error's expected-token set includes "."; the terminator
	// keyword is only listed when there were no leading object-type identifiers,
	// matching the reference's LALR state.
	term := "TO"
	if isRevoke {
		term = "FROM"
	}
	if !isKeyword(p.peek(), term) {
		expected := `"."`
		if len(objectTypes) == 0 {
			expected = `"." or keyword ` + term
		}
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected %s but got %s", expected, describeToken(p.peek()))
	}
	p.advance() // TO / FROM
	grantees, err := p.parseGranteeList()
	if err != nil {
		return nil, err
	}

	sp := span(startTok.Pos, grantees.End())
	if isRevoke {
		return &ast.RevokeStatement{Span: sp, Privileges: privs, ObjectTypes: objectTypes, Path: path, Grantees: grantees}, nil
	}
	return &ast.GrantStatement{Span: sp, Privileges: privs, ObjectTypes: objectTypes, Path: path, Grantees: grantees}, nil
}

// parseDescribeStatement parses "{DESCRIBE|DESC} [object_type] name
// [FROM path]"; see describe_statement and describe_info in googlesql.tm. The
// optional leading object-type identifier is present only when the first
// identifier is followed by another identifier that begins the object path.
func (p *parser) parseDescribeStatement() (ast.Statement, error) {
	kwTok := p.advance() // DESCRIBE or DESC

	var objectType *ast.Identifier
	if beginsObjectIdentifier(p.peek()) && beginsObjectIdentifier(p.peekAt(1)) {
		objectType = p.parseIdentifierToken(p.advance())
	}
	name, err := p.parseMaybeSlashedOrDashedPathExpression()
	if err != nil {
		return nil, err
	}
	stmt := &ast.DescribeStatement{Span: span(kwTok.Pos, name.End()), ObjectType: objectType, Name: name}
	if isKeyword(p.peek(), "FROM") {
		p.advance() // FROM
		from, err := p.parseMaybeSlashedOrDashedPathExpression()
		if err != nil {
			return nil, err
		}
		stmt.OptionalFrom = from
		stmt.Stop = from.End()
	}
	return stmt, nil
}

// parseShowStatement parses "SHOW show_target [FROM path] [LIKE 'pattern']";
// see show_statement in googlesql.tm. The show_target is either the two-word
// "MATERIALIZED VIEWS" (folded into one identifier) or a single identifier.
func (p *parser) parseShowStatement() (ast.Statement, error) {
	showTok := p.advance() // SHOW

	var target *ast.Identifier
	if isKeyword(p.peek(), "MATERIALIZED") && isKeyword(p.peekAt(1), "VIEWS") {
		m := p.advance() // MATERIALIZED
		v := p.advance() // VIEWS
		target = &ast.Identifier{Span: span(m.Pos, v.End), Name: "MATERIALIZED VIEWS"}
	} else {
		id, err := p.parseIdentifier()
		if err != nil {
			return nil, err
		}
		target = id
	}
	stmt := &ast.ShowStatement{Span: span(showTok.Pos, target.End()), Target: target}

	if isKeyword(p.peek(), "FROM") {
		p.advance() // FROM
		path, err := p.parseMaybeSlashedOrDashedPathExpression()
		if err != nil {
			return nil, err
		}
		stmt.OptionalName = path
		stmt.Stop = path.End()
	}
	if isKeyword(p.peek(), "LIKE") {
		p.advance() // LIKE
		if p.peek().Kind != token.STRING {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected string literal but got %s", describeToken(p.peek()))
		}
		like, err := p.parseStringLiteral()
		if err != nil {
			return nil, err
		}
		stmt.Like = like
		stmt.Stop = like.End()
	}
	return stmt, nil
}

// beginsObjectIdentifier reports whether tok can begin an object identifier or
// object-type word in a GRANT/REVOKE statement: a quoted identifier or a
// non-reserved (bare or unreserved-keyword) identifier.
func beginsObjectIdentifier(tok token.Token) bool {
	return tok.Kind == token.QUOTED_IDENT || (tok.Kind == token.IDENT && !isReservedStatic(tok))
}

// parsePrivileges parses "ALL [PRIVILEGES]" or a comma-separated privilege
// list; see privileges in googlesql.tm. "ALL" produces an empty ASTPrivileges
// node (no privilege children).
func (p *parser) parsePrivileges() (*ast.Privileges, error) {
	if isKeyword(p.peek(), "ALL") {
		allTok := p.advance() // ALL
		stop := allTok.End
		if isKeyword(p.peek(), "PRIVILEGES") {
			stop = p.advance().End
		}
		return &ast.Privileges{Span: span(allTok.Pos, stop)}, nil
	}
	first, err := p.parsePrivilege()
	if err != nil {
		return nil, err
	}
	privs := &ast.Privileges{Span: span(first.Pos(), first.End()), Privileges: []*ast.Privilege{first}}
	for p.peek().Kind == token.COMMA {
		p.advance()
		priv, err := p.parsePrivilege()
		if err != nil {
			return nil, err
		}
		privs.Privileges = append(privs.Privileges, priv)
		privs.Stop = priv.End()
	}
	return privs, nil
}

// parsePrivilege parses "privilege_name [(path [, ...])]"; see privilege in
// googlesql.tm.
func (p *parser) parsePrivilege() (*ast.Privilege, error) {
	name, err := p.parsePrivilegeName()
	if err != nil {
		return nil, err
	}
	priv := &ast.Privilege{Span: span(name.Pos(), name.End()), Name: name}
	if p.peek().Kind == token.LPAREN {
		cols, err := p.parsePathExpressionListWithParens()
		if err != nil {
			return nil, err
		}
		priv.Columns = cols
		priv.Stop = cols.End()
	}
	return priv, nil
}

// parsePrivilegeName parses a privilege name: any identifier or the reserved
// keyword SELECT; see privilege_name in googlesql.tm.
func (p *parser) parsePrivilegeName() (*ast.Identifier, error) {
	tok := p.peek()
	if isKeyword(tok, "SELECT") || beginsObjectIdentifier(tok) {
		return p.parseIdentifierToken(p.advance()), nil
	}
	return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
}

// parsePathExpressionListWithParens parses "( path [, ...] )"; see
// path_expression_list_with_parens in googlesql.tm. The opening "(" is next.
func (p *parser) parsePathExpressionListWithParens() (*ast.PathExpressionList, error) {
	lparen, err := p.expect(token.LPAREN, `"("`)
	if err != nil {
		return nil, err
	}
	list := &ast.PathExpressionList{Span: span(lparen.Pos, 0)}
	for {
		path, err := p.parsePathExpression()
		if err != nil {
			return nil, err
		}
		list.Paths = append(list.Paths, path)
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
	case isKeyword(kindTok, "ROW"):
		return p.parseAlterRowAccessPolicyStatement(alterTok)
	case isKeyword(kindTok, "ALL"):
		return p.parseAlterAllRowAccessPoliciesStatement(alterTok)
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
		// ALTER generic_entity_type ...; see alter_statement in googlesql.tm. A
		// generic entity type is a bare identifier or the PROJECT keyword. When
		// it is in the supported set the statement parses; otherwise it reports
		// "<type> is not a supported object type" (no "Syntax error: " prefix).
		if isGenericEntityTypeToken(kindTok) {
			if !p.entityTypes[strings.ToUpper(kindTok.Image)] {
				return nil, p.errorf(kindTok.Pos, "%s is not a supported object type", kindTok.Image)
			}
			return p.parseAlterEntityStatement(alterTok)
		}
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
	var path *ast.PathExpression
	var err error
	if nodeName == "AlterTableStatement" {
		// Only ALTER TABLE uses a maybe-dashed table name; see
		// alter_statement in googlesql.tm.
		path, err = p.parseMaybeDashedPathExpression()
	} else {
		path, err = p.parsePathExpression()
	}
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

// parseAlterEntityStatement parses "ALTER generic_entity_type opt_if_exists
// [path_expression] alter_action_list"; see the ALTER generic_entity_type
// productions in googlesql.tm. The entity type is the next token (already
// verified supported by the caller). The path expression is optional: when the
// token after opt_if_exists begins an alter action rather than a path, the
// no-path form applies (e.g. "ALTER PROJECT SET OPTIONS (...)").
func (p *parser) parseAlterEntityStatement(alterTok token.Token) (ast.Statement, error) {
	typeTok := p.advance() // entity type
	entType := p.parseIdentifierToken(typeTok)
	stmt := &ast.AlterStatement{Span: span(alterTok.Pos, typeTok.End), NodeName: "AlterEntityStatement", EntityType: entType}
	stmt.IsIfExists = p.tryParseIfExists()
	if p.hasEntityAlterPath() {
		path, err := p.parsePathExpression()
		if err != nil {
			return nil, err
		}
		stmt.Path = path
		stmt.Stop = path.End()
	}
	actions, err := p.parseAlterActionList()
	if err != nil {
		return nil, err
	}
	stmt.Actions = actions
	stmt.Stop = actions.End()
	return stmt, nil
}

// hasEntityAlterPath reports whether an optional path_expression precedes the
// alter action list of an ALTER generic_entity_type statement. A path is
// present when the current token can begin a path_expression (a valid
// identifier) and does not itself begin an alter action; see the two ALTER
// generic_entity_type productions in googlesql.tm. For example "ALTER PROJECT
// ADD DROP CONSTRAINT foo" reads ADD as the path (since "ADD DROP" cannot begin
// an action) while "ALTER PROJECT DROP CONSTRAINT foo" reads DROP as the start
// of a DROP CONSTRAINT action.
func (p *parser) hasEntityAlterPath() bool {
	tok := p.peek()
	if tok.Kind != token.QUOTED_IDENT && (tok.Kind != token.IDENT || p.isReserved(tok)) {
		return false
	}
	return !p.tokenStartsAlterAction()
}

// tokenStartsAlterAction reports whether the tokens at the current position
// begin an alter_action; see alter_action in googlesql.tm. It uses one token
// of lookahead to distinguish, e.g., "ADD DROP ..." (ADD is not an action)
// from "ADD COLUMN ..." (an ADD COLUMN action).
func (p *parser) tokenStartsAlterAction() bool {
	tok := p.peek()
	next := p.peekAt(1)
	switch {
	case isKeyword(tok, "SET"), isKeyword(tok, "RENAME"):
		return true
	case isKeyword(tok, "REPLACE"):
		return isKeyword(next, "ROW")
	case isKeyword(tok, "ADD"):
		return isKeyword(next, "COLUMN") || isKeyword(next, "CONSTRAINT") ||
			isKeyword(next, "ROW") || isKeyword(next, "PRIMARY") ||
			isKeyword(next, "FOREIGN") || isKeyword(next, "CHECK") ||
			isSubEntityTypeToken(next)
	case isKeyword(tok, "DROP"):
		return isKeyword(next, "COLUMN") || isKeyword(next, "CONSTRAINT") ||
			isKeyword(next, "PRIMARY") || isKeyword(next, "ROW") ||
			isSubEntityTypeToken(next)
	case isKeyword(tok, "ALTER"):
		return isKeyword(next, "COLUMN") || isKeyword(next, "CONSTRAINT") ||
			isSubEntityTypeToken(next)
	}
	return false
}

// isSubEntityTypeToken reports whether tok can begin a generic sub-entity
// type: a bare (non-keyword) identifier or the REPLICA keyword; see
// sub_entity_type_identifier in googlesql.tm.
func isSubEntityTypeToken(tok token.Token) bool {
	if tok.Kind != token.IDENT {
		return false
	}
	if strings.EqualFold(tok.Image, "REPLICA") {
		return true
	}
	return !keywordNames[strings.ToLower(tok.Image)]
}

// parseAddSubEntityAction parses "generic_sub_entity_type opt_if_not_exists
// identifier opt_options_list" after ADD; see the ADD generic_sub_entity_type
// production in googlesql.tm. The sub-entity type is the next token.
func (p *parser) parseAddSubEntityAction(addTok token.Token) (ast.Node, error) {
	typeTok := p.advance() // sub-entity type
	if !p.subEntityTypes[strings.ToUpper(typeTok.Image)] {
		// No "Syntax error: " prefix; see generic_sub_entity_type in googlesql.tm.
		return nil, p.errorf(typeTok.Pos, "%s is not a supported nested object type", typeTok.Image)
	}
	entType := p.parseIdentifierToken(typeTok)
	ifNotExists, err := p.parseOptIfNotExists()
	if err != nil {
		return nil, err
	}
	name, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	node := &ast.AddSubEntityAction{Span: span(addTok.Pos, name.End()), IsIfNotExists: ifNotExists, Type: entType, Name: name}
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		node.Options = opts
		node.Stop = opts.End()
	}
	return node, nil
}

// parseDropSubEntityAction parses "generic_sub_entity_type opt_if_exists
// identifier" after DROP; see the DROP generic_sub_entity_type production in
// googlesql.tm. The sub-entity type is the next token.
func (p *parser) parseDropSubEntityAction(dropTok token.Token) (ast.Node, error) {
	typeTok := p.advance() // sub-entity type
	if !p.subEntityTypes[strings.ToUpper(typeTok.Image)] {
		return nil, p.errorf(typeTok.Pos, "%s is not a supported nested object type", typeTok.Image)
	}
	entType := p.parseIdentifierToken(typeTok)
	ifExists := p.tryParseIfExists()
	name, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	return &ast.DropSubEntityAction{Span: span(dropTok.Pos, name.End()), IsIfExists: ifExists, Type: entType, Name: name}, nil
}

// parseAlterSubEntityAction parses "generic_sub_entity_type opt_if_exists
// identifier alter_action" after ALTER; see the ALTER generic_sub_entity_type
// production in googlesql.tm. The sub-entity type is the next token.
func (p *parser) parseAlterSubEntityAction(alterTok token.Token) (ast.Node, error) {
	typeTok := p.advance() // sub-entity type
	if !p.subEntityTypes[strings.ToUpper(typeTok.Image)] {
		return nil, p.errorf(typeTok.Pos, "%s is not a supported nested object type", typeTok.Image)
	}
	entType := p.parseIdentifierToken(typeTok)
	ifExists := p.tryParseIfExists()
	name, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	action, err := p.parseAlterAction()
	if err != nil {
		return nil, err
	}
	return &ast.AlterSubEntityAction{Span: span(alterTok.Pos, action.End()), IsIfExists: ifExists, Type: entType, Name: name, Action: action}, nil
}

// parseAlterRowAccessPolicyStatement parses
// "ALTER ROW ACCESS POLICY [IF EXISTS] name ON path alter_action_list"; see
// alter_row_access_policy_statement in googlesql.tm. The ALTER keyword and the
// ROW keyword lookahead are already consumed by the caller's dispatch.
func (p *parser) parseAlterRowAccessPolicyStatement(alterTok token.Token) (ast.Statement, error) {
	p.advance() // ROW
	if _, err := p.expectKeyword("ACCESS"); err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("POLICY"); err != nil {
		return nil, err
	}
	stmt := &ast.AlterRowAccessPolicyStatement{}
	if isKeyword(p.peek(), "IF") && isKeyword(p.peekAt(1), "EXISTS") {
		p.advance()
		p.advance()
		stmt.IsIfExists = true
	}
	name, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	stmt.Name = name
	if _, err := p.expectKeyword("ON"); err != nil {
		return nil, err
	}
	path, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	stmt.Path = path
	actions, err := p.parseRowAccessPolicyAlterActionList()
	if err != nil {
		return nil, err
	}
	stmt.Actions = actions
	stmt.Span = span(alterTok.Pos, actions.End())
	return stmt, nil
}

// parseAlterAllRowAccessPoliciesStatement parses
// "ALTER ALL ROW ACCESS POLICIES ON path revoke_from_clause"; see
// alter_all_row_access_policies_statement in googlesql.tm. The ALTER keyword
// and the ALL keyword lookahead are already consumed by the caller's dispatch.
func (p *parser) parseAlterAllRowAccessPoliciesStatement(alterTok token.Token) (ast.Statement, error) {
	p.advance() // ALL
	if _, err := p.expectKeyword("ROW"); err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("ACCESS"); err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("POLICIES"); err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("ON"); err != nil {
		return nil, err
	}
	path, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	revoke, err := p.parseRevokeFromClause()
	if err != nil {
		return nil, err
	}
	return &ast.AlterAllRowAccessPoliciesStatement{
		Span:   span(alterTok.Pos, revoke.End()),
		Path:   path,
		Revoke: revoke,
	}, nil
}

// parseRowAccessPolicyAlterActionList parses the comma-separated list of row
// access policy alter actions (RENAME TO, GRANT TO, REVOKE FROM, FILTER USING).
func (p *parser) parseRowAccessPolicyAlterActionList() (*ast.AlterActionList, error) {
	first, err := p.parseRowAccessPolicyAlterAction(true)
	if err != nil {
		return nil, err
	}
	list := &ast.AlterActionList{Span: span(first.Pos(), first.End()), Actions: []ast.Node{first}}
	for p.peek().Kind == token.COMMA {
		p.advance()
		action, err := p.parseRowAccessPolicyAlterAction(false)
		if err != nil {
			return nil, err
		}
		list.Actions = append(list.Actions, action)
		list.Stop = action.End()
	}
	return list, nil
}

// parseRowAccessPolicyAlterAction parses a single row access policy alter
// action; see row_access_policy_alter_action in googlesql.tm. isFirst selects
// the error message used when no valid action keyword follows.
func (p *parser) parseRowAccessPolicyAlterAction(isFirst bool) (ast.Node, error) {
	tok := p.peek()
	switch {
	case isKeyword(tok, "RENAME"):
		renameTok := p.advance()
		if _, err := p.expectKeyword("TO"); err != nil {
			return nil, err
		}
		id, err := p.parseIdentifier()
		if err != nil {
			return nil, err
		}
		newName := &ast.PathExpression{Span: span(id.Pos(), id.End()), Names: []*ast.Identifier{id}}
		return &ast.RenameToClause{Span: span(renameTok.Pos, newName.End()), NewName: newName}, nil
	case isKeyword(tok, "GRANT"):
		return p.parseGrantToClause()
	case isKeyword(tok, "REVOKE"):
		return p.parseRevokeFromClause()
	case isKeyword(tok, "FILTER"):
		return p.parseFilterUsingClause()
	}
	if isFirst {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	return nil, p.errorf(tok.Pos, "Syntax error: Expected keyword FILTER or keyword GRANT or keyword RENAME or keyword REVOKE but got %s", describeToken(tok))
}

// parseGrantToClause parses "GRANT TO (grantee_list)"; see grant_to_clause in
// googlesql.tm.
func (p *parser) parseGrantToClause() (*ast.GrantToClause, error) {
	grantTok := p.advance() // GRANT
	if _, err := p.expectKeyword("TO"); err != nil {
		return nil, err
	}
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	grantees, err := p.parseGranteeList()
	if err != nil {
		return nil, err
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	return &ast.GrantToClause{Span: span(grantTok.Pos, rparen.End), Grantees: grantees}, nil
}

// parseRevokeFromClause parses "REVOKE FROM (grantee_list)" or
// "REVOKE FROM ALL"; see revoke_from_clause in googlesql.tm.
func (p *parser) parseRevokeFromClause() (*ast.RevokeFromClause, error) {
	revokeTok := p.advance() // REVOKE
	if _, err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	if isKeyword(p.peek(), "ALL") {
		allTok := p.advance()
		return &ast.RevokeFromClause{Span: span(revokeTok.Pos, allTok.End), IsRevokeFromAll: true}, nil
	}
	if p.peek().Kind != token.LPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" or keyword ALL but got %s`, describeToken(p.peek()))
	}
	p.advance() // (
	grantees, err := p.parseGranteeList()
	if err != nil {
		return nil, err
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	return &ast.RevokeFromClause{Span: span(revokeTok.Pos, rparen.End), Grantees: grantees}, nil
}

// parseFilterUsingClause parses "FILTER USING (expr)"; see filter_using_clause
// in googlesql.tm.
func (p *parser) parseFilterUsingClause() (*ast.FilterUsingClause, error) {
	// The FILTER keyword is optional; when omitted the clause location starts
	// at USING. See filter_using_clause in googlesql.tm.
	start := p.peek().Pos
	if isKeyword(p.peek(), "FILTER") {
		p.advance()
	}
	if _, err := p.expectKeyword("USING"); err != nil {
		return nil, err
	}
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	return &ast.FilterUsingClause{Span: span(start, rparen.End), Predicate: expr}, nil
}

// parseCreateRowAccessPolicyStatement parses the tail of "CREATE [OR REPLACE]
// ROW [ACCESS] POLICY [IF NOT EXISTS] [name] ON path [grant_to] filter_using";
// see create_row_access_policy_statement in googlesql.tm. The ROW keyword is
// the next token. The optional policy name is a single identifier and is
// emitted last in the debug tree.
func (p *parser) parseCreateRowAccessPolicyStatement(createTok token.Token, isOrReplace bool) (ast.Statement, error) {
	p.advance() // ROW
	if isKeyword(p.peek(), "ACCESS") {
		p.advance()
	}
	if _, err := p.expectKeyword("POLICY"); err != nil {
		return nil, err
	}
	stmt := &ast.CreateRowAccessPolicyStatement{Span: span(createTok.Pos, 0), IsOrReplace: isOrReplace}
	if isKeyword(p.peek(), "IF") && isKeyword(p.peekAt(1), "NOT") && isKeyword(p.peekAt(2), "EXISTS") {
		p.advance()
		p.advance()
		p.advance()
		stmt.IsIfNotExists = true
	}
	// Optional policy name: a single (non-reserved) identifier wrapped in a
	// PathExpression node. A reserved keyword such as ON or TO is not an
	// identifier, so the common no-name form falls through to the ON
	// expectation below. Uses the full reserved-keyword set (keywords.cc)
	// rather than the parser's expression-boundary subset so that reserved
	// words like TO are not mistaken for a policy name.
	var name *ast.PathExpression
	if tok := p.peek(); tok.Kind == token.QUOTED_IDENT || (tok.Kind == token.IDENT && !token.IsReservedKeyword(tok.Image)) {
		ident := p.parseIdentifierToken(p.advance())
		name = &ast.PathExpression{Span: span(ident.Pos(), ident.End()), Names: []*ast.Identifier{ident}}
	}
	if _, err := p.expectKeyword("ON"); err != nil {
		return nil, err
	}
	target, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	stmt.TargetPath = target
	// Optional create_row_access_policy_grant_to_clause: either
	// "GRANT TO (grantee_list)" or the bare "TO grantee_list".
	grantConsumed := false
	switch {
	case isKeyword(p.peek(), "GRANT"):
		gt, err := p.parseGrantToClause()
		if err != nil {
			return nil, err
		}
		stmt.GrantTo = gt
		grantConsumed = true
	case isKeyword(p.peek(), "TO"):
		toTok := p.advance() // TO
		grantees, err := p.parseGranteeList()
		if err != nil {
			return nil, err
		}
		stmt.GrantTo = &ast.GrantToClause{Span: span(toTok.Pos, grantees.End()), Grantees: grantees}
		grantConsumed = true
	}
	// filter_using_clause is mandatory and is introduced by either the optional
	// FILTER keyword or USING. When a grant_to clause was just consumed the only
	// valid continuation is FILTER or USING, so the reference (bison) lists them:
	// "Expected keyword FILTER or keyword USING but got X". Directly after the
	// target path the expected set is large (path continuation, GRANT, TO,
	// FILTER, USING), so bison drops the list and reports "Unexpected X".
	if !isKeyword(p.peek(), "FILTER") && !isKeyword(p.peek(), "USING") {
		if grantConsumed {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword FILTER or keyword USING but got %s", describeToken(p.peek()))
		}
		return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
	}
	fu, err := p.parseFilterUsingClause()
	if err != nil {
		return nil, err
	}
	stmt.FilterUsing = fu
	stmt.Name = name
	stmt.Stop = fu.End()
	return stmt, nil
}

// parseGranteeList parses one or more comma-separated grantees; the opening
// "(" has already been consumed and the caller consumes the closing ")". Each
// grantee is a string literal, a query parameter, or a system variable; see
// grantee_list in googlesql.tm.
func (p *parser) parseGranteeList() (*ast.GranteeList, error) {
	first, err := p.parseGrantee()
	if err != nil {
		return nil, err
	}
	list := &ast.GranteeList{Span: span(first.Pos(), first.End()), Grantees: []ast.Node{first}}
	for p.peek().Kind == token.COMMA {
		p.advance()
		g, err := p.parseGrantee()
		if err != nil {
			return nil, err
		}
		list.Grantees = append(list.Grantees, g)
		list.Stop = g.End()
	}
	return list, nil
}

// parseGrantee parses a single grantee: a string literal, a query parameter,
// or a system variable; see grantee in googlesql.tm.
func (p *parser) parseGrantee() (ast.Node, error) {
	tok := p.peek()
	switch tok.Kind {
	case token.STRING:
		return p.parseStringLiteral()
	case token.PARAM:
		p.advance()
		name := &ast.Identifier{Span: span(tok.Pos+1, tok.End), Name: tok.Image[1:]}
		return &ast.ParameterExpr{Span: span(tok.Pos, tok.End), Name: name}, nil
	case token.SYSTEM_VARIABLE:
		return p.parseSystemVariableExpr()
	}
	return nil, p.errorf(tok.Pos, `Syntax error: Expected "@" or "@@" or string literal but got %s`, describeToken(tok))
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

// tryParseIfExists consumes an "IF EXISTS" clause if present, reporting whether
// it was consumed; see opt_if_exists in googlesql.tm.
func (p *parser) tryParseIfExists() bool {
	if isKeyword(p.peek(), "IF") && isKeyword(p.peekAt(1), "EXISTS") {
		p.advance()
		p.advance()
		return true
	}
	return false
}

// parseOptIfNotExists consumes an "IF NOT EXISTS" clause if present, reporting
// whether it was consumed; see opt_if_not_exists in googlesql.tm. As in the
// bison grammar, seeing the "IF" keyword commits to parsing the whole clause,
// so "IF" must be followed by "NOT" "EXISTS" or a syntax error is reported.
func (p *parser) parseOptIfNotExists() (bool, error) {
	if !isKeyword(p.peek(), "IF") {
		return false, nil
	}
	p.advance() // IF
	if _, err := p.expectKeyword("NOT"); err != nil {
		return false, err
	}
	if _, err := p.expectKeyword("EXISTS"); err != nil {
		return false, err
	}
	return true, nil
}

// parseAlterAction parses a single alter action; see alter_action in
// googlesql.tm.
func (p *parser) parseAlterAction() (ast.Node, error) {
	tok := p.peek()
	switch {
	case isKeyword(tok, "RENAME"):
		return p.parseRenameAlterAction()
	case isKeyword(tok, "SET"):
		return p.parseSetAlterAction()
	case isKeyword(tok, "ADD"):
		return p.parseAddAlterAction()
	case isKeyword(tok, "DROP"):
		return p.parseDropAlterAction()
	case isKeyword(tok, "ALTER"):
		if isKeyword(p.peekAt(1), "COLUMN") {
			return p.parseAlterColumnAction()
		}
		if isSubEntityTypeToken(p.peekAt(1)) {
			alterTok := p.advance() // ALTER
			return p.parseAlterSubEntityAction(alterTok)
		}
		return p.parseAlterConstraintAlterAction()
	case isKeyword(tok, "REPLACE"):
		return p.parseReplaceAlterAction()
	}
	return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
}

// parseRenameAlterAction parses "RENAME TO path" or
// "RENAME COLUMN [IF EXISTS] identifier TO identifier".
func (p *parser) parseRenameAlterAction() (ast.Node, error) {
	renameTok := p.advance() // RENAME
	if isKeyword(p.peek(), "COLUMN") {
		p.advance() // COLUMN
		ifExists := p.tryParseIfExists()
		name, err := p.parseIdentifier()
		if err != nil {
			return nil, err
		}
		if _, err := p.expectKeyword("TO"); err != nil {
			return nil, err
		}
		newName, err := p.parseIdentifier()
		if err != nil {
			return nil, err
		}
		return &ast.RenameColumnAction{Span: span(renameTok.Pos, newName.End()), IsIfExists: ifExists, Name: name, NewName: newName}, nil
	}
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

// parseSetAlterAction parses "SET OPTIONS (...)" or "SET AS
// generic_entity_body"; see the "SET" alter_action productions in
// googlesql.tm.
func (p *parser) parseSetAlterAction() (ast.Node, error) {
	setTok := p.advance() // SET
	next := p.peek()
	switch {
	case isKeyword(next, "OPTIONS"):
		p.advance() // OPTIONS
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		return &ast.SetOptionsAction{Span: span(setTok.Pos, opts.End()), Options: opts}, nil
	case isKeyword(next, "AS"):
		p.advance() // AS
		jsonBody, textBody, err := p.parseGenericEntityBody()
		if err != nil {
			return nil, err
		}
		end := textBody
		if jsonBody != nil {
			end = jsonBody
		}
		return &ast.SetAsAction{Span: span(setTok.Pos, end.End()), JSONBody: jsonBody, TextBody: textBody}, nil
	case isKeyword(next, "ON"):
		// spanner_set_on_delete_action: "SET" "ON" "DELETE" foreign_key_action.
		onTok := p.advance() // ON
		if !p.features.Enabled(FeatureSpannerLegacyDDL) {
			return nil, p.errorf(onTok.Pos, "Syntax error: Unexpected keyword ON")
		}
		if _, err := p.expectKeyword("DELETE"); err != nil {
			return nil, err
		}
		action, actEnd, err := p.parseForeignKeyAction()
		if err != nil {
			return nil, err
		}
		return &ast.SpannerSetOnDeleteAction{Span: span(setTok.Pos, actEnd), Action: action}, nil
	}
	return nil, p.errorf(next.Pos, "Syntax error: Expected keyword AS or keyword DEFAULT or keyword ON or keyword OPTIONS but got %s", describeToken(next))
}

// parseAddAlterAction parses the "ADD ..." alter actions: ADD COLUMN, ADD
// CONSTRAINT, an unnamed table constraint or primary key, and ADD ROW DELETION
// POLICY; see alter_action in googlesql.tm.
func (p *parser) parseAddAlterAction() (ast.Node, error) {
	addTok := p.advance() // ADD
	next := p.peek()
	switch {
	case isKeyword(next, "COLUMN"):
		return p.parseAddColumnAction(addTok)
	case isKeyword(next, "CONSTRAINT"):
		return p.parseAddNamedConstraintAction(addTok)
	case isKeyword(next, "ROW"):
		return p.parseAddTtlAction(addTok)
	case isKeyword(next, "CHECK"), isKeyword(next, "FOREIGN"), (isKeyword(next, "PRIMARY") && isKeyword(p.peekAt(1), "KEY")):
		constraint, err := p.parseConstraintSpec()
		if err != nil {
			return nil, err
		}
		return &ast.AddConstraintAction{Span: span(addTok.Pos, constraint.End()), Constraint: constraint}, nil
	case isSubEntityTypeToken(next):
		return p.parseAddSubEntityAction(addTok)
	}
	return nil, p.errorf(next.Pos, "Syntax error: Unexpected %s", describeToken(next))
}

// parseAddColumnAction parses "COLUMN [IF NOT EXISTS] column_definition
// [column_position] [FILL USING expression]" after ADD; see ASTAddColumnAction.
func (p *parser) parseAddColumnAction(addTok token.Token) (ast.Node, error) {
	p.advance() // COLUMN
	ifNotExists, err := p.parseOptIfNotExists()
	if err != nil {
		return nil, err
	}
	col, err := p.parseColumnDefinition()
	if err != nil {
		return nil, err
	}
	node := &ast.AddColumnAction{Span: span(addTok.Pos, col.End()), IsIfNotExists: ifNotExists, Column: col}
	// opt_column_position
	if isKeyword(p.peek(), "PRECEDING") || isKeyword(p.peek(), "FOLLOWING") {
		posTok := p.advance()
		kind := "PRECEDING"
		if isKeyword(posTok, "FOLLOWING") {
			kind = "FOLLOWING"
		}
		ident, err := p.parseIdentifier()
		if err != nil {
			return nil, err
		}
		node.Position = &ast.ColumnPosition{Span: span(posTok.Pos, ident.End()), Type: kind, Identifier: ident}
		node.Stop = ident.End()
	}
	// opt_fill_using_expression
	if isKeyword(p.peek(), "FILL") {
		p.advance() // FILL
		if _, err := p.expectKeyword("USING"); err != nil {
			return nil, err
		}
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		node.FillExpression = expr
		node.Stop = p.extEnd(expr)
	}
	return node, nil
}

// parseAddNamedConstraintAction parses "CONSTRAINT [IF NOT EXISTS] identifier
// constraint_spec" after ADD; see the "ADD" "CONSTRAINT" production in
// googlesql.tm. The constraint name is attached to the constraint node and the
// constraint's start location is moved to the name.
func (p *parser) parseAddNamedConstraintAction(addTok token.Token) (ast.Node, error) {
	p.advance() // CONSTRAINT
	ifNotExists, err := p.parseOptIfNotExists()
	if err != nil {
		return nil, err
	}
	name, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	constraint, err := p.parseConstraintSpec()
	if err != nil {
		return nil, err
	}
	// ExtendNodeRight($constraint, ..., $name); WithStartLocation(name.start()).
	switch c := constraint.(type) {
	case *ast.CheckConstraint:
		c.ConstraintName = name
		c.Start = name.Pos()
	case *ast.PrimaryKey:
		c.ConstraintName = name
		c.Start = name.Pos()
	case *ast.ForeignKey:
		c.ConstraintName = name
		c.Start = name.Pos()
	}
	return &ast.AddConstraintAction{Span: span(addTok.Pos, constraint.End()), IsIfNotExists: ifNotExists, Constraint: constraint}, nil
}

// parseConstraintSpec parses primary_key_or_table_constraint_spec: a CHECK or
// FOREIGN table constraint, or a PRIMARY KEY spec; see googlesql.tm. FOREIGN
// KEY references are not implemented yet.
func (p *parser) parseConstraintSpec() (ast.Node, error) {
	tok := p.peek()
	switch {
	case isKeyword(tok, "CHECK"):
		return p.parseCheckConstraint()
	case isKeyword(tok, "PRIMARY"):
		return p.parsePrimaryKey()
	case isKeyword(tok, "FOREIGN"):
		return p.parseForeignKey()
	}
	return nil, p.errorf(tok.Pos, "Syntax error: Expected keyword CHECK or keyword FOREIGN or keyword PRIMARY but got %s", describeToken(tok))
}

// parseAddTtlAction parses "ROW DELETION POLICY [IF NOT EXISTS] (expression)"
// after ADD; see the "ADD" "ROW" "DELETION" "POLICY" production in googlesql.tm.
func (p *parser) parseAddTtlAction(addTok token.Token) (ast.Node, error) {
	rowTok := p.advance() // ROW
	if _, err := p.expectKeyword("DELETION"); err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("POLICY"); err != nil {
		return nil, err
	}
	if !p.features.Enabled(FeatureTtl) {
		return nil, p.errorf(rowTok.Pos, "ADD ROW DELETION POLICY clause is not supported.")
	}
	ifNotExists, err := p.parseOptIfNotExists()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	return &ast.AddTtlAction{Span: span(addTok.Pos, rparen.End), IsIfNotExists: ifNotExists, Expression: expr}, nil
}

// parseReplaceAlterAction parses "REPLACE ROW DELETION POLICY [IF EXISTS]
// (expression)"; see the "REPLACE" "ROW" "DELETION" "POLICY" production.
func (p *parser) parseReplaceAlterAction() (ast.Node, error) {
	replaceTok := p.advance() // REPLACE
	rowTok, err := p.expectKeyword("ROW")
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("DELETION"); err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("POLICY"); err != nil {
		return nil, err
	}
	if !p.features.Enabled(FeatureTtl) {
		return nil, p.errorf(rowTok.Pos, "REPLACE ROW DELETION POLICY clause is not supported.")
	}
	ifExists := p.tryParseIfExists()
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	return &ast.ReplaceTtlAction{Span: span(replaceTok.Pos, rparen.End), IsIfExists: ifExists, Expression: expr}, nil
}

// parseDropAlterAction parses the "DROP ..." alter actions: DROP COLUMN, DROP
// CONSTRAINT, DROP PRIMARY KEY, and DROP ROW DELETION POLICY; see alter_action.
func (p *parser) parseDropAlterAction() (ast.Node, error) {
	dropTok := p.advance() // DROP
	next := p.peek()
	switch {
	case isKeyword(next, "COLUMN"):
		p.advance() // COLUMN
		ifExists := p.tryParseIfExists()
		name, err := p.parseIdentifier()
		if err != nil {
			return nil, err
		}
		return &ast.DropColumnAction{Span: span(dropTok.Pos, name.End()), IsIfExists: ifExists, Name: name}, nil
	case isKeyword(next, "CONSTRAINT"):
		p.advance() // CONSTRAINT
		ifExists := p.tryParseIfExists()
		name, err := p.parseIdentifier()
		if err != nil {
			return nil, err
		}
		return &ast.DropConstraintAction{Span: span(dropTok.Pos, name.End()), IsIfExists: ifExists, Name: name}, nil
	case isKeyword(next, "PRIMARY") && isKeyword(p.peekAt(1), "KEY"):
		p.advance() // PRIMARY
		keyTok := p.advance()
		ifExists := p.tryParseIfExists()
		end := keyTok.End
		if ifExists {
			end = p.prevEnd()
		}
		return &ast.DropPrimaryKeyAction{Span: span(dropTok.Pos, end), IsIfExists: ifExists}, nil
	case isKeyword(next, "ROW"):
		rowTok := p.advance() // ROW
		if _, err := p.expectKeyword("DELETION"); err != nil {
			return nil, err
		}
		policyTok, err := p.expectKeyword("POLICY")
		if err != nil {
			return nil, err
		}
		if !p.features.Enabled(FeatureTtl) {
			return nil, p.errorf(rowTok.Pos, "DROP ROW DELETION POLICY clause is not supported.")
		}
		ifExists := p.tryParseIfExists()
		end := policyTok.End
		if ifExists {
			end = p.prevEnd()
		}
		return &ast.DropTtlAction{Span: span(dropTok.Pos, end), IsIfExists: ifExists}, nil
	case isSubEntityTypeToken(next):
		return p.parseDropSubEntityAction(dropTok)
	}
	return nil, p.errorf(next.Pos, "Syntax error: Unexpected %s", describeToken(next))
}

// parseAlterConstraintAlterAction parses "ALTER CONSTRAINT [IF EXISTS]
// identifier {ENFORCED|NOT ENFORCED | SET OPTIONS (...)}"; see the "ALTER"
// "CONSTRAINT" productions in googlesql.tm.
func (p *parser) parseAlterConstraintAlterAction() (ast.Node, error) {
	alterTok := p.advance() // ALTER
	if !isKeyword(p.peek(), "CONSTRAINT") {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
	}
	p.advance() // CONSTRAINT
	ifExists := p.tryParseIfExists()
	name, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	next := p.peek()
	switch {
	case isKeyword(next, "ENFORCED"):
		endTok := p.advance()
		return &ast.AlterConstraintEnforcementAction{Span: span(alterTok.Pos, endTok.End), IsIfExists: ifExists, IsEnforced: true, Name: name}, nil
	case isKeyword(next, "NOT"):
		p.advance() // NOT
		endTok, err := p.expectKeyword("ENFORCED")
		if err != nil {
			return nil, err
		}
		return &ast.AlterConstraintEnforcementAction{Span: span(alterTok.Pos, endTok.End), IsIfExists: ifExists, IsEnforced: false, Name: name}, nil
	case isKeyword(next, "SET"):
		p.advance() // SET
		if _, err := p.expectKeyword("OPTIONS"); err != nil {
			return nil, err
		}
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		return &ast.AlterConstraintSetOptionsAction{Span: span(alterTok.Pos, opts.End()), IsIfExists: ifExists, Name: name, Options: opts}, nil
	}
	return nil, p.errorf(next.Pos, "Syntax error: Expected keyword ENFORCED or keyword NOT or keyword SET but got %s", describeToken(next))
}

// parseAlterColumnAction parses the "ALTER COLUMN [IF EXISTS] identifier ..."
// alter actions: SET DEFAULT, DROP DEFAULT, SET GENERATED, and DROP GENERATED;
// see the "ALTER" "COLUMN" productions in googlesql.tm. ALTER is the next
// token.
func (p *parser) parseAlterColumnAction() (ast.Node, error) {
	alterTok := p.advance() // ALTER
	p.advance()             // COLUMN
	ifPos := p.peek().Pos
	ifExists, err := p.parseOptIfExists()
	if err != nil {
		return nil, err
	}
	name, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	next := p.peek()
	// spanner_alter_column_action: "ALTER" "COLUMN" opt_if_exists identifier
	// column_schema_inner ...; when the token after the column name is neither
	// SET nor DROP, this is the Spanner form (an inline column redefinition).
	if !isKeyword(next, "SET") && !isKeyword(next, "DROP") {
		return p.parseSpannerAlterColumnAction(alterTok, ifPos, ifExists, name)
	}
	switch {
	case isKeyword(next, "SET"):
		p.advance() // SET
		sub := p.peek()
		switch {
		case isKeyword(sub, "DEFAULT"):
			p.advance() // DEFAULT
			expr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			return &ast.AlterColumnSetDefaultAction{Span: span(alterTok.Pos, p.extEnd(expr)), IsIfExists: ifExists, Column: name, DefaultExpression: expr}, nil
		case isKeyword(sub, "GENERATED"):
			p.advance() // GENERATED
			info, err := p.parseGeneratedColumnInfoForAlter()
			if err != nil {
				return nil, err
			}
			return &ast.AlterColumnSetGeneratedAction{Span: span(alterTok.Pos, info.End()), IsIfExists: ifExists, Column: name, Info: info}, nil
		case isKeyword(sub, "DATA"):
			p.advance() // DATA
			if _, err := p.expectKeyword("TYPE"); err != nil {
				return nil, err
			}
			schema, err := p.parseFieldSchema()
			if err != nil {
				return nil, err
			}
			return &ast.AlterColumnTypeAction{Span: span(alterTok.Pos, schema.End()), IsIfExists: ifExists, Column: name, Schema: schema}, nil
		case isKeyword(sub, "OPTIONS"):
			p.advance() // OPTIONS
			opts, err := p.parseOptionsList()
			if err != nil {
				return nil, err
			}
			return &ast.AlterColumnOptionsAction{Span: span(alterTok.Pos, opts.End()), IsIfExists: ifExists, Column: name, Options: opts}, nil
		}
		return nil, p.errorf(sub.Pos, "Syntax error: Expected keyword DATA or keyword DEFAULT or keyword GENERATED or keyword OPTIONS but got %s", describeToken(sub))
	case isKeyword(next, "DROP"):
		p.advance() // DROP
		sub := p.peek()
		switch {
		case isKeyword(sub, "DEFAULT"):
			end := p.advance().End
			return &ast.AlterColumnDropDefaultAction{Span: span(alterTok.Pos, end), IsIfExists: ifExists, Column: name}, nil
		case isKeyword(sub, "GENERATED"):
			end := p.advance().End
			return &ast.AlterColumnDropGeneratedAction{Span: span(alterTok.Pos, end), IsIfExists: ifExists, Column: name}, nil
		case isKeyword(sub, "NOT"):
			p.advance() // NOT
			nullTok, err := p.expectKeyword("NULL")
			if err != nil {
				return nil, err
			}
			return &ast.AlterColumnDropNotNullAction{Span: span(alterTok.Pos, nullTok.End), IsIfExists: ifExists, Column: name}, nil
		}
		return nil, p.errorf(sub.Pos, "Syntax error: Expected keyword DEFAULT or keyword GENERATED or keyword NOT but got %s", describeToken(sub))
	}
	// Unreachable: the SET/DROP dispatch above is exhaustive because the
	// non-SET/non-DROP case is handled by the Spanner form earlier.
	return nil, p.errorf(next.Pos, "Syntax error: Expected keyword DROP or keyword SET but got %s", describeToken(next))
}

// parseSpannerAlterColumnAction parses the tail of spanner_alter_column_action:
// "column_schema_inner [NOT NULL] [AS (expr) STORED | DEFAULT expr]
// [OPTIONS(...)]" after "ALTER" "COLUMN" opt_if_exists identifier; see
// spanner_alter_column_action in googlesql.tm. Requires
// FEATURE_SPANNER_LEGACY_DDL and forbids IF EXISTS. alterTok is the ALTER
// token, ifPos the location of a consumed IF EXISTS clause, and name the
// column identifier.
func (p *parser) parseSpannerAlterColumnAction(alterTok token.Token, ifPos int, ifExists bool, name *ast.Identifier) (ast.Node, error) {
	schemaStart := p.peek().Pos
	schema, err := p.parseColumnSchemaInner()
	if err != nil {
		return nil, err
	}
	end := schema.End()
	// spanner_not_null_attribute?
	var attrs *ast.ColumnAttributeList
	if isKeyword(p.peek(), "NOT") && isKeyword(p.peekAt(1), "NULL") {
		notTok := p.advance()  // NOT
		nullTok := p.advance() // NULL
		attr := &ast.NotNullColumnAttribute{Span: span(notTok.Pos, nullTok.End)}
		attrs = &ast.ColumnAttributeList{Span: span(notTok.Pos, nullTok.End), Attributes: []ast.Node{attr}}
		end = nullTok.End
	}
	// spanner_generated_or_default?
	var genOrDefault ast.Node
	switch {
	case isKeyword(p.peek(), "AS"):
		asTok := p.advance() // AS
		if _, err := p.expect(token.LPAREN, `"("`); err != nil {
			return nil, err
		}
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(token.RPAREN, `")"`); err != nil {
			return nil, err
		}
		storedTok, err := p.expectKeyword("STORED")
		if err != nil {
			return nil, err
		}
		genOrDefault = &ast.GeneratedColumnInfo{Span: span(asTok.Pos, storedTok.End), GeneratedMode: "ALWAYS", StoredMode: "STORED", Expression: expr}
		end = storedTok.End
	case isKeyword(p.peek(), "DEFAULT"):
		p.advance() // DEFAULT
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		genOrDefault = expr
		end = p.extEnd(expr)
	}
	// opt_options_list
	var options *ast.OptionsList
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		options = opts
		end = opts.End()
	}
	// Feature and IF EXISTS checks happen after the schema is fully parsed, as
	// in the reference grammar action.
	if !p.features.Enabled(FeatureSpannerLegacyDDL) {
		return nil, p.errorf(schemaStart, "Expected keyword DROP or keyword SET but got identifier")
	}
	if ifExists {
		return nil, p.errorf(ifPos, "Syntax error: IF EXISTS is not supported")
	}
	// ExtendNodeRight(column_schema_inner, end, generated_or_default,
	// not_null_attribute, options): the schema debug order is default/generated
	// before attributes before options.
	if s, ok := schema.(*ast.SimpleColumnSchema); ok {
		s.DefaultExpression = genOrDefault
		s.Attributes = attrs
		s.Options = options
		s.Stop = end
	} else {
		setColumnSchemaTail(schema, nil, attrs, options)
	}
	col := &ast.ColumnDefinition{Span: span(name.Pos(), end), Name: name, Schema: schema}
	return &ast.SpannerAlterColumnAction{Span: span(alterTok.Pos, end), Column: col}, nil
}

// parseGeneratedColumnInfoForAlter parses
// generated_column_info_for_alter_column_action: an optional generated mode
// ("ALWAYS AS" / "AS" / "BY DEFAULT AS") followed by an identity column body;
// see generated_column_info_for_alter_column_action in googlesql.tm. GENERATED
// has already been consumed.
func (p *parser) parseGeneratedColumnInfoForAlter() (*ast.GeneratedColumnInfo, error) {
	// generated_mode_for_alter_column_action: "ALWAYS"? "AS" | "BY" "DEFAULT" "AS"
	start := p.peek().Pos
	mode := "ALWAYS"
	switch {
	case isKeyword(p.peek(), "ALWAYS"):
		p.advance() // ALWAYS
		if _, err := p.expectKeyword("AS"); err != nil {
			return nil, err
		}
	case isKeyword(p.peek(), "BY"):
		p.advance() // BY
		if _, err := p.expectKeyword("DEFAULT"); err != nil {
			return nil, err
		}
		if _, err := p.expectKeyword("AS"); err != nil {
			return nil, err
		}
		mode = "BY_DEFAULT"
	case isKeyword(p.peek(), "AS"):
		p.advance() // AS
	default:
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword ALWAYS or keyword AS or keyword BY but got %s", describeToken(p.peek()))
	}
	identity, err := p.parseIdentityColumnInfo()
	if err != nil {
		return nil, err
	}
	return &ast.GeneratedColumnInfo{Span: span(start, identity.End()), GeneratedMode: mode, Identity: identity}, nil
}

// parseIdentityColumnInfo parses "IDENTITY ( [START WITH n] [INCREMENT BY n]
// [MAXVALUE n] [MINVALUE n] [CYCLE|NO CYCLE] )"; see identity_column_info in
// googlesql.tm.
func (p *parser) parseIdentityColumnInfo() (*ast.IdentityColumnInfo, error) {
	identityTok, err := p.expectKeyword("IDENTITY")
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	info := &ast.IdentityColumnInfo{Span: span(identityTok.Pos, 0)}
	if isKeyword(p.peek(), "START") {
		startTok := p.advance() // START
		if _, err := p.expectKeyword("WITH"); err != nil {
			return nil, err
		}
		value, err := p.parseSignedNumericalLiteral()
		if err != nil {
			return nil, err
		}
		info.StartWith = &ast.IdentityColumnStartWith{Span: span(startTok.Pos, p.extEnd(value)), Value: value}
	}
	if isKeyword(p.peek(), "INCREMENT") {
		incTok := p.advance() // INCREMENT
		if _, err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		value, err := p.parseSignedNumericalLiteral()
		if err != nil {
			return nil, err
		}
		info.IncrementBy = &ast.IdentityColumnIncrementBy{Span: span(incTok.Pos, p.extEnd(value)), Value: value}
	}
	if isKeyword(p.peek(), "MAXVALUE") {
		maxTok := p.advance() // MAXVALUE
		value, err := p.parseSignedNumericalLiteral()
		if err != nil {
			return nil, err
		}
		info.MaxValue = &ast.IdentityColumnMaxValue{Span: span(maxTok.Pos, p.extEnd(value)), Value: value}
	}
	if isKeyword(p.peek(), "MINVALUE") {
		minTok := p.advance() // MINVALUE
		value, err := p.parseSignedNumericalLiteral()
		if err != nil {
			return nil, err
		}
		info.MinValue = &ast.IdentityColumnMinValue{Span: span(minTok.Pos, p.extEnd(value)), Value: value}
	}
	// opt_cycle: "CYCLE" | "NO" "CYCLE" | empty. The flag is not represented in
	// the debug output, so it is parsed and discarded.
	if isKeyword(p.peek(), "CYCLE") {
		p.advance()
	} else if isKeyword(p.peek(), "NO") && isKeyword(p.peekAt(1), "CYCLE") {
		p.advance()
		p.advance()
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	info.Stop = rparen.End
	return info, nil
}

// parseSignedNumericalLiteral parses signed_numerical_literal: an integer,
// floating point, NUMERIC/DECIMAL or BIGNUMERIC/BIGDECIMAL literal, optionally
// negated (only integers and floats may carry a leading "-"); see
// signed_numerical_literal in googlesql.tm.
func (p *parser) parseSignedNumericalLiteral() (ast.Node, error) {
	if p.peek().Kind == token.MINUS {
		minusTok := p.advance() // -
		tok := p.peek()
		switch tok.Kind {
		case token.INT:
			p.advance()
			inner := &ast.IntLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image}
			return &ast.UnaryExpression{Span: span(minusTok.Pos, tok.End), Op: "-", Operand: inner}, nil
		case token.FLOAT:
			p.advance()
			inner := &ast.FloatLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image}
			return &ast.UnaryExpression{Span: span(minusTok.Pos, tok.End), Op: "-", Operand: inner}, nil
		}
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	tok := p.peek()
	switch {
	case tok.Kind == token.INT:
		p.advance()
		return &ast.IntLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image}, nil
	case tok.Kind == token.FLOAT:
		p.advance()
		return &ast.FloatLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image}, nil
	case tok.Kind == token.IDENT && isNumericTypedLiteralPrefix(tok.Image) && p.peekAt(1).Kind == token.STRING:
		return p.parseTypedLiteral()
	}
	return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
}

// isNumericTypedLiteralPrefix reports whether image introduces a NUMERIC or
// BIGNUMERIC typed literal usable as a signed_numerical_literal.
func isNumericTypedLiteralPrefix(image string) bool {
	switch strings.ToUpper(image) {
	case "NUMERIC", "DECIMAL", "BIGNUMERIC", "BIGDECIMAL":
		return true
	}
	return false
}

// parseOptIfExists consumes an "IF EXISTS" clause if present, reporting whether
// it was consumed; see opt_if_exists in googlesql.tm. As in the bison grammar,
// seeing "IF" commits to parsing "IF" "EXISTS", so "IF" must be followed by
// "EXISTS" or a syntax error is reported.
func (p *parser) parseOptIfExists() (bool, error) {
	if !isKeyword(p.peek(), "IF") {
		return false, nil
	}
	p.advance() // IF
	if _, err := p.expectKeyword("EXISTS"); err != nil {
		return false, err
	}
	return true, nil
}

// parseOptionsList parses "( [options_entry, ...] )".
func (p *parser) parseOptionsList() (*ast.OptionsList, error) {
	lparen, err := p.expect(token.LPAREN, `"("`)
	if err != nil {
		return nil, err
	}
	list := &ast.OptionsList{Span: span(lparen.Pos, 0)}
	if p.peek().Kind != token.RPAREN {
		// An options entry must begin with an identifier (the option name). If
		// it cannot, the list can only be closed, so the reference reports the
		// missing ")"; see options_list in googlesql.tm.
		if !optionsNameOK(p.peek()) {
			return nil, p.errorf(p.peek().Pos, `Syntax error: Expected ")" but got %s`, describeToken(p.peek()))
		}
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
	if !optionsNameOK(tok) {
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
		// The grammar always accepts "=", "+=", and "-=". When the
		// ENABLE_ALTER_ARRAY_OPTIONS feature is off, the reference drops the
		// "+=" and "-=" from the expected-token set in the error message; see
		// the expectations_set.erase calls in parser_internal.cc.
		if p.features.Enabled(FeatureEnableAlterArrayOptions) {
			return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "+=" or "-=" or "=" but got %s`, describeToken(p.peek()))
		}
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "=" but got %s`, describeToken(p.peek()))
	}
	// expression_or_proto in googlesql.tm: the value may be the bare reserved
	// keyword PROTO, which is turned into a path expression naming "PROTO"
	// (reserved keywords are otherwise not valid expressions).
	if isKeyword(p.peek(), "PROTO") {
		protoTok := p.advance()
		ident := &ast.Identifier{Span: span(protoTok.Pos, protoTok.End), Name: "PROTO"}
		path := &ast.PathExpression{Span: span(protoTok.Pos, protoTok.End), Names: []*ast.Identifier{ident}}
		return &ast.OptionsEntry{Span: span(name.Pos(), path.End()), Name: name, Op: op, Value: path}, nil
	}
	value, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	return &ast.OptionsEntry{Span: span(name.Pos(), value.End()), Name: name, Op: op, Value: value}, nil
}

// hasLockMode reports whether any node in the subtree is a LockMode clause,
// mirroring HasLockMode in parser_internal.h. Queries used in DDL statements
// (and function bodies) must not contain lock modes, to avoid unintentionally
// acquiring locks when executing them.
func hasLockMode(n ast.Node) bool {
	if n == nil {
		return false
	}
	if _, ok := n.(*ast.LockMode); ok {
		return true
	}
	for _, c := range n.Children() {
		if hasLockMode(c) {
			return true
		}
	}
	return false
}

// parseQueryAfterAs parses the query in a DDL/EXPORT "AS" position, which
// accepts either an ordinary query or a graph query; see query_after_as
// (query | gql_query) in googlesql.tm. The gql_query alternative is part of
// the grammar independent of the SQL_GRAPH language feature (feature gating
// happens in the analyzer), so a leading GRAPH keyword always selects it.
func (p *parser) parseQueryAfterAs() (*ast.Query, error) {
	if isKeyword(p.peek(), "GRAPH") {
		return p.parseGraphQuery()
	}
	return p.parseQuery()
}

// parseQuery parses "[WITH ...] query_primary [ORDER BY] [LIMIT] [FOR
// UPDATE]" followed by any pipe operators; see query and
// query_without_pipe_operators in googlesql.tm.
func (p *parser) parseQuery() (*ast.Query, error) {
	// A nested query (subquery, parenthesized query, etc.) is a fresh context:
	// its own FROM clause is not the standalone FROM query's, so QUALIFY there
	// is handled normally. Restore on exit so an enclosing FROM query's flag is
	// preserved.
	savedInFromQuery := p.inFromQuery
	p.inFromQuery = false
	defer func() { p.inFromQuery = savedInFromQuery }()
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
		// A standalone FROM query is only valid with pipe syntax enabled; see
		// the from_query alternative of query_primary_or_from_query in
		// googlesql.tm. Without it, FROM is not a valid query start. The
		// reference still parses the FROM clause first, so a syntax error
		// inside the FROM leaks out ahead of the "Unexpected FROM" error.
		fromQuery, err := p.parseFromQueryTail(start, with)
		if err != nil {
			return nil, err
		}
		if !p.features.Enabled(FeaturePipes) {
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected FROM")
		}
		return fromQuery, nil
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
		// "( query ) AS alias" is only valid with pipes; see the
		// parenthesized_query alternative of query_primary in googlesql.tm.
		// With pipes it becomes an AliasedQueryExpression; without pipes the AS
		// is an error.
		if isKeyword(p.peek(), "AS") {
			if !p.features.Enabled(FeaturePipes) {
				return nil, p.errorf(p.peek().Pos, "Syntax error: Alias not allowed on parenthesized outer query")
			}
			alias, err := p.parseRequiredAsAlias()
			if err != nil {
				return nil, err
			}
			aliased := &ast.AliasedQueryExpression{Span: span(tok.Pos, alias.End()), Query: inner, Alias: alias}
			primary, primaryEnd = aliased, aliased.End()
		} else {
			primary, primaryEnd = inner, parenEnd
		}
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
		if !p.features.Enabled(FeatureForUpdate) {
			return nil, p.errorf(forTok.Pos, "FOR UPDATE is not supported")
		}
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
		if entry.AliasedQuery != nil && isKeyword(p.peek(), "WITH") && !isKeyword(p.peekAt(1), "DEPTH") {
			// After an aliased query, WITH can only start a recursion depth
			// modifier; see recursion_depth_modifier in googlesql.tm.
			return nil, p.errorf(p.peekAt(1).Pos, "Syntax error: Expected keyword DEPTH but got %s", describeToken(p.peekAt(1)))
		}
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance() // ,
		next := p.peek()
		if isKeyword(next, "SELECT") || isKeyword(next, "FROM") {
			// See with_clause_with_trailing_comma in googlesql.tm.
			return nil, p.errorf(next.Pos, "Syntax error: Trailing comma after the WITH clause before the main query is not allowed")
		}
		if (next.Kind != token.IDENT && next.Kind != token.QUOTED_IDENT) || p.isReserved(next) {
			// Only another WITH entry (starting with an identifier) or the
			// main query may follow the comma; the reference LALR state
			// reports only SELECT here.
			return nil, p.errorf(next.Pos, "Syntax error: Expected keyword SELECT but got %s", describeToken(next))
		}
	}
	if p.peek().Kind == token.PIPE_INPUT {
		if !p.features.Enabled(FeaturePipes) {
			// With pipes disabled, the reference lookahead transformer does
			// not fuse "|" ">" into a "|>" token, so the query is followed by
			// a stray bitwise-or operator; report it as an unexpected "|".
			return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected |")
		}
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
	if p.isReserved(tok) {
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
	if !isKeyword(p.peek(), "AS") {
		// Both "identifier ( ) AS GROUP ROWS" and "identifier AS ( query )"
		// are possible here; see with_clause_entry in googlesql.tm.
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" or keyword AS but got %s`, describeToken(p.peek()))
	}
	p.advance() // AS
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
	if isKeyword(p.peek(), "WITH") && isKeyword(p.peekAt(1), "DEPTH") {
		mod, err := p.parseRecursionDepthModifier()
		if err != nil {
			return nil, err
		}
		aq.Modifiers = &ast.AliasedQueryModifiers{Span: mod.Span, RecursionDepth: mod}
		aq.Stop = mod.End()
	}
	return &ast.WithClauseEntry{Span: aq.Span, AliasedQuery: aq}, nil
}

// parseRecursionDepthModifier parses "WITH DEPTH [AS alias] [BETWEEN lo AND hi
// | MAX hi]"; the WITH keyword is the next token (and is followed by DEPTH).
// See recursion_depth_modifier in googlesql.tm. When no BETWEEN/MAX bound is
// given, the bounds default to unbounded, located at the end of the modifier.
func (p *parser) parseRecursionDepthModifier() (*ast.RecursionDepthModifier, error) {
	withTok := p.advance() // WITH
	depthTok, err := p.expectKeyword("DEPTH")
	if err != nil {
		return nil, err
	}
	mod := &ast.RecursionDepthModifier{}
	// as_alias_with_required_as: an alias here must be introduced by AS.
	endOfPrefix := depthTok.End
	if isKeyword(p.peek(), "AS") {
		alias, err := p.parseOptionalAlias()
		if err != nil {
			return nil, err
		}
		mod.Alias = alias
		endOfPrefix = alias.End()
	}
	switch {
	case isKeyword(p.peek(), "BETWEEN"):
		p.advance() // BETWEEN
		lo, err := p.parsePossiblyUnboundedIntOrParameter()
		if err != nil {
			return nil, err
		}
		if _, err := p.expectKeyword("AND"); err != nil {
			return nil, err
		}
		hi, err := p.parsePossiblyUnboundedIntOrParameter()
		if err != nil {
			return nil, err
		}
		mod.LowerBound = lo
		mod.UpperBound = hi
		mod.Span = span(withTok.Pos, hi.End())
	case isKeyword(p.peek(), "MAX"):
		p.advance() // MAX
		hi, err := p.parsePossiblyUnboundedIntOrParameter()
		if err != nil {
			return nil, err
		}
		mod.LowerBound = &ast.IntOrUnbounded{Span: span(endOfPrefix, endOfPrefix)}
		mod.UpperBound = hi
		mod.Span = span(withTok.Pos, hi.End())
	default:
		mod.Span = span(withTok.Pos, endOfPrefix)
		mod.LowerBound = &ast.IntOrUnbounded{Span: span(endOfPrefix, endOfPrefix)}
		mod.UpperBound = &ast.IntOrUnbounded{Span: span(endOfPrefix, endOfPrefix)}
	}
	return mod, nil
}

// parsePossiblyUnboundedIntOrParameter parses "UNBOUNDED" or an integer
// literal / parameter into an IntOrUnbounded node; see
// possibly_unbounded_int_literal_or_parameter in googlesql.tm.
func (p *parser) parsePossiblyUnboundedIntOrParameter() (*ast.IntOrUnbounded, error) {
	tok := p.peek()
	if isKeyword(tok, "UNBOUNDED") {
		p.advance()
		return &ast.IntOrUnbounded{Span: span(tok.Pos, tok.End)}, nil
	}
	// In this context the reference state expects UNBOUNDED, an integer
	// literal, a parameter, or a system variable; any other token yields a
	// plain "Unexpected <token>" error (there is no single expected keyword to
	// name). See possibly_unbounded_int_literal_or_parameter in googlesql.tm.
	switch tok.Kind {
	case token.INT, token.PARAM, token.QUESTION, token.SYSTEM_VARIABLE:
	default:
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	val, err := p.parseIntLiteralOrParameter()
	if err != nil {
		return nil, err
	}
	return &ast.IntOrUnbounded{Span: span(val.Pos(), val.End()), Bound: val}, nil
}

// parsePipeWith parses "WITH [RECURSIVE] with_entry [, ...] [,]" after a |>
// token; see pipe_with in googlesql.tm. Unlike the WITH clause before a main
// query, a trailing comma is allowed and is included in the operator's span.
func (p *parser) parsePipeWith(pipeTok token.Token) (ast.Node, error) {
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
		// Continue to another entry only when the comma is followed by an entry
		// start; otherwise the comma is pipe WITH's optional trailing comma.
		if next := p.peekAt(1); (next.Kind != token.IDENT && next.Kind != token.QUOTED_IDENT) || p.isReserved(next) {
			break
		}
		p.advance() // ,
	}
	node := &ast.PipeWith{Span: span(pipeTok.Pos, wc.End()), With: wc}
	if p.peek().Kind == token.COMMA {
		comma := p.advance()
		node.Stop = comma.End
	}
	return node, nil
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
	savedInFromQuery := p.inFromQuery
	p.inFromQuery = true
	from, err := p.parseFromClause()
	p.inFromQuery = savedInFromQuery
	if err != nil {
		return nil, err
	}
	tok := p.peek()
	// These dedicated "not supported after FROM query" errors suggest a pipe
	// operator, so they only apply with pipe syntax enabled. Without pipes, a
	// standalone FROM query is invalid outright: the caller rejects the FROM
	// itself once its clause has parsed, so we leave any trailing keyword here.
	if p.features.Enabled(FeaturePipes) {
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
	}
	fromQuery := &ast.FromQuery{Span: span(from.Pos(), from.End()), From: from}
	query := &ast.Query{Span: span(start, fromQuery.End()), WithClause: with, QueryExpr: fromQuery}
	if isKeyword(p.peek(), "FOR") && isKeyword(p.peekAt(1), "UPDATE") {
		forTok := p.advance()    // FOR
		updateTok := p.advance() // UPDATE
		if !p.features.Enabled(FeatureForUpdate) {
			return nil, p.errorf(forTok.Pos, "FOR UPDATE is not supported")
		}
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
		if p.isReserved(tok) {
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
		// "( query ) AS alias" is only valid with pipes; see the
		// parenthesized_query alternative of query_primary in googlesql.tm.
		// With pipes it becomes an AliasedQueryExpression; without pipes the AS
		// is an error.
		if isKeyword(p.peek(), "AS") {
			if !p.features.Enabled(FeaturePipes) {
				return nil, p.errorf(p.peek().Pos, "Syntax error: Alias not allowed on parenthesized outer query")
			}
			alias, err := p.parseRequiredAsAlias()
			if err != nil {
				return nil, err
			}
			return &ast.AliasedQueryExpression{Span: span(tok.Pos, alias.End()), Query: query, Alias: alias}, nil
		}
		return query, nil
	}
	return nil, p.errorf(tok.Pos, `Syntax error: Expected "(" or keyword SELECT or keyword TABLE but got %s`, describeToken(tok))
}

// parseTableClause parses "TABLE path"; see table_clause in googlesql.tm.
// Table-valued function calls after TABLE are not implemented yet.
func (p *parser) parseTableClause() (*ast.TableClause, error) {
	tableTok := p.advance() // TABLE
	body, err := p.parseTableClauseNoKeyword()
	if err != nil {
		return nil, err
	}
	return &ast.TableClause{Span: span(tableTok.Pos, body.End()), Table: body}, nil
}

// parseTableClauseNoKeyword parses the operand after the TABLE keyword of a
// table clause: either a path expression (TABLE path) or a table-valued
// function call (TABLE path(args...)); see table_clause_no_keyword in
// googlesql.tm.
func (p *parser) parseTableClauseNoKeyword() (ast.Node, error) {
	path, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind == token.LPAREN {
		return p.parseTVFRest(path)
	}
	return path, nil
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
	case isKeyword(tok, "GROUP"):
		// A pipe GROUP BY is an error; GROUP BY belongs to a pipe AGGREGATE
		// operator. See pipe_group_by in googlesql.tm.
		return nil, p.errorf(tok.Pos, "Syntax error: GROUP BY should be part of a pipe AGGREGATE operator, without a leading pipe symbol")
	case isKeyword(tok, "SELECT"):
		sel, err := p.parseSelectClause()
		if err != nil {
			return nil, err
		}
		if isKeyword(p.peek(), "WINDOW") {
			window, err := p.parseWindowClause(true)
			if err != nil {
				return nil, err
			}
			sel.Window = window
			sel.Stop = window.End()
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
	case isKeyword(tok, "AS"):
		return p.parsePipeAs(pipeTok)
	case isKeyword(tok, "RENAME"):
		return p.parsePipeRename(pipeTok)
	case isKeyword(tok, "DROP"):
		return p.parsePipeDrop(pipeTok)
	case isKeyword(tok, "ASSERT"):
		return p.parsePipeAssert(pipeTok)
	case isKeyword(tok, "DESCRIBE"):
		descTok := p.advance()
		return &ast.PipeDescribe{Span: span(pipeTok.Pos, descTok.End)}, nil
	case isKeyword(tok, "STATIC_DESCRIBE"):
		descTok := p.advance()
		return &ast.PipeStaticDescribe{Span: span(pipeTok.Pos, descTok.End)}, nil
	case isKeyword(tok, "MATCH_RECOGNIZE"):
		clause, err := p.parseMatchRecognizeClause()
		if err != nil {
			return nil, err
		}
		return &ast.PipeMatchRecognize{Span: span(pipeTok.Pos, clause.End()), Clause: clause}, nil
	case isKeyword(tok, "TABLESAMPLE"):
		clause, err := p.parseSampleClause(false)
		if err != nil {
			return nil, err
		}
		return &ast.PipeTablesample{Span: span(pipeTok.Pos, clause.End()), Sample: clause}, nil
	case isKeyword(tok, "PIVOT"):
		return p.parsePipePivot(pipeTok)
	case isKeyword(tok, "UNPIVOT"):
		return p.parsePipeUnpivot(pipeTok)
	case isKeyword(tok, "EXPORT"):
		return p.parsePipeExportData(pipeTok)
	case isKeyword(tok, "CREATE"):
		return p.parsePipeCreateTable(pipeTok)
	case isKeyword(tok, "CALL"):
		return p.parsePipeCall(pipeTok)
	case isKeyword(tok, "WITH"):
		return p.parsePipeWith(pipeTok)
	case isKeyword(tok, "INSERT"):
		return p.parsePipeInsert(pipeTok)
	case isKeyword(tok, "IF"):
		return p.parsePipeIf(pipeTok)
	case isKeyword(tok, "ELSEIF"):
		// A pipe ELSEIF is an error; ELSEIF belongs to a pipe IF operator.
		// See pipe_elseif in googlesql.tm.
		return nil, p.errorf(tok.Pos, "Syntax error: ELSEIF should be part of a pipe IF, without a leading pipe symbol")
	case isKeyword(tok, "ELSE"):
		// A pipe ELSE is an error; ELSE belongs to a pipe IF operator.
		// See pipe_else in googlesql.tm.
		return nil, p.errorf(tok.Pos, "Syntax error: ELSE should be part of a pipe IF, without a leading pipe symbol")
	case isKeyword(tok, "FORK"):
		return p.parsePipeFork(pipeTok)
	case isKeyword(tok, "TEE"):
		return p.parsePipeTee(pipeTok)
	case isKeyword(tok, "RECURSIVE"):
		return p.parsePipeRecursiveUnion(pipeTok)
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

// parsePipeInsert parses "INSERT [mode] [INTO] target [hint] [column_list]
// [ON CONFLICT ...] [ASSERT_ROWS_MODIFIED ...] [THEN RETURN ...]" after a |>
// token; see pipe_insert and insert_statement_in_pipe in googlesql.tm. Unlike
// a standalone INSERT, there is no VALUES or query source: the rows come from
// the pipe input.
func (p *parser) parsePipeInsert(pipeTok token.Token) (ast.Node, error) {
	insertTok := p.advance() // INSERT
	stmt := &ast.InsertStatement{Span: span(insertTok.Pos, insertTok.End)}
	if isKeyword(p.peek(), "OR") {
		p.advance()
	}
	switch {
	case isKeyword(p.peek(), "IGNORE"):
		p.advance()
		stmt.InsertMode = "IGNORE"
	case isKeyword(p.peek(), "REPLACE"):
		p.advance()
		stmt.InsertMode = "REPLACE"
	case isKeyword(p.peek(), "UPDATE"):
		p.advance()
		stmt.InsertMode = "UPDATE"
	}
	if isKeyword(p.peek(), "INTO") {
		p.advance()
	}
	target, err := p.parseMaybeDashedPathExpression()
	if err != nil {
		return nil, err
	}
	stmt.Target = target
	stmt.Stop = p.extEnd(target)
	hint, err := p.parseOptionalHint()
	if err != nil {
		return nil, err
	}
	if hint != nil {
		stmt.Hint = hint
		stmt.Stop = hint.End()
	}
	if p.peek().Kind == token.LPAREN {
		cols, err := p.parseInsertColumnList()
		if err != nil {
			return nil, err
		}
		stmt.Columns = cols
		stmt.Stop = cols.End()
	}
	if isKeyword(p.peek(), "ON") && isKeyword(p.peekAt(1), "CONFLICT") {
		oc, err := p.parseOnConflictClause()
		if err != nil {
			return nil, err
		}
		stmt.OnConflict = oc
		stmt.Stop = oc.End()
	}
	if isKeyword(p.peek(), "ASSERT_ROWS_MODIFIED") {
		arm, err := p.parseAssertRowsModified()
		if err != nil {
			return nil, err
		}
		stmt.AssertRowsModified = arm
		stmt.Stop = arm.End()
	}
	if isKeyword(p.peek(), "THEN") {
		rc, err := p.parseReturningClause()
		if err != nil {
			return nil, err
		}
		stmt.Returning = rc
		stmt.Stop = rc.End()
	}
	return &ast.PipeInsert{Span: span(pipeTok.Pos, stmt.End()), Insert: stmt}, nil
}

// parsePipeRecursiveUnion parses "RECURSIVE set_operation_metadata
// [WITH DEPTH ...] (query|subpipeline) [AS alias]" after a |> token; see
// pipe_recursive_union in googlesql.tm. Exactly one input operand is allowed,
// and an alias must be introduced by AS.
func (p *parser) parsePipeRecursiveUnion(pipeTok token.Token) (ast.Node, error) {
	p.advance() // RECURSIVE
	md, err := p.parseSetOperationMetadata()
	if err != nil {
		return nil, err
	}
	node := &ast.PipeRecursiveUnion{Span: span(pipeTok.Pos, md.End()), Metadata: md}
	if isKeyword(p.peek(), "WITH") && isKeyword(p.peekAt(1), "DEPTH") {
		mod, err := p.parseRecursionDepthModifier()
		if err != nil {
			return nil, err
		}
		node.Depth = mod
		node.Stop = mod.End()
	}
	if p.peek().Kind != token.LPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" but got %s`, describeToken(p.peek()))
	}
	input, err := p.parseSubqueryOrSubpipeline()
	if err != nil {
		return nil, err
	}
	node.Input = input
	node.Stop = input.End()
	switch {
	case isKeyword(p.peek(), "AS"):
		alias, err := p.parseOptionalAlias()
		if err != nil {
			return nil, err
		}
		node.Alias = alias
		node.Stop = alias.End()
	case (p.peek().Kind == token.IDENT || p.peek().Kind == token.QUOTED_IDENT) && !p.isReserved(p.peek()):
		// as_alias_with_required_as: a bare identifier alias is rejected with a
		// dedicated error; see pipe_recursive_union in googlesql.tm.
		return nil, p.errorf(p.peek().Pos, `Syntax error: The keyword "AS" is required before the alias for pipe RECURSIVE UNION`)
	}
	return node, nil
}

// parseSubqueryOrSubpipeline parses "( query )" or a parenthesized
// subpipeline "( |> ... )" (including an empty "()"); the open parenthesis is
// the next token. See subquery_or_subpipeline in googlesql.tm. A parenthesized
// query's location includes the parentheses.
func (p *parser) parseSubqueryOrSubpipeline() (ast.Node, error) {
	lparen := p.peek() // (
	if next := p.peekAt(1); next.Kind == token.PIPE_INPUT || next.Kind == token.RPAREN {
		return p.parseSubpipeline()
	}
	query, parenEnd, err := p.parseParenthesizedQuery()
	if err != nil {
		return nil, err
	}
	query.Start, query.Stop = lparen.Pos, parenEnd
	return query, nil
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
		if (tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT) || p.isReserved(tok) {
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
		if (next.Kind != token.IDENT && next.Kind != token.QUOTED_IDENT) || p.isReserved(next) {
			// Trailing comma; it is included in the operator's location.
			node.Stop = comma.End
			break
		}
	}
	return node, nil
}

// parsePipeAs parses "AS identifier" after a |> token; see pipe_as in
// googlesql.tm. The alias location covers just the identifier.
func (p *parser) parsePipeAs(pipeTok token.Token) (ast.Node, error) {
	p.advance() // AS
	ident, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	alias := &ast.Alias{Span: span(ident.Pos(), ident.End()), Identifier: ident}
	return &ast.PipeAs{Span: span(pipeTok.Pos, alias.End()), Alias: alias}, nil
}

// parsePipeRename parses "RENAME rename_item [, ...][,]" after a |> token,
// where each item is "old_name [AS] new_name"; see pipe_rename in googlesql.tm.
// A trailing comma is not included in the operator's location.
func (p *parser) parsePipeRename(pipeTok token.Token) (ast.Node, error) {
	p.advance() // RENAME
	node := &ast.PipeRename{Span: span(pipeTok.Pos, 0)}
	for {
		item, err := p.parsePipeRenameItem()
		if err != nil {
			return nil, err
		}
		node.Items = append(node.Items, item)
		node.Stop = item.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance() // ,
		next := p.peek()
		if (next.Kind != token.IDENT && next.Kind != token.QUOTED_IDENT) || p.isReserved(next) {
			// Trailing comma; it is not included in the operator's location.
			break
		}
	}
	return node, nil
}

// parsePipeRenameItem parses one "old_name [AS] new_name" pair; see
// pipe_rename_item in googlesql.tm.
func (p *parser) parsePipeRenameItem() (*ast.PipeRenameItem, error) {
	oldName, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind == token.DOT {
		return nil, p.errorf(oldName.Pos(), "Syntax error: Pipe RENAME can only rename columns by name alone; Renaming columns under table aliases or fields under paths is not supported")
	}
	if isKeyword(p.peek(), "AS") {
		p.advance() // AS
	}
	newName, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	return &ast.PipeRenameItem{Span: span(oldName.Pos(), newName.End()), OldName: oldName, NewName: newName}, nil
}

// parsePipeDrop parses "DROP identifier [, ...][,]" after a |> token; see
// pipe_drop in googlesql.tm. A trailing comma is included in the operator's
// location but not in the IdentifierList child.
func (p *parser) parsePipeDrop(pipeTok token.Token) (ast.Node, error) {
	p.advance() // DROP
	list := &ast.IdentifierList{Span: span(0, 0)}
	node := &ast.PipeDrop{Span: span(pipeTok.Pos, 0), ColumnList: list}
	for {
		ident, err := p.parseIdentifier()
		if err != nil {
			return nil, err
		}
		if p.peek().Kind == token.DOT {
			return nil, p.errorf(ident.Pos(), "Syntax error: Pipe DROP can only drop columns by name alone; Dropping columns under table aliases or fields under paths is not supported")
		}
		if len(list.Identifiers) == 0 {
			list.Start = ident.Pos()
		}
		list.Identifiers = append(list.Identifiers, ident)
		list.Stop = ident.End()
		node.Stop = ident.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		comma := p.advance()
		next := p.peek()
		if (next.Kind != token.IDENT && next.Kind != token.QUOTED_IDENT) || p.isReserved(next) {
			// Trailing comma; included in the operator's location only.
			node.Stop = comma.End
			break
		}
	}
	return node, nil
}

// parsePipeAssert parses "ASSERT expression [, expression ...][,]" after a |>
// token; see pipe_assert in googlesql.tm. The first expression is the asserted
// condition and the rest are message expressions. A trailing comma is included
// in the operator's location.
func (p *parser) parsePipeAssert(pipeTok token.Token) (ast.Node, error) {
	p.advance() // ASSERT
	node := &ast.PipeAssert{Span: span(pipeTok.Pos, 0)}
	for {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		node.Exprs = append(node.Exprs, expr)
		node.Stop = p.extEnd(expr)
		if p.peek().Kind != token.COMMA {
			break
		}
		comma := p.advance()
		if !startsExpression(p.peek()) {
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
		tok.Kind == token.IDENT && !p.isReserved(tok),
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

// parseSubpipelineNoParens parses one or more "|> operator" pipe operators
// without enclosing parentheses into a Subpipeline; at least one operator is
// required. The first "|>" is the next token. See subpipeline_no_parens in
// googlesql.tm.
func (p *parser) parseSubpipelineNoParens() (*ast.Subpipeline, error) {
	start := p.peek().Pos
	sub := &ast.Subpipeline{Span: span(start, start)}
	for p.peek().Kind == token.PIPE_INPUT {
		op, err := p.parsePipeOperator()
		if err != nil {
			return nil, err
		}
		sub.PipeOperators = append(sub.PipeOperators, op)
		sub.Stop = op.End()
	}
	return sub, nil
}

// maybeStatementWithPipeOperators attaches a "|> op ..." suffix to a statement
// that can produce a table (SHOW, DESCRIBE, EXECUTE IMMEDIATE, RUN, CALL),
// forming an ASTStatementWithPipeOperators. When no "|>" follows, the statement
// is returned unchanged. The pipe operators are parsed before the feature is
// checked, matching the grammar's reduction order (so an invalid pipe operator
// is reported ahead of the feature error). See sql_statement_body and
// sql_statement_body_maybe_pipe_suffix in googlesql.tm.
func (p *parser) maybeStatementWithPipeOperators(stmt ast.Statement) (ast.Statement, error) {
	if p.peek().Kind != token.PIPE_INPUT {
		return stmt, nil
	}
	pipeStart := p.peek().Pos
	sub, err := p.parseSubpipelineNoParens()
	if err != nil {
		return nil, err
	}
	if !p.features.Enabled(FeatureStatementWithPipeOperators) {
		// This diagnostic deliberately omits the "Syntax error: " prefix, as in
		// the reference (see sql_statement_body in googlesql.tm).
		return nil, p.errorf(pipeStart, "Pipe operators are not supported on this statement")
	}
	suffix := &ast.SubpipelineStatement{Span: span(sub.Pos(), sub.End()), Subpipeline: sub}
	return &ast.StatementWithPipeOperators{
		Span:       span(stmt.Pos(), suffix.End()),
		Statement:  stmt,
		PipeSuffix: suffix,
	}, nil
}

// parsePipePivot parses "PIVOT(...) [[AS] alias]" after a |> token; see
// pipe_pivot in googlesql.tm. The alias is carried on the PivotClause.
func (p *parser) parsePipePivot(pipeTok token.Token) (ast.Node, error) {
	clause, err := p.parsePivotClause()
	if err != nil {
		return nil, err
	}
	return &ast.PipePivot{Span: span(pipeTok.Pos, clause.End()), Pivot: clause.(*ast.PivotClause)}, nil
}

// parsePipeUnpivot parses "UNPIVOT(...) [[AS] alias]" after a |> token; see
// pipe_unpivot in googlesql.tm. The alias is carried on the UnpivotClause.
func (p *parser) parsePipeUnpivot(pipeTok token.Token) (ast.Node, error) {
	clause, err := p.parseUnpivotClause()
	if err != nil {
		return nil, err
	}
	return &ast.PipeUnpivot{Span: span(pipeTok.Pos, clause.End()), Unpivot: clause.(*ast.UnpivotClause)}, nil
}

// parsePipeIf parses "IF [hint] expr THEN subpipeline [ELSEIF expr THEN
// subpipeline ...] [ELSE subpipeline]" after a |> token; see pipe_if,
// pipe_if_prefix, and pipe_if_elseif in googlesql.tm.
func (p *parser) parsePipeIf(pipeTok token.Token) (ast.Node, error) {
	ifTok := p.advance() // IF
	node := &ast.PipeIf{Span: span(pipeTok.Pos, ifTok.End)}
	hint, err := p.parseOptionalHint()
	if err != nil {
		return nil, err
	}
	if hint != nil {
		node.Hint = hint
	}
	// First (IF) case. The case node's location starts at the IF keyword, even
	// when a hint appears between IF and the condition.
	firstCase, err := p.parsePipeIfCase(ifTok.Pos)
	if err != nil {
		return nil, err
	}
	node.Cases = append(node.Cases, firstCase)
	node.Stop = firstCase.End()
	for {
		tok := p.peek()
		if isKeyword(tok, "ELSEIF") {
			elseifTok := p.advance()
			c, err := p.parsePipeIfCase(elseifTok.Pos)
			if err != nil {
				return nil, err
			}
			node.Cases = append(node.Cases, c)
			node.Stop = c.End()
			continue
		}
		if isKeyword(tok, "ELSE") {
			elseTok := p.advance()
			if isKeyword(p.peek(), "IF") {
				// "ELSE IF" is a common typo for ELSEIF; the parser accepts it
				// here so it can suggest ELSEIF. See pipe_if_elseif in
				// googlesql.tm.
				return nil, p.errorf(elseTok.Pos, "Syntax error: Unexpected ELSE IF; Expected ELSEIF")
			}
			if p.peek().Kind != token.LPAREN {
				return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" or keyword IF but got %s`, describeToken(p.peek()))
			}
			sub, err := p.parseSubpipeline()
			if err != nil {
				return nil, err
			}
			node.Else = sub
			node.Stop = sub.End()
		}
		break
	}
	return node, nil
}

// parsePipeIfCase parses "expr THEN subpipeline" for one IF/ELSEIF branch,
// with the leading IF or ELSEIF keyword already consumed; start is the
// location of that keyword, which the case node spans from.
func (p *parser) parsePipeIfCase(start int) (*ast.PipeIfCase, error) {
	cond, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("THEN"); err != nil {
		return nil, err
	}
	if p.peek().Kind != token.LPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" but got %s`, describeToken(p.peek()))
	}
	sub, err := p.parseSubpipeline()
	if err != nil {
		return nil, err
	}
	return &ast.PipeIfCase{Span: span(start, sub.End()), Condition: cond, Body: sub}, nil
}

// parsePipeExportData parses "EXPORT DATA [WITH CONNECTION ...] [OPTIONS(...)]"
// after a |> token; see pipe_export_data and export_data_no_query in
// googlesql.tm. A trailing AS query is a dedicated error.
func (p *parser) parsePipeExportData(pipeTok token.Token) (ast.Node, error) {
	stmt, err := p.parseExportDataNoQuery()
	if err != nil {
		return nil, err
	}
	if isKeyword(p.peek(), "AS") {
		return nil, p.errorf(p.peek().Pos, "Syntax error: AS query is not allowed on pipe EXPORT DATA")
	}
	return &ast.PipeExportData{Span: span(pipeTok.Pos, stmt.End()), ExportData: stmt}, nil
}

// parseExportDataNoQuery parses "EXPORT DATA [WITH CONNECTION ...]
// [OPTIONS(...)]", i.e. an ExportDataStatement without its AS query; see
// export_data_no_query in googlesql.tm. EXPORT is the next token.
func (p *parser) parseExportDataNoQuery() (*ast.ExportDataStatement, error) {
	exportTok := p.advance() // EXPORT
	dataTok, err := p.expectKeyword("DATA")
	if err != nil {
		return nil, err
	}
	stmt := &ast.ExportDataStatement{Span: span(exportTok.Pos, dataTok.End)}
	if isKeyword(p.peek(), "WITH") {
		wc, err := p.parseWithConnectionClause()
		if err != nil {
			return nil, err
		}
		stmt.WithConnection = wc
		stmt.Stop = wc.End()
	}
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance()
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		stmt.Options = opts
		stmt.Stop = opts.End()
	}
	return stmt, nil
}

// parseExportDataStatement parses "EXPORT DATA [WITH CONNECTION ...]
// [OPTIONS(...)] AS query"; see export_data_statement in googlesql.tm. EXPORT
// is the next token.
func (p *parser) parseExportDataStatement() (ast.Statement, error) {
	stmt, err := p.parseExportDataNoQuery()
	if err != nil {
		return nil, err
	}
	asTok, err := p.expectKeyword("AS")
	if err != nil {
		return nil, err
	}
	_ = asTok
	query, err := p.parseQueryAfterAs()
	if err != nil {
		return nil, err
	}
	stmt.Query = query
	// The statement covers all consumed tokens, which can exceed the query
	// node's end: a parenthesized query keeps the location of the query inside
	// the parentheses.
	stmt.Stop = p.prevEnd()
	return stmt, nil
}

// parseExportModelStatement parses "EXPORT MODEL path_expression
// [WITH CONNECTION ...] [OPTIONS(...)]"; see export_model_statement in
// googlesql.tm. EXPORT is the next token.
func (p *parser) parseExportModelStatement() (ast.Statement, error) {
	exportTok := p.advance() // EXPORT
	p.advance()              // MODEL
	name, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	stmt := &ast.ExportModelStatement{Span: span(exportTok.Pos, name.End()), Name: name}
	if isKeyword(p.peek(), "WITH") {
		wc, err := p.parseWithConnectionClause()
		if err != nil {
			return nil, err
		}
		stmt.WithConnection = wc
		stmt.Stop = wc.End()
	}
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		stmt.Options = opts
		stmt.Stop = opts.End()
	}
	return stmt, nil
}

// isGenericEntityTypeToken reports whether tok can begin a generic entity
// type: a bare (non-keyword) identifier or the PROJECT keyword; see
// generic_entity_type_unchecked in googlesql.tm.
func isGenericEntityTypeToken(tok token.Token) bool {
	if tok.Kind != token.IDENT {
		return false
	}
	if strings.EqualFold(tok.Image, "PROJECT") {
		return true
	}
	return !keywordNames[strings.ToLower(tok.Image)]
}

// parseCreateEntityStatement parses "CREATE [OR REPLACE] generic_entity_type
// [IF NOT EXISTS] path_expression [OPTIONS(...)] [AS generic_entity_body]";
// see create_entity_statement in googlesql.tm. The entity type is the next
// token.
func (p *parser) parseCreateEntityStatement(createTok token.Token, isOrReplace bool) (ast.Statement, error) {
	typeTok := p.advance() // entity type
	if !p.entityTypes[strings.ToUpper(typeTok.Image)] {
		// No "Syntax error: " prefix; see generic_entity_type in googlesql.tm.
		return nil, p.errorf(typeTok.Pos, "%s is not a supported object type", typeTok.Image)
	}
	entType := p.parseIdentifierToken(typeTok)
	stmt := &ast.CreateEntityStatement{Span: span(createTok.Pos, typeTok.End), IsOrReplace: isOrReplace, Type: entType}
	ifne, err := p.parseOptIfNotExists()
	if err != nil {
		return nil, err
	}
	stmt.IsIfNotExists = ifne
	name, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	stmt.Name = name
	stmt.Stop = name.End()
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		stmt.Options = opts
		stmt.Stop = opts.End()
	}
	if isKeyword(p.peek(), "AS") {
		p.advance() // AS
		jsonBody, textBody, err := p.parseGenericEntityBody()
		if err != nil {
			return nil, err
		}
		stmt.JSONBody, stmt.TextBody = jsonBody, textBody
		if jsonBody != nil {
			stmt.Stop = jsonBody.End()
		} else {
			stmt.Stop = textBody.End()
		}
	}
	return stmt, nil
}

// parseGenericEntityBody parses a generic_entity_body: a JSON literal or a
// string literal; see generic_entity_body in googlesql.tm. Exactly one of the
// returned nodes is non-nil.
func (p *parser) parseGenericEntityBody() (jsonBody, textBody ast.Node, err error) {
	tok := p.peek()
	if isKeyword(tok, "JSON") && p.peekAt(1).Kind == token.STRING {
		lit, err := p.parseTypedLiteral()
		if err != nil {
			return nil, nil, err
		}
		return lit, nil, nil
	}
	if tok.Kind == token.STRING {
		lit, err := p.parseStringLiteralValue()
		if err != nil {
			return nil, nil, err
		}
		return nil, lit, nil
	}
	return nil, nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
}

// parsePipeCreateTable parses "CREATE TABLE ..." (a create_table_statement
// without an AS query) after a |> token; see pipe_create_table in
// googlesql.tm. A trailing AS query is a dedicated error.
func (p *parser) parsePipeCreateTable(pipeTok token.Token) (ast.Node, error) {
	saved := p.inPipeCreateTable
	p.inPipeCreateTable = true
	stmt, err := p.parseCreateStatement()
	p.inPipeCreateTable = saved
	if err != nil {
		return nil, err
	}
	ct := stmt.(*ast.CreateTableStatement)
	return &ast.PipeCreateTable{Span: span(pipeTok.Pos, ct.End()), CreateTable: ct}, nil
}

// parsePipeFork parses "FORK [hint] subpipeline [, subpipeline ...][,]" after
// a |> token; see pipe_fork in googlesql.tm. At least one subpipeline is
// required.
func (p *parser) parsePipeFork(pipeTok token.Token) (ast.Node, error) {
	forkTok := p.advance() // FORK
	node := &ast.PipeFork{Span: span(pipeTok.Pos, forkTok.End)}
	hint, err := p.parseOptionalHint()
	if err != nil {
		return nil, err
	}
	if hint != nil {
		node.Hint = hint
		node.Stop = hint.End()
	}
	if p.peek().Kind != token.LPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" or @ for hint but got %s`, describeToken(p.peek()))
	}
	for {
		sub, err := p.parseSubpipeline()
		if err != nil {
			return nil, err
		}
		node.Subpipelines = append(node.Subpipelines, sub)
		node.Stop = sub.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		comma := p.advance()
		node.Stop = comma.End
		if p.peek().Kind != token.LPAREN {
			break
		}
	}
	return node, nil
}

// parsePipeTee parses "TEE [hint] [subpipeline [, subpipeline ...][,]]" after
// a |> token; see pipe_tee in googlesql.tm. Unlike FORK, TEE allows zero
// subpipelines.
func (p *parser) parsePipeTee(pipeTok token.Token) (ast.Node, error) {
	teeTok := p.advance() // TEE
	node := &ast.PipeTee{Span: span(pipeTok.Pos, teeTok.End)}
	hint, err := p.parseOptionalHint()
	if err != nil {
		return nil, err
	}
	if hint != nil {
		node.Hint = hint
		node.Stop = hint.End()
	}
	// TEE with no subpipeline is allowed; anything other than "(" ends the
	// operator here.
	if p.peek().Kind != token.LPAREN {
		return node, nil
	}
	for {
		sub, err := p.parseSubpipeline()
		if err != nil {
			return nil, err
		}
		node.Subpipelines = append(node.Subpipelines, sub)
		node.Stop = sub.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		comma := p.advance()
		node.Stop = comma.End
		if p.peek().Kind != token.LPAREN {
			break
		}
	}
	return node, nil
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
			col, err := p.parsePipeAggregateSelectColumn()
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
		groupBy, err := p.parseGroupBy(groupByPipe)
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
	if isKeyword(p.peek(), "WINDOW") {
		window, err := p.parseWindowClause(true)
		if err != nil {
			return nil, err
		}
		sel.Window = window
		sel.Stop = window.End()
	}
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
		token.LBRACKET, token.LBRACE, token.LPAREN, token.MINUS, token.PLUS, token.TILDE,
		token.QUOTED_IDENT, token.PARAM, token.QUESTION, token.SYSTEM_VARIABLE:
		return true
	case token.IDENT:
		if !isReservedStatic(tok) {
			return true
		}
		if reservedFunctionNameKeywords[strings.ToUpper(tok.Image)] {
			// Reserved keywords that name a function call (IF, GROUPING, LEFT,
			// RIGHT, COLLATE) begin an expression; see function_name_from_keyword
			// in googlesql.tm.
			return true
		}
		switch strings.ToUpper(tok.Image) {
		case "NULL", "TRUE", "FALSE", "NOT", "ARRAY", "CASE", "CAST", "STRUCT", "EXISTS",
			"NEW", "INTERVAL", "RANGE":
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

	// Optional hint immediately after SELECT; see "SELECT" hint? in
	// select_clause in googlesql.tm. (This is distinct from a hint preceding
	// the whole statement, which produces an ASTHintedStatement.)
	if p.atsignOpensHint() {
		hint, err := p.parseOptionalHint()
		if err != nil {
			return nil, err
		}
		sel.Hint = hint
		sel.Stop = hint.End()
	}

	// Optional WITH modifier (anonymization / differential privacy). A WITH
	// immediately introducing a WITH expression ("WITH ( var AS ...") is not a
	// modifier; the reference lexer distinguishes it via
	// KW_WITH_STARTING_WITH_EXPRESSION (see lookahead_transformer.cc).
	if isKeyword(p.peek(), "WITH") && !p.startsWithExpression() {
		wm, err := p.parseWithModifier()
		if err != nil {
			return nil, err
		}
		sel.WithModifier = wm
		sel.Stop = wm.End()
	}

	if isKeyword(p.peek(), "DISTINCT") {
		p.advance()
		sel.Distinct = true
	} else if isKeyword(p.peek(), "ALL") {
		p.advance()
	}

	// Optional AS STRUCT / AS VALUE / AS <type_name> clause; see
	// opt_select_as_clause in googlesql.tm.
	if isKeyword(p.peek(), "AS") {
		sa, err := p.parseSelectAs()
		if err != nil {
			return nil, err
		}
		sel.SelectAs = sa
		sel.Stop = sa.End()
	}

	// An empty select list followed by FROM has a dedicated error; see the
	// second production of select_clause in googlesql.tm.
	if isKeyword(p.peek(), "FROM") {
		return nil, p.errorf(p.peek().Pos, "Syntax error: SELECT list must not be empty")
	}

	list, err := p.parseSelectList()
	if err != nil {
		return nil, err
	}
	sel.SelectList = list
	sel.Stop = list.End()
	return sel, nil
}

// parseWithModifier parses "WITH <identifier> [OPTIONS(...)]" after SELECT; see
// opt_with_modifier in googlesql.tm. OPTIONS is only consumed as the modifier's
// options list when it immediately follows the identifier and is directly
// followed by "(", matching the KW_OPTIONS_IN_WITH_OPTIONS lookahead
// disambiguation in lookahead_transformer.cc. Otherwise OPTIONS is left for the
// select list (as a path expression or function call).
func (p *parser) parseWithModifier() (*ast.WithModifier, error) {
	withTok := p.advance() // WITH
	id, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	wm := &ast.WithModifier{Span: span(withTok.Pos, id.End()), Identifier: id}
	if isKeyword(p.peek(), "OPTIONS") && p.peekAt(1).Kind == token.LPAREN {
		p.advance() // OPTIONS
		opts, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		wm.Options = opts
		wm.Stop = opts.End()
	}
	return wm, nil
}

// parseSelectAs parses "AS STRUCT", "AS VALUE", or "AS <path_expression>"; see
// opt_select_as_clause in googlesql.tm. An unquoted single-identifier VALUE is
// treated as the VALUE mode rather than a type name.
func (p *parser) parseSelectAs() (*ast.SelectAs, error) {
	asTok := p.advance() // AS
	if isKeyword(p.peek(), "STRUCT") {
		structTok := p.advance()
		return &ast.SelectAs{Span: span(asTok.Pos, structTok.End), AsMode: "STRUCT"}, nil
	}
	tok := p.peek()
	if tok.Kind == token.IDENT && strings.EqualFold(tok.Image, "VALUE") && p.peekAt(1).Kind != token.DOT {
		valTok := p.advance()
		return &ast.SelectAs{Span: span(asTok.Pos, valTok.End), AsMode: "VALUE"}, nil
	}
	path, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	return &ast.SelectAs{Span: span(asTok.Pos, path.End()), TypeName: path}, nil
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
		groupBy, err := p.parseGroupBy(groupByRegular)
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
	// QUALIFY is a non-reserved keyword. In the reference grammar the QUALIFY
	// clause is only permitted here after a WHERE, GROUP BY or HAVING clause;
	// otherwise QUALIFY directly following a table is treated as an alias. See
	// opt_clauses_following_from and qualify_clause in googlesql.tm.
	if isKeyword(p.peek(), "QUALIFY") {
		qualify, err := p.parseQualifyClause()
		if err != nil {
			return nil, err
		}
		sel.Qualify = qualify
		sel.Stop = qualify.End()
	}
	if isKeyword(p.peek(), "WINDOW") {
		window, err := p.parseWindowClause(false)
		if err != nil {
			return nil, err
		}
		sel.Window = window
		sel.Stop = window.End()
	}
	return sel, nil
}

// parseQualifyClause parses "QUALIFY expression"; the QUALIFY keyword is
// included in the resulting node's location. The FEATURE_QUALIFY language
// feature gates the clause: without it the reference parser reports
// "QUALIFY is not supported" at the QUALIFY keyword. See qualify_clause in
// googlesql.tm.
func (p *parser) parseQualifyClause() (*ast.Qualify, error) {
	qualifyTok := p.advance() // QUALIFY
	if !p.features.Enabled(FeatureQualify) {
		return nil, p.errorf(qualifyTok.Pos, "QUALIFY is not supported")
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	return &ast.Qualify{Span: span(qualifyTok.Pos, p.extEnd(expr)), Expr: expr}, nil
}

// isPostfixQualify reports whether the next tokens are a non-reserved QUALIFY
// keyword immediately followed by an expression, i.e. a QUALIFY clause in the
// postfix table operator position rather than a table alias named QUALIFY.
func (p *parser) isPostfixQualify() bool {
	return isKeyword(p.peek(), "QUALIFY") && startsExpression(p.peekAt(1))
}

// atPivotOrUnpivotClauseStart reports whether the next tokens begin a PIVOT or
// UNPIVOT postfix table operator rather than a table alias named PIVOT or
// UNPIVOT. PIVOT and UNPIVOT are non-reserved keywords, so "t PIVOT" alone is
// a table aliased PIVOT; only "PIVOT (" (or "UNPIVOT (", "UNPIVOT EXCLUDE",
// "UNPIVOT INCLUDE") introduces the clause. See the pivot_or_unpivot_clause
// disambiguation in lookahead_transformer.cc.
func (p *parser) atPivotOrUnpivotClauseStart() bool {
	switch {
	case isKeyword(p.peek(), "PIVOT"):
		return p.peekAt(1).Kind == token.LPAREN
	case isKeyword(p.peek(), "UNPIVOT"):
		next := p.peekAt(1)
		return next.Kind == token.LPAREN || isKeyword(next, "EXCLUDE") || isKeyword(next, "INCLUDE")
	}
	return false
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
		if next.Kind != token.STAR && !startsExpression(next) && !p.startsWithExpression() {
			// Trailing comma; it is included in the list's location. See
			// select_list in googlesql.tm. A WITH that introduces a WITH
			// expression ("WITH ( var AS ...") starts another column even
			// though WITH is a reserved keyword.
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

// parsePipeAggregateSelectColumn parses one pipe AGGREGATE selection item:
// "select_column_expr opt_selection_item_order" or "select_column_dot_star"
// (which has no order suffix); see pipe_selection_item_with_order in
// googlesql.tm.
func (p *parser) parsePipeAggregateSelectColumn() (*ast.SelectColumn, error) {
	col, err := p.parseSelectColumnOrDotStar()
	if err != nil {
		return nil, err
	}
	switch col.Expr.(type) {
	case *ast.DotStar, *ast.DotStarWithModifiers:
		// select_column_dot_star does not take an ordering suffix.
		return col, nil
	}
	order, err := p.parseSelectionItemOrder()
	if err != nil {
		return nil, err
	}
	if order != nil {
		col.Order = order
		col.Stop = order.End()
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
			if p.isReserved(tok) {
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
			if tok.Kind != token.QUOTED_IDENT && (tok.Kind != token.IDENT || p.isReserved(tok)) {
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
	if !p.currentIsAttachedAlias() {
		return nil
	}
	return p.errorf(p.peek().Pos, "Syntax error: Missing whitespace between literal and alias")
}

// currentIsAttachedAlias reports whether the current token is an unquoted
// identifier immediately following (with no whitespace) an integer or floating
// point literal, as in `123abc`. The reference lexer re-labels such an
// identifier as the ATTACHED_ALIAS token; it is only valid in the trailing
// alias position of a select column. See
// IsLiteralBeforeAdjacentUnquotedIdentifier in
// googlesql/parser/lookahead_transformer.cc.
func (p *parser) currentIsAttachedAlias() bool {
	tok := p.peek()
	if tok.Kind != token.IDENT || p.pos == 0 {
		return false
	}
	prev := p.toks[p.pos-1]
	if (prev.Kind != token.INT && prev.Kind != token.FLOAT) || prev.End != tok.Pos {
		return false
	}
	// Inputs like "123.abc" tokenize as the float "123." followed by the
	// identifier "abc" and remain valid, mirroring the reference lexer.
	return !strings.HasSuffix(prev.Image, ".")
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
	if tok.Kind == token.QUOTED_IDENT || (tok.Kind == token.IDENT && !p.isReserved(tok)) {
		ident := p.parseIdentifierToken(p.advance())
		return &ast.Alias{Span: span(start, ident.End()), Identifier: ident}, nil
	}
	if hasAs {
		// The reference LALR parser reports a generic error after "AS"
		// rather than "Expected identifier".
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
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
	node, err := p.parseTablePrimaryBase()
	if err != nil {
		return nil, err
	}
	return p.parsePostfixTableOperators(node)
}

func (p *parser) parseTablePrimaryBase() (ast.Node, error) {
	// A query parameter or system variable cannot appear where a table name is
	// expected; the reference reports a dedicated error (no "Syntax error:"
	// prefix) at the "@"/"?"/"@@" token. See the "@", "?", and "@@" table
	// primary rules in googlesql.tm.
	switch tok := p.peek(); tok.Kind {
	case token.PARAM, token.ATSIGN, token.QUESTION:
		return nil, p.errorf(tok.Pos, "Query parameters cannot be used in place of table names")
	case token.SYSTEM_VARIABLE:
		return nil, p.errorf(tok.Pos, "System variables cannot be used in place of table names")
	}
	// GRAPH_TABLE(...) is a graph table expression when GRAPH_TABLE is a
	// reserved keyword; see graph_table_query in googlesql.tm.
	if p.reserveGraphTable && isKeyword(p.peek(), "GRAPH_TABLE") && p.peekAt(1).Kind == token.LPAREN {
		return p.parseGraphTableQuery()
	}
	if isKeyword(p.peek(), "LATERAL") {
		return p.parseLateralTablePrimary()
	}
	if p.peek().Kind == token.LPAREN {
		if p.lparenStartsQuery() {
			start := p.pos
			node, subErr := p.parseTableSubquery()
			if subErr == nil {
				return node, nil
			}
			// "((query) join ...)" is a parenthesized join whose first
			// item is a table subquery; lparenStartsQuery sees through the
			// extra parenthesis, so retry from the opening parenthesis.
			if p.toks[start+1].Kind == token.LPAREN {
				p.pos = start
				join, joinErr := p.parseParenthesizedJoin()
				if joinErr == nil {
					return join, nil
				}
				return nil, furthestError(subErr, joinErr)
			}
			return nil, subErr
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
	if tok := p.peek(); (tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT) || p.isReserved(tok) {
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
	if !p.atPivotOrUnpivotClauseStart() {
		alias, err := p.parseOptionalAlias()
		if err != nil {
			return nil, err
		}
		if alias != nil {
			node.Alias = alias
			node.Stop = alias.End()
		}
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
	} else if isKeyword(p.peek(), "IF") && p.peekAt(1).Kind == token.LPAREN {
		// "IF(...)" is the only reserved keyword accepted as a TVF name in the
		// FROM clause; see the "(path_expression | IF)" tvf rule in
		// googlesql.tm.
		ifTok := p.advance()
		ident := p.parseIdentifierToken(ifTok)
		path := &ast.PathExpression{Span: span(ifTok.Pos, ifTok.End), Names: []*ast.Identifier{ident}}
		return p.parseTVFRest(path)
	} else {
		// A table primary reports plain "Unexpected" errors rather than the
		// path expression's "Expected identifier" ones.
		if tok := p.peek(); tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
		}
		p.inTablePath = true
		path, err := p.parseMaybeDashedPathExpression()
		p.inTablePath = false
		if err != nil {
			return nil, err
		}
		if p.peek().Kind == token.LPAREN {
			return p.parseTVFRest(path)
		}
		table = &ast.TablePathExpression{Span: span(path.Pos(), path.End()), Path: path}
	}
	// Array element access and generalized field access on a table path are
	// only allowed inside UNNEST; see table_path_expression_base in
	// googlesql.tm.
	if tok := p.peek(); tok.Kind == token.LBRACKET {
		return nil, p.errorf(tok.Pos, "Syntax error: Array element access is not allowed in the FROM clause without UNNEST; Use UNNEST(<expression>)")
	}
	if p.peek().Kind == token.DOT && p.peekAt(1).Kind == token.LPAREN {
		return nil, p.errorf(p.peekAt(1).Pos, "Syntax error: Generalized field access is not allowed in the FROM clause without UNNEST; Use UNNEST(<expression>)")
	}
	// An optional hint (@{...}) between the table path and the alias; see
	// "table_path_expression_base hint? as_alias?" in googlesql.tm.
	if p.atsignOpensHint() {
		hint, err := p.parseOptionalHint()
		if err != nil {
			return nil, err
		}
		table.Hint = hint
		table.Stop = hint.End()
	}
	// A non-reserved QUALIFY keyword immediately followed by an expression is a
	// QUALIFY clause in the postfix table operator position, not a table alias;
	// leave it for parsePostfixTableOperators (see qualify_clause_nonreserved
	// in pivot_or_unpivot_clause in googlesql.tm). QUALIFY not followed by an
	// expression (e.g. at end of input or before WHERE) is a plain alias.
	if !p.isPostfixQualify() && !p.atPivotOrUnpivotClauseStart() {
		alias, err := p.parseOptionalAlias()
		if err != nil {
			return nil, err
		}
		if alias != nil {
			table.Alias = alias
			table.Stop = alias.End()
		}
	}
	// WITH OFFSET [[AS] alias]; see with_offset_and_alias in googlesql.tm.
	// A bare WITH in this position must be followed by OFFSET.
	if isKeyword(p.peek(), "WITH") {
		withTok := p.advance() // WITH
		offsetTok, err := p.expectKeyword("OFFSET")
		if err != nil {
			return nil, err
		}
		offset := &ast.WithOffset{Span: span(withTok.Pos, offsetTok.End)}
		if !p.atPivotOrUnpivotClauseStart() {
			offsetAlias, err := p.parseOptionalAlias()
			if err != nil {
				return nil, err
			}
			if offsetAlias != nil {
				offset.Alias = offsetAlias
				offset.Stop = offsetAlias.End()
			}
		}
		table.Offset = offset
		table.Stop = offset.End()
	}
	// FOR SYSTEM TIME AS OF <expression>; see at_system_time in googlesql.tm.
	// FOR is only consumed here when followed by SYSTEM/SYSTEM_TIME; a bare
	// FOR (e.g. FOR UPDATE lock mode) belongs to a higher-level rule.
	if p.atForSystemTime() {
		fst, err := p.parseForSystemTime()
		if err != nil {
			return nil, err
		}
		table.ForSystemTime = fst
		table.Stop = fst.End()
	}
	return table, nil
}

// atForSystemTime reports whether the next tokens begin a FOR SYSTEM_TIME
// clause; see at_system_time in googlesql.tm.
func (p *parser) atForSystemTime() bool {
	return isKeyword(p.peek(), "FOR") &&
		(isKeyword(p.peekAt(1), "SYSTEM") || isKeyword(p.peekAt(1), "SYSTEM_TIME"))
}

// parseForSystemTime parses a "FOR SYSTEM_TIME AS OF <expression>" clause; see
// at_system_time in googlesql.tm.
func (p *parser) parseForSystemTime() (*ast.ForSystemTime, error) {
	forTok := p.advance() // FOR
	if isKeyword(p.peek(), "SYSTEM_TIME") {
		p.advance()
	} else {
		if _, err := p.expectKeyword("SYSTEM"); err != nil {
			return nil, err
		}
		if _, err := p.expectKeyword("TIME"); err != nil {
			return nil, err
		}
	}
	if _, err := p.expectKeyword("AS"); err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("OF"); err != nil {
		return nil, err
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	return &ast.ForSystemTime{Span: span(forTok.Pos, expr.End()), Expr: expr}, nil
}

// parseTVFRest parses the argument list and optional alias of a
// table-valued function call in a FROM clause, after the function's path
// expression has already been parsed; see tvf in googlesql.tm. The opening
// parenthesis is the next token.
func (p *parser) parseTVFRest(path *ast.PathExpression) (*ast.TVF, error) {
	tvf, err := p.parseTVFCore(path)
	if err != nil {
		return nil, err
	}
	if !p.atPivotOrUnpivotClauseStart() {
		alias, err := p.parseOptionalAlias()
		if err != nil {
			return nil, err
		}
		if alias != nil {
			tvf.Alias = alias
			tvf.Stop = alias.End()
		}
	}
	return tvf, nil
}

// parseTVFCore parses "( tvf_argument, ... ) hint?" after the function's path
// expression, i.e. the shared part of the tvf grammar rule without the outer
// alias; see tvf in googlesql.tm. The opening parenthesis is the next token.
func (p *parser) parseTVFCore(path *ast.PathExpression) (*ast.TVF, error) {
	p.advance() // (
	tvf := &ast.TVF{Span: span(path.Pos(), 0), Name: path}
	if p.peek().Kind != token.RPAREN {
		first := true
		for {
			arg, err := p.parseTVFArgument(first)
			if err != nil {
				return nil, err
			}
			first = false
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
	hint, err := p.parseOptionalHint()
	if err != nil {
		return nil, err
	}
	if hint != nil {
		tvf.Hint = hint
		tvf.Stop = hint.End()
	}
	return tvf, nil
}

// parsePipeCall parses "CALL tvf [[AS] alias]" after a |> token; see pipe_call
// in googlesql.tm. The alias is carried on the TVF node.
func (p *parser) parsePipeCall(pipeTok token.Token) (ast.Node, error) {
	p.advance() // CALL
	// The TVF name is a path expression (the IF keyword is also allowed as a
	// name); an "@" hint or anything that cannot start a path is an error here.
	if tok := p.peek(); (tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT || p.isReserved(tok)) && !isKeyword(tok, "IF") {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
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
	alias, err := p.parseOptionalAlias()
	if err != nil {
		return nil, err
	}
	if alias != nil {
		tvf.Alias = alias
		tvf.Stop = alias.End()
	}
	return &ast.PipeCall{Span: span(pipeTok.Pos, tvf.End()), Call: tvf}, nil
}

// atInputTableArgument reports whether the next two tokens are "INPUT TABLE",
// the input_table_argument form; see input_table_argument in googlesql.tm.
func (p *parser) atInputTableArgument() bool {
	return isKeyword(p.peek(), "INPUT") && isKeyword(p.peekAt(1), "TABLE")
}

// parseInputTableArgument parses "INPUT TABLE" into an InputTableArgument; the
// INPUT keyword is the next token. See input_table_argument in googlesql.tm.
func (p *parser) parseInputTableArgument() *ast.InputTableArgument {
	inputTok := p.advance() // INPUT
	tableTok := p.advance() // TABLE
	return &ast.InputTableArgument{Span: span(inputTok.Pos, tableTok.End)}
}

// parseDescriptorArgument parses "DESCRIPTOR ( column [, ...] )" into a
// Descriptor; the DESCRIPTOR keyword is the next token. See
// descriptor_argument in googlesql.tm.
func (p *parser) parseDescriptorArgument() (*ast.Descriptor, error) {
	descTok := p.advance() // DESCRIPTOR
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	list := &ast.DescriptorColumnList{}
	for {
		id, err := p.parseIdentifier()
		if err != nil {
			return nil, err
		}
		col := &ast.DescriptorColumn{Span: span(id.Pos(), id.End()), Name: id}
		list.Columns = append(list.Columns, col)
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance() // ,
	}
	list.Span = span(list.Columns[0].Pos(), list.Columns[len(list.Columns)-1].End())
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	return &ast.Descriptor{Span: span(descTok.Pos, rparen.End), Columns: list}, nil
}

// parseTVFArgument parses a single table-valued function (or CALL statement)
// argument: an expression, or a TABLE, MODEL, or CONNECTION clause; see
// tvf_argument in googlesql.tm. The keyword forms apply only when the
// keyword is followed by a token that can start the clause's operand, so a
// plain column reference named "table" still parses as an expression.
func (p *parser) parseTVFArgument(emptyListAllowed bool) (*ast.TVFArgument, error) {
	tok := p.peek()
	isPathStart := func(t token.Token) bool {
		return (t.Kind == token.IDENT || t.Kind == token.QUOTED_IDENT) && !p.isReserved(t)
	}
	switch {
	case p.atInputTableArgument():
		arg := p.parseInputTableArgument()
		return &ast.TVFArgument{Span: arg.Span, Expr: arg}, nil
	case isKeyword(tok, "DESCRIPTOR") && p.peekAt(1).Kind == token.LPAREN:
		desc, err := p.parseDescriptorArgument()
		if err != nil {
			return nil, err
		}
		return &ast.TVFArgument{Span: desc.Span, Expr: desc}, nil
	case isKeyword(tok, "SELECT"):
		// A bare subquery argument must be parenthesized; see tvf_argument in
		// googlesql.tm.
		return nil, p.errorf(tok.Pos, "Syntax error: Each subquery argument for table-valued function calls must be enclosed in parentheses. To fix this, replace SELECT... with (SELECT...)")
	case isKeyword(tok, "WITH"):
		return nil, p.errorf(tok.Pos, "Syntax error: Each subquery argument for table-valued function calls must be enclosed in parentheses. To fix this, replace WITH... with (WITH...)")
	case tok.Kind == token.LPAREN && isPathStart(p.peekAt(1)) && p.peekAt(2).Kind == token.LAMBDA:
		// "(" named_argument ")": a named argument must not be parenthesized;
		// see tvf_argument in googlesql.tm. The error points at the open paren.
		return nil, p.errorf(tok.Pos, "Syntax error: Named arguments for table-valued function calls written as \"name => value\" must not be enclosed in parentheses. To fix this, replace (name => value) with name => value")
	case isKeyword(tok, "TABLE") && isPathStart(p.peekAt(1)):
		p.advance() // TABLE
		body, err := p.parseTableClauseNoKeyword()
		if err != nil {
			return nil, err
		}
		clause := &ast.TableClause{Span: span(tok.Pos, body.End()), Table: body}
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
	case isPathStart(tok) && p.peekAt(1).Kind == token.LAMBDA:
		// named_argument: identifier "=>" expression, or named_non_expr_argument:
		// identifier "=>" table_clause; see tvf_argument in googlesql.tm.
		name := p.parseIdentifierToken(p.advance())
		p.advance() // consume =>
		if tableTok := p.peek(); isKeyword(tableTok, "TABLE") && isPathStart(p.peekAt(1)) {
			// named_non_expr_argument: the TABLE clause value is wrapped in a
			// Query and an ExpressionSubquery, both spanning the table clause.
			p.advance() // TABLE
			body, err := p.parseTableClauseNoKeyword()
			if err != nil {
				return nil, err
			}
			clause := &ast.TableClause{Span: span(tableTok.Pos, body.End()), Table: body}
			query := &ast.Query{Span: clause.Span, QueryExpr: clause}
			subquery := &ast.ExpressionSubquery{Span: clause.Span, Query: query}
			named := &ast.NamedArgument{
				Span:  span(name.Pos(), clause.End()),
				Name:  name,
				Value: subquery,
			}
			return &ast.TVFArgument{Span: named.Span, Expr: named}, nil
		}
		if p.atInputTableArgument() {
			// named_argument: identifier "=>" input_table_argument; see
			// googlesql.tm.
			arg := p.parseInputTableArgument()
			named := &ast.NamedArgument{Span: span(name.Pos(), arg.End()), Name: name, Value: arg}
			return &ast.TVFArgument{Span: named.Span, Expr: named}, nil
		}
		value, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		if p.peek().Kind == token.ARROW {
			value, err = p.finishLambda(value)
			if err != nil {
				return nil, err
			}
		}
		named := &ast.NamedArgument{
			Span:  span(name.Pos(), p.extEnd(value)),
			Name:  name,
			Value: value,
		}
		return &ast.TVFArgument{Span: named.Span, Expr: named}, nil
	}
	startPos := p.pos
	expr, err := p.parseExpression()
	if err != nil {
		// A token that cannot begin an expression (and none of the argument
		// forms above) leaves the argument slot empty. When an empty argument
		// list (an immediate ")") would also be valid here, the reference
		// reports the expected closing parenthesis rather than an "unexpected"
		// error. See tvf in googlesql.tm; e.g. "tvf(from t)". This only applies
		// to the first argument slot; after a comma an argument is required, so
		// the plain expression error stands.
		if emptyListAllowed && p.pos == startPos {
			return nil, p.errorf(p.peek().Pos, `Syntax error: Expected ")" but got %s`, describeToken(p.peek()))
		}
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
		// A trailing "name => value" named argument (the optional array_zip_mode)
		// may follow the expression list; see unnest_expression in googlesql.tm.
		if len(node.Expressions) > 0 && p.peekAt(1).Kind == token.LAMBDA &&
			(p.peek().Kind == token.IDENT || p.peek().Kind == token.QUOTED_IDENT) &&
			!p.isReserved(p.peek()) {
			name := p.parseIdentifierToken(p.advance())
			p.advance() // consume =>
			value, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			if p.peek().Kind == token.ARROW {
				value, err = p.finishLambda(value)
				if err != nil {
					return nil, err
				}
			}
			node.ArrayZipMode = &ast.NamedArgument{
				Span:  span(name.Pos(), p.extEnd(value)),
				Name:  name,
				Value: value,
			}
			break
		}
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

// finishLambda parses "-> expression" after a lambda argument list has already
// been parsed as params, producing an ASTLambda; see lambda_argument in
// googlesql.tm. The caller must have verified the current token is ARROW.
// params is a PathExpression (single unparenthesized argument) or a
// StructConstructorWithParens (parenthesized argument list).
func (p *parser) finishLambda(params ast.Node) (ast.Node, error) {
	// The lambda argument list must be a single identifier (parsed as a
	// PathExpression) or a parenthesized argument list (a
	// StructConstructorWithParens); any other expression is rejected. See
	// lambda_argument_list in googlesql.tm.
	switch params.(type) {
	case *ast.PathExpression, *ast.StructConstructorWithParens:
	default:
		return nil, p.errorf(p.extStart(params), "Syntax error: Expecting lambda argument list")
	}
	p.advance() // consume ->
	body, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	return &ast.Lambda{Span: span(p.extStart(params), p.extEnd(body)), Params: params, Body: body}, nil
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
		if tok.Kind != token.QUOTED_IDENT && (tok.Kind != token.IDENT || p.isReserved(tok)) {
			return nil, p.errorf(tok.Pos, "Syntax error: Expected identifier but got %s", describeToken(tok))
		}
		ident := p.parseIdentifierToken(p.advance())
		node.Alias = &ast.Alias{Span: span(asTok.Pos, ident.End()), Identifier: ident}
		node.Stop = ident.End()
	}
	return node, nil
}

// groupByMode selects which GROUP BY grammar to parse; see the group_by_*
// rules in googlesql.tm.
type groupByMode int

const (
	// groupByRegular is a standard-syntax GROUP BY: it permits GROUP BY ALL
	// and plain grouping_items (no alias or ordering), and no trailing comma.
	groupByRegular groupByMode = iota
	// groupByFunc is the group_by_clause_prefix used as a multi-level
	// aggregation modifier inside a function call: no ALL, plain grouping
	// items, no trailing comma.
	groupByFunc
	// groupByPipe is pipe AGGREGATE's GROUP BY: no ALL, grouping items that
	// accept an alias and an ASC/DESC/NULLS ordering suffix, an optional
	// "AND ORDER" preamble, and a trailing comma.
	groupByPipe
)

func (p *parser) parseGroupBy(mode groupByMode) (*ast.GroupBy, error) {
	groupTok, err := p.expectKeyword("GROUP")
	if err != nil {
		return nil, err
	}
	hint, err := p.parseOptionalHint()
	if err != nil {
		return nil, err
	}
	andOrderBy := false
	if mode == groupByPipe && isKeyword(p.peek(), "AND") {
		p.advance() // AND
		if _, err := p.expectKeyword("ORDER"); err != nil {
			return nil, err
		}
		andOrderBy = true
	}
	if !isKeyword(p.peek(), "BY") {
		if cerr := p.exceptClashError(); cerr != nil {
			return nil, cerr
		}
		// Right after "GROUP" the preamble is "hint? BY" (plus "(AND ORDER)?"
		// in pipe AGGREGATE). When no hint and no "AND ORDER" have been
		// consumed, "@" (for a hint) is still a valid continuation alongside
		// "BY", so the reference lists both; see group_by_preamble in
		// googlesql.tm.
		if mode != groupByPipe && hint == nil {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Expected @ for hint or keyword BY but got %s", describeToken(p.peek()))
		}
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword BY but got %s", describeToken(p.peek()))
	}
	p.advance() // BY
	groupBy := &ast.GroupBy{Span: span(groupTok.Pos, groupTok.End), Hint: hint, AndOrderBy: andOrderBy}
	if mode == groupByRegular && isKeyword(p.peek(), "ALL") {
		allTok := p.advance()
		groupBy.All = &ast.GroupByAll{Span: span(allTok.Pos, allTok.End)}
		groupBy.Stop = allTok.End
		return groupBy, nil
	}
	for {
		item, err := p.parseGroupingItem(mode == groupByPipe)
		if err != nil {
			return nil, err
		}
		groupBy.Items = append(groupBy.Items, item)
		groupBy.Stop = item.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		comma := p.advance()
		if mode == groupByPipe && !startsExpression(p.peek()) {
			// Trailing comma; it is included in the clause's location. See
			// group_by_clause_in_pipe in googlesql.tm.
			groupBy.Stop = comma.End
			break
		}
	}
	return groupBy, nil
}

// parseGroupingItem parses one grouping item; see grouping_item and
// grouping_item_in_pipe in googlesql.tm. A grouping item is the empty item
// "()", a ROLLUP/CUBE list, a GROUPING SETS list, or an expression. In pipe
// AGGREGATE (inPipe) the expression form also accepts an alias and an
// ASC/DESC/NULLS ordering suffix.
func (p *parser) parseGroupingItem(inPipe bool) (*ast.GroupingItem, error) {
	// grouping_item_base: "(" ")"
	if p.peek().Kind == token.LPAREN && p.peekAt(1).Kind == token.RPAREN {
		lp := p.advance()
		rp := p.advance()
		return &ast.GroupingItem{Span: span(lp.Pos, rp.End)}, nil
	}
	// grouping_item_base: ROLLUP / CUBE list.
	if isKeyword(p.peek(), "ROLLUP") || isKeyword(p.peek(), "CUBE") {
		rollup, cube, err := p.parseRollupOrCube()
		if err != nil {
			return nil, err
		}
		if rollup != nil {
			return &ast.GroupingItem{Span: span(rollup.Pos(), rollup.End()), Rollup: rollup}, nil
		}
		return &ast.GroupingItem{Span: span(cube.Pos(), cube.End()), Cube: cube}, nil
	}
	// grouping_item_base: GROUPING SETS ( ... ).
	if isKeyword(p.peek(), "GROUPING") && isKeyword(p.peekAt(1), "SETS") {
		list, err := p.parseGroupingSetList()
		if err != nil {
			return nil, err
		}
		return &ast.GroupingItem{Span: span(list.Pos(), list.End()), GroupingSetList: list}, nil
	}
	// Expression form.
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	item := &ast.GroupingItem{Span: span(p.extStart(expr), p.extEnd(expr)), Expr: expr}
	if inPipe {
		alias, err := p.parseOptionalAlias()
		if err != nil {
			return nil, err
		}
		if alias != nil {
			item.Alias = alias
			item.Stop = alias.End()
		}
		order, err := p.parseGroupingItemOrder()
		if err != nil {
			return nil, err
		}
		if order != nil {
			item.Order = order
			item.Stop = order.End()
		}
	}
	return item, nil
}

// parseRollupOrCube parses a "ROLLUP(expr, ...)" or "CUBE(expr, ...)" list;
// see rollup_list and cube_list in googlesql.tm. Exactly one of the returned
// nodes is non-nil.
func (p *parser) parseRollupOrCube() (*ast.Rollup, *ast.Cube, error) {
	kwTok := p.advance() // ROLLUP or CUBE
	isRollup := strings.EqualFold(kwTok.Image, "ROLLUP")
	if p.peek().Kind != token.LPAREN {
		return nil, nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" but got %s`, describeToken(p.peek()))
	}
	p.advance() // (
	exprs, end, err := p.parseGroupingExpressionList()
	if err != nil {
		return nil, nil, err
	}
	if isRollup {
		return &ast.Rollup{Span: span(kwTok.Pos, end), Expressions: exprs}, nil, nil
	}
	return nil, &ast.Cube{Span: span(kwTok.Pos, end), Expressions: exprs}, nil
}

// parseGroupingExpressionList parses "expression (, expression)* )" with the
// opening "(" already consumed, returning the expressions and the end offset
// of the closing ")". At least one expression is required.
func (p *parser) parseGroupingExpressionList() ([]ast.Node, int, error) {
	var exprs []ast.Node
	for {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, 0, err
		}
		exprs = append(exprs, expr)
		switch p.peek().Kind {
		case token.COMMA:
			p.advance()
		case token.RPAREN:
			rp := p.advance()
			return exprs, rp.End, nil
		default:
			return nil, 0, p.errorf(p.peek().Pos, `Syntax error: Expected ")" or "," but got %s`, describeToken(p.peek()))
		}
	}
}

// parseGroupingSetList parses "GROUPING SETS ( grouping_set, ... )"; see
// grouping_item_base and grouping_set_list in googlesql.tm.
func (p *parser) parseGroupingSetList() (*ast.GroupingSetList, error) {
	groupingTok := p.advance() // GROUPING
	p.advance()                // SETS
	if p.peek().Kind != token.LPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" but got %s`, describeToken(p.peek()))
	}
	p.advance() // (
	list := &ast.GroupingSetList{Span: span(groupingTok.Pos, 0)}
	for {
		gs, err := p.parseGroupingSet()
		if err != nil {
			return nil, err
		}
		list.GroupingSets = append(list.GroupingSets, gs)
		switch p.peek().Kind {
		case token.COMMA:
			p.advance()
		case token.RPAREN:
			rp := p.advance()
			list.Stop = rp.End
			return list, nil
		default:
			return nil, p.errorf(p.peek().Pos, `Syntax error: Expected ")" or "," but got %s`, describeToken(p.peek()))
		}
	}
}

// parseGroupingSet parses one grouping set: the empty set "()", a ROLLUP or
// CUBE list, or an expression; see grouping_set in googlesql.tm.
func (p *parser) parseGroupingSet() (*ast.GroupingSet, error) {
	if p.peek().Kind == token.LPAREN && p.peekAt(1).Kind == token.RPAREN {
		lp := p.advance()
		rp := p.advance()
		return &ast.GroupingSet{Span: span(lp.Pos, rp.End)}, nil
	}
	if isKeyword(p.peek(), "ROLLUP") || isKeyword(p.peek(), "CUBE") {
		rollup, cube, err := p.parseRollupOrCube()
		if err != nil {
			return nil, err
		}
		if rollup != nil {
			return &ast.GroupingSet{Span: span(rollup.Pos(), rollup.End()), Rollup: rollup}, nil
		}
		return &ast.GroupingSet{Span: span(cube.Pos(), cube.End()), Cube: cube}, nil
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	// The reference wraps the expression with WithEndLocation($1, @$), which
	// extends the expression node's own end to the grouping set's end (the
	// closing parenthesis of a "(expr)" form). See grouping_set in
	// googlesql.tm.
	start := p.extStart(expr)
	end := p.extEnd(expr)
	setNodeEnd(expr, end)
	return &ast.GroupingSet{Span: span(start, end), Expr: expr}, nil
}

// parseGroupingItemOrder parses the optional ASC/DESC and/or NULLS FIRST/LAST
// ordering suffix on a pipe AGGREGATE grouping item; see opt_grouping_item_order
// in googlesql.tm. It returns nil when no ordering suffix is present.
func (p *parser) parseGroupingItemOrder() (*ast.GroupingItemOrder, error) {
	var order *ast.GroupingItemOrder
	switch {
	case isKeyword(p.peek(), "ASC"):
		tok := p.advance()
		order = &ast.GroupingItemOrder{Span: span(tok.Pos, tok.End), Spec: "ASC"}
	case isKeyword(p.peek(), "DESC"):
		tok := p.advance()
		order = &ast.GroupingItemOrder{Span: span(tok.Pos, tok.End), Spec: "DESC"}
	case isKeyword(p.peek(), "NULLS"):
		order = &ast.GroupingItemOrder{Span: span(p.peek().Pos, p.peek().Pos), Spec: "UNSPECIFIED"}
	default:
		return nil, nil
	}
	if err := p.parseNullOrderSuffix(order); err != nil {
		return nil, err
	}
	return order, nil
}

// parseSelectionItemOrder parses the optional ASC/DESC ordering suffix (with an
// optional NULLS FIRST/LAST) on a pipe AGGREGATE selection item; see
// selection_item_order / opt_selection_item_order in googlesql.tm. Unlike
// opt_grouping_item_order it does not accept NULLS FIRST/LAST without a
// preceding ASC/DESC. It returns nil when no ordering suffix is present.
func (p *parser) parseSelectionItemOrder() (*ast.GroupingItemOrder, error) {
	var order *ast.GroupingItemOrder
	switch {
	case isKeyword(p.peek(), "ASC"):
		tok := p.advance()
		order = &ast.GroupingItemOrder{Span: span(tok.Pos, tok.End), Spec: "ASC"}
	case isKeyword(p.peek(), "DESC"):
		tok := p.advance()
		order = &ast.GroupingItemOrder{Span: span(tok.Pos, tok.End), Spec: "DESC"}
	default:
		return nil, nil
	}
	if err := p.parseNullOrderSuffix(order); err != nil {
		return nil, err
	}
	return order, nil
}

// parseNullOrderSuffix parses an optional "NULLS FIRST" / "NULLS LAST" suffix
// and attaches it to order; see opt_null_order in googlesql.tm.
func (p *parser) parseNullOrderSuffix(order *ast.GroupingItemOrder) error {
	if !isKeyword(p.peek(), "NULLS") {
		return nil
	}
	nullsTok := p.advance()
	var nullsFirst bool
	switch {
	case isKeyword(p.peek(), "FIRST"):
		nullsFirst = true
	case isKeyword(p.peek(), "LAST"):
		nullsFirst = false
	default:
		return p.errorf(p.peek().Pos, "Syntax error: Expected keyword FIRST or keyword LAST but got %s", describeToken(p.peek()))
	}
	endTok := p.advance()
	order.NullOrder = &ast.NullOrder{Span: span(nullsTok.Pos, endTok.End), NullsFirst: nullsFirst}
	order.Stop = endTok.End
	return nil
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

// parseCollate parses "COLLATE <collation>" with the COLLATE keyword as the
// next token; see collate_clause in googlesql.tm. The collation name is a
// string literal, a query parameter, or a system variable.
func (p *parser) parseCollate() (*ast.Collate, error) {
	collateTok := p.advance() // COLLATE
	var name ast.Node
	var err error
	switch p.peek().Kind {
	case token.STRING:
		name, err = p.parseStringLiteral()
	case token.PARAM, token.QUESTION, token.SYSTEM_VARIABLE:
		name, err = p.parseIntLiteralOrParameter()
	default:
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "@" or "@@" or string literal but got %s`, describeToken(p.peek()))
	}
	if err != nil {
		return nil, err
	}
	return &ast.Collate{Span: span(collateTok.Pos, name.End()), Name: name}, nil
}

// atsignOpensHint reports whether the "@" at the current position begins a
// hint. The lookahead transformer only turns "@" into a hint opener
// (KW_OPEN_HINT / KW_OPEN_INTEGER_HINT) when it is immediately followed by "{"
// or an integer literal; otherwise "@" stays an ATSIGN that begins a named
// parameter (e.g. "@ name"). See the ATSIGN case in lookahead_transformer.cc.
func (p *parser) atsignOpensHint() bool {
	if p.peek().Kind != token.ATSIGN {
		return false
	}
	k := p.peekAt(1).Kind
	return k == token.LBRACE || k == token.INT
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
	if p.isReserved(tok) && !isKeyword(tok, "HASH") && !isKeyword(tok, "PROTO") && !isKeyword(tok, "PARTITION") {
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
		// The reference lexes a NOT followed by BETWEEN, IN, LIKE, or DISTINCT
		// as KW_NOT_SPECIAL (see lookahead_transformer.cc), which is only valid
		// as an infix operator. As a prefix (start of an expression) it has no
		// grammar rule, so the parser rejects the NOT itself.
		next := p.peekAt(1)
		if isKeyword(next, "BETWEEN") || isKeyword(next, "IN") || isKeyword(next, "LIKE") || isKeyword(next, "DISTINCT") {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected keyword %s", strings.ToUpper(p.peek().Image))
		}
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
	// A PIVOT clause's FOR expression must not consume a following top-level
	// IN operator (it introduces the pivot value list). Capture and clear the
	// flag so only this outermost comparison is affected; nested sub-parses
	// proceed normally.
	suppressIn := p.suppressTopLevelIn
	p.suppressTopLevelIn = false

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
		case isKeyword(next, "IN") && suppressIn:
			// Leave "NOT IN" for the enclosing PIVOT clause to reject.
		case isKeyword(next, "BETWEEN"), isKeyword(next, "IN"), isKeyword(next, "LIKE"):
			notTok = p.advance()
		case isKeyword(next, "DISTINCT"):
			return nil, p.errorf(next.Pos, "Syntax error: Expected keyword BETWEEN or keyword IN or keyword LIKE but got %s", describeToken(next))
		}
	}

	// [NOT] BETWEEN
	if isKeyword(p.peek(), "BETWEEN") {
		betweenTok := p.advance()
		// The middle operand is an expression_higher_prec_than_and (see the
		// between rule in googlesql.tm): it may itself parse a comparison,
		// nested BETWEEN, or unary NOT, but such operators are disallowed
		// unless parenthesized because BETWEEN is not associative.
		low, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		if !p.isAllowedInComparison(low) {
			return nil, p.errorf(p.extStart(low), "Syntax error: Expression in BETWEEN must be parenthesized")
		}
		// OR has lower precedence than AND and is factored out of
		// expression_higher_prec_than_and, so it is not caught by the
		// middle-operand check above. A dedicated grammar rule reports the
		// same parenthesization error, pointing at the middle operand; see
		// the "OR" alternative of the between rule in googlesql.tm.
		if isKeyword(p.peek(), "OR") {
			return nil, p.errorf(p.extStart(low), "Syntax error: Expression in BETWEEN must be parenthesized")
		}
		if p.peek().Kind == token.EOF {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected end of statement")
		}
		// After the fully-reduced middle operand the grammar state accepts AND
		// plus every operator that could extend the operand (all already
		// consumed by parseNot); a token that fits none of them is a generic
		// "Unexpected <token>" conflict rather than a bare missing-AND error.
		if !isKeyword(p.peek(), "AND") {
			return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
		}
		p.advance() // AND
		high, err := p.parseBitwiseOr()
		if err != nil {
			return nil, err
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
	if isKeyword(p.peek(), "IN") && !suppressIn {
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
		case p.peek().Kind == token.LBRACE && p.features.Enabled(FeatureSqlGraph):
			// "X IN { graph subquery }"; see the in_operator
			// braced_graph_subquery alternative of
			// expression_higher_prec_than_and in googlesql.tm.
			query, err := p.parseBracedGraphSubquery()
			if err != nil {
				return nil, err
			}
			in.Query = query
			end = query.End()
		default:
			return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
		}
		in.Span = span(p.extStart(lhs), end)
		wrapped, err := p.applyPostfix(in)
		if err != nil {
			return nil, err
		}
		return p.finishComparison(wrapped)
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
		case isKeyword(tok, "UNKNOWN"):
			// IS [NOT] UNKNOWN produces a unary expression (see the
			// is_operator "UNKNOWN" alternative of
			// expression_higher_prec_than_and in googlesql.tm).
			p.advance()
			op := "IS UNKNOWN"
			if isNot {
				op = "IS NOT UNKNOWN"
			}
			wrapped, err := p.applyPostfix(&ast.UnaryExpression{
				Span:    span(p.extStart(lhs), tok.End),
				Op:      op,
				Operand: lhs,
			})
			if err != nil {
				return nil, err
			}
			return p.finishComparison(wrapped)
		case isKeyword(tok, "LABELED"):
			// IS [NOT] LABELED <label_expression>; see is_labeled_operator and
			// the graph_expression production in googlesql.tm.
			p.advance() // LABELED
			labelExpr, err := p.parseGraphLabelOr()
			if err != nil {
				return nil, err
			}
			return p.finishComparison(&ast.GraphIsLabeledPredicate{
				Span:    span(p.extStart(lhs), p.prevEnd()),
				IsNot:   isNot,
				Operand: lhs,
				Label:   labelExpr,
			})
		case isKeyword(tok, "SOURCE"), isKeyword(tok, "DESTINATION"):
			// IS [NOT] SOURCE [OF] / IS [NOT] DESTINATION [OF] <expr>; see the
			// edge_source_endpoint_operator / edge_dest_endpoint_operator and
			// graph_expression productions in googlesql.tm. The optional OF is
			// dropped and the operator always renders with "OF".
			kw := strings.ToUpper(tok.Image)
			p.advance() // SOURCE / DESTINATION
			if isKeyword(p.peek(), "OF") {
				p.advance() // OF
			}
			rhs, err := p.parseBitwiseOr()
			if err != nil {
				return nil, err
			}
			return p.finishComparison(&ast.BinaryExpression{
				Span:  span(p.extStart(lhs), p.extEnd(rhs)),
				Op:    "IS " + kw + " OF",
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
			return nil, p.errorf(tok.Pos, "Syntax error: Expected keyword FALSE or keyword NULL or keyword TRUE or keyword UNKNOWN but got %s", describeToken(tok))
		}
		wrapped, err := p.applyPostfix(&ast.BinaryExpression{
			Span:  span(p.extStart(lhs), p.extEnd(rhs)),
			Op:    "IS",
			IsNot: isNot,
			Left:  lhs,
			Right: rhs,
		})
		if err != nil {
			return nil, err
		}
		return p.finishComparison(wrapped)
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
	opTok := p.advance()

	// Quantified comparison "<lhs> <op> ANY|SOME|ALL <rhs>"; see the
	// any_some_all alternatives of expression_higher_prec_than_and in
	// googlesql.tm.
	if isKeyword(p.peek(), "ANY") || isKeyword(p.peek(), "SOME") || isKeyword(p.peek(), "ALL") {
		quantTok := p.advance()
		q := &ast.QuantifiedComparisonExpression{
			Op:         op,
			Lhs:        lhs,
			Location:   &ast.Location{Span: span(opTok.Pos, opTok.End)},
			Quantifier: &ast.AnySomeAllOp{Span: span(quantTok.Pos, quantTok.End), Op: strings.ToUpper(quantTok.Image)},
		}
		var hint *ast.Hint
		if p.peek().Kind == token.ATSIGN {
			hint, err = p.parseOptionalHint()
			if err != nil {
				return nil, err
			}
		}
		var end int
		switch {
		case isKeyword(p.peek(), "UNNEST"):
			if hint != nil {
				return nil, p.errorf(hint.Pos(), "Syntax error: HINTs cannot be specified on ANY/SOME/ALL clause with UNNEST")
			}
			unnest, err := p.parseUnnestExpression()
			if err != nil {
				return nil, err
			}
			q.UnnestExpr = unnest
			end = unnest.End()
		case p.peek().Kind == token.LPAREN:
			query, list, rhsEnd, err := p.parseInRhs(true)
			if err != nil {
				return nil, err
			}
			if list != nil && hint != nil {
				return nil, p.errorf(hint.Pos(), "Syntax error: HINTs cannot be specified on ANY/SOME/ALL clause with value list")
			}
			q.Query, q.List, q.Hint = query, list, hint
			end = rhsEnd
		default:
			return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
		}
		q.Span = span(p.extStart(lhs), end)
		return p.finishComparison(q)
	}

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

// finishComparison enforces non-associativity of the comparison level. The
// reference grammar splits these operators into two precedence tiers within
// expression_higher_prec_than_and (see googlesql.tm): IN and IS bind tighter
// than the comparison, LIKE, and BETWEEN operators.
//
// When the already-parsed operand (op1, n) is an IN or IS expression, it is a
// valid higher-precedence operand for the following operator's rule, which
// then rejects it via IsAllowedInComparison with a dedicated "Expression to
// the left of <op> must be parenthesized" message. When op1 is instead a
// comparison, LIKE, or BETWEEN expression, it sits at the non-associative
// comparison tier and a following comparison-level operator produces a plain
// "Unexpected <token>" conflict.
func (p *parser) finishComparison(n ast.Node) (ast.Node, error) {
	tok := p.peek()

	// Classify the following operator (op2), if any. NOT introduces the
	// NOT LIKE / NOT IN / NOT BETWEEN forms.
	opName := ""
	switch {
	case tok.Kind == token.EQ, tok.Kind == token.NEQ, tok.Kind == token.LT,
		tok.Kind == token.GT, tok.Kind == token.LTE, tok.Kind == token.GTE:
		opName = "comparison"
	case isKeyword(tok, "LIKE"):
		opName = "LIKE"
	case isKeyword(tok, "IN"):
		opName = "IN"
	case isKeyword(tok, "IS"):
		opName = "IS"
	case isKeyword(tok, "BETWEEN"):
		opName = "BETWEEN"
	case isKeyword(tok, "NOT"):
		switch {
		case isKeyword(p.peekAt(1), "LIKE"):
			opName = "LIKE"
		case isKeyword(p.peekAt(1), "IN"):
			opName = "IN"
		case isKeyword(p.peekAt(1), "BETWEEN"):
			opName = "BETWEEN"
		}
	}
	if opName == "" {
		return n, nil
	}

	// A following comparison operator immediately succeeded by ANY/SOME/ALL is
	// a quantified comparison. When its left operand is an IN/IS expression or
	// a quantified (LIKE/comparison ANY/SOME/ALL) expression, the reference
	// rejects the unparenthesized left operand with a dedicated message
	// pointing at the comparison operator; a plain comparison/LIKE/BETWEEN
	// left operand is an ordinary "Unexpected" conflict instead.
	if opName == "comparison" &&
		(isKeyword(p.peekAt(1), "ANY") || isKeyword(p.peekAt(1), "SOME") || isKeyword(p.peekAt(1), "ALL")) {
		switch n.(type) {
		case *ast.LikeExpression, *ast.QuantifiedComparisonExpression:
			return nil, p.errorf(tok.Pos, "Syntax error: Expression to the left of the comparison operator must be parenthesized")
		}
		if isInOrIsExpr(n) {
			return nil, p.errorf(tok.Pos, "Syntax error: Expression to the left of the comparison operator must be parenthesized")
		}
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}

	if isInOrIsExpr(n) {
		// op1 binds tighter; the following operator rejects the parenthesis-
		// less left operand. The message points at the operator keyword
		// itself, which for the NOT LIKE / NOT IN / NOT BETWEEN forms is the
		// token after NOT.
		pos := tok.Pos
		if isKeyword(tok, "NOT") {
			pos = p.peekAt(1).Pos
		}
		return nil, p.errorf(pos, "Syntax error: Expression to the left of %s must be parenthesized", opName)
	}
	// op1 is at the non-associative comparison tier; the following operator is
	// simply unexpected.
	return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
}

// isInOrIsExpr reports whether n is an IN or IS expression, which bind tighter
// than the comparison/LIKE/BETWEEN operators (see finishComparison).
func isInOrIsExpr(n ast.Node) bool {
	switch e := n.(type) {
	case *ast.InExpression:
		return true
	case *ast.BinaryExpression:
		// IS NULL / IS [NOT] TRUE / IS [NOT] FALSE. IS DISTINCT FROM binds at
		// the comparison tier, not here.
		return e.Op == "IS"
	}
	return false
}

// isAllowedInComparison reports whether n may appear unparenthesized as an
// operand of a non-associative comparison-level operator (e.g. the middle
// operand of BETWEEN). It mirrors ASTNode::IsAllowedInComparison in
// googlesql/parser/parse_tree.cc: parenthesized expressions are always
// allowed, while unparenthesized AND/OR/BETWEEN, comparison/LIKE/IS binary
// expressions, and unary NOT are not.
func (p *parser) isAllowedInComparison(n ast.Node) bool {
	if _, ok := p.extents[n]; ok {
		// A recorded extent means the node was parenthesized.
		return true
	}
	switch e := n.(type) {
	case *ast.AndExpr, *ast.OrExpr, *ast.BetweenExpression,
		*ast.InExpression, *ast.LikeExpression,
		*ast.QuantifiedComparisonExpression:
		return false
	case *ast.UnaryExpression:
		return e.Op != "NOT"
	case *ast.BinaryExpression:
		switch e.Op {
		case "LIKE", "IS", "=", "!=", "<>", ">", "<", ">=", "<=":
			return false
		}
		return true
	}
	return true
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
	// After an in-list expression, only "," (continue the list) or ")" (close
	// it) are valid. On any other token the reference reports an expected ","
	// rather than ")"; see the in_list_two_or_more_prefix reduction in
	// parenthesized_in_rhs in googlesql.tm.
	if p.peek().Kind != token.RPAREN {
		eerr := p.errorf(p.peek().Pos, `Syntax error: Expected "," but got %s`, describeToken(p.peek()))
		// The in-list interpretation wins ties against an abandoned query
		// parse: the reference reduces the parenthesized rhs as an in-list and
		// reports the expected "," even when a query parse reached the same
		// point (e.g. "IN((select 1) foo)").
		if qerr != nil {
			var ea, eb *Error
			if errors.As(qerr, &ea) && errors.As(eerr, &eb) && ea.Offset > eb.Offset {
				return nil, nil, 0, qerr
			}
		}
		return nil, nil, 0, eerr
	}
	rparen := p.advance()
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
	lhs, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	for {
		tok := p.peek()
		if tok.Kind != token.LSHIFT && tok.Kind != token.RSHIFT {
			return lhs, nil
		}
		p.advance()
		rhs, err := p.parseAdditive()
		if err != nil {
			return nil, err
		}
		lhs = &ast.BitwiseShiftExpression{
			Span:             span(p.extStart(lhs), p.extEnd(rhs)),
			IsLeftShift:      tok.Kind == token.LSHIFT,
			Lhs:              lhs,
			OperatorLocation: &ast.Location{Span: span(tok.Pos, tok.End)},
			Rhs:              rhs,
		}
	}
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
// checkChainedCallBase reports the reference error for a chained function call
// whose base is a bare integer or floating point literal (which is only valid
// when parenthesized, e.g. "(1).x()" rather than "1.x()"). The error is
// reported at the opening "(" of the call. See the chained-call branch of
// function_call_expression_base in googlesql.tm.
func (p *parser) checkChainedCallBase(base ast.Node) error {
	switch base.(type) {
	case *ast.IntLiteral, *ast.FloatLiteral:
		if _, parenthesized := p.extents[base]; !parenthesized {
			return p.errorf(p.peek().Pos, `Syntax error: Unexpected "("`)
		}
	}
	return nil
}

func (p *parser) parsePostfix() (ast.Node, error) {
	expr, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	return p.applyPostfix(expr)
}

// applyPostfix consumes any trailing postfix operators (".identifier",
// ".(path)" generalized field access, "[expression]" array element access, and
// chained function calls) that follow an already-parsed expression. These
// operators bind at PRIMARY_PRECEDENCE in the reference (see the
// expression_higher_prec_than_and rules in googlesql.tm), so they may attach to
// the result of a "closed" higher-level construct such as IS NULL or IN(...)
// as well as to a primary expression.
func (p *parser) applyPostfix(expr ast.Node) (ast.Node, error) {
	for {
		switch p.peek().Kind {
		case token.DOT:
			next := p.peekAt(1)
			if next.Kind == token.LPAREN {
				// "expression . ( path )" generalized field access; see the
				// expression_higher_prec_than_and "." "(" path_expression ")"
				// rule in googlesql.tm.
				p.advance() // .
				p.advance() // (
				inner, err := p.parseGeneralizedFieldInnerPath()
				if err != nil {
					return nil, err
				}
				rparen := p.advance() // ) (guaranteed by parseGeneralizedFieldInnerPath)
				expr = &ast.DotGeneralizedField{Span: span(p.extStart(expr), rparen.End), Expr: expr, Path: inner}
				continue
			}
			if next.Kind != token.IDENT && next.Kind != token.QUOTED_IDENT {
				if next.Kind == token.STAR {
					// Stop in front of a select column's ".*" and record
					// which expression it binds to; see
					// select_column_dot_star in googlesql.tm.
					if p.allowDotStar {
						p.dotStarTarget = expr
					}
					return expr, nil
				}
				// A primary expression followed by "." requires an identifier
				// (generalized field access); commit to the "." and report the
				// error at the following token rather than treating the "." as
				// unexpected. See the primary_expression "." identifier rule in
				// googlesql.tm.
				p.advance() // .
				return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
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
			// (generalized) path expression; anything else is an error (see
			// function_call_expression_base in googlesql.tm).
			switch e := expr.(type) {
			case *ast.PathExpression:
				// A function call operator applies only to an *unparenthesized*
				// path expression; a parenthesized path such as "(foo)(...)" is
				// rejected here. An unparenthesized path followed by "(" is
				// already consumed as a call at the primary level. See
				// function_call_expression_base in googlesql.tm.
				if _, parenthesized := p.extents[expr]; parenthesized {
					return nil, p.errorf(p.peek().Pos, "Syntax error: Function call cannot be applied to this expression. Function calls require a path, e.g. a.b.c()")
				}
				return expr, nil
			case *ast.DotIdentifier:
				// "base.method(...)" is a chained function call, unless the
				// whole ".method" access is itself parenthesized. The method
				// name becomes the function path and the base becomes the
				// first argument. See the chained-call branch of
				// function_call_expression_base in googlesql.tm.
				if _, parenthesized := p.extents[e]; !parenthesized {
					if err := p.checkChainedCallBase(e.Expr); err != nil {
						return nil, err
					}
					methodPath := &ast.PathExpression{Span: span(e.Name.Pos(), e.Name.End()), Names: []*ast.Identifier{e.Name}}
					call, err := p.finishFunctionCallBase(methodPath, e.Expr, p.extStart(e))
					if err != nil {
						return nil, err
					}
					expr = call
					continue
				}
				// A parenthesized ".method" access cannot take a call; see the
				// non-chained else branch of function_call_expression_base in
				// googlesql.tm.
				return nil, p.errorf(p.peek().Pos, "Syntax error: Function call cannot be applied to this expression. Function calls require a path, e.g. a.b.c()")
			case *ast.DotGeneralizedField:
				// "base.(path)(...)" is a chained function call using the
				// generalized field's path as the function name.
				if _, parenthesized := p.extents[e]; !parenthesized {
					if err := p.checkChainedCallBase(e.Expr); err != nil {
						return nil, err
					}
					call, err := p.finishFunctionCallBase(e.Path, e.Expr, p.extStart(e))
					if err != nil {
						return nil, err
					}
					expr = call
					continue
				}
				return nil, p.errorf(p.peek().Pos, "Syntax error: Function call cannot be applied to this expression. Function calls require a path, e.g. a.b.c()")
			case *ast.FunctionCall:
				// A parenthesized function call falls through to the
				// "cannot be applied" error below; see the
				// function_call_expression_with_args_prefix rule in
				// googlesql.tm.
				if _, parenthesized := p.extents[expr]; !parenthesized {
					return nil, p.errorf(p.peek().Pos, "Syntax error: Double function call parentheses")
				}
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

// parseGeneralizedFieldInnerPath parses the path_expression inside a ".(...)"
// generalized field access, stopping in front of (but not consuming) the
// closing ")". Its error wording follows the reference LALR states rather than
// parsePathExpression: at the start of the path a non-identifier is reported as
// "Unexpected <token>", and after a path component the parser expects ")" or
// "." (see the generalized field access rules in googlesql.tm).
func (p *parser) parseGeneralizedFieldInnerPath() (*ast.PathExpression, error) {
	tok := p.peek()
	if (tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT) || p.isReserved(tok) {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	first := p.parseIdentifierToken(p.advance())
	path := &ast.PathExpression{Span: span(first.Pos(), first.End()), Names: []*ast.Identifier{first}}
	for {
		switch p.peek().Kind {
		case token.RPAREN:
			return path, nil
		case token.DOT:
			p.advance() // .
			// After a ".", the reference tokenizer allows any keyword as an
			// identifier (IDENTIFIER_DOT mode), so keywords are accepted here.
			nt := p.peek()
			if nt.Kind != token.IDENT && nt.Kind != token.QUOTED_IDENT {
				return nil, p.errorf(nt.Pos, "Syntax error: Unexpected %s", describeToken(nt))
			}
			ident := p.parseIdentifierToken(p.advance())
			path.Names = append(path.Names, ident)
			path.Stop = ident.End()
		default:
			return nil, p.errorf(p.peek().Pos, `Syntax error: Expected ")" or "." but got %s`, describeToken(p.peek()))
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
	// An unquoted identifier fused to a preceding literal (the reference's
	// ATTACHED_ALIAS token, e.g. the "d" in "@1d") cannot begin an expression;
	// the reference reports it as unexpected. See the select_column_expr rule
	// in googlesql.tm.
	if p.currentIsAttachedAlias() {
		return nil, p.errorf(tok.Pos, `Syntax error: Unexpected "%s"`, tok.Image)
	}
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
	case token.LBRACE:
		// A bare "{...}" in expression position is a braced (proto)
		// constructor; see braced_constructor in googlesql.tm.
		return p.parseBracedConstructor()
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
	case token.ATSIGN:
		// A bare "@" (the lexer did not fuse it with the following name, e.g.
		// because of whitespace or a backquoted name) begins a named
		// parameter "@" identifier. Any keyword immediately after "@" is
		// treated as an identifier; see the ATSIGN lookback case in
		// lookahead_transformer.cc.
		p.advance() // @
		next := p.peek()
		if next.Kind == token.FLOAT {
			// "@" immediately followed by a digit is an integer hint opener
			// (KW_OPEN_INTEGER_HINT; the lexer's lookahead only treats a
			// digit-leading number this way). The hint rule then requires an
			// integer literal, so a floating point literal such as "1.1" is
			// reported as the wrong literal kind. A float that starts with "."
			// (e.g. "@.1") is not a hint opener, so it is simply unexpected.
			// See the ATSIGN case in lookahead_transformer.cc and the hint rule
			// in googlesql.tm.
			if next.Image != "" && next.Image[0] >= '0' && next.Image[0] <= '9' {
				return nil, p.errorf(next.Pos, "Syntax error: Expected integer literal but got %s", describeToken(next))
			}
			return nil, p.errorf(next.Pos, "Syntax error: Unexpected %s", describeToken(next))
		}
		if next.Kind == token.INT {
			// An integer hint opener followed by the integer literal forms a
			// complete hint, which is not valid in expression position; the
			// reference reports the error at the token following the integer.
			p.advance() // integer literal
			after := p.peek()
			return nil, p.errorf(after.Pos, "Syntax error: Unexpected %s", describeToken(after))
		}
		if next.Kind != token.IDENT && next.Kind != token.QUOTED_IDENT {
			return nil, p.errorf(next.Pos, "Syntax error: Unexpected %s", describeToken(next))
		}
		name := p.parseIdentifierToken(p.advance())
		return &ast.ParameterExpr{Span: span(tok.Pos, name.End()), Name: name}, nil
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
		case isKeyword(tok, "EXTRACT") && p.peekAt(1).Kind == token.LPAREN:
			// EXTRACT is non-reserved: it is only the extract keyword when
			// followed by "(" (see keywords.cc); otherwise it falls through
			// to the identifier cases below.
			return p.parseExtractExpression()
		case isKeyword(tok, "REPLACE_FIELDS") && p.peekAt(1).Kind == token.LPAREN:
			// REPLACE_FIELDS is non-reserved: it is only the replace-fields
			// keyword when followed by "(" (see the replace_fields_expression
			// rule in googlesql.tm); otherwise it falls through to the
			// identifier cases below.
			return p.parseReplaceFieldsExpression()
		case isKeyword(tok, "ARRAY") && p.peekAt(1).Kind == token.LBRACE && p.features.Enabled(FeatureSqlGraph):
			// "ARRAY { graph subquery }"; see the ARRAY braced_graph_subquery
			// alternative of expression_subquery_with_keyword in googlesql.tm.
			p.advance() // ARRAY
			return p.parseBracedGraphExpressionSubquery(tok.Pos, "ARRAY", nil)
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
		case isKeyword(tok, "ARRAY"):
			// A bare reserved "ARRAY" in expression position must begin an array
			// constructor. Having ruled out "[", "(", and "<" above, the LALR
			// parser is left expecting the typed-constructor "<". See
			// array_constructor and array_constructor_prefix_no_expressions in
			// googlesql.tm.
			p.advance() // ARRAY
			return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "<" but got %s`, describeToken(p.peek()))
		case isKeyword(tok, "VALUE") && p.features.Enabled(FeatureSqlGraph) &&
			(p.peekAt(1).Kind == token.LBRACE || p.peekAt(1).Kind == token.ATSIGN):
			// "VALUE hint? { graph subquery }"; see the VALUE
			// braced_graph_subquery alternative of
			// expression_subquery_with_keyword in googlesql.tm.
			p.advance() // VALUE
			hint, err := p.parseOptionalHint()
			if err != nil {
				return nil, err
			}
			if p.peek().Kind != token.LBRACE {
				return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "{" but got %s`, describeToken(p.peek()))
			}
			return p.parseBracedGraphExpressionSubquery(tok.Pos, "VALUE", hint)
		case isKeyword(tok, "EXISTS") && (p.peekAt(1).Kind == token.LPAREN || p.peekAt(1).Kind == token.ATSIGN ||
			(p.peekAt(1).Kind == token.LBRACE && p.features.Enabled(FeatureSqlGraph))):
			// EXISTS takes an optional hint before the subquery; see
			// expression_subquery_with_keyword in googlesql.tm. With the graph
			// feature the subquery may be a braced graph subquery.
			p.advance() // EXISTS; the subquery's span starts at the keyword.
			hint, err := p.parseOptionalHint()
			if err != nil {
				return nil, err
			}
			if p.features.Enabled(FeatureSqlGraph) && p.peek().Kind == token.LBRACE {
				return p.parseExistsGraphSubquery(tok.Pos, hint)
			}
			node, err := p.parseModifiedSubquery(tok.Pos, "EXISTS")
			if err != nil {
				return nil, err
			}
			node.(*ast.ExpressionSubquery).Hint = hint
			return node, nil
		case p.startsWithExpression():
			// "WITH ( var AS expr [, ...], final_expr )" is a WITH expression;
			// the reference lexer retokenizes the leading WITH as
			// KW_WITH_STARTING_WITH_EXPRESSION on this same lookahead. See
			// with_expression in googlesql.tm.
			return p.parseWithExpression()
		case isKeyword(tok, "NEW"):
			return p.parseNewConstructor()
		case isKeyword(tok, "STRUCT"):
			// STRUCT always starts a struct constructor in expression
			// position: "STRUCT(...)", "STRUCT<...>(...)", or the braced
			// forms "STRUCT {...}" and "STRUCT<...> {...}". Anything else
			// after the keyword is a syntax error reported there.
			return p.parseStructConstructorWithKeyword()
		case isKeyword(tok, "CASE"):
			return p.parseCaseExpression()
		case isTypedLiteralPrefix(tok.Image) && p.peekAt(1).Kind == token.STRING:
			// A typed-literal keyword directly followed by a string literal
			// (e.g. DATE '2021-01-01', JSON '{}', NUMERIC '1'); see
			// date_or_time_literal / numeric_literal / json_literal in
			// googlesql.tm. Otherwise the keyword is an ordinary identifier
			// or function name and falls through below.
			return p.parseTypedLiteral()
		case isKeyword(tok, "RANGE") && p.peekAt(1).Kind == token.LT:
			// "RANGE<type> '...'" is a range literal; see range_literal in
			// googlesql.tm.
			return p.parseRangeLiteral()
		case isKeyword(tok, "INTERVAL"):
			// "INTERVAL <expr> <datepart> [TO <datepart>]"; see
			// interval_expression in googlesql.tm.
			return p.parseIntervalExpr()
		case isKeyword(tok, "GROUPING") || isKeyword(tok, "IF"):
			// GROUPING and IF are non-reserved keywords, but in expression
			// position they can only name a function call requiring "(":
			// they are not listed in keyword_as_identifier, so a bare
			// GROUPING/IF is not a valid path expression. See
			// function_name_from_keyword and function_call_expression_base in
			// googlesql.tm. (RANGE is also in that grammar rule but has its own
			// range-literal handling above and otherwise parses as a path.)
			kw := p.advance()
			ident := &ast.Identifier{Span: span(kw.Pos, kw.End), Name: kw.Image}
			path := &ast.PathExpression{Span: span(kw.Pos, kw.End), Names: []*ast.Identifier{ident}}
			if p.peek().Kind != token.LPAREN {
				return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" but got %s`, describeToken(p.peek()))
			}
			return p.finishFunctionCall(path)
		case isReservedFunctionNameKeyword(tok):
			// COLLATE, LEFT, and RIGHT are reserved keywords, but in expression
			// position they can name a function call: "COLLATE(...)". They must
			// be followed by "(". See function_name_from_keyword in
			// googlesql.tm. (RANGE is non-reserved and reaches parsePathOrCall
			// below; GROUPING and IF are handled just above.)
			kw := p.advance()
			ident := &ast.Identifier{Span: span(kw.Pos, kw.End), Name: kw.Image}
			path := &ast.PathExpression{Span: span(kw.Pos, kw.End), Names: []*ast.Identifier{ident}}
			if p.peek().Kind != token.LPAREN {
				return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" but got %s`, describeToken(p.peek()))
			}
			return p.finishFunctionCall(path)
		case p.isReserved(tok):
			if err := p.exceptClashError(); err != nil {
				return nil, err
			}
			if isKeyword(tok, "OVER") {
				// When the OVER keyword is used in the wrong place, we tell
				// the user exactly where it can be used; see the KW_OVER
				// special case in googlesql/parser/parser_internal.cc.
				return nil, p.errorf(tok.Pos, "Syntax error: OVER keyword must follow a function call")
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

// parseStructConstructorWithKeyword parses "STRUCT( args )" or
// "STRUCT<...>( args )" with the STRUCT keyword as the next token; see
// struct_constructor_prefix_with_keyword in googlesql.tm.
func (p *parser) parseStructConstructorWithKeyword() (ast.Node, error) {
	start := p.peek().Pos
	var structType *ast.StructType
	if p.peekAt(1).Kind == token.LPAREN || p.peekAt(1).Kind == token.LBRACE {
		p.advance() // STRUCT
	} else {
		// Anything but "(" or "{" after STRUCT must open a struct type;
		// parseStructType reports `Expected "<"` otherwise, matching the
		// reference parser.
		typ, err := p.parseStructType()
		if err != nil {
			return nil, err
		}
		structType = typ
	}
	if p.peek().Kind == token.LBRACE {
		// "STRUCT { ... }" or "STRUCT<...> { ... }"; see
		// struct_braced_constructor in googlesql.tm.
		ctor, err := p.parseBracedConstructor()
		if err != nil {
			return nil, err
		}
		return &ast.StructBracedConstructor{
			Span:        span(start, ctor.End()),
			StructType:  structType,
			Constructor: ctor,
		}, nil
	}
	s := &ast.StructConstructorWithKeyword{Span: span(start, 0), StructType: structType}
	// After a struct type the reference parser can also start a braced
	// constructor, so its error message offers "{" too.
	if _, err := p.expect(token.LPAREN, `"(" or "{"`); err != nil {
		return nil, err
	}
	if p.peek().Kind != token.RPAREN {
		for {
			arg, err := p.parseStructConstructorArg()
			if err != nil {
				return nil, err
			}
			s.Fields = append(s.Fields, arg)
			if p.peek().Kind != token.COMMA {
				break
			}
			p.advance()
		}
	}
	rparen, err := p.expect(token.RPAREN, `")" or ","`)
	if err != nil {
		return nil, err
	}
	s.Stop = rparen.End
	return s, nil
}

// parseStructConstructorArg parses "expression [AS alias]"; the alias
// requires the AS keyword. See struct_constructor_arg and
// as_alias_with_required_as in googlesql.tm.
func (p *parser) parseStructConstructorArg() (*ast.StructConstructorArg, error) {
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	arg := &ast.StructConstructorArg{Span: span(expr.Pos(), expr.End()), Expression: expr}
	if isKeyword(p.peek(), "AS") {
		alias, err := p.parseOptionalAlias()
		if err != nil {
			return nil, err
		}
		arg.Alias = alias
		arg.Stop = alias.End()
	}
	return arg, nil
}

// startsWithExpression reports whether the next tokens begin a WITH expression
// "WITH ( identifier AS ...". The reference lexer retokenizes such a WITH as
// KW_WITH_STARTING_WITH_EXPRESSION; see the KW_WITH case in
// lookahead_transformer.cc and with_expression in googlesql.tm.
func (p *parser) startsWithExpression() bool {
	return isKeyword(p.peek(), "WITH") && p.peekAt(1).Kind == token.LPAREN &&
		(p.peekAt(2).Kind == token.IDENT || p.peekAt(2).Kind == token.QUOTED_IDENT) &&
		!p.isReserved(p.peekAt(2)) && isKeyword(p.peekAt(3), "AS")
}

// parseWithExpression parses "WITH ( var AS expr [, ...], result_expr )" with
// the WITH keyword as the next token; see with_expression in googlesql.tm. Each
// variable definition becomes a SelectColumn (value expression with an alias
// spanning "name AS"), and the trailing expression becomes the result.
func (p *parser) parseWithExpression() (ast.Node, error) {
	withTok := p.advance() // WITH
	p.advance()            // (
	list := &ast.SelectList{}
	for {
		nameTok := p.advance() // identifier (guaranteed by caller / loop check)
		ident := p.parseIdentifierToken(nameTok)
		asTok, err := p.expectKeyword("AS")
		if err != nil {
			return nil, err
		}
		value, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		alias := &ast.Alias{Span: span(ident.Pos(), asTok.End), Identifier: ident}
		col := &ast.SelectColumn{Span: span(ident.Pos(), p.extEnd(value)), Expr: value, Alias: alias}
		list.Columns = append(list.Columns, col)
		if _, err := p.expect(token.COMMA, `","`); err != nil {
			return nil, err
		}
		// Another "name AS ..." starts another variable; otherwise what follows
		// the comma is the trailing result expression.
		if !((p.peek().Kind == token.IDENT || p.peek().Kind == token.QUOTED_IDENT) &&
			!p.isReserved(p.peek()) && isKeyword(p.peekAt(1), "AS")) {
			break
		}
	}
	list.Span = span(list.Columns[0].Pos(), list.Columns[len(list.Columns)-1].End())
	result, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	return &ast.WithExpression{
		Span:      span(withTok.Pos, rparen.End),
		Variables: list,
		Expr:      result,
	}, nil
}

// parseNewConstructor parses "NEW type_name(arg, ...)" or the braced form
// "NEW type_name { ... }" with the NEW keyword as the next token; see
// new_constructor and braced_new_constructor in googlesql.tm. Only named
// types are allowed after NEW.
func (p *parser) parseNewConstructor() (ast.Node, error) {
	newTok := p.advance() // NEW
	tok := p.peek()
	if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	path, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	typ := &ast.SimpleType{Span: span(path.Pos(), path.End()), Name: path}
	if p.peek().Kind == token.LBRACE {
		ctor, err := p.parseBracedConstructor()
		if err != nil {
			return nil, err
		}
		return &ast.BracedNewConstructor{
			Span:        span(newTok.Pos, ctor.End()),
			TypeName:    typ,
			Constructor: ctor,
		}, nil
	}
	if _, err := p.expect(token.LPAREN, `"(" or "{"`); err != nil {
		return nil, err
	}
	n := &ast.NewConstructor{Span: span(newTok.Pos, 0), TypeName: typ}
	if p.peek().Kind != token.RPAREN {
		for {
			arg, err := p.parseNewConstructorArg()
			if err != nil {
				return nil, err
			}
			n.Args = append(n.Args, arg)
			if p.peek().Kind != token.COMMA {
				break
			}
			p.advance()
		}
	}
	rparen, err := p.expect(token.RPAREN, `")" or ","`)
	if err != nil {
		return nil, err
	}
	n.Stop = rparen.End
	return n, nil
}

// parseNewConstructorArg parses "expression [AS identifier | AS ( path )]";
// see new_constructor_arg in googlesql.tm. Aliases require the AS keyword,
// and multi-part alias paths must be parenthesized.
func (p *parser) parseNewConstructorArg() (*ast.NewConstructorArg, error) {
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	arg := &ast.NewConstructorArg{Span: span(expr.Pos(), expr.End()), Expression: expr}
	if isKeyword(p.peek(), "AS") {
		p.advance()
		if p.peek().Kind == token.LPAREN {
			p.advance()
			path, err := p.parsePathExpression()
			if err != nil {
				return nil, err
			}
			rparen, err := p.expect(token.RPAREN, `")"`)
			if err != nil {
				return nil, err
			}
			arg.OptionalPathExpression = path
			arg.Stop = rparen.End
			return arg, nil
		}
		tok := p.peek()
		if (tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT) || p.isReserved(tok) {
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
		}
		ident := p.parseIdentifierToken(p.advance())
		arg.OptionalIdentifier = ident
		arg.Stop = ident.End()
	}
	return arg, nil
}

// parseBracedConstructor parses "{ [field [, field ...] [,]] }" with the
// opening brace as the next token; see braced_constructor in googlesql.tm.
// Fields may be separated by commas or, proto text-format style, by
// whitespace alone; in the whitespace form the next field's name must be a
// plain path expression.
func (p *parser) parseBracedConstructor() (*ast.BracedConstructor, error) {
	lbrace := p.peek()
	if !p.features.Enabled(FeatureBracedProtoConstructors) {
		// The reference emits this via MakeSyntaxError without a "Syntax
		// error: " prefix; see the braced_constructor guard in googlesql.tm.
		return nil, p.errorf(lbrace.Pos, "Braced constructors are not supported")
	}
	p.advance() // {
	b := &ast.BracedConstructor{Span: span(lbrace.Pos, 0)}
	if p.peek().Kind != token.RBRACE {
		commaSeparated := false
		for {
			field, err := p.parseBracedConstructorField(commaSeparated)
			if err != nil {
				return nil, err
			}
			b.Fields = append(b.Fields, field)
			if p.peek().Kind == token.COMMA {
				p.advance()
				if p.peek().Kind == token.RBRACE {
					break // trailing comma
				}
				commaSeparated = true
				continue
			}
			if p.peek().Kind == token.RBRACE {
				break
			}
			// Without a comma the next field's name must be a plain path
			// expression (see braced_constructor_field_following_omitted_comma
			// in googlesql.tm); anything else is a syntax error.
			next := p.peek()
			if (next.Kind != token.IDENT && next.Kind != token.QUOTED_IDENT) || p.isReserved(next) {
				return nil, p.errorf(next.Pos, "Syntax error: Unexpected %s", describeToken(next))
			}
			commaSeparated = false
		}
	}
	rbrace := p.advance() // }
	b.Stop = rbrace.End
	return b, nil
}

// parseBracedConstructorField parses one "lhs: expression" or "lhs { ... }"
// braced constructor field; see braced_constructor_field in googlesql.tm.
// The field name is a path expression or a parenthesized extension path
// "(path.to.extension)".
func (p *parser) parseBracedConstructorField(commaSeparated bool) (*ast.BracedConstructorField, error) {
	tok := p.peek()
	lhs := &ast.BracedConstructorLhs{Span: span(tok.Pos, 0)}
	switch {
	case tok.Kind == token.LPAREN:
		// "(path.to.extension)"; see braced_constructor_extension_expression
		// in googlesql.tm. The lhs span includes the parentheses.
		p.advance()
		path, err := p.parsePathExpression()
		if err != nil {
			return nil, err
		}
		rparen, err := p.expect(token.RPAREN, `")"`)
		if err != nil {
			return nil, err
		}
		lhs.Expression = path
		lhs.Stop = rparen.End
	case (tok.Kind == token.IDENT || tok.Kind == token.QUOTED_IDENT) && !p.isReserved(tok):
		path, err := p.parsePathExpression()
		if err != nil {
			return nil, err
		}
		lhs.Expression = path
		lhs.Stop = path.End()
	default:
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	value := &ast.BracedConstructorFieldValue{}
	switch p.peek().Kind {
	case token.COLON:
		p.advance()
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		// The field value's location follows the "expression" symbol, which for
		// a parenthesized expression covers the parentheses even though the
		// inner node keeps its unparenthesized location; see
		// braced_constructor_field in googlesql.tm.
		value.Span = span(p.extStart(expr), p.extEnd(expr))
		value.Expression = expr
		value.ColonPrefixed = true
	case token.LBRACE:
		// Sub-message form "lhs { ... }" without a colon.
		sub, err := p.parseBracedConstructor()
		if err != nil {
			return nil, err
		}
		value.Span = span(sub.Pos(), sub.End())
		value.Expression = sub
	default:
		return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
	}
	return &ast.BracedConstructorField{
		Span:           span(lhs.Pos(), value.End()),
		Lhs:            lhs,
		Value:          value,
		CommaSeparated: commaSeparated,
	}, nil
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
				// After an element the array expects either "," (another
				// element) or "]" (close); see array_constructor in
				// googlesql.tm.
				if p.peek().Kind != token.RBRACKET {
					return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "," or "]" but got %s`, describeToken(p.peek()))
				}
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

// isCurrentDatetimeName reports whether name is one of the CURRENT_* date/time
// functions that may be called without parentheses (CURRENT_DATE,
// CURRENT_TIME, CURRENT_DATETIME, CURRENT_TIMESTAMP); see the path_expression
// rule in googlesql.tm.
func isCurrentDatetimeName(name string) bool {
	switch strings.ToLower(name) {
	case "current_date", "current_time", "current_datetime", "current_timestamp":
		return true
	}
	return false
}

// parsePathOrCall parses a path expression, possibly followed by a function
// call argument list.
func (p *parser) parsePathOrCall() (ast.Node, error) {
	// A bare, unquoted reference to a CURRENT_DATE/TIME/DATETIME/TIMESTAMP
	// function is parsed as a parenthesis-less function call; see the
	// path_expression rule in googlesql.tm that sets
	// is_current_date_time_without_parentheses.
	first := p.peek()
	isCurrentDatetime := first.Kind == token.IDENT && isCurrentDatetimeName(first.Image)
	prevAllow := p.allowGeneralizedField
	p.allowGeneralizedField = true
	path, err := p.parsePathExpression()
	p.allowGeneralizedField = prevAllow
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != token.LPAREN {
		if isCurrentDatetime && len(path.Names) == 1 {
			return &ast.FunctionCall{Span: span(path.Pos(), path.End()), Function: path}, nil
		}
		return path, nil
	}
	return p.finishFunctionCall(path)
}

// finishFunctionCall parses the argument list and trailing modifiers of a
// function call whose function path has already been parsed and whose opening
// "(" is the next token; see function_call_expression in googlesql.tm. It is
// shared by ordinary path calls and by the function_name_from_keyword calls
// (IF, GROUPING, LEFT, RIGHT, COLLATE, RANGE) whose names are keywords.
func (p *parser) finishFunctionCall(path *ast.PathExpression) (ast.Node, error) {
	return p.finishFunctionCallBase(path, nil, path.Pos())
}

// finishFunctionCallBase parses the argument list and trailing modifiers of a
// function call whose function path has already been parsed and whose opening
// "(" is the next token. If base is non-nil the call is a chained function
// call ("expr.method(...)"): the base expression is added as the first
// argument and is_chained_call is set, and the call's location starts at
// start (the base expression's outer start). See the chained-call branch of
// function_call_expression_base in googlesql.tm.
func (p *parser) finishFunctionCallBase(path *ast.PathExpression, base ast.Node, start int) (ast.Node, error) {
	var err error
	p.advance() // consume (
	call := &ast.FunctionCall{Span: span(start, 0), Function: path}
	if base != nil {
		call.IsChained = true
		call.Args = append(call.Args, base)
	}
	if isKeyword(p.peek(), "DISTINCT") {
		p.advance()
		call.Distinct = true
	}
	// null_handling_modifier in googlesql.tm: after the argument list, a leading
	// "IGNORE" / "RESPECT" begins the modifier and requires "NULLS". Both are
	// reserved keywords, so they can never be arguments; with an empty argument
	// list they still begin a modifier rather than arguments.
	nullHandlingAhead := func() bool {
		return isKeyword(p.peek(), "IGNORE") || isKeyword(p.peek(), "RESPECT")
	}
	// WHERE, GROUP, HAVING, ORDER, and LIMIT are reserved keywords that cannot
	// start an expression; with an empty argument list they begin trailing
	// modifiers.
	if p.peek().Kind != token.RPAREN && !nullHandlingAhead() &&
		!isKeyword(p.peek(), "WHERE") && !isKeyword(p.peek(), "GROUP") &&
		!isKeyword(p.peek(), "HAVING") && !isKeyword(p.peek(), "ORDER") &&
		!isKeyword(p.peek(), "LIMIT") && !isKeyword(p.peek(), "WITH") {
		firstArg := true
		explicitArgs := 0
		for {
			var arg ast.Node
			switch {
			case p.peek().Kind == token.STAR:
				// A "*" is only a valid argument as the very first one (e.g.
				// COUNT(*) or ANON_COUNT(*, ...)); in any later position it is a
				// syntax error. Subsequent arguments are function_call_argument,
				// which has no "*" alternative. See
				// function_call_expression_with_args_prefix in googlesql.tm.
				if explicitArgs > 0 {
					return nil, p.errorf(p.peek().Pos, `Syntax error: Unexpected "*"`)
				}
				star := p.advance()
				arg = &ast.Star{Span: span(star.Pos, star.End), Image: star.Image}
			case isKeyword(p.peek(), "SEQUENCE") && !isKeyword(p.peekAt(1), "CLAMPED"):
				// sequence_arg: "SEQUENCE" path_expression; see
				// function_call_argument in googlesql.tm. When SEQUENCE is
				// directly followed by CLAMPED the reference lexer treats
				// SEQUENCE as an ordinary identifier so that "CLAMPED BETWEEN"
				// can be a modifier (see the KW_SEQUENCE case in
				// lookahead_transformer.cc); such a SEQUENCE falls through to
				// the default expression case below.
				seqTok := p.advance() // SEQUENCE
				seqPath, perr := p.parsePathExpression()
				if perr != nil {
					return nil, perr
				}
				arg = &ast.SequenceArg{Span: span(seqTok.Pos, seqPath.End()), Sequence: seqPath}
			case p.peekAt(1).Kind == token.LAMBDA &&
				(p.peek().Kind == token.IDENT || p.peek().Kind == token.QUOTED_IDENT) &&
				!p.isReserved(p.peek()):
				// named_argument: identifier "=>" (expression |
				// input_table_argument); see function_call_argument in
				// googlesql.tm.
				name := p.parseIdentifierToken(p.advance())
				p.advance() // consume =>
				if p.atInputTableArgument() {
					input := p.parseInputTableArgument()
					arg = &ast.NamedArgument{
						Span:  span(name.Pos(), input.End()),
						Name:  name,
						Value: input,
					}
					break
				}
				value, verr := p.parseExpression()
				if verr != nil {
					return nil, verr
				}
				if p.peek().Kind == token.ARROW {
					value, verr = p.finishLambda(value)
					if verr != nil {
						return nil, verr
					}
				}
				arg = &ast.NamedArgument{
					Span:  span(name.Pos(), p.extEnd(value)),
					Name:  name,
					Value: value,
				}
			case p.peek().Kind == token.LPAREN && p.peekAt(1).Kind == token.RPAREN &&
				p.peekAt(2).Kind == token.ARROW:
				// No-param lambda "() -> expr"; the empty argument list is an
				// empty StructConstructorWithParens. See lambda_argument_list in
				// googlesql.tm.
				lp := p.advance() // (
				rp := p.advance() // )
				params := &ast.StructConstructorWithParens{Span: span(lp.Pos, rp.End)}
				arg, err = p.finishLambda(params)
				if err != nil {
					return nil, err
				}
			default:
				if isKeyword(p.peek(), "SELECT") {
					// A function argument cannot be a bare query; see the
					// "SELECT" alternative of function_call_argument in
					// googlesql.tm. Dedicated error without a "Syntax error: "
					// prefix.
					return nil, p.errorf(p.peek().Pos, "Each function argument is an expression, not a query; to use a query as an expression, the query must be wrapped with additional parentheses to make it a scalar subquery expression")
				}
				startTok := p.peek()
				arg, err = p.parseExpression()
				if err != nil {
					// In the empty-argument-list state (nothing parsed yet),
					// the reference LALR parser can reduce to a zero-argument
					// call, so an unparseable first token is reported as a
					// missing ")" rather than "Unexpected ...". See the empty
					// argument list alternative of function_call_expression in
					// googlesql.tm.
					if firstArg {
						var e *Error
						if errors.As(err, &e) && e.Offset == startTok.Pos {
							return nil, p.errorf(startTok.Pos, `Syntax error: Expected ")" but got %s`, describeToken(startTok))
						}
					}
					return nil, err
				}
				if p.peek().Kind == token.ARROW {
					arg, err = p.finishLambda(arg)
					if err != nil {
						return nil, err
					}
				}
				// function_call_argument: expression as_alias_with_required_as?;
				// see googlesql.tm. An "AS alias" wraps the argument in an
				// ExpressionWithAlias node. The AS keyword is required, so a bare
				// trailing identifier is left for the caller to reject.
				if isKeyword(p.peek(), "AS") {
					alias, aerr := p.parseOptionalAlias()
					if aerr != nil {
						return nil, aerr
					}
					arg = &ast.ExpressionWithAlias{
						Span:       span(p.extStart(arg), alias.End()),
						Expression: arg,
						Alias:      alias,
					}
				}
			}
			call.Args = append(call.Args, arg)
			firstArg = false
			explicitArgs++
			if p.peek().Kind != token.COMMA {
				break
			}
			p.advance()
		}
	}
	if nullHandlingAhead() {
		if isKeyword(p.peek(), "IGNORE") {
			call.NullHandling = "IGNORE_NULLS"
		} else {
			call.NullHandling = "RESPECT_NULLS"
		}
		p.advance() // consume IGNORE or RESPECT
		if _, err := p.expectKeyword("NULLS"); err != nil {
			return nil, err
		}
	}
	// opt_where_clause in function_call_expression: a multi-level aggregation
	// WHERE modifier; see googlesql.tm.
	if isKeyword(p.peek(), "WHERE") {
		where, err := p.parseWhereClause()
		if err != nil {
			return nil, err
		}
		call.Where = where
	}
	// having_modifier in googlesql.tm: "HAVING" ("MAX" | "MIN") expression.
	if isKeyword(p.peek(), "HAVING") {
		havingTok := p.advance()
		var kind string
		switch {
		case isKeyword(p.peek(), "MAX"):
			kind = "MAX"
			p.advance()
		case isKeyword(p.peek(), "MIN"):
			kind = "MIN"
			p.advance()
		default:
			return nil, p.errorf(p.peek().Pos,
				"Syntax error: Expected keyword MAX or keyword MIN but got %s",
				describeToken(p.peek()))
		}
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		call.Having = &ast.HavingModifier{
			Span: span(havingTok.Pos, p.extEnd(expr)),
			Kind: kind,
			Expr: expr,
		}
	}
	// (group_by_clause_prefix having_clause?)? in function_call_expression: a
	// multi-level aggregation GROUP BY modifier, optionally followed by a full
	// HAVING clause; see googlesql.tm.
	if isKeyword(p.peek(), "GROUP") {
		groupBy, err := p.parseGroupBy(groupByFunc)
		if err != nil {
			return nil, err
		}
		call.GroupBy = groupBy
		if isKeyword(p.peek(), "HAVING") {
			havingTok := p.advance()
			expr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			call.HavingClause = &ast.Having{Span: span(havingTok.Pos, p.extEnd(expr)), Expr: expr}
		}
	}
	// clamped_between_modifier in googlesql.tm. It requires at least one
	// argument; with no arguments, CLAMPED parses as an identifier inside
	// the first argument expression. Once CLAMPED appears directly after an
	// argument (not after a comma) it can only be this modifier, so "BETWEEN"
	// is then required.
	if len(call.Args) > 0 && isKeyword(p.peek(), "CLAMPED") {
		clampedTok := p.advance()
		if _, err := p.expectKeyword("BETWEEN"); err != nil {
			return nil, err
		}
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
		call.ClampedBetween = &ast.ClampedBetweenModifier{
			Span: span(clampedTok.Pos, p.extEnd(high)),
			Low:  low,
			High: high,
		}
	}
	// with_report_modifier in googlesql.tm: "WITH" "REPORT" options_list?.
	if isKeyword(p.peek(), "WITH") && isKeyword(p.peekAt(1), "REPORT") {
		withTok := p.advance()   // WITH
		reportTok := p.advance() // REPORT
		wr := &ast.WithReportModifier{Span: span(withTok.Pos, reportTok.End)}
		if p.peek().Kind == token.LPAREN {
			opts, err := p.parseOptionsList()
			if err != nil {
				return nil, err
			}
			wr.Options = opts
			wr.Stop = opts.End()
		}
		call.WithReport = wr
	}
	// order_by_clause? opt_limit_offset_clause in function_call_expression.
	if isKeyword(p.peek(), "ORDER") {
		orderBy, err := p.parseOrderBy(false)
		if err != nil {
			return nil, err
		}
		call.OrderBy = orderBy
	}
	if isKeyword(p.peek(), "LIMIT") {
		limitOffset, err := p.parseLimitOffset()
		if err != nil {
			return nil, err
		}
		call.LimitOffset = limitOffset
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	call.Stop = rparen.End
	// "function_call_expression braced_constructor" is a proto UPDATE
	// constructor; see function_call_expression_with_clauses in googlesql.tm.
	if p.peek().Kind == token.LBRACE && p.features.Enabled(FeatureBracedProtoConstructors) {
		braced, err := p.parseBracedConstructor()
		if err != nil {
			return nil, err
		}
		return &ast.UpdateConstructor{
			Span:        span(call.Pos(), braced.End()),
			Function:    call,
			Constructor: braced,
		}, nil
	}
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
		if p.isReserved(tok) {
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
	if (tok.Kind == token.IDENT || tok.Kind == token.QUOTED_IDENT) && !p.isReserved(tok) &&
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
	if tok := p.peek(); tok.Kind != token.RPAREN && !isKeyword(tok, "ORDER") &&
		!isKeyword(tok, "ROWS") && !isKeyword(tok, "RANGE") {
		return nil, p.errorf(tok.Pos,
			`Syntax error: Expected ")" or keyword ORDER or keyword RANGE or keyword ROWS but got %s`,
			describeToken(tok))
	}
	if isKeyword(p.peek(), "ORDER") {
		orderBy, err := p.parseOrderBy(false)
		if err != nil {
			return nil, err
		}
		windowSpec.OrderBy = orderBy
	}
	if tok := p.peek(); tok.Kind != token.RPAREN &&
		!isKeyword(tok, "ROWS") && !isKeyword(tok, "RANGE") {
		return nil, p.errorf(tok.Pos,
			`Syntax error: Expected ")" or keyword RANGE or keyword ROWS but got %s`,
			describeToken(tok))
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

// parseWindowClause parses "WINDOW identifier AS window_specification
// [, ...]"; see window_clause in googlesql.tm. If allowTrailingComma is
// true (pipe SELECT/EXTEND), a trailing comma is permitted and extends the
// clause's end location; see window_clause_with_trailing_comma.
func (p *parser) parseWindowClause(allowTrailingComma bool) (*ast.WindowClause, error) {
	windowTok := p.advance() // WINDOW
	clause := &ast.WindowClause{Span: span(windowTok.Pos, 0)}
	for {
		def, err := p.parseWindowDefinition()
		if err != nil {
			return nil, err
		}
		clause.Windows = append(clause.Windows, def)
		clause.Stop = def.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		comma := p.advance()
		next := p.peek()
		if next.Kind != token.IDENT && next.Kind != token.QUOTED_IDENT || p.isReserved(next) {
			// Trailing comma; it is included in the clause's location.
			if !allowTrailingComma {
				return nil, p.errorf(next.Pos, "Syntax error: Unexpected %s", describeToken(next))
			}
			clause.Stop = comma.End
			break
		}
	}
	return clause, nil
}

// parseWindowDefinition parses "identifier AS window_specification"; see
// window_definition in googlesql.tm.
func (p *parser) parseWindowDefinition() (*ast.WindowDefinition, error) {
	tok := p.peek()
	if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT || p.isReserved(tok) {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	name := p.parseIdentifierToken(p.advance())
	if _, err := p.expectKeyword("AS"); err != nil {
		return nil, err
	}
	spec, err := p.parseWindowSpecification()
	if err != nil {
		return nil, err
	}
	return &ast.WindowDefinition{
		Span:       span(name.Pos(), spec.End()),
		Name:       name,
		WindowSpec: spec,
	}, nil
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

// parsePartitionByNoHint parses "PARTITION BY expression, ..." without an
// optional hint; see partition_by_clause_prefix_no_hint in googlesql.tm. A
// hint (e.g. "@{...}") is disallowed here, so "PARTITION" must be followed
// immediately by "BY". PARTITION is the next token.
func (p *parser) parsePartitionByNoHint() (*ast.PartitionBy, error) {
	partitionTok := p.advance() // PARTITION
	if _, err := p.expectKeyword("BY"); err != nil {
		return nil, err
	}
	partitionBy := &ast.PartitionBy{Span: span(partitionTok.Pos, 0)}
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

// parseMaybeDashedPathExpression parses a table-name path expression whose
// first component may be a dashed identifier (e.g. my-project.dataset.table);
// see maybe_dashed_path_expression in googlesql.tm.
func (p *parser) parseMaybeDashedPathExpression() (*ast.PathExpression, error) {
	prev := p.allowDashes
	p.allowDashes = true
	path, err := p.parsePathExpression()
	p.allowDashes = prev
	return path, err
}

// parseMaybeSlashedOrDashedPathExpression parses a table-name path that may be
// a dashed path (my-project.dataset.table) or a slashed path starting with "/"
// (/span/db/my-grp:db.Table); see maybe_slashed_or_dashed_path_expression in
// googlesql.tm.
func (p *parser) parseMaybeSlashedOrDashedPathExpression() (*ast.PathExpression, error) {
	if p.peek().Kind == token.SLASH {
		return p.parseSlashedPathExpression()
	}
	return p.parseMaybeDashedPathExpression()
}

// parseSlashedPathExpression parses a slashed_path_expression: a
// slashed_identifier (a "/"-prefixed run of identifiers/integers joined by
// adjacent "/", "-", and ":" separators) followed by optional ".identifier"
// path components; see slashed_identifier and slashed_path_expression in
// googlesql.tm. When FEATURE_ALLOW_SLASH_PATHS is off, the whole construct is a
// syntax error naming the slashed identifier as the part that must be quoted.
func (p *parser) parseSlashedPathExpression() (*ast.PathExpression, error) {
	slash := p.peek() // "/"
	start, end, err := p.parseSlashedIdentifier()
	if err != nil {
		return nil, err
	}
	// The joined identifier text equals the source slice: every part is
	// adjacent (embedded whitespace is rejected while scanning), so there are
	// no gaps to elide. This matches SeparatedIdentifierTmpNode::BuildPathParts,
	// which concatenates the parts of the first path element.
	joined := p.sql[start:end]
	first := &ast.Identifier{Span: span(start, end), Name: joined}
	names := []*ast.Identifier{first}
	pathEnd := end
	// slashed_path_expression "." identifier: trailing regular components.
	for p.peek().Kind == token.DOT {
		p.advance() // .
		tok := p.peek()
		if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
		}
		ident := p.parseIdentifierToken(p.advance())
		names = append(names, ident)
		pathEnd = ident.End()
	}
	if !p.features.Enabled(FeatureAllowSlashPaths) {
		// See maybe_slashed_or_dashed_path_expression in googlesql.tm: without
		// the feature, a slashed table name must be quoted. The error points at
		// the "/" and names the slashed identifier as the part to quote; the
		// wording differs when the path has trailing components.
		target := "It "
		if len(names) > 1 {
			target = "The slashed identifier part of the table name "
		}
		return nil, p.errorf(slash.Pos, "Syntax error: Table name contains '/' character. %sneeds to be quoted: %s", target, toIdentifierLiteral(joined))
	}
	return &ast.PathExpression{Span: span(start, pathEnd), Names: names}, nil
}

// parseSlashedIdentifier consumes a slashed_identifier ("/" identifier_or_integer
// followed by adjacent "sep identifier_or_integer" runs, with separators "/",
// "-", ":") and returns its source span; see slashed_identifier in googlesql.tm.
// Every part must be adjacent to its separator: embedded whitespace or a quoted
// part is a syntax error. Only unquoted identifiers and integers are accepted as
// parts (the floating-point-in-path forms are not exercised by table names).
func (p *parser) parseSlashedIdentifier() (start, end int, err error) {
	slash := p.advance() // "/"
	start = slash.Pos
	idTok := p.peek()
	if idTok.Pos != slash.End || !isSlashPathPart(idTok) {
		return 0, 0, p.errorf(slash.Pos, "Syntax error: Unexpected \"/\"")
	}
	p.advance()
	end = idTok.End
	for {
		sep := p.peek()
		var img string
		switch sep.Kind {
		case token.SLASH:
			img = "/"
		case token.MINUS:
			img = "-"
		case token.COLON:
			img = ":"
		default:
			return start, end, nil
		}
		if sep.Pos != end {
			// Non-adjacent separator: not part of this identifier.
			return start, end, nil
		}
		next := p.peekAt(1)
		if next.Pos != sep.End || !isSlashPathPart(next) {
			return 0, 0, p.errorf(sep.Pos, "Syntax error: Unexpected %q", img)
		}
		p.advance() // separator
		p.advance() // part
		end = next.End
	}
}

// isSlashPathPart reports whether tok is a valid identifier_or_integer part of a
// slashed identifier: an unquoted identifier or an integer literal. Quoted
// identifiers are rejected (they trigger an "Unexpected \"/\"" error upstream).
func isSlashPathPart(tok token.Token) bool {
	return tok.Kind == token.IDENT || tok.Kind == token.INT
}

// parseMaybeDashedGeneralizedPathExpression parses the target of a DELETE (or
// UPDATE) statement: either a generalized path expression (which may contain
// ".ident", ".(field)", and "[expr]" accesses) or a dashed path expression;
// see maybe_dashed_generalized_path_expression in googlesql.tm. A dashed table
// name never contains generalized accesses, so when the first identifier is
// immediately followed by a dash it is parsed as a plain dashed path.
func (p *parser) parseMaybeDashedGeneralizedPathExpression() (ast.Node, error) {
	tok := p.peek()
	if tok.Kind == token.IDENT && !p.isReserved(tok) {
		if dash := p.peekAt(1); dash.Kind == token.MINUS && dash.Pos == tok.End {
			return p.parseMaybeDashedPathExpression()
		}
	}
	return p.parseGeneralizedPathExpression()
}

func (p *parser) parsePathExpression() (*ast.PathExpression, error) {
	tok := p.peek()
	if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	if p.isReserved(tok) {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected keyword %s", strings.ToUpper(tok.Image))
	}
	first := p.parseIdentifierToken(p.advance())
	names := []*ast.Identifier{first}
	dashed := false
	if p.allowDashes && tok.Kind == token.IDENT {
		ids, err := p.extendDashedIdentifier(first)
		if err != nil {
			return nil, err
		}
		if ids != nil {
			names = ids
			dashed = true
		}
	}
	path := &ast.PathExpression{Span: span(names[0].Pos(), names[len(names)-1].End()), Names: names}
	for p.peek().Kind == token.DOT {
		// In a select column a path may stop before a ".*"; parsePostfix
		// records it as the dot-star target. A path that is not the whole
		// column expression fails the target check there instead.
		if p.allowDotStar && p.peekAt(1).Kind == token.STAR {
			return path, nil
		}
		if p.peekAt(1).Kind == token.LPAREN {
			if p.inTablePath {
				p.advance() // .
				return nil, p.errorf(p.peek().Pos, "Syntax error: Generalized field access is not allowed in the FROM clause without UNNEST; Use UNNEST(<expression>)")
			}
			if p.allowGeneralizedField {
				// Leave ".(" for the caller to parse as a generalized field
				// access; see the "." "(" path_expression ")" rule in
				// googlesql.tm.
				return path, nil
			}
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
	if dashed && !p.features.Enabled(FeatureAllowDashesInTableName) {
		// See maybe_dashed_path_expression in googlesql.tm: without the
		// feature, a dashed table name must be quoted. The error points at the
		// first (dashed) component and names it as the part to quote.
		target := "It "
		if len(path.Names) > 1 {
			target = "The dashed identifier part of the table name "
		}
		return nil, p.errorf(path.Names[0].Pos(), "Syntax error: Table name contains '-' character. %sneeds to be quoted: %s", target, toIdentifierLiteral(path.Names[0].Name))
	}
	return path, nil
}

// extendDashedIdentifier consumes a "dashed identifier" continuation following
// first, per the dashed_identifier rules in googlesql.tm: a chain of adjacent
// "- identifier" or "- integer". A floating-point literal arises when the
// lexer folds a path "." into the number (e.g. "123." in "foo-123.bar"); its
// trailing "." is really a path separator, so the digits complete the dashed
// run and the adjacent following identifier becomes a fresh path component.
//
// It returns nil (having consumed nothing) when first is not followed by an
// adjacent dash. Otherwise it returns the leading path components: one
// dash-joined identifier, plus a second component when a float folded in a
// separator. first must be an unquoted identifier.
func (p *parser) extendDashedIdentifier(first *ast.Identifier) ([]*ast.Identifier, error) {
	dash := p.peek()
	if dash.Kind != token.MINUS || dash.Pos != first.End() {
		return nil, nil
	}
	name := first.Name
	start := first.Pos()
	end := first.End()
	for {
		dash := p.peek()
		if dash.Kind != token.MINUS || dash.Pos != end {
			break
		}
		next := p.peekAt(1)
		if next.Pos != dash.End {
			// Non-adjacent: the "-" is not part of a dashed identifier.
			return nil, p.errorf(dash.Pos, "Syntax error: Unexpected \"-\"")
		}
		switch next.Kind {
		case token.IDENT, token.INT:
			p.advance() // -
			p.advance() // ident or int
			name += "-" + next.Image
			end = next.End
		case token.FLOAT:
			img := next.Image
			ident := p.peekAt(2)
			if !strings.HasSuffix(img, ".") || ident.Kind != token.IDENT || ident.Pos != next.End {
				return nil, p.errorf(dash.Pos, "Syntax error: Unexpected \"-\"")
			}
			p.advance() // -
			p.advance() // float ("N.")
			p.advance() // trailing identifier
			name += "-" + strings.TrimSuffix(img, ".")
			// The float's "." was the path separator; the dashed run ends at
			// the last digit and the trailing identifier is a new component.
			// In googlesql.tm this is one dashed_identifier reduction
			// ("identifier - FLOAT identifier") whose two path parts are both
			// built by SeparatedIdentifierTmpNode::BuildPathParts with the same
			// bison location (@1) spanning the whole run, so both identifiers
			// share that location.
			whole := span(start, ident.End)
			dashedID := &ast.Identifier{Span: whole, Name: name}
			tailID := &ast.Identifier{Span: whole, Name: ident.Image}
			return []*ast.Identifier{dashedID, tailID}, nil
		default:
			return nil, p.errorf(dash.Pos, "Syntax error: Unexpected \"-\"")
		}
	}
	return []*ast.Identifier{{Span: span(start, end), Name: name}}, nil
}

// toIdentifierLiteral renders name the way ZetaSQL's ToIdentifierLiteral does
// for diagnostics: valid unquoted identifiers print as-is, everything else is
// backquoted. Ported minimally for the dashed-table-name error.
func toIdentifierLiteral(name string) string {
	valid := name != ""
	if valid {
		for i := 0; i < len(name); i++ {
			c := name[i]
			isStart := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
			if i == 0 {
				if !isStart {
					valid = false
					break
				}
			} else if !isStart && !(c >= '0' && c <= '9') {
				valid = false
				break
			}
		}
	}
	if valid && !token.IsReservableKeyword(name) && !token.NonReservedIdentifierMustBeBackquoted(name) {
		return name
	}
	var b strings.Builder
	b.WriteByte('`')
	for i := 0; i < len(name); i++ {
		if name[i] == '`' || name[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(name[i])
	}
	b.WriteByte('`')
	return b.String()
}

// parseSystemVariableExpr parses a system variable reference "@@path"; see
// system_variable_expression in googlesql.tm. When "@@" is immediately
// followed by identifier characters the lexer emits "@@name" as one token and
// subsequent ".name" segments extend the path ("@@a.b" is a single system
// variable named "a.b"). When "@@" stands alone (e.g. "@@ name"), the path
// follows as ordinary tokens.
func (p *parser) parseSystemVariableExpr() (ast.Node, error) {
	tok := p.advance() // @@ or @@name
	var path *ast.PathExpression
	if len(tok.Image) > 2 {
		first := &ast.Identifier{Span: span(tok.Pos+2, tok.End), Name: tok.Image[2:]}
		path = &ast.PathExpression{Span: span(first.Pos(), first.End()), Names: []*ast.Identifier{first}}
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
	} else {
		// A bare "@@" token: the path expression follows as separate tokens.
		// A token that cannot start a path is reported as unexpected. Any
		// keyword immediately after "@@" (or after a "." in the path) is
		// treated as an identifier; see the KW_DOUBLE_AT and
		// LB_DOT_IN_PATH_EXPRESSION lookback cases in lookahead_transformer.cc.
		if next := p.peek(); next.Kind != token.IDENT && next.Kind != token.QUOTED_IDENT {
			return nil, p.errorf(next.Pos, "Syntax error: Unexpected %s", describeToken(next))
		}
		first := p.parseIdentifierToken(p.advance())
		path = &ast.PathExpression{Span: span(first.Pos(), first.End()), Names: []*ast.Identifier{first}}
		for p.peek().Kind == token.DOT {
			p.advance()
			nextTok := p.peek()
			if nextTok.Kind != token.IDENT && nextTok.Kind != token.QUOTED_IDENT {
				return nil, p.errorf(nextTok.Pos, "Syntax error: Unexpected %s", describeToken(nextTok))
			}
			ident := p.parseIdentifierToken(p.advance())
			path.Names = append(path.Names, ident)
			path.Stop = ident.End()
		}
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
		if tok.Kind == token.QUESTION && !p.quantifierQuestions[tok.Pos] {
			n++
		}
	}
	return n
}

func (p *parser) parseStringLiteral() (ast.Node, error) {
	return p.parseStringLiteralValue()
}

// parseStringLiteralValue parses one or more adjacent string literals that
// concatenate into a single StringLiteral with multiple components; see
// string_literal in googlesql.tm.
func (p *parser) parseStringLiteralValue() (*ast.StringLiteral, error) {
	tok := p.advance()
	component := &ast.StringLiteralComponent{Span: span(tok.Pos, tok.End), Image: tok.Image}
	lit := &ast.StringLiteral{
		Span:       span(tok.Pos, tok.End),
		Components: []*ast.StringLiteralComponent{component},
	}
	// Adjacent string literals concatenate into one literal with multiple
	// components, but must be separated by whitespace or comments; see
	// string_literal in googlesql.tm.
	prevEnd := tok.End
	for p.peek().Kind == token.STRING {
		next := p.peek()
		if next.Pos == prevEnd {
			return nil, p.errorf(next.Pos, "Syntax error: concatenated string literals must be separated by whitespace or comments")
		}
		tok := p.advance()
		component := &ast.StringLiteralComponent{Span: span(tok.Pos, tok.End), Image: tok.Image}
		lit.Components = append(lit.Components, component)
		lit.Stop = tok.End
		prevEnd = tok.End
	}
	// String and bytes literals cannot be concatenated together.
	if p.peek().Kind == token.BYTES {
		return nil, p.errorf(p.peek().Pos, "Syntax error: string and bytes literals cannot be concatenated.")
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
	prevEnd := tok.End
	for p.peek().Kind == token.BYTES {
		next := p.peek()
		if next.Pos == prevEnd {
			return nil, p.errorf(next.Pos, "Syntax error: concatenated bytes literals must be separated by whitespace or comments")
		}
		tok := p.advance()
		component := &ast.BytesLiteralComponent{Span: span(tok.Pos, tok.End), Image: tok.Image}
		lit.Components = append(lit.Components, component)
		lit.Stop = tok.End
		prevEnd = tok.End
	}
	// String and bytes literals cannot be concatenated together.
	if p.peek().Kind == token.STRING {
		return nil, p.errorf(p.peek().Pos, "Syntax error: string and bytes literals cannot be concatenated.")
	}
	return lit, nil
}

// dateOrTimeLiteralKind maps a typed-literal keyword to its ZetaSQL type kind
// name, or "" if the keyword does not introduce a date/time literal.
func dateOrTimeLiteralKind(image string) string {
	switch strings.ToUpper(image) {
	case "DATE":
		return "TYPE_DATE"
	case "DATETIME":
		return "TYPE_DATETIME"
	case "TIME":
		return "TYPE_TIME"
	case "TIMESTAMP":
		return "TYPE_TIMESTAMP"
	}
	return ""
}

// isTypedLiteralPrefix reports whether image is a keyword that introduces a
// typed literal whose payload is a string literal (NUMERIC/DECIMAL,
// BIGNUMERIC/BIGDECIMAL, JSON, DATE/DATETIME/TIME/TIMESTAMP).
func isTypedLiteralPrefix(image string) bool {
	switch strings.ToUpper(image) {
	case "NUMERIC", "DECIMAL", "BIGNUMERIC", "BIGDECIMAL", "JSON",
		"DATE", "DATETIME", "TIME", "TIMESTAMP":
		return true
	}
	return false
}

// parseTypedLiteral parses a keyword-prefixed typed literal whose payload is a
// string literal: NUMERIC/DECIMAL, BIGNUMERIC/BIGDECIMAL, JSON, and
// DATE/DATETIME/TIME/TIMESTAMP. The caller has verified that the keyword is
// immediately followed by a string literal. See numeric_literal,
// bignumeric_literal, json_literal and date_or_time_literal in googlesql.tm.
func (p *parser) parseTypedLiteral() (ast.Node, error) {
	kw := p.advance()
	value, err := p.parseStringLiteralValue()
	if err != nil {
		return nil, err
	}
	sp := span(kw.Pos, value.End())
	switch strings.ToUpper(kw.Image) {
	case "NUMERIC", "DECIMAL":
		return &ast.NumericLiteral{Span: sp, Value: value}, nil
	case "BIGNUMERIC", "BIGDECIMAL":
		return &ast.BigNumericLiteral{Span: sp, Value: value}, nil
	case "JSON":
		return &ast.JSONLiteral{Span: sp, Value: value}, nil
	}
	return &ast.DateOrTimeLiteral{Span: sp, TypeKind: dateOrTimeLiteralKind(kw.Image), Value: value}, nil
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
	} else if p.peek().Kind != token.RPAREN {
		// After the type (and before any FORMAT clause) the grammar state
		// accepts the optional FORMAT keyword or the closing ")"; see
		// cast_expression and opt_format in googlesql.tm.
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected ")" or keyword FORMAT but got %s`, describeToken(p.peek()))
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

// parseExtractExpression parses "EXTRACT(part FROM expr [AT TIME ZONE tz])";
// see extract_expression in googlesql.tm.
func (p *parser) parseExtractExpression() (ast.Node, error) {
	kw := p.advance() // EXTRACT
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	lhs, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	rhs, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	ext := &ast.ExtractExpression{LhsExpr: lhs, RhsExpr: rhs}
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
		ext.TimeZone = tz
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	ext.Span = span(kw.Pos, rparen.End)
	return ext, nil
}

// parseReplaceFieldsExpression parses "REPLACE_FIELDS(expression,
// replace_fields_arg [, ...])"; see the replace_fields_expression,
// replace_fields_prefix, and replace_fields_arg rules in googlesql.tm.
func (p *parser) parseReplaceFieldsExpression() (ast.Node, error) {
	kw := p.advance() // REPLACE_FIELDS
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	node := &ast.ReplaceFieldsExpression{Expr: expr}
	// At least one "," replace_fields_arg is required.
	if _, err := p.expect(token.COMMA, `","`); err != nil {
		return nil, err
	}
	for {
		arg, err := p.parseReplaceFieldsArg()
		if err != nil {
			return nil, err
		}
		node.Args = append(node.Args, arg)
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance() // ,
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	node.Span = span(kw.Pos, rparen.End)
	return node, nil
}

// parseReplaceFieldsArg parses "expression AS generalized_path_expression" (or
// "expression AS generalized_extension_path"); see replace_fields_arg in
// googlesql.tm.
func (p *parser) parseReplaceFieldsArg() (*ast.ReplaceFieldsArg, error) {
	value, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("AS"); err != nil {
		return nil, err
	}
	path, err := p.parseGeneralizedPathOrExtension()
	if err != nil {
		return nil, err
	}
	return &ast.ReplaceFieldsArg{Span: span(p.extStart(value), p.prevEnd()), Value: value, Path: path}, nil
}

// parseGeneralizedPathOrExtension parses either a generalized_path_expression
// (which starts with an identifier) or a generalized_extension_path (which
// starts with a parenthesized path); see the replace_fields_arg alternatives
// in googlesql.tm.
func (p *parser) parseGeneralizedPathOrExtension() (ast.Node, error) {
	tok := p.peek()
	if tok.Kind == token.LPAREN {
		return p.parseGeneralizedExtensionPath()
	}
	if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	if p.isReserved(tok) {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected keyword %s", strings.ToUpper(tok.Image))
	}
	return p.parseGeneralizedPathExpression()
}

// parseGeneralizedExtensionPath parses "( path ) [. ident | . ( path )]...";
// see generalized_extension_path in googlesql.tm. The parenthesized base path
// keeps its own (inner) location, but the enclosing DotIdentifier and
// DotGeneralizedField nodes' locations begin at the opening parenthesis.
func (p *parser) parseGeneralizedExtensionPath() (ast.Node, error) {
	lparen := p.advance() // (
	inner, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(token.RPAREN, `")"`); err != nil {
		return nil, err
	}
	var expr ast.Node = inner
	start := lparen.Pos
	for p.peek().Kind == token.DOT {
		if p.peekAt(1).Kind == token.LPAREN {
			p.advance() // .
			p.advance() // (
			p2, err := p.parsePathExpression()
			if err != nil {
				return nil, err
			}
			rp, err := p.expect(token.RPAREN, `")"`)
			if err != nil {
				return nil, err
			}
			expr = &ast.DotGeneralizedField{Span: span(start, rp.End), Expr: expr, Path: p2}
			continue
		}
		if p.peekAt(1).Kind != token.IDENT && p.peekAt(1).Kind != token.QUOTED_IDENT {
			break
		}
		p.advance() // .
		ident := p.parseIdentifierToken(p.advance())
		expr = &ast.DotIdentifier{Span: span(start, ident.End()), Expr: expr, Name: ident}
	}
	return expr, nil
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
	if !isReservedStatic(tok) {
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
	case *ast.MapType:
		t.TypeParameters, t.Collate, t.Stop = params, collate, end
	case *ast.FunctionType:
		t.TypeParameters, t.Collate, t.Stop = params, collate, end
	}
	return raw, nil
}

// parseFunctionType parses "FUNCTION<arg_list -> return_type>"; see
// function_type in googlesql.tm. FUNCTION is the next token.
func (p *parser) parseFunctionType() (*ast.FunctionType, error) {
	funcTok := p.advance() // FUNCTION
	if _, err := p.expectTemplateOpen(); err != nil {
		return nil, err
	}
	argList, err := p.parseFunctionTypeArgList(funcTok)
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(token.ARROW, `"->"`); err != nil {
		return nil, err
	}
	ret, err := p.parseType()
	if err != nil {
		return nil, err
	}
	closeTok, err := p.expectTemplateClose()
	if err != nil {
		return nil, err
	}
	return &ast.FunctionType{Span: span(funcTok.Pos, closeTok.End), ArgList: argList, ReturnType: ret}, nil
}

// parseFunctionTypeArgList parses the argument-type list of a function type:
// an empty "()", a parenthesized comma-separated list, or a single unadorned
// type; see function_type_arg_list in googlesql.tm. The reference gives the
// parenthesized non-empty form a location starting at the FUNCTION keyword,
// while the empty "()" form spans just the parentheses and the bare-type form
// spans just the type.
func (p *parser) parseFunctionTypeArgList(funcTok token.Token) (*ast.FunctionTypeArgList, error) {
	if p.peek().Kind == token.LPAREN {
		lparen := p.advance() // (
		if p.peek().Kind == token.RPAREN {
			rparen := p.advance() // )
			return &ast.FunctionTypeArgList{Span: span(lparen.Pos, rparen.End)}, nil
		}
		var args []ast.Node
		for {
			t, err := p.parseType()
			if err != nil {
				return nil, err
			}
			args = append(args, t)
			if p.peek().Kind != token.COMMA {
				break
			}
			p.advance()
		}
		if _, err := p.expect(token.RPAREN, `")"`); err != nil {
			return nil, err
		}
		return &ast.FunctionTypeArgList{Span: span(funcTok.Pos, args[len(args)-1].End()), Args: args}, nil
	}
	t, err := p.parseType()
	if err != nil {
		return nil, err
	}
	return &ast.FunctionTypeArgList{Span: span(t.Pos(), t.End()), Args: []ast.Node{t}}, nil
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
	case isKeyword(tok, "MAP"):
		return p.parseMapType()
	case isKeyword(tok, "FUNCTION"):
		return p.parseFunctionType()
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
	// A reserved keyword such as PROTO or ENUM cannot name a type; the
	// non-type-introducing reserved keywords were already handled above.
	if tok.Kind == token.IDENT && p.isReserved(tok) {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected keyword %s", strings.ToUpper(tok.Image))
	}
	path, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	return &ast.SimpleType{Span: span(path.Pos(), path.End()), Name: path}, nil
}

// expectTemplateOpen consumes the "<" opening a template type. A "<>" token
// (as in the empty "STRUCT<>") is split so its ">" can close the template
// type; the reference lexer emits separate tokens in template contexts.
func (p *parser) expectTemplateOpen() (token.Token, error) {
	tok := p.peek()
	if tok.Kind == token.NEQ && tok.Image == "<>" {
		p.toks[p.pos] = token.Token{Kind: token.GT, Image: ">", Pos: tok.Pos + 1, End: tok.End}
		return token.Token{Kind: token.LT, Image: "<", Pos: tok.Pos, End: tok.Pos + 1}, nil
	}
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
	// Unlike STRUCT, ARRAY has no empty form, so a "<>" token is not split
	// into "<" ">"; the reference reports it as an unexpected token where
	// only "<" is allowed.
	if _, err := p.expect(token.LT, `"<"`); err != nil {
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
	// RANGE has no empty form; see parseArrayType for the "<>" handling.
	if _, err := p.expect(token.LT, `"<"`); err != nil {
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

// parseMapType parses "MAP<key_type, value_type>"; see map_type in
// googlesql.tm.
func (p *parser) parseMapType() (*ast.MapType, error) {
	mapTok := p.advance() // MAP
	// MAP has no empty form; see parseArrayType for the "<>" handling.
	if _, err := p.expect(token.LT, `"<"`); err != nil {
		return nil, err
	}
	key, err := p.parseType()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(token.COMMA, `","`); err != nil {
		return nil, err
	}
	value, err := p.parseType()
	if err != nil {
		return nil, err
	}
	closeTok, err := p.expectTemplateClose()
	if err != nil {
		return nil, err
	}
	return &ast.MapType{Span: span(mapTok.Pos, closeTok.End), KeyType: key, ValueType: value}, nil
}

// parseRangeLiteral parses "RANGE<type> '...'"; see range_literal in
// googlesql.tm.
func (p *parser) parseRangeLiteral() (ast.Node, error) {
	typ, err := p.parseRangeType()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != token.STRING {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected string literal but got %s", describeToken(p.peek()))
	}
	value, err := p.parseStringLiteralValue()
	if err != nil {
		return nil, err
	}
	return &ast.RangeLiteral{Span: span(typ.Pos(), value.End()), Type: typ, Value: value}, nil
}

// parseIntervalExpr parses "INTERVAL <expr> <datepart> [TO <datepart>]"; see
// interval_expression in googlesql.tm.
func (p *parser) parseIntervalExpr() (ast.Node, error) {
	kw := p.advance() // INTERVAL
	value, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	datePart, err := p.parseIntervalDatePart()
	if err != nil {
		return nil, err
	}
	node := &ast.IntervalExpr{Span: span(kw.Pos, datePart.End()), Value: value, DatePart: datePart}
	if isKeyword(p.peek(), "TO") {
		p.advance()
		to, err := p.parseIntervalDatePart()
		if err != nil {
			return nil, err
		}
		node.DatePartTo = to
		node.Stop = to.End()
	}
	return node, nil
}

// parseIntervalDatePart parses the date-part identifier of an INTERVAL
// expression. The grammar rule is a plain identifier, so any non-reserved
// identifier (e.g. HOUR, MINUTE) is accepted.
func (p *parser) parseIntervalDatePart() (*ast.Identifier, error) {
	tok := p.peek()
	if tok.Kind == token.QUOTED_IDENT || (tok.Kind == token.IDENT && !p.isReserved(tok)) {
		return p.parseIdentifierToken(p.advance()), nil
	}
	return nil, p.errorf(tok.Pos, "Syntax error: Expected identifier but got %s", describeToken(tok))
}

// parseStructType parses "STRUCT<field, ...>" (possibly empty); see
// struct_type in googlesql.tm.
func (p *parser) parseStructType() (*ast.StructType, error) {
	structTok := p.advance() // STRUCT
	if _, err := p.expectTemplateOpen(); err != nil {
		return nil, err
	}
	st := &ast.StructType{Span: span(structTok.Pos, 0)}
	// At EOF there is no field to parse; fall through to expectTemplateClose
	// so the error is the reference's `Expected ">" but got end of type`
	// rather than an "unexpected end of type" from the field parser.
	if p.peek().Kind != token.GT && p.peek().Kind != token.RSHIFT && p.peek().Kind != token.EOF {
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
	named := (tok.Kind == token.QUOTED_IDENT || (tok.Kind == token.IDENT && !p.isReserved(tok))) &&
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
	// A parameter must be followed by "," or ")"; the reference lists both
	// in the error since either could continue the list.
	if p.peek().Kind != token.RPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected ")" or "," but got %s`, describeToken(p.peek()))
	}
	p.advance()
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
