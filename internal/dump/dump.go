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
	"github.com/sqlc-dev/zetajones/token"
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
	case *ast.WithClause:
		if t.Recursive {
			return "WithClause (recursive)"
		}
		return "WithClause"
	case *ast.WithClauseEntry:
		return "WithClauseEntry"
	case *ast.AliasedQuery:
		return "AliasedQuery"
	case *ast.AliasedGroupRows:
		return "AliasedGroupRows"
	case *ast.TVF:
		return "TVF"
	case *ast.TVFArgument:
		return "TVFArgument"
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
	case *ast.StarWithModifiers:
		return "StarWithModifiers"
	case *ast.DotStar:
		return "DotStar"
	case *ast.DotStarWithModifiers:
		return "DotStarWithModifiers"
	case *ast.StarModifiers:
		return "StarModifiers"
	case *ast.StarExceptList:
		return "StarExceptList"
	case *ast.StarReplaceItem:
		return "StarReplaceItem"
	case *ast.FromClause:
		return "FromClause"
	case *ast.TablePathExpression:
		return "TablePathExpression"
	case *ast.TableSubquery:
		return "TableSubquery"
	case *ast.UnnestExpression:
		return "UnnestExpression"
	case *ast.ExpressionWithOptAlias:
		return "ExpressionWithOptAlias"
	case *ast.WithOffset:
		return "WithOffset"
	case *ast.ArrayConstructor:
		return "ArrayConstructor"
	case *ast.Join:
		// See ASTJoin::SingleNodeDebugString in parse_tree.cc: the details
		// are NATURAL, the join type (comma joins show as COMMA), and the
		// join hint, in that order.
		var attrs []string
		if t.Natural {
			attrs = append(attrs, "NATURAL")
		}
		if t.JoinType != "" {
			attrs = append(attrs, t.JoinType)
		}
		if t.JoinHint != "" {
			attrs = append(attrs, t.JoinHint)
		}
		if len(attrs) == 0 {
			return "Join"
		}
		return fmt.Sprintf("Join(%s)", strings.Join(attrs, ", "))
	case *ast.OnClause:
		return "OnClause"
	case *ast.UsingClause:
		return "UsingClause"
	case *ast.OnOrUsingClauseList:
		return "OnOrUsingClauseList"
	case *ast.ParenthesizedJoin:
		return "ParenthesizedJoin"
	case *ast.PipeJoin:
		return "PipeJoin"
	case *ast.PipeJoinLhsPlaceholder:
		return "PipeJoinLhsPlaceholder"
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
	case *ast.Collate:
		return "Collate"
	case *ast.NullOrder:
		if t.NullsFirst {
			return "NullOrder(NULLS FIRST)"
		}
		return "NullOrder(NULLS LAST)"
	case *ast.Hint:
		return "Hint"
	case *ast.HintEntry:
		return "HintEntry"
	case *ast.AnalyticFunctionCall:
		return "AnalyticFunctionCall"
	case *ast.WindowSpecification:
		return "WindowSpecification"
	case *ast.WindowFrame:
		return "WindowFrame(" + t.Unit + ")"
	case *ast.WindowFrameExpr:
		return "WindowFrameExpr(" + t.BoundaryType + ")"
	case *ast.PartitionBy:
		return "PartitionBy"
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
		return fmt.Sprintf("Identifier(%s)", identifierLiteral(t.Name))
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
		// See ASTBinaryExpression::GetSQLForOperator in parse_tree.cc for
		// how is_not combines with the operator name.
		op := t.Op
		if t.IsNot {
			switch t.Op {
			case "IS":
				op = "IS NOT"
			case "LIKE":
				op = "NOT LIKE"
			case "IS DISTINCT FROM":
				op = "IS NOT DISTINCT FROM"
			}
		}
		return fmt.Sprintf("BinaryExpression(%s)", op)
	case *ast.InExpression:
		if t.IsNot {
			return "InExpression(NOT IN)"
		}
		return "InExpression(IN)"
	case *ast.InList:
		return "InList"
	case *ast.LikeExpression:
		if t.IsNot {
			return "LikeExpression(NOT LIKE)"
		}
		return "LikeExpression(LIKE)"
	case *ast.AnySomeAllOp:
		return fmt.Sprintf("AnySomeAllOp(%s)", t.Op)
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
	case *ast.ClampedBetweenModifier:
		return "ClampedBetweenModifier"
	case *ast.HavingModifier:
		return "HavingModifier"
	case *ast.StructConstructorWithParens:
		return "StructConstructorWithParens"
	case *ast.StructConstructorWithKeyword:
		return "StructConstructorWithKeyword"
	case *ast.StructConstructorArg:
		return "StructConstructorArg"
	case *ast.NewConstructor:
		return "NewConstructor"
	case *ast.NewConstructorArg:
		return "NewConstructorArg"
	case *ast.BracedNewConstructor:
		return "BracedNewConstructor"
	case *ast.StructBracedConstructor:
		return "StructBracedConstructor"
	case *ast.BracedConstructor:
		return "BracedConstructor"
	case *ast.BracedConstructorField:
		return "BracedConstructorField"
	case *ast.BracedConstructorLhs:
		return "BracedConstructorLhs"
	case *ast.BracedConstructorFieldValue:
		return "BracedConstructorFieldValue"
	case *ast.FromQuery:
		return "FromQuery"
	case *ast.SetOperation:
		// The parenthesized detail is the first operator's SQL; see
		// ASTSetOperationMetadata::GetSQLForOperation in parse_tree.cc.
		md := t.Metadata.Entries[0]
		return fmt.Sprintf("SetOperation(%s %s)", md.OpType.Op, md.AllOrDistinct.Value)
	case *ast.SetOperationMetadataList:
		return "SetOperationMetadataList"
	case *ast.SetOperationMetadata:
		return "SetOperationMetadata"
	case *ast.SetOperationType:
		return "SetOperationType"
	case *ast.SetOperationAllOrDistinct:
		return "SetOperationAllOrDistinct"
	case *ast.SetOperationColumnMatchMode:
		return "SetOperationColumnMatchMode"
	case *ast.SetOperationColumnPropagationMode:
		return "SetOperationColumnPropagationMode"
	case *ast.ColumnList:
		return "ColumnList"
	case *ast.TableClause:
		return "TableClause"
	case *ast.ModelClause:
		return "ModelClause"
	case *ast.ConnectionClause:
		return "ConnectionClause"
	case *ast.DefaultLiteral:
		return "DefaultLiteral"
	case *ast.CallStatement:
		return "CallStatement"
	case *ast.CaseValueExpression:
		return "CaseValueExpression"
	case *ast.CaseNoValueExpression:
		return "CaseNoValueExpression"
	case *ast.DotIdentifier:
		return "DotIdentifier"
	case *ast.ArrayElement:
		return "ArrayElement"
	case *ast.ParameterExpr:
		// Positional parameters show their 1-based position; see
		// ASTParameterExpr::SingleNodeDebugString.
		if t.Name == nil {
			return fmt.Sprintf("ParameterExpr(%d)", t.Position)
		}
		return "ParameterExpr"
	case *ast.SystemVariableExpr:
		return "SystemVariableExpr"
	case *ast.PipeSetOperation:
		return "PipeSetOperation"
	case *ast.Subpipeline:
		return "Subpipeline"
	case *ast.PipeLog:
		return "PipeLog"
	case *ast.PipeSelect:
		return "PipeSelect"
	case *ast.PipeExtend:
		return "PipeExtend"
	case *ast.PipeWindow:
		return "PipeWindow"
	case *ast.PipeLimitOffset:
		return "PipeLimitOffset"
	case *ast.PipeDistinct:
		return "PipeDistinct"
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
	case *ast.CastExpression:
		// See ASTCastExpression::SingleNodeDebugString in parse_tree.cc.
		if t.IsSafeCast {
			return "CastExpression(return_null_on_error=true)"
		}
		return "CastExpression"
	case *ast.FormatClause:
		return "FormatClause"
	case *ast.SimpleType:
		return "SimpleType"
	case *ast.ArrayType:
		return "ArrayType"
	case *ast.StructType:
		return "StructType"
	case *ast.StructField:
		return "StructField"
	case *ast.RangeType:
		return "RangeType"
	case *ast.TypeParameterList:
		return "TypeParameterList"
	case *ast.MaxLiteral:
		// ASTMaxLiteral is a printable leaf with an empty image, so the
		// reference prints an empty parenthesized detail.
		return "MaxLiteral()"
	case *ast.ExpressionSubquery:
		if t.Modifier != "" {
			return fmt.Sprintf("ExpressionSubquery(modifier=%s)", t.Modifier)
		}
		return "ExpressionSubquery"
	case *ast.CreateTableStatement:
		var mods []string
		switch t.Scope {
		case "PRIVATE":
			mods = append(mods, "is_private")
		case "PUBLIC":
			mods = append(mods, "is_public")
		case "TEMP":
			mods = append(mods, "is_temp")
		}
		if t.IsOrReplace {
			mods = append(mods, "is_or_replace")
		}
		if t.IsIfNotExists {
			mods = append(mods, "is_if_not_exists")
		}
		if len(mods) == 0 {
			return "CreateTableStatement"
		}
		return fmt.Sprintf("CreateTableStatement(%s)", strings.Join(mods, ", "))
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

