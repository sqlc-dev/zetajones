// Package ast defines the GoogleSQL abstract syntax tree.
//
// The node structure mirrors the parse tree defined in
// github.com/google/googlesql (googlesql/parser/parse_tree.h and ast_node.h)
// so that trees can be rendered byte-for-byte identical to ZetaSQL's
// ASTNode::DebugString output for verification against the upstream parser
// test suite.
package ast

import "reflect"

// Node is implemented by every AST node.
type Node interface {
	// Pos returns the byte offset of the first byte of the node in the input.
	Pos() int
	// End returns the byte offset one past the last byte of the node.
	End() int
	// Children returns the node's children in source order. Nil children are
	// not included.
	Children() []Node
}

// Span records the source location of a node. It is embedded in every node.
type Span struct {
	Start int `json:"start"`
	Stop  int `json:"end"`
}

func (s Span) Pos() int { return s.Start }
func (s Span) End() int { return s.Stop }

func children(nodes ...Node) []Node {
	var out []Node
	for _, n := range nodes {
		if n == nil {
			continue
		}
		// A nil typed pointer (e.g. a nil *Alias field) still produces a
		// non-nil Node interface; filter those out too.
		if v := reflect.ValueOf(n); v.Kind() == reflect.Pointer && v.IsNil() {
			continue
		}
		out = append(out, n)
	}
	return out
}

// Statement is implemented by all statement nodes.
type Statement interface {
	Node
	statementNode()
}

// QueryStatement is a query used as a statement.
type QueryStatement struct {
	Span
	Query *Query `json:"query"`
}

func (n *QueryStatement) statementNode() {}
func (n *QueryStatement) Children() []Node {
	return children(n.Query)
}

// CallStatement is a "CALL procedure(args)" statement; see ASTCallStatement
// in googlesql/parser/parse_tree.h.
type CallStatement struct {
	Span
	Procedure *PathExpression `json:"procedure"`
	Args      []*TVFArgument  `json:"args,omitempty"`
}

func (n *CallStatement) statementNode() {}
func (n *CallStatement) Children() []Node {
	out := children(n.Procedure)
	for _, a := range n.Args {
		out = append(out, a)
	}
	return out
}

// ImportStatement is "IMPORT MODULE|PROTO path_or_string [AS/INTO alias]
// [OPTIONS(...)]"; see ASTImportStatement in googlesql/parser/parse_tree.h.
type ImportStatement struct {
	Span
	// Name is the imported path (PathExpression) or string literal.
	Name    Node         `json:"name"`
	Alias   *Alias       `json:"alias,omitempty"`
	Into    *IntoAlias   `json:"into,omitempty"`
	Options *OptionsList `json:"options,omitempty"`
}

func (n *ImportStatement) statementNode() {}
func (n *ImportStatement) Children() []Node {
	out := children(n.Name)
	if n.Alias != nil {
		out = append(out, n.Alias)
	}
	if n.Into != nil {
		out = append(out, n.Into)
	}
	if n.Options != nil {
		out = append(out, n.Options)
	}
	return out
}

// RenameStatement is "RENAME <identifier> <old_path> TO <new_path>"; see
// ASTRenameStatement in googlesql/parser/parse_tree.h.
type RenameStatement struct {
	Span
	// Identifier names the object type being renamed (e.g. table).
	Identifier *Identifier     `json:"identifier"`
	OldName    *PathExpression `json:"old_name"`
	NewName    *PathExpression `json:"new_name"`
}

func (n *RenameStatement) statementNode() {}
func (n *RenameStatement) Children() []Node {
	return children(n.Identifier, n.OldName, n.NewName)
}

// SingleAssignment is "SET identifier = expression"; see ASTSingleAssignment
// in googlesql/parser/parse_tree.h.
type SingleAssignment struct {
	Span
	Variable *Identifier `json:"variable"`
	Value    Node        `json:"value"`
}

func (n *SingleAssignment) statementNode() {}
func (n *SingleAssignment) Children() []Node {
	return children(n.Variable, n.Value)
}

// ParameterAssignment is "SET @parameter = expression"; see
// ASTParameterAssignment in googlesql/parser/parse_tree.h.
type ParameterAssignment struct {
	Span
	Parameter *ParameterExpr `json:"parameter"`
	Value     Node           `json:"value"`
}

func (n *ParameterAssignment) statementNode() {}
func (n *ParameterAssignment) Children() []Node {
	return children(n.Parameter, n.Value)
}

// SystemVariableAssignment is "SET @@system_variable = expression"; see
// ASTSystemVariableAssignment in googlesql/parser/parse_tree.h.
type SystemVariableAssignment struct {
	Span
	SystemVariable *SystemVariableExpr `json:"system_variable"`
	Value          Node                `json:"value"`
}

func (n *SystemVariableAssignment) statementNode() {}
func (n *SystemVariableAssignment) Children() []Node {
	return children(n.SystemVariable, n.Value)
}

// AssignmentFromStruct is "SET (a, b, ...) = expression"; see
// ASTAssignmentFromStruct in googlesql/parser/parse_tree.h.
type AssignmentFromStruct struct {
	Span
	Variables *IdentifierList `json:"variables"`
	Value     Node            `json:"value"`
}

func (n *AssignmentFromStruct) statementNode() {}
func (n *AssignmentFromStruct) Children() []Node {
	return children(n.Variables, n.Value)
}

// BeginStatement is a "BEGIN [TRANSACTION]" or "START TRANSACTION" statement
// with an optional transaction mode list; see ASTBeginStatement in
// googlesql/parser/parse_tree.h.
type BeginStatement struct {
	Span
	ModeList *TransactionModeList `json:"mode_list,omitempty"`
}

func (n *BeginStatement) statementNode()   {}
func (n *BeginStatement) Children() []Node { return children(n.ModeList) }

// SetTransactionStatement is a "SET TRANSACTION mode[, ...]" statement; see
// ASTSetTransactionStatement in googlesql/parser/parse_tree.h.
type SetTransactionStatement struct {
	Span
	ModeList *TransactionModeList `json:"mode_list"`
}

func (n *SetTransactionStatement) statementNode()   {}
func (n *SetTransactionStatement) Children() []Node { return children(n.ModeList) }

// TransactionModeList is the list of transaction modes on a BEGIN/START/SET
// TRANSACTION statement; see ASTTransactionModeList in
// googlesql/parser/parse_tree.h.
type TransactionModeList struct {
	Span
	Modes []Node `json:"modes"`
}

func (n *TransactionModeList) Children() []Node { return append([]Node(nil), n.Modes...) }

// TransactionReadWriteMode is a "READ ONLY" or "READ WRITE" transaction mode;
// see ASTTransactionReadWriteMode in googlesql/parser/parse_tree.h.
type TransactionReadWriteMode struct {
	Span
	// Mode is "READ_ONLY" or "READ_WRITE".
	Mode string `json:"mode"`
}

func (n *TransactionReadWriteMode) Children() []Node { return nil }

// TransactionIsolationLevel is an "ISOLATION LEVEL identifier [identifier]"
// transaction mode; see ASTTransactionIsolationLevel in
// googlesql/parser/parse_tree.h.
type TransactionIsolationLevel struct {
	Span
	Identifier1 *Identifier `json:"identifier1,omitempty"`
	Identifier2 *Identifier `json:"identifier2,omitempty"`
}

func (n *TransactionIsolationLevel) Children() []Node {
	return children(n.Identifier1, n.Identifier2)
}

// CommitStatement is a "COMMIT [TRANSACTION]" statement; see ASTCommitStatement
// in googlesql/parser/parse_tree.h.
type CommitStatement struct {
	Span
}

func (n *CommitStatement) statementNode()   {}
func (n *CommitStatement) Children() []Node { return nil }

// RollbackStatement is a "ROLLBACK [TRANSACTION]" statement; see
// ASTRollbackStatement in googlesql/parser/parse_tree.h.
type RollbackStatement struct {
	Span
}

func (n *RollbackStatement) statementNode()   {}
func (n *RollbackStatement) Children() []Node { return nil }

// StartBatchStatement is a "START BATCH [batch_type]" statement; see
// ASTStartBatchStatement in googlesql/parser/parse_tree.h.
type StartBatchStatement struct {
	Span
	BatchType *Identifier `json:"batch_type,omitempty"`
}

func (n *StartBatchStatement) statementNode()   {}
func (n *StartBatchStatement) Children() []Node { return children(n.BatchType) }

// RunBatchStatement is a "RUN BATCH" statement; see ASTRunBatchStatement in
// googlesql/parser/parse_tree.h.
type RunBatchStatement struct {
	Span
}

func (n *RunBatchStatement) statementNode()   {}
func (n *RunBatchStatement) Children() []Node { return nil }

// AbortBatchStatement is an "ABORT BATCH" statement; see ASTAbortBatchStatement
// in googlesql/parser/parse_tree.h.
type AbortBatchStatement struct {
	Span
}

func (n *AbortBatchStatement) statementNode()   {}
func (n *AbortBatchStatement) Children() []Node { return nil }

// Script is the top-level or procedure-body wrapper around a StatementList;
// see ASTScript in googlesql/parser/parse_tree.h.
type Script struct {
	Span
	Statements *StatementList `json:"statements"`
}

func (n *Script) statementNode()   {}
func (n *Script) Children() []Node { return children(n.Statements) }

// StatementList is a sequence of statements, e.g. a script body or the body of
// a BEGIN/END block; see ASTStatementList in googlesql/parser/parse_tree.h.
type StatementList struct {
	Span
	Statements []Node `json:"statements,omitempty"`
}

func (n *StatementList) Children() []Node { return append([]Node(nil), n.Statements...) }

// BeginEndBlock is a "BEGIN statement_list [EXCEPTION ...] END" block; see
// ASTBeginEndBlock in googlesql/parser/parse_tree.h.
type BeginEndBlock struct {
	Span
	Statements *StatementList        `json:"statements"`
	Handlers   *ExceptionHandlerList `json:"handlers,omitempty"`
}

func (n *BeginEndBlock) statementNode() {}
func (n *BeginEndBlock) Children() []Node {
	out := children(n.Statements)
	if n.Handlers != nil {
		out = append(out, n.Handlers)
	}
	return out
}

// ExceptionHandler is a single "WHEN ERROR THEN statement_list" handler; see
// ASTExceptionHandler in googlesql/parser/parse_tree.h.
type ExceptionHandler struct {
	Span
	Body *StatementList `json:"body"`
}

func (n *ExceptionHandler) Children() []Node { return children(n.Body) }

// ExceptionHandlerList is the list of exception handlers on a BEGIN/END block;
// see ASTExceptionHandlerList in googlesql/parser/parse_tree.h. The grammar
// currently allows exactly one handler.
type ExceptionHandlerList struct {
	Span
	Handlers []*ExceptionHandler `json:"handlers"`
}

func (n *ExceptionHandlerList) Children() []Node {
	out := make([]Node, 0, len(n.Handlers))
	for _, h := range n.Handlers {
		out = append(out, h)
	}
	return out
}

// RaiseStatement is a "RAISE [USING MESSAGE = expr]" script statement; see
// ASTRaiseStatement in googlesql/parser/parse_tree.h. Message is nil for a
// bare rethrow.
type RaiseStatement struct {
	Span
	Message Node `json:"message,omitempty"`
}

func (n *RaiseStatement) statementNode()   {}
func (n *RaiseStatement) Children() []Node { return children(n.Message) }

// BreakStatement is a "BREAK" or "LEAVE" script statement; both parse to
// ASTBreakStatement (Keyword records which spelling was used). See
// ASTBreakStatement in googlesql/parser/parse_tree.h.
type BreakStatement struct {
	Span
	Keyword string `json:"keyword"`
}

func (n *BreakStatement) statementNode()   {}
func (n *BreakStatement) Children() []Node { return nil }

// ContinueStatement is a "CONTINUE" or "ITERATE" script statement; both parse
// to ASTContinueStatement (Keyword records which spelling was used). See
// ASTContinueStatement in googlesql/parser/parse_tree.h.
type ContinueStatement struct {
	Span
	Keyword string `json:"keyword"`
}

func (n *ContinueStatement) statementNode()   {}
func (n *ContinueStatement) Children() []Node { return nil }

// MacroBody is the body of a DEFINE MACRO statement: the raw source text
// (including whitespace and comments) from the first body token to the last,
// stored verbatim. See ASTMacroBody in googlesql/parser/parse_tree.h; it is an
// ASTPrintableLeaf whose debug string is "MacroBody(<image>)".
type MacroBody struct {
	Span
	Image string `json:"image"`
}

func (n *MacroBody) Children() []Node { return nil }

// DefineMacroStatement is "DEFINE MACRO <name> <body>"; see
// ASTDefineMacroStatement in googlesql/parser/parse_tree.h.
type DefineMacroStatement struct {
	Span
	Name *Identifier `json:"name"`
	Body *MacroBody  `json:"body"`
}

func (n *DefineMacroStatement) statementNode() {}
func (n *DefineMacroStatement) Children() []Node {
	return []Node{n.Name, n.Body}
}

// VariableDeclaration is "DECLARE identifier_list [type] [DEFAULT expr]"; see
// ASTVariableDeclaration in googlesql/parser/parse_tree.h.
type VariableDeclaration struct {
	Span
	Variables    *IdentifierList `json:"variables"`
	Type         Node            `json:"type,omitempty"`
	DefaultValue Node            `json:"default_value,omitempty"`
}

func (n *VariableDeclaration) statementNode() {}
func (n *VariableDeclaration) Children() []Node {
	return children(n.Variables, n.Type, n.DefaultValue)
}

// IfStatement is "IF expr THEN stmts [ELSEIF ...] [ELSE stmts] END IF"; see
// ASTIfStatement in googlesql/parser/parse_tree.h.
type IfStatement struct {
	Span
	Condition     Node              `json:"condition"`
	ThenList      *StatementList    `json:"then_list"`
	ElseifClauses *ElseifClauseList `json:"elseif_clauses,omitempty"`
	ElseList      *StatementList    `json:"else_list,omitempty"`
}

func (n *IfStatement) statementNode() {}
func (n *IfStatement) Children() []Node {
	return children(n.Condition, n.ThenList, n.ElseifClauses, n.ElseList)
}

// ElseifClause is a single "ELSEIF expr THEN stmts" clause; see ASTElseifClause
// in googlesql/parser/parse_tree.h.
type ElseifClause struct {
	Span
	Condition Node           `json:"condition"`
	Body      *StatementList `json:"body"`
}

func (n *ElseifClause) Children() []Node { return children(n.Condition, n.Body) }

// ElseifClauseList is the list of ELSEIF clauses in an IF statement; see
// ASTElseifClauseList in googlesql/parser/parse_tree.h.
type ElseifClauseList struct {
	Span
	Clauses []*ElseifClause `json:"clauses"`
}

func (n *ElseifClauseList) Children() []Node {
	out := make([]Node, 0, len(n.Clauses))
	for _, c := range n.Clauses {
		out = append(out, c)
	}
	return out
}

// ReturnStatement is the "RETURN" script statement; see ASTReturnStatement in
// googlesql/parser/parse_tree.h. It has no children.
type ReturnStatement struct {
	Span
}

func (n *ReturnStatement) statementNode()   {}
func (n *ReturnStatement) Children() []Node { return nil }

// WhileStatement is a "LOOP statement_list END LOOP" or "WHILE expression DO
// statement_list END WHILE" loop; both parse to ASTWhileStatement (Condition is
// nil for the LOOP form). See ASTWhileStatement in
// googlesql/parser/parse_tree.h.
type WhileStatement struct {
	Span
	Condition Node           `json:"condition,omitempty"`
	Body      *StatementList `json:"body"`
}

func (n *WhileStatement) statementNode()   {}
func (n *WhileStatement) Children() []Node { return children(n.Condition, n.Body) }

// UntilClause is the "UNTIL expression" clause of a REPEAT statement; see
// ASTUntilClause in googlesql/parser/parse_tree.h.
type UntilClause struct {
	Span
	Condition Node `json:"condition"`
}

func (n *UntilClause) Children() []Node { return children(n.Condition) }

// RepeatStatement is "REPEAT statement_list until_clause END REPEAT"; see
// ASTRepeatStatement in googlesql/parser/parse_tree.h.
type RepeatStatement struct {
	Span
	Body  *StatementList `json:"body"`
	Until *UntilClause   `json:"until"`
}

func (n *RepeatStatement) statementNode()   {}
func (n *RepeatStatement) Children() []Node { return children(n.Body, n.Until) }

// ForInStatement is "FOR identifier IN ( query ) DO statement_list END FOR";
// see ASTForInStatement in googlesql/parser/parse_tree.h.
type ForInStatement struct {
	Span
	Variable *Identifier    `json:"variable"`
	Query    *Query         `json:"query"`
	Body     *StatementList `json:"body"`
}

func (n *ForInStatement) statementNode()   {}
func (n *ForInStatement) Children() []Node { return children(n.Variable, n.Query, n.Body) }

// ModuleStatement is "MODULE path_expression [OPTIONS(...)]"; see
// ASTModuleStatement in googlesql/parser/parse_tree.h.
type ModuleStatement struct {
	Span
	Name    *PathExpression `json:"name"`
	Options *OptionsList    `json:"options,omitempty"`
}

func (n *ModuleStatement) statementNode() {}
func (n *ModuleStatement) Children() []Node {
	out := children(n.Name)
	if n.Options != nil {
		out = append(out, n.Options)
	}
	return out
}

// ExecuteImmediateStatement is "EXECUTE IMMEDIATE <expr> [INTO ...] [USING ...]";
// see ASTExecuteImmediateStatement in googlesql/parser/parse_tree.h.
type ExecuteImmediateStatement struct {
	Span
	Sql   Node                `json:"sql"`
	Into  *ExecuteIntoClause  `json:"into,omitempty"`
	Using *ExecuteUsingClause `json:"using,omitempty"`
}

func (n *ExecuteImmediateStatement) statementNode() {}
func (n *ExecuteImmediateStatement) Children() []Node {
	out := children(n.Sql)
	if n.Into != nil {
		out = append(out, n.Into)
	}
	if n.Using != nil {
		out = append(out, n.Using)
	}
	return out
}

// ExecuteIntoClause is "INTO <identifier_list>"; see ASTExecuteIntoClause in
// googlesql/parser/parse_tree.h.
type ExecuteIntoClause struct {
	Span
	Identifiers *IdentifierList `json:"identifiers"`
}

func (n *ExecuteIntoClause) Children() []Node { return children(n.Identifiers) }

// IdentifierList is a comma-separated list of identifiers; see
// ASTIdentifierList in googlesql/parser/parse_tree.h.
type IdentifierList struct {
	Span
	Identifiers []*Identifier `json:"identifiers"`
}

func (n *IdentifierList) Children() []Node {
	out := make([]Node, 0, len(n.Identifiers))
	for _, id := range n.Identifiers {
		out = append(out, id)
	}
	return out
}

// ExecuteUsingClause is "USING <argument_list>"; see ASTExecuteUsingClause in
// googlesql/parser/parse_tree.h.
type ExecuteUsingClause struct {
	Span
	Arguments []*ExecuteUsingArgument `json:"arguments"`
}

func (n *ExecuteUsingClause) Children() []Node {
	out := make([]Node, 0, len(n.Arguments))
	for _, a := range n.Arguments {
		out = append(out, a)
	}
	return out
}

// ExecuteUsingArgument is a single USING argument, optionally aliased with AS;
// see ASTExecuteUsingArgument in googlesql/parser/parse_tree.h.
type ExecuteUsingArgument struct {
	Span
	Expr  Node   `json:"expr"`
	Alias *Alias `json:"alias,omitempty"`
}

func (n *ExecuteUsingArgument) Children() []Node {
	out := children(n.Expr)
	if n.Alias != nil {
		out = append(out, n.Alias)
	}
	return out
}

// HintedStatement wraps a statement preceded by a "@{...}" hint; see
// ASTHintedStatement in googlesql/parser/parse_tree.h.
type HintedStatement struct {
	Span
	Hint      *Hint     `json:"hint"`
	Statement Statement `json:"statement"`
}

func (n *HintedStatement) statementNode() {}
func (n *HintedStatement) Children() []Node {
	return children(n.Hint, n.Statement)
}

// ExplainStatement is "EXPLAIN <statement>"; see ASTExplainStatement in
// googlesql/parser/parse_tree.h and explain_statement in googlesql.tm.
type ExplainStatement struct {
	Span
	Statement Statement `json:"statement"`
}

func (n *ExplainStatement) statementNode() {}
func (n *ExplainStatement) Children() []Node {
	return children(n.Statement)
}

// DeleteStatement is a DELETE statement; see ASTDeleteStatement in
// googlesql/parser/parse_tree.h.
type DeleteStatement struct {
	Span
	Target             Node                `json:"target"` // *PathExpression, *DotIdentifier, *ArrayElement, ...
	Alias              *Alias              `json:"alias,omitempty"`
	Offset             *WithOffset         `json:"offset,omitempty"`
	Where              Node                `json:"where,omitempty"`
	AssertRowsModified *AssertRowsModified `json:"assert_rows_modified,omitempty"`
	Returning          *ReturningClause    `json:"returning,omitempty"`
}

func (n *DeleteStatement) statementNode() {}
func (n *DeleteStatement) Children() []Node {
	return children(n.Target, n.Alias, n.Offset, n.Where, n.AssertRowsModified, n.Returning)
}

// InsertStatement is an INSERT statement; see ASTInsertStatement in
// googlesql/parser/parse_tree.h. InsertMode is "", "IGNORE", "REPLACE", or
// "UPDATE".
type InsertStatement struct {
	Span
	InsertMode         string               `json:"insert_mode,omitempty"`
	Target             Node                 `json:"target"`
	Hint               *Hint                `json:"hint,omitempty"`
	Columns            *ColumnList          `json:"columns,omitempty"`
	Rows               *InsertValuesRowList `json:"rows,omitempty"`
	Query              *Query               `json:"query,omitempty"`
	OnConflict         *OnConflictClause    `json:"on_conflict,omitempty"`
	AssertRowsModified *AssertRowsModified  `json:"assert_rows_modified,omitempty"`
	Returning          *ReturningClause     `json:"returning,omitempty"`
}

func (n *InsertStatement) statementNode() {}
func (n *InsertStatement) Children() []Node {
	out := children(n.Target)
	if n.Hint != nil {
		out = append(out, n.Hint)
	}
	if n.Columns != nil {
		out = append(out, n.Columns)
	}
	if n.Rows != nil {
		out = append(out, n.Rows)
	}
	if n.Query != nil {
		out = append(out, n.Query)
	}
	if n.OnConflict != nil {
		out = append(out, n.OnConflict)
	}
	return append(out, children(n.AssertRowsModified, n.Returning)...)
}

// OnConflictClause is the "ON CONFLICT ... DO ..." clause of an INSERT; see
// ASTOnConflictClause in googlesql/parser/parse_tree.h.
type OnConflictClause struct {
	Span
	// ConflictAction is "NOTHING" or "UPDATE".
	ConflictAction string `json:"conflict_action"`
	// ConflictTarget is an optional conflict target: either a *ColumnList (a
	// column list) or an *Identifier (a unique constraint name).
	ConflictTarget Node            `json:"conflict_target,omitempty"`
	UpdateItemList *UpdateItemList `json:"update_item_list,omitempty"`
	UpdateWhere    Node            `json:"update_where,omitempty"`
}

func (n *OnConflictClause) Children() []Node {
	out := children(n.ConflictTarget)
	if n.UpdateItemList != nil {
		out = append(out, n.UpdateItemList)
	}
	if n.UpdateWhere != nil {
		out = append(out, n.UpdateWhere)
	}
	return out
}

// InsertValuesRowList is the VALUES list of an INSERT statement; see
// ASTInsertValuesRowList in googlesql/parser/parse_tree.h.
type InsertValuesRowList struct {
	Span
	Rows []*InsertValuesRow `json:"rows"`
}

func (n *InsertValuesRowList) Children() []Node {
	var out []Node
	for _, r := range n.Rows {
		out = append(out, r)
	}
	return out
}

// InsertValuesRow is a single "( expr, ... )" row in a VALUES list; see
// ASTInsertValuesRow in googlesql/parser/parse_tree.h.
type InsertValuesRow struct {
	Span
	Values []Node `json:"values"`
}

func (n *InsertValuesRow) Children() []Node {
	return children(n.Values...)
}

// UpdateStatement is an UPDATE statement; see ASTUpdateStatement in
// googlesql/parser/parse_tree.h.
type UpdateStatement struct {
	Span
	Target             Node                `json:"target"`
	Alias              *Alias              `json:"alias,omitempty"`
	Offset             *WithOffset         `json:"offset,omitempty"`
	UpdateItemList     *UpdateItemList     `json:"update_item_list"`
	From               *FromClause         `json:"from,omitempty"`
	Where              Node                `json:"where,omitempty"`
	AssertRowsModified *AssertRowsModified `json:"assert_rows_modified,omitempty"`
	Returning          *ReturningClause    `json:"returning,omitempty"`
}

func (n *UpdateStatement) statementNode() {}
func (n *UpdateStatement) Children() []Node {
	return children(n.Target, n.Alias, n.Offset, n.UpdateItemList, n.From, n.Where, n.AssertRowsModified, n.Returning)
}

// UpdateItemList is the SET item list of an UPDATE statement; see
// ASTUpdateItemList in googlesql/parser/parse_tree.h.
type UpdateItemList struct {
	Span
	Items []*UpdateItem `json:"items"`
}

func (n *UpdateItemList) Children() []Node {
	var out []Node
	for _, it := range n.Items {
		out = append(out, it)
	}
	return out
}

// UpdateItem is a single item in an UPDATE SET list: either a set-value
// assignment or a nested INSERT/UPDATE/DELETE statement; see ASTUpdateItem in
// googlesql/parser/parse_tree.h.
type UpdateItem struct {
	Span
	SetValue  *UpdateSetValue `json:"set_value,omitempty"`
	Statement Statement       `json:"statement,omitempty"`
}

func (n *UpdateItem) Children() []Node {
	if n.SetValue != nil {
		return children(n.SetValue)
	}
	return children(n.Statement)
}

// UpdateSetValue is a "path = value" assignment in an UPDATE SET list; see
// ASTUpdateSetValue in googlesql/parser/parse_tree.h.
type UpdateSetValue struct {
	Span
	Path  Node `json:"path"`
	Value Node `json:"value"`
}

func (n *UpdateSetValue) Children() []Node {
	return children(n.Path, n.Value)
}

// MergeStatement is a MERGE statement; see ASTMergeStatement in
// googlesql/parser/parse_tree.h.
type MergeStatement struct {
	Span
	Target         *PathExpression      `json:"target"`
	Alias          *Alias               `json:"alias,omitempty"`
	Source         Node                 `json:"source"` // *TablePathExpression or *TableSubquery
	MergeCondition Node                 `json:"merge_condition"`
	WhenClauseList *MergeWhenClauseList `json:"when_clause_list"`
}

