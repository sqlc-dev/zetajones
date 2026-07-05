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
	if p.isPostfixQualify() && !p.inFromQuery {
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
	// A single optional PIVOT or UNPIVOT clause may follow the table primary
	// (and its alias), before any MATCH_RECOGNIZE or TABLESAMPLE operators;
	// see the pivot_or_unpivot_clause references in the table_path_expression,
	// table_subquery, and tvf rules in googlesql.tm. It is not allowed on a
	// parenthesized join (which falls through to an "Expected end of input"
	// error at the enclosing level).
	switch node.(type) {
	case *ast.TablePathExpression, *ast.TableSubquery, *ast.TVF:
		if isKeyword(p.peek(), "PIVOT") || isKeyword(p.peek(), "UNPIVOT") {
			clause, err := p.parsePivotOrUnpivotClause()
			if err != nil {
				return nil, err
			}
			if _, ok := clause.(*ast.PivotClause); ok {
				if sub, ok := node.(*ast.TableSubquery); ok {
					sub.Query.IsPivotInput = true
				}
			}
			// A PIVOT/UNPIVOT clause and FOR SYSTEM TIME AS OF cannot be
			// combined on a table path expression; see table_path_expression
			// in googlesql.tm.
			if _, ok := node.(*ast.TablePathExpression); ok &&
				isKeyword(p.peek(), "FOR") &&
				(isKeyword(p.peekAt(1), "SYSTEM") || isKeyword(p.peekAt(1), "SYSTEM_TIME")) {
				kind := "PIVOT"
				if _, ok := clause.(*ast.UnpivotClause); ok {
					kind = "UNPIVOT"
				}
				return nil, p.errorf(p.peek().Pos, "Syntax error: %s and FOR SYSTEM TIME AS OF may not be combined", kind)
			}
			node = attachPostfixOperator(node, clause)
		}
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
		if attachPostfixOperator(node, clause) == nil {
			return node, nil
		}
	}
}

