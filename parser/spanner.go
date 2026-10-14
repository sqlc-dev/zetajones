package parser

import (
	"strings"

	"github.com/sqlc-dev/zetajones/ast"
	"github.com/sqlc-dev/zetajones/token"
)

// Cloud Spanner DDL that GoogleSQL's reference parser does not accept, gated
// by FeatureSpannerDDL. The nodes are defined in ast/spanner.go; reference
// nodes are reused wherever the Spanner syntax has a GoogleSQL equivalent.

// spannerEntityTypes and spannerSubEntityTypes are the generic entity and
// sub-entity types FeatureSpannerDDL accepts: CREATE/DROP ROLE and PLACEMENT
// parse as CreateEntityStatement/DropEntityStatement, and ALTER TABLE ADD/DROP
// SYNONYM as AddSubEntityAction/DropSubEntityAction.
var (
	spannerEntityTypes    = []string{"ROLE", "PLACEMENT"}
	spannerSubEntityTypes = []string{"SYNONYM"}
)

// spanner reports whether FeatureSpannerDDL is enabled.
func (p *parser) spanner() bool {
	return p.features.Enabled(FeatureSpannerDDL)
}

// parseCreateLocalityGroupStatement parses "CREATE LOCALITY GROUP path
// [OPTIONS(...)]"; see create_locality_group_statement in googlesql.tm. The
// next token is LOCALITY. Requires FEATURE_CREATE_LOCALITY_GROUP.
func (p *parser) parseCreateLocalityGroupStatement(createTok token.Token) (ast.Statement, error) {
	localityTok := p.advance() // LOCALITY
	if _, err := p.expectKeyword("GROUP"); err != nil {
		return nil, err
	}
	name, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	stmt := &ast.CreateLocalityGroupStatement{Span: span(createTok.Pos, name.End()), Name: name}
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		if stmt.Options, err = p.parseOptionsList(); err != nil {
			return nil, err
		}
		stmt.Stop = stmt.Options.End()
	}
	if !p.features.Enabled(FeatureCreateLocalityGroup) {
		// No "Syntax error: " prefix, matching the reference rule action.
		return nil, p.errorf(localityTok.Pos, "CREATE LOCALITY GROUP is not supported")
	}
	return stmt, nil
}

// parseSpannerCreateStatement parses the Spanner-only CREATE statements
// (CHANGE STREAM, PROTO BUNDLE, QUEUE) when the tokens after CREATE start one,
// reporting ok=false otherwise.
func (p *parser) parseSpannerCreateStatement(createTok token.Token) (stmt ast.Statement, ok bool, err error) {
	switch tok, next := p.peek(), p.peekAt(1); {
	case isKeyword(tok, "CHANGE") && isKeyword(next, "STREAM"):
		stmt, err = p.parseCreateChangeStreamStatement(createTok)
	case isKeyword(tok, "PROTO") && isKeyword(next, "BUNDLE"):
		stmt, err = p.parseCreateProtoBundleStatement(createTok)
	case isKeyword(tok, "QUEUE") && next.Kind != token.LPAREN:
		stmt, err = p.parseCreateQueueStatement(createTok)
	default:
		return nil, false, nil
	}
	return stmt, true, err
}

// parseCreateChangeStreamStatement parses "CREATE CHANGE STREAM [IF NOT
// EXISTS] name [FOR ...] [OPTIONS (...)]". CHANGE is the next token.
func (p *parser) parseCreateChangeStreamStatement(createTok token.Token) (ast.Statement, error) {
	p.advance() // CHANGE
	p.advance() // STREAM
	ifNotExists, err := p.parseOptIfNotExists()
	if err != nil {
		return nil, err
	}
	name, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	stmt := &ast.CreateChangeStreamStatement{Span: span(createTok.Pos, name.End()), IsIfNotExists: ifNotExists, Name: name}
	if isKeyword(p.peek(), "FOR") {
		if stmt.For, err = p.parseChangeStreamForClause(); err != nil {
			return nil, err
		}
		stmt.Stop = stmt.For.End()
	}
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		if stmt.Options, err = p.parseOptionsList(); err != nil {
			return nil, err
		}
		stmt.Stop = stmt.Options.End()
	}
	return stmt, nil
}