func (n *MergeStatement) statementNode() {}
func (n *MergeStatement) Children() []Node {
	return children(n.Target, n.Alias, n.Source, n.MergeCondition, n.WhenClauseList)
}

// MergeWhenClauseList is the list of WHEN clauses of a MERGE statement; see
// ASTMergeWhenClauseList in googlesql/parser/parse_tree.h.
type MergeWhenClauseList struct {
	Span
	Clauses []*MergeWhenClause `json:"clauses"`
}

func (n *MergeWhenClauseList) Children() []Node {
	var out []Node
	for _, c := range n.Clauses {
		out = append(out, c)
	}
	return out
}

// MergeWhenClause is a single WHEN clause of a MERGE statement; see
// ASTMergeWhenClause in googlesql/parser/parse_tree.h. MatchType is
// "MATCHED", "NOT_MATCHED_BY_SOURCE", or "NOT_MATCHED_BY_TARGET".
type MergeWhenClause struct {
	Span
	MatchType       string       `json:"match_type"`
	SearchCondition Node         `json:"search_condition,omitempty"`
	Action          *MergeAction `json:"action"`
}

func (n *MergeWhenClause) Children() []Node {
	return children(n.SearchCondition, n.Action)
}

// MergeAction is the action of a MERGE WHEN clause; see ASTMergeAction in
// googlesql/parser/parse_tree.h. ActionType is "INSERT", "UPDATE", or
// "DELETE".
type MergeAction struct {
	Span
	ActionType       string           `json:"action_type"`
	InsertColumnList *ColumnList      `json:"insert_column_list,omitempty"`
	InsertRow        *InsertValuesRow `json:"insert_row,omitempty"`
	UpdateItemList   *UpdateItemList  `json:"update_item_list,omitempty"`
}

func (n *MergeAction) Children() []Node {
	return children(n.InsertColumnList, n.InsertRow, n.UpdateItemList)
}

// TruncateStatement is a TRUNCATE TABLE statement; see ASTTruncateStatement in
// googlesql/parser/parse_tree.h.
type TruncateStatement struct {
	Span
	Target *PathExpression `json:"target"`
	Where  Node            `json:"where,omitempty"`
}

func (n *TruncateStatement) statementNode() {}
func (n *TruncateStatement) Children() []Node {
	return children(n.Target, n.Where)
}

// CloneDataStatement is a CLONE DATA statement; see ASTCloneDataStatement in
// googlesql/parser/parse_tree.h.
type CloneDataStatement struct {
	Span
	Target  *PathExpression      `json:"target"`
	Sources *CloneDataSourceList `json:"sources"`
}

func (n *CloneDataStatement) statementNode() {}
func (n *CloneDataStatement) Children() []Node {
	return children(n.Target, n.Sources)
}

// CloneDataSourceList is the list of clone data sources (joined by UNION ALL)
// in a CLONE DATA statement; see ASTCloneDataSourceList in
// googlesql/parser/parse_tree.h.
type CloneDataSourceList struct {
	Span
	Sources []*CloneDataSource `json:"sources"`
}

func (n *CloneDataSourceList) Children() []Node {
	var out []Node
	for _, s := range n.Sources {
		out = append(out, s)
	}
	return out
}

// CloneDataSource is a single source of a CLONE DATA statement: a table path
// with an optional FOR SYSTEM_TIME clause and WHERE clause; see
// ASTCloneDataSource in googlesql/parser/parse_tree.h.
type CloneDataSource struct {
	Span
	Path          *PathExpression `json:"path"`
	ForSystemTime *ForSystemTime  `json:"for_system_time,omitempty"`
	Where         *WhereClause    `json:"where,omitempty"`
}

func (n *CloneDataSource) Children() []Node {
	return children(n.Path, n.ForSystemTime, n.Where)
}

// AssertRowsModified is the ASSERT_ROWS_MODIFIED clause on a DML statement;
// see ASTAssertRowsModified in googlesql/parser/parse_tree.h.
type AssertRowsModified struct {
	Span
	Value Node `json:"value"`
}

func (n *AssertRowsModified) Children() []Node {
	return children(n.Value)
}

// ReturningClause is the THEN RETURN clause on a DML statement; see
// ASTReturningClause in googlesql/parser/parse_tree.h. ActionAlias records the
// "WITH ACTION [AS alias]" column name (defaulting to "ACTION").
type ReturningClause struct {
	Span
	SelectList  *SelectList `json:"select_list"`
	ActionAlias *Alias      `json:"action_alias,omitempty"`
}

func (n *ReturningClause) Children() []Node {
	return children(n.SelectList, n.ActionAlias)
}

// DotGeneralizedField is "expression . ( path )" generalized field access;
// see ASTDotGeneralizedField in googlesql/parser/parse_tree.h.
type DotGeneralizedField struct {
	Span
	Expr Node            `json:"expr"`
	Path *PathExpression `json:"path"`
}

func (n *DotGeneralizedField) Children() []Node {
	return children(n.Expr, n.Path)
}

// Query is a query expression with an optional WITH clause, optional ORDER
// BY and LIMIT/OFFSET, optionally followed by |> pipe operators.
type Query struct {
	Span
	WithClause    *WithClause  `json:"with_clause,omitempty"`
	QueryExpr     Node         `json:"query_expr"` // *Select, *FromQuery, *SetOperation, or parenthesized *Query
	OrderBy       *OrderBy     `json:"order_by,omitempty"`
	Limit         *LimitOffset `json:"limit,omitempty"`
	LockMode      *LockMode    `json:"lock_mode,omitempty"`
	PipeOperators []Node       `json:"pipe_operators,omitempty"`
	// Parenthesized records that the query was written inside parentheses.
	// It does not appear in the debug output but affects how trailing pipe
	// operators nest; see the query rule in googlesql.tm.
	Parenthesized bool `json:"parenthesized,omitempty"`
	// IsPivotInput records that this query is the input to a PIVOT clause; it
	// renders as " (pivot input)" in the debug string (see ASTQuery in
	// parse_tree.cc).
	IsPivotInput bool `json:"is_pivot_input,omitempty"`
}

func (n *Query) Children() []Node {
	out := children(n.WithClause, n.QueryExpr, n.OrderBy, n.Limit, n.LockMode)
	return append(out, n.PipeOperators...)
}

// AliasedQueryExpression is a parenthesized query with an alias, e.g.
// "(SELECT 1) AS q1", used as a query primary; see ASTAliasedQueryExpression
// in googlesql/parser/parse_tree.h. It is only valid with pipe syntax enabled.
type AliasedQueryExpression struct {
	Span
	Query *Query `json:"query"`
	Alias *Alias `json:"alias"`
}

func (n *AliasedQueryExpression) Children() []Node {
	return children(n.Query, n.Alias)
}

// WithClause is a WITH clause holding one or more common table expression
// entries; see ASTWithClause in googlesql/parser/parse_tree.h.
type WithClause struct {
	Span
	Recursive bool               `json:"recursive,omitempty"`
	Entries   []*WithClauseEntry `json:"entries"`
}

func (n *WithClause) Children() []Node {
	var out []Node
	for _, e := range n.Entries {
		out = append(out, e)
	}
	return out
}

// WithClauseEntry is a single entry in a WITH clause, wrapping either an
// aliased query or an aliased GROUP ROWS entry; see ASTWithClauseEntry in
// googlesql/parser/parse_tree.h. Exactly one of AliasedQuery and
// AliasedGroupRows is set.
type WithClauseEntry struct {
	Span
	AliasedQuery     *AliasedQuery     `json:"aliased_query,omitempty"`
	AliasedGroupRows *AliasedGroupRows `json:"aliased_group_rows,omitempty"`
}

func (n *WithClauseEntry) Children() []Node {
	return children(n.AliasedQuery, n.AliasedGroupRows)
}

// AliasedGroupRows is "name() AS GROUP ROWS" in a WITH clause; see
// ASTAliasedGroupRows in googlesql/parser/parse_tree.h.
type AliasedGroupRows struct {
	Span
	Identifier *Identifier `json:"identifier"`
}

func (n *AliasedGroupRows) Children() []Node {
	return children(n.Identifier)
}

// AliasedQuery is "name AS ( query )"; see ASTAliasedQuery in
// googlesql/parser/parse_tree.h. The query's location includes the
// parentheses.
type AliasedQuery struct {
	Span
	Identifier *Identifier            `json:"identifier"`
	Query      *Query                 `json:"query"`
	Modifiers  *AliasedQueryModifiers `json:"modifiers,omitempty"`
}

func (n *AliasedQuery) Children() []Node {
	return children(n.Identifier, n.Query, n.Modifiers)
}

// AliasedQueryModifiers holds the modifiers of an aliased query, currently
// only a recursion depth modifier; see ASTAliasedQueryModifiers in
// googlesql/parser/parse_tree.h.
type AliasedQueryModifiers struct {
	Span
	RecursionDepth *RecursionDepthModifier `json:"recursion_depth,omitempty"`
}

func (n *AliasedQueryModifiers) Children() []Node {
	return children(n.RecursionDepth)
}

// RecursionDepthModifier is a "WITH DEPTH [AS alias] [BETWEEN lo AND hi | MAX
// hi]" modifier on a recursive CTE; see ASTRecursionDepthModifier in
// googlesql/parser/parse_tree.h.
type RecursionDepthModifier struct {
	Span
	Alias      *Alias          `json:"alias,omitempty"`
	LowerBound *IntOrUnbounded `json:"lower_bound"`
	UpperBound *IntOrUnbounded `json:"upper_bound"`
}

func (n *RecursionDepthModifier) Children() []Node {
	var out []Node
	if n.Alias != nil {
		out = append(out, n.Alias)
	}
	return append(out, n.LowerBound, n.UpperBound)
}

// IntOrUnbounded is a recursion depth bound: either an int literal / parameter
// or unbounded (no child); see ASTIntOrUnbounded in
// googlesql/parser/parse_tree.h.
type IntOrUnbounded struct {
	Span
	Bound Node `json:"bound,omitempty"`
}

func (n *IntOrUnbounded) Children() []Node {
	return children(n.Bound)
}

// LockMode is a FOR UPDATE locking clause on a query; see ASTLockMode in
// googlesql/parser/parse_tree.h. The only strength is UPDATE.
type LockMode struct {
	Span
}

func (n *LockMode) Children() []Node { return nil }

// Select is a SELECT clause with its associated clauses.
type Select struct {
	Span
	Hint         *Hint         `json:"hint,omitempty"`
	WithModifier *WithModifier `json:"with_modifier,omitempty"`
	Distinct     bool          `json:"distinct,omitempty"`
	SelectAs     *SelectAs     `json:"select_as,omitempty"`
	SelectList   *SelectList   `json:"select_list"`
	From         *FromClause   `json:"from,omitempty"`
	Where        *WhereClause  `json:"where,omitempty"`
	GroupBy      *GroupBy      `json:"group_by,omitempty"`
	Having       *Having       `json:"having,omitempty"`
	Qualify      *Qualify      `json:"qualify,omitempty"`
	Window       *WindowClause `json:"window,omitempty"`
}

func (n *Select) Children() []Node {
	return children(n.Hint, n.WithModifier, n.SelectAs, n.SelectList, n.From, n.Where, n.GroupBy, n.Having, n.Qualify, n.Window)
}

// SelectAs is the "AS STRUCT", "AS VALUE", or "AS <type_name>" clause that may
// follow SELECT (and ALL/DISTINCT). AsMode is "STRUCT" or "VALUE"; for a type
// name it is empty and TypeName holds the path expression. See ASTSelectAs in
// googlesql/parser/parse_tree.h.
type SelectAs struct {
	Span
	AsMode   string          `json:"as_mode,omitempty"`
	TypeName *PathExpression `json:"type_name,omitempty"`
}

func (n *SelectAs) Children() []Node { return children(n.TypeName) }

// WithModifier is the "WITH <identifier> [OPTIONS(...)]" modifier after SELECT,
// used for anonymization and differential privacy. See ASTWithModifier in
// googlesql/parser/parse_tree.h.
type WithModifier struct {
	Span
	Identifier *Identifier  `json:"identifier"`
	Options    *OptionsList `json:"options,omitempty"`
}

func (n *WithModifier) Children() []Node { return children(n.Identifier, n.Options) }

// SelectList is the list of expressions being selected.
type SelectList struct {
	Span
	Columns []*SelectColumn `json:"columns"`
}

func (n *SelectList) Children() []Node {
	var out []Node
	for _, c := range n.Columns {
		out = append(out, c)
	}
	return out
}

// SelectColumn is a single item in a select list.
type SelectColumn struct {
	Span
	Expr  Node   `json:"expr"`
	Alias *Alias `json:"alias,omitempty"`
	// Order is the ASC/DESC[/NULLS] suffix on a pipe AGGREGATE selection
	// item; see pipe_selection_item_with_order in googlesql.tm.
	Order *GroupingItemOrder `json:"order,omitempty"`
}

func (n *SelectColumn) Children() []Node {
	return children(n.Expr, n.Alias, n.Order)
}

// Alias is an [AS] name alias.
type Alias struct {
	Span
	Identifier *Identifier `json:"identifier"`
}

func (n *Alias) Children() []Node {
	return children(n.Identifier)
}

// IntoAlias is an "INTO name" alias; see ASTIntoAlias in
// googlesql/parser/parse_tree.h.
type IntoAlias struct {
	Span
	Identifier *Identifier `json:"identifier"`
}

func (n *IntoAlias) Children() []Node {
	return children(n.Identifier)
}

// Star is the * in SELECT *.
type Star struct {
	Span
	Image string `json:"image"`
}

func (n *Star) Children() []Node { return nil }

// StarWithModifiers is "* EXCEPT (...) REPLACE (...)" in a select list; see
// ASTStarWithModifiers in googlesql/parser/parse_tree.h.
type StarWithModifiers struct {
	Span
	Modifiers *StarModifiers `json:"modifiers"`
}

func (n *StarWithModifiers) Children() []Node { return children(n.Modifiers) }

// DotStar is "expression . *" as a select list item; see ASTDotStar in
// googlesql/parser/parse_tree.h.
type DotStar struct {
	Span
	Expr Node `json:"expr"`
}

func (n *DotStar) Children() []Node { return children(n.Expr) }

// DotStarWithModifiers is "expression . * EXCEPT (...) REPLACE (...)"; see
// ASTDotStarWithModifiers in googlesql/parser/parse_tree.h.
type DotStarWithModifiers struct {
	Span
	Expr      Node           `json:"expr"`
	Modifiers *StarModifiers `json:"modifiers"`
}

func (n *DotStarWithModifiers) Children() []Node { return children(n.Expr, n.Modifiers) }

// StarModifiers holds the EXCEPT column list and REPLACE items following "*"
// or ".*"; see ASTStarModifiers in googlesql/parser/parse_tree.h.
type StarModifiers struct {
	Span
	ExceptList   *StarExceptList    `json:"except_list"`
	ReplaceItems []*StarReplaceItem `json:"replace_items"`
}

func (n *StarModifiers) Children() []Node {
	out := children(n.ExceptList)
	for _, item := range n.ReplaceItems {
		out = append(out, item)
	}
	return out
}

// StarExceptList is the "EXCEPT ( identifiers )" part of star modifiers; see
// ASTStarExceptList in googlesql/parser/parse_tree.h.
type StarExceptList struct {
	Span
	Identifiers []*Identifier `json:"identifiers"`
}

func (n *StarExceptList) Children() []Node {
	var out []Node
	for _, id := range n.Identifiers {
		out = append(out, id)
	}
	return out
}

// StarReplaceItem is one "expression AS identifier" inside "REPLACE (...)";
// see ASTStarReplaceItem in googlesql/parser/parse_tree.h.
type StarReplaceItem struct {
	Span
	Expr  Node        `json:"expr"`
	Alias *Identifier `json:"alias"`
}

func (n *StarReplaceItem) Children() []Node { return children(n.Expr, n.Alias) }

// FromClause holds the FROM clause contents.
type FromClause struct {
	Span
	TableExpression Node `json:"table_expression"`
}

func (n *FromClause) Children() []Node {
	return children(n.TableExpression)
}

// TablePathExpression is a table reference by (possibly dotted) path or an
// UNNEST expression; exactly one of Path and UnnestExpr is set.
type TablePathExpression struct {
	Span
	Path       *PathExpression   `json:"path,omitempty"`
	UnnestExpr *UnnestExpression `json:"unnest_expr,omitempty"`
	// Hint is an optional hint (@{...}) between the path and the alias.
	Hint   *Hint       `json:"hint,omitempty"`
	Alias  *Alias      `json:"alias,omitempty"`
	Offset *WithOffset `json:"offset,omitempty"`
	// ForSystemTime is the optional FOR SYSTEM TIME AS OF clause.
	ForSystemTime *ForSystemTime `json:"for_system_time,omitempty"`
	// PostfixOperators holds trailing postfix table operators such as
	// MATCH_RECOGNIZE(...); see ASTPostfixTableOperator in
	// googlesql/parser/parse_tree.h.
	PostfixOperators []Node `json:"postfix_operators,omitempty"`
}

func (n *TablePathExpression) Children() []Node {
	out := children(n.Path, n.UnnestExpr, n.Hint, n.Alias, n.Offset, n.ForSystemTime)
	return append(out, n.PostfixOperators...)
}

// TableSubquery is a parenthesized query used as a table in a FROM clause,
// with an optional alias; see ASTTableSubquery in
// googlesql/parser/parse_tree.h. The span includes the parentheses and the
// alias, but the query's own location excludes the parentheses.
type TableSubquery struct {
	Span
	Query *Query `json:"query"`
	Alias *Alias `json:"alias,omitempty"`
	// IsLateral is true when the subquery is preceded by the LATERAL
	// keyword; the span then includes the keyword.
	IsLateral bool `json:"is_lateral,omitempty"`
	// PostfixOperators holds trailing postfix table operators such as
	// MATCH_RECOGNIZE(...).
	PostfixOperators []Node `json:"postfix_operators,omitempty"`
}

func (n *TableSubquery) Children() []Node {
	out := children(n.Query, n.Alias)
	return append(out, n.PostfixOperators...)
}

// TVF is a call to a table-valued function in a FROM clause, e.g.
// "FROM tvf(arg1, arg2)"; see ASTTVF in googlesql/parser/parse_tree.h. The
// span includes the closing parenthesis and the alias when present.
type TVF struct {
	Span
	Name  *PathExpression `json:"name"`
	Args  []*TVFArgument  `json:"args,omitempty"`
	Hint  *Hint           `json:"hint,omitempty"`
	Alias *Alias          `json:"alias,omitempty"`
	// IsLateral is true when the call is preceded by the LATERAL keyword;
	// the span then includes the keyword.
	IsLateral bool `json:"is_lateral,omitempty"`
	// PostfixOperators holds trailing postfix table operators such as
	// MATCH_RECOGNIZE(...).
	PostfixOperators []Node `json:"postfix_operators,omitempty"`
}

func (n *TVF) Children() []Node {
	out := children(n.Name)
	for _, a := range n.Args {
		out = append(out, a)
	}
	out = append(out, children(n.Hint)...)
	out = append(out, children(n.Alias)...)
	return append(out, n.PostfixOperators...)
}

// TVFArgument is a single argument to a table-valued function call; see
// ASTTVFArgument in googlesql/parser/parse_tree.h.
type TVFArgument struct {
	Span
	Expr Node `json:"expr"`
}

func (n *TVFArgument) Children() []Node {
	return children(n.Expr)
}

// InputTableArgument is the "INPUT TABLE" argument form for a table-valued
// function call in a pipe CALL; see ASTInputTableArgument in
// googlesql/parser/parse_tree.h. It has no children.
type InputTableArgument struct {
	Span
}

func (n *InputTableArgument) Children() []Node { return nil }

// Descriptor is a "DESCRIPTOR(col, ...)" table-valued function argument; see
// ASTDescriptor in googlesql/parser/parse_tree.h.
type Descriptor struct {
	Span
	Columns *DescriptorColumnList `json:"columns"`
}

func (n *Descriptor) Children() []Node { return children(n.Columns) }

// DescriptorColumnList is the list of columns in a DESCRIPTOR(...) argument;
// see ASTDescriptorColumnList in googlesql/parser/parse_tree.h.
type DescriptorColumnList struct {
	Span
	Columns []*DescriptorColumn `json:"columns"`
}

func (n *DescriptorColumnList) Children() []Node {
	out := make([]Node, 0, len(n.Columns))
	for _, c := range n.Columns {
		out = append(out, c)
	}
	return out
}

// DescriptorColumn is a single column name in a DESCRIPTOR(...) argument; see
// ASTDescriptorColumn in googlesql/parser/parse_tree.h.
type DescriptorColumn struct {
	Span
	Name *Identifier `json:"name"`
}

func (n *DescriptorColumn) Children() []Node { return children(n.Name) }

// UnnestExpression is UNNEST(expr [AS alias], ...); see ASTUnnestExpression
// in googlesql/parser/parse_tree.h. The span includes the UNNEST keyword and
// the closing parenthesis.
type UnnestExpression struct {
	Span
	Expressions []*ExpressionWithOptAlias `json:"expressions"`
	// ArrayZipMode is an optional trailing named argument, e.g. the
	// "mode => 'pad'" in UNNEST([1], mode => 'pad'); see unnest_expression in
	// googlesql.tm.
	ArrayZipMode *NamedArgument `json:"array_zip_mode,omitempty"`
}

func (n *UnnestExpression) Children() []Node {
	var out []Node
	for _, e := range n.Expressions {
		out = append(out, e)
	}
	if n.ArrayZipMode != nil {
		out = append(out, n.ArrayZipMode)
	}
	return out
}

// ExpressionWithOptAlias is an expression with an optional "AS alias" (the
// AS keyword is required when the alias is present).
type ExpressionWithOptAlias struct {
	Span
	Expr  Node   `json:"expr"`
	Alias *Alias `json:"alias,omitempty"`
}

func (n *ExpressionWithOptAlias) Children() []Node {
	return children(n.Expr, n.Alias)
}

// WithOffset is the WITH OFFSET [[AS] alias] clause on a FROM-clause table
// expression.
type WithOffset struct {
	Span
	Alias *Alias `json:"alias,omitempty"`
}

func (n *WithOffset) Children() []Node {
	return children(n.Alias)
}

// ForSystemTime is a FOR SYSTEM TIME AS OF <expression> clause on a
// FROM-clause table expression; see ASTForSystemTime in
// googlesql/parser/parse_tree.h.
type ForSystemTime struct {
	Span
	Expr Node `json:"expr"`
}

func (n *ForSystemTime) Children() []Node {
	return children(n.Expr)
}

// SampleClause is a TABLESAMPLE clause: "TABLESAMPLE <method> ( <size> )
// [<suffix>]"; see ASTSampleClause in googlesql/parser/parse_tree.h.
type SampleClause struct {
	Span
	Method *Identifier   `json:"method"`
	Size   *SampleSize   `json:"size"`
	Suffix *SampleSuffix `json:"suffix,omitempty"`
}

func (n *SampleClause) Children() []Node {
	return children(n.Method, n.Size, n.Suffix)
}

// SampleSize is the size portion of a TABLESAMPLE clause: "<value> ROWS" or
// "<value> PERCENT", with an optional PARTITION BY; see ASTSampleSize in
// googlesql/parser/parse_tree.h. Unit is stored but not shown in the debug
// string.
type SampleSize struct {
	Span
	Value       Node         `json:"value"`
	Unit        string       `json:"unit"` // "ROWS" or "PERCENT"
	PartitionBy *PartitionBy `json:"partition_by,omitempty"`
}

func (n *SampleSize) Children() []Node {
	return children(n.Value, n.PartitionBy)
}

// SampleSuffix is the optional suffix of a TABLESAMPLE clause: a REPEATABLE
// clause and/or a WITH WEIGHT clause; see ASTSampleSuffix in
// googlesql/parser/parse_tree.h.
type SampleSuffix struct {
	Span
	Weight     *WithWeight       `json:"weight,omitempty"`
	Repeatable *RepeatableClause `json:"repeatable,omitempty"`
}

func (n *SampleSuffix) Children() []Node {
	return children(n.Weight, n.Repeatable)
}

// WithWeight is the "WITH WEIGHT [[AS] alias]" part of a TABLESAMPLE suffix;
// see ASTWithWeight in googlesql/parser/parse_tree.h.
type WithWeight struct {
	Span
	Alias *Alias `json:"alias,omitempty"`
}

func (n *WithWeight) Children() []Node {
	return children(n.Alias)
}

// RepeatableClause is the "REPEATABLE ( <value> )" part of a TABLESAMPLE
// suffix; see ASTRepeatableClause in googlesql/parser/parse_tree.h.
type RepeatableClause struct {
	Span
	Value Node `json:"value"`
}

func (n *RepeatableClause) Children() []Node {
	return children(n.Value)
}

// PathExpressionList is a comma-separated list of path expressions; see
// ASTPathExpressionList in googlesql/parser/parse_tree.h.
type PathExpressionList struct {
	Span
	Paths []*PathExpression `json:"paths"`
}

func (n *PathExpressionList) Children() []Node {
	out := make([]Node, 0, len(n.Paths))
	for _, p := range n.Paths {
		out = append(out, p)
	}
	return out
}

// PivotClause is a "PIVOT(...)" postfix table operator; see ASTPivotClause in
// googlesql/parser/parse_tree.h. Child order matches the reference:
// PivotExpressionList, the FOR expression, PivotValueList, then an optional
// output alias.
type PivotClause struct {
	Span
	Expressions *PivotExpressionList `json:"expressions"`
	ForExpr     Node                 `json:"for_expr"`
	Values      *PivotValueList      `json:"values"`
	Alias       *Alias               `json:"alias,omitempty"`
}

func (n *PivotClause) Children() []Node {
	return children(n.Expressions, n.ForExpr, n.Values, n.Alias)
}

// PivotExpressionList is the list of pivot expressions in a PIVOT clause; see
// ASTPivotExpressionList in googlesql/parser/parse_tree.h.
type PivotExpressionList struct {
	Span
	Expressions []*PivotExpression `json:"expressions"`
}

func (n *PivotExpressionList) Children() []Node {
	out := make([]Node, 0, len(n.Expressions))
	for _, e := range n.Expressions {
		out = append(out, e)
	}
	return out
}

// PivotExpression is a single pivot expression with an optional alias; see
// ASTPivotExpression in googlesql/parser/parse_tree.h.
type PivotExpression struct {
	Span
	Expr  Node   `json:"expr"`
	Alias *Alias `json:"alias,omitempty"`
}

func (n *PivotExpression) Children() []Node {
	return children(n.Expr, n.Alias)
}

// PivotValueList is the list of pivot values in a PIVOT clause; see
// ASTPivotValueList in googlesql/parser/parse_tree.h.
type PivotValueList struct {
	Span
	Values []*PivotValue `json:"values"`
}

func (n *PivotValueList) Children() []Node {
	out := make([]Node, 0, len(n.Values))
	for _, v := range n.Values {
		out = append(out, v)
	}
	return out
}

