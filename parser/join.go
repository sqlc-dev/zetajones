package parser

// Join parsing and the consecutive ON/USING clause transformation, ported
// from github.com/google/googlesql googlesql/parser/googlesql.tm
// (from_clause_contents, join, on_or_using_clause_lists, pipe_join) and
// googlesql/parser/join_processor.cc (Apache 2.0).

import (
	"fmt"
	"strings"

	"github.com/sqlc-dev/zetajones/ast"
	"github.com/sqlc-dev/zetajones/token"
)

// parseFromClauseContents parses "table_primary (comma or JOIN table_primary
// with optional ON/USING clauses)*"; see from_clause_contents in
// googlesql.tm. The result may still need transformJoinExpression applied.
func (p *parser) parseFromClauseContents() (ast.Node, error) {
	lhs, err := p.parseTablePrimary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.peek().Kind == token.COMMA:
			commaTok := p.advance()
			rhs, err := p.parseTablePrimary()
			if err != nil {
				return nil, err
			}
			// See CommaJoinRuleAction in join_processor.cc.
			if isTransformationNeeded(lhs) {
				return nil, p.errorf(lhs.Pos(), "Syntax error: Comma join is not allowed after consecutive ON/USING clauses")
			}
			lhs = &ast.Join{
				Span:              span(lhs.Pos(), p.prevEnd()),
				JoinType:          "COMMA",
				Lhs:               lhs,
				JoinLocation:      &ast.Location{Span: span(commaTok.Pos, commaTok.End)},
				Rhs:               rhs,
				ContainsCommaJoin: true,
			}
		case p.atJoinStart():
			join, err := p.parseJoinRest(lhs)
			if err != nil {
				return nil, err
			}
			lhs = join
		default:
			return lhs, nil
		}
	}
}

// atJoinStart reports whether the current token can begin a join clause
// after a table expression in a FROM clause.
func (p *parser) atJoinStart() bool {
	// FULL/LEFT/INNER directly before a set operation keyword lex as set
	// operation keywords, not join types; see the KW_FULL/KW_LEFT/KW_INNER
	// cases in googlesql/parser/lookahead_transformer.cc.
	if p.atSetOpMetadataStart() {
		return false
	}
	tok := p.peek()
	for _, kw := range [...]string{"JOIN", "NATURAL", "CROSS", "HASH", "LOOKUP", "FULL", "INNER", "LEFT", "RIGHT"} {
		if isKeyword(tok, kw) {
			return true
		}
	}
	return false
}

// parseJoinRest parses "[NATURAL] [join_type] [join_hint] JOIN [hint]
// table_primary on_or_using_clause*" onto an already-parsed left input; see
// the join and from_clause_contents rules in googlesql.tm.
func (p *parser) parseJoinRest(lhs ast.Node) (*ast.Join, error) {
	natural, joinType, joinHint, typeTok, joinLocation, err := p.parseJoinPrefix()
	if err != nil {
		return nil, err
	}
	hint, err := p.parseOptionalHint()
	if err != nil {
		return nil, err
	}
	rhs, err := p.parseTablePrimary()
	if err != nil {
		return nil, err
	}
	clauses, err := p.parseOnOrUsingClauses()
	if err != nil {
		return nil, err
	}
	// A RIGHT or FULL JOIN following a comma join would violate the
	// standard's binding order; see from_clause_contents in googlesql.tm.
	if joinType == "FULL" || joinType == "RIGHT" {
		for j, ok := lhs.(*ast.Join); ok; j, ok = j.Lhs.(*ast.Join) {
			if j.JoinType == "COMMA" {
				return nil, p.errorf(typeTok.Pos, "Syntax error: %s JOIN must be parenthesized when following a comma join.  Also, if the preceding comma join is a correlated CROSS JOIN that unnests an array, then CROSS JOIN syntax must be used in place of the comma join", joinType)
			}
		}
	}
	return p.joinRuleAction(lhs.Pos(), lhs, natural, joinType, joinHint, hint, rhs, clauses, joinLocation)
}

