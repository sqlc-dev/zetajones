# zetajones

A GoogleSQL (formerly ZetaSQL) parser written in pure Go — no cgo. Parses
GoogleSQL, the SQL dialect shared by BigQuery, Spanner, and F1, into an
Abstract Syntax Tree that matches the parse tree produced by the reference
implementation, [google/googlesql](https://github.com/google/googlesql).

> **Status:** the parser passes the entire upstream parser golden-test suite
> (all ~4,700 cases from google/googlesql release 2026.01.1), byte-for-byte
> including error messages and source positions.

## Installation

```bash
go get github.com/sqlc-dev/zetajones
```

## Usage

```go
package main

import (
	"encoding/json"
	"fmt"

	"github.com/sqlc-dev/zetajones/parser"
)

func main() {
	stmt, err := parser.ParseStatement(`SELECT id, name FROM users WHERE active = true LIMIT 10`)
	if err != nil {
		panic(err)
	}

	jsonBytes, _ := json.MarshalIndent(stmt, "", "  ")
	fmt.Println(string(jsonBytes))
}
```

Every AST node records the byte offsets of the source text it was parsed
from, matching the reference implementation's parse locations exactly.

## How it is tested

The test corpus in `parser/testdata/` is vendored directly from
[google/googlesql](https://github.com/google/googlesql) (release 2026.01.1) —
213 files containing thousands of test cases, each with the SQL input and the
expected parse tree debug string as produced by ZetaSQL itself. The
`internal/dump` package renders this parser's AST byte-for-byte in the same
format, so every case verifies node structure, node details, and source
locations against the reference implementation.

Golden files for new queries (e.g. from bug reports) can be generated with
the prebuilt `execute_query` reference binary:

```bash
go run ./cmd/regenerate-parse -sql "select 1 + 2"
```

## Acknowledgments

zetajones is developed with constant reference to
[google/googlesql](https://github.com/google/googlesql), the GoogleSQL
reference implementation. The test suite in `parser/testdata/` is vendored
from that project, and the AST structure, debug output format, and grammar
follow it directly. Huge thanks to the ZetaSQL/GoogleSQL team. GoogleSQL is
licensed under the Apache License 2.0.

## License

zetajones is under the Apache 2.0 license. See the LICENSE file for details.