// PivotValue is a single pivot value with an optional alias; see
// ASTPivotValue in googlesql/parser/parse_tree.h.
type PivotValue struct {
	Span
	Value Node   `json:"value"`
	Alias *Alias `json:"alias,omitempty"`
}

func (n *PivotValue) Children() []Node {
	return children(n.Value, n.Alias)
}

// UnpivotClause is an "UNPIVOT(...)" postfix table operator; see
// ASTUnpivotClause in googlesql/parser/parse_tree.h. Child order matches the
// reference: the unpivot column list, the FOR path expression, the IN item
// list, then an optional output alias.
type UnpivotClause struct {
	Span
	// NullFilter is "", "EXCLUDE NULLS", or "INCLUDE NULLS".
	NullFilter string              `json:"null_filter,omitempty"`
	Columns    *PathExpressionList `json:"columns"`
	ForExpr    *PathExpression     `json:"for_expr"`
	InItems    *UnpivotInItemList  `json:"in_items"`
	Alias      *Alias              `json:"alias,omitempty"`
}

func (n *UnpivotClause) Children() []Node {
	return children(n.Columns, n.ForExpr, n.InItems, n.Alias)
}

// UnpivotInItemList is the list of IN items in an UNPIVOT clause; see
// ASTUnpivotInItemList in googlesql/parser/parse_tree.h. Its span includes
// the surrounding parentheses.
type UnpivotInItemList struct {
	Span
	Items []*UnpivotInItem `json:"items"`
}

func (n *UnpivotInItemList) Children() []Node {
	out := make([]Node, 0, len(n.Items))
	for _, it := range n.Items {
		out = append(out, it)
	}
	return out
}

// UnpivotInItem is a single IN item in an UNPIVOT clause: a column list with
// an optional label; see ASTUnpivotInItem in googlesql/parser/parse_tree.h.
type UnpivotInItem struct {
	Span
	Columns *PathExpressionList `json:"columns"`
	Label   *UnpivotInItemLabel `json:"label,omitempty"`
}

func (n *UnpivotInItem) Children() []Node {
	return children(n.Columns, n.Label)
}

// UnpivotInItemLabel is the "[AS] (string|integer) literal" label of an
// UNPIVOT IN item; see ASTUnpivotInItemLabel in
// googlesql/parser/parse_tree.h.
type UnpivotInItemLabel struct {
	Span
	Label Node `json:"label"`
}

func (n *UnpivotInItemLabel) Children() []Node {
	return children(n.Label)
}

// Join is a JOIN between two table expressions, including comma joins; see
// ASTJoin in googlesql/parser/parse_tree.h. Child order matches the
// reference: Lhs, Hint, JoinLocation, Rhs, then the ON/USING clause (or the
// clause list before consecutive-ON transformation).
type Join struct {
	Span
	// JoinType is "" for a plain JOIN, or "COMMA", "CROSS", "FULL", "INNER",
	// "LEFT", or "RIGHT".
	JoinType string `json:"join_type,omitempty"`
	// JoinHint is "" or the join hint keyword "HASH" or "LOOKUP".
	JoinHint string `json:"join_hint,omitempty"`
	Natural  bool   `json:"natural,omitempty"`
	Lhs      Node   `json:"lhs"`
	Hint     *Hint  `json:"hint,omitempty"`
	// JoinLocation covers the join keywords (e.g. "LEFT OUTER JOIN"), or the
	// comma of a comma join.
	JoinLocation *Location `json:"join_location"`
	Rhs          Node      `json:"rhs"`
	// OnOrUsingClause is the *OnClause or *UsingClause, if any.
	OnOrUsingClause Node `json:"on_or_using_clause,omitempty"`
	// ClauseList holds two or more consecutive ON/USING clauses before the
	// join transformation dissolves them; see ASTOnOrUsingClauseList and
	// googlesql/parser/join_processor.cc.
	ClauseList *OnOrUsingClauseList `json:"clause_list,omitempty"`

	// The fields below are parser bookkeeping used by the consecutive
	// ON/USING clause transformation (join_processor.cc); they are not part
	// of the parse tree.
	UnmatchedJoinCount   int             `json:"-"`
	TransformationNeeded bool            `json:"-"`
	ContainsCommaJoin    bool            `json:"-"`
	ParseError           *JoinParseError `json:"-"`
}

func (n *Join) Children() []Node {
	return children(n.Lhs, n.Hint, n.JoinLocation, n.Rhs, n.OnOrUsingClause, n.ClauseList)
}

// JoinParseError is a deferred error recorded on a Join when there are more
// ON/USING clauses than joins that need one; see ASTJoin::ParseError in
// googlesql/parser/parse_tree.h.
type JoinParseError struct {
	ErrorNode Node
	Message   string
}

// OnClause is the ON condition of a join; see ASTOnClause in
// googlesql/parser/parse_tree.h.
type OnClause struct {
	Span
	Expr Node `json:"expr"`
}

func (n *OnClause) Children() []Node {
	return children(n.Expr)
}

// UsingClause is the USING (column, ...) clause of a join; see
// ASTUsingClause in googlesql/parser/parse_tree.h. The span includes the
// USING keyword and the parentheses.
type UsingClause struct {
	Span
	Keys []*Identifier `json:"keys"`
}

func (n *UsingClause) Children() []Node {
	var out []Node
	for _, k := range n.Keys {
		out = append(out, k)
	}
	return out
}

// OnOrUsingClauseList holds consecutive ON/USING clauses of a join before
// the join transformation; see ASTOnOrUsingClauseList in
// googlesql/parser/parse_tree.h.
type OnOrUsingClauseList struct {
	Span
	Clauses []Node `json:"clauses"` // *OnClause or *UsingClause
}

func (n *OnOrUsingClauseList) Children() []Node {
	return append([]Node(nil), n.Clauses...)
}

// ParenthesizedJoin is "( join )" used as a table primary; see
// ASTParenthesizedJoin in googlesql/parser/parse_tree.h. The span includes
// the parentheses.
type ParenthesizedJoin struct {
	Span
	Join Node `json:"join"`
	// PostfixOperators holds trailing postfix table operators such as
	// MATCH_RECOGNIZE(...).
	PostfixOperators []Node `json:"postfix_operators,omitempty"`
}

func (n *ParenthesizedJoin) Children() []Node {
	out := children(n.Join)
	return append(out, n.PostfixOperators...)
}

// PipeJoin is a |> JOIN pipe operator; see ASTPipeJoin in
// googlesql/parser/parse_tree.h. The contained Join's Lhs is a
// PipeJoinLhsPlaceholder because the left input is the pipe input.
type PipeJoin struct {
	Span
	Join Node `json:"join"`
}

func (n *PipeJoin) Children() []Node {
	return children(n.Join)
}

// PipeJoinLhsPlaceholder stands in for the missing left input of a pipe
// JOIN; see ASTPipeJoinLhsPlaceholder in googlesql/parser/parse_tree.h.
type PipeJoinLhsPlaceholder struct {
	Span
}

func (n *PipeJoinLhsPlaceholder) Children() []Node { return nil }

// ArrayConstructor is "[...]" or "ARRAY[...]"; see ASTArrayConstructor in
// googlesql/parser/parse_tree.h.
type ArrayConstructor struct {
	Span
	Type     *ArrayType `json:"type,omitempty"`
	Elements []Node     `json:"elements"`
}

func (n *ArrayConstructor) Children() []Node {
	return append(children(n.Type), n.Elements...)
}

// WhereClause holds the WHERE clause expression.
type WhereClause struct {
	Span
	Expr Node `json:"expr"`
}

func (n *WhereClause) Children() []Node {
	return children(n.Expr)
}

// GroupBy is a GROUP BY clause.
type GroupBy struct {
	Span
	Hint *Hint       `json:"hint,omitempty"`
	All  *GroupByAll `json:"all,omitempty"`
	// AndOrderBy is true when a pipe AGGREGATE's GROUP BY was written as
	// "GROUP AND ORDER BY"; see group_by_preamble_in_pipe in googlesql.tm.
	AndOrderBy bool            `json:"and_order_by,omitempty"`
	Items      []*GroupingItem `json:"items"`
}

func (n *GroupBy) Children() []Node {
	out := children(n.Hint)
	if n.All != nil {
		out = append(out, n.All)
	}
	for _, item := range n.Items {
		out = append(out, item)
	}
	return out
}

// GroupByAll is the ALL keyword in "GROUP BY ALL".
type GroupByAll struct {
	Span
}

func (n *GroupByAll) Children() []Node { return nil }

// GroupingItem is a single GROUP BY item: an expression (a regular group by
// key), a ROLLUP list, a CUBE list, a GROUPING SETS list, or the empty item
// "()". See ASTGroupingItem in googlesql/parser/parse_tree.h. Alias and Order
// may only be present for the expression case in pipe AGGREGATE.
type GroupingItem struct {
	Span
	Expr            Node               `json:"expr,omitempty"`
	Rollup          *Rollup            `json:"rollup,omitempty"`
	Cube            *Cube              `json:"cube,omitempty"`
	GroupingSetList *GroupingSetList   `json:"grouping_set_list,omitempty"`
	Alias           *Alias             `json:"alias,omitempty"`
	Order           *GroupingItemOrder `json:"grouping_item_order,omitempty"`
}

func (n *GroupingItem) Children() []Node {
	return children(n.Expr, n.Rollup, n.Cube, n.GroupingSetList, n.Alias, n.Order)
}

// Rollup is a "ROLLUP(expr, ...)" grouping specification; see ASTRollup in
// googlesql/parser/parse_tree.h.
type Rollup struct {
	Span
	Expressions []Node `json:"expressions"`
}

func (n *Rollup) Children() []Node { return children(n.Expressions...) }

// Cube is a "CUBE(expr, ...)" grouping specification; see ASTCube in
// googlesql/parser/parse_tree.h.
type Cube struct {
	Span
	Expressions []Node `json:"expressions"`
}

func (n *Cube) Children() []Node { return children(n.Expressions...) }

// GroupingSet is one grouping set inside GROUPING SETS: the empty set "()", a
// single expression, a ROLLUP, or a CUBE. See ASTGroupingSet in
// googlesql/parser/parse_tree.h.
type GroupingSet struct {
	Span
	Expr   Node    `json:"expr,omitempty"`
	Rollup *Rollup `json:"rollup,omitempty"`
	Cube   *Cube   `json:"cube,omitempty"`
}

func (n *GroupingSet) Children() []Node { return children(n.Expr, n.Rollup, n.Cube) }

// GroupingSetList is the list of grouping sets in "GROUPING SETS(...)"; see
// ASTGroupingSetList in googlesql/parser/parse_tree.h.
type GroupingSetList struct {
	Span
	GroupingSets []*GroupingSet `json:"grouping_sets"`
}

func (n *GroupingSetList) Children() []Node {
	out := make([]Node, 0, len(n.GroupingSets))
	for _, gs := range n.GroupingSets {
		out = append(out, gs)
	}
	return out
}

// GroupingItemOrder is the ASC/DESC and/or NULLS FIRST/LAST suffix on a pipe
// AGGREGATE grouping item; see ASTGroupingItemOrder in
// googlesql/parser/parse_tree.h. Spec is "ASC", "DESC", or "UNSPECIFIED"; the
// last is used when only a null order was written.
type GroupingItemOrder struct {
	Span
	Spec      string     `json:"spec"`
	NullOrder *NullOrder `json:"null_order,omitempty"`
}

func (n *GroupingItemOrder) Children() []Node { return children(n.NullOrder) }

// Having is a HAVING clause.
type Having struct {
	Span
	Expr Node `json:"expr"`
}

func (n *Having) Children() []Node {
	return children(n.Expr)
}

// Qualify is a QUALIFY clause.
type Qualify struct {
	Span
	Expr Node `json:"expr"`
}

func (n *Qualify) Children() []Node {
	return children(n.Expr)
}

// OrderBy is an ORDER BY clause.
type OrderBy struct {
	Span
	Hint  *Hint                 `json:"hint,omitempty"`
	Items []*OrderingExpression `json:"items"`
}

func (n *OrderBy) Children() []Node {
	out := children(n.Hint)
	for _, item := range n.Items {
		out = append(out, item)
	}
	return out
}

// OrderingExpression is a single ORDER BY item.
type OrderingExpression struct {
	Span
	Expr       Node         `json:"expr"`
	Collate    *Collate     `json:"collate,omitempty"`
	NullOrder  *NullOrder   `json:"null_order,omitempty"`
	Options    *OptionsList `json:"options,omitempty"` // per-column OPTIONS in CREATE INDEX
	Descending bool         `json:"descending,omitempty"`
	HasAsc     bool         `json:"has_asc,omitempty"` // explicit ASC keyword present
}

func (n *OrderingExpression) Children() []Node {
	return children(n.Expr, n.Collate, n.NullOrder, n.Options)
}

// Collate is a "COLLATE <string literal or parameter>" clause; see ASTCollate
// in googlesql/parser/parse_tree.h. The span includes the COLLATE keyword.
type Collate struct {
	Span
	Name Node `json:"name"`
}

func (n *Collate) Children() []Node {
	return children(n.Name)
}

// NullOrder is a "NULLS FIRST" or "NULLS LAST" clause on an ordering
// expression; see ASTNullOrder in googlesql/parser/parse_tree.h.
type NullOrder struct {
	Span
	NullsFirst bool `json:"nulls_first"`
}

func (n *NullOrder) Children() []Node { return nil }

// Hint is a "@{name=value, ...}" hint annotation, optionally preceded by an
// integer shard count as in "@4"; see ASTHint in
// googlesql/parser/parse_tree.h. The span starts at the "@".
type Hint struct {
	Span
	NumShardsHint Node         `json:"num_shards_hint,omitempty"` // integer literal in @<int>
	Entries       []*HintEntry `json:"entries,omitempty"`
}

func (n *Hint) Children() []Node {
	out := children(n.NumShardsHint)
	for _, e := range n.Entries {
		out = append(out, e)
	}
	return out
}

// HintEntry is a single "[qualifier.]name = value" entry in a hint; see
// ASTHintEntry in googlesql/parser/parse_tree.h.
type HintEntry struct {
	Span
	Qualifier *Identifier `json:"qualifier,omitempty"`
	Name      *Identifier `json:"name"`
	Value     Node        `json:"value"`
}

func (n *HintEntry) Children() []Node {
	return children(n.Qualifier, n.Name, n.Value)
}

// LimitOffset is a LIMIT [OFFSET] clause.
type LimitOffset struct {
	Span
	Limit  *Limit `json:"limit"`
	Offset Node   `json:"offset,omitempty"`
}

func (n *LimitOffset) Children() []Node {
	return children(n.Limit, n.Offset)
}

// Limit wraps the expression (or ALL) of a LIMIT clause, including the LIMIT
// keyword in its location.
type Limit struct {
	Span
	Expr Node `json:"expr"` // expression, or *LimitAll for LIMIT ALL
}

func (n *Limit) Children() []Node {
	return children(n.Expr)
}

// LimitAll is the ALL keyword in LIMIT ALL.
type LimitAll struct {
	Span
}

func (n *LimitAll) Children() []Node { return nil }

// Identifier is a single (possibly quoted) identifier.
type Identifier struct {
	Span
	Name string `json:"name"` // unquoted name
}

func (n *Identifier) Children() []Node { return nil }

// PathExpression is a dotted path of identifiers.
type PathExpression struct {
	Span
	Names []*Identifier `json:"names"`
}

func (n *PathExpression) Children() []Node {
	var out []Node
	for _, name := range n.Names {
		out = append(out, name)
	}
	return out
}

// NullLiteral is the NULL literal.
type NullLiteral struct {
	Span
	Image string `json:"image"`
}

func (n *NullLiteral) Children() []Node { return nil }

// BooleanLiteral is TRUE or FALSE.
type BooleanLiteral struct {
	Span
	Image string `json:"image"`
	Value bool   `json:"value"`
}

func (n *BooleanLiteral) Children() []Node { return nil }

// IntLiteral is an integer literal.
type IntLiteral struct {
	Span
	Image string `json:"image"`
}

func (n *IntLiteral) Children() []Node { return nil }

// FloatLiteral is a floating point literal.
type FloatLiteral struct {
	Span
	Image string `json:"image"`
}

func (n *FloatLiteral) Children() []Node { return nil }

// StringLiteral is a string literal, made of one or more concatenated
// components.
type StringLiteral struct {
	Span
	Components []*StringLiteralComponent `json:"components"`
}

func (n *StringLiteral) Children() []Node {
	var out []Node
	for _, c := range n.Components {
		out = append(out, c)
	}
	return out
}

// StringLiteralComponent is a single quoted piece of a string literal.
type StringLiteralComponent struct {
	Span
	Image string `json:"image"` // raw text including quotes and any prefix
}

func (n *StringLiteralComponent) Children() []Node { return nil }

// BytesLiteral is a bytes literal, made of one or more concatenated
// components.
type BytesLiteral struct {
	Span
	Components []*BytesLiteralComponent `json:"components"`
}

func (n *BytesLiteral) Children() []Node {
	var out []Node
	for _, c := range n.Components {
		out = append(out, c)
	}
	return out
}

// BytesLiteralComponent is a single quoted piece of a bytes literal.
type BytesLiteralComponent struct {
	Span
	Image string `json:"image"` // raw text including quotes and prefix
}

func (n *BytesLiteralComponent) Children() []Node { return nil }

// NumericLiteral is a NUMERIC/DECIMAL typed literal: NUMERIC '123'.
type NumericLiteral struct {
	Span
	Value *StringLiteral `json:"value"`
}

func (n *NumericLiteral) Children() []Node { return children(n.Value) }

// BigNumericLiteral is a BIGNUMERIC/BIGDECIMAL typed literal.
type BigNumericLiteral struct {
	Span
	Value *StringLiteral `json:"value"`
}

func (n *BigNumericLiteral) Children() []Node { return children(n.Value) }

// JSONLiteral is a JSON typed literal: JSON '{"a": 1}'.
type JSONLiteral struct {
	Span
	Value *StringLiteral `json:"value"`
}

func (n *JSONLiteral) Children() []Node { return children(n.Value) }

// DateOrTimeLiteral is a DATE/DATETIME/TIME/TIMESTAMP typed literal. TypeKind
// is one of the ZetaSQL type kind names, e.g. "TYPE_DATE".
type DateOrTimeLiteral struct {
	Span
	TypeKind string         `json:"type_kind"`
	Value    *StringLiteral `json:"value"`
}

func (n *DateOrTimeLiteral) Children() []Node { return children(n.Value) }

// IntervalExpr is an INTERVAL expression: INTERVAL <value> <datepart> with an
// optional "TO <datepart>" range; see ASTIntervalExpr / interval_expression in
// googlesql.tm.
type IntervalExpr struct {
	Span
	Value      Node        `json:"value"`
	DatePart   *Identifier `json:"date_part"`
	DatePartTo *Identifier `json:"date_part_to,omitempty"`
}

func (n *IntervalExpr) Children() []Node {
	return children(n.Value, n.DatePart, n.DatePartTo)
}

// RangeLiteral is a RANGE<type> typed literal: RANGE<DATE> '[a, b)'.
type RangeLiteral struct {
	Span
	Type  *RangeType     `json:"type"`
	Value *StringLiteral `json:"value"`
}

func (n *RangeLiteral) Children() []Node { return children(n.Type, n.Value) }

// UnaryExpression is a unary operator applied to an expression.
type UnaryExpression struct {
	Span
	Op      string `json:"op"` // "-", "+", "~", "NOT"
	Operand Node   `json:"operand"`
}

func (n *UnaryExpression) Children() []Node {
	return children(n.Operand)
}

// BinaryExpression is a binary operator applied to two expressions.
type BinaryExpression struct {
	Span
	Op    string `json:"op"` // "+", "-", "*", "/", "=", "!=", "<", ...
	IsNot bool   `json:"is_not,omitempty"`
	Left  Node   `json:"left"`
	Right Node   `json:"right"`
}

func (n *BinaryExpression) Children() []Node {
	return children(n.Left, n.Right)
}

// BitwiseShiftExpression is <lhs> (<< | >>) <rhs>. Unlike other binary
// operators, ZetaSQL gives the shift operators a dedicated node with an
// explicit Location child covering the operator token; see
// ASTBitwiseShiftExpression in googlesql/parser/parse_tree.h.
type BitwiseShiftExpression struct {
	Span
	IsLeftShift      bool      `json:"is_left_shift"`
	Lhs              Node      `json:"lhs"`
	OperatorLocation *Location `json:"operator_location"`
	Rhs              Node      `json:"rhs"`
}

func (n *BitwiseShiftExpression) Children() []Node {
	return children(n.Lhs, n.OperatorLocation, n.Rhs)
}

// AndExpr is an AND expression with two or more conjuncts, flattened.
type AndExpr struct {
	Span
	Conjuncts []Node `json:"conjuncts"`
}

func (n *AndExpr) Children() []Node {
	return append([]Node(nil), n.Conjuncts...)
}

// OrExpr is an OR expression with two or more disjuncts, flattened.
type OrExpr struct {
	Span
	Disjuncts []Node `json:"disjuncts"`
}

func (n *OrExpr) Children() []Node {
	return append([]Node(nil), n.Disjuncts...)
}

// Location marks the source location of a piece of syntax that has no
// dedicated node, such as the BETWEEN keyword inside a BetweenExpression.
type Location struct {
	Span
}

func (n *Location) Children() []Node { return nil }

// BetweenExpression is <lhs> [NOT] BETWEEN <low> AND <high>.
type BetweenExpression struct {
	Span
	IsNot           bool      `json:"is_not,omitempty"`
	Lhs             Node      `json:"lhs"`
	BetweenLocation *Location `json:"between_location"`
	Low             Node      `json:"low"`
	High            Node      `json:"high"`
}

func (n *BetweenExpression) Children() []Node {
	return children(n.Lhs, n.BetweenLocation, n.Low, n.High)
}

// ClampedBetweenModifier is the "CLAMPED BETWEEN <low> AND <high>" modifier
// on aggregate function calls; see ASTClampedBetweenModifier in
// googlesql/parser/parse_tree.h.
type ClampedBetweenModifier struct {
	Span
	Low  Node `json:"low"`
	High Node `json:"high"`
}

func (n *ClampedBetweenModifier) Children() []Node {
	return children(n.Low, n.High)
}

// WithReportModifier is the "WITH REPORT [options_list]" modifier on an
// aggregate function call; see ASTWithReportModifier in
// googlesql/parser/parse_tree.h and with_report_modifier in googlesql.tm.
type WithReportModifier struct {
	Span
	Options *OptionsList `json:"options,omitempty"`
}

func (n *WithReportModifier) Children() []Node {
	return children(n.Options)
}

// SequenceArg is a "SEQUENCE path" function-call argument; see ASTSequenceArg
// in googlesql/parser/parse_tree.h and sequence_arg in googlesql.tm.
type SequenceArg struct {
	Span
	Sequence *PathExpression `json:"sequence"`
}

func (n *SequenceArg) Children() []Node {
	return children(n.Sequence)
}

// InList is the parenthesized value list of an IN or LIKE ANY/SOME/ALL
// expression; see ASTInList in googlesql/parser/parse_tree.h. Its location
// covers the expressions but not the enclosing parentheses (except for a
// single extra-parenthesized subquery element).
type InList struct {
	Span
	Exprs []Node `json:"exprs"`
}

func (n *InList) Children() []Node { return n.Exprs }

// InExpression is <lhs> [NOT] IN <rhs> where rhs is exactly one of a value
// list, a subquery, or an UNNEST expression; see ASTInExpression in
// googlesql/parser/parse_tree.h.
type InExpression struct {
	Span
	IsNot      bool              `json:"is_not,omitempty"`
	Lhs        Node              `json:"lhs"`
	InLocation *Location         `json:"in_location"`
	Query      *Query            `json:"query,omitempty"`
	List       *InList           `json:"in_list,omitempty"`
	UnnestExpr *UnnestExpression `json:"unnest_expr,omitempty"`
}

func (n *InExpression) Children() []Node {
	return children(n.Lhs, n.InLocation, n.Query, n.List, n.UnnestExpr)
}

// AnySomeAllOp is the ANY, SOME, or ALL quantifier of a LIKE or comparison
// expression; see ASTAnySomeAllOp in googlesql/parser/parse_tree.h.
type AnySomeAllOp struct {
	Span
	Op string `json:"op"` // "ANY", "SOME", or "ALL"
}

func (n *AnySomeAllOp) Children() []Node { return nil }

// LikeExpression is <lhs> [NOT] LIKE ANY|SOME|ALL <rhs> where rhs is exactly
// one of a value list, a subquery, or an UNNEST expression; see
// ASTLikeExpression in googlesql/parser/parse_tree.h. Plain "<lhs> LIKE
// <rhs>" is a BinaryExpression instead.
type LikeExpression struct {
	Span
	IsNot        bool              `json:"is_not,omitempty"`
	Lhs          Node              `json:"lhs"`
	LikeLocation *Location         `json:"like_location"`
	Op           *AnySomeAllOp     `json:"op"`
	Query        *Query            `json:"query,omitempty"`
	List         *InList           `json:"in_list,omitempty"`
	UnnestExpr   *UnnestExpression `json:"unnest_expr,omitempty"`
}

func (n *LikeExpression) Children() []Node {
	return children(n.Lhs, n.LikeLocation, n.Op, n.Query, n.List, n.UnnestExpr)
}

// QuantifiedComparisonExpression is <lhs> <op> ANY|SOME|ALL <rhs> where op is
// a comparison operator and rhs is exactly one of a value list, a subquery, or
// an UNNEST expression; see ASTQuantifiedComparisonExpression in
// googlesql/parser/parse_tree.h. Only the subquery form may carry a hint.
type QuantifiedComparisonExpression struct {
	Span
	Op         string            `json:"op"` // "=", "!=", "<>", "<", "<=", ">", ">="
	Lhs        Node              `json:"lhs"`
	Location   *Location         `json:"location"`
	Quantifier *AnySomeAllOp     `json:"quantifier"`
	Hint       *Hint             `json:"hint,omitempty"`
	Query      *Query            `json:"query,omitempty"`
	List       *InList           `json:"in_list,omitempty"`
	UnnestExpr *UnnestExpression `json:"unnest_expr,omitempty"`
}

func (n *QuantifiedComparisonExpression) Children() []Node {
	return children(n.Lhs, n.Location, n.Quantifier, n.Hint, n.Query, n.List, n.UnnestExpr)
}

// StructConstructorWithParens is "(expr1, expr2 [, ... ])" with at least two
// expressions; see ASTStructConstructorWithParens in parse_tree.h. The
// single-expression form is a parenthesized expression, not a struct.
type StructConstructorWithParens struct {
	Span
	FieldExpressions []Node `json:"field_expressions"`
}

func (n *StructConstructorWithParens) Children() []Node {
	return children(n.FieldExpressions...)
}

// StructConstructorArg is one "expression [AS alias]" argument of a
// keyword-form struct constructor; see ASTStructConstructorArg in
// parse_tree.h. The alias requires the AS keyword (see
// as_alias_with_required_as in googlesql.tm).
type StructConstructorArg struct {
	Span
	Expression Node   `json:"expression"`
	Alias      *Alias `json:"alias,omitempty"`
}