// parseChangeStreamForClause parses "FOR ALL" or "FOR table [(column, ...)],
// ...". FOR is the next token.
func (p *parser) parseChangeStreamForClause() (*ast.ChangeStreamForClause, error) {
	forTok := p.advance() // FOR
	if isKeyword(p.peek(), "ALL") {
		allTok := p.advance()
		return &ast.ChangeStreamForClause{Span: span(forTok.Pos, allTok.End), All: true}, nil
	}
	clause := &ast.ChangeStreamForClause{Span: span(forTok.Pos, 0)}
	for {
		table, err := p.parsePathExpression()
		if err != nil {
			return nil, err
		}
		tracked := &ast.ChangeStreamTrackedTable{Span: span(table.Pos(), table.End()), Table: table}
		if p.peek().Kind == token.LPAREN {
			if tracked.Columns, err = p.parseParenColumnList(); err != nil {
				return nil, err
			}
			tracked.Stop = tracked.Columns.End()
		}
		clause.Tables = append(clause.Tables, tracked)
		clause.Stop = tracked.End()
		if p.peek().Kind != token.COMMA {
			return clause, nil
		}
		p.advance() // ,
	}
}

// parseParenColumnList parses "( [identifier, ...] )", allowing an empty
// list. The list's location includes the parentheses.
func (p *parser) parseParenColumnList() (*ast.ColumnList, error) {
	lparen, err := p.expect(token.LPAREN, `"("`)
	if err != nil {
		return nil, err
	}
	list := &ast.ColumnList{Span: span(lparen.Pos, 0)}
	for p.peek().Kind != token.RPAREN {
		id, err := p.parseIdentifier()
		if err != nil {
			return nil, err
		}
		list.Identifiers = append(list.Identifiers, id)
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

// parseAlterChangeStreamStatement parses "ALTER CHANGE STREAM [IF EXISTS]
// name {SET FOR ... | DROP FOR ALL | SET OPTIONS (...)}". CHANGE is the next
// token.
func (p *parser) parseAlterChangeStreamStatement(alterTok token.Token) (ast.Statement, error) {
	p.advance() // CHANGE
	p.advance() // STREAM
	stmt := &ast.AlterChangeStreamStatement{IsIfExists: p.tryParseIfExists()}
	var err error
	if stmt.Name, err = p.parsePathExpression(); err != nil {
		return nil, err
	}
	list := &ast.AlterActionList{}
	for {
		tok := p.peek()
		var action ast.Node
		switch {
		case isKeyword(tok, "SET") && isKeyword(p.peekAt(1), "FOR"):
			p.advance() // SET
			forClause, err := p.parseChangeStreamForClause()
			if err != nil {
				return nil, err
			}
			action = &ast.SetChangeStreamForAction{Span: span(tok.Pos, forClause.End()), For: forClause}
		case isKeyword(tok, "SET") && isKeyword(p.peekAt(1), "OPTIONS"):
			p.advance() // SET
			p.advance() // OPTIONS
			opts, err := p.parseOptionsList()
			if err != nil {
				return nil, err
			}
			action = &ast.SetOptionsAction{Span: span(tok.Pos, opts.End()), Options: opts}
		case isKeyword(tok, "DROP"):
			p.advance() // DROP
			if _, err := p.expectKeyword("FOR"); err != nil {
				return nil, err
			}
			allTok, err := p.expectKeyword("ALL")
			if err != nil {
				return nil, err
			}
			action = &ast.DropChangeStreamForAllAction{Span: span(tok.Pos, allTok.End)}
		default:
			return nil, p.errorf(tok.Pos, "Syntax error: Expected keyword DROP or keyword SET but got %s", describeToken(tok))
		}
		list.Actions = append(list.Actions, action)
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance() // ,
	}
	list.Span = span(list.Actions[0].Pos(), p.prevEnd())
	stmt.Actions = list
	stmt.Span = span(alterTok.Pos, p.prevEnd())
	return stmt, nil
}

// parseAlterLocalityGroupStatement parses "ALTER LOCALITY GROUP [IF EXISTS]
// name alter_action_list" (in practice SET OPTIONS (...)). LOCALITY is the
// next token.
func (p *parser) parseAlterLocalityGroupStatement(alterTok token.Token) (ast.Statement, error) {
	p.advance() // LOCALITY
	p.advance() // GROUP
	stmt := &ast.AlterLocalityGroupStatement{IsIfExists: p.tryParseIfExists()}
	var err error
	if stmt.Name, err = p.parsePathExpression(); err != nil {
		return nil, err
	}
	if stmt.Actions, err = p.parseAlterActionList(); err != nil {
		return nil, err
	}
	stmt.Span = span(alterTok.Pos, stmt.Actions.End())
	return stmt, nil
}

// parseCreateProtoBundleStatement parses "CREATE PROTO BUNDLE (type, ...)".
// PROTO is the next token.
func (p *parser) parseCreateProtoBundleStatement(createTok token.Token) (ast.Statement, error) {
	p.advance() // PROTO
	p.advance() // BUNDLE
	types, err := p.parseProtoBundleTypeList()
	if err != nil {
		return nil, err
	}
	return &ast.CreateProtoBundleStatement{Span: span(createTok.Pos, types.End()), Types: types}, nil
}

// parseProtoBundleTypeList parses "( path [, ...] [,] )": the proto type names
// of a proto bundle, allowing a trailing comma. The list's location includes
// the parentheses.
func (p *parser) parseProtoBundleTypeList() (*ast.PathExpressionList, error) {
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
		p.advance() // ,
		if p.peek().Kind == token.RPAREN {
			break
		}
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	list.Stop = rparen.End
	return list, nil
}

// parseAlterProtoBundleStatement parses "ALTER PROTO BUNDLE [INSERT (...)]
// [UPDATE (...)] [DELETE (...)]" with at least one action. PROTO is the next
// token.
func (p *parser) parseAlterProtoBundleStatement(alterTok token.Token) (ast.Statement, error) {
	p.advance() // PROTO
	p.advance() // BUNDLE
	list := &ast.AlterActionList{}
	for isKeyword(p.peek(), "INSERT") || isKeyword(p.peek(), "UPDATE") || isKeyword(p.peek(), "DELETE") {
		opTok := p.advance()
		types, err := p.parseProtoBundleTypeList()
		if err != nil {
			return nil, err
		}
		list.Actions = append(list.Actions, &ast.ProtoBundleAction{Span: span(opTok.Pos, types.End()), Op: strings.ToUpper(opTok.Image), Types: types})
	}
	if len(list.Actions) == 0 {
		return nil, p.errorf(p.peek().Pos, "Syntax error: Expected keyword DELETE or keyword INSERT or keyword UPDATE but got %s", describeToken(p.peek()))
	}
	list.Span = span(list.Actions[0].Pos(), p.prevEnd())
	return &ast.AlterProtoBundleStatement{Span: span(alterTok.Pos, p.prevEnd()), Actions: list}, nil
}

// parseDropProtoBundleStatement parses "DROP PROTO BUNDLE". PROTO is the next
// token.
func (p *parser) parseDropProtoBundleStatement(dropTok token.Token) (ast.Statement, error) {
	p.advance()              // PROTO
	bundleTok := p.advance() // BUNDLE
	return &ast.DropProtoBundleStatement{Span: span(dropTok.Pos, bundleTok.End)}, nil
}

// parseCreateQueueStatement parses "CREATE QUEUE [IF NOT EXISTS] name
// (elements) PRIMARY KEY (...) [, INTERLEAVE IN [PARENT] ...] [, OPTIONS
// (...)]": a queue is declared like a Spanner table. QUEUE is the next token.
func (p *parser) parseCreateQueueStatement(createTok token.Token) (ast.Statement, error) {
	p.advance() // QUEUE
	ifNotExists, err := p.parseOptIfNotExists()
	if err != nil {
		return nil, err
	}
	name, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	stmt := &ast.CreateQueueStatement{Span: span(createTok.Pos, name.End()), IsIfNotExists: ifNotExists, Name: name}
	if p.peek().Kind != token.LPAREN {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" but got %s`, describeToken(p.peek()))
	}
	if stmt.TableElementList, err = p.parseTableElementList(); err != nil {
		return nil, err
	}
	stmt.Stop = stmt.TableElementList.End()
	if isKeyword(p.peek(), "PRIMARY") && isKeyword(p.peekAt(1), "KEY") {
		if stmt.SpannerOptions, err = p.parseSpannerTableOptions(); err != nil {
			return nil, err
		}
		stmt.Stop = stmt.SpannerOptions.End()
		ttl, opts, err := p.parseSpannerTableTrailers(stmt.SpannerOptions)
		if err != nil {
			return nil, err
		}
		if ttl != nil {
			return nil, p.errorf(ttl.Pos(), "Syntax error: ROW DELETION POLICY is not supported for queues")
		}
		stmt.Options = opts
		stmt.Stop = p.prevEnd()
	}
	if stmt.Options == nil && isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		if stmt.Options, err = p.parseOptionsList(); err != nil {
			return nil, err
		}
		stmt.Stop = stmt.Options.End()
	}
	return stmt, nil
}

// parseSpannerTableTrailers parses the comma-separated clauses Spanner accepts
// after "PRIMARY KEY (...) [, INTERLEAVE IN PARENT ...]", in order: ",
// INTERLEAVE IN path" (no PARENT; attached to opts like INTERLEAVE IN
// PARENT), ", ROW DELETION POLICY (expression)" and ", OPTIONS (...)". The
// TTL clause and options are the nodes the reference produces for the same
// clauses without the comma. Only called under FeatureSpannerDDL.
func (p *parser) parseSpannerTableTrailers(opts *ast.SpannerTableOptions) (*ast.TtlClause, *ast.OptionsList, error) {
	if opts.Interleave == nil && p.peek().Kind == token.COMMA &&
		isKeyword(p.peekAt(1), "INTERLEAVE") && isKeyword(p.peekAt(2), "IN") {
		commaTok := p.advance() // ,
		p.advance()             // INTERLEAVE
		p.advance()             // IN
		path, err := p.parseMaybeDashedPathExpression()
		if err != nil {
			return nil, nil, err
		}
		opts.Interleave = &ast.SpannerInterleaveClause{Span: span(commaTok.Pos, path.End()), TableName: path, Type: "IN"}
		opts.Stop = path.End()
	}
	var ttl *ast.TtlClause
	if p.peek().Kind == token.COMMA && isKeyword(p.peekAt(1), "ROW") && isKeyword(p.peekAt(2), "DELETION") {
		p.advance() // ,
		var err error
		if ttl, err = p.parseTtlClause(); err != nil {
			return nil, nil, err
		}
	}
	var options *ast.OptionsList
	if p.peek().Kind == token.COMMA && isKeyword(p.peekAt(1), "OPTIONS") {
		p.advance() // ,
		p.advance() // OPTIONS
		var err error
		if options, err = p.parseOptionsList(); err != nil {
			return nil, nil, err
		}
	}
	return ttl, options, nil
}

// parseTtlClause parses "ROW DELETION POLICY (expression)"; see ttl_clause in
// googlesql.tm. ROW is the next token. Requires FEATURE_TTL.
func (p *parser) parseTtlClause() (*ast.TtlClause, error) {
	rowTok := p.advance() // ROW
	if _, err := p.expectKeyword("DELETION"); err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("POLICY"); err != nil {
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
	if !p.features.Enabled(FeatureTTL) {
		return nil, p.errorf(rowTok.Pos, "ROW DELETION POLICY clause is not supported.")
	}
	return &ast.TtlClause{Span: span(rowTok.Pos, rparen.End), Expression: expr}, nil
}

// parseTableSynonym parses the Spanner "SYNONYM (name)" table element.
// SYNONYM is the next token.
func (p *parser) parseTableSynonym() (*ast.TableSynonym, error) {
	synTok := p.advance() // SYNONYM
	if _, err := p.expect(token.LPAREN, `"("`); err != nil {
		return nil, err
	}
	name, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	return &ast.TableSynonym{Span: span(synTok.Pos, rparen.End), Name: name}, nil
}

// parseColumnOnUpdate parses a Spanner column's "ON UPDATE expression"
// clause. ON is the next token.
func (p *parser) parseColumnOnUpdate() (*ast.ColumnOnUpdateClause, error) {
	onTok := p.advance() // ON
	if _, err := p.expectKeyword("UPDATE"); err != nil {
		return nil, err
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	return &ast.ColumnOnUpdateClause{Span: span(onTok.Pos, p.extEnd(expr)), Expression: expr}, nil
}

// parseSpannerIdentityOptions parses the body of a Spanner identity column's
// option list after "IDENTITY (": any of a sequence kind (e.g.
// BIT_REVERSED_POSITIVE), "SKIP RANGE min, max", "START COUNTER WITH n", and
// the GoogleSQL options START WITH, INCREMENT BY, MAXVALUE, MINVALUE and
// [NO] CYCLE, in any order. START COUNTER WITH reuses IdentityColumnStartWith.
func (p *parser) parseSpannerIdentityOptions(info *ast.IdentityColumnInfo) error {
	for p.peek().Kind != token.RPAREN {
		tok := p.peek()
		switch {
		case isKeyword(tok, "START") && info.StartWith == nil:
			p.advance() // START
			if isKeyword(p.peek(), "COUNTER") {
				p.advance()
			}
			if _, err := p.expectKeyword("WITH"); err != nil {
				return err
			}
			value, err := p.parseSignedNumericalLiteral()
			if err != nil {
				return err
			}
			info.StartWith = &ast.IdentityColumnStartWith{Span: span(tok.Pos, p.extEnd(value)), Value: value}
		case isKeyword(tok, "SKIP") && info.SkipRange == nil:
			skipRange, err := p.parseSkipRange()
			if err != nil {
				return err
			}
			info.SkipRange = skipRange
		case isKeyword(tok, "INCREMENT") && info.IncrementBy == nil:
			p.advance() // INCREMENT
			if _, err := p.expectKeyword("BY"); err != nil {
				return err
			}
			value, err := p.parseSignedNumericalLiteral()
			if err != nil {
				return err
			}
			info.IncrementBy = &ast.IdentityColumnIncrementBy{Span: span(tok.Pos, p.extEnd(value)), Value: value}
		case isKeyword(tok, "MAXVALUE") && info.MaxValue == nil:
			p.advance() // MAXVALUE
			value, err := p.parseSignedNumericalLiteral()
			if err != nil {
				return err
			}
			info.MaxValue = &ast.IdentityColumnMaxValue{Span: span(tok.Pos, p.extEnd(value)), Value: value}
		case isKeyword(tok, "MINVALUE") && info.MinValue == nil:
			p.advance() // MINVALUE
			value, err := p.parseSignedNumericalLiteral()
			if err != nil {
				return err
			}
			info.MinValue = &ast.IdentityColumnMinValue{Span: span(tok.Pos, p.extEnd(value)), Value: value}
		case isKeyword(tok, "CYCLE"):
			p.advance()
		case isKeyword(tok, "NO") && isKeyword(p.peekAt(1), "CYCLE"):
			p.advance()
			p.advance()
		case info.SequenceKind == nil && tok.Kind == token.IDENT && !p.isReserved(tok):
			info.SequenceKind = p.parseIdentifierToken(p.advance())
		default:
			return p.errorf(tok.Pos, `Syntax error: Expected ")" but got %s`, describeToken(tok))
		}
	}
	return nil
}

// parseSkipRange parses "SKIP RANGE min, max". SKIP is the next token.
func (p *parser) parseSkipRange() (*ast.IdentityColumnSkipRange, error) {
	skipTok := p.advance() // SKIP
	if _, err := p.expectKeyword("RANGE"); err != nil {
		return nil, err
	}
	lo, err := p.parseSignedNumericalLiteral()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(token.COMMA, `","`); err != nil {
		return nil, err
	}
	hi, err := p.parseSignedNumericalLiteral()
	if err != nil {
		return nil, err
	}
	return &ast.IdentityColumnSkipRange{Span: span(skipTok.Pos, p.extEnd(hi)), Min: lo, Max: hi}, nil
}

// parseAlterColumnIdentityAction parses the tail of "ALTER COLUMN c ALTER
// IDENTITY {SET SKIP RANGE min, max | SET NO SKIP RANGE | RESTART COUNTER
// WITH n}". The next token is the second ALTER.
func (p *parser) parseAlterColumnIdentityAction(alterTok token.Token, name *ast.Identifier) (ast.Node, error) {
	p.advance() // ALTER
	p.advance() // IDENTITY
	action := &ast.AlterColumnIdentityAction{Column: name}
	tok := p.peek()
	switch {
	case isKeyword(tok, "SET") && isKeyword(p.peekAt(1), "SKIP"):
		p.advance() // SET
		skipRange, err := p.parseSkipRange()
		if err != nil {
			return nil, err
		}
		action.SkipRange = skipRange
	case isKeyword(tok, "SET") && isKeyword(p.peekAt(1), "NO"):
		p.advance() // SET
		p.advance() // NO
		if _, err := p.expectKeyword("SKIP"); err != nil {
			return nil, err
		}
		if _, err := p.expectKeyword("RANGE"); err != nil {
			return nil, err
		}
		action.NoSkipRange = true
	case isKeyword(tok, "RESTART"):
		p.advance() // RESTART
		if _, err := p.expectKeyword("COUNTER"); err != nil {
			return nil, err
		}
		if _, err := p.expectKeyword("WITH"); err != nil {
			return nil, err
		}
		value, err := p.parseSignedNumericalLiteral()
		if err != nil {
			return nil, err
		}
		action.RestartCounterWith = &ast.IdentityColumnRestartCounterWith{Span: span(tok.Pos, p.extEnd(value)), Value: value}
	default:
		return nil, p.errorf(tok.Pos, "Syntax error: Expected keyword RESTART or keyword SET but got %s", describeToken(tok))
	}
	action.Span = span(alterTok.Pos, p.prevEnd())
	return action, nil
}

// grantObjectToken reports whether tok can begin an object name or object-type
// word in a GRANT/REVOKE statement. Under FeatureSpannerDDL the reserved
// keyword DEFAULT also names an object (Spanner's default schema, as in "ON
// SCHEMA default").
func (p *parser) grantObjectToken(tok token.Token) bool {
	return beginsObjectIdentifier(tok) || (p.spanner() && isKeyword(tok, "DEFAULT"))
}

// parseGrantObjectPath parses an object name in a Spanner GRANT/REVOKE,
// accepting DEFAULT as a one-part name.
func (p *parser) parseGrantObjectPath() (*ast.PathExpression, error) {
	if tok := p.peek(); isKeyword(tok, "DEFAULT") {
		p.advance()
		id := &ast.Identifier{Span: span(tok.Pos, tok.End), Name: tok.Image}
		return &ast.PathExpression{Span: id.Span, Names: []*ast.Identifier{id}}, nil
	}
	return p.parsePathExpression()
}

// startsSpannerRoleList reports whether the next tokens are "ROLE identifier",
// the Spanner grantee or granted-role list of a GRANT/REVOKE.
func (p *parser) startsSpannerRoleList() bool {
	return p.spanner() && isKeyword(p.peek(), "ROLE") && beginsObjectIdentifier(p.peekAt(1))
}

// parseGrantRoleStatement parses "GRANT ROLE role, ... TO ROLE role, ..." or
// "REVOKE ROLE role, ... FROM ROLE role, ...". The next token is ROLE.
func (p *parser) parseGrantRoleStatement(startTok token.Token, isRevoke bool) (ast.Statement, error) {
	p.advance() // ROLE
	roles, err := p.parseIdentifierList()
	if err != nil {
		return nil, err
	}
	term := "TO"
	if isRevoke {
		term = "FROM"
	}
	if _, err := p.expectKeyword(term); err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("ROLE"); err != nil {
		return nil, err
	}
	grantees, err := p.parseIdentifierList()
	if err != nil {
		return nil, err
	}
	return &ast.GrantRoleStatement{Span: span(startTok.Pos, grantees.End()), IsRevoke: isRevoke, Roles: roles, Grantees: grantees}, nil
}

// parseGrantOnAllInSchema parses the Spanner object clause "ALL kind IN SCHEMA
// schema, ..." after ON, where kind is a plural object type such as TABLES.
// ALL is the next token.
func (p *parser) parseGrantOnAllInSchema() (*ast.Identifier, *ast.PathExpressionList, error) {
	p.advance() // ALL
	kind, err := p.parseIdentifier()
	if err != nil {
		return nil, nil, err
	}
	if _, err := p.expectKeyword("IN"); err != nil {
		return nil, nil, err
	}
	if _, err := p.expectKeyword("SCHEMA"); err != nil {
		return nil, nil, err
	}
	objects, err := p.parseGrantObjectList()
	if err != nil {
		return nil, nil, err
	}
	return kind, objects, nil
}

// parseGrantObjectList parses the comma-separated object names of a Spanner
// GRANT/REVOKE.
func (p *parser) parseGrantObjectList() (*ast.PathExpressionList, error) {
	list := &ast.PathExpressionList{}
	for {
		path, err := p.parseGrantObjectPath()
		if err != nil {
			return nil, err
		}
		list.Paths = append(list.Paths, path)
		if p.peek().Kind != token.COMMA {
			break
		}
		p.advance() // ,
	}
	list.Span = span(list.Paths[0].Pos(), list.Paths[len(list.Paths)-1].End())
	return list, nil
}

// parseSpannerIndexSuffix parses the clauses of a Spanner CREATE [SEARCH |
// VECTOR] INDEX after STORING: "[PARTITION BY ...] [ORDER BY ...] [WHERE
// expression] [, INTERLEAVE IN path] [OPTIONS (...)]", also accepting the
// GoogleSQL order with OPTIONS before ", INTERLEAVE IN".
func (p *parser) parseSpannerIndexSuffix(stmt *ast.CreateIndexStatement) (ast.Statement, error) {
	var err error
	if isKeyword(p.peek(), "PARTITION") {
		if stmt.PartitionBy, err = p.parseIndexPartitionBy(); err != nil {
			return nil, err
		}
		stmt.Stop = stmt.PartitionBy.End()
	}
	if isKeyword(p.peek(), "ORDER") {
		if stmt.OrderBy, err = p.parseOrderBy(false); err != nil {
			return nil, err
		}
		stmt.Stop = stmt.OrderBy.End()
	}
	if isKeyword(p.peek(), "WHERE") {
		whereTok := p.advance() // WHERE
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		stmt.Where = &ast.WhereClause{Span: span(whereTok.Pos, p.extEnd(expr)), Expr: expr}
		stmt.Stop = stmt.Where.End()
	}
	parseInterleave := func() error {
		if p.peek().Kind != token.COMMA || !isKeyword(p.peekAt(1), "INTERLEAVE") || !isKeyword(p.peekAt(2), "IN") {
			return nil
		}
		commaTok := p.advance() // ,
		p.advance()             // INTERLEAVE
		p.advance()             // IN
		path, err := p.parseMaybeDashedPathExpression()
		if err != nil {
			return err
		}
		stmt.SpannerInterleave = &ast.SpannerInterleaveClause{Span: span(commaTok.Pos, path.End()), TableName: path, Type: "IN"}
		stmt.Stop = max(stmt.Stop, path.End())
		return nil
	}
	if err := parseInterleave(); err != nil {
		return nil, err
	}
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		if stmt.Options, err = p.parseOptionsList(); err != nil {
			return nil, err
		}
		stmt.Stop = stmt.Options.End()
	}
	if stmt.SpannerInterleave == nil {
		if err := parseInterleave(); err != nil {
			return nil, err
		}
	}
	return stmt, nil
}