// identifierLiteral renders an identifier name the way ZetaSQL's
// ToIdentifierLiteral (googlesql/public/strings.cc, Apache 2.0) does: names
// that are valid unquoted identifiers — and not keywords whose meaning would
// change without quoting — print as-is; everything else is backquoted with
// C-style escaping.
func identifierLiteral(name string) string {
	if isValidUnquotedIdentifier(name) && !token.NonReservedIdentifierMustBeBackquoted(name) {
		return name
	}
	return "`" + escapeIdentifier(name) + "`"
}

// isValidUnquotedIdentifier ports IsValidUnquotedIdentifier from
// googlesql/public/strings.cc with all reservable keywords enabled and
// allow_reserved_keywords=false, which is how ToIdentifierLiteral calls it.
func isValidUnquotedIdentifier(s string) bool {
	if s == "" {
		return false
	}
	if !isIdentStartByte(s[0]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isIdentStartByte(c) && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return !token.IsReservableKeyword(s)
}

func isIdentStartByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// escapeIdentifier ports CEscapeInternal from googlesql/public/strings.cc
// (Apache 2.0) with utf8_safe=true and '`' as the quote char to escape.
func escapeIdentifier(s string) string {
	var b strings.Builder
	lastHexEscape := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		isHexEscape := false
		switch c {
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\\':
			b.WriteString(`\\`)
		case '`':
			b.WriteString("\\`")
		default:
			isPrint := c >= 0x20 && c < 0x7f
			isXDigit := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if c < 0x80 && (!isPrint || (lastHexEscape && isXDigit)) {
				fmt.Fprintf(&b, `\x%02X`, c)
				isHexEscape = true
			} else {
				b.WriteByte(c)
			}
		}
		lastHexEscape = isHexEscape
	}
	return b.String()
}
