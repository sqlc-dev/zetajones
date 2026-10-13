// Command regenerate-parse runs GoogleSQL's reference implementation to
// produce golden parse trees for new test queries, using the prebuilt
// execute_query binary published with googlesql releases
// (https://github.com/google/googlesql/releases).
//
// The vendored files in parser/testdata come directly from the googlesql
// repository and should never be regenerated or modified. This tool exists to
// mint expected output for NEW queries (e.g. from bug reports) so they can be
// added as additional test cases.
//
// Usage:
//
//	go run ./cmd/regenerate-parse -sql "select 1 + 2"
//	go run ./cmd/regenerate-parse -file query.sql
//	go run ./cmd/regenerate-parse -features MAXIMUM,+SPANNER_LEGACY_DDL -sql "..."
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/sqlc-dev/zetajones/internal/refparse"
)

func main() {
	sql := flag.String("sql", "", "SQL statement to parse")
	file := flag.String("file", "", "File containing the SQL statement to parse")
	features := flag.String("features", "", "--enabled_language_features spec (e.g. MAXIMUM,+SPANNER_LEGACY_DDL)")
	mode := flag.String("mode", "", "--sql_mode: query (default), script, or expression")
	flag.Parse()

	if (*sql == "") == (*file == "") {
		fmt.Fprintln(os.Stderr, "exactly one of -sql or -file is required")
		os.Exit(1)
	}
	if *file != "" {
		data, err := os.ReadFile(*file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading %s: %v\n", *file, err)
			os.Exit(1)
		}
		*sql = string(data)
	}

	bin, err := refparse.EnsureBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "setting up execute_query: %v\n", err)
		os.Exit(1)
	}

	out, err := refparse.Parse(bin, *sql, refparse.Options{LanguageFeatures: *features, Mode: *mode})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	fmt.Println(out)
}
