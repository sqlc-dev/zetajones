// Package lexer converts GoogleSQL source text into a stream of tokens.
//
// Error messages are ported from github.com/google/googlesql
// (googlesql/parser/googlesql.tm lexer rules). GoogleSQL is Apache 2.0.
package lexer

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/sqlc-dev/zetajones/token"
)

// Error is a lexical error with a byte offset into the input. Message is a
// full ZetaSQL-style message such as `Syntax error: Illegal input character
// "\357"`.
type Error struct {
	Message string
	Offset  int
}

func (e *Error) Error() string { return e.Message }

// Lex scans the input SQL string and returns all tokens, ending with an EOF token.
func Lex(sql string) ([]token.Token, error) {
	l := &lexer{sql: sql}
	var toks []token.Token
	for {
		tok, err := l.next()
		if err != nil {
			return nil, err
		}
		toks = append(toks, tok)
		l.updateLookback(tok)
		if tok.Kind == token.EOF {
			return toks, nil
		}
	}
}

type lexer struct {
	sql string
	pos int
	// prev is the last token emitted, used as the one-token lookback for
	// path-dot decisions; see LookbackTokenCanBeBeforeDotInPathExpression
	// in googlesql/parser/lookahead_transformer.cc.
	prev token.Token
	// afterPathDot is true when the last token was a "." that continues a
	// path expression; digits after such a dot lex as identifiers ("t.1",
	// "t.1b") rather than as numbers.
	afterPathDot bool
	// lastCommentEnd is the end offset of the last comment consumed by the
	// current skipSpaceAndComments pass, or -1 if none. Dash/pound (line)
	// comments include their terminating line break, so their end lands on the
	// next line; block comments end at "*/". This is used to place the
	// end-of-input position after a trailing comment (see next), matching the
	// EOI rule in googlesql/parser/googlesql.tm: whitespace before EOI is
	// skipped but trailing comments are not.
	lastCommentEnd int
}

// updateLookback records tok as the lookback token and tracks whether the
// lexer is positioned right after a path-continuation dot.
func (l *lexer) updateLookback(tok token.Token) {
	l.afterPathDot = tok.Kind == token.DOT && lookbackAllowsPathDot(l.prev)
	l.prev = tok
}

// lookbackAllowsPathDot reports whether a "." following tok continues a path
// expression rather than starting a floating point literal; ported from
// LookbackTokenCanBeBeforeDotInPathExpression in
// googlesql/parser/lookahead_transformer.cc (Apache 2.0). Identifiers,
// non-reserved keywords, ")", "]", "?", named parameters, and named system
// variables can all precede a path dot.
func lookbackAllowsPathDot(tok token.Token) bool {
	switch tok.Kind {
	case token.IDENT:
		return !token.IsReservedKeyword(tok.Image)
	case token.QUOTED_IDENT, token.RPAREN, token.RBRACKET, token.QUESTION, token.PARAM:
		return true
	case token.SYSTEM_VARIABLE:
		// A bare "@@" with no name does not end in an identifier.
		return len(tok.Image) > len("@@")
	}
	return false
}

func (l *lexer) errorf(pos int, format string, args ...any) error {
	return &Error{Message: fmt.Sprintf(format, args...), Offset: pos}
}

// cEscapeByte escapes a single byte the way absl::CEscape does: standard
// C escapes for the common control characters, quote, and backslash, and
// three-digit octal escapes for other non-printable bytes.
func cEscapeByte(c byte) string {
	switch c {
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	case '\t':
		return `\t`
	case '"':
		return `\"`
	case '\'':
		return `\'`
	case '\\':
		return `\\`
	}
	if c < 0x20 || c > 0x7e {
		return fmt.Sprintf(`\%03o`, c)
	}
	return string(c)
}

func (l *lexer) peekAt(off int) byte {
	if l.pos+off < len(l.sql) {
		return l.sql[l.pos+off]
	}
	return 0
}

func (l *lexer) peek() byte { return l.peekAt(0) }

