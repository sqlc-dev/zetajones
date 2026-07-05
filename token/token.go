// Package token defines the lexical tokens of GoogleSQL.
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
)

// Token is a single lexical token with its position in the input.
type Token struct {
	Kind  Kind
	Image string // raw text of the token as written in the input
	Pos   int    // byte offset of the first byte of the token
	End   int    // byte offset one past the last byte of the token
}
