package parser

// MATCH_RECOGNIZE clause and row pattern parsing; see match_recognize_clause
// and the row_pattern_* rules in googlesql/parser/googlesql.tm (Apache 2.0).

import (
	"github.com/sqlc-dev/zetajones/ast"
	"github.com/sqlc-dev/zetajones/token"
)

// parsePostfixTableOperators parses any trailing postfix table operators
// (currently MATCH_RECOGNIZE) onto a table primary, extending its span; see
// the "table_primary match_recognize_clause" alternative of table_primary in
// googlesql.tm.
func (p *parser) parsePostfixTableOperators(node ast.Node) (ast.Node, error) {
	// A non-reserved QUALIFY keyword followed by an expression in this
	// position is a QUALIFY clause that is not preceded by a WHERE, GROUP BY
	// or HAVING clause. The reference grammar rejects this via the
	// qualify_clause_nonreserved alternative of pivot_or_unpivot_clause.
	if p.isPostfixQualify() {
		qualifyTok := p.advance() // QUALIFY
		// Parse the expression so any error inside it is reported before the
		// clause-position errors below (matching the reference reduction order).
		if _, err := p.parseExpression(); err != nil {
			return nil, err
		}
		if !p.features.Enabled(FeatureQualify) {
			return nil, p.errorf(qualifyTok.Pos, "QUALIFY is not supported")
		}
		return nil, p.errorf(qualifyTok.Pos, "QUALIFY clause must be used in conjunction with WHERE or GROUP BY or HAVING clause")
	}
	for {
		var clause ast.Node
		var err error
		switch {
		case isKeyword(p.peek(), "MATCH_RECOGNIZE"):
			clause, err = p.parseMatchRecognizeClause()
		case isKeyword(p.peek(), "TABLESAMPLE"):
			clause, err = p.parseSampleClause()
		default:
			return node, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := node.(type) {
		case *ast.TablePathExpression:
			t.PostfixOperators = append(t.PostfixOperators, clause)
			t.Stop = clause.End()
		case *ast.TableSubquery:
			t.PostfixOperators = append(t.PostfixOperators, clause)
			t.Stop = clause.End()
		case *ast.TVF:
			t.PostfixOperators = append(t.PostfixOperators, clause)
			t.Stop = clause.End()
		case *ast.ParenthesizedJoin:
			t.PostfixOperators = append(t.PostfixOperators, clause)
			t.Stop = clause.End()
		default:
			return node, nil
		}
	}
}

// parseSampleClause parses "TABLESAMPLE <method> ( <size> ) [<suffix>]"; the
// TABLESAMPLE keyword is the next token. See sample_clause in googlesql.tm.
func (p *parser) parseSampleClause() (*ast.SampleClause, error) {
	tsTok := p.advance() // TABLESAMPLE
	clause := &ast.SampleClause{Span: span(tsTok.Pos, 0)}
	method, err := p.parseAliasIdentifier()
	if err != nil {
		return nil, err
	}
	clause.Method = method
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	size, err := p.parseSampleSize()
	if err != nil {
		return nil, err
	}
	clause.Size = size
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	clause.Stop = rparen.End
	suffix, err := p.parseOptionalSampleSuffix()
	if err != nil {
		return nil, err
	}
	if suffix != nil {
		clause.Suffix = suffix
		clause.Stop = suffix.End()
	}
	return clause, nil
}

// parseSampleSize parses "<value> ( ROWS | PERCENT ) [ PARTITION BY ... ]";
// see sample_size in googlesql.tm.
func (p *parser) parseSampleSize() (*ast.SampleSize, error) {
	start := p.peek().Pos
	var value ast.Node
	var err error
	if p.peek().Kind == token.FLOAT {
		tok := p.advance()
		value = &ast.FloatLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image}
	} else {
		value, err = p.parsePossiblyCastIntLiteralOrParameter()
		if err != nil {
			return nil, err
		}
	}
	size := &ast.SampleSize{Span: span(start, p.extEnd(value)), Value: value}
	switch {
	case isKeyword(p.peek(), "ROWS"):
		size.Unit = "ROWS"
	case isKeyword(p.peek(), "PERCENT"):
		size.Unit = "PERCENT"
	default:
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword ROWS or keyword PERCENT but got %s", describeToken(p.peek()))
	}
	unitTok := p.advance()
	size.Stop = unitTok.End
	// An optional PARTITION BY (without a hint); see
	// opt_partition_by_clause_no_hint in googlesql.tm.
	if isKeyword(p.peek(), "PARTITION") {
		partitionBy, err := p.parsePartitionBy()
		if err != nil {
			return nil, err
		}
		size.PartitionBy = partitionBy
		size.Stop = partitionBy.End()
	}
	return size, nil
}