func (n *StructConstructorArg) Children() []Node {
	return children(n.Expression, n.Alias)
}

// StructConstructorWithKeyword is "STRUCT(...)" or "STRUCT<...>(...)"; see
// ASTStructConstructorWithKeyword in parse_tree.h. StructType is nil for the
// typeless "STRUCT(...)" form.
type StructConstructorWithKeyword struct {
	Span
	StructType *StructType             `json:"struct_type,omitempty"`
	Fields     []*StructConstructorArg `json:"fields,omitempty"`
}

func (n *StructConstructorWithKeyword) Children() []Node {
	out := children(n.StructType)
	for _, f := range n.Fields {
		out = append(out, f)
	}
	return out
}

// NewConstructor is "NEW type_name(arg, ...)"; see ASTNewConstructor in
// parse_tree.h.
type NewConstructor struct {
	Span
	TypeName *SimpleType          `json:"type_name"`
	Args     []*NewConstructorArg `json:"args,omitempty"`
}

func (n *NewConstructor) Children() []Node {
	out := children(n.TypeName)
	for _, a := range n.Args {
		out = append(out, a)
	}
	return out
}

// NewConstructorArg is one "expression [AS identifier | AS (path)]" argument
// of a NEW constructor; see ASTNewConstructorArg in parse_tree.h. At most
// one of OptionalIdentifier and OptionalPathExpression is set.
type NewConstructorArg struct {
	Span
	Expression             Node            `json:"expression"`
	OptionalIdentifier     *Identifier     `json:"optional_identifier,omitempty"`
	OptionalPathExpression *PathExpression `json:"optional_path_expression,omitempty"`
}

func (n *NewConstructorArg) Children() []Node {
	return children(n.Expression, n.OptionalIdentifier, n.OptionalPathExpression)
}

// BracedNewConstructor is "NEW type_name { ... }"; see
// ASTBracedNewConstructor in parse_tree.h.
type BracedNewConstructor struct {
	Span
	TypeName    *SimpleType        `json:"type_name"`
	Constructor *BracedConstructor `json:"constructor"`
}

func (n *BracedNewConstructor) Children() []Node {
	return children(n.TypeName, n.Constructor)
}

// UpdateConstructor is "function_call { ... }", a proto UPDATE constructor
// consisting of a function call followed by a braced constructor; see
// ASTUpdateConstructor in parse_tree.h and the function_call_expression
// braced_constructor rule in googlesql.tm.
type UpdateConstructor struct {
	Span
	Function    Node               `json:"function"`
	Constructor *BracedConstructor `json:"constructor"`
}

func (n *UpdateConstructor) Children() []Node {
	return children(n.Function, n.Constructor)
}

// WithExpression is "WITH(var AS expr [, ...], result_expr)"; see
// ASTWithExpression in parse_tree.h and with_expression in googlesql.tm. The
// variable definitions are represented as a SelectList of SelectColumns
// (value AS alias) and Expr is the trailing result expression.
type WithExpression struct {
	Span
	Variables *SelectList `json:"variables"`
	Expr      Node        `json:"expr"`
}

func (n *WithExpression) Children() []Node {
	return children(n.Variables, n.Expr)
}

// BracedConstructor is a braced proto/struct constructor body
// "{ field [, field ...] }"; see ASTBracedConstructor in parse_tree.h.
type BracedConstructor struct {
	Span
	Fields []*BracedConstructorField `json:"fields,omitempty"`
}

func (n *BracedConstructor) Children() []Node {
	out := make([]Node, 0, len(n.Fields))
	for _, f := range n.Fields {
		out = append(out, f)
	}
	return out
}

// BracedConstructorField is one "lhs: value" or "lhs { ... }" entry of a
// braced constructor; see ASTBracedConstructorField in parse_tree.h.
// CommaSeparated records whether the field was preceded by a comma (fields
// may also be separated by whitespace only, proto text-format style).
type BracedConstructorField struct {
	Span
	Lhs            *BracedConstructorLhs        `json:"lhs"`
	Value          *BracedConstructorFieldValue `json:"value"`
	CommaSeparated bool                         `json:"comma_separated,omitempty"`
}

func (n *BracedConstructorField) Children() []Node {
	return children(n.Lhs, n.Value)
}

// BracedConstructorLhs is the field name of a braced constructor field: a
// path expression, or a parenthesized extension path "(path.to.extension)";
// see ASTBracedConstructorLhs in parse_tree.h.
type BracedConstructorLhs struct {
	Span
	Expression Node `json:"expression"`
}

func (n *BracedConstructorLhs) Children() []Node {
	return children(n.Expression)
}

// BracedConstructorFieldValue is the value of a braced constructor field;
// see ASTBracedConstructorFieldValue in parse_tree.h. ColonPrefixed is true
// for "lhs: value" and false for the sub-message form "lhs { ... }".
type BracedConstructorFieldValue struct {
	Span
	Expression    Node `json:"expression"`
	ColonPrefixed bool `json:"colon_prefixed,omitempty"`
}

func (n *BracedConstructorFieldValue) Children() []Node {
	return children(n.Expression)
}

// StructBracedConstructor is "STRUCT { ... }" or "STRUCT<...> { ... }"; see
// ASTStructBracedConstructor in parse_tree.h. StructType is nil for the
// typeless form.
type StructBracedConstructor struct {
	Span
	StructType  *StructType        `json:"struct_type,omitempty"`
	Constructor *BracedConstructor `json:"constructor"`
}

func (n *StructBracedConstructor) Children() []Node {
	return children(n.StructType, n.Constructor)
}

// FromQuery is a standalone FROM clause used as a query, e.g. "FROM t";
// see ASTFromQuery in googlesql/parser/parse_tree.h.
type FromQuery struct {
	Span
	From *FromClause `json:"from"`
}

func (n *FromQuery) Children() []Node {
	return children(n.From)
}

// Subpipeline is a parenthesized sequence of pipe operators, e.g.
// "(|> WHERE x)"; see ASTSubpipeline in googlesql/parser/parse_tree.h. The
// span includes the parentheses.
type Subpipeline struct {
	Span
	PipeOperators []Node `json:"pipe_operators,omitempty"`
}

func (n *Subpipeline) Children() []Node {
	return append([]Node(nil), n.PipeOperators...)
}

// SubpipelineStatement is a standalone subpipeline used as a statement (e.g.
// "|> WHERE x") or the pipe-operator suffix carried by a
// StatementWithPipeOperators; see ASTSubpipelineStatement in
// googlesql/parser/parse_tree.h.
type SubpipelineStatement struct {
	Span
	Subpipeline *Subpipeline `json:"subpipeline"`
}

func (n *SubpipelineStatement) statementNode() {}
func (n *SubpipelineStatement) Children() []Node {
	return children(n.Subpipeline)
}

// StatementWithPipeOperators is a statement followed by a subpipeline suffix,
// e.g. "SHOW tables |> WHERE x"; see ASTStatementWithPipeOperators in
// googlesql/parser/parse_tree.h.
type StatementWithPipeOperators struct {
	Span
	Statement  Statement             `json:"statement"`
	PipeSuffix *SubpipelineStatement `json:"pipe_suffix"`
}

func (n *StatementWithPipeOperators) statementNode() {}
func (n *StatementWithPipeOperators) Children() []Node {
	return children(n.Statement, n.PipeSuffix)
}

// PipeLog is a |> LOG pipe operator with an optional subpipeline.
type PipeLog struct {
	Span
	Subpipeline *Subpipeline `json:"subpipeline,omitempty"`
}

func (n *PipeLog) Children() []Node {
	return children(n.Subpipeline)
}

// PipeSelect is a |> SELECT pipe operator. The select list is represented
// as an ASTSelect for resolver code sharing in the reference implementation.
type PipeSelect struct {
	Span
	Select *Select `json:"select"`
}

func (n *PipeSelect) Children() []Node {
	return children(n.Select)
}

// PipeExtend is a |> EXTEND pipe operator. The selection item list is
// represented as an ASTSelect for resolver code sharing in the reference
// implementation.
type PipeExtend struct {
	Span
	Select *Select `json:"select"`
}

func (n *PipeExtend) Children() []Node {
	return children(n.Select)
}

// PipeWindow is a |> WINDOW pipe operator. The selection item list is
// represented as an ASTSelect for resolver code sharing in the reference
// implementation.
type PipeWindow struct {
	Span
	Select *Select `json:"select"`
}

func (n *PipeWindow) Children() []Node {
	return children(n.Select)
}

// PipeLimitOffset is a |> LIMIT [OFFSET] pipe operator.
type PipeLimitOffset struct {
	Span
	LimitOffset *LimitOffset `json:"limit_offset"`
}

func (n *PipeLimitOffset) Children() []Node {
	return children(n.LimitOffset)
}

// PipeDistinct is a |> DISTINCT pipe operator; it has no children.
type PipeDistinct struct {
	Span
}

func (n *PipeDistinct) Children() []Node { return nil }

// PipeAggregate is a |> AGGREGATE pipe operator. The aggregate list and
// optional GROUP BY are represented as an ASTSelect for resolver code
// sharing in the reference implementation.
type PipeAggregate struct {
	Span
	Select *Select `json:"select"`
}

func (n *PipeAggregate) Children() []Node {
	return children(n.Select)
}

// PipeWhere is a |> WHERE pipe operator.
type PipeWhere struct {
	Span
	Where *WhereClause `json:"where"`
}

func (n *PipeWhere) Children() []Node {
	return children(n.Where)
}

// PipeTablesample is a |> TABLESAMPLE pipe operator; see ASTPipeTablesample
// in googlesql/parser/parse_tree.h.
type PipeTablesample struct {
	Span
	Sample *SampleClause `json:"sample"`
}

func (n *PipeTablesample) Children() []Node {
	return children(n.Sample)
}

// PipeOrderBy is a |> ORDER BY pipe operator.
type PipeOrderBy struct {
	Span
	OrderBy *OrderBy `json:"order_by"`
}

func (n *PipeOrderBy) Children() []Node {
	return children(n.OrderBy)
}

// PipeSet is a |> SET pipe operator.
type PipeSet struct {
	Span
	Items []*PipeSetItem `json:"items"`
}

func (n *PipeSet) Children() []Node {
	var out []Node
	for _, item := range n.Items {
		out = append(out, item)
	}
	return out
}

// PipeSetItem is a single "column = expression" assignment in a pipe SET
// operator.
type PipeSetItem struct {
	Span
	Column *Identifier `json:"column"`
	Expr   Node        `json:"expr"`
}

func (n *PipeSetItem) Children() []Node {
	return children(n.Column, n.Expr)
}

// PipeAs is a |> AS pipe operator; see ASTPipeAs in
// googlesql/parser/parse_tree.h.
type PipeAs struct {
	Span
	Alias *Alias `json:"alias"`
}

func (n *PipeAs) Children() []Node {
	return children(n.Alias)
}

// PipeRename is a |> RENAME pipe operator; see ASTPipeRename in
// googlesql/parser/parse_tree.h.
type PipeRename struct {
	Span
	Items []*PipeRenameItem `json:"items"`
}

func (n *PipeRename) Children() []Node {
	out := make([]Node, 0, len(n.Items))
	for _, it := range n.Items {
		out = append(out, it)
	}
	return out
}

// PipeRenameItem is a single "old_name [AS] new_name" pair in a pipe RENAME
// operator; see ASTPipeRenameItem in googlesql/parser/parse_tree.h.
type PipeRenameItem struct {
	Span
	OldName *Identifier `json:"old_name"`
	NewName *Identifier `json:"new_name"`
}

func (n *PipeRenameItem) Children() []Node {
	return children(n.OldName, n.NewName)
}

// PipeDrop is a |> DROP pipe operator; see ASTPipeDrop in
// googlesql/parser/parse_tree.h.
type PipeDrop struct {
	Span
	ColumnList *IdentifierList `json:"column_list"`
}

func (n *PipeDrop) Children() []Node {
	return children(n.ColumnList)
}

// PipeAssert is a |> ASSERT pipe operator; the first expression is the
// asserted condition and the rest are message expressions. See ASTPipeAssert
// in googlesql/parser/parse_tree.h.
type PipeAssert struct {
	Span
	Exprs []Node `json:"exprs"`
}

func (n *PipeAssert) Children() []Node {
	return append([]Node(nil), n.Exprs...)
}

// PipeDescribe is a |> DESCRIBE pipe operator; see ASTPipeDescribe in
// googlesql/parser/parse_tree.h.
type PipeDescribe struct {
	Span
}

func (n *PipeDescribe) Children() []Node { return nil }

// PipeStaticDescribe is a |> STATIC_DESCRIBE pipe operator; see
// ASTPipeStaticDescribe in googlesql/parser/parse_tree.h.
type PipeStaticDescribe struct {
	Span
}

func (n *PipeStaticDescribe) Children() []Node { return nil }

// AlterStatement is an ALTER <object kind> statement. NodeName holds the
// per-object-kind parse tree node name (e.g. "AlterTableStatement",
// "AlterViewStatement"), matching the distinct ASTAlter*Statement node
// classes in the reference implementation.
type AlterStatement struct {
	Span
	NodeName   string           `json:"node_name"`
	IsIfExists bool             `json:"is_if_exists,omitempty"`
	EntityType *Identifier      `json:"entity_type,omitempty"`
	Path       *PathExpression  `json:"path,omitempty"`
	Actions    *AlterActionList `json:"actions"`
}

func (n *AlterStatement) statementNode() {}
func (n *AlterStatement) Children() []Node {
	return children(n.EntityType, n.Path, n.Actions)
}

// DropStatement is a DROP <object kind> statement. NodeName holds the
// per-object-kind parse tree node name: the generic "DropStatement" (which
// also shows ObjectKind, e.g. "TABLE" or "EXTERNAL SCHEMA"), or one of the
// distinct node classes "DropFunctionStatement",
// "DropTableFunctionStatement", "DropMaterializedViewStatement" and
// "DropSnapshotTableStatement", matching the reference implementation.
type DropStatement struct {
	Span
	NodeName   string          `json:"node_name"`
	ObjectKind string          `json:"object_kind,omitempty"`
	IsIfExists bool            `json:"is_if_exists,omitempty"`
	DropMode   string          `json:"drop_mode,omitempty"`
	Path       *PathExpression `json:"path"`
	// Parameters holds the optional function parameter list for
	// DropFunctionStatement (e.g. DROP FUNCTION foo(int32)); nil otherwise.
	Parameters *FunctionParameters `json:"parameters,omitempty"`
}

func (n *DropStatement) statementNode() {}
func (n *DropStatement) Children() []Node {
	return children(n.Path, n.Parameters)
}

// AlterActionList is the comma-separated list of actions in an ALTER
// statement.
type AlterActionList struct {
	Span
	Actions []Node `json:"actions"`
}

func (n *AlterActionList) Children() []Node {
	return append([]Node(nil), n.Actions...)
}

// RenameToClause is a RENAME TO <path> alter action.
type RenameToClause struct {
	Span
	NewName *PathExpression `json:"new_name"`
}

func (n *RenameToClause) Children() []Node {
	return children(n.NewName)
}

// AlterRowAccessPolicyStatement is
// "ALTER ROW ACCESS POLICY [IF EXISTS] name ON path alter_action_list"; see
// ASTAlterRowAccessPolicyStatement in googlesql/parser/parse_tree.h.
type AlterRowAccessPolicyStatement struct {
	Span
	IsIfExists bool             `json:"is_if_exists,omitempty"`
	Name       *Identifier      `json:"name"`
	Path       *PathExpression  `json:"path"`
	Actions    *AlterActionList `json:"actions"`
}

func (n *AlterRowAccessPolicyStatement) statementNode() {}
func (n *AlterRowAccessPolicyStatement) Children() []Node {
	return children(n.Name, n.Path, n.Actions)
}

// AlterAllRowAccessPoliciesStatement is
// "ALTER ALL ROW ACCESS POLICIES ON path revoke_from_clause"; see
// ASTAlterAllRowAccessPoliciesStatement in googlesql/parser/parse_tree.h.
type AlterAllRowAccessPoliciesStatement struct {
	Span
	Path   *PathExpression   `json:"path"`
	Revoke *RevokeFromClause `json:"revoke"`
}

func (n *AlterAllRowAccessPoliciesStatement) statementNode() {}
func (n *AlterAllRowAccessPoliciesStatement) Children() []Node {
	return children(n.Path, n.Revoke)
}

// DropRowAccessPolicyStatement is a
// "DROP ROW ACCESS POLICY [IF EXISTS] name ON target" statement; see
// ASTDropRowAccessPolicyStatement in googlesql/parser/parse_tree.h.
type DropRowAccessPolicyStatement struct {
	Span
	IsIfExists bool            `json:"is_if_exists,omitempty"`
	Name       *PathExpression `json:"name"`
	Target     *PathExpression `json:"target"`
}

func (n *DropRowAccessPolicyStatement) statementNode() {}
func (n *DropRowAccessPolicyStatement) Children() []Node {
	return children(n.Name, n.Target)
}

// DropAllRowAccessPoliciesStatement is a
// "DROP ALL ROW [ACCESS] POLICIES ON target" statement; see
// ASTDropAllRowAccessPoliciesStatement in googlesql/parser/parse_tree.h.
type DropAllRowAccessPoliciesStatement struct {
	Span
	HasAccessKeyword bool            `json:"has_access_keyword,omitempty"`
	Target           *PathExpression `json:"target"`
}

func (n *DropAllRowAccessPoliciesStatement) statementNode() {}
func (n *DropAllRowAccessPoliciesStatement) Children() []Node {
	return children(n.Target)
}

// DescribeStatement is a "DESCRIBE [object_type] name [FROM path]" statement;
// see ASTDescribeStatement in googlesql/parser/parse_tree.h. ObjectType is the
// optional leading kind identifier (e.g. INDEX, FUNCTION, TYPE, COLUMN).
type DescribeStatement struct {
	Span
	ObjectType   *Identifier     `json:"object_type,omitempty"`
	Name         *PathExpression `json:"name"`
	OptionalFrom *PathExpression `json:"optional_from,omitempty"`
}

func (n *DescribeStatement) statementNode() {}
func (n *DescribeStatement) Children() []Node {
	return children(n.ObjectType, n.Name, n.OptionalFrom)
}

// ShowStatement is a "SHOW target [FROM path] [LIKE 'pattern']" statement; see
// ASTShowStatement in googlesql/parser/parse_tree.h.
type ShowStatement struct {
	Span
	Target       *Identifier     `json:"target"`
	OptionalName *PathExpression `json:"optional_name,omitempty"`
	Like         Node            `json:"like,omitempty"`
}

func (n *ShowStatement) statementNode() {}
func (n *ShowStatement) Children() []Node {
	return children(n.Target, n.OptionalName, n.Like)
}

// GrantStatement is a "GRANT privileges ON [type] name TO grantees" statement;
// see ASTGrantStatement in googlesql/parser/parse_tree.h. ObjectTypes holds the
// zero, one, or two leading object-type identifiers (e.g. "table", or
// "materialized view").
type GrantStatement struct {
	Span
	Privileges  *Privileges     `json:"privileges"`
	ObjectTypes []*Identifier   `json:"object_types,omitempty"`
	Path        *PathExpression `json:"path"`
	Grantees    *GranteeList    `json:"grantees"`
}

func (n *GrantStatement) statementNode() {}
func (n *GrantStatement) Children() []Node {
	out := []Node{n.Privileges}
	for _, id := range n.ObjectTypes {
		out = append(out, id)
	}
	out = append(out, n.Path, n.Grantees)
	return out
}

// RevokeStatement is a "REVOKE privileges ON [type] name FROM grantees"
// statement; see ASTRevokeStatement in googlesql/parser/parse_tree.h.
type RevokeStatement struct {
	Span
	Privileges  *Privileges     `json:"privileges"`
	ObjectTypes []*Identifier   `json:"object_types,omitempty"`
	Path        *PathExpression `json:"path"`
	Grantees    *GranteeList    `json:"grantees"`
}

func (n *RevokeStatement) statementNode() {}
func (n *RevokeStatement) Children() []Node {
	out := []Node{n.Privileges}
	for _, id := range n.ObjectTypes {
		out = append(out, id)
	}
	out = append(out, n.Path, n.Grantees)
	return out
}

// Privileges is a list of privileges in a GRANT/REVOKE statement; see
// ASTPrivileges in googlesql/parser/parse_tree.h. An empty Privileges (no
// children) represents "ALL [PRIVILEGES]".
type Privileges struct {
	Span
	Privileges []*Privilege `json:"privileges,omitempty"`
}

func (n *Privileges) Children() []Node {
	out := make([]Node, 0, len(n.Privileges))
	for _, p := range n.Privileges {
		out = append(out, p)
	}
	return out
}

// Privilege is a single privilege (a name and an optional parenthesized column
// list) in a GRANT/REVOKE statement; see ASTPrivilege in
// googlesql/parser/parse_tree.h.
type Privilege struct {
	Span
	Name    *Identifier         `json:"name"`
	Columns *PathExpressionList `json:"columns,omitempty"`
}

func (n *Privilege) Children() []Node {
	return children(n.Name, n.Columns)
}

// GrantToClause is a "GRANT TO (grantee_list)" row access policy alter action;
// see ASTGrantToClause in googlesql/parser/parse_tree.h.
type GrantToClause struct {
	Span
	Grantees *GranteeList `json:"grantees"`
}

func (n *GrantToClause) Children() []Node {
	return children(n.Grantees)
}

// RevokeFromClause is a "REVOKE FROM (grantee_list)" or "REVOKE FROM ALL" row
// access policy alter action; see ASTRevokeFromClause in
// googlesql/parser/parse_tree.h.
type RevokeFromClause struct {
	Span
	Grantees        *GranteeList `json:"grantees,omitempty"`
	IsRevokeFromAll bool         `json:"is_revoke_from_all,omitempty"`
}

func (n *RevokeFromClause) Children() []Node {
	return children(n.Grantees)
}

// FilterUsingClause is a "FILTER USING (expr)" row access policy alter action;
// see ASTFilterUsingClause in googlesql/parser/parse_tree.h.
type FilterUsingClause struct {
	Span
	Predicate Node `json:"predicate"`
}

func (n *FilterUsingClause) Children() []Node {
	return children(n.Predicate)
}

// GranteeList is a parenthesized comma-separated list of grantees (string
// literals, parameters, or system variables); see ASTGranteeList in
// googlesql/parser/parse_tree.h.
type GranteeList struct {
	Span
	Grantees []Node `json:"grantees"`
}

func (n *GranteeList) Children() []Node {
	return append([]Node(nil), n.Grantees...)
}

// SetOptionsAction is a SET OPTIONS (...) alter action.
type SetOptionsAction struct {
	Span
	Options *OptionsList `json:"options"`
}

func (n *SetOptionsAction) Children() []Node {
	return children(n.Options)
}

// SetAsAction is a "SET AS generic_entity_body" alter action, where the body
// is a JSON literal or a text (string) literal; see ASTSetAsAction in
// googlesql/parser/parse_tree.h.
type SetAsAction struct {
	Span
	JSONBody Node `json:"json_body,omitempty"`
	TextBody Node `json:"text_body,omitempty"`
}

func (n *SetAsAction) Children() []Node {
	return children(n.JSONBody, n.TextBody)
}

// AddSubEntityAction is an "ADD generic_sub_entity_type [IF NOT EXISTS]
// identifier [OPTIONS(...)]" alter action; see ASTAddSubEntityAction in
// googlesql/parser/parse_tree.h.
type AddSubEntityAction struct {
	Span
	IsIfNotExists bool         `json:"is_if_not_exists,omitempty"`
	Type          *Identifier  `json:"type"`
	Name          *Identifier  `json:"name"`
	Options       *OptionsList `json:"options,omitempty"`
}

func (n *AddSubEntityAction) Children() []Node {
	return children(n.Type, n.Name, n.Options)
}

// AlterSubEntityAction is an "ALTER generic_sub_entity_type [IF EXISTS]
// identifier alter_action" alter action; see ASTAlterSubEntityAction in
// googlesql/parser/parse_tree.h.
type AlterSubEntityAction struct {
	Span
	IsIfExists bool        `json:"is_if_exists,omitempty"`
	Type       *Identifier `json:"type"`
	Name       *Identifier `json:"name"`
	Action     Node        `json:"action"`
}

func (n *AlterSubEntityAction) Children() []Node {
	return children(n.Type, n.Name, n.Action)
}

// DropSubEntityAction is a "DROP generic_sub_entity_type [IF EXISTS]
// identifier" alter action; see ASTDropSubEntityAction in
// googlesql/parser/parse_tree.h.
type DropSubEntityAction struct {
	Span
	IsIfExists bool        `json:"is_if_exists,omitempty"`
	Type       *Identifier `json:"type"`
	Name       *Identifier `json:"name"`
}

func (n *DropSubEntityAction) Children() []Node {
	return children(n.Type, n.Name)
}

// CreateEntityStatement is a "CREATE [OR REPLACE] generic_entity_type
// [IF NOT EXISTS] path [OPTIONS(...)] [AS generic_entity_body]" statement; see
// ASTCreateEntityStatement in googlesql/parser/parse_tree.h.
type CreateEntityStatement struct {
	Span
	IsOrReplace   bool            `json:"is_or_replace,omitempty"`
	IsIfNotExists bool            `json:"is_if_not_exists,omitempty"`
	Type          *Identifier     `json:"type"`
	Name          *PathExpression `json:"name"`
	Options       *OptionsList    `json:"options,omitempty"`
	JSONBody      Node            `json:"json_body,omitempty"`
	TextBody      Node            `json:"text_body,omitempty"`
}

func (n *CreateEntityStatement) statementNode() {}
func (n *CreateEntityStatement) Children() []Node {
	return children(n.Type, n.Name, n.Options, n.JSONBody, n.TextBody)
}

// DropEntityStatement is a "DROP generic_entity_type [IF EXISTS] path"
// statement; see ASTDropEntityStatement in googlesql/parser/parse_tree.h.
type DropEntityStatement struct {
	Span
	IsIfExists bool            `json:"is_if_exists,omitempty"`
	EntityType *Identifier     `json:"entity_type"`
	Name       *PathExpression `json:"name"`
}

func (n *DropEntityStatement) statementNode() {}
func (n *DropEntityStatement) Children() []Node {
	return children(n.EntityType, n.Name)
}

// AddColumnAction is an "ADD COLUMN [IF NOT EXISTS] column_definition
// [column_position] [FILL USING expression]" alter action; see
// ASTAddColumnAction in googlesql/parser/parse_tree.h.
type AddColumnAction struct {
	Span
	IsIfNotExists  bool              `json:"is_if_not_exists,omitempty"`
	Column         *ColumnDefinition `json:"column"`
	Position       *ColumnPosition   `json:"position,omitempty"`
	FillExpression Node              `json:"fill_expression,omitempty"`
}

func (n *AddColumnAction) Children() []Node {
	return children(n.Column, n.Position, n.FillExpression)
}

