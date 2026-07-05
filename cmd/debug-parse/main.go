// Command debug-parse parses the SQL given as command-line arguments and
// prints the AST debug string (or the parse error with its caret rendering).
// It exits 1 when the input fails to parse, so shell scripts can detect
// failures; the printed "ERROR: ..." output still matches the golden-file
// format either way.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sqlc-dev/zetajones/ast"
	"github.com/sqlc-dev/zetajones/internal/dump"
	"github.com/sqlc-dev/zetajones/parser"
)

func main() {
	feats := flag.String("features", "NONE", "language_features spec (e.g. NONE, MAXIMUM, NONE,+SQL_GRAPH)")
	noloc := flag.Bool("noloc", false, "omit the bracketed source text after each location range")
	mode := flag.String("mode", "statement", "parse mode: statement, script, expression, or type")
	macro := flag.String("macro", "none", "macro_expansion_mode: none, lenient, or strict")
	reserve := flag.Bool("reserve_graph_table", false, "reserve the GRAPH_TABLE keyword")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: debug-parse [flags] SQL...")
		flag.PrintDefaults()
		os.Exit(2)
	}
	sql := strings.Join(flag.Args(), " ")
	var opts parser.Options
	opts.Features = parser.ParseFeatureSet(*feats)
	opts.MacroExpansionMode = *macro
	opts.ReserveGraphTable = *reserve
	var (
		node ast.Node
		err  error
	)
	switch *mode {
	case "statement":
		node, err = parser.ParseStatementWithOptions(sql, opts)
	case "script":
		node, err = parser.ParseScriptWithOptions(sql, opts)
	case "expression":
		node, err = parser.ParseExpressionWithOptions(sql, opts)
	case "type":
		node, err = parser.ParseTypeWithOptions(sql, opts)
	default:
		fmt.Fprintf(os.Stderr, "debug-parse: unknown -mode %q (want statement, script, expression, or type)\n", *mode)
		os.Exit(2)
	}
	if err != nil {
		var perr *parser.Error
		if errors.As(err, &perr) {
			fmt.Println("ERROR: " + perr.Caret())
		} else {
			fmt.Println("ERROR: " + err.Error())
		}
		os.Exit(1)
	}
	fmt.Println(dump.Tree(node, dump.Options{SQL: sql, ShowLocationText: !*noloc}))
}