// parseJoinPrefix parses "[NATURAL] [join_type] [join_hint] JOIN" and builds
// the Location node covering those keywords; see NonEmptyRangeLocation in
// the join rules of googlesql.tm.
func (p *parser) parseJoinPrefix() (natural bool, joinType, joinHint string, typeTok token.Token, joinLocation *ast.Location, err error) {
	kwStart := p.peek().Pos
	if isKeyword(p.peek(), "NATURAL") {
		p.advance()
		natural = true
	}
	typeTok = token.Token{Pos: -1}
	optOuter := func() {
		if isKeyword(p.peek(), "OUTER") {
			p.advance()
		}
	}
	switch {
	case isKeyword(p.peek(), "CROSS"):
		typeTok = p.advance()
		joinType = "CROSS"
	case isKeyword(p.peek(), "FULL"):
		typeTok = p.advance()
		optOuter()
		joinType = "FULL"
	case isKeyword(p.peek(), "INNER"):
		typeTok = p.advance()
		joinType = "INNER"
	case isKeyword(p.peek(), "LEFT"):
		typeTok = p.advance()
		optOuter()
		joinType = "LEFT"
	case isKeyword(p.peek(), "RIGHT"):
		typeTok = p.advance()
		optOuter()
		joinType = "RIGHT"
	}
	switch {
	case isKeyword(p.peek(), "HASH"):
		p.advance()
		joinHint = "HASH"
	case isKeyword(p.peek(), "LOOKUP"):
		p.advance()
		joinHint = "LOOKUP"
	}
	joinTok, err := p.expectKeyword("JOIN")
	if err != nil {
		return false, "", "", typeTok, nil, err
	}
	return natural, joinType, joinHint, typeTok, &ast.Location{Span: span(kwStart, joinTok.End)}, nil
}

// parseOnOrUsingClauses parses zero or more consecutive ON/USING clauses;
// see on_or_using_clause_lists in googlesql.tm. More than one clause
// requires the ALLOW_CONSECUTIVE_ON language feature.
func (p *parser) parseOnOrUsingClauses() ([]ast.Node, error) {
	var clauses []ast.Node
	for {
		var clause ast.Node
		switch {
		case isKeyword(p.peek(), "ON"):
			onTok := p.advance()
			expr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			clause = &ast.OnClause{Span: span(onTok.Pos, p.extEnd(expr)), Expr: expr}
		case isKeyword(p.peek(), "USING"):
			uc, err := p.parseUsingClause()
			if err != nil {
				return nil, err
			}
			clause = uc
		default:
			return clauses, nil
		}
		if len(clauses) == 1 && !p.features.Enabled(FeatureAllowConsecutiveOn) {
			return nil, p.errorf(clause.Pos(), "Syntax error: Expected end of input but got keyword %s", onOrUsingKeyword(clause))
		}
		clauses = append(clauses, clause)
	}
}

