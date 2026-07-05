package parser_test

import (
	"path/filepath"
	"testing"

	"github.com/sqlc-dev/zetajones/internal/testfile"
	"github.com/sqlc-dev/zetajones/parser"
)

// FuzzParse feeds arbitrary input through every parse mode. The parser must
// never panic: invalid input is expected to produce an error, valid input a
// tree. Both feature extremes are exercised (NONE and MAXIMUM with
// reserve_graph_table) since feature gating selects different code paths.
func FuzzParse(f *testing.F) {
	// Seed with the golden corpus so the fuzzer starts from inputs that reach
	// deep into the grammar.
	files, _ := filepath.Glob(filepath.Join("testdata", "*.test"))
	seeded := 0
	for _, path := range files {
		cases, err := testfile.ParseFile(path)
		if err != nil {
			continue
		}
		for _, c := range cases {
			// Cap the seed corpus; the fuzzer mutates from here.
			if seeded >= 2000 {
				break
			}
			f.Add(c.SQL)
			seeded++
		}
	}
	f.Add("SELECT 1")
	f.Add("graph g match (a)-[e]->(b) return a")
	f.Add("`")
	f.Add("-- comment\n")
	f.Add(`"unterminated`)

	maxOpts := parser.Options{
		Features:           parser.ParseFeatureSet("MAXIMUM"),
		ReserveGraphTable:  true,
		MacroExpansionMode: "lenient",
	}
	var noneOpts parser.Options
	noneOpts.Features = parser.ParseFeatureSet("NONE")

	f.Fuzz(func(t *testing.T, sql string) {
		for _, opts := range []parser.Options{noneOpts, maxOpts} {
			_, _ = parser.ParseStatementWithOptions(sql, opts)
			_, _ = parser.ParseScriptWithOptions(sql, opts)
			_, _ = parser.ParseExpressionWithOptions(sql, opts)
			_, _ = parser.ParseTypeWithOptions(sql, opts)
			_, _ = parser.ParseMultipleWithOptions(sql, opts)
		}
	})
}
