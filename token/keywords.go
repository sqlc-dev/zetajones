package token

import "strings"

// reservedKeywords is the full set of GoogleSQL reserved keywords
// (lowercase). Ported from github.com/google/googlesql
// googlesql/parser/keywords.cc (Apache 2.0), entries marked kReserved.
var reservedKeywords = map[string]bool{
	"all": true, "and": true, "any": true, "array": true, "as": true,
	"asc": true, "assert_rows_modified": true, "at": true, "between": true,
	"by": true, "case": true, "cast": true, "collate": true,
	"contains": true, "create": true, "cross": true, "cube": true,
	"current": true, "default": true, "define": true, "desc": true,
	"distinct": true, "else": true, "end": true, "enum": true,
	"escape": true, "except": true, "exclude": true, "exists": true,
	"extract": true, "false": true, "fetch": true, "following": true,
	"for": true, "from": true, "full": true, "group": true,
	"grouping": true, "groups": true, "hash": true, "having": true,
	"if": true, "ignore": true, "in": true, "inner": true,
	"intersect": true, "interval": true, "into": true, "is": true,
	"join": true, "lateral": true, "left": true, "like": true,
	"limit": true, "lookup": true, "merge": true, "natural": true,
	"new": true, "no": true, "not": true, "null": true, "nulls": true,
	"of": true, "on": true, "or": true, "order": true, "outer": true,
	"over": true, "partition": true, "preceding": true, "proto": true,
	"range": true, "recursive": true, "respect": true, "right": true,
	"rollup": true, "rows": true, "select": true, "set": true,
	"some": true, "struct": true, "tablesample": true, "then": true,
	"to": true, "treat": true, "true": true, "unbounded": true,
	"union": true, "unnest": true, "using": true, "when": true,
	"where": true, "window": true, "with": true, "within": true,
}

// conditionallyReservedKeywords are keywords that are reserved only when the
// corresponding language feature is enabled; see kConditionallyReserved in
// googlesql/parser/keywords.cc.
var conditionallyReservedKeywords = map[string]bool{
	"graph_table":     true,
	"match_recognize": true,
	"qualify":         true,
}

// nonReservedMustBackquote are non-reserved keywords that must be rendered
// backquoted when used as identifiers because their meaning changes without
// quoting; see CreateNonReservedIdentifiersThatMustBeBackquotedTrie in
// googlesql/parser/keywords.cc.
var nonReservedMustBackquote = map[string]bool{
	"access":                    true,
	"current_date":              true,
	"current_datetime":          true,
	"current_time":              true,
	"current_timestamp":         true,
	"current_timestamp_micros":  true,
	"current_timestamp_millis":  true,
	"current_timestamp_seconds": true,
	"function":                  true,
	"inout":                     true,
	"out":                       true,
	"policy":                    true,
	"replace":                   true,
	"row":                       true,
	"safe_cast":                 true,
	"update":                    true,
	"clamped":                   true,
}

// IsReservedKeyword reports whether s is an (unconditionally) reserved
// GoogleSQL keyword, ASCII case-insensitively.
func IsReservedKeyword(s string) bool {
	return reservedKeywords[strings.ToLower(s)]
}

// IsReservableKeyword reports whether s is reserved or conditionally
// reserved; this matches LanguageOptions::EnableAllReservableKeywords in the
// reference implementation, which identifier rendering uses.
func IsReservableKeyword(s string) bool {
	lower := strings.ToLower(s)
	return reservedKeywords[lower] || conditionallyReservedKeywords[lower]
}

// NonReservedIdentifierMustBeBackquoted reports whether the non-reserved
// keyword s must be backquoted when rendered as an identifier; see
// NonReservedIdentifierMustBeBackquoted in googlesql/parser/keywords.cc.
func NonReservedIdentifierMustBeBackquoted(s string) bool {
	return nonReservedMustBackquote[strings.ToLower(s)]
}