// ColumnPosition is a "PRECEDING identifier" or "FOLLOWING identifier" clause
// on an ADD COLUMN action; see ASTColumnPosition in
// googlesql/parser/parse_tree.h. Type is "PRECEDING" or "FOLLOWING".
type ColumnPosition struct {
	Span
	Type       string      `json:"type"`
	Identifier *Identifier `json:"identifier"`
}

func (n *ColumnPosition) Children() []Node {
	return children(n.Identifier)
}

// SpannerAlterColumnAction is a Cloud Spanner "ALTER COLUMN identifier
// column_schema [NOT NULL] [DEFAULT|AS ...] [OPTIONS(...)]" alter action; see
// ASTSpannerAlterColumnAction in googlesql/parser/parse_tree.h. It is gated by
// FEATURE_SPANNER_LEGACY_DDL.
type SpannerAlterColumnAction struct {
	Span
	Column *ColumnDefinition `json:"column"`
}

func (n *SpannerAlterColumnAction) Children() []Node {
	return children(n.Column)
}

// SpannerSetOnDeleteAction is a Cloud Spanner "SET ON DELETE {CASCADE|NO
// ACTION}" alter action; see ASTSpannerSetOnDeleteAction in
// googlesql/parser/parse_tree.h. Action is the referential action string; it
// is not rendered in the debug string. Gated by FEATURE_SPANNER_LEGACY_DDL.
type SpannerSetOnDeleteAction struct {
	Span
	Action string `json:"action"`
}

func (n *SpannerSetOnDeleteAction) Children() []Node { return nil }

// DropColumnAction is a "DROP COLUMN [IF EXISTS] identifier" alter action; see
// ASTDropColumnAction in googlesql/parser/parse_tree.h.
type DropColumnAction struct {
	Span
	IsIfExists bool        `json:"is_if_exists,omitempty"`
	Name       *Identifier `json:"name"`
}

func (n *DropColumnAction) Children() []Node {
	return children(n.Name)
}

// RenameColumnAction is a "RENAME COLUMN [IF EXISTS] identifier TO identifier"
// alter action; see ASTRenameColumnAction in googlesql/parser/parse_tree.h.
type RenameColumnAction struct {
	Span
	IsIfExists bool        `json:"is_if_exists,omitempty"`
	Name       *Identifier `json:"name"`
	NewName    *Identifier `json:"new_name"`
}

func (n *RenameColumnAction) Children() []Node {
	return children(n.Name, n.NewName)
}

// AddConstraintAction is an "ADD [CONSTRAINT [IF NOT EXISTS] name]
// constraint_spec" alter action; see ASTAddConstraintAction in
// googlesql/parser/parse_tree.h. Constraint is a CheckConstraint, PrimaryKey,
// or ForeignKey node.
type AddConstraintAction struct {
	Span
	IsIfNotExists bool `json:"is_if_not_exists,omitempty"`
	Constraint    Node `json:"constraint"`
}

func (n *AddConstraintAction) Children() []Node {
	return children(n.Constraint)
}

// DropConstraintAction is a "DROP CONSTRAINT [IF EXISTS] identifier" alter
// action; see ASTDropConstraintAction in googlesql/parser/parse_tree.h.
type DropConstraintAction struct {
	Span
	IsIfExists bool        `json:"is_if_exists,omitempty"`
	Name       *Identifier `json:"name"`
}

func (n *DropConstraintAction) Children() []Node {
	return children(n.Name)
}

// DropPrimaryKeyAction is a "DROP PRIMARY KEY [IF EXISTS]" alter action; see
// ASTDropPrimaryKeyAction in googlesql/parser/parse_tree.h.
type DropPrimaryKeyAction struct {
	Span
	IsIfExists bool `json:"is_if_exists,omitempty"`
}

func (n *DropPrimaryKeyAction) Children() []Node { return nil }

// AlterConstraintEnforcementAction is an "ALTER CONSTRAINT [IF EXISTS]
// identifier {ENFORCED|NOT ENFORCED}" alter action; see
// ASTAlterConstraintEnforcementAction in googlesql/parser/parse_tree.h.
type AlterConstraintEnforcementAction struct {
	Span
	IsIfExists bool        `json:"is_if_exists,omitempty"`
	IsEnforced bool        `json:"is_enforced,omitempty"`
	Name       *Identifier `json:"name"`
}

func (n *AlterConstraintEnforcementAction) Children() []Node {
	return children(n.Name)
}

// AlterConstraintSetOptionsAction is an "ALTER CONSTRAINT [IF EXISTS]
// identifier SET OPTIONS (...)" alter action; see
// ASTAlterConstraintSetOptionsAction in googlesql/parser/parse_tree.h.
type AlterConstraintSetOptionsAction struct {
	Span
	IsIfExists bool         `json:"is_if_exists,omitempty"`
	Name       *Identifier  `json:"name"`
	Options    *OptionsList `json:"options"`
}

func (n *AlterConstraintSetOptionsAction) Children() []Node {
	return children(n.Name, n.Options)
}

// AlterColumnSetDefaultAction is an "ALTER COLUMN [IF EXISTS] identifier SET
// DEFAULT expression" alter action; see ASTAlterColumnSetDefaultAction in
// googlesql/parser/parse_tree.h.
type AlterColumnSetDefaultAction struct {
	Span
	IsIfExists        bool        `json:"is_if_exists,omitempty"`
	Column            *Identifier `json:"column"`
	DefaultExpression Node        `json:"default_expression"`
}

func (n *AlterColumnSetDefaultAction) Children() []Node {
	return children(n.Column, n.DefaultExpression)
}

// AlterColumnDropDefaultAction is an "ALTER COLUMN [IF EXISTS] identifier DROP
// DEFAULT" alter action; see ASTAlterColumnDropDefaultAction in
// googlesql/parser/parse_tree.h.
type AlterColumnDropDefaultAction struct {
	Span
	IsIfExists bool        `json:"is_if_exists,omitempty"`
	Column     *Identifier `json:"column"`
}

func (n *AlterColumnDropDefaultAction) Children() []Node {
	return children(n.Column)
}

// AlterColumnDropGeneratedAction is an "ALTER COLUMN [IF EXISTS] identifier
// DROP GENERATED" alter action; see ASTAlterColumnDropGeneratedAction in
// googlesql/parser/parse_tree.h.
type AlterColumnDropGeneratedAction struct {
	Span
	IsIfExists bool        `json:"is_if_exists,omitempty"`
	Column     *Identifier `json:"column"`
}

func (n *AlterColumnDropGeneratedAction) Children() []Node {
	return children(n.Column)
}

// AlterColumnDropNotNullAction is an "ALTER COLUMN [IF EXISTS] identifier DROP
// NOT NULL" alter action; see ASTAlterColumnDropNotNullAction in
// googlesql/parser/parse_tree.h.
type AlterColumnDropNotNullAction struct {
	Span
	IsIfExists bool        `json:"is_if_exists,omitempty"`
	Column     *Identifier `json:"column"`
}

func (n *AlterColumnDropNotNullAction) Children() []Node {
	return children(n.Column)
}

// AlterColumnSetGeneratedAction is an "ALTER COLUMN [IF EXISTS] identifier SET
// GENERATED generated_column_info" alter action; see
// ASTAlterColumnSetGeneratedAction in googlesql/parser/parse_tree.h.
type AlterColumnSetGeneratedAction struct {
	Span
	IsIfExists bool                 `json:"is_if_exists,omitempty"`
	Column     *Identifier          `json:"column"`
	Info       *GeneratedColumnInfo `json:"info"`
}

func (n *AlterColumnSetGeneratedAction) Children() []Node {
	return children(n.Column, n.Info)
}

// AlterColumnOptionsAction is an "ALTER COLUMN [IF EXISTS] identifier SET
// OPTIONS options_list" alter action; see ASTAlterColumnOptionsAction in
// googlesql/parser/parse_tree.h.
type AlterColumnOptionsAction struct {
	Span
	IsIfExists bool         `json:"is_if_exists,omitempty"`
	Column     *Identifier  `json:"column"`
	Options    *OptionsList `json:"options"`
}

func (n *AlterColumnOptionsAction) Children() []Node {
	return children(n.Column, n.Options)
}

// AlterColumnTypeAction is an "ALTER COLUMN [IF EXISTS] identifier SET DATA
// TYPE field_schema" alter action; see ASTAlterColumnTypeAction in
// googlesql/parser/parse_tree.h. Collate is a separate top-level collation
// clause, distinct from any collation inside the schema.
type AlterColumnTypeAction struct {
	Span
	IsIfExists bool        `json:"is_if_exists,omitempty"`
	Column     *Identifier `json:"column"`
	Schema     Node        `json:"schema"`
	Collate    *Collate    `json:"collate,omitempty"`
}

func (n *AlterColumnTypeAction) Children() []Node {
	return children(n.Column, n.Schema, n.Collate)
}

// GeneratedColumnInfo describes a generated column: either "AS (expression)
// [STORED [VOLATILE]]" or an identity column. GeneratedMode is "ALWAYS" or
// "BY_DEFAULT"; StoredMode is "", "STORED", or "STORED_VOLATILE". See
// ASTGeneratedColumnInfo in googlesql/parser/parse_tree.h.
type GeneratedColumnInfo struct {
	Span
	GeneratedMode string              `json:"generated_mode"`
	StoredMode    string              `json:"stored_mode,omitempty"`
	Expression    Node                `json:"expression,omitempty"`
	Identity      *IdentityColumnInfo `json:"identity,omitempty"`
}

func (n *GeneratedColumnInfo) Children() []Node {
	return children(n.Expression, n.Identity)
}

// IdentityColumnInfo is the "IDENTITY(...)" body of a generated identity
// column; see ASTIdentityColumnInfo in googlesql/parser/parse_tree.h. The
// sequence attributes appear as child nodes in source order; the CYCLE flag is
// not represented as a child.
type IdentityColumnInfo struct {
	Span
	StartWith   *IdentityColumnStartWith   `json:"start_with,omitempty"`
	IncrementBy *IdentityColumnIncrementBy `json:"increment_by,omitempty"`
	MaxValue    *IdentityColumnMaxValue    `json:"max_value,omitempty"`
	MinValue    *IdentityColumnMinValue    `json:"min_value,omitempty"`
}

func (n *IdentityColumnInfo) Children() []Node {
	return children(n.StartWith, n.IncrementBy, n.MaxValue, n.MinValue)
}

// IdentityColumnStartWith is the "START WITH value" clause of an identity
// column; see ASTIdentityColumnStartWith in googlesql/parser/parse_tree.h.
type IdentityColumnStartWith struct {
	Span
	Value Node `json:"value"`
}

func (n *IdentityColumnStartWith) Children() []Node { return children(n.Value) }

// IdentityColumnIncrementBy is the "INCREMENT BY value" clause of an identity
// column; see ASTIdentityColumnIncrementBy in googlesql/parser/parse_tree.h.
type IdentityColumnIncrementBy struct {
	Span
	Value Node `json:"value"`
}

func (n *IdentityColumnIncrementBy) Children() []Node { return children(n.Value) }

// IdentityColumnMaxValue is the "MAXVALUE value" clause of an identity column;
// see ASTIdentityColumnMaxValue in googlesql/parser/parse_tree.h.
type IdentityColumnMaxValue struct {
	Span
	Value Node `json:"value"`
}

func (n *IdentityColumnMaxValue) Children() []Node { return children(n.Value) }

// IdentityColumnMinValue is the "MINVALUE value" clause of an identity column;
// see ASTIdentityColumnMinValue in googlesql/parser/parse_tree.h.
type IdentityColumnMinValue struct {
	Span
	Value Node `json:"value"`
}

func (n *IdentityColumnMinValue) Children() []Node { return children(n.Value) }

// AddTtlAction is an "ADD ROW DELETION POLICY [IF NOT EXISTS] (expression)"
// alter action; see ASTAddTtlAction in googlesql/parser/parse_tree.h.
type AddTtlAction struct {
	Span
	IsIfNotExists bool `json:"is_if_not_exists,omitempty"`
	Expression    Node `json:"expression"`
}

func (n *AddTtlAction) Children() []Node {
	return children(n.Expression)
}

// ReplaceTtlAction is a "REPLACE ROW DELETION POLICY [IF EXISTS] (expression)"
// alter action; see ASTReplaceTtlAction in googlesql/parser/parse_tree.h.
type ReplaceTtlAction struct {
	Span
	IsIfExists bool `json:"is_if_exists,omitempty"`
	Expression Node `json:"expression"`
}

func (n *ReplaceTtlAction) Children() []Node {
	return children(n.Expression)
}

// DropTtlAction is a "DROP ROW DELETION POLICY [IF EXISTS]" alter action; see
// ASTDropTtlAction in googlesql/parser/parse_tree.h.
type DropTtlAction struct {
	Span
	IsIfExists bool `json:"is_if_exists,omitempty"`
}

func (n *DropTtlAction) Children() []Node { return nil }

// OptionsList is a parenthesized list of name = value options.
type OptionsList struct {
	Span
	Entries []*OptionsEntry `json:"entries"`
}

func (n *OptionsList) Children() []Node {
	var out []Node
	for _, e := range n.Entries {
		out = append(out, e)
	}
	return out
}

// OptionsEntry is a single "name <op> value" entry in an options list. Op is
// "=", "+=", or "-=".
type OptionsEntry struct {
	Span
	Name  *Identifier `json:"name"`
	Op    string      `json:"op"`
	Value Node        `json:"value"`
}

func (n *OptionsEntry) Children() []Node {
	return children(n.Name, n.Value)
}

// AnalyzeStatement is "ANALYZE [OPTIONS(...)] [table_and_column_info, ...]";
// see ASTAnalyzeStatement in googlesql/parser/parse_tree.h.
type AnalyzeStatement struct {
	Span
	Options   *OptionsList            `json:"options,omitempty"`
	TableInfo *TableAndColumnInfoList `json:"table_info,omitempty"`
}

func (n *AnalyzeStatement) statementNode() {}
func (n *AnalyzeStatement) Children() []Node {
	var out []Node
	if n.Options != nil {
		out = append(out, n.Options)
	}
	if n.TableInfo != nil {
		out = append(out, n.TableInfo)
	}
	return out
}

// TableAndColumnInfoList is the list of table (and optional column) targets in
// an ANALYZE statement; see ASTTableAndColumnInfoList in
// googlesql/parser/parse_tree.h.
type TableAndColumnInfoList struct {
	Span
	Infos []*TableAndColumnInfo `json:"infos"`
}

func (n *TableAndColumnInfoList) Children() []Node {
	var out []Node
	for _, i := range n.Infos {
		out = append(out, i)
	}
	return out
}

// TableAndColumnInfo is a single "table [column_list]" target in an ANALYZE
// statement; see ASTTableAndColumnInfo in googlesql/parser/parse_tree.h.
type TableAndColumnInfo struct {
	Span
	Table   *PathExpression `json:"table"`
	Columns *ColumnList     `json:"columns,omitempty"`
}

func (n *TableAndColumnInfo) Children() []Node {
	out := children(n.Table)
	if n.Columns != nil {
		out = append(out, n.Columns)
	}
	return out
}

// AssertStatement is "ASSERT expression [AS description]"; see
// ASTAssertStatement in googlesql/parser/parse_tree.h.
type AssertStatement struct {
	Span
	Expression  Node           `json:"expression"`
	Description *StringLiteral `json:"description,omitempty"`
}

func (n *AssertStatement) statementNode() {}
func (n *AssertStatement) Children() []Node {
	out := children(n.Expression)
	if n.Description != nil {
		out = append(out, n.Description)
	}
	return out
}

// ReplaceFieldsExpression is "REPLACE_FIELDS(expression, arg [, ...])"; see
// ASTReplaceFieldsExpression in googlesql/parser/parse_tree.h.
type ReplaceFieldsExpression struct {
	Span
	Expr Node                `json:"expr"`
	Args []*ReplaceFieldsArg `json:"args"`
}

func (n *ReplaceFieldsExpression) Children() []Node {
	out := children(n.Expr)
	for _, a := range n.Args {
		out = append(out, a)
	}
	return out
}

// ReplaceFieldsArg is a single "expression AS generalized_path" argument to
// REPLACE_FIELDS; see ASTReplaceFieldsArg in googlesql/parser/parse_tree.h.
type ReplaceFieldsArg struct {
	Span
	Value Node `json:"value"`
	Path  Node `json:"path"`
}

func (n *ReplaceFieldsArg) Children() []Node {
	return children(n.Value, n.Path)
}

// ExpressionSubquery is a subquery used as an expression: "( query )",
// "ARRAY( query )", or "EXISTS( query )"; see ASTExpressionSubquery in
// googlesql/parser/parse_tree.h. The span includes the parentheses (and the
// modifier keyword, if any); the inner query keeps the span of the query
// text inside the parentheses. Modifier is "", "ARRAY", "EXISTS", or
// "VALUE".
type ExpressionSubquery struct {
	Span
	Modifier string `json:"modifier,omitempty"`
	Hint     *Hint  `json:"hint,omitempty"`
	Query    *Query `json:"query"`
}

func (n *ExpressionSubquery) Children() []Node {
	return children(n.Hint, n.Query)
}

// CaseValueExpression is "CASE value WHEN ... THEN ... [ELSE ...] END"; see
// ASTCaseValueExpression in googlesql/parser/parse_tree.h. Arguments holds
// the value expression, then alternating WHEN/THEN expression pairs, then
// the optional ELSE expression.
type CaseValueExpression struct {
	Span
	Arguments []Node `json:"arguments"`
}

func (n *CaseValueExpression) Children() []Node {
	return children(n.Arguments...)
}

// CaseNoValueExpression is "CASE WHEN ... THEN ... [ELSE ...] END"; see
// ASTCaseNoValueExpression in googlesql/parser/parse_tree.h. Arguments holds
// alternating WHEN/THEN expression pairs, then the optional ELSE expression.
type CaseNoValueExpression struct {
	Span
	Arguments []Node `json:"arguments"`
}

func (n *CaseNoValueExpression) Children() []Node {
	return children(n.Arguments...)
}

// DotIdentifier is "expression . identifier" where the expression is not a
// plain path expression (e.g. a parenthesized expression or a CASE
// expression); see ASTDotIdentifier in googlesql/parser/parse_tree.h.
type DotIdentifier struct {
	Span
	Expr Node        `json:"expr"`
	Name *Identifier `json:"name"`
}

func (n *DotIdentifier) Children() []Node {
	return children(n.Expr, n.Name)
}

// ArrayElement is "expression [ position ]"; see ASTArrayElement in
// googlesql/parser/parse_tree.h. BracketLocation covers the "[" token.
type ArrayElement struct {
	Span
	Array           Node      `json:"array"`
	BracketLocation *Location `json:"bracket_location"`
	Position        Node      `json:"position"`
}

func (n *ArrayElement) Children() []Node {
	return children(n.Array, n.BracketLocation, n.Position)
}

// CreateTableStatement is a CREATE TABLE statement, optionally with an AS
// query; see ASTCreateTableStatement in googlesql/parser/parse_tree.h.
// Scope is "", "TEMP", "PUBLIC", or "PRIVATE" (TEMPORARY normalizes to
// "TEMP").
type CreateTableStatement struct {
	Span
	Scope            string                `json:"scope,omitempty"`
	IsOrReplace      bool                  `json:"is_or_replace,omitempty"`
	IsIfNotExists    bool                  `json:"is_if_not_exists,omitempty"`
	Name             *PathExpression       `json:"name"`
	TableElementList *TableElementList     `json:"table_element_list,omitempty"`
	LikeName         *PathExpression       `json:"like_name,omitempty"`
	SpannerOptions   *SpannerTableOptions  `json:"spanner_options,omitempty"`
	Clone            *CloneDataSource      `json:"clone,omitempty"`
	PartitionBy      *PartitionBy          `json:"partition_by,omitempty"`
	ClusterBy        *ClusterBy            `json:"cluster_by,omitempty"`
	WithConnection   *WithConnectionClause `json:"with_connection,omitempty"`
	Options          *OptionsList          `json:"options,omitempty"`
	Query            *Query                `json:"query,omitempty"`
}

func (n *CreateTableStatement) statementNode() {}
func (n *CreateTableStatement) Children() []Node {
	return children(n.Name, n.TableElementList, n.LikeName, n.SpannerOptions, n.Clone,
		n.PartitionBy, n.ClusterBy, n.WithConnection, n.Options, n.Query)
}

// SpannerTableOptions holds the Cloud Spanner "PRIMARY KEY (...) [, INTERLEAVE
// IN PARENT ...]" options that follow the table element list in a CREATE TABLE
// statement; see ASTSpannerTableOptions in googlesql/parser/parse_tree.h. It is
// gated by FEATURE_SPANNER_LEGACY_DDL.
type SpannerTableOptions struct {
	Span
	PrimaryKey *PrimaryKey              `json:"primary_key"`
	Interleave *SpannerInterleaveClause `json:"interleave,omitempty"`
}

func (n *SpannerTableOptions) Children() []Node {
	return children(n.PrimaryKey, n.Interleave)
}

// SpannerInterleaveClause is a ", INTERLEAVE IN [PARENT] path [ON DELETE ...]"
// clause used by Cloud Spanner CREATE TABLE options and CREATE INDEX; see
// ASTSpannerInterleaveClause in googlesql/parser/parse_tree.h. Type is "IN" or
// "IN_PARENT"; Action is the ON DELETE referential action (IN_PARENT only).
// Neither is rendered in the debug string.
type SpannerInterleaveClause struct {
	Span
	TableName *PathExpression `json:"table_name"`
	Type      string          `json:"type"`
	Action    string          `json:"action,omitempty"`
}

func (n *SpannerInterleaveClause) Children() []Node {
	return children(n.TableName)
}

// ExportDataStatement is an EXPORT DATA statement, or the inner node of a
// |> EXPORT DATA pipe operator; see ASTExportDataStatement in
// googlesql/parser/parse_tree.h. Query is present only for the statement form.
type ExportDataStatement struct {
	Span
	WithConnection *WithConnectionClause `json:"with_connection,omitempty"`
	Options        *OptionsList          `json:"options,omitempty"`
	Query          *Query                `json:"query,omitempty"`
}

func (n *ExportDataStatement) statementNode() {}
func (n *ExportDataStatement) Children() []Node {
	return children(n.WithConnection, n.Options, n.Query)
}

// ExportModelStatement is an "EXPORT MODEL path [WITH CONNECTION ...]
// [OPTIONS(...)]" statement; see ASTExportModelStatement in
// googlesql/parser/parse_tree.h.
type ExportModelStatement struct {
	Span
	Name           *PathExpression       `json:"name"`
	WithConnection *WithConnectionClause `json:"with_connection,omitempty"`
	Options        *OptionsList          `json:"options,omitempty"`
}

func (n *ExportModelStatement) statementNode() {}
func (n *ExportModelStatement) Children() []Node {
	return children(n.Name, n.WithConnection, n.Options)
}

// CreateExternalTableStatement is a CREATE EXTERNAL TABLE statement; see
// ASTCreateExternalTableStatement in googlesql/parser/parse_tree.h. Scope is
// "", "TEMP", "PUBLIC", or "PRIVATE".
type CreateExternalTableStatement struct {
	Span
	Scope            string                      `json:"scope,omitempty"`
	IsOrReplace      bool                        `json:"is_or_replace,omitempty"`
	IsIfNotExists    bool                        `json:"is_if_not_exists,omitempty"`
	Name             *PathExpression             `json:"name"`
	TableElementList *TableElementList           `json:"table_element_list,omitempty"`
	WithPartition    *WithPartitionColumnsClause `json:"with_partition_columns,omitempty"`
	WithConnection   *WithConnectionClause       `json:"with_connection,omitempty"`
	Options          *OptionsList                `json:"options"`
}

func (n *CreateExternalTableStatement) statementNode() {}
func (n *CreateExternalTableStatement) Children() []Node {
	return children(n.Name, n.TableElementList, n.WithPartition, n.WithConnection, n.Options)
}

// TableElementList is the parenthesized list of column definitions and table
// constraints in a CREATE TABLE-family statement; see ASTTableElementList in
// googlesql/parser/parse_tree.h. Its span runs from the opening to the
// closing parenthesis.
type TableElementList struct {
	Span
	Elements []Node `json:"elements,omitempty"`
}

func (n *TableElementList) Children() []Node {
	return append([]Node(nil), n.Elements...)
}

// ColumnDefinition is "identifier column_schema [attributes] [OPTIONS(...)]" in
// a table element list; see ASTColumnDefinition in
// googlesql/parser/parse_tree.h.
type ColumnDefinition struct {
	Span
	Name   *Identifier `json:"name"`
	Schema Node        `json:"schema"`
}

func (n *ColumnDefinition) Children() []Node {
	return children(n.Name, n.Schema)
}

// SimpleColumnSchema is a column schema naming a type by path expression (e.g.
// "int64"); see ASTSimpleColumnSchema in googlesql/parser/parse_tree.h. The
// optional ColumnAttributeList holds trailing attributes such as NOT NULL.
type SimpleColumnSchema struct {
	Span
	Type              *PathExpression      `json:"type"`
	TypeParameters    *TypeParameterList   `json:"type_parameters,omitempty"`
	Collate           *Collate             `json:"collate,omitempty"`
	DefaultExpression Node                 `json:"default_expression,omitempty"`
	Attributes        *ColumnAttributeList `json:"attributes,omitempty"`
	Options           *OptionsList         `json:"options,omitempty"`
}

func (n *SimpleColumnSchema) Children() []Node {
	return children(n.Type, n.TypeParameters, n.Collate, n.DefaultExpression, n.Attributes, n.Options)
}

// ArrayColumnSchema is an "ARRAY<field_schema>" column schema; see
// ASTArrayColumnSchema in googlesql/parser/parse_tree.h. Trailing type
// parameters, collation, attributes, and an options list attach here through
// column_schema_inner and field_schema.
type ArrayColumnSchema struct {
	Span
	ElementSchema  Node                 `json:"element_schema"`
	TypeParameters *TypeParameterList   `json:"type_parameters,omitempty"`
	Collate        *Collate             `json:"collate,omitempty"`
	Attributes     *ColumnAttributeList `json:"attributes,omitempty"`
	Options        *OptionsList         `json:"options,omitempty"`
}

func (n *ArrayColumnSchema) Children() []Node {
	return children(n.ElementSchema, n.TypeParameters, n.Collate, n.Attributes, n.Options)
}

// StructColumnSchema is a "STRUCT<field, ...>" column schema; see
// ASTStructColumnSchema in googlesql/parser/parse_tree.h.
type StructColumnSchema struct {
	Span
	Fields         []*StructColumnField `json:"fields"`
	TypeParameters *TypeParameterList   `json:"type_parameters,omitempty"`
	Collate        *Collate             `json:"collate,omitempty"`
	Attributes     *ColumnAttributeList `json:"attributes,omitempty"`
	Options        *OptionsList         `json:"options,omitempty"`
}

