// Package dump renders an AST in the same format as ZetaSQL's
// ASTNode::DebugString (github.com/google/googlesql,
// googlesql/parser/parse_tree.cc), so output can be compared byte-for-byte
// against the upstream parser test suite.
package dump

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/sqlc-dev/zetajones/ast"
)

// Options controls the debug output format.
type Options struct {
	// SQL is the original query text, used to render the source snippet for
	// each node. If ShowLocationText is false it is unused.
	SQL string
	// ShowLocationText appends the summarized source text of each node, e.g.
	// "Identifier(col) [7-10] [col]". This matches ZetaSQL's default in the
	// parser test suite; tests with no_show_parse_location_text disable it.
	ShowLocationText bool
}

// Tree renders the debug string for the AST rooted at n. The output has no
// trailing newline.
func Tree(n ast.Node, opts Options) string {
	var b strings.Builder
	walk(&b, n, 0, opts)
	return strings.TrimSuffix(b.String(), "\n")
}

func walk(b *strings.Builder, n ast.Node, depth int, opts Options) {
	for i := 0; i < depth; i++ {
		b.WriteString("  ")
	}
	b.WriteString(nodeString(n))
	fmt.Fprintf(b, " [%d-%d]", n.Pos(), n.End())
	if opts.ShowLocationText && n.Pos() >= 0 && n.End() >= n.Pos() && n.End() <= len(opts.SQL) {
		summary, ok := summaryString(opts.SQL[n.Pos():n.End()], 30)
		if ok {
			fmt.Fprintf(b, " [%s]", summary)
		}
	}
	b.WriteString("\n")
	for _, child := range n.Children() {
		walk(b, child, depth+1, opts)
	}
}

// nodeString returns the single-node debug string: the node kind name plus
// node-specific details, matching each node's SingleNodeDebugString in
// ZetaSQL's parse_tree.cc.
func nodeString(n ast.Node) string {
	switch t := n.(type) {
	case *ast.QueryStatement:
		return "QueryStatement"
	case *ast.Query:
		return "Query"
	case *ast.Select:
		if t.Distinct {
			return "Select(distinct=true)"
		}
		return "Select"
	case *ast.SelectList:
		return "SelectList"
	case *ast.SelectColumn:
		return "SelectColumn"
	case *ast.Alias:
		return "Alias"
	case *ast.Star:
		return fmt.Sprintf("Star(%s)", t.Image)
	case *ast.FromClause:
		return "FromClause"
	case *ast.TablePathExpression:
		return "TablePathExpression"
	case *ast.UnnestExpression:
		return "UnnestExpression"
	case *ast.ExpressionWithOptAlias:
		return "ExpressionWithOptAlias"
	case *ast.WithOffset:
		return "WithOffset"
	case *ast.ArrayConstructor:
		return "ArrayConstructor"
	case *ast.WhereClause:
		return "WhereClause"
	case *ast.GroupBy:
		return "GroupBy"
	case *ast.GroupingItem:
		return "GroupingItem"
	case *ast.Having:
		return "Having"
	case *ast.OrderBy:
		return "OrderBy"
	case *ast.OrderingExpression:
		if t.Descending {
			return "OrderingExpression(DESC)"
		}
		if t.HasAsc {
			return "OrderingExpression(ASC EXPLICITLY)"
		}
		return "OrderingExpression(ASC)"
	case *ast.LimitOffset:
		return "LimitOffset"
	case *ast.Limit:
		return "Limit"
	case *ast.LimitAll:
		return "LimitAll"
	case *ast.LockMode:
		return "LockMode"
	case *ast.AlterStatement:
		if t.IsIfExists {
			return t.NodeName + "(is_if_exists)"
		}
		return t.NodeName
	case *ast.AlterActionList:
		return "AlterActionList"
	case *ast.RenameToClause:
		return "RenameToClause"
	case *ast.SetOptionsAction:
		return "SetOptionsAction"
	case *ast.OptionsList:
		return "OptionsList"
	case *ast.OptionsEntry:
		return "OptionsEntry"
	case *ast.Identifier:
		return fmt.Sprintf("Identifier(%s)", t.Name)
	case *ast.PathExpression:
		return "PathExpression"
	case *ast.NullLiteral:
		return fmt.Sprintf("NullLiteral(%s)", t.Image)
	case *ast.BooleanLiteral:
		return fmt.Sprintf("BooleanLiteral(%s)", t.Image)
	case *ast.IntLiteral:
		return fmt.Sprintf("IntLiteral(%s)", t.Image)
	case *ast.FloatLiteral:
		return fmt.Sprintf("FloatLiteral(%s)", t.Image)
	case *ast.StringLiteral:
		return "StringLiteral"
	case *ast.StringLiteralComponent:
		return fmt.Sprintf("StringLiteralComponent(%s)", t.Image)
	case *ast.BytesLiteral:
		return "BytesLiteral"
	case *ast.BytesLiteralComponent:
		return fmt.Sprintf("BytesLiteralComponent(%s)", t.Image)
	case *ast.UnaryExpression:
		return fmt.Sprintf("UnaryExpression(%s)", t.Op)
	case *ast.BinaryExpression:
		if t.IsNot {
			return fmt.Sprintf("BinaryExpression(%s (with IS NOT))", t.Op)
		}
		return fmt.Sprintf("BinaryExpression(%s)", t.Op)
	case *ast.AndExpr:
		return "AndExpr"
	case *ast.OrExpr:
		return "OrExpr"
	case *ast.Location:
		return "Location"
	case *ast.BetweenExpression:
		if t.IsNot {
			return "BetweenExpression(NOT BETWEEN)"
		}
		return "BetweenExpression(BETWEEN)"
	case *ast.StructConstructorWithParens:
		return "StructConstructorWithParens"
	case *ast.FromQuery:
		return "FromQuery"
	case *ast.Subpipeline:
		return "Subpipeline"
	case *ast.PipeLog:
		return "PipeLog"
	case *ast.PipeAggregate:
		return "PipeAggregate"
	case *ast.PipeWhere:
		return "PipeWhere"
	case *ast.PipeOrderBy:
		return "PipeOrderBy"
	case *ast.PipeSet:
		return "PipeSet"
	case *ast.PipeSetItem:
		return "PipeSetItem"
	case *ast.FunctionCall:
		if t.Distinct {
			return "FunctionCall(distinct=true)"
		}
		return "FunctionCall"
	default:
		return fmt.Sprintf("UNKNOWN_NODE(%T)", n)
	}
}

