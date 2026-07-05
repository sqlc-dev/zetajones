package parser_test

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sqlc-dev/zetajones/ast"
	"github.com/sqlc-dev/zetajones/internal/dump"
	"github.com/sqlc-dev/zetajones/internal/testfile"
	"github.com/sqlc-dev/zetajones/parser"
)

// checkParse runs cases listed in parse_todo to see which ones now pass, and
// updates the metadata files. Use with:
//
//	go test ./parser -check-parse -v 2>&1 | grep "PARSE PASSES NOW"
var checkParse = flag.Bool("check-parse", false, "Run parse_todo cases and update metadata for ones that now pass")

// runCase parses the case's SQL and returns the rendered output in the same
// shape as the expected section: either the parse tree debug string or an
// "ERROR: ..." message.
func runCase(c *testfile.Case) string {
	var opts parser.Options
	// The last language_features option wins: case options follow inherited
	// [default ...] options. The reference test driver defaults to NONE; see
	// run_parser_test.cc.
	opts.Features = parser.ParseFeatureSet("NONE")
	// The reference test driver defaults macro_expansion_mode to "none"; see
	// run_parser_test.cc.
	opts.MacroExpansionMode = "none"
	mode := "statement"
	for _, opt := range c.Options {
		if spec, ok := strings.CutPrefix(opt, "language_features="); ok {
			opts.Features = parser.ParseFeatureSet(spec)
		}
		if m, ok := strings.CutPrefix(opt, "mode="); ok {
			mode = m
		}
		if spec, ok := strings.CutPrefix(opt, "macro_expansion_mode="); ok {
			opts.MacroExpansionMode = spec
		}
		// GRAPH_TABLE is a conditionally reserved keyword; the graph_* test
		// files enable it via [default reserve_graph_table] and individual
		// cases may override with [no_reserve_graph_table]. See
		// reserve_graph_table in run_parser_test.cc.
		if opt == "reserve_graph_table" {
			opts.ReserveGraphTable = true
		}
		if opt == "no_reserve_graph_table" {
			opts.ReserveGraphTable = false
		}
		// The reference driver splits these configs on "," (an empty config
		// yields a single empty element); see run_parser_test.cc.
		if spec, ok := strings.CutPrefix(opt, "supported_generic_entity_types="); ok {
			opts.SupportedGenericEntityTypes = strings.Split(spec, ",")
		}
		if spec, ok := strings.CutPrefix(opt, "supported_generic_sub_entity_types="); ok {
			opts.SupportedGenericSubEntityTypes = strings.Split(spec, ",")
		}
	}
	var (
		node ast.Node
		err  error
	)
	switch mode {
	case "type":
		node, err = parser.ParseTypeWithOptions(c.SQL, opts)
	case "script":
		node, err = parser.ParseScriptWithOptions(c.SQL, opts)
	case "expression":
		node, err = parser.ParseExpressionWithOptions(c.SQL, opts)
	default:
		var stmt ast.Statement
		stmt, err = parser.ParseStatementWithOptions(c.SQL, opts)
		node = stmt
	}
	if err != nil {
		var perr *parser.Error
		if errors.As(err, &perr) {
			return "ERROR: " + perr.Caret()
		}
		return "ERROR: " + err.Error()
	}
	return dump.Tree(node, dump.Options{
		SQL:              c.SQL,
		ShowLocationText: !c.HasOption("no_show_parse_location_text"),
	})
}

func TestParser(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.test"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no .test files found in testdata")
	}

	for _, path := range files {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".test"), func(t *testing.T) {
			t.Parallel()
			testFile(t, path)
		})
	}
}

func testFile(t *testing.T, path string) {
	meta, hasMeta, err := testfile.LoadMetadata(path)
	if err != nil {
		t.Fatalf("reading metadata: %v", err)
	}
	if meta.Skip != "" {
		t.Skipf("skipped: %s", meta.Skip)
	}
	cases, err := testfile.ParseFile(path)
	if err != nil {
		t.Fatalf("parsing test file: %v", err)
	}

	// Bootstrap mode: with -check-parse and no metadata file yet, record
	// every failing case as todo.
	bootstrap := *checkParse && !hasMeta
	// Ensure the maps are writable (a loaded sidecar may omit either key).
	// Empty maps are dropped on save via omitempty.
	if meta.ParseTodo == nil {
		meta.ParseTodo = map[string]bool{}
	}
	if meta.Alternations == nil {
		meta.Alternations = map[string]bool{}
	}

	changed := false

	// check runs one named case with bootstrap/check-parse/normal semantics.
	// Alternation expansions reuse this via names like "case_5.alt_2".
	check := func(name string, c *testfile.Case) {
		isTodo := meta.ParseTodo[name]
		switch {
		case bootstrap:
			if runCase(c) != c.Expected {
				meta.ParseTodo[name] = true
				changed = true
			}
		case isTodo && *checkParse:
			if runCase(c) == c.Expected {
				t.Logf("PARSE PASSES NOW: %s %s", path, name)
				delete(meta.ParseTodo, name)
				changed = true
			}
		case isTodo:
			// Not implemented yet; skip silently.
		default:
			c := c
			t.Run(name, func(t *testing.T) {
				got := runCase(c)
				if got != c.Expected {
					t.Errorf("parse mismatch\nSQL:\n%s\n\ngot:\n%s\n\nwant:\n%s", c.SQL, got, c.Expected)
				}
			})
		}
	}

	for _, c := range cases {
		name := fmt.Sprintf("case_%d", c.Index)

		if c.HasAlternation {
			// Cases the harness cannot confidently expand stay skipped and are
			// recorded in the alternations map (never run, to avoid false
			// passes).
			if c.AltUnexpandable {
				if (bootstrap || *checkParse) && !meta.Alternations[name] {
					meta.Alternations[name] = true
					changed = true
				}
				continue
			}

			// Bootstrap and -check-parse fully (re-)triage every expansion:
			// harvest ones that now pass out of parse_todo and record ones that
			// still fail as "case_N.alt_K" todos. Any legacy whole-case marker
			// (alternations entry or stale case_N todo from before the case was
			// expandable) is dropped. This is what replaces the old skipped
			// "alternations" tracking with real per-expansion tracking.
			if bootstrap || *checkParse {
				if meta.Alternations[name] {
					delete(meta.Alternations, name)
					changed = true
				}
				if meta.ParseTodo[name] {
					delete(meta.ParseTodo, name)
					changed = true
				}
				for _, child := range c.AltCases {
					cname := fmt.Sprintf("%s.alt_%d", name, child.Index)
					pass := runCase(child) == child.Expected
					switch {
					case pass && meta.ParseTodo[cname]:
						t.Logf("PARSE PASSES NOW: %s %s", path, cname)
						delete(meta.ParseTodo, cname)
						changed = true
					case !pass && !meta.ParseTodo[cname]:
						meta.ParseTodo[cname] = true
						changed = true
					}
				}
				continue
			}

			// Normal run: a case still marked in the alternations map has not
			// been migrated yet (run `go test ./parser -check-parse`); skip it
			// so the run does not regress. Otherwise run each expansion with the
			// usual per-expansion todo semantics.
			if meta.Alternations[name] {
				continue
			}
			for _, child := range c.AltCases {
				check(fmt.Sprintf("%s.alt_%d", name, child.Index), child)
			}
			continue
		}

		check(name, c)
	}

	if changed {
		if err := testfile.SaveMetadata(path, meta); err != nil {
			t.Fatalf("writing metadata: %v", err)
		}
	}
}