// skipSpaceAndComments advances past whitespace and comments. It records the
// end offset of the last comment consumed in l.lastCommentEnd (or -1 if the
// pass consumes no comment) so that next can place the end-of-input position
// after a trailing comment rather than past trailing whitespace.
func (l *lexer) skipSpaceAndComments() error {
	l.lastCommentEnd = -1
	for l.pos < len(l.sql) {
		c := l.sql[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' || c == '\b':
			l.pos++
		case c >= 0x80:
			r, size := utf8.DecodeRuneInString(l.sql[l.pos:])
			if !isUnicodeSpace(r) {
				return nil
			}
			l.pos += size
		case c == '#':
			l.skipLineComment()
		case c == '-' && l.peekAt(1) == '-':
			l.skipLineComment()
		case c == '/' && l.peekAt(1) == '*':
			end := strings.Index(l.sql[l.pos+2:], "*/")
			if end < 0 {
				return l.errorf(l.pos, "Syntax error: Unclosed comment")
			}
			l.pos += 2 + end + 2
			l.lastCommentEnd = l.pos
		default:
			return nil
		}
	}
	return nil
}

// isUnicodeSpace reports whether r is one of the Unicode space characters
// treated as whitespace by the GoogleSQL lexer; see whitespace_character in
// googlesql/parser/googlesql.tm. Zero-width spaces (U+200B, U+FEFF), the
// Ogham space mark (U+1680) and the Mongolian vowel separator (U+180E) are
// deliberately excluded, matching the reference implementation.
func isUnicodeSpace(r rune) bool {
	switch r {
	case 0x00A0, // NO-BREAK SPACE
		0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, // EN QUAD..SIX-PER-EM
		0x2006, 0x2007, 0x2008, 0x2009, 0x200A, // ..HAIR SPACE
		0x202F, // NARROW NO-BREAK SPACE
		0x205F, // MEDIUM MATHEMATICAL SPACE
		0x3000: // IDEOGRAPHIC SPACE
		return true
	}
	return false
}

