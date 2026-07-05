package parser_test

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"regexp"
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

// newlineRange matches a DebugString location range like "[12-34]". The
// reference driver ignores these when comparing outputs across newline
// conventions, because \r\n shifts every byte offset relative to \n.
var newlineRange = regexp.MustCompile(`\[[0-9]+-[0-9]+\]`)

// caseOptions builds parser.Options and the parse mode from a case's options.
func caseOptions(c *testfile.Case) (parser.Options, string) {
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
	return opts, mode
}

// errorOutput renders a parse error as the reference driver does.
func errorOutput(err error) string {
	var perr *parser.Error
	if errors.As(err, &perr) {
		return "ERROR: " + perr.Caret()
	}
	return "ERROR: " + err.Error()
}

// runCase parses the case's SQL and returns the rendered output in the same
// shape as the expected section: either the parse tree debug string or an
// "ERROR: ..." message. Cases whose expected output carries a [NEWLINE X]
// annotation are re-run once per newline convention (see runNewlineCase).
func runCase(c *testfile.Case) string {
	if strings.HasPrefix(c.Expected, "[NEWLINE ") {
		return runNewlineCase(c)
	}
	return parseAndRender(c, c.SQL)
}

// parseAndRender parses sql under the case's options and returns the rendered
// output. Under parse_multiple it renders each ";"-separated statement in
// order, "--"-joined, matching TestMulti in run_parser_test.cc.
func parseAndRender(c *testfile.Case, sql string) string {
	opts, mode := caseOptions(c)
	dumpOpts := dump.Options{SQL: sql, ShowLocationText: !c.HasOption("no_show_parse_location_text")}

	if mode == "statement" && c.BoolOption("parse_multiple") {
		stmts, err := parser.ParseMultipleWithOptions(sql, opts)
		var outs []string
		for _, stmt := range stmts {
			outs = append(outs, dump.Tree(stmt, dumpOpts))
		}
		if err != nil {
			outs = append(outs, errorOutput(err))
		}
		return strings.Join(outs, "\n--\n")
	}

	var (
		node ast.Node
		err  error
	)
	switch mode {
	case "type":
		node, err = parser.ParseTypeWithOptions(sql, opts)
	case "script":
		node, err = parser.ParseScriptWithOptions(sql, opts)
	case "expression":
		node, err = parser.ParseExpressionWithOptions(sql, opts)
	default:
		var stmt ast.Statement
		stmt, err = parser.ParseStatementWithOptions(sql, opts)
		node = stmt
	}
	if err != nil {
		return errorOutput(err)
	}
	return dump.Tree(node, dumpOpts)
}

// runNewlineCase re-runs the parse under each newline convention (\n, \r,
// \r\n), mirroring file_based_test_driver's RunTestForNewlineTypes. Every
// newline in the input is replaced with the convention, the resulting output's
// convention characters are normalized back to \n, and the three outputs are
// compared ignoring byte-offset ranges. If they agree the first is returned
// unannotated; otherwise each is prefixed with its [NEWLINE X] line and the
// three are "--"-joined.
func runNewlineCase(c *testfile.Case) string {
	newlines := []string{"\n", "\r", "\r\n"}
	annotations := []string{`NEWLINE \n`, `NEWLINE \r`, `NEWLINE \r\n`}
	outputs := make([]string, len(newlines))
	for i, nl := range newlines {
		out := parseAndRender(c, strings.ReplaceAll(c.SQL, "\n", nl))
		outputs[i] = strings.ReplaceAll(out, nl, "\n")
	}
	same := true
	redacted0 := newlineRange.ReplaceAllString(outputs[0], "")
	for _, out := range outputs[1:] {
		if newlineRange.ReplaceAllString(out, "") != redacted0 {
			same = false
			break
		}
	}
	if same {
		return outputs[0]
	}
	parts := make([]string, len(outputs))
	for i, out := range outputs {
		parts[i] = "[" + annotations[i] + "]\n" + out
	}
	return strings.Join(parts, "\n--\n")
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