// parseOptionalSampleSuffix parses the optional TABLESAMPLE suffix: a
// REPEATABLE clause and/or a WITH WEIGHT clause; see
// opt_sample_clause_suffix in googlesql.tm.
func (p *parser) parseOptionalSampleSuffix() (*ast.SampleSuffix, error) {
	switch {
	case isKeyword(p.peek(), "REPEATABLE"):
		repeatable, err := p.parseRepeatableClause()
		if err != nil {
			return nil, err
		}
		return &ast.SampleSuffix{Span: span(repeatable.Pos(), repeatable.End()), Repeatable: repeatable}, nil
	case isKeyword(p.peek(), "WITH"):
		withTok := p.advance() // WITH
		if _, err := p.expectKeyword("WEIGHT"); err != nil {
			return nil, err
		}
		weight := &ast.WithWeight{Span: span(withTok.Pos, p.prevEnd())}
		// Optional [AS] alias identifier.
		if isKeyword(p.peek(), "AS") || p.peek().Kind == token.IDENT && !isReserved(p.peek()) || p.peek().Kind == token.QUOTED_IDENT {
			alias, err := p.parseOptionalAlias()
			if err != nil {
				return nil, err
			}
			if alias != nil {
				weight.Alias = alias
				weight.Stop = alias.End()
			}
		}
		suffix := &ast.SampleSuffix{Span: span(withTok.Pos, weight.End()), Weight: weight}
		if isKeyword(p.peek(), "REPEATABLE") {
			repeatable, err := p.parseRepeatableClause()
			if err != nil {
				return nil, err
			}
			suffix.Repeatable = repeatable
			suffix.Stop = repeatable.End()
		}
		return suffix, nil
	}
	return nil, nil
}

