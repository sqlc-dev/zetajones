// Command next-test picks the best next parser test case to implement: the
// .test file closest to fully passing (fewest pending cases), breaking ties
// by file size. It prints the first pending case's SQL and expected output.
//
// Pending cases are the keys of each file's metadata parse_todo map. A key is
// either "case_N" (an ordinary case) or "case_N.alt_K" (the K-th alternation
// expansion of case N); the latter resolves to case N's K-th AltCase.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sqlc-dev/zetajones/internal/testfile"
)

// pendingItem is one resolved parse_todo entry ready to print.
type pendingItem struct {
	key      string
	sql      string
	expected string
	options  []string
	altLabel string // non-empty only for alternation expansions
}

type candidate struct {
	path     string
	pending  []pendingItem
	fileSize int64
}

// parseKey splits a parse_todo key into its case index and (for alternation
// expansions) its 1-based expansion index; altIdx is 0 for an ordinary case.
func parseKey(key string) (caseIdx, altIdx int, ok bool) {
	if i := strings.Index(key, ".alt_"); i >= 0 {
		if _, err := fmt.Sscanf(key[:i], "case_%d", &caseIdx); err != nil {
			return 0, 0, false
		}
		if _, err := fmt.Sscanf(key[i+len(".alt_"):], "%d", &altIdx); err != nil {
			return 0, 0, false
		}
		return caseIdx, altIdx, true
	}
	if _, err := fmt.Sscanf(key, "case_%d", &caseIdx); err != nil {
		return 0, 0, false
	}
	return caseIdx, 0, true
}

// resolve maps a parse_todo key to its printable case. Returns ok=false for a
// stale key that no longer matches a case/expansion in the file.
func resolve(cases []*testfile.Case, key string) (pendingItem, bool) {
	caseIdx, altIdx, ok := parseKey(key)
	if !ok || caseIdx < 1 || caseIdx > len(cases) {
		return pendingItem{}, false
	}
	c := cases[caseIdx-1]
	if altIdx == 0 {
		return pendingItem{key: key, sql: c.SQL, expected: c.Expected, options: c.Options}, true
	}
	if altIdx < 1 || altIdx > len(c.AltCases) {
		return pendingItem{}, false
	}
	a := c.AltCases[altIdx-1]
	return pendingItem{key: key, sql: a.SQL, expected: a.Expected, options: a.Options, altLabel: a.AltLabel}, true
}

func main() {
	files, err := filepath.Glob(filepath.Join("parser", "testdata", "*.test"))
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no .test files found (run from the repository root): %v\n", err)
		os.Exit(1)
	}

	var candidates []candidate
	totalPending, totalUnexpandable := 0, 0

	for _, path := range files {
		meta, _, err := testfile.LoadMetadata(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading metadata for %s: %v\n", path, err)
			os.Exit(1)
		}
		if meta.Skip != "" {
			continue
		}
		totalUnexpandable += len(meta.Alternations)
		if len(meta.ParseTodo) == 0 {
			continue
		}
		cases, err := testfile.ParseFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "parsing %s: %v\n", path, err)
			os.Exit(1)
		}
		// Resolve keys in a stable order so the "next" case is deterministic.
		keys := make([]string, 0, len(meta.ParseTodo))
		for k := range meta.ParseTodo {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			ci, ai, _ := parseKey(keys[i])
			cj, aj, _ := parseKey(keys[j])
			if ci != cj {
				return ci < cj
			}
			return ai < aj
		})
		var pending []pendingItem
		for _, k := range keys {
			if item, ok := resolve(cases, k); ok {
				pending = append(pending, item)
			}
		}
		if len(pending) == 0 {
			continue
		}
		totalPending += len(pending)
		info, err := os.Stat(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stat %s: %v\n", path, err)
			os.Exit(1)
		}
		candidates = append(candidates, candidate{path: path, pending: pending, fileSize: info.Size()})
	}

	if len(candidates) == 0 {
		fmt.Println("No parse_todo cases left!")
		return
	}

	sort.Slice(candidates, func(i, j int) bool {
		if len(candidates[i].pending) != len(candidates[j].pending) {
			return len(candidates[i].pending) < len(candidates[j].pending)
		}
		return candidates[i].fileSize < candidates[j].fileSize
	})

	next := candidates[0]
	item := next.pending[0]
	fmt.Printf("Next test: %s %s (%d pending in file)\n\n", next.path, item.key, len(next.pending))
	if item.altLabel != "" {
		fmt.Printf("Alternation expansion label: %q\n\n", item.altLabel)
	}
	if len(item.options) > 0 {
		fmt.Printf("Options: %v\n\n", item.options)
	}
	fmt.Printf("SQL:\n%s\n\n", item.sql)
	fmt.Printf("Expected:\n%s\n", item.expected)

	fmt.Printf("\nFiles with pending cases: %d\n", len(candidates))
	fmt.Printf("Total pending cases: %d\n", totalPending)
	fmt.Printf("Total unexpandable alternation cases (skipped): %d\n", totalUnexpandable)
}