// parseUsingClause parses "USING ( identifier, ... )" with the USING keyword
// as the next token; see using_clause in googlesql.tm.
func (p *parser) parseUsingClause() (*ast.UsingClause, error) {
	usingTok := p.advance() // USING
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	uc := &ast.UsingClause{Span: span(usingTok.Pos, 0)}
	for {
		tok := p.peek()
		if tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT {
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
		}
		if p.isReserved(tok) {
			return nil, p.errorf(tok.Pos, "Syntax error: Unexpected keyword %s", strings.ToUpper(tok.Image))
		}
		uc.Keys = append(uc.Keys, p.parseIdentifierToken(p.advance()))
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance()
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	uc.Stop = rparen.End
	return uc, nil
}

// onOrUsingKeyword returns the keyword naming the clause in error messages.
func onOrUsingKeyword(clause ast.Node) string {
	if _, ok := clause.(*ast.OnClause); ok {
		return "ON"
	}
	return "USING"
}

// joinRuleAction builds a Join node and applies the clause count checks; a
// port of JoinRuleAction in join_processor.cc. startPos is the join's start
// (the left input's first token, or the opt_natural location for pipe
// joins); the join ends at the last consumed token.
func (p *parser) joinRuleAction(startPos int, lhs ast.Node, natural bool, joinType, joinHint string, hint *ast.Hint, rhs ast.Node, clauses []ast.Node, joinLocation *ast.Location) (*ast.Join, error) {
	clauseCount := len(clauses)
	unmatched := unmatchedJoinCount(lhs)
	// The current join needs a condition unless it is a CROSS or NATURAL
	// join.
	if joinType != "CROSS" && !natural {
		unmatched++
	}
	// Consecutive ON/USING clauses cannot mix with comma joins.
	if clauseCount >= 2 && containsCommaJoin(lhs) {
		return nil, p.errorf(clauses[1].Pos(), "Syntax error: Unexpected keyword %s", onOrUsingKeyword(clauses[1]))
	}

	join := &ast.Join{
		Span:     span(startPos, p.prevEnd()),
		JoinType: joinType, JoinHint: joinHint, Natural: natural,
		Lhs: lhs, Hint: hint, JoinLocation: joinLocation, Rhs: rhs,
	}
	if clauseCount <= 1 {
		if clauseCount == 1 {
			join.OnOrUsingClause = clauses[0]
		}
		join.TransformationNeeded = isTransformationNeeded(lhs)
	} else {
		join.ClauseList = &ast.OnOrUsingClauseList{
			Span:    span(clauses[0].Pos(), clauses[clauseCount-1].End()),
			Clauses: clauses,
		}
		join.TransformationNeeded = true
	}
	join.UnmatchedJoinCount = unmatched - clauseCount
	join.ContainsCommaJoin = containsCommaJoin(lhs)

	// Detect more ON/USING clauses than joins. With a single clause the
	// error is only recorded, for backward compatibility; it surfaces if
	// consecutive clauses appear later in the join expression.
	parseError := getJoinParseError(lhs)
	if parseError != nil || clauseCount > unmatched {
		var errorNode ast.Node
		var message string
		if parseError != nil {
			errorNode, message = parseError.ErrorNode, parseError.Message
		} else {
			errorNode = clauses[unmatched]
			message = fmt.Sprintf("The number of join conditions is %d but the number of joins that require a join condition is only %d. Unexpected keyword %s",
				clauseCount, unmatched, onOrUsingKeyword(errorNode))
		}
		if clauseCount >= 2 {
			return nil, p.errorf(errorNode.Pos(), "Syntax error: %s", message)
		}
		join.ParseError = &ast.JoinParseError{ErrorNode: errorNode, Message: message}
	}
	return join, nil
}

// parseParenthesizedJoin parses "( join )" used as a table primary, with the
// opening parenthesis as the next token; see the `"(" join ")"` alternative
// of table_primary in googlesql.tm. The contents must include at least one
// JOIN; comma joins are not allowed inside the parentheses.
func (p *parser) parseParenthesizedJoin() (*ast.ParenthesizedJoin, error) {
	lparen := p.advance() // (
	table, err := p.parseTablePrimary()
	if err != nil {
		return nil, err
	}
	var joined ast.Node = table
	for first := true; first || p.atJoinStart(); first = false {
		join, err := p.parseJoinRest(joined)
		if err != nil {
			return nil, err
		}
		joined = join
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	node, err := p.transformJoinExpression(joined)
	if err != nil {
		return nil, err
	}
	return &ast.ParenthesizedJoin{Span: span(lparen.Pos, rparen.End), Join: node}, nil
}

// parsePipeJoin parses "[NATURAL] [join_type] [join_hint] JOIN [hint]
// table_primary [on_or_using_clause]" after a |> token; see pipe_join in
// googlesql.tm. The join's left input is a PipeJoinLhsPlaceholder.
func (p *parser) parsePipeJoin(pipeTok token.Token) (ast.Node, error) {
	// The join's span starts at the opt_natural location; when NATURAL is
	// absent that is the empty range right after the |> token.
	joinStart := pipeTok.End
	if isKeyword(p.peek(), "NATURAL") {
		joinStart = p.peek().Pos
	}
	placeholderStart := p.peek().Pos
	natural, joinType, joinHint, _, joinLocation, err := p.parseJoinPrefix()
	if err != nil {
		return nil, err
	}
	hint, err := p.parseOptionalHint()
	if err != nil {
		return nil, err
	}
	rhs, err := p.parseTablePrimary()
	if err != nil {
		return nil, err
	}
	// opt_on_or_using_clause: at most one clause.
	var clauses []ast.Node
	switch {
	case isKeyword(p.peek(), "ON"):
		onTok := p.advance()
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		clauses = append(clauses, &ast.OnClause{Span: span(onTok.Pos, p.extEnd(expr)), Expr: expr})
	case isKeyword(p.peek(), "USING"):
		uc, err := p.parseUsingClause()
		if err != nil {
			return nil, err
		}
		clauses = append(clauses, uc)
	}
	// The placeholder covers the whole pipe_join rule after the |> token.
	placeholder := &ast.PipeJoinLhsPlaceholder{Span: span(placeholderStart, p.prevEnd())}
	join, err := p.joinRuleAction(joinStart, placeholder, natural, joinType, joinHint, hint, rhs, clauses, joinLocation)
	if err != nil {
		return nil, err
	}
	return &ast.PipeJoin{Span: span(pipeTok.Pos, p.prevEnd()), Join: join}, nil
}

// unmatchedJoinCount, isTransformationNeeded, containsCommaJoin and
// getJoinParseError read the join bookkeeping fields off a node; non-join
// nodes have zero values. See the same-named helpers in join_processor.cc.
func unmatchedJoinCount(n ast.Node) int {
	if j, ok := n.(*ast.Join); ok {
		return j.UnmatchedJoinCount
	}
	return 0
}

func isTransformationNeeded(n ast.Node) bool {
	if j, ok := n.(*ast.Join); ok {
		return j.TransformationNeeded
	}
	return false
}

func containsCommaJoin(n ast.Node) bool {
	if j, ok := n.(*ast.Join); ok {
		return j.ContainsCommaJoin
	}
	return false
}

func getJoinParseError(n ast.Node) *ast.JoinParseError {
	if j, ok := n.(*ast.Join); ok {
		return j.ParseError
	}
	return nil
}

// isQualifiedJoin reports whether the join requires a join condition:
// CROSS, comma, and NATURAL joins do not; see IsQualifiedJoin in
// join_processor.cc.
func isQualifiedJoin(j *ast.Join) bool {
	return j.JoinType != "CROSS" && j.JoinType != "COMMA" && !j.Natural
}

// transformJoinExpression rewrites a join tree containing consecutive
// ON/USING clauses so each clause matches the nearest unmatched join; a port
// of TransformJoinExpression in join_processor.cc.
func (p *parser) transformJoinExpression(node ast.Node) (ast.Node, error) {
	if !isTransformationNeeded(node) {
		return node, nil
	}
	return p.processFlattenedJoinExpression(flattenJoinExpression(node))
}

// flattenJoinExpression flattens a left-deep join tree into a stack (top at
// the end of the slice) of table expressions, join nodes, and ON/USING
// clauses in source order from the top; a port of FlattenJoinExpression in
// join_processor.cc.
func flattenJoinExpression(node ast.Node) []ast.Node {
	var q []ast.Node
	for node != nil {
		join, ok := node.(*ast.Join)
		if !ok {
			q = append(q, node)
			break
		}
		// Push the clauses so that the first clause ends nearest the top.
		if join.ClauseList != nil {
			for i := len(join.ClauseList.Clauses) - 1; i >= 0; i-- {
				q = append(q, join.ClauseList.Clauses[i])
			}
		} else if join.OnOrUsingClause != nil {
			q = append(q, join.OnOrUsingClause)
		}
		q = append(q, join.Rhs, join)
		node = join.Lhs
	}
	return q
}

// processFlattenedJoinExpression rebuilds the join tree by matching each
// ON/USING clause with the nearest unmatched join; a port of
// ProcessFlattenedJoinExpression in join_processor.cc.
func (p *parser) processFlattenedJoinExpression(q []ast.Node) (ast.Node, error) {
	var stack []ast.Node
	popQ := func() ast.Node {
		n := q[len(q)-1]
		q = q[:len(q)-1]
		return n
	}
	popStack := func() ast.Node {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		return n
	}
	// Error tracking counts joins and conditions per join block; see
	// JoinErrorTracker in join_processor.cc.
	joinCount, condCount := 0, 0
	created := map[*ast.Join]bool{}

	for len(q) > 0 {
		item := popQ()
		switch t := item.(type) {
		case *ast.Join:
			if isQualifiedJoin(t) {
				joinCount++
				stack = append(stack, t)
				continue
			}
			// A CROSS, comma, or NATURAL join recombines immediately with
			// the inputs around it.
			lhs := popStack()
			rhs := popQ()
			newJoin := &ast.Join{
				Span:     span(t.Pos(), rhs.End()),
				JoinType: t.JoinType, JoinHint: t.JoinHint, Natural: t.Natural,
				Lhs: lhs, Hint: t.Hint,
				JoinLocation: &ast.Location{Span: t.JoinLocation.Span},
				Rhs:          rhs,
			}
			stack = append(stack, newJoin)
		case *ast.OnClause, *ast.UsingClause:
			condCount++
			if condCount == joinCount {
				// A new join block starts, so the counts reset.
				joinCount, condCount = 0, 0
			}
			rhs := popStack()
			join := popStack().(*ast.Join)
			lhs := popStack()
			newJoin := &ast.Join{
				Span:     span(join.Pos(), item.End()),
				JoinType: join.JoinType, JoinHint: join.JoinHint, Natural: join.Natural,
				Lhs: lhs, Hint: join.Hint,
				JoinLocation:    &ast.Location{Span: join.JoinLocation.Span},
				Rhs:             rhs,
				OnOrUsingClause: item,
			}
			created[newJoin] = true
			stack = append(stack, newJoin)
		default:
			stack = append(stack, item)
		}
	}

	if len(stack) == 1 {
		return stack[0], nil
	}
	// More joins than join conditions. The error points at the first
	// qualified join of the join block: the bottom-most qualified join in
	// the stack that the transformation did not create; see GenerateError in
	// join_processor.cc.
	var errJoin *ast.Join
	for i := len(stack) - 1; i >= 0; i-- {
		if j, ok := stack[i].(*ast.Join); ok && !created[j] && isQualifiedJoin(j) {
			errJoin = j
		}
	}
	if errJoin == nil {
		return nil, p.errorf(stack[len(stack)-1].Pos(), "Internal error: Failed to find the qualified join")
	}
	typeName := errJoin.JoinType
	if typeName == "" {
		typeName = "INNER"
	}
	return nil, p.errorf(errJoin.JoinLocation.Pos(),
		"Syntax error: The number of join conditions is %d but the number of joins that require a join condition is %d. %s JOIN must have an ON or USING clause",
		condCount, joinCount, typeName)
}
