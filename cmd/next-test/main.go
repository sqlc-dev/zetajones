// Command next-test picks the best next parser test case to implement: the
// .test file closest to fully passing (fewest pending cases), breaking ties
// by file size. It prints the first pending case's SQL and expected output.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/sqlc-dev/zetajones/internal/testfile"
)

type candidate struct {
	path     string
	pending  []*testfile.Case
	fileSize int64
}

func main() {
	files, err := filepath.Glob(filepath.Join("parser", "testdata", "*.test"))
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no .test files found (run from the repository root): %v\n", err)
		os.Exit(1)
	}

	var candidates []candidate
	totalPending, totalAlternations := 0, 0

	for _, path := range files {
		meta, _, err := testfile.LoadMetadata(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading metadata for %s: %v\n", path, err)
			os.Exit(1)
		}
		if meta.Skip != "" {
			continue
		}
		totalAlternations += len(meta.Alternations)
		if len(meta.ParseTodo) == 0 {
			continue
		}
		cases, err := testfile.ParseFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "parsing %s: %v\n", path, err)
			os.Exit(1)
		}
		var pending []*testfile.Case
		for _, c := range cases {
			if meta.ParseTodo[fmt.Sprintf("case_%d", c.Index)] {
				pending = append(pending, c)
			}
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
	c := next.pending[0]
	fmt.Printf("Next test: %s case_%d (%d pending in file)\n\n", next.path, c.Index, len(next.pending))
	if len(c.Options) > 0 {
		fmt.Printf("Options: %v\n\n", c.Options)
	}
	fmt.Printf("SQL:\n%s\n\n", c.SQL)
	fmt.Printf("Expected:\n%s\n", c.Expected)

	fmt.Printf("\nFiles with pending cases: %d\n", len(candidates))
	fmt.Printf("Total pending cases: %d\n", totalPending)
	fmt.Printf("Total alternation cases (not yet supported by harness): %d\n", totalAlternations)
}
