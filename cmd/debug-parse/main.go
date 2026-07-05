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
	feats := flag.String("features", "NONE", "")
	noloc := flag.Bool("noloc", false, "")
	mode := flag.String("mode", "statement", "statement or script")
	macro := flag.String("macro", "none", "macro_expansion_mode: none, lenient, or strict")
	reserve := flag.Bool("reserve_graph_table", false, "reserve GRAPH_TABLE keyword")
	flag.Parse()
	sql := strings.Join(flag.Args(), " ")
	var opts parser.Options
	opts.Features = parser.ParseFeatureSet(*feats)
	opts.MacroExpansionMode = *macro
	opts.ReserveGraphTable = *reserve
	var (
		node ast.Node
		err  error
	)
	if *mode == "script" {
		node, err = parser.ParseScriptWithOptions(sql, opts)
	} else {
		node, err = parser.ParseStatementWithOptions(sql, opts)
	}
	if err != nil {
		var perr *parser.Error
		if errors.As(err, &perr) {
			fmt.Println("ERROR: " + perr.Caret())
			os.Exit(0)
		}
		fmt.Println("ERROR: " + err.Error())
		os.Exit(0)
	}
	fmt.Println(dump.Tree(node, dump.Options{SQL: sql, ShowLocationText: !*noloc}))
}
