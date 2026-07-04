package parser_test

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

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
	// [default ...] options. Without one, all features stay enabled.
	for _, opt := range c.Options {
		if spec, ok := strings.CutPrefix(opt, "language_features="); ok {
			opts.Features = parser.ParseFeatureSet(spec)
		}
	}
	stmt, err := parser.ParseStatementWithOptions(c.SQL, opts)
	if err != nil {
		var perr *parser.Error
		if errors.As(err, &perr) {
			return "ERROR: " + perr.Caret()
		}
		return "ERROR: " + err.Error()
	}
	return dump.Tree(stmt, dump.Options{
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
	if bootstrap {
		meta.ParseTodo = map[string]bool{}
		meta.Alternations = map[string]bool{}
	}

	changed := false
	for _, c := range cases {
		name := fmt.Sprintf("case_%d", c.Index)

		if c.HasAlternation {
			if bootstrap {
				meta.Alternations[name] = true
				changed = true
			}
			continue
		}

		isTodo := meta.ParseTodo[name]
		switch {
		case bootstrap:
			got := runCase(c)
			if got != c.Expected {
				meta.ParseTodo[name] = true
				changed = true
			}
		case isTodo && *checkParse:
			got := runCase(c)
			if got == c.Expected {
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

	if changed {
		if err := testfile.SaveMetadata(path, meta); err != nil {
			t.Fatalf("writing metadata: %v", err)
		}
	}
}