// summaryString is a port of GetSummaryString from ZetaSQL
// (googlesql/common/utf_util.cc). It normalizes whitespace and, when the text
// exceeds maxCodePoints, elides the middle with "...", preferring to break at
// word boundaries. Returns ok=false for invalid UTF-8 (ZetaSQL omits the
// snippet in that case).
func summaryString(s string, maxCodePoints int) (string, bool) {
	// Normalize: strip leading/trailing whitespace, replace \r\n with a
	// single space, then replace every whitespace character with ' '.
	s = strings.TrimFunc(s, isASCIISpace)
	s = strings.ReplaceAll(s, "\r\n", " ")
	norm := []byte(s)
	for i := range norm {
		if isASCIISpaceByte(norm[i]) {
			norm[i] = ' '
		}
	}
	if !utf8.Valid(norm) {
		return "", false
	}
	runes := []rune(string(norm))
	if len(runes) <= maxCodePoints {
		return string(runes), true
	}

	minPrefixSuffix := (maxCodePoints - 3) / 2
	if maxCodePoints/3 < minPrefixSuffix {
		minPrefixSuffix = maxCodePoints / 3
	}

	prefix, prefixLen := summaryPrefix(runes, minPrefixSuffix, maxCodePoints)
	suffix := summarySuffix(runes, minPrefixSuffix, maxCodePoints-prefixLen)
	return prefix + "..." + suffix, true
}

// summaryPrefix computes the text before the "...", extending past
// minChars to avoid breaking in the middle of a word, up to
// maxTotal-3-minChars characters.
func summaryPrefix(runes []rune, minChars, maxTotal int) (string, int) {
	maxPrefixChars := maxTotal - 3 - minChars
	var prefix []rune
	inWord := false
	trailingSpaces := 0
	for _, r := range runes {
		prevInWord := inWord
		inWord = isWordChar(r)
		if len(prefix) >= minChars && (!prevInWord || !inWord) {
			break
		}
		prefix = append(prefix, r)
		if r == ' ' {
			trailingSpaces++
		} else {
			trailingSpaces = 0
		}
		if len(prefix) >= maxPrefixChars {
			break
		}
	}
	charLen := len(prefix) - trailingSpaces
	out := strings.TrimRight(string(prefix), " ")
	return out, charLen
}

// summarySuffix computes the text after the "...", walking backwards.
func summarySuffix(runes []rune, minChars, maxTotal int) string {
	maxSuffixChars := maxTotal - 3
	var suffix []rune
	inWord := false
	for i := len(runes) - 1; i >= 0; i-- {
		r := runes[i]
		prevInWord := inWord
		inWord = isWordChar(r)
		if len(suffix) >= minChars && (!prevInWord || !inWord) {
			break
		}
		suffix = append([]rune{r}, suffix...)
		if len(suffix) >= maxSuffixChars {
			break
		}
	}
	return strings.TrimLeft(string(suffix), " ")
}

func isWordChar(r rune) bool {
	return r == '_' ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9')
}

func isASCIISpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f'
}

func isASCIISpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}
