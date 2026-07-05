// Package token defines the lexical tokens of GoogleSQL.
//
// The token kinds mirror the lexer rules of github.com/google/googlesql
// (googlesql/parser/googlesql.tm). GoogleSQL is Apache 2.0.
package token

// Kind identifies the type of a lexical token.
type Kind int

const (
	ILLEGAL Kind = iota
	EOF

	IDENT           // unquoted identifier or keyword
	QUOTED_IDENT    // `quoted identifier`
	INT             // integer literal, including hex
	FLOAT           // floating point literal
	STRING          // string literal (any quoting form, including raw)
	BYTES           // bytes literal (any quoting form, including raw)
	PARAM           // @named query parameter
	SYSTEM_VARIABLE // @@system_variable
	ATSIGN          // bare @ (hint prefix)
	// MACRO_INVOCATION covers the "$"-prefixed macro tokens recognized by the
	// base lexer: "$name" (invocation), "$$name" (builtin invocation), and
	// "$digits" (argument reference). See the MACRO_* rules in
	// googlesql/parser/googlesql.tm. The parser rejects a bare one as
	// unexpected (macros are expanded away before parsing).
	MACRO_INVOCATION

	// Operators and punctuation
	PLUS       // +
	MINUS      // -
	STAR       // *
	SLASH      // /
	PERCENT    // %
	EQ         // =
	NEQ        // != or <>
	LT         // <
	GT         // >
	LTE        // <=
	GTE        // >=
	LSHIFT     // <<
	RSHIFT     // >>
	AMP        // &
	PIPE       // |
	CARET      // ^
	TILDE      // ~
	CONCAT     // ||
	LPAREN     // (
	RPAREN     // )
	LBRACKET   // [
	RBRACKET   // ]
	LBRACE     // {
	RBRACE     // }
	COMMA      // ,
	DOT        // .
	SEMICOLON  // ;
	COLON      // :
	QUESTION   // ?
	DOLLAR     // $ (row pattern anchor)
	ARROW      // ->
	LAMBDA     // =>
	PIPE_INPUT // |>
	BACKSLASH  // \ (only for lenient macro expansion)
	EXCL       // ! (standalone; the parser rejects it as unexpected)

	// SCRIPT_LABEL is an identifier that opens a labeled script statement, e.g.
	// the "L1" in "L1: BEGIN ... END". The lexer force-emits this in place of
	// an identifier/keyword when the token is followed by ":" and a
	// block-opening keyword at a statement-start position; see
	// LookaheadTransformer::IsCurrentTokenScriptLabel in
	// googlesql/parser/lookahead_transformer.cc. Its error description omits
	// the "identifier" qualifier.
	SCRIPT_LABEL
)

// Token is a single lexical token with its position in the input.
type Token struct {
	Kind  Kind
	Image string // raw text of the token as written in the input
	Pos   int    // byte offset of the first byte of the token
	End   int    // byte offset one past the last byte of the token
}