// parseRepeatableClause parses "REPEATABLE ( <value> )"; see
// repeatable_clause in googlesql.tm.
func (p *parser) parseRepeatableClause() (*ast.RepeatableClause, error) {
	repTok := p.advance() // REPEATABLE
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	value, err := p.parsePossiblyCastIntLiteralOrParameter()
	if err != nil {
		return nil, err
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	return &ast.RepeatableClause{Span: span(repTok.Pos, rparen.End), Value: value}, nil
}

// parseMatchRecognizeClause parses "MATCH_RECOGNIZE ( ... ) [alias]"; the
// MATCH_RECOGNIZE keyword is the next token. See match_recognize_clause in
// googlesql.tm.
func (p *parser) parseMatchRecognizeClause() (*ast.MatchRecognizeClause, error) {
	mrTok := p.advance() // MATCH_RECOGNIZE
	clause := &ast.MatchRecognizeClause{Span: span(mrTok.Pos, 0)}
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	if isKeyword(p.peek(), "PARTITION") {
		partitionBy, err := p.parsePartitionBy()
		if err != nil {
			return nil, err
		}
		clause.PartitionBy = partitionBy
	}
	orderBy, err := p.parseOrderBy(false)
	if err != nil {
		return nil, err
	}
	clause.OrderBy = orderBy
	if _, err := p.expectKeyword("MEASURES"); err != nil {
		return nil, err
	}
	measures, err := p.parseSelectListWithRequiredAliases()
	if err != nil {
		return nil, err
	}
	clause.Measures = measures
	if isKeyword(p.peek(), "AFTER") {
		p.advance() // AFTER
		if _, err := p.expectKeyword("MATCH"); err != nil {
			return nil, err
		}
		if _, err := p.expectKeyword("SKIP"); err != nil {
			return nil, err
		}
		skip, err := p.parseAfterMatchSkipTarget()
		if err != nil {
			return nil, err
		}
		clause.AfterMatchSkip = skip
	}
	if _, err := p.expectKeyword("PATTERN"); err != nil {
		return nil, err
	}
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	pattern, err := p.parseRowPatternExpr()
	if err != nil {
		return nil, err
	}
	clause.Pattern = pattern
	if _, err := p.expect(token.RPAREN, `")"`); err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("DEFINE"); err != nil {
		return nil, err
	}
	definitions, err := p.parseWithExpressionVariableList()
	if err != nil {
		return nil, err
	}
	clause.Definitions = definitions
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		options, err := p.parseOptionsList()
		if err != nil {
			return nil, err
		}
		clause.Options = options
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	clause.Stop = rparen.End
	alias, err := p.parseOptionalAlias()
	if err != nil {
		return nil, err
	}
	if alias != nil {
		clause.Alias = alias
		clause.Stop = alias.End()
	}
	return clause, nil
}

// parseAfterMatchSkipTarget parses the skip_to_target after "AFTER MATCH
// SKIP": "PAST LAST ROW" or "TO NEXT ROW". The node's span covers only the
// target words; see skip_to_target in googlesql.tm.
func (p *parser) parseAfterMatchSkipTarget() (*ast.AfterMatchSkipClause, error) {
	start := p.peek()
	var targetType string
	switch {
	case isKeyword(start, "PAST"):
		p.advance()
		if _, err := p.expectKeyword("LAST"); err != nil {
			return nil, err
		}
		targetType = "PAST_LAST_ROW"
	case isKeyword(start, "TO"):
		p.advance()
		if _, err := p.expectKeyword("NEXT"); err != nil {
			return nil, err
		}
		targetType = "TO_NEXT_ROW"
	default:
		return nil, p.errorf(start.Pos, "Syntax error: Expected keyword PAST or keyword TO but got %s", describeToken(start))
	}
	rowTok, err := p.expectKeyword("ROW")
	if err != nil {
		return nil, err
	}
	return &ast.AfterMatchSkipClause{Span: span(start.Pos, rowTok.End), TargetType: targetType}, nil
}

// parseSelectListWithRequiredAliases parses "expr AS ident, ..." for the
// MEASURES clause; see select_list_prefix_with_as_aliases in googlesql.tm.
func (p *parser) parseSelectListWithRequiredAliases() (*ast.SelectList, error) {
	list := &ast.SelectList{Span: span(p.peek().Pos, 0)}
	for {
		exprStart := p.peek().Pos
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		asTok, err := p.expectKeyword("AS")
		if err != nil {
			return nil, err
		}
		ident, err := p.parseAliasIdentifier()
		if err != nil {
			return nil, err
		}
		alias := &ast.Alias{Span: span(asTok.Pos, ident.End()), Identifier: ident}
		col := &ast.SelectColumn{Span: span(exprStart, ident.End()), Expr: expr, Alias: alias}
		list.Columns = append(list.Columns, col)
		list.Stop = col.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance()
	}
	return list, nil
}

// parseWithExpressionVariableList parses "ident AS expr, ..." for the DEFINE
// clause. Each entry becomes a SelectColumn whose alias node covers "ident
// AS"; see with_expression_variable_prefix in googlesql.tm.
func (p *parser) parseWithExpressionVariableList() (*ast.SelectList, error) {
	list := &ast.SelectList{Span: span(p.peek().Pos, 0)}
	for {
		ident, err := p.parseAliasIdentifier()
		if err != nil {
			return nil, err
		}
		asTok, err := p.expectKeyword("AS")
		if err != nil {
			return nil, err
		}
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		alias := &ast.Alias{Span: span(ident.Pos(), asTok.End), Identifier: ident}
		col := &ast.SelectColumn{Span: span(ident.Pos(), p.extEnd(expr)), Expr: expr, Alias: alias}
		list.Columns = append(list.Columns, col)
		list.Stop = col.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance()
	}
	return list, nil
}

// parseAliasIdentifier consumes an identifier token (an unreserved keyword
// or a quoted identifier), reporting the reference parser's generic
// "Unexpected" error otherwise.
func (p *parser) parseAliasIdentifier() (*ast.Identifier, error) {
	tok := p.peek()
	if tok.Kind != token.QUOTED_IDENT && (tok.Kind != token.IDENT || isReserved(tok)) {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	return p.parseIdentifierToken(p.advance()), nil
}

// parseRowPatternExpr parses a row pattern alternation over "|" and "||";
// see row_pattern_expr in googlesql.tm. Same-level operands flatten into a
// single ALTERNATE operation, and "||" contributes an empty pattern between
// its two bars (see MakeOrCombineRowPatternOperation in parser_internal.h).
func (p *parser) parseRowPatternExpr() (ast.Node, error) {
	exprStart := p.peek().Pos
	first, err := p.parseRowPatternConcatenationOrEmpty()
	if err != nil {
		return nil, err
	}
	if k := p.peek().Kind; k != token.PIPE && k != token.CONCAT {
		return first, nil
	}
	// An unparenthesized empty pattern on the left keeps the location of
	// the end of the previous token, outside the operation; move it to the
	// operation's start (the first "|").
	if e, ok := first.(*ast.EmptyRowPattern); ok && !e.Parenthesized {
		e.Start, e.Stop = exprStart, exprStart
	}
	op := &ast.RowPatternOperation{
		Span:     span(exprStart, 0),
		OpType:   "ALTERNATE",
		Operands: []ast.Node{first},
	}
	for {
		barTok := p.peek()
		if barTok.Kind == token.CONCAT {
			// "||" is one token; it contributes an empty pattern located
			// between the two bars.
			p.advance()
			mid := barTok.Pos + 1
			op.Operands = append(op.Operands, &ast.EmptyRowPattern{Span: span(mid, mid)})
		} else if barTok.Kind == token.PIPE {
			p.advance()
		} else {
			break
		}
		next, err := p.parseRowPatternConcatenationOrEmpty()
		if err != nil {
			return nil, err
		}
		op.Operands = append(op.Operands, next)
		op.Stop = p.prevEnd()
	}
	return op, nil
}

// parseRowPatternConcatenationOrEmpty parses a concatenation of row pattern
// factors, or an empty pattern when the next token ends the alternation
// operand; see row_pattern_concatenation_or_empty in googlesql.tm. An empty
// pattern is located at the end of the previous token.
func (p *parser) parseRowPatternConcatenationOrEmpty() (ast.Node, error) {
	if k := p.peek().Kind; k == token.PIPE || k == token.CONCAT || k == token.RPAREN {
		pos := p.prevEnd()
		return &ast.EmptyRowPattern{Span: span(pos, pos)}, nil
	}
	if !p.atRowPatternFactorStart() {
		return nil, p.rowPatternError()
	}
	concatStart := p.peek().Pos
	first, err := p.parseRowPatternFactor()
	if err != nil {
		return nil, err
	}
	var op *ast.RowPatternOperation
	for p.atRowPatternFactorStart() {
		factor, err := p.parseRowPatternFactor()
		if err != nil {
			return nil, err
		}
		if op == nil {
			op = &ast.RowPatternOperation{
				Span:     span(concatStart, 0),
				OpType:   "CONCAT",
				Operands: []ast.Node{first},
			}
		}
		op.Operands = append(op.Operands, factor)
		op.Stop = p.prevEnd()
	}
	if k := p.peek().Kind; k != token.PIPE && k != token.CONCAT && k != token.RPAREN {
		return nil, p.rowPatternError()
	}
	if op != nil {
		return op, nil
	}
	return first, nil
}

// rowPatternError reports the reference parser's error for a token that
// cannot continue a row pattern.
func (p *parser) rowPatternError() error {
	return p.errorf(p.peek().Pos, `Syntax error: Expected ")" or "|" or || but got %s`, describeToken(p.peek()))
}

// atRowPatternFactorStart reports whether the next token can start a row
// pattern factor: a pattern variable, a parenthesized pattern, or an anchor.
func (p *parser) atRowPatternFactorStart() bool {
	tok := p.peek()
	switch tok.Kind {
	case token.QUOTED_IDENT, token.LPAREN, token.CARET, token.DOLLAR:
		return true
	case token.IDENT:
		return !isReserved(tok)
	}
	return false
}

// parseRowPatternFactor parses one row pattern factor: an anchor, or a
// primary with an optional quantifier; see row_pattern_factor in
// googlesql.tm. Anchors cannot be quantified; a quantifier after an anchor
// fails the concatenation loop instead.
func (p *parser) parseRowPatternFactor() (ast.Node, error) {
	tok := p.peek()
	switch tok.Kind {
	case token.CARET:
		p.advance()
		return &ast.RowPatternAnchor{Span: span(tok.Pos, tok.End), Anchor: "START"}, nil
	case token.DOLLAR:
		p.advance()
		return &ast.RowPatternAnchor{Span: span(tok.Pos, tok.End), Anchor: "END"}, nil
	}
	primaryStart := tok.Pos
	var primary ast.Node
	if tok.Kind == token.LPAREN {
		p.advance() // (
		inner, err := p.parseRowPatternExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(token.RPAREN, `")"`); err != nil {
			return nil, err
		}
		// The location stays on the inner expression, excluding the
		// parentheses; see row_pattern_primary in googlesql.tm.
		switch t := inner.(type) {
		case *ast.EmptyRowPattern:
			t.Parenthesized = true
		case *ast.RowPatternOperation:
			t.Parenthesized = true
		}
		primary = inner
	} else {
		ident := p.parseIdentifierToken(p.advance())
		primary = &ast.RowPatternVariable{Span: span(ident.Pos(), ident.End()), Name: ident}
	}
	switch p.peek().Kind {
	case token.QUESTION, token.PLUS, token.STAR, token.LBRACE:
		quantifier, err := p.parseRowPatternQuantifier()
		if err != nil {
			return nil, err
		}
		return &ast.RowPatternQuantification{
			Span:       span(primaryStart, quantifier.End()),
			Primary:    primary,
			Quantifier: quantifier,
		}, nil
	}
	return primary, nil
}

// parseRowPatternQuantifier parses "?", "+", "*", "{n}", or "{lo,hi}", each
// except "{n}" optionally followed by "?" to mark it reluctant; see
// row_pattern_quantifier in googlesql.tm.
func (p *parser) parseRowPatternQuantifier() (ast.Node, error) {
	tok := p.advance()
	switch tok.Kind {
	case token.QUESTION, token.PLUS, token.STAR:
		p.markQuantifierQuestion(tok)
		symbol := map[token.Kind]string{
			token.QUESTION: "QUESTION_MARK",
			token.PLUS:     "PLUS",
			token.STAR:     "STAR",
		}[tok.Kind]
		q := &ast.SymbolQuantifier{Span: span(tok.Pos, tok.End), Symbol: symbol}
		if p.peek().Kind == token.QUESTION {
			reluctant := p.advance()
			p.markQuantifierQuestion(reluctant)
			q.IsReluctant = true
			q.Stop = reluctant.End
		}
		return q, nil
	}
	// "{": fixed or bounded quantifier.
	lower, err := p.parseOptionalQuantifierBound()
	if err != nil {
		return nil, err
	}
	if lower != nil && p.peek().Kind == token.RBRACE {
		rbrace := p.advance()
		return &ast.FixedQuantifier{Span: span(tok.Pos, rbrace.End), Bound: lower}, nil
	}
	if _, err := p.expect(token.COMMA, `","`); err != nil {
		return nil, err
	}
	upper, err := p.parseOptionalQuantifierBound()
	if err != nil {
		return nil, err
	}
	rbrace, err := p.expect(token.RBRACE, `"}"`)
	if err != nil {
		return nil, err
	}
	// Both bounds span the whole "{lo,hi}" range; see the
	// potentially_reluctant_quantifier rule in googlesql.tm.
	q := &ast.BoundedQuantifier{
		Span:       span(tok.Pos, rbrace.End),
		LowerBound: &ast.QuantifierBound{Span: span(tok.Pos, rbrace.End), Bound: lower},
		UpperBound: &ast.QuantifierBound{Span: span(tok.Pos, rbrace.End), Bound: upper},
	}
	if p.peek().Kind == token.QUESTION {
		reluctant := p.advance()
		p.markQuantifierQuestion(reluctant)
		q.IsReluctant = true
		q.Stop = reluctant.End
	}
	return q, nil
}

// parseOptionalQuantifierBound parses an int_literal_or_parameter bound if
// one is present: an integer literal, a query parameter, or a system
// variable; see int_literal_or_parameter in googlesql.tm.
func (p *parser) parseOptionalQuantifierBound() (ast.Node, error) {
	tok := p.peek()
	switch tok.Kind {
	case token.INT:
		p.advance()
		return &ast.IntLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image}, nil
	case token.QUESTION:
		p.advance()
		return &ast.ParameterExpr{
			Span:     span(tok.Pos, tok.End),
			Position: p.positionalParameterOrdinal(),
		}, nil
	case token.PARAM:
		p.advance()
		name := &ast.Identifier{Span: span(tok.Pos+1, tok.End), Name: tok.Image[1:]}
		return &ast.ParameterExpr{Span: span(tok.Pos, tok.End), Name: name}, nil
	case token.SYSTEM_VARIABLE:
		return p.parseSystemVariableExpr()
	}
	return nil, nil
}

// markQuantifierQuestion records a "?" token consumed as a row pattern
// quantifier (or reluctant marker) so that positional query parameter
// numbering skips it.
func (p *parser) markQuantifierQuestion(tok token.Token) {
	if tok.Kind != token.QUESTION {
		return
	}
	if p.quantifierQuestions == nil {
		p.quantifierQuestions = map[int]bool{}
	}
	p.quantifierQuestions[tok.Pos] = true
}