func (n *StructColumnSchema) Children() []Node {
	out := make([]Node, 0, len(n.Fields)+4)
	for _, f := range n.Fields {
		out = append(out, f)
	}
	return append(out, children(n.TypeParameters, n.Collate, n.Attributes, n.Options)...)
}

// StructColumnField is one "[name] field_schema" field of a struct column
// schema; see ASTStructColumnField in googlesql/parser/parse_tree.h. Name is
// nil for an unnamed field.
type StructColumnField struct {
	Span
	Name   *Identifier `json:"name,omitempty"`
	Schema Node        `json:"schema"`
}

func (n *StructColumnField) Children() []Node {
	return children(n.Name, n.Schema)
}

// ColumnAttributeList is the list of column attributes trailing a column
// schema; see ASTColumnAttributeList in googlesql/parser/parse_tree.h.
type ColumnAttributeList struct {
	Span
	Attributes []Node `json:"attributes,omitempty"`
}

func (n *ColumnAttributeList) Children() []Node {
	return append([]Node(nil), n.Attributes...)
}

// NotNullColumnAttribute is the "NOT NULL" column attribute; see
// ASTNotNullColumnAttribute in googlesql/parser/parse_tree.h. It has no
// children.
type NotNullColumnAttribute struct {
	Span
}

func (n *NotNullColumnAttribute) Children() []Node { return nil }

// PrimaryKeyColumnAttribute is the "PRIMARY KEY [ENFORCED|NOT ENFORCED]" column
// attribute; see ASTPrimaryKeyColumnAttribute in
// googlesql/parser/parse_tree.h. Enforced defaults to true.
type PrimaryKeyColumnAttribute struct {
	Span
	Enforced bool `json:"enforced"`
}

func (n *PrimaryKeyColumnAttribute) Children() []Node { return nil }

// PrimaryKey is a "PRIMARY KEY (elements) [ENFORCED|NOT ENFORCED]" table
// constraint; see ASTPrimaryKey in googlesql/parser/parse_tree.h. Enforced
// defaults to true.
type PrimaryKey struct {
	Span
	Enforced       bool                   `json:"enforced"`
	ElementList    *PrimaryKeyElementList `json:"element_list,omitempty"`
	Options        *OptionsList           `json:"options,omitempty"`
	ConstraintName *Identifier            `json:"constraint_name,omitempty"`
}

func (n *PrimaryKey) Children() []Node {
	return children(n.ElementList, n.Options, n.ConstraintName)
}

// PrimaryKeyElementList is the parenthesized list of primary key elements; see
// ASTPrimaryKeyElementList in googlesql/parser/parse_tree.h.
type PrimaryKeyElementList struct {
	Span
	Elements []*PrimaryKeyElement `json:"elements"`
}

func (n *PrimaryKeyElementList) Children() []Node {
	out := make([]Node, 0, len(n.Elements))
	for _, e := range n.Elements {
		out = append(out, e)
	}
	return out
}

// PrimaryKeyElement is a single "column [ASC|DESC] [NULLS FIRST|LAST]" entry
// in a primary key element list; see ASTPrimaryKeyElement in
// googlesql/parser/parse_tree.h. Ordering is "", "ASC", or "DESC" (the empty
// value renders as "(ASC)", an explicit "ASC" renders as "(ASC EXPLICITLY)").
type PrimaryKeyElement struct {
	Span
	Column   *Identifier `json:"column"`
	Ordering string      `json:"ordering,omitempty"`
}

func (n *PrimaryKeyElement) Children() []Node {
	return children(n.Column)
}

// CheckConstraint is a "CHECK (expression) [ENFORCED|NOT ENFORCED]" table
// constraint; see ASTCheckConstraint in googlesql/parser/parse_tree.h.
type CheckConstraint struct {
	Span
	Enforced       bool         `json:"enforced"`
	Expression     Node         `json:"expression"`
	Options        *OptionsList `json:"options,omitempty"`
	ConstraintName *Identifier  `json:"constraint_name,omitempty"`
}

func (n *CheckConstraint) Children() []Node {
	return children(n.Expression, n.Options, n.ConstraintName)
}

// ForeignKey is a "FOREIGN KEY (columns) REFERENCES ... [ENFORCED|NOT
// ENFORCED] [OPTIONS(...)]" table constraint; see ASTForeignKey in
// googlesql/parser/parse_tree.h. When the constraint is named, ConstraintName
// is set and the node's start is moved to the name.
type ForeignKey struct {
	Span
	ColumnList     *ColumnList          `json:"column_list"`
	Reference      *ForeignKeyReference `json:"reference"`
	Options        *OptionsList         `json:"options,omitempty"`
	ConstraintName *Identifier          `json:"constraint_name,omitempty"`
}

func (n *ForeignKey) Children() []Node {
	return children(n.ColumnList, n.Reference, n.Options, n.ConstraintName)
}

// ForeignKeyReference is a "REFERENCES path (columns) [MATCH mode] [actions]"
// clause of a foreign key; see ASTForeignKeyReference in
// googlesql/parser/parse_tree.h. Match is "SIMPLE", "FULL", or "NOT DISTINCT".
// Enforced defaults to true.
type ForeignKeyReference struct {
	Span
	Match      string             `json:"match"`
	Enforced   bool               `json:"enforced"`
	Reference  *PathExpression    `json:"path"`
	ColumnList *ColumnList        `json:"column_list"`
	Actions    *ForeignKeyActions `json:"actions"`
}

func (n *ForeignKeyReference) Children() []Node {
	return children(n.Reference, n.ColumnList, n.Actions)
}

// ForeignKeyActions holds the ON UPDATE / ON DELETE referential actions of a
// foreign key; see ASTForeignKeyActions in googlesql/parser/parse_tree.h. Each
// action is "NO ACTION", "RESTRICT", "CASCADE", or "SET NULL" (default "NO
// ACTION").
type ForeignKeyActions struct {
	Span
	UpdateAction string `json:"update_action"`
	DeleteAction string `json:"delete_action"`
}

func (n *ForeignKeyActions) Children() []Node { return nil }

// WithPartitionColumnsClause is "WITH PARTITION COLUMNS [(table elements)]" in
// a CREATE EXTERNAL TABLE statement; see ASTWithPartitionColumnsClause in
// googlesql/parser/parse_tree.h.
type WithPartitionColumnsClause struct {
	Span
	TableElementList *TableElementList `json:"table_element_list,omitempty"`
}

func (n *WithPartitionColumnsClause) Children() []Node {
	return children(n.TableElementList)
}

// CreateViewStatement is a CREATE [MATERIALIZED|APPROX] VIEW statement; see
// ASTCreateViewStatement, ASTCreateMaterializedViewStatement, and
// ASTCreateApproxViewStatement in googlesql/parser/parse_tree.h. ViewKind is
// "" for a plain view, "MATERIALIZED", or "APPROX". Scope is "", "TEMP",
// "PUBLIC", or "PRIVATE". SqlSecurity is "", "INVOKER", or "DEFINER".
type CreateViewStatement struct {
	Span
	ViewKind      string                 `json:"view_kind,omitempty"`
	Scope         string                 `json:"scope,omitempty"`
	IsOrReplace   bool                   `json:"is_or_replace,omitempty"`
	IsIfNotExists bool                   `json:"is_if_not_exists,omitempty"`
	Recursive     bool                   `json:"recursive,omitempty"`
	SqlSecurity   string                 `json:"sql_security,omitempty"`
	Name          *PathExpression        `json:"name"`
	Columns       *ColumnWithOptionsList `json:"columns,omitempty"`
	// PartitionBy, ClusterBy, and ReplicaSource apply only to
	// CREATE MATERIALIZED VIEW; see ASTCreateMaterializedViewStatement in
	// googlesql/parser/parse_tree.h. They are nil for plain and APPROX views.
	PartitionBy   *PartitionBy    `json:"partition_by,omitempty"`
	ClusterBy     *ClusterBy      `json:"cluster_by,omitempty"`
	Options       *OptionsList    `json:"options,omitempty"`
	Query         *Query          `json:"query,omitempty"`
	ReplicaSource *PathExpression `json:"replica_source,omitempty"`
}

func (n *CreateViewStatement) statementNode() {}
func (n *CreateViewStatement) Children() []Node {
	// The child order follows ASTCreateMaterializedViewStatement's
	// init_fields_order: name, columns, partition_by, cluster_by, options,
	// query, replica_source. For plain and APPROX views the materialized-only
	// fields are nil, reducing to name, columns, options, query.
	return children(n.Name, n.Columns, n.PartitionBy, n.ClusterBy, n.Options, n.Query, n.ReplicaSource)
}

// CreateSequenceStatement is a CREATE [OR REPLACE] SEQUENCE [IF NOT EXISTS]
// <name> [OPTIONS(...)] statement; see ASTCreateSequenceStatement in
// googlesql/parser/parse_tree.h.
type CreateSequenceStatement struct {
	Span
	IsOrReplace   bool            `json:"is_or_replace,omitempty"`
	IsIfNotExists bool            `json:"is_if_not_exists,omitempty"`
	Name          *PathExpression `json:"name"`
	Options       *OptionsList    `json:"options,omitempty"`
}

func (n *CreateSequenceStatement) statementNode() {}
func (n *CreateSequenceStatement) Children() []Node {
	return children(n.Name, n.Options)
}

// CreateDatabaseStatement is a CREATE DATABASE <name> [OPTIONS(...)]
// statement; see ASTCreateDatabaseStatement in
// googlesql/parser/parse_tree.h. It takes no scope, OR REPLACE, or IF NOT
// EXISTS modifiers.
type CreateDatabaseStatement struct {
	Span
	Name    *PathExpression `json:"name"`
	Options *OptionsList    `json:"options,omitempty"`
}

func (n *CreateDatabaseStatement) statementNode() {}
func (n *CreateDatabaseStatement) Children() []Node {
	return children(n.Name, n.Options)
}

// CreateSchemaStatement is a CREATE [OR REPLACE] SCHEMA [IF NOT EXISTS]
// <name> [DEFAULT COLLATE ...] [OPTIONS(...)] statement; see
// ASTCreateSchemaStatement in googlesql/parser/parse_tree.h. It takes no scope
// modifier.
type CreateSchemaStatement struct {
	Span
	IsOrReplace   bool            `json:"is_or_replace,omitempty"`
	IsIfNotExists bool            `json:"is_if_not_exists,omitempty"`
	Name          *PathExpression `json:"name"`
	Collate       *Collate        `json:"collate,omitempty"`
	Options       *OptionsList    `json:"options,omitempty"`
}

func (n *CreateSchemaStatement) statementNode() {}
func (n *CreateSchemaStatement) Children() []Node {
	return children(n.Name, n.Collate, n.Options)
}

// CreateExternalSchemaStatement is a CREATE [OR REPLACE] [scope] EXTERNAL
// SCHEMA [IF NOT EXISTS] <name> [WITH CONNECTION <connection>] OPTIONS(...)
// statement; see ASTCreateExternalSchemaStatement in
// googlesql/parser/parse_tree.h.
type CreateExternalSchemaStatement struct {
	Span
	Scope          string                `json:"scope,omitempty"`
	IsOrReplace    bool                  `json:"is_or_replace,omitempty"`
	IsIfNotExists  bool                  `json:"is_if_not_exists,omitempty"`
	Name           *PathExpression       `json:"name"`
	WithConnection *WithConnectionClause `json:"with_connection,omitempty"`
	Options        *OptionsList          `json:"options,omitempty"`
}

func (n *CreateExternalSchemaStatement) statementNode() {}
func (n *CreateExternalSchemaStatement) Children() []Node {
	return children(n.Name, n.WithConnection, n.Options)
}

// DefineTableStatement is a DEFINE TABLE <name> (options) statement; see
// ASTDefineTableStatement in googlesql/parser/parse_tree.h. The options list
// is required.
type DefineTableStatement struct {
	Span
	Name    *PathExpression `json:"name"`
	Options *OptionsList    `json:"options"`
}

func (n *DefineTableStatement) statementNode() {}
func (n *DefineTableStatement) Children() []Node {
	return children(n.Name, n.Options)
}

// CreateConstantStatement is a CREATE CONSTANT statement; see
// ASTCreateConstantStatement in googlesql/parser/parse_tree.h. Scope is "",
// "TEMP", "PUBLIC", or "PRIVATE". Value is the constant's assigned expression.
type CreateConstantStatement struct {
	Span
	Scope         string          `json:"scope,omitempty"`
	IsOrReplace   bool            `json:"is_or_replace,omitempty"`
	IsIfNotExists bool            `json:"is_if_not_exists,omitempty"`
	Name          *PathExpression `json:"name"`
	Value         Node            `json:"value"`
}

func (n *CreateConstantStatement) statementNode() {}
func (n *CreateConstantStatement) Children() []Node {
	return children(n.Name, n.Value)
}

// CreateModelStatement is a CREATE MODEL statement; see
// ASTCreateModelStatement in googlesql/parser/parse_tree.h. Scope is "",
// "TEMP", "PUBLIC", or "PRIVATE". The trailing query is either a single Query
// (AS query) or an AliasedQueryList (AS (a AS (...), ...)). IsRemote is not
// reflected in the debug string.
type CreateModelStatement struct {
	Span
	Scope          string                `json:"scope,omitempty"`
	IsOrReplace    bool                  `json:"is_or_replace,omitempty"`
	IsIfNotExists  bool                  `json:"is_if_not_exists,omitempty"`
	IsRemote       bool                  `json:"is_remote,omitempty"`
	Name           *PathExpression       `json:"name"`
	InputOutput    *InputOutputClause    `json:"input_output_clause,omitempty"`
	Transform      *TransformClause      `json:"transform_clause,omitempty"`
	WithConnection *WithConnectionClause `json:"with_connection,omitempty"`
	Options        *OptionsList          `json:"options,omitempty"`
	Query          *Query                `json:"query,omitempty"`
	AliasedQueries *AliasedQueryList     `json:"aliased_query_list,omitempty"`
}

func (n *CreateModelStatement) statementNode() {}
func (n *CreateModelStatement) Children() []Node {
	return children(n.Name, n.InputOutput, n.Transform, n.WithConnection,
		n.Options, n.Query, n.AliasedQueries)
}

// InputOutputClause is "INPUT (columns) OUTPUT (columns)" in a CREATE MODEL
// statement; see ASTInputOutputClause in googlesql/parser/parse_tree.h.
type InputOutputClause struct {
	Span
	Input  *TableElementList `json:"input"`
	Output *TableElementList `json:"output"`
}

func (n *InputOutputClause) Children() []Node {
	return children(n.Input, n.Output)
}

// TransformClause is "TRANSFORM (select_list)" in a CREATE MODEL statement;
// see ASTTransformClause in googlesql/parser/parse_tree.h.
type TransformClause struct {
	Span
	SelectList *SelectList `json:"select_list"`
}

func (n *TransformClause) Children() []Node {
	return children(n.SelectList)
}

// AliasedQueryList is the comma-separated list of aliased queries in a
// CREATE MODEL "AS (a AS (...), ...)" clause; see ASTAliasedQueryList in
// googlesql/parser/parse_tree.h. The span excludes the surrounding
// parentheses.
type AliasedQueryList struct {
	Span
	Queries []*AliasedQuery `json:"queries,omitempty"`
}

func (n *AliasedQueryList) Children() []Node {
	out := make([]Node, len(n.Queries))
	for i, q := range n.Queries {
		out[i] = q
	}
	return out
}

// ColumnWithOptionsList is the parenthesized column name list (each with an
// optional OPTIONS clause) in a CREATE VIEW statement; see
// ASTColumnWithOptionsList in googlesql/parser/parse_tree.h.
type ColumnWithOptionsList struct {
	Span
	Columns []*ColumnWithOptions `json:"columns"`
}

func (n *ColumnWithOptionsList) Children() []Node {
	out := make([]Node, 0, len(n.Columns))
	for _, c := range n.Columns {
		out = append(out, c)
	}
	return out
}

// ColumnWithOptions is a single "identifier [OPTIONS(...)]" entry in a
// ColumnWithOptionsList; see ASTColumnWithOptions in
// googlesql/parser/parse_tree.h.
type ColumnWithOptions struct {
	Span
	Name    *Identifier  `json:"name"`
	Options *OptionsList `json:"options,omitempty"`
}

func (n *ColumnWithOptions) Children() []Node {
	return children(n.Name, n.Options)
}

// CreateIndexStatement is a
// "CREATE [OR REPLACE] [UNIQUE] [SEARCH|VECTOR] INDEX [IF NOT EXISTS] name
// ON table [AS alias] [unnest_list] (index_items) [STORING (...)]
// [PARTITION BY ...] [OPTIONS(...)]" statement; see ASTCreateIndexStatement in
// googlesql/parser/parse_tree.h. Children appear in the fixed grammar order.
type CreateIndexStatement struct {
	Span
	IsOrReplace          bool                        `json:"is_or_replace,omitempty"`
	IsUnique             bool                        `json:"is_unique,omitempty"`
	IsSearch             bool                        `json:"is_search,omitempty"`
	IsVector             bool                        `json:"is_vector,omitempty"`
	IsIfNotExists        bool                        `json:"is_if_not_exists,omitempty"`
	Name                 *PathExpression             `json:"name"`
	TableName            *PathExpression             `json:"table_name"`
	Alias                *Alias                      `json:"alias,omitempty"`
	UnnestExpressionList *IndexUnnestExpressionList  `json:"unnest_expression_list,omitempty"`
	IndexItemList        *IndexItemList              `json:"index_item_list"`
	Storing              *IndexStoringExpressionList `json:"storing,omitempty"`
	PartitionBy          *PartitionBy                `json:"partition_by,omitempty"`
	Options              *OptionsList                `json:"options,omitempty"`
	IsNullFiltered       bool                        `json:"is_null_filtered,omitempty"`
	SpannerInterleave    *SpannerInterleaveClause    `json:"spanner_interleave,omitempty"`
}

func (n *CreateIndexStatement) statementNode() {}
func (n *CreateIndexStatement) Children() []Node {
	return children(n.Name, n.TableName, n.Alias, n.UnnestExpressionList, n.IndexItemList, n.Storing, n.PartitionBy, n.Options, n.SpannerInterleave)
}

// IndexItemList is the parenthesized list of ordering expressions in a
// CREATE INDEX statement; see ASTIndexItemList in
// googlesql/parser/parse_tree.h.
type IndexItemList struct {
	Span
	OrderingExpressions []*OrderingExpression `json:"ordering_expressions"`
}

func (n *IndexItemList) Children() []Node {
	out := make([]Node, 0, len(n.OrderingExpressions))
	for _, e := range n.OrderingExpressions {
		out = append(out, e)
	}
	return out
}

// IndexAllColumns is the "ALL COLUMNS [WITH COLUMN OPTIONS (...)]" form of the
// index item list; see ASTIndexAllColumns in
// googlesql/parser/parse_tree.h. The optional child is the per-column options
// list (an IndexItemList).
type IndexAllColumns struct {
	Span
	Image         string         `json:"image"`
	ColumnOptions *IndexItemList `json:"column_options,omitempty"`
}

func (n *IndexAllColumns) Children() []Node {
	return children(n.ColumnOptions)
}

// IndexUnnestExpressionList is the list of UNNEST expressions preceding the
// index items in a CREATE INDEX statement; see
// ASTIndexUnnestExpressionList in googlesql/parser/parse_tree.h.
type IndexUnnestExpressionList struct {
	Span
	UnnestExpressions []*UnnestExpressionWithOptAliasAndOffset `json:"unnest_expressions"`
}

func (n *IndexUnnestExpressionList) Children() []Node {
	out := make([]Node, 0, len(n.UnnestExpressions))
	for _, e := range n.UnnestExpressions {
		out = append(out, e)
	}
	return out
}

// UnnestExpressionWithOptAliasAndOffset is an UNNEST expression with an
// optional alias and optional WITH OFFSET clause; see
// ASTUnnestExpressionWithOptAliasAndOffset in
// googlesql/parser/parse_tree.h.
type UnnestExpressionWithOptAliasAndOffset struct {
	Span
	Expression *UnnestExpression `json:"expression"`
	Alias      *Alias            `json:"alias,omitempty"`
	WithOffset *WithOffset       `json:"with_offset,omitempty"`
}

func (n *UnnestExpressionWithOptAliasAndOffset) Children() []Node {
	return children(n.Expression, n.Alias, n.WithOffset)
}

// IndexStoringExpressionList is the "STORING (expr, ...)" clause of a
// CREATE INDEX statement; see ASTIndexStoringExpressionList in
// googlesql/parser/parse_tree.h. The span starts at the opening parenthesis
// after the STORING keyword.
type IndexStoringExpressionList struct {
	Span
	Expressions []Node `json:"expressions"`
}

func (n *IndexStoringExpressionList) Children() []Node {
	return append([]Node(nil), n.Expressions...)
}

// CreateRowAccessPolicyStatement is a
// "CREATE [OR REPLACE] ROW [ACCESS] POLICY [IF NOT EXISTS] [name] ON path
// [grant_to_clause] filter_using_clause" statement; see
// ASTCreateRowAccessPolicyStatement in googlesql/parser/parse_tree.h. The
// optional policy name is emitted LAST in the debug tree, matching the
// reference child order (target path, grant-to, filter-using, name).
type CreateRowAccessPolicyStatement struct {
	Span
	IsOrReplace   bool               `json:"is_or_replace,omitempty"`
	IsIfNotExists bool               `json:"is_if_not_exists,omitempty"`
	TargetPath    *PathExpression    `json:"target_path"`
	GrantTo       *GrantToClause     `json:"grant_to,omitempty"`
	FilterUsing   *FilterUsingClause `json:"filter_using"`
	Name          *PathExpression    `json:"name,omitempty"`
}

func (n *CreateRowAccessPolicyStatement) statementNode() {}
func (n *CreateRowAccessPolicyStatement) Children() []Node {
	return children(n.TargetPath, n.GrantTo, n.FilterUsing, n.Name)
}

// CreateTableFunctionStatement is a CREATE TABLE FUNCTION statement; see
// ASTCreateTableFunctionStatement in googlesql/parser/parse_tree.h. Scope is
// "", "TEMP", "PUBLIC", or "PRIVATE" (TEMPORARY normalizes to "TEMP").
// SqlSecurity ("", "INVOKER", or "DEFINER") is parsed but, matching the
// reference, is not shown in the debug tree. Children appear in a fixed order
// regardless of the source order of the options and language clauses.
type CreateTableFunctionStatement struct {
	Span
	Scope         string               `json:"scope,omitempty"`
	IsOrReplace   bool                 `json:"is_or_replace,omitempty"`
	IsIfNotExists bool                 `json:"is_if_not_exists,omitempty"`
	SqlSecurity   string               `json:"sql_security,omitempty"`
	Declaration   *FunctionDeclaration `json:"declaration"`
	Returns       *TVFSchema           `json:"returns,omitempty"`
	Options       *OptionsList         `json:"options,omitempty"`
	Language      *Identifier          `json:"language,omitempty"`
	Query         *Query               `json:"query,omitempty"`
}

func (n *CreateTableFunctionStatement) statementNode() {}
func (n *CreateTableFunctionStatement) Children() []Node {
	return children(n.Declaration, n.Returns, n.Options, n.Language, n.Query)
}

// CreateFunctionStatement is a CREATE FUNCTION statement (scalar or aggregate
// function); see ASTCreateFunctionStatement in googlesql/parser/parse_tree.h.
// Scope is "", "TEMP", "PUBLIC", or "PRIVATE" (TEMPORARY normalizes to
// "TEMP"). Determinism is "", "DETERMINISTIC", "NOT DETERMINISTIC",
// "IMMUTABLE", "STABLE", or "VOLATILE". SqlSecurity is "", "INVOKER", or
// "DEFINER" and, unlike CreateTableFunctionStatement, IS shown in the debug
// tree. IsRemote records the REMOTE keyword, which is parsed but not shown.
// Children appear in a fixed order (declaration, return type, language,
// with-connection, body, options) regardless of source order.
type CreateFunctionStatement struct {
	Span
	Scope          string                `json:"scope,omitempty"`
	IsOrReplace    bool                  `json:"is_or_replace,omitempty"`
	IsIfNotExists  bool                  `json:"is_if_not_exists,omitempty"`
	IsAggregate    bool                  `json:"is_aggregate,omitempty"`
	Determinism    string                `json:"determinism,omitempty"`
	SqlSecurity    string                `json:"sql_security,omitempty"`
	IsRemote       bool                  `json:"is_remote,omitempty"`
	Declaration    *FunctionDeclaration  `json:"declaration"`
	ReturnType     Node                  `json:"return_type,omitempty"`
	Language       *Identifier           `json:"language,omitempty"`
	WithConnection *WithConnectionClause `json:"with_connection,omitempty"`
	Body           Node                  `json:"body,omitempty"`
	Options        *OptionsList          `json:"options,omitempty"`
}

func (n *CreateFunctionStatement) statementNode() {}
func (n *CreateFunctionStatement) Children() []Node {
	return children(n.Declaration, n.ReturnType, n.Language, n.WithConnection, n.Body, n.Options)
}

// CreateProcedureStatement is a CREATE PROCEDURE statement; see
// ASTCreateProcedureStatement in googlesql/parser/parse_tree.h. Scope is "",
// "TEMP", "PUBLIC", or "PRIVATE". ExternalSecurity is "", "INVOKER", or
// "DEFINER".
type CreateProcedureStatement struct {
	Span
	Scope            string                `json:"scope,omitempty"`
	IsOrReplace      bool                  `json:"is_or_replace,omitempty"`
	IsIfNotExists    bool                  `json:"is_if_not_exists,omitempty"`
	ExternalSecurity string                `json:"external_security,omitempty"`
	Name             *PathExpression       `json:"name"`
	Parameters       *FunctionParameters   `json:"parameters"`
	Options          *OptionsList          `json:"options,omitempty"`
	Body             *Script               `json:"body,omitempty"`
	WithConnection   *WithConnectionClause `json:"with_connection,omitempty"`
	Language         *Identifier           `json:"language,omitempty"`
	Code             Node                  `json:"code,omitempty"`
}

func (n *CreateProcedureStatement) statementNode() {}
func (n *CreateProcedureStatement) Children() []Node {
	return children(n.Name, n.Parameters, n.Options, n.Body, n.WithConnection, n.Language, n.Code)
}

// SqlFunctionBody is the "(expression)" body of a SQL-defined function; see
// ASTSqlFunctionBody in googlesql/parser/parse_tree.h. Its span includes the
// enclosing parentheses; its single child is the body expression.
type SqlFunctionBody struct {
	Span
	Expression Node `json:"expression"`
}

func (n *SqlFunctionBody) Children() []Node {
	return children(n.Expression)
}

// TemplatedParameterType is a templated function-argument type such as
// "ANY TYPE"; see ASTTemplatedParameterType in googlesql/parser/parse_tree.h.
// It has no children.
type TemplatedParameterType struct {
	Span
	Kind string `json:"kind,omitempty"`
}

func (n *TemplatedParameterType) Children() []Node { return nil }

