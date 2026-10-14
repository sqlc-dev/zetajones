package parser

// Property graph DDL: CREATE PROPERTY GRAPH. It mirrors
// create_property_graph_statement and the element_table_* / label / properties
// productions in googlesql/parser/googlesql.tm. DROP PROPERTY GRAPH is a
// generic DropStatement (see parseDropStatement).

import (
	"github.com/sqlc-dev/zetajones/ast"
	"github.com/sqlc-dev/zetajones/token"
)

// parseCreatePropertyGraphStatement parses the tail of "CREATE [OR REPLACE]
// [scope] PROPERTY GRAPH [IF NOT EXISTS] name NODE TABLES (...) [EDGE TABLES
// (...)] [OPTIONS(...)]"; see create_property_graph_statement in
// googlesql.tm. The parser is at PROPERTY.
func (p *parser) parseCreatePropertyGraphStatement(createTok token.Token, scope string, isOrReplace bool) (ast.Statement, error) {
	p.advance() // PROPERTY
	if _, err := p.expectKeyword("GRAPH"); err != nil {
		return nil, err
	}
	stmt := &ast.CreatePropertyGraphStatement{Scope: scope, IsOrReplace: isOrReplace}
	ifNotExists, err := p.parseOptIfNotExists()
	if err != nil {
		return nil, err
	}
	stmt.IsIfNotExists = ifNotExists
	if stmt.Name, err = p.parsePathExpression(); err != nil {
		return nil, err
	}
	if !isKeyword(p.peek(), "NODE") {
		return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "." or keyword NODE but got %s`, describeToken(p.peek()))
	}
	p.advance() // NODE
	if _, err := p.expectKeyword("TABLES"); err != nil {
		return nil, err
	}
	if stmt.NodeTables, err = p.parseGraphElementTableList(); err != nil {
		return nil, err
	}
	if isKeyword(p.peek(), "EDGE") {
		p.advance() // EDGE
		if _, err := p.expectKeyword("TABLES"); err != nil {
			return nil, err
		}
		if stmt.EdgeTables, err = p.parseGraphElementTableList(); err != nil {
			return nil, err
		}
	}
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		if stmt.Options, err = p.parseOptionsList(); err != nil {
			return nil, err
		}
	}
	stmt.Span = span(createTok.Pos, p.prevEnd())
	return stmt, nil
}

// parseGraphElementTableList parses "( element_table_definition, ... [,] )";
// see element_table_list in googlesql.tm. The list's location includes the
// parentheses.
func (p *parser) parseGraphElementTableList() (*ast.GraphElementTableList, error) {
	lparen, err := p.expect(token.LPAREN, `"("`)
	if err != nil {
		return nil, err
	}
	list := &ast.GraphElementTableList{}
	for {
		table, err := p.parseGraphElementTable()
		if err != nil {
			return nil, err
		}
		list.Tables = append(list.Tables, table)
		if p.peek().Kind != token.COMMA {
			if p.peek().Kind != token.RPAREN {
				return nil, p.errorf(p.peek().Pos, `Syntax error: Expected ")" or "," but got %s`, describeToken(p.peek()))
			}
			break
		}
		p.advance() // ,
		if p.peek().Kind == token.RPAREN {
			break // trailing comma
		}
	}
	rparen, err := p.expect(token.RPAREN, `")"`)
	if err != nil {
		return nil, err
	}
	list.Span = span(lparen.Pos, rparen.End)
	return list, nil
}

// parseGraphElementTable parses an element_table_definition: "path [AS alias]
// [KEY (...)] [SOURCE KEY ...] [DESTINATION KEY ...] [OPTIONS(...)]
// [labels and properties] [DYNAMIC LABEL (...)] [DYNAMIC PROPERTIES (...)]";
// see element_table_definition in googlesql.tm.
func (p *parser) parseGraphElementTable() (*ast.GraphElementTable, error) {
	name, err := p.parsePathExpression()
	if err != nil {
		return nil, err
	}
	table := &ast.GraphElementTable{Name: name}
	if isKeyword(p.peek(), "AS") {
		if table.Alias, err = p.parseRequiredAsAlias(); err != nil {
			return nil, err
		}
	}
	if isKeyword(p.peek(), "KEY") {
		p.advance() // KEY
		if table.Key, err = p.parseColumnList(); err != nil {
			return nil, err
		}
	}
	if isKeyword(p.peek(), "SOURCE") {
		if table.Source, err = p.parseGraphNodeTableReference("SOURCE"); err != nil {
			return nil, err
		}
	}
	if isKeyword(p.peek(), "DESTINATION") {
		if table.Destination, err = p.parseGraphNodeTableReference("DESTINATION"); err != nil {
			return nil, err
		}
	}
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		if table.Options, err = p.parseOptionsList(); err != nil {
			return nil, err
		}
	}
	if table.LabelProperties, err = p.parseGraphLabelAndPropertiesList(); err != nil {
		return nil, err
	}
	if err := p.parseGraphDynamicLabelAndProperties(table); err != nil {
		return nil, err
	}
	table.Span = span(name.Pos(), p.prevEnd())
	return table, nil
}

// parseGraphNodeTableReference parses "{SOURCE|DESTINATION} KEY (...)
// REFERENCES node [(...)]"; see source_node_table_clause and
// dest_node_table_clause in googlesql.tm. refType is the leading keyword.
func (p *parser) parseGraphNodeTableReference(refType string) (*ast.GraphNodeTableReference, error) {
	startTok := p.advance() // SOURCE or DESTINATION
	if _, err := p.expectKeyword("KEY"); err != nil {
		return nil, err
	}
	ref := &ast.GraphNodeTableReference{Type: refType}
	var err error
	if ref.EdgeColumns, err = p.parseColumnList(); err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("REFERENCES"); err != nil {
		return nil, err
	}
	if ref.NodeTable, err = p.parseIdentifier(); err != nil {
		return nil, err
	}
	if p.peek().Kind == token.LPAREN {
		if ref.NodeColumns, err = p.parseColumnList(); err != nil {
			return nil, err
		}
	}
	ref.Span = span(startTok.Pos, p.prevEnd())
	return ref, nil
}

// parseGraphLabelAndPropertiesList parses opt_label_and_properties_clause. A
// bare properties clause or an empty clause is an implicit DEFAULT LABEL; an
// empty clause additionally means PROPERTIES ARE ALL COLUMNS, located at the
// end of the preceding token.
func (p *parser) parseGraphLabelAndPropertiesList() (*ast.GraphElementLabelAndPropertiesList, error) {
	implicit := func(props *ast.GraphProperties) *ast.GraphElementLabelAndPropertiesList {
		item := &ast.GraphElementLabelAndProperties{Span: props.Span, Properties: props}
		return &ast.GraphElementLabelAndPropertiesList{Span: props.Span, Items: []*ast.GraphElementLabelAndProperties{item}}
	}
	switch {
	case isKeyword(p.peek(), "PROPERTIES") || isKeyword(p.peek(), "NO"):
		props, err := p.parseGraphProperties()
		if err != nil {
			return nil, err
		}
		return implicit(props), nil
	case !isKeyword(p.peek(), "LABEL") && !isKeyword(p.peek(), "DEFAULT"):
		end := p.prevEnd()
		return implicit(&ast.GraphProperties{Span: span(end, end)}), nil
	}
	list := &ast.GraphElementLabelAndPropertiesList{}
	for isKeyword(p.peek(), "LABEL") || isKeyword(p.peek(), "DEFAULT") {
		startTok := p.advance() // DEFAULT or LABEL
		item := &ast.GraphElementLabelAndProperties{}
		var err error
		if isKeyword(startTok, "DEFAULT") {
			if _, err := p.expectKeyword("LABEL"); err != nil {
				return nil, err
			}
			if isKeyword(p.peek(), "OPTIONS") {
				p.advance() // OPTIONS
				if item.Options, err = p.parseOptionsList(); err != nil {
					return nil, err
				}
			}
		} else if item.Label, err = p.parseIdentifier(); err != nil {
			return nil, err
		}
		if isKeyword(p.peek(), "PROPERTIES") || isKeyword(p.peek(), "NO") {
			if item.Properties, err = p.parseGraphProperties(); err != nil {
				return nil, err
			}
		} else {
			end := p.prevEnd()
			item.Properties = &ast.GraphProperties{Span: span(end, end)}
		}
		item.Span = span(startTok.Pos, p.prevEnd())
		list.Items = append(list.Items, item)
	}
	list.Span = span(list.Items[0].Pos(), p.prevEnd())
	return list, nil
}

// atGraphLabel reports whether a "LABEL" or "DEFAULT LABEL" keyword sequence
// (static_label_or_properties_keywords) starts at the next token.
func (p *parser) atGraphLabel() bool {
	return isKeyword(p.peek(), "LABEL") ||
		(isKeyword(p.peek(), "DEFAULT") && isKeyword(p.peekAt(1), "LABEL"))
}

// atGraphProperties reports whether a "PROPERTIES" or "NO PROPERTIES"
// keyword sequence (static_label_or_properties_keywords) starts at the next
// token.
func (p *parser) atGraphProperties() bool {
	return isKeyword(p.peek(), "PROPERTIES") ||
		(isKeyword(p.peek(), "NO") && isKeyword(p.peekAt(1), "PROPERTIES"))
}

// parseGraphProperties parses a properties_clause: "NO PROPERTIES",
// "PROPERTIES [ARE] ALL COLUMNS [EXCEPT (...)]" or "PROPERTIES (derived, ...)";
// see properties_clause in googlesql.tm.
func (p *parser) parseGraphProperties() (*ast.GraphProperties, error) {
	startTok := p.advance() // NO or PROPERTIES
	props := &ast.GraphProperties{}
	switch {
	case isKeyword(startTok, "NO"):
		if _, err := p.expectKeyword("PROPERTIES"); err != nil {
			return nil, err
		}
		props.NoProperties = true
	case p.peek().Kind == token.LPAREN:
		p.advance() // (
		list := &ast.GraphDerivedPropertyList{}
		for {
			prop, err := p.parseGraphDerivedProperty()
			if err != nil {
				return nil, err
			}
			list.Properties = append(list.Properties, prop)
			if p.peek().Kind != token.COMMA {
				break
			}
			p.advance() // ,
		}
		list.Span = span(list.Properties[0].Pos(), p.prevEnd())
		if _, err := p.expect(token.RPAREN, `")"`); err != nil {
			return nil, err
		}
		props.DerivedProperties = list
	default:
		if isKeyword(p.peek(), "ARE") {
			p.advance() // ARE
			if _, err := p.expectKeyword("ALL"); err != nil {
				return nil, err
			}
		} else if !isKeyword(p.peek(), "ALL") {
			return nil, p.errorf(p.peek().Pos, `Syntax error: Expected "(" or keyword ALL or keyword ARE but got %s`, describeToken(p.peek()))
		} else {
			p.advance() // ALL
		}
		if _, err := p.expectKeyword("COLUMNS"); err != nil {
			return nil, err
		}
		if isKeyword(p.peek(), "EXCEPT") {
			// The lexer reports EXCEPT not followed by ALL, DISTINCT or "("
			// before the grammar sees it, and lexes EXCEPT ALL/DISTINCT as a
			// set operation, which ends the element table.
			if err := p.exceptClashError(); err != nil {
				return nil, err
			}
		}
		if isKeyword(p.peek(), "EXCEPT") && p.peekAt(1).Kind == token.LPAREN {
			p.advance() // EXCEPT
			cols, err := p.parseColumnList()
			if err != nil {
				return nil, err
			}
			props.AllExceptColumns = cols
		}
	}
	props.Span = span(startTok.Pos, p.prevEnd())
	return props, nil
}

// parseGraphDerivedProperty parses "expression [AS alias] [OPTIONS(...)]";
// see derived_property in googlesql.tm.
func (p *parser) parseGraphDerivedProperty() (*ast.GraphDerivedProperty, error) {
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	prop := &ast.GraphDerivedProperty{Expression: expr}
	if isKeyword(p.peek(), "AS") {
		if prop.Alias, err = p.parseRequiredAsAlias(); err != nil {
			return nil, err
		}
	}
	if isKeyword(p.peek(), "OPTIONS") {
		p.advance() // OPTIONS
		if prop.Options, err = p.parseOptionsList(); err != nil {
			return nil, err
		}
	}
	prop.Span = span(p.extStart(expr), p.prevEnd())
	return prop, nil
}

// parseGraphDynamicLabelAndProperties parses the optional "DYNAMIC LABEL
// (expr)" and "DYNAMIC PROPERTIES (expr)" clauses, in either order, each at
// most once; see dynamic_label_and_properties in googlesql.tm.
func (p *parser) parseGraphDynamicLabelAndProperties(table *ast.GraphElementTable) error {
	sawDynamic := false
	for isKeyword(p.peek(), "DYNAMIC") {
		sawDynamic = true
		dynamicTok := p.advance() // DYNAMIC
		kindTok := p.peek()
		isLabel := isKeyword(kindTok, "LABEL")
		if !isLabel && !isKeyword(kindTok, "PROPERTIES") {
			return p.errorf(kindTok.Pos, "Syntax error: Expected keyword LABEL or keyword PROPERTIES but got %s", describeToken(kindTok))
		}
		p.advance() // LABEL or PROPERTIES
		if _, err := p.expect(token.LPAREN, `"("`); err != nil {
			return err
		}
		expr, err := p.parseExpression()
		if err != nil {
			return err
		}
		if _, err := p.expect(token.RPAREN, `")"`); err != nil {
			return err
		}
		exprSpan := span(p.extStart(expr), p.extEnd(expr))
		if isLabel {
			if table.DynamicLabel != nil {
				return p.errorf(dynamicTok.Pos, "DYNAMIC LABEL cannot be used more than once in a single element table")
			}
			table.DynamicLabel = &ast.GraphDynamicLabel{Span: exprSpan, Expression: expr}
		} else {
			if table.DynamicProperties != nil {
				return p.errorf(dynamicTok.Pos, "DYNAMIC PROPERTIES cannot be used more than once in a single element table")
			}
			table.DynamicProperties = &ast.GraphDynamicProperties{Span: exprSpan, Expression: expr}
		}
	}
	if !sawDynamic {
		return nil
	}
	// A static clause after a dynamic one; NO and DEFAULT commit to their
	// two-keyword forms.
	tok := p.peek()
	switch {
	case isKeyword(tok, "NO") && !isKeyword(p.peekAt(1), "PROPERTIES"):
		p.advance()
		return p.errorf(p.peek().Pos, "Syntax error: Expected keyword PROPERTIES but got %s", describeToken(p.peek()))
	case isKeyword(tok, "DEFAULT") && !isKeyword(p.peekAt(1), "LABEL"):
		p.advance()
		return p.errorf(p.peek().Pos, "Syntax error: Expected keyword LABEL but got %s", describeToken(p.peek()))
	case p.atGraphLabel() || p.atGraphProperties():
		return p.errorf(tok.Pos, "Syntax error: LABEL or PROPERTIES clause should be moved before DYNAMIC LABEL/PROPERTIES clause")
	}
	return nil
}
