# Claude Development Guide

## Next Steps

To find the next parser test case to work on (files closest to fully passing
first), run from the repository root:

```bash
go run ./cmd/next-test
```

This prints the case's SQL, the expected parse tree, and overall progress.

## Running Tests

Always run parser tests with a 60 second timeout:

```bash
go test ./... -timeout 60s
```

The tests are fast. If a test times out, it almost certainly indicates an
infinite loop in the parser.

## Checking for Newly Passing Cases

**IMPORTANT:** After implementing parser/dump changes, ALWAYS run check-parse
to update metadata files:

```bash
go test ./parser -check-parse -v 2>&1 | grep "PARSE PASSES NOW"
```

This command:

1. Runs all cases listed in `parse_todo` in the metadata files
2. Automatically removes now-passing cases from `parse_todo`
3. Reports which cases now pass

**You must run this after every change to parser, lexer, ast, or dump code**,
then commit the updated `.metadata.json` files along with your code changes.

## Test Structure

`parser/testdata/*.test` files are vendored unmodified from
[google/googlesql](https://github.com/google/googlesql) release 2026.01.1
(`googlesql/parser/testdata/`). Each file contains cases separated by `==`
lines; each case has the SQL input, the expected parse tree (or `ERROR:`
message), and the expected unparse output, separated by `--` lines. See
`internal/testfile` for the format parser.

Each `<name>.metadata.json` sidecar (ours, not vendored) tracks:

- `parse_todo` — cases whose output doesn't match yet (`case_N` keys)
- `alternations` — cases using `{{a|b}}` alternation groups, which the
  harness cannot expand yet
- `skip` — reason string to skip the whole file

A `.test` file with no sidecar passes completely.

## Important Rules

- **NEVER modify `.test` files** — they are vendored golden files from the
  reference implementation. If output doesn't match, fix the Go code.
- **NEVER hand-edit expected output into metadata** — metadata only marks
  what is not implemented yet.
- The dump format must match ZetaSQL's `ASTNode::DebugString`
  (googlesql/parser/parse_tree.cc) byte-for-byte, including the summarized
  source text (see `GetSummaryString` in googlesql/common/utf_util.cc, ported
  in `internal/dump`).
- Parse error output must match ZetaSQL's error format:
  `Syntax error: <message> [at line:col]` plus the source line and a caret.
- When porting logic or tables (keywords, precedence) from googlesql source,
  keep the attribution note in the file header. GoogleSQL is Apache 2.0.

## Reference Implementation

The grammar and AST are defined in
[google/googlesql](https://github.com/google/googlesql):

- `googlesql/parser/parse_tree.h` — AST node definitions and details
- `googlesql/parser/parse_tree.cc` — `SingleNodeDebugString` per-node output
- `googlesql/parser/keywords.cc` — keyword and reserved word tables
- `googlesql/parser/*.tm` / grammar files — syntax reference

For new test queries, golden output comes from the prebuilt reference binary
(pinned in `cmd/regenerate-parse/main.go`, must match the vendored testdata
release):

```bash
go run ./cmd/regenerate-parse -sql "select 1 + 2"
```