// WithConnectionClause is "WITH CONNECTION <connection>"; see
// ASTWithConnectionClause in googlesql/parser/parse_tree.h. Its single child
// is a ConnectionClause.
type WithConnectionClause struct {
	Span
	Connection *ConnectionClause `json:"connection"`
}

func (n *WithConnectionClause) Children() []Node {
	return children(n.Connection)
}

// FunctionDeclaration is "path(params)" in a CREATE FUNCTION-family statement;
// see ASTFunctionDeclaration in googlesql/parser/parse_tree.h.
type FunctionDeclaration struct {
	Span
	Name       *PathExpression     `json:"name"`
	Parameters *FunctionParameters `json:"parameters"`
}

func (n *FunctionDeclaration) Children() []Node {
	return children(n.Name, n.Parameters)
}

// FunctionParameters is the parenthesized parameter list of a function
// declaration; see ASTFunctionParameters in googlesql/parser/parse_tree.h.
type FunctionParameters struct {
	Span
	Parameters []*FunctionParameter `json:"parameters,omitempty"`
}

func (n *FunctionParameters) Children() []Node {
	var out []Node
	for _, p := range n.Parameters {
		out = append(out, p)
	}
	return out
}

// FunctionParameter is a single "[name] type [AS alias] [DEFAULT expr]
// [NOT AGGREGATE]" function parameter; see ASTFunctionParameter in
// googlesql/parser/parse_tree.h. DefaultValue and IsNotAggregate surface in
// the node's debug string; Alias and DefaultValue are children.
type FunctionParameter struct {
	Span
	Name           *Identifier `json:"name,omitempty"`
	Type           Node        `json:"type,omitempty"`
	Alias          *Alias      `json:"alias,omitempty"`
	DefaultValue   Node        `json:"default_value,omitempty"`
	IsNotAggregate bool        `json:"is_not_aggregate,omitempty"`
	// Mode is the procedure-parameter mode: "", "IN", "OUT", or "INOUT".
	Mode string `json:"mode,omitempty"`
}

func (n *FunctionParameter) Children() []Node {
	return children(n.Name, n.Type, n.Alias, n.DefaultValue)
}

// TVFSchema is the "TABLE<col, ...>" return schema of a table-valued function;
// see ASTTVFSchema in googlesql/parser/parse_tree.h.
type TVFSchema struct {
	Span
	Columns []*TVFSchemaColumn `json:"columns"`
}

func (n *TVFSchema) Children() []Node {
	var out []Node
	for _, c := range n.Columns {
		out = append(out, c)
	}
	return out
}

// TVFSchemaColumn is a single "[name] type" column in a TVFSchema; see
// ASTTVFSchemaColumn in googlesql/parser/parse_tree.h.
type TVFSchemaColumn struct {
	Span
	Name *Identifier `json:"name,omitempty"`
	Type Node        `json:"type"`
}

func (n *TVFSchemaColumn) Children() []Node {
	return children(n.Name, n.Type)
}

// FunctionCall is a function call expression.
type FunctionCall struct {
	Span
	Function *PathExpression `json:"function"`
	Args     []Node          `json:"args"`
	Distinct bool            `json:"distinct,omitempty"`
	// IsChained is set for a chained function call "expr.method(...)", where
	// the base expression is stored as the first element of Args; see
	// ASTFunctionCall::is_chained_call and function_call_expression_base in
	// googlesql.tm.
	IsChained bool `json:"is_chained_call,omitempty"`
	// NullHandling is "IGNORE_NULLS" or "RESPECT_NULLS" when the call has an
	// "IGNORE NULLS" or "RESPECT NULLS" modifier; empty otherwise. It is not
	// shown in the debug tree; see ASTFunctionCall::NullHandlingModifier.
	NullHandling string          `json:"null_handling_modifier,omitempty"`
	Where        *WhereClause    `json:"where,omitempty"`
	Having       *HavingModifier `json:"having_modifier,omitempty"`
	// GroupBy and HavingClause are the multi-level aggregation modifiers
	// "GROUP BY ..." and the full "HAVING expr" that may follow it inside a
	// function call; see function_call_expression in googlesql.tm.
	GroupBy        *GroupBy                `json:"group_by,omitempty"`
	HavingClause   *Having                 `json:"having,omitempty"`
	ClampedBetween *ClampedBetweenModifier `json:"clamped_between_modifier,omitempty"`
	WithReport     *WithReportModifier     `json:"with_report_modifier,omitempty"`
	OrderBy        *OrderBy                `json:"order_by,omitempty"`
	LimitOffset    *LimitOffset            `json:"limit_offset,omitempty"`
}

func (n *FunctionCall) Children() []Node {
	out := children(n.Function)
	out = append(out, n.Args...)
	if n.Where != nil {
		out = append(out, n.Where)
	}
	if n.Having != nil {
		out = append(out, n.Having)
	}
	if n.GroupBy != nil {
		out = append(out, n.GroupBy)
	}
	if n.HavingClause != nil {
		out = append(out, n.HavingClause)
	}
	if n.ClampedBetween != nil {
		out = append(out, n.ClampedBetween)
	}
	if n.WithReport != nil {
		out = append(out, n.WithReport)
	}
	if n.OrderBy != nil {
		out = append(out, n.OrderBy)
	}
	if n.LimitOffset != nil {
		out = append(out, n.LimitOffset)
	}
	return out
}

// NamedArgument is a "name => value" argument in a function or table-valued
// function call; see ASTNamedArgument in googlesql/parser/parse_tree.h.
type NamedArgument struct {
	Span
	Name  *Identifier `json:"name"`
	Value Node        `json:"value"`
}

func (n *NamedArgument) Children() []Node {
	return children(n.Name, n.Value)
}

// ExpressionWithAlias is a function-call argument written as "expression AS
// alias"; see ASTExpressionWithAlias in googlesql/parser/parse_tree.h and
// function_call_argument in googlesql.tm.
type ExpressionWithAlias struct {
	Span
	Expression Node   `json:"expression"`
	Alias      *Alias `json:"alias"`
}

func (n *ExpressionWithAlias) Children() []Node {
	return children(n.Expression, n.Alias)
}

// Lambda is a lambda argument "param -> body" or "(a, b) -> body" passed to a
// function or table-valued function call; see ASTLambda in
// googlesql/parser/parse_tree.h. Params is a PathExpression (single argument)
// or a StructConstructorWithParens (parenthesized argument list).
type Lambda struct {
	Span
	Params Node `json:"params"`
	Body   Node `json:"body"`
}

func (n *Lambda) Children() []Node {
	return children(n.Params, n.Body)
}

// HavingModifier is the "HAVING MAX <expr>" or "HAVING MIN <expr>" modifier
// on aggregate function calls; see ASTHavingModifier in
// googlesql/parser/parse_tree.h.
type HavingModifier struct {
	Span
	Kind string `json:"kind"` // "MAX" or "MIN"
	Expr Node   `json:"expr"`
}

func (n *HavingModifier) Children() []Node {
	return children(n.Expr)
}

// AnalyticFunctionCall is a function call followed by an OVER clause; see
// ASTAnalyticFunctionCall in googlesql/parser/parse_tree.h.
type AnalyticFunctionCall struct {
	Span
	Expr       Node                 `json:"expr"` // the *FunctionCall
	WindowSpec *WindowSpecification `json:"window_spec"`
}

func (n *AnalyticFunctionCall) Children() []Node {
	return children(n.Expr, n.WindowSpec)
}

// WindowSpecification is the window after OVER: either a base window name, or
// "( [base window name] [PARTITION BY ...] [ORDER BY ...] [frame] )"; see
// ASTWindowSpecification in googlesql/parser/parse_tree.h. For the
// parenthesized form the span includes the parentheses.
type WindowSpecification struct {
	Span
	Name        *Identifier  `json:"name,omitempty"`
	PartitionBy *PartitionBy `json:"partition_by,omitempty"`
	OrderBy     *OrderBy     `json:"order_by,omitempty"`
	WindowFrame *WindowFrame `json:"window_frame,omitempty"`
}

func (n *WindowSpecification) Children() []Node {
	return children(n.Name, n.PartitionBy, n.OrderBy, n.WindowFrame)
}

// WindowClause is a "WINDOW name AS (...), ..." named window clause on a
// SELECT; see ASTWindowClause in googlesql/parser/parse_tree.h.
type WindowClause struct {
	Span
	Windows []*WindowDefinition `json:"windows"`
}

func (n *WindowClause) Children() []Node {
	var out []Node
	for _, w := range n.Windows {
		out = append(out, w)
	}
	return out
}

// WindowDefinition is a single "name AS window_specification" entry in a
// WINDOW clause; see ASTWindowDefinition in googlesql/parser/parse_tree.h.
type WindowDefinition struct {
	Span
	Name       *Identifier          `json:"name"`
	WindowSpec *WindowSpecification `json:"window_spec"`
}

func (n *WindowDefinition) Children() []Node {
	return children(n.Name, n.WindowSpec)
}

// WindowFrame is a "ROWS|RANGE ..." window frame clause; see ASTWindowFrame
// in googlesql/parser/parse_tree.h. EndExpr is only set for the
// "BETWEEN low AND high" form.
type WindowFrame struct {
	Span
	Unit      string           `json:"unit"` // "ROWS" or "RANGE"
	StartExpr *WindowFrameExpr `json:"start_expr"`
	EndExpr   *WindowFrameExpr `json:"end_expr,omitempty"`
}

func (n *WindowFrame) Children() []Node {
	return children(n.StartExpr, n.EndExpr)
}

// WindowFrameExpr is a window frame boundary; see ASTWindowFrameExpr in
// googlesql/parser/parse_tree.h. Expression is only set for the
// "expression PRECEDING/FOLLOWING" (OFFSET) forms.
type WindowFrameExpr struct {
	Span
	// BoundaryType is one of "UNBOUNDED PRECEDING", "OFFSET PRECEDING",
	// "CURRENT ROW", "OFFSET FOLLOWING" or "UNBOUNDED FOLLOWING".
	BoundaryType string `json:"boundary_type"`
	Expression   Node   `json:"expression,omitempty"`
}

func (n *WindowFrameExpr) Children() []Node {
	return children(n.Expression)
}

// PartitionBy is a "PARTITION [hint] BY expr, ..." clause; see ASTPartitionBy
// in googlesql/parser/parse_tree.h.
type PartitionBy struct {
	Span
	Hint        *Hint  `json:"hint,omitempty"`
	Expressions []Node `json:"expressions"`
}

func (n *PartitionBy) Children() []Node {
	out := children(n.Hint)
	return append(out, n.Expressions...)
}

// ClusterBy is a "CLUSTER BY expr, ..." clause in a CREATE TABLE-family
// statement; see ASTClusterBy in googlesql/parser/parse_tree.h.
type ClusterBy struct {
	Span
	Expressions []Node `json:"expressions"`
}

func (n *ClusterBy) Children() []Node {
	return append([]Node(nil), n.Expressions...)
}

// SetOperation is a chain of query primaries combined with the same set
// operation (UNION/INTERSECT/EXCEPT), e.g. "q1 UNION ALL q2 UNION ALL q3";
// see ASTSetOperation in googlesql/parser/parse_tree.h. Metadata holds one
// entry per operator; Inputs holds the operand queries in order.
type SetOperation struct {
	Span
	Metadata *SetOperationMetadataList `json:"metadata"`
	Inputs   []Node                    `json:"inputs"`
}

func (n *SetOperation) Children() []Node {
	out := children(n.Metadata)
	return append(out, n.Inputs...)
}

// SetOperationMetadataList holds the metadata of each set operator in a
// SetOperation; see ASTSetOperationMetadataList in
// googlesql/parser/parse_tree.h.
type SetOperationMetadataList struct {
	Span
	Entries []*SetOperationMetadata `json:"entries"`
}

func (n *SetOperationMetadataList) Children() []Node {
	var out []Node
	for _, e := range n.Entries {
		out = append(out, e)
	}
	return out
}

// SetOperationMetadata describes one set operator: its type, ALL/DISTINCT
// modifier, optional hint, and optional column match/propagation modes; see
// ASTSetOperationMetadata in googlesql/parser/parse_tree.h. Children keep the
// constructor order of the reference (not source order): type, ALL/DISTINCT,
// hint, column match mode, column propagation mode, column list.
type SetOperationMetadata struct {
	Span
	OpType                *SetOperationType                  `json:"op_type"`
	AllOrDistinct         *SetOperationAllOrDistinct         `json:"all_or_distinct"`
	Hint                  *Hint                              `json:"hint,omitempty"`
	ColumnMatchMode       *SetOperationColumnMatchMode       `json:"column_match_mode,omitempty"`
	ColumnPropagationMode *SetOperationColumnPropagationMode `json:"column_propagation_mode,omitempty"`
	ColumnList            *ColumnList                        `json:"column_list,omitempty"`
}

func (n *SetOperationMetadata) Children() []Node {
	return children(n.OpType, n.AllOrDistinct, n.Hint, n.ColumnMatchMode,
		n.ColumnPropagationMode, n.ColumnList)
}

// SetOperationType is the operator keyword of a set operation; Op is "UNION",
// "INTERSECT", or "EXCEPT". See ASTSetOperationType in
// googlesql/parser/parse_tree.h.
type SetOperationType struct {
	Span
	Op string `json:"op"`
}

func (n *SetOperationType) Children() []Node { return nil }

// SetOperationAllOrDistinct is the ALL or DISTINCT modifier of a set
// operation; Value is "ALL" or "DISTINCT". See ASTSetOperationAllOrDistinct
// in googlesql/parser/parse_tree.h.
type SetOperationAllOrDistinct struct {
	Span
	Value string `json:"value"`
}

func (n *SetOperationAllOrDistinct) Children() []Node { return nil }

// SetOperationColumnMatchMode is a CORRESPONDING / CORRESPONDING BY /
// BY NAME / BY NAME ON modifier on a set operation; Value is
// "CORRESPONDING", "CORRESPONDING_BY", "BY_NAME", or "BY_NAME_ON". See
// ASTSetOperationColumnMatchMode in googlesql/parser/parse_tree.h.
type SetOperationColumnMatchMode struct {
	Span
	Value string `json:"value"`
}

func (n *SetOperationColumnMatchMode) Children() []Node { return nil }

// SetOperationColumnPropagationMode is a FULL/LEFT/INNER outer mode prefix or
// a STRICT modifier on a set operation; Value is "FULL", "LEFT", "INNER", or
// "STRICT". See ASTSetOperationColumnPropagationMode in
// googlesql/parser/parse_tree.h.
type SetOperationColumnPropagationMode struct {
	Span
	Value string `json:"value"`
}

func (n *SetOperationColumnPropagationMode) Children() []Node { return nil }

// ColumnList is a parenthesized list of column name identifiers, e.g. "(a,
// b)"; see ASTColumnList in googlesql/parser/parse_tree.h. The span includes
// the parentheses.
type ColumnList struct {
	Span
	Identifiers []*Identifier `json:"identifiers"`
}

func (n *ColumnList) Children() []Node {
	var out []Node
	for _, id := range n.Identifiers {
		out = append(out, id)
	}
	return out
}

// TableClause is a "TABLE path" clause used as a query; see ASTTableClause
// in googlesql/parser/parse_tree.h.
type TableClause struct {
	Span
	// Table is the table_clause_no_keyword operand: either a *PathExpression
	// (TABLE path) or a *TVF (TABLE path(args...)); see table_clause_no_keyword
	// in googlesql.tm.
	Table Node `json:"table"`
}

func (n *TableClause) Children() []Node {
	return children(n.Table)
}

// ModelClause is a "MODEL path" table-valued function argument; see
// ASTModelClause in googlesql/parser/parse_tree.h.
type ModelClause struct {
	Span
	Path *PathExpression `json:"path"`
}

func (n *ModelClause) Children() []Node {
	return children(n.Path)
}

// ConnectionClause is a "CONNECTION {path | DEFAULT}" table-valued function
// argument; see ASTConnectionClause in googlesql/parser/parse_tree.h. Path
// is either a *PathExpression or a *DefaultLiteral.
type ConnectionClause struct {
	Span
	Path Node `json:"path"`
}

func (n *ConnectionClause) Children() []Node {
	return children(n.Path)
}

// DefaultLiteral is the DEFAULT keyword used in place of a path expression;
// see ASTDefaultLiteral in googlesql/parser/parse_tree.h.
type DefaultLiteral struct {
	Span
}

func (n *DefaultLiteral) Children() []Node { return nil }

// ParameterExpr is a named query parameter "@name" or a positional query
// parameter "?"; see ASTParameterExpr in googlesql/parser/parse_tree.h. For
// positional parameters Name is nil and Position is the 1-based ordinal of
// the "?" in the statement.
type ParameterExpr struct {
	Span
	Name     *Identifier `json:"name,omitempty"`
	Position int         `json:"position,omitempty"`
}

func (n *ParameterExpr) Children() []Node {
	return children(n.Name)
}

// SystemVariableExpr is a system variable reference "@@path"; see
// ASTSystemVariableExpr in googlesql/parser/parse_tree.h.
type SystemVariableExpr struct {
	Span
	Path *PathExpression `json:"path"`
}

func (n *SystemVariableExpr) Children() []Node {
	return children(n.Path)
}

// PipeSetOperation is a "|> UNION ALL (query), ..." pipe operator; see
// ASTPipeSetOperation in googlesql/parser/parse_tree.h.
type PipeSetOperation struct {
	Span
	Metadata *SetOperationMetadata `json:"metadata"`
	Inputs   []Node                `json:"inputs"`
}

func (n *PipeSetOperation) Children() []Node {
	out := children(n.Metadata)
	return append(out, n.Inputs...)
}

// CastExpression is "CAST(expr AS type [FORMAT ...])" or "SAFE_CAST(...)";
// see ASTCastExpression in googlesql/parser/parse_tree.h.
type CastExpression struct {
	Span
	Expr       Node          `json:"expr"`
	Type       Node          `json:"type"`
	Format     *FormatClause `json:"format,omitempty"`
	IsSafeCast bool          `json:"is_safe_cast,omitempty"`
}

func (n *CastExpression) Children() []Node {
	return children(n.Expr, n.Type, n.Format)
}

// ExtractExpression is "EXTRACT(part FROM expr [AT TIME ZONE tz])"; see
// ASTExtractExpression in googlesql/parser/parse_tree.h.
type ExtractExpression struct {
	Span
	LhsExpr  Node `json:"lhs_expr"`
	RhsExpr  Node `json:"rhs_expr"`
	TimeZone Node `json:"time_zone_expr,omitempty"`
}

func (n *ExtractExpression) Children() []Node {
	return children(n.LhsExpr, n.RhsExpr, n.TimeZone)
}

// FormatClause is the "FORMAT expr [AT TIME ZONE expr]" clause of a cast;
// see ASTFormatClause in googlesql/parser/parse_tree.h.
type FormatClause struct {
	Span
	Format   Node `json:"format"`
	TimeZone Node `json:"time_zone,omitempty"`
}

func (n *FormatClause) Children() []Node {
	return children(n.Format, n.TimeZone)
}

// SimpleType is a named type like "int64" or "a.b.c"; see ASTSimpleType in
// googlesql/parser/parse_tree.h.
type SimpleType struct {
	Span
	Name           *PathExpression    `json:"name"`
	TypeParameters *TypeParameterList `json:"type_parameters,omitempty"`
	Collate        *Collate           `json:"collate,omitempty"`
}

func (n *SimpleType) Children() []Node {
	return children(n.Name, n.TypeParameters, n.Collate)
}

// ArrayType is "ARRAY<type>"; see ASTArrayType in
// googlesql/parser/parse_tree.h.
type ArrayType struct {
	Span
	ElementType    Node               `json:"element_type"`
	TypeParameters *TypeParameterList `json:"type_parameters,omitempty"`
	Collate        *Collate           `json:"collate,omitempty"`
}

func (n *ArrayType) Children() []Node {
	return children(n.ElementType, n.TypeParameters, n.Collate)
}

// StructType is "STRUCT<field, ...>"; see ASTStructType in
// googlesql/parser/parse_tree.h.
type StructType struct {
	Span
	Fields         []*StructField     `json:"fields"`
	TypeParameters *TypeParameterList `json:"type_parameters,omitempty"`
	Collate        *Collate           `json:"collate,omitempty"`
}

func (n *StructType) Children() []Node {
	var out []Node
	for _, f := range n.Fields {
		out = append(out, f)
	}
	return append(out, children(n.TypeParameters, n.Collate)...)
}

// StructField is one "[name] type" entry in a STRUCT type; see
// ASTStructField in googlesql/parser/parse_tree.h.
type StructField struct {
	Span
	Name *Identifier `json:"name,omitempty"`
	Type Node        `json:"type"`
}

func (n *StructField) Children() []Node {
	return children(n.Name, n.Type)
}

// RangeType is "RANGE<type>"; see ASTRangeType in
// googlesql/parser/parse_tree.h.
type RangeType struct {
	Span
	ElementType    Node               `json:"element_type"`
	TypeParameters *TypeParameterList `json:"type_parameters,omitempty"`
	Collate        *Collate           `json:"collate,omitempty"`
}

func (n *RangeType) Children() []Node {
	return children(n.ElementType, n.TypeParameters, n.Collate)
}

// MapType is "MAP<key_type, value_type>"; see ASTMapType in
// googlesql/parser/parse_tree.h.
type MapType struct {
	Span
	KeyType        Node               `json:"key_type"`
	ValueType      Node               `json:"value_type"`
	TypeParameters *TypeParameterList `json:"type_parameters,omitempty"`
	Collate        *Collate           `json:"collate,omitempty"`
}

func (n *MapType) Children() []Node {
	return children(n.KeyType, n.ValueType, n.TypeParameters, n.Collate)
}

// FunctionType is "FUNCTION<argtypes -> returntype>"; see ASTFunctionType in
// googlesql/parser/parse_tree.h. Children are the argument list, the return
// type, and then any trailing type parameters and collation.
type FunctionType struct {
	Span
	ArgList        *FunctionTypeArgList `json:"arg_list"`
	ReturnType     Node                 `json:"return_type"`
	TypeParameters *TypeParameterList   `json:"type_parameters,omitempty"`
	Collate        *Collate             `json:"collate,omitempty"`
}

func (n *FunctionType) Children() []Node {
	return children(n.ArgList, n.ReturnType, n.TypeParameters, n.Collate)
}

// FunctionTypeArgList is the argument-type list of a FunctionType; see
// ASTFunctionTypeArgList in googlesql/parser/parse_tree.h.
type FunctionTypeArgList struct {
	Span
	Args []Node `json:"args,omitempty"`
}

func (n *FunctionTypeArgList) Children() []Node {
	return append([]Node(nil), n.Args...)
}

// TypeParameterList is the "(param, ...)" suffix of a parameterized type
// like STRING(10); see ASTTypeParameterList in
// googlesql/parser/parse_tree.h. Its span covers "(" through the last
// parameter, excluding the closing ")", matching the reference grammar.
type TypeParameterList struct {
	Span
	Parameters []Node `json:"parameters"`
}

func (n *TypeParameterList) Children() []Node {
	return append([]Node(nil), n.Parameters...)
}

// MaxLiteral is the special "MAX" type parameter, e.g. NUMERIC(MAX); see
// ASTMaxLiteral in googlesql/parser/parse_tree.h.
type MaxLiteral struct {
	Span
}

func (n *MaxLiteral) Children() []Node { return nil }

// MatchRecognizeClause is a "MATCH_RECOGNIZE(...)" postfix table operator;
// see ASTMatchRecognizeClause in googlesql/parser/parse_tree.h. Child order
// matches the reference node: options, partition by, order by, measures,
// after match skip, pattern, definitions, alias.
type MatchRecognizeClause struct {
	Span
	Options        *OptionsList          `json:"options,omitempty"`
	PartitionBy    *PartitionBy          `json:"partition_by,omitempty"`
	OrderBy        *OrderBy              `json:"order_by"`
	Measures       *SelectList           `json:"measures"`
	AfterMatchSkip *AfterMatchSkipClause `json:"after_match_skip,omitempty"`
	Pattern        Node                  `json:"pattern"`
	Definitions    *SelectList           `json:"definitions"`
	Alias          *Alias                `json:"alias,omitempty"`
}

func (n *MatchRecognizeClause) Children() []Node {
	return children(n.Options, n.PartitionBy, n.OrderBy, n.Measures,
		n.AfterMatchSkip, n.Pattern, n.Definitions, n.Alias)
}

// AfterMatchSkipClause is the "AFTER MATCH SKIP ..." clause inside
// MATCH_RECOGNIZE; see ASTAfterMatchSkipClause in
// googlesql/parser/parse_tree.h. TargetType is "PAST_LAST_ROW" or
// "TO_NEXT_ROW". Its span covers only the skip target (e.g. "TO NEXT ROW").
type AfterMatchSkipClause struct {
	Span
	TargetType string `json:"target_type"`
}

func (n *AfterMatchSkipClause) Children() []Node { return nil }

// PipeMatchRecognize is a |> MATCH_RECOGNIZE pipe operator; see
// ASTPipeMatchRecognize in googlesql/parser/parse_tree.h.
type PipeMatchRecognize struct {
	Span
	Clause *MatchRecognizeClause `json:"clause"`
}

func (n *PipeMatchRecognize) Children() []Node {
	return children(n.Clause)
}

// PipePivot is a |> PIVOT pipe operator; see ASTPipePivot in
// googlesql/parser/parse_tree.h. The PivotClause carries the optional alias.
type PipePivot struct {
	Span
	Pivot *PivotClause `json:"pivot"`
}

func (n *PipePivot) Children() []Node {
	return children(n.Pivot)
}

// PipeUnpivot is a |> UNPIVOT pipe operator; see ASTPipeUnpivot in
// googlesql/parser/parse_tree.h. The UnpivotClause carries the optional alias.
type PipeUnpivot struct {
	Span
	Unpivot *UnpivotClause `json:"unpivot"`
}

func (n *PipeUnpivot) Children() []Node {
	return children(n.Unpivot)
}

// PipeCall is a |> CALL pipe operator invoking a table-valued function; see
// ASTPipeCall in googlesql/parser/parse_tree.h. Any alias is carried on the
// TVF node.
type PipeCall struct {
	Span
	Call *TVF `json:"call"`
}

func (n *PipeCall) Children() []Node {
	return children(n.Call)
}

// PipeWith is a |> WITH pipe operator carrying a WITH clause; see ASTPipeWith
// in googlesql/parser/parse_tree.h.
type PipeWith struct {
	Span
	With *WithClause `json:"with"`
}

func (n *PipeWith) Children() []Node {
	return children(n.With)
}

// PipeInsert is a |> INSERT pipe operator carrying an INSERT statement without
// a source (the rows come from the pipe input); see ASTPipeInsert in
// googlesql/parser/parse_tree.h.
type PipeInsert struct {
	Span
	Insert *InsertStatement `json:"insert"`
}

func (n *PipeInsert) Children() []Node {
	return children(n.Insert)
}