// attachPostfixOperator appends a postfix table operator to a table primary
// and extends its span, returning the node (or nil for a node kind that
// cannot carry postfix operators).
func attachPostfixOperator(node ast.Node, clause ast.Node) ast.Node {
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
		return nil
	}
	return node
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
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword PERCENT or keyword ROWS but got %s", describeToken(p.peek()))
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
		// Optional [AS] alias identifier. "REPEATABLE(" starts the repeatable
		// clause rather than an alias; the reference grammar resolves the
		// shift/reduce conflict in favor of repeatable_clause when the "("
		// lookahead is present (see the note on opt_sample_clause_suffix in
		// googlesql.tm). A bare REPEATABLE not followed by "(" is an alias.
		startsRepeatable := isKeyword(p.peek(), "REPEATABLE") && p.peekAt(1).Kind == token.LPAREN
		if isKeyword(p.peek(), "AS") || (!startsRepeatable && p.peek().Kind == token.IDENT && !isReserved(p.peek())) || p.peek().Kind == token.QUOTED_IDENT {
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
			// The WithWeight node's location spans the whole
			// "WITH WEIGHT [alias] [REPEATABLE(...)]" production; its @$
			// includes the trailing repeatable clause. See
			// opt_sample_clause_suffix in googlesql.tm.
			weight.Stop = repeatable.End()
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

// parsePivotOrUnpivotClause parses one PIVOT or UNPIVOT postfix table
// operator, including its optional output alias; see pivot_clause,
// unpivot_clause, and pivot_or_unpivot_clause in googlesql.tm.
func (p *parser) parsePivotOrUnpivotClause() (ast.Node, error) {
	if isKeyword(p.peek(), "UNPIVOT") {
		return p.parseUnpivotClause()
	}
	return p.parsePivotClause()
}

// parsePivotClause parses "PIVOT ( pivot_expression_list FOR expr IN (
// pivot_value_list ) ) [[AS] alias]"; the PIVOT keyword is the next token.
// See pivot_clause in googlesql.tm.
func (p *parser) parsePivotClause() (ast.Node, error) {
	pivotTok := p.advance() // PIVOT
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	exprs, err := p.parsePivotExpressionList()
	if err != nil {
		return nil, err
	}
	// After the pivot expression list, the grammar continues with a "," (more
	// expressions) or FOR.
	if !isKeyword(p.peek(), "FOR") {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "," or keyword FOR but got %s`, describeToken(p.peek()))
	}
	p.advance() // FOR
	// The FOR expression is an expression_higher_prec_than_and; the following
	// IN introduces the pivot value list, so it must not be consumed as an IN
	// expression.
	p.suppressTopLevelIn = true
	forExpr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	// After the FOR expression, IN introduces the value list; any other token
	// is an unexpected continuation of the expression.
	if !isKeyword(p.peek(), "IN") {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Unexpected %s", describeToken(p.peek()))
	}
	p.advance() // IN
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	values, err := p.parsePivotValueList()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(token.RPAREN, `")"`); err != nil {
		return nil, err
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	clause := &ast.PivotClause{
		Span:        span(pivotTok.Pos, rparen.End),
		Expressions: exprs,
		ForExpr:     forExpr,
		Values:      values,
	}
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

// parsePivotExpressionList parses a comma-separated list of pivot
// expressions; see pivot_expression_list in googlesql.tm.
func (p *parser) parsePivotExpressionList() (*ast.PivotExpressionList, error) {
	list := &ast.PivotExpressionList{Span: span(p.peek().Pos, 0)}
	for {
		expr, err := p.parsePivotExpression()
		if err != nil {
			return nil, err
		}
		list.Expressions = append(list.Expressions, expr)
		list.Stop = expr.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance() // ,
	}
	return list, nil
}

// parsePivotExpression parses "expression [[AS] alias]"; see pivot_expression
// in googlesql.tm.
func (p *parser) parsePivotExpression() (*ast.PivotExpression, error) {
	start := p.peek().Pos
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	pe := &ast.PivotExpression{Span: span(start, p.extEnd(expr)), Expr: expr}
	alias, err := p.parseOptionalAlias()
	if err != nil {
		return nil, err
	}
	if alias != nil {
		pe.Alias = alias
		pe.Stop = alias.End()
	}
	return pe, nil
}

// parsePivotValueList parses a comma-separated list of pivot values; see
// pivot_value_list in googlesql.tm.
func (p *parser) parsePivotValueList() (*ast.PivotValueList, error) {
	list := &ast.PivotValueList{Span: span(p.peek().Pos, 0)}
	for {
		value, err := p.parsePivotValue()
		if err != nil {
			return nil, err
		}
		list.Values = append(list.Values, value)
		list.Stop = value.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance() // ,
	}
	return list, nil
}

// parsePivotValue parses "expression [[AS] alias]"; see pivot_value in
// googlesql.tm.
func (p *parser) parsePivotValue() (*ast.PivotValue, error) {
	start := p.peek().Pos
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	pv := &ast.PivotValue{Span: span(start, p.extEnd(expr)), Value: expr}
	alias, err := p.parseOptionalAlias()
	if err != nil {
		return nil, err
	}
	if alias != nil {
		pv.Alias = alias
		pv.Stop = alias.End()
	}
	return pv, nil
}

// parseUnpivotClause parses "UNPIVOT [EXCLUDE|INCLUDE NULLS] (
// path_expression_list_with_opt_parens FOR path_expression IN
// unpivot_in_item_list ) [[AS] alias]"; the UNPIVOT keyword is the next
// token. See unpivot_clause in googlesql.tm.
func (p *parser) parseUnpivotClause() (ast.Node, error) {
	unpivotTok := p.advance() // UNPIVOT
	nullFilter := ""
	switch {
	case isKeyword(p.peek(), "EXCLUDE"):
		p.advance()
		if _, err := p.expectKeyword("NULLS"); err != nil {
			return nil, err
		}
		nullFilter = "EXCLUDE NULLS"
	case isKeyword(p.peek(), "INCLUDE"):
		p.advance()
		if _, err := p.expectKeyword("NULLS"); err != nil {
			return nil, err
		}
		nullFilter = "INCLUDE NULLS"
	}
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	columns, err := p.parsePathExpressionListWithOptParens()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("FOR"); err != nil {
		return nil, err
	}
	forExpr, err := p.parseUnpivotPathExpression()
	if err != nil {
		return nil, err
	}
	// After the FOR path expression, the grammar can extend the path with a
	// "." or reduce and expect IN; report both when neither follows.
	if !isKeyword(p.peek(), "IN") {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "." or keyword IN but got %s`, describeToken(p.peek()))
	}
	p.advance() // IN
	inItems, err := p.parseUnpivotInItemList()
	if err != nil {
		return nil, err
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	clause := &ast.UnpivotClause{
		Span:       span(unpivotTok.Pos, rparen.End),
		NullFilter: nullFilter,
		Columns:    columns,
		ForExpr:    forExpr,
		InItems:    inItems,
	}
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

// parseUnpivotPathExpression parses a required path expression in an UNPIVOT
// clause, reporting the reference parser's generic "Unexpected" error (rather
// than "Expected identifier") when the next token cannot start a path.
func (p *parser) parseUnpivotPathExpression() (*ast.PathExpression, error) {
	if tok := p.peek(); (tok.Kind != token.IDENT && tok.Kind != token.QUOTED_IDENT) || isReserved(tok) {
		return nil, p.errorf(tok.Pos, "Syntax error: Unexpected %s", describeToken(tok))
	}
	return p.parsePathExpression()
}

// parsePathExpressionList parses a comma-separated list of path expressions;
// see path_expression_list in googlesql.tm.
func (p *parser) parsePathExpressionList() (*ast.PathExpressionList, error) {
	list := &ast.PathExpressionList{Span: span(p.peek().Pos, 0)}
	for {
		path, err := p.parseUnpivotPathExpression()
		if err != nil {
			return nil, err
		}
		list.Paths = append(list.Paths, path)
		list.Stop = path.End()
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance() // ,
	}
	return list, nil
}

// parsePathExpressionListWithOptParens parses either "( path_expression_list
// )" or a single bare path expression; the list's span excludes the optional
// surrounding parentheses. See path_expression_list_with_opt_parens in
// googlesql.tm.
func (p *parser) parsePathExpressionListWithOptParens() (*ast.PathExpressionList, error) {
	if p.peek().Kind == token.LPAREN {
		p.advance() // (
		list, err := p.parsePathExpressionList()
		if err != nil {
			return nil, err
		}
		// After each path the list continues with "," or closes with ")".
		if p.peek().Kind != token.RPAREN {
			return nil, p.errorf(p.peek().Pos, `Syntax error: Expected ")" or "," but got %s`, describeToken(p.peek()))
		}
		p.advance() // )
		return list, nil
	}
	// The unparenthesized form is a single path expression, not a
	// comma-separated list.
	path, err := p.parseUnpivotPathExpression()
	if err != nil {
		return nil, err
	}
	return &ast.PathExpressionList{Span: span(path.Pos(), path.End()), Paths: []*ast.PathExpression{path}}, nil
}

// parseUnpivotInItemList parses "( unpivot_in_item , ... )"; its span
// includes the parentheses. See unpivot_in_item_list in googlesql.tm.
func (p *parser) parseUnpivotInItemList() (*ast.UnpivotInItemList, error) {
	lparen, err := p.expect(token.LPAREN, `"("`)
	if err != nil {
		return nil, err
	}
	list := &ast.UnpivotInItemList{Span: span(lparen.Pos, 0)}
	for {
		item, err := p.parseUnpivotInItem()
		if err != nil {
			return nil, err
		}
		list.Items = append(list.Items, item)
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance() // ,
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	list.Stop = rparen.End
	return list, nil
}

// parseUnpivotInItem parses "path_expression_list_with_opt_parens
// [opt_as_string_or_integer]"; its span includes any parentheses around the
// column list. See unpivot_in_item in googlesql.tm.
func (p *parser) parseUnpivotInItem() (*ast.UnpivotInItem, error) {
	start := p.peek().Pos
	columns, err := p.parsePathExpressionListWithOptParens()
	if err != nil {
		return nil, err
	}
	item := &ast.UnpivotInItem{Span: span(start, p.prevEnd()), Columns: columns}
	label, err := p.parseOptionalUnpivotInItemLabel()
	if err != nil {
		return nil, err
	}
	if label != nil {
		item.Label = label
		item.Stop = label.End()
	}
	return item, nil
}

// parseOptionalUnpivotInItemLabel parses "[AS] (integer_literal |
// string_literal)"; see opt_as_string_or_integer in googlesql.tm. A label is
// required after AS; without AS one is present only when a string or integer
// literal directly follows.
func (p *parser) parseOptionalUnpivotInItemLabel() (*ast.UnpivotInItemLabel, error) {
	start := p.peek().Pos
	hasAs := false
	if isKeyword(p.peek(), "AS") {
		p.advance()
		hasAs = true
	}
	tok := p.peek()
	switch tok.Kind {
	case token.STRING:
		lit, err := p.parseStringLiteral()
		if err != nil {
			return nil, err
		}
		return &ast.UnpivotInItemLabel{Span: span(start, lit.End()), Label: lit}, nil
	case token.INT:
		p.advance()
		lit := &ast.IntLiteral{Span: span(tok.Pos, tok.End), Image: tok.Image}
		return &ast.UnpivotInItemLabel{Span: span(start, lit.End()), Label: lit}, nil
	}
	if hasAs {
		return nil, p.errorf(tok.Pos, "Syntax error: Expected integer literal or string literal but got %s", describeToken(tok))
	}
	return nil, nil
}