// skipLineComment consumes a dash ("--") or pound ("#") comment, including its
// terminating line break, matching dash_comment/pound_comment in
// googlesql/parser/googlesql.tm (/...[^\r\n]*(\r|\n|\r\n)?/). Because the line
// break is part of the comment, l.lastCommentEnd (set here) lands at the start
// of the next line, which is where ZetaSQL reports end-of-input after a
// trailing line comment.
func (l *lexer) skipLineComment() {
	for l.pos < len(l.sql) && l.sql[l.pos] != '\n' && l.sql[l.pos] != '\r' {
		l.pos++
	}
	// Consume one optional line terminator: "\r\n", "\r", or "\n".
	if l.pos < len(l.sql) {
		if l.sql[l.pos] == '\r' {
			l.pos++
			if l.pos < len(l.sql) && l.sql[l.pos] == '\n' {
				l.pos++
			}
		} else if l.sql[l.pos] == '\n' {
			l.pos++
		}
	}
	l.lastCommentEnd = l.pos
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHexDigit(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// hexIntegerLen returns the length of a hex integer literal "0x[0-9a-f]+"
// starting at position p, or 0 if none is present. Matches the hex_integer
// lexer rule in googlesql/parser/googlesql.tm.
func (l *lexer) hexIntegerLen(p int) int {
	if p+2 < len(l.sql) && l.sql[p] == '0' && (l.sql[p+1] == 'x' || l.sql[p+1] == 'X') && isHexDigit(l.sql[p+2]) {
		end := p + 2
		for end < len(l.sql) && isHexDigit(l.sql[end]) {
			end++
		}
		return end - p
	}
	return 0
}

// startsDecimalInteger reports whether a DECIMAL_INTEGER token ("[0-9]+", and
// not the start of a hex integer) begins at position p. Under maximal munch a
// leading "0x..." is a HEX_INTEGER, not a decimal one.
func (l *lexer) startsDecimalInteger(p int) bool {
	return p < len(l.sql) && isDigit(l.sql[p]) && l.hexIntegerLen(p) == 0
}

func (l *lexer) next() (token.Token, error) {
	if err := l.skipSpaceAndComments(); err != nil {
		return token.Token{}, err
	}
	start := l.pos
	if l.pos >= len(l.sql) {
		// End of input. ZetaSQL reports the end-of-input position after the last
		// real token or trailing comment, but not past trailing whitespace (see
		// the EOI rule in googlesql/parser/googlesql.tm). A trailing line comment
		// includes its terminating line break, so its end is on the next line.
		pos := max(l.lastCommentEnd, l.prev.End)
		return token.Token{Kind: token.EOF, Pos: pos, End: pos}, nil
	}
	c := l.sql[l.pos]

	switch {
	case isIdentStart(c):
		return l.identOrPrefixedString()
	case isDigit(c) && l.afterPathDot:
		// Digits after a path-continuation dot lex as an identifier ("t.1",
		// "t.1b", "t.0x1"); see TransformIntegerLiteral in
		// googlesql/parser/lookahead_transformer.cc.
		for l.pos < len(l.sql) && isIdentPart(l.sql[l.pos]) {
			l.pos++
		}
		return l.emit(token.IDENT, start), nil
	case isDigit(c) || (c == '.' && l.startsDecimalInteger(l.pos+1) && !lookbackAllowsPathDot(l.prev)):
		// A "." after an expression-ending token continues a path expression
		// rather than starting a float; see TransformDotSymbol in
		// googlesql/parser/lookahead_transformer.cc. A "." is only the start of
		// a float when a DECIMAL_INTEGER (not a hex integer) follows it, so
		// ".0x1" lexes as a "." token followed by the hex integer "0x1".
		return l.number()
	case c == '\'' || c == '"':
		return l.str(start, false, false)
	case c == '`':
		return l.quotedIdent()
	case c == '@':
		if l.peekAt(1) == '@' {
			l.pos += 2
			// A system-variable name is an identifier, which cannot start with
			// a digit; "@@1" is a bare "@@" followed by the number, matching
			// the reference lexer (system_variable_expression takes a
			// path_expression, so "@@1" is a syntax error at the "1").
			if l.pos < len(l.sql) && isIdentStart(l.sql[l.pos]) {
				for l.pos < len(l.sql) && isIdentPart(l.sql[l.pos]) {
					l.pos++
				}
			}
			return l.emit(token.SYSTEM_VARIABLE, start), nil
		}
		l.pos++
		// A parameter name is an identifier, which cannot start with a
		// digit; "@5" is a bare "@" (an integer hint) followed by a number.
		if l.pos < len(l.sql) && isIdentStart(l.sql[l.pos]) {
			for l.pos < len(l.sql) && isIdentPart(l.sql[l.pos]) {
				l.pos++
			}
			return l.emit(token.PARAM, start), nil
		}
		// A bare "@" with no name, e.g. the "@" opening a hint.
		return l.emit(token.ATSIGN, start), nil
	}

	if c == '$' {
		// Macro tokens recognized by maximal munch: "$$name" (builtin), "$name"
		// (invocation), and "$digits" (argument reference). A bare "$" (row
		// pattern anchor) is emitted otherwise. See the MACRO_* rules in
		// googlesql/parser/googlesql.tm.
		if l.peekAt(1) == '$' && isIdentStart(l.peekAt(2)) {
			l.pos += 2
			for l.pos < len(l.sql) && isIdentPart(l.sql[l.pos]) {
				l.pos++
			}
			return l.emit(token.MACRO_INVOCATION, start), nil
		}
		if isIdentStart(l.peekAt(1)) {
			l.pos++
			for l.pos < len(l.sql) && isIdentPart(l.sql[l.pos]) {
				l.pos++
			}
			return l.emit(token.MACRO_INVOCATION, start), nil
		}
		if isDigit(l.peekAt(1)) {
			l.pos++
			for l.pos < len(l.sql) && isDigit(l.sql[l.pos]) {
				l.pos++
			}
			return l.emit(token.MACRO_INVOCATION, start), nil
		}
		l.pos++
		return l.emit(token.DOLLAR, start), nil
	}

	// Operators and punctuation, longest match first.
	two := ""
	if l.pos+2 <= len(l.sql) {
		two = l.sql[l.pos : l.pos+2]
	}
	switch two {
	case "!=", "<>":
		l.pos += 2
		return l.emit(token.NEQ, start), nil
	case "<=":
		l.pos += 2
		return l.emit(token.LTE, start), nil
	case ">=":
		l.pos += 2
		return l.emit(token.GTE, start), nil
	case "<<":
		l.pos += 2
		return l.emit(token.LSHIFT, start), nil
	case ">>":
		l.pos += 2
		return l.emit(token.RSHIFT, start), nil
	case "||":
		l.pos += 2
		return l.emit(token.CONCAT, start), nil
	case "->":
		l.pos += 2
		return l.emit(token.ARROW, start), nil
	case "=>":
		l.pos += 2
		return l.emit(token.LAMBDA, start), nil
	case "|>":
		l.pos += 2
		return l.emit(token.PIPE_INPUT, start), nil
	}

	if kind := singleCharKind[c]; kind != token.ILLEGAL {
		l.pos++
		return l.emit(kind, start), nil
	}
	if c == '\\' {
		// A lone backslash lexes as a BACKSLASH token (used for lenient macro
		// expansion); see googlesql.tm. The parser then reports it as an
		// unexpected token rather than an illegal input character.
		l.pos++
		return l.emit(token.BACKSLASH, start), nil
	}
	if c == '!' {
		// A lone "!" (not part of "!=") lexes as the EXCL token; see the
		// EXCL rule (/!/) in googlesql/parser/googlesql.tm. The grammar has
		// no production for it, so the parser reports it as an unexpected
		// token rather than an illegal input character.
		l.pos++
		return l.emit(token.EXCL, start), nil
	}
	return token.Token{}, l.errorf(start, `Syntax error: Illegal input character "%s"`, cEscapeByte(c))
}

// singleCharKind maps a single-character token byte to its kind; ILLEGAL (the
// zero value) marks bytes that do not form a single-character token.
var singleCharKind = [256]token.Kind{
	'+': token.PLUS, '-': token.MINUS, '*': token.STAR, '/': token.SLASH,
	'%': token.PERCENT, '=': token.EQ, '<': token.LT, '>': token.GT,
	'&': token.AMP, '|': token.PIPE, '^': token.CARET, '~': token.TILDE,
	'(': token.LPAREN, ')': token.RPAREN, '[': token.LBRACKET, ']': token.RBRACKET,
	'{': token.LBRACE, '}': token.RBRACE, ',': token.COMMA, '.': token.DOT,
	';': token.SEMICOLON, ':': token.COLON, '?': token.QUESTION,
}

func (l *lexer) emit(kind token.Kind, start int) token.Token {
	return token.Token{Kind: kind, Image: l.sql[start:l.pos], Pos: start, End: l.pos}
}

// identOrPrefixedString scans an identifier, or a string/bytes literal with a
// raw/bytes prefix such as r'...', b"...", rb”'...”'.
func (l *lexer) identOrPrefixedString() (token.Token, error) {
	start := l.pos
	for l.pos < len(l.sql) && isIdentPart(l.sql[l.pos]) {
		l.pos++
	}
	ident := l.sql[start:l.pos]
	if l.pos < len(l.sql) && (l.sql[l.pos] == '\'' || l.sql[l.pos] == '"') {
		lower := strings.ToLower(ident)
		var raw, bytes bool
		switch lower {
		case "r":
			raw = true
		case "b":
			bytes = true
		case "rb", "br":
			raw, bytes = true, true
		default:
			return l.emit(token.IDENT, start), nil
		}
		return l.str(start, raw, bytes)
	}
	return l.emit(token.IDENT, start), nil
}

// str scans a string or bytes literal starting at the quote character at the
// current position. start is the beginning of the whole literal including any
// prefix. Triple-quoted forms are supported.
func (l *lexer) str(start int, raw, bytes bool) (token.Token, error) {
	quote := l.sql[l.pos]
	triple := false
	if l.pos+3 <= len(l.sql) && l.sql[l.pos+1] == quote && l.sql[l.pos+2] == quote {
		// A triple quote, unless it is an empty string followed by something
		// else, e.g. '' in isolation. ZetaSQL treats ''' as the start of a
		// triple-quoted string.
		triple = true
		l.pos += 3
	} else {
		l.pos++
	}
	contentStart := l.pos
	for l.pos < len(l.sql) {
		c := l.sql[l.pos]
		if c == '\\' && !raw {
			l.pos += 2
			continue
		}
		if c == '\\' && raw {
			// In raw strings a backslash still escapes the quote for lexing
			// purposes (r'\'' is the two characters \').
			l.pos += 2
			continue
		}
		if c == quote {
			if triple {
				if l.pos+3 <= len(l.sql) && l.sql[l.pos+1] == quote && l.sql[l.pos+2] == quote {
					contentEnd := l.pos
					if err := l.checkStringUTF8(contentStart, contentEnd, bytes); err != nil {
						return token.Token{}, err
					}
					if err := l.validateEscapes(contentStart, contentEnd, raw, bytes); err != nil {
						return token.Token{}, err
					}
					l.pos += 3
					return l.emitStr(start, bytes), nil
				}
				l.pos++
				continue
			}
			contentEnd := l.pos
			if err := l.checkStringUTF8(contentStart, contentEnd, bytes); err != nil {
				return token.Token{}, err
			}
			if err := l.validateEscapes(contentStart, contentEnd, raw, bytes); err != nil {
				return token.Token{}, err
			}
			l.pos++
			return l.emitStr(start, bytes), nil
		}
		if c == '\n' && !triple {
			return token.Token{}, l.unclosedError(start, raw, bytes, triple)
		}
		l.pos++
	}
	return token.Token{}, l.unclosedError(start, raw, bytes, triple)
}

// checkStringUTF8 validates that the content of a (non-bytes) string literal is
// structurally well-formed UTF-8. Bytes literals are exempt. Ported from the
// UTF8 check in CUnescapeInternal in googlesql/public/strings.cc.
func (l *lexer) checkStringUTF8(contentStart, contentEnd int, bytes bool) error {
	if bytes {
		return nil
	}
	content := l.sql[contentStart:contentEnd]
	span := spanWellFormedUTF8(content)
	if span >= len(content) {
		return nil
	}
	return l.errorf(contentStart+span,
		"Syntax error: Structurally invalid UTF8 string: %s", escapeBytes(content))
}

// validateEscapes validates the backslash escape sequences in the content of a
// string or bytes literal (the text between the quotes). Raw literals accept
// all escapes and are skipped. Ported from CUnescapeInternal in
// googlesql/public/strings.cc (Apache 2.0); errors are attributed to the start
// of the offending escape.
func (l *lexer) validateEscapes(contentStart, contentEnd int, raw, bytes bool) error {
	if raw {
		return nil
	}
	s := l.sql[contentStart:contentEnd]
	end := len(s)
	p := 0
	for p < end {
		if s[p] != '\\' {
			p++
			continue
		}
		escStart := p
		if p+1 >= end {
			// A backslash as the final character of the content.
			msg := "String literal cannot end with \\"
			if bytes {
				msg = "Bytes literal cannot end with \\"
			}
			return l.errorf(contentStart+end, "Syntax error: %s", msg)
		}
		errOff := contentStart + escStart
		p++ // read past the backslash
		c := s[p]
		switch {
		case c == 'a' || c == 'b' || c == 'f' || c == 'n' || c == 'r' ||
			c == 't' || c == 'v' || c == '\\' || c == '?' || c == '\'' ||
			c == '"' || c == '`':
			// Valid single-character escape.
		case c >= '0' && c <= '3':
			octalStart := p
			if p+2 >= end {
				return l.errorf(errOff, "Syntax error: Illegal escape sequence: Octal escape must be followed by 3 octal digits but saw: \\%s", s[octalStart:end])
			}
			octalEnd := p + 2
			for ; p <= octalEnd; p++ {
				if !isOctalDigit(s[p]) {
					return l.errorf(errOff, "Syntax error: Illegal escape sequence: Octal escape must be followed by 3 octal digits but saw: \\%s", s[octalStart:octalStart+3])
				}
			}
			p = octalEnd
		case c == 'x' || c == 'X':
			hexStart := p
			if p+2 >= end {
				return l.errorf(errOff, "Syntax error: Illegal escape sequence: Hex escape must be followed by 2 hex digits but saw: \\%s", s[hexStart:end])
			}
			hexEnd := p + 2
			for p++; p <= hexEnd; p++ {
				if !isHexDigit(s[p]) {
					return l.errorf(errOff, "Syntax error: Illegal escape sequence: Hex escape must be followed by 2 hex digits but saw: \\%s", s[hexStart:hexStart+3])
				}
			}
			p = hexEnd
		case c == 'u':
			if bytes {
				return l.errorf(errOff, "Syntax error: Illegal escape sequence: Unicode escape sequence \\u cannot be used in bytes literals")
			}
			hexStart := p
			if p+4 >= end {
				return l.errorf(errOff, "Syntax error: Illegal escape sequence: \\u must be followed by 4 hex digits but saw: \\%s", s[hexStart:end])
			}
			cp := 0
			for range 4 {
				if !isHexDigit(s[p+1]) {
					return l.errorf(errOff, "Syntax error: Illegal escape sequence: \\u must be followed by 4 hex digits but saw: \\%s", s[hexStart:hexStart+5])
				}
				p++
				cp = cp*16 + hexDigitValue(s[p])
			}
			if isSurrogate(cp) {
				return l.errorf(errOff, "Syntax error: Illegal escape sequence: Unicode value \\%s is invalid", s[hexStart:hexStart+5])
			}
		case c == 'U':
			if bytes {
				return l.errorf(errOff, "Syntax error: Illegal escape sequence: Unicode escape sequence \\U cannot be used in bytes literals")
			}
			hexStart := p
			if p+8 >= end {
				return l.errorf(errOff, "Syntax error: Illegal escape sequence: \\U must be followed by 8 hex digits but saw: \\%s", s[hexStart:end])
			}
			cp := 0
			for range 8 {
				if !isHexDigit(s[p+1]) {
					return l.errorf(errOff, "Syntax error: Illegal escape sequence: \\U must be followed by 8 hex digits but saw: \\%s", s[hexStart:hexStart+9])
				}
				p++
				cp = cp*16 + hexDigitValue(s[p])
				if cp > 0x10FFFF {
					return l.errorf(errOff, "Syntax error: Illegal escape sequence: Value of \\%s exceeds Unicode limit (0x0010FFFF)", s[hexStart:hexStart+9])
				}
			}
			if isSurrogate(cp) {
				return l.errorf(errOff, "Syntax error: Illegal escape sequence: Unicode value \\%s is invalid", s[hexStart:hexStart+9])
			}
		case c == '\r' || c == '\n':
			return l.errorf(errOff, "Syntax error: Illegal escaped newline")
		default:
			return l.errorf(errOff, "Syntax error: Illegal escape sequence: \\%c", c)
		}
		p++ // read past the escaped character
	}
	return nil
}

func isOctalDigit(c byte) bool { return c >= '0' && c <= '7' }

func isSurrogate(cp int) bool { return cp >= 0xD800 && cp <= 0xDFFF }

func hexDigitValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

// spanWellFormedUTF8 returns the length in bytes of the longest prefix of s that
// is structurally valid UTF-8.
func spanWellFormedUTF8(s string) int {
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return i
}

// escapeBytes renders bytes the way EscapeBytes does in
// googlesql/public/strings.cc with escape_all_bytes=false and no quote char:
// non-printable bytes become \xHH, backslash is doubled, other bytes pass
// through.
func escapeBytes(s string) string {
	var b strings.Builder
	const hexdigits = "0123456789abcdef"
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e {
			b.WriteString(`\x`)
			b.WriteByte(hexdigits[c>>4])
			b.WriteByte(hexdigits[c&0xf])
		} else if c == '\\' {
			b.WriteString(`\\`)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// unclosedError builds a "Syntax error: Unclosed ..." message matching
// SetUnclosedError/SetTripleUnclosedError in googlesql/parser/googlesql.tm.
func (l *lexer) unclosedError(start int, raw, bytes, triple bool) error {
	kind := "string literal"
	if bytes {
		kind = "bytes literal"
	}
	if raw {
		kind = "raw " + kind
	}
	if triple {
		kind = "triple-quoted " + kind
	}
	return l.errorf(start, "Syntax error: Unclosed %s", kind)
}

func (l *lexer) emitStr(start int, bytes bool) token.Token {
	kind := token.STRING
	if bytes {
		kind = token.BYTES
	}
	return l.emit(kind, start)
}

func (l *lexer) quotedIdent() (token.Token, error) {
	start := l.pos
	l.pos++ // consume opening backquote
	contentStart := l.pos
	for l.pos < len(l.sql) {
		c := l.sql[l.pos]
		if c == '\\' {
			l.pos += 2
			continue
		}
		if c == '\n' || c == '\r' {
			// A raw newline is excluded from the backquoted identifier content
			// (bqtext_0 = /`([^\\`\r\n]|escape)*/ in googlesql.tm); an escaped
			// newline is handled by the "\\" branch above. Reaching one here
			// means the identifier has no closing backquote on this line.
			break
		}
		if c == '`' {
			// Quoted identifiers are unescaped with the same rules as string
			// literals; see ParseIdentifier in googlesql/public/strings.cc.
			if err := l.validateEscapes(contentStart, l.pos, false, false); err != nil {
				return token.Token{}, err
			}
			// An identifier of just "``" (empty content) is invalid; see the
			// empty-identifier check in ParseIdentifier (strings.cc).
			if l.pos == contentStart {
				return token.Token{}, l.errorf(start, "Syntax error: Invalid empty identifier")
			}
			l.pos++
			return l.emit(token.QUOTED_IDENT, start), nil
		}
		l.pos++
	}
	return token.Token{}, l.errorf(start, "Syntax error: Unclosed identifier literal")
}

// number scans an integer or floating point literal. It reproduces the token
// fusing performed by the reference lookahead transformer
// (TransformIntegerLiteral, TransformDotSymbol and
// FuseExponentPartIntoFloatingPointLiteral in
// googlesql/parser/lookahead_transformer.cc): the base lexer emits separate
// DECIMAL_INTEGER, HEX_INTEGER, ".", "e"/"E[0-9]+" tokens and only adjacent
// runs are fused into one literal. In particular a hex integer never starts or
// extends a float, so inputs like "1.0x1" lex as the float "1." followed by the
// hex integer "0x1".
func (l *lexer) number() (token.Token, error) {
	start := l.pos
	// Hex integer. Hex integers are never part of a floating point literal.
	// "0x" with no hex digit lexes as the decimal integer "0" followed by the
	// identifier "x" (which the parser then reports as a
	// missing-whitespace-before-alias error).
	if hl := l.hexIntegerLen(l.pos); hl > 0 {
		l.pos += hl
		return l.emit(token.INT, start), nil
	}

	isFloat := false
	if l.peek() == '.' {
		// Dot-started float ".D..."; the dispatcher guarantees a decimal
		// integer (not a hex integer) follows the ".".
		isFloat = true
		l.pos++ // consume "."
		for l.pos < len(l.sql) && isDigit(l.sql[l.pos]) {
			l.pos++
		}
	} else {
		// Decimal integer mantissa.
		for l.pos < len(l.sql) && isDigit(l.sql[l.pos]) {
			l.pos++
		}
		// A "." immediately after the decimal integer begins the fractional
		// part, e.g. "1." or "1.5". A hex integer after the "." does not fuse,
		// so "1.0x1" stops the float at "1.".
		if l.peek() == '.' {
			isFloat = true
			l.pos++ // consume "."
			if l.hexIntegerLen(l.pos) == 0 {
				for l.pos < len(l.sql) && isDigit(l.sql[l.pos]) {
					l.pos++
				}
			}
		}
	}

	if l.fuseExponent() {
		isFloat = true
	}
	if isFloat {
		return l.emit(token.FLOAT, start), nil
	}
	return l.emit(token.INT, start), nil
}

// fuseExponent consumes an exponent part immediately following the mantissa at
// l.pos, matching FuseExponentPartIntoFloatingPointLiteral. It handles the two
// base-token forms: EXP_IN_FLOAT_NO_SIGN ("E[0-9]+") and STANDALONE_EXPONENT_SIGN
// ("E") followed by a sign and a decimal integer ("E+10"). It returns true and
// advances l.pos when an exponent is consumed.
func (l *lexer) fuseExponent() bool {
	if c := l.peek(); c != 'e' && c != 'E' {
		return false
	}
	// EXP_IN_FLOAT_NO_SIGN: "E[0-9]+". Under maximal munch this only wins over
	// an identifier when the digits are not followed by an identifier letter,
	// so "E10" fuses but "E10x" and "E0x1" are identifiers.
	if isDigit(l.peekAt(1)) {
		end := l.pos + 1
		for end < len(l.sql) && isDigit(l.sql[end]) {
			end++
		}
		if end >= len(l.sql) || !isIdentStart(l.sql[end]) {
			l.pos = end
			return true
		}
		return false
	}
	// STANDALONE_EXPONENT_SIGN: "E" followed by a sign and a decimal integer,
	// e.g. "E+10". A hex integer after the sign does not fuse.
	if s := l.peekAt(1); s == '+' || s == '-' {
		if l.startsDecimalInteger(l.pos + 2) {
			end := l.pos + 2
			for end < len(l.sql) && isDigit(l.sql[end]) {
				end++
			}
			l.pos = end
			return true
		}
	}
	return false
}