// PipeRecursiveUnion is a |> RECURSIVE UNION pipe operator; see
// ASTPipeRecursiveUnion in googlesql/parser/parse_tree.h. Input is either a
// parenthesized Query or a Subpipeline.
type PipeRecursiveUnion struct {
	Span
	Metadata *SetOperationMetadata   `json:"metadata"`
	Depth    *RecursionDepthModifier `json:"depth,omitempty"`
	Input    Node                    `json:"input"`
	Alias    *Alias                  `json:"alias,omitempty"`
}

func (n *PipeRecursiveUnion) Children() []Node {
	out := children(n.Metadata)
	if n.Depth != nil {
		out = append(out, n.Depth)
	}
	out = append(out, n.Input)
	if n.Alias != nil {
		out = append(out, n.Alias)
	}
	return out
}

// PipeExportData is a |> EXPORT DATA pipe operator; see ASTPipeExportData in
// googlesql/parser/parse_tree.h. The ExportDataStatement never has a query
// when used as a pipe operator.
type PipeExportData struct {
	Span
	ExportData *ExportDataStatement `json:"export_data"`
}

func (n *PipeExportData) Children() []Node {
	return children(n.ExportData)
}

// PipeCreateTable is a |> CREATE TABLE pipe operator; see ASTPipeCreateTable
// in googlesql/parser/parse_tree.h. The CreateTableStatement never has an
// AS query when used as a pipe operator.
type PipeCreateTable struct {
	Span
	CreateTable *CreateTableStatement `json:"create_table"`
}

func (n *PipeCreateTable) Children() []Node {
	return children(n.CreateTable)
}

// PipeFork is a |> FORK pipe operator with an optional hint and one or more
// subpipelines; see ASTPipeFork in googlesql/parser/parse_tree.h.
type PipeFork struct {
	Span
	Hint         *Hint          `json:"hint,omitempty"`
	Subpipelines []*Subpipeline `json:"subpipelines"`
}

func (n *PipeFork) Children() []Node {
	var out []Node
	if n.Hint != nil {
		out = append(out, n.Hint)
	}
	for _, sub := range n.Subpipelines {
		out = append(out, sub)
	}
	return out
}

// PipeTee is a |> TEE pipe operator with an optional hint and zero or more
// subpipelines; see ASTPipeTee in googlesql/parser/parse_tree.h.
type PipeTee struct {
	Span
	Hint         *Hint          `json:"hint,omitempty"`
	Subpipelines []*Subpipeline `json:"subpipelines,omitempty"`
}

func (n *PipeTee) Children() []Node {
	var out []Node
	if n.Hint != nil {
		out = append(out, n.Hint)
	}
	for _, sub := range n.Subpipelines {
		out = append(out, sub)
	}
	return out
}

// PipeIf is a |> IF pipe operator with an optional hint, one or more
// IF/ELSEIF cases, and an optional final ELSE subpipeline; see ASTPipeIf in
// googlesql/parser/parse_tree.h.
type PipeIf struct {
	Span
	Hint  *Hint         `json:"hint,omitempty"`
	Cases []*PipeIfCase `json:"cases"`
	Else  *Subpipeline  `json:"else,omitempty"`
}

func (n *PipeIf) Children() []Node {
	var out []Node
	if n.Hint != nil {
		out = append(out, n.Hint)
	}
	for _, c := range n.Cases {
		out = append(out, c)
	}
	if n.Else != nil {
		out = append(out, n.Else)
	}
	return out
}

// PipeIfCase is a single IF/ELSEIF branch of a |> IF pipe operator: a
// condition expression and the subpipeline run when it is true; see
// ASTPipeIfCase in googlesql/parser/parse_tree.h.
type PipeIfCase struct {
	Span
	Condition Node         `json:"condition"`
	Body      *Subpipeline `json:"body"`
}

func (n *PipeIfCase) Children() []Node {
	return children(n.Condition, n.Body)
}

// RowPatternOperation is an alternation or concatenation of row pattern
// expressions inside PATTERN(...); see ASTRowPatternOperation in
// googlesql/parser/parse_tree.h. OpType is "ALTERNATE" or "CONCAT".
type RowPatternOperation struct {
	Span
	OpType        string `json:"op_type"`
	Parenthesized bool   `json:"parenthesized,omitempty"`
	Operands      []Node `json:"operands"`
}

func (n *RowPatternOperation) Children() []Node {
	return append([]Node(nil), n.Operands...)
}

// EmptyRowPattern is an empty row pattern, e.g. in "PATTERN ()" or between
// alternation bars; see ASTEmptyRowPattern in googlesql/parser/parse_tree.h.
type EmptyRowPattern struct {
	Span
	Parenthesized bool `json:"parenthesized,omitempty"`
}

func (n *EmptyRowPattern) Children() []Node { return nil }

// RowPatternVariable is a pattern variable reference inside PATTERN(...);
// see ASTRowPatternVariable in googlesql/parser/parse_tree.h.
type RowPatternVariable struct {
	Span
	Name *Identifier `json:"name"`
}

func (n *RowPatternVariable) Children() []Node {
	return children(n.Name)
}

// RowPatternAnchor is a "^" (START) or "$" (END) anchor inside
// PATTERN(...); see ASTRowPatternAnchor in googlesql/parser/parse_tree.h.
type RowPatternAnchor struct {
	Span
	Anchor string `json:"anchor"`
}

func (n *RowPatternAnchor) Children() []Node { return nil }

// RowPatternQuantification is a quantified row pattern primary, e.g. "A+"
// or "(A B){1,2}"; see ASTRowPatternQuantification in
// googlesql/parser/parse_tree.h.
type RowPatternQuantification struct {
	Span
	Primary    Node `json:"primary"`
	Quantifier Node `json:"quantifier"`
}

func (n *RowPatternQuantification) Children() []Node {
	return children(n.Primary, n.Quantifier)
}

// SymbolQuantifier is a "?", "+", or "*" quantifier; see ASTSymbolQuantifier
// in googlesql/parser/parse_tree.h. Symbol is "QUESTION_MARK", "PLUS", or
// "STAR".
type SymbolQuantifier struct {
	Span
	Symbol      string `json:"symbol"`
	IsReluctant bool   `json:"is_reluctant,omitempty"`
}

func (n *SymbolQuantifier) Children() []Node { return nil }

// FixedQuantifier is a "{n}" quantifier; see ASTFixedQuantifier in
// googlesql/parser/parse_tree.h.
type FixedQuantifier struct {
	Span
	Bound Node `json:"bound"`
}

func (n *FixedQuantifier) Children() []Node {
	return children(n.Bound)
}

// QuantifierBound is one bound of a bounded quantifier; its span covers the
// whole "{lo,hi}" range and its child is the bound expression, if any. See
// ASTQuantifierBound in googlesql/parser/parse_tree.h.
type QuantifierBound struct {
	Span
	Bound Node `json:"bound,omitempty"`
}

func (n *QuantifierBound) Children() []Node {
	return children(n.Bound)
}

// BoundedQuantifier is a "{lo,hi}" quantifier where either bound may be
// omitted; see ASTBoundedQuantifier in googlesql/parser/parse_tree.h.
type BoundedQuantifier struct {
	Span
	LowerBound  *QuantifierBound `json:"lower_bound"`
	UpperBound  *QuantifierBound `json:"upper_bound"`
	IsReluctant bool             `json:"is_reluctant,omitempty"`
}

func (n *BoundedQuantifier) Children() []Node {
	return children(n.LowerBound, n.UpperBound)
}

// -----------------------------------------------------------------------------
// GoogleSQL graph (GQL / GRAPH_TABLE) query nodes.
//
// These mirror the ASTGql*/ASTGraph* node families in
// googlesql/parser/parse_tree.h and their SingleNodeDebugString output in
// parse_tree.cc. They form the foundation of the graph query feature: the
// GRAPH statement (gql_query), the GRAPH_TABLE(...) table expression, and the
// core graph pattern nodes (path/node/edge patterns, labels, and the linear
// GQL operators MATCH/LET/FILTER/RETURN). See the graph_* productions in
// googlesql.tm.
// -----------------------------------------------------------------------------

// GqlQuery wraps a graph query used as a query expression; see ASTGqlQuery in
// googlesql/parser/parse_tree.h. Its single child is a GraphTableQuery.
type GqlQuery struct {
	Span
	Query Node `json:"query"`
}

func (n *GqlQuery) Children() []Node { return children(n.Query) }

// GraphTableQuery is either "GRAPH name <operations>" (as a statement) or
// "GRAPH_TABLE( name <match> [COLUMNS(...)] )" / "GRAPH_TABLE( name
// <operations> )" (as a table expression); see ASTGraphTableQuery in
// googlesql/parser/parse_tree.h. Op is a *GqlMatch (single-match COLUMNS form)
// or a *GqlOperatorList (linear/composite form); Shape is the COLUMNS select
// list when present.
type GraphTableQuery struct {
	Span
	Graph            *PathExpression `json:"graph"`
	Op               Node            `json:"op"`
	Shape            *SelectList     `json:"shape,omitempty"`
	Alias            *Alias          `json:"alias,omitempty"`
	PostfixOperators []Node          `json:"postfix_operators,omitempty"`
}

func (n *GraphTableQuery) Children() []Node {
	out := children(n.Graph, n.Op, n.Shape, n.Alias)
	return append(out, n.PostfixOperators...)
}

// GqlOperatorList is a list of GQL linear operators; see ASTGqlOperatorList in
// googlesql/parser/parse_tree.h. The top-level list holds one nested
// GqlOperatorList per NEXT-separated composite block.
type GqlOperatorList struct {
	Span
	Operators []Node `json:"operators"`
}

func (n *GqlOperatorList) Children() []Node { return append([]Node(nil), n.Operators...) }

// GqlMatch is a "MATCH <graph_pattern>" operator (or "OPTIONAL MATCH ...");
// see ASTGqlMatch in googlesql/parser/parse_tree.h.
type GqlMatch struct {
	Span
	Pattern  *GraphPattern `json:"pattern"`
	Hint     *Hint         `json:"hint,omitempty"`
	Optional bool          `json:"optional,omitempty"`
}

func (n *GqlMatch) Children() []Node { return children(n.Pattern, n.Hint) }

// GqlLet is a "LET <definitions>" operator; see ASTGqlLet in
// googlesql/parser/parse_tree.h.
type GqlLet struct {
	Span
	Definitions *GqlLetVariableDefinitionList `json:"definitions"`
}

func (n *GqlLet) Children() []Node { return children(n.Definitions) }

// GqlLetVariableDefinitionList is the comma-separated list of variable
// definitions in a LET operator; see ASTGqlLetVariableDefinitionList in
// googlesql/parser/parse_tree.h.
type GqlLetVariableDefinitionList struct {
	Span
	Definitions []*GqlLetVariableDefinition `json:"definitions"`
}

func (n *GqlLetVariableDefinitionList) Children() []Node {
	out := make([]Node, 0, len(n.Definitions))
	for _, d := range n.Definitions {
		out = append(out, d)
	}
	return out
}

// GqlLetVariableDefinition is a single "name = expression" binding; see
// ASTGqlLetVariableDefinition in googlesql/parser/parse_tree.h.
type GqlLetVariableDefinition struct {
	Span
	Name *Identifier `json:"name"`
	Expr Node        `json:"expr"`
}

func (n *GqlLetVariableDefinition) Children() []Node { return children(n.Name, n.Expr) }

// GqlFilter is a "FILTER [WHERE] <expr>" operator; see ASTGqlFilter in
// googlesql/parser/parse_tree.h. Its single child is a WhereClause.
type GqlFilter struct {
	Span
	Where *WhereClause `json:"where"`
}

func (n *GqlFilter) Children() []Node { return children(n.Where) }

// GqlReturn is a "RETURN <items>" operator; see ASTGqlReturn in
// googlesql/parser/parse_tree.h. Its first child is a Select holding the return
// item list; OrderByPage holds an optional trailing ORDER BY / OFFSET / LIMIT.
type GqlReturn struct {
	Span
	Select      *Select            `json:"select"`
	OrderByPage *GqlOrderByAndPage `json:"order_by_and_page,omitempty"`
}

func (n *GqlReturn) Children() []Node { return children(n.Select, n.OrderByPage) }

// GqlSample is a "TABLESAMPLE ..." operator in a graph linear query; see
// ASTGqlSample in googlesql/parser/parse_tree.h. Its single child is a
// SampleClause.
type GqlSample struct {
	Span
	Sample *SampleClause `json:"sample"`
}

func (n *GqlSample) Children() []Node { return children(n.Sample) }

// GqlWith is a "WITH [ALL|DISTINCT] [hint] <items> [GROUP BY ...]" accumulate
// operator; see ASTGqlWith in googlesql/parser/parse_tree.h. Its single child
// is a Select holding the item list (and optional hint / group by).
type GqlWith struct {
	Span
	Select *Select `json:"select"`
}

func (n *GqlWith) Children() []Node { return children(n.Select) }

// GqlFor is a "FOR <name> IN <expr> [WITH OFFSET [AS alias]]" operator; see
// ASTGqlFor in googlesql/parser/parse_tree.h.
type GqlFor struct {
	Span
	Name       *Identifier `json:"name"`
	Expr       Node        `json:"expression"`
	WithOffset *WithOffset `json:"with_offset,omitempty"`
}

func (n *GqlFor) Children() []Node { return children(n.Name, n.Expr, n.WithOffset) }

// GqlNamedCall is a "CALL [PER(...)] tvf [YIELD ...]" operator; see
// ASTGqlNamedCall in googlesql/parser/parse_tree.h. Children are the TVF, an
// optional YIELD item list, and an optional PER capture list.
type GqlNamedCall struct {
	Span
	Optional      bool            `json:"optional,omitempty"`
	IsPartitioned bool            `json:"is_partitioned,omitempty"`
	TVF           *TVF            `json:"tvf"`
	Yield         *YieldItemList  `json:"yield,omitempty"`
	Per           *IdentifierList `json:"per,omitempty"`
}

func (n *GqlNamedCall) Children() []Node { return children(n.TVF, n.Yield, n.Per) }

// GqlInlineSubqueryCall is a "CALL [PER(...)|(captures)] { subquery }"
// operator; see ASTGqlInlineSubqueryCall in googlesql/parser/parse_tree.h.
// Children are the braced graph subquery and an optional capture / PER
// identifier list.
type GqlInlineSubqueryCall struct {
	Span
	Optional      bool            `json:"optional,omitempty"`
	IsPartitioned bool            `json:"is_partitioned,omitempty"`
	Subquery      *Query          `json:"subquery"`
	Captures      *IdentifierList `json:"captures,omitempty"`
}

func (n *GqlInlineSubqueryCall) Children() []Node { return children(n.Subquery, n.Captures) }

// YieldItemList is the "YIELD <item>, ..." list of a named CALL operator; see
// ASTYieldItemList in googlesql/parser/parse_tree.h.
type YieldItemList struct {
	Span
	Items []*ExpressionWithOptAlias `json:"items"`
}

func (n *YieldItemList) Children() []Node {
	out := make([]Node, 0, len(n.Items))
	for _, it := range n.Items {
		out = append(out, it)
	}
	return out
}

// GqlGraphPatternQuery is the query body of an "EXISTS { [GRAPH g]
// graph_pattern }" subquery; see ASTGqlGraphPatternQuery in
// googlesql/parser/parse_tree.h. Children are an optional graph reference and
// the graph pattern.
type GqlGraphPatternQuery struct {
	Span
	Graph   *PathExpression `json:"graph,omitempty"`
	Pattern *GraphPattern   `json:"graph_pattern"`
}

func (n *GqlGraphPatternQuery) Children() []Node { return children(n.Graph, n.Pattern) }

// GqlLinearOpsQuery is the query body of an "EXISTS { [GRAPH g]
// linear_operator_list }" subquery; see ASTGqlLinearOpsQuery in
// googlesql/parser/parse_tree.h. Children are an optional graph reference and
// the linear operator list.
type GqlLinearOpsQuery struct {
	Span
	Graph *PathExpression  `json:"graph,omitempty"`
	Ops   *GqlOperatorList `json:"linear_ops"`
}

func (n *GqlLinearOpsQuery) Children() []Node { return children(n.Graph, n.Ops) }

// GraphPattern is a comma-separated list of path patterns with an optional
// trailing WHERE clause; see ASTGraphPattern in
// googlesql/parser/parse_tree.h.
type GraphPattern struct {
	Span
	Paths []*GraphPathPattern `json:"paths"`
	Where *WhereClause        `json:"where,omitempty"`
}

func (n *GraphPattern) Children() []Node {
	out := make([]Node, 0, len(n.Paths)+1)
	for _, p := range n.Paths {
		out = append(out, p)
	}
	if n.Where != nil {
		out = append(out, n.Where)
	}
	return out
}

// GraphPathPattern is a sequence of node/edge (path factor) patterns forming a
// path; see ASTGraphPathPattern in googlesql/parser/parse_tree.h. When
// Parenthesized is set the debug name is prefixed with "Parenthesized".
// PathName is the optional "<identifier> =" path-variable assignment and
// SearchPrefix is the optional path search prefix (ANY / ALL / SHORTEST ...);
// both, when present, appear before the path factors.
type GraphPathPattern struct {
	Span
	PathName      *Identifier            `json:"path_name,omitempty"`
	SearchPrefix  *GraphPathSearchPrefix `json:"search_prefix,omitempty"`
	Factors       []Node                 `json:"factors"`
	Parenthesized bool                   `json:"parenthesized,omitempty"`
}

func (n *GraphPathPattern) Children() []Node {
	out := make([]Node, 0, len(n.Factors)+2)
	if n.PathName != nil {
		out = append(out, n.PathName)
	}
	if n.SearchPrefix != nil {
		out = append(out, n.SearchPrefix)
	}
	out = append(out, n.Factors...)
	return out
}

// GraphPathSearchPrefix is a path search prefix that restricts a graph pattern
// match by selecting paths from each partition of endpoints; see
// ASTGraphPathSearchPrefix in googlesql/parser/parse_tree.h. Type is one of
// "ANY", "SHORTEST", "ALL", "ALL_SHORTEST", "CHEAPEST", or "ALL_CHEAPEST" (not
// shown in the debug string). Count, when present, holds the number of paths.
type GraphPathSearchPrefix struct {
	Span
	Type  string                      `json:"type"`
	Count *GraphPathSearchPrefixCount `json:"count,omitempty"`
}

func (n *GraphPathSearchPrefix) Children() []Node {
	if n.Count != nil {
		return []Node{n.Count}
	}
	return nil
}

// GraphPathSearchPrefixCount holds the number of paths to retain from each
// partition of a path search prefix; see ASTGraphPathSearchPrefixCount in
// googlesql/parser/parse_tree.h.
type GraphPathSearchPrefixCount struct {
	Span
	PathCount Node `json:"path_count"`
}

func (n *GraphPathSearchPrefixCount) Children() []Node { return children(n.PathCount) }

// GraphNodePattern is a "(<filler>)" node pattern; see ASTGraphNodePattern in
// googlesql/parser/parse_tree.h.
type GraphNodePattern struct {
	Span
	Filler *GraphElementPatternFiller `json:"filler"`
}

func (n *GraphNodePattern) Children() []Node { return children(n.Filler) }

// GraphEdgePattern is an edge pattern such as "-[e]->", "<-[e]-", "-", "->",
// or "<-"; see ASTGraphEdgePattern in googlesql/parser/parse_tree.h. Filler is
// nil for abbreviated edges. Orientation is "ANY", "LEFT", or "RIGHT" (not
// shown in the debug string).
type GraphEdgePattern struct {
	Span
	LhsHint     *GraphLhsHint              `json:"lhs_hint,omitempty"`
	RhsHint     *GraphRhsHint              `json:"rhs_hint,omitempty"`
	Filler      *GraphElementPatternFiller `json:"filler,omitempty"`
	Orientation string                     `json:"orientation,omitempty"`
}

func (n *GraphEdgePattern) Children() []Node {
	return children(n.LhsHint, n.RhsHint, n.Filler)
}

// GraphLhsHint is a hint attached to the left-hand side of an edge pattern
// (i.e. a hint appearing before the edge); see ASTGraphLhsHint in
// googlesql/parser/parse_tree.h. Its single child is the Hint.
type GraphLhsHint struct {
	Span
	Hint *Hint `json:"hint"`
}

func (n *GraphLhsHint) Children() []Node { return children(n.Hint) }

// GraphRhsHint is a hint attached to the right-hand side of an edge pattern
// (i.e. a hint appearing after the edge); see ASTGraphRhsHint in
// googlesql/parser/parse_tree.h. Its single child is the Hint.
type GraphRhsHint struct {
	Span
	Hint *Hint `json:"hint"`
}

func (n *GraphRhsHint) Children() []Node { return children(n.Hint) }

// GraphElementPatternFiller holds the optional variable name, label filter,
// and WHERE clause inside a node or edge pattern; see
// ASTGraphElementPatternFiller in googlesql/parser/parse_tree.h.
type GraphElementPatternFiller struct {
	Span
	Name     *Identifier                 `json:"name,omitempty"`
	Label    *GraphLabelFilter           `json:"label,omitempty"`
	PropSpec *GraphPropertySpecification `json:"prop_spec,omitempty"`
	Where    *WhereClause                `json:"where,omitempty"`
	Hint     *Hint                       `json:"hint,omitempty"`
	// Cost is the optional "COST <expression>" edge/element weight; see
	// opt_graph_cost in googlesql.tm. It stores the expression node directly
	// (the COST keyword is not represented).
	Cost Node `json:"cost,omitempty"`
}

func (n *GraphElementPatternFiller) Children() []Node {
	return children(n.Name, n.Label, n.PropSpec, n.Where, n.Hint, n.Cost)
}

// GraphPropertySpecification is the "{ name: value, ... }" property list in a
// node or edge pattern filler; see ASTGraphPropertySpecification in
// googlesql/parser/parse_tree.h.
type GraphPropertySpecification struct {
	Span
	Properties []*GraphPropertyNameAndValue `json:"properties"`
}

func (n *GraphPropertySpecification) Children() []Node {
	out := make([]Node, 0, len(n.Properties))
	for _, prop := range n.Properties {
		out = append(out, prop)
	}
	return out
}

// GraphPropertyNameAndValue is a single "name: value" property; see
// ASTGraphPropertyNameAndValue in googlesql/parser/parse_tree.h.
type GraphPropertyNameAndValue struct {
	Span
	Name  *Identifier `json:"name"`
	Value Node        `json:"value"`
}

func (n *GraphPropertyNameAndValue) Children() []Node { return children(n.Name, n.Value) }

// GraphPathMode is a "WALK"/"TRAIL"/"SIMPLE"/"ACYCLIC" path mode keyword; see
// ASTGraphPathMode in googlesql/parser/parse_tree.h. The mode keyword is not
// shown in the debug string.
type GraphPathMode struct {
	Span
	Mode string `json:"mode,omitempty"`
}

func (n *GraphPathMode) Children() []Node { return nil }

// GraphLabelFilter is an "IS <label_expr>" or ":<label_expr>" clause; see
// ASTGraphLabelFilter in googlesql/parser/parse_tree.h.
type GraphLabelFilter struct {
	Span
	Expr Node `json:"expr"`
}

func (n *GraphLabelFilter) Children() []Node { return children(n.Expr) }

// GraphElementLabel is a single label name in a label expression; see
// ASTGraphElementLabel in googlesql/parser/parse_tree.h.
type GraphElementLabel struct {
	Span
	Name *Identifier `json:"name"`
}

func (n *GraphElementLabel) Children() []Node { return children(n.Name) }

// GraphWildcardLabel is the "%" wildcard label; see ASTGraphWildcardLabel in
// googlesql/parser/parse_tree.h.
type GraphWildcardLabel struct {
	Span
}

func (n *GraphWildcardLabel) Children() []Node { return nil }

// GraphLabelOperation is a "!"/"&"/"|" label expression; Op is "NOT", "AND",
// or "OR". See ASTGraphLabelOperation in googlesql/parser/parse_tree.h.
// Parenthesized records that the expression was written in parentheses, which
// prevents flattening of adjacent same-operator operands.
type GraphLabelOperation struct {
	Span
	Op            string `json:"op"`
	Operands      []Node `json:"operands"`
	Parenthesized bool   `json:"parenthesized,omitempty"`
}

func (n *GraphLabelOperation) Children() []Node { return append([]Node(nil), n.Operands...) }

// GraphIsLabeledPredicate is an "<operand> IS [NOT] LABELED <label_expr>"
// predicate; see ASTGraphIsLabeledPredicate in googlesql/parser/parse_tree.h.
// IsNot records the NOT; it is not shown in the debug string. Children are the
// operand expression followed by the label expression.
type GraphIsLabeledPredicate struct {
	Span
	IsNot   bool `json:"is_not,omitempty"`
	Operand Node `json:"operand"`
	Label   Node `json:"label_expression"`
}

func (n *GraphIsLabeledPredicate) Children() []Node { return children(n.Operand, n.Label) }

// GqlSetOperation is a GQL composite query: a set operation (UNION / INTERSECT
// / EXCEPT) between two or more linear query operations; see ASTGqlSetOperation
// in googlesql/parser/parse_tree.h. Metadata holds one entry per operator and
// Inputs holds the GqlOperatorList operands.
type GqlSetOperation struct {
	Span
	Metadata *SetOperationMetadataList `json:"metadata"`
	Inputs   []Node                    `json:"inputs"`
}

func (n *GqlSetOperation) Children() []Node {
	out := children(n.Metadata)
	return append(out, n.Inputs...)
}

// GqlOrderByAndPage is a GQL ORDER BY and/or paging (OFFSET/LIMIT) operator;
// see ASTGqlOrderByAndPage in googlesql/parser/parse_tree.h. Either or both of
// OrderBy and Page may be present.
type GqlOrderByAndPage struct {
	Span
	OrderBy *OrderBy `json:"order_by,omitempty"`
	Page    *GqlPage `json:"page,omitempty"`
}

func (n *GqlOrderByAndPage) Children() []Node { return children(n.OrderBy, n.Page) }

// GqlPage holds a GQL OFFSET/SKIP and/or LIMIT clause; see ASTGqlPage in
// googlesql/parser/parse_tree.h.
type GqlPage struct {
	Span
	Offset *GqlPageOffset `json:"offset,omitempty"`
	Limit  *GqlPageLimit  `json:"limit,omitempty"`
}

func (n *GqlPage) Children() []Node { return children(n.Offset, n.Limit) }

// GqlPageOffset is a GQL "OFFSET <value>" or "SKIP <value>" clause; see
// ASTGqlPageOffset in googlesql/parser/parse_tree.h.
type GqlPageOffset struct {
	Span
	Value Node `json:"value"`
}

func (n *GqlPageOffset) Children() []Node { return children(n.Value) }

// GqlPageLimit is a GQL "LIMIT <value>" clause; see ASTGqlPageLimit in
// googlesql/parser/parse_tree.h.
type GqlPageLimit struct {
	Span
	Value Node `json:"value"`
}

func (n *GqlPageLimit) Children() []Node { return children(n.Value) }
