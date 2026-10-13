// Command spanner-corpus builds golden parser tests from the SQL examples in
// Cloud Spanner's GoogleSQL reference
// (https://docs.cloud.google.com/spanner/docs/reference/standard-sql/).
//
// For each reference page it downloads the HTML (cached under .spanner-docs/),
// extracts the copyable SQL example blocks (grammar blocks are skipped), splits
// them into statements, and runs each statement through the reference parser
// (see internal/refparse) with Spanner's language features enabled.
//
//   - Statements the reference accepts become cases in
//     parser/testdata/spanner/<page>.test, in the same format as the vendored
//     googlesql testdata, so the parser tests run them (with parse_todo
//     tracking in a .metadata.json sidecar like any other file).
//   - Statements the reference rejects are Spanner syntax that GoogleSQL's
//     parser does not accept (or doc snippets that are not complete SQL).
//     They have no golden output, so they are listed in
//     parser/testdata/spanner/<page>.rejected.sql for triage instead.
//
// The generated files are regenerated wholesale; do not edit them by hand.
// After regenerating, run
//
//	go test ./parser -check-parse
//
// to record cases the parser does not handle yet in the sidecars.
//
// The examples are taken from Google Cloud documentation; code samples there
// are licensed under the Apache 2.0 License.
//
// Usage:
//
//	go run ./cmd/spanner-corpus                  # all pages
//	go run ./cmd/spanner-corpus -pages dml-syntax
//	go run ./cmd/spanner-corpus -refresh         # re-download pages
package main

import (
	"flag"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/sqlc-dev/zetajones/internal/refparse"
	"github.com/sqlc-dev/zetajones/internal/testfile"
)

const baseURL = "https://docs.cloud.google.com/spanner/docs/reference/standard-sql/"

// Language features Spanner's dialect needs from the reference parser. The
// graph pages additionally need the SQL graph features.
const (
	spannerFeatures = "MAXIMUM,+SPANNER_LEGACY_DDL"
	graphFeatures   = spannerFeatures + ",+SQL_GRAPH,+SQL_GRAPH_ADVANCED_QUERY,+SQL_GRAPH_BOUNDED_PATH_QUANTIFICATION,+SQL_GRAPH_PATH_TYPE,+SQL_GRAPH_PATH_MODE"
)

type page struct {
	name     string
	features string
}

// pages are the reference pages scraped, in output order. Statements that
// appear on more than one page are kept only on the first.
var pages = []page{
	{"data-definition-language", spannerFeatures},
	{"dml-syntax", spannerFeatures},
	{"query-syntax", spannerFeatures},
	{"pipe-syntax", spannerFeatures},
	{"subqueries", spannerFeatures},
	{"procedural-language", spannerFeatures},
	{"stored-procedures", spannerFeatures},
	{"graph-schema-statements", graphFeatures},
	{"graph-query-statements", graphFeatures},
	{"graph-sql-queries", graphFeatures},
	{"graph-patterns", graphFeatures},
	{"graph-subqueries", graphFeatures},
	// Function and expression reference pages.
	{"aggregate_functions", spannerFeatures},
	{"array_functions", spannerFeatures},
	{"bit_functions", spannerFeatures},
	{"compression-functions", spannerFeatures},
	{"conditional_expressions", spannerFeatures},
	{"conversion_functions", spannerFeatures},
	{"date_functions", spannerFeatures},
	{"debugging_functions", spannerFeatures},
	{"graph-conditional-expressions", graphFeatures},
	{"graph-gql-functions", graphFeatures},
	{"graph-operators", graphFeatures},
	{"graph-sql-functions", graphFeatures},
	{"hash_functions", spannerFeatures},
	{"interval_functions", spannerFeatures},
	{"json_functions", spannerFeatures},
	{"mathematical_functions", spannerFeatures},
	{"ml-functions", spannerFeatures},
	{"net_functions", spannerFeatures},
	{"operators", spannerFeatures},
	{"protocol_buffer_functions", spannerFeatures},
	{"search_functions", spannerFeatures},
	{"security_functions", spannerFeatures},
	{"sequence_functions", spannerFeatures},
	{"statistical_aggregate_functions", spannerFeatures},
	{"string_functions", spannerFeatures},
	{"timestamp_functions", spannerFeatures},
	{"utility-functions", spannerFeatures},
	{"aggregate-function-calls", spannerFeatures},
	{"data-types", spannerFeatures},
	{"lexical", spannerFeatures},
	{"protocol-buffers", spannerFeatures},
	{"collation-concepts", spannerFeatures},
	{"format-elements", spannerFeatures},
	{"conversion_rules", spannerFeatures},
}

func main() {
	only := flag.String("pages", "", "comma-separated page names to regenerate (default: all)")
	refresh := flag.Bool("refresh", false, "re-download cached pages")
	cacheDir := flag.String("cache", ".spanner-docs", "directory for downloaded HTML")
	outDir := flag.String("out", filepath.Join("parser", "testdata", "spanner"), "output directory")
	flag.Parse()

	selected := pages
	if *only != "" {
		names := strings.Split(*only, ",")
		selected = nil
		for _, p := range pages {
			if slices.Contains(names, p.name) {
				selected = append(selected, p)
			}
		}
		if len(selected) != len(names) {
			fatalf("unknown page in -pages %q", *only)
		}
	}

	bin, err := refparse.EnsureBinary()
	if err != nil {
		fatalf("setting up execute_query: %v", err)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatalf("%v", err)
	}

	// Statements are deduplicated across pages in page order, so a selected
	// subset must still see the earlier pages' statements.
	seen := map[string]bool{}
	for _, p := range pages {
		doc, err := fetch(p.name, *cacheDir, *refresh && slices.Contains(selected, p))
		if err != nil {
			fatalf("%s: %v", p.name, err)
		}
		blocks := extractBlocks(doc)
		if !slices.Contains(selected, p) {
			for _, b := range blocks {
				seen[strings.TrimSpace(b.sql)] = true
				for _, s := range splitStatements(b.sql) {
					seen[s] = true
				}
			}
			continue
		}
		accepted, rejected := triage(bin, p, blocks, seen)
		if err := writeTests(filepath.Join(*outDir, p.name+".test"), p, accepted); err != nil {
			fatalf("%v", err)
		}
		if err := writeRejected(filepath.Join(*outDir, p.name+".rejected.sql"), p, rejected); err != nil {
			fatalf("%v", err)
		}
		fmt.Printf("%-26s %4d blocks  %4d accepted  %4d rejected\n", p.name, len(blocks), len(accepted), len(rejected))
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "spanner-corpus: "+format+"\n", args...)
	os.Exit(1)
}

// fetch returns the page's HTML, downloading it into the cache if needed.
func fetch(name, cacheDir string, refresh bool) (string, error) {
	path := filepath.Join(cacheDir, name+".html")
	if !refresh {
		if data, err := os.ReadFile(path); err == nil {
			return string(data), nil
		}
	}
	url := baseURL + name
	fmt.Fprintf(os.Stderr, "Downloading %s...\n", url)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return string(data), nil
}

// block is one SQL example block on a page.
type block struct {
	section string // id of the nearest preceding heading
	sql     string
}

var (
	// blockRE matches a heading with an id (to track the current section) or
	// a <pre> block.
	blockRE    = regexp.MustCompile(`(?s)<h[1-6][^>]*\bid="([^"]*)"|<pre([^>]*)>(.*?)</pre>`)
	syntaxAttr = regexp.MustCompile(`\bsyntax="([^"]*)"`)
	tagRE      = regexp.MustCompile(`<[^>]*>`)
)

// extractBlocks returns the page's SQL example blocks. Copyable blocks
// (class devsite-click-to-copy) marked as SQL are examples; the reference's
// grammar blocks are plain <pre>s and are skipped.
func extractBlocks(doc string) []block {
	var blocks []block
	section := ""
	for _, m := range blockRE.FindAllStringSubmatch(doc, -1) {
		if m[1] != "" {
			section = m[1]
			continue
		}
		attrs := m[2]
		if !strings.Contains(attrs, "devsite-click-to-copy") || strings.Contains(attrs, "devsite-disable-click-to-copy") {
			continue
		}
		// Grammar placeholders are marked up as <var>; such blocks are
		// templates, not runnable SQL.
		if strings.Contains(m[3], "<var") {
			continue
		}
		if s := syntaxAttr.FindStringSubmatch(attrs); s == nil || (s[1] != "SQL" && s[1] != "GoogleSQL") {
			continue
		}
		text := html.UnescapeString(tagRE.ReplaceAllString(m[3], ""))
		text = strings.ReplaceAll(text, "\r\n", "\n")
		text = strings.ReplaceAll(text, " ", " ")
		blocks = append(blocks, block{section: section, sql: text})
	}
	return blocks
}

// splitStatements splits sql on top-level ";" (outside string literals,
// quoted identifiers and comments) and returns the trimmed statements that
// contain more than comments.
func splitStatements(sql string) []string {
	var out []string
	start, hasCode := 0, false
	flush := func(end int) {
		if s := strings.TrimSpace(sql[start:end]); hasCode && s != "" {
			out = append(out, s)
		}
		hasCode = false
	}
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == ';':
			flush(i)
			i++
			start = i
		case c == '-' && strings.HasPrefix(sql[i:], "--"), c == '#':
			i = skipLine(sql, i)
		case c == '/' && strings.HasPrefix(sql[i:], "/*"):
			if end := strings.Index(sql[i+2:], "*/"); end >= 0 {
				i += 2 + end + 2
			} else {
				i = len(sql)
			}
		case c == '\'' || c == '"' || c == '`':
			hasCode = true
			i = skipQuoted(sql, i)
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		default:
			hasCode = true
			i++
		}
	}
	flush(len(sql))
	return out
}

func skipLine(s string, i int) int {
	if end := strings.IndexByte(s[i:], '\n'); end >= 0 {
		return i + end + 1
	}
	return len(s)
}

// skipQuoted returns the index just past the quoted token starting at i,
// handling triple-quoted strings and backslash escapes.
func skipQuoted(s string, i int) int {
	q := s[i : i+1]
	if q != "`" && strings.HasPrefix(s[i:], q+q+q) {
		q = q + q + q
	}
	for j := i + len(q); j < len(s); j++ {
		if s[j] == '\\' {
			j++
			continue
		}
		if strings.HasPrefix(s[j:], q) {
			return j + len(q)
		}
	}
	return len(s)
}

// testCase is an example the reference accepted.
type testCase struct {
	section string
	sql     string
	mode    string // "" (statement), "script" or "expression"
	tree    string
}

// rejection is a statement the reference rejected.
type rejection struct {
	section string
	sql     string
	err     string
}

type unit struct {
	block   int
	section string
	sql     string
}

// triage runs every new statement through the reference parser. A block
// whose statements do not all parse individually is retried as one script,
// which covers procedural blocks (BEGIN ... END, procedure bodies) whose inner
// ";"s the naive splitter cuts. A statement that still fails is retried as an
// expression, which covers the function pages' bare call and literal
// examples.
func triage(bin string, p page, blocks []block, seen map[string]bool) ([]testCase, []rejection) {
	type result struct {
		tree string
		err  error
	}
	parse := func(sql, mode string) result {
		tree, err := refparse.Parse(bin, sql+"\n", refparse.Options{LanguageFeatures: p.features, Mode: mode})
		return result{tree, err}
	}

	var units []unit
	for i, b := range blocks {
		for _, s := range splitStatements(b.sql) {
			units = append(units, unit{block: i, section: b.section, sql: s})
		}
	}
	stmt := make([]result, len(units))
	parallel(len(units), func(i int) { stmt[i] = parse(units[i].sql, "") })

	var accepted []testCase
	var rejected []rejection
	for i := 0; i < len(units); {
		j := i
		ok := true
		for j < len(units) && units[j].block == units[i].block {
			ok = ok && stmt[j].err == nil
			j++
		}
		if !ok && j-i > 1 {
			script := strings.TrimSpace(blocks[units[i].block].sql)
			if r := parse(script, "script"); r.err == nil {
				if !seen[script] {
					seen[script] = true
					accepted = append(accepted, testCase{section: units[i].section, sql: script, mode: "script", tree: r.tree})
				}
				i = j
				continue
			}
		}
		for k := i; k < j; k++ {
			u := units[k]
			if seen[u.sql] {
				continue
			}
			seen[u.sql] = true
			if stmt[k].err == nil {
				accepted = append(accepted, testCase{section: u.section, sql: u.sql, tree: stmt[k].tree})
			} else if r := parse(u.sql, "expression"); r.err == nil {
				accepted = append(accepted, testCase{section: u.section, sql: u.sql, mode: "expression", tree: r.tree})
			} else {
				rejected = append(rejected, rejection{section: u.section, sql: u.sql, err: firstLine(stmt[k].err.Error())})
			}
		}
		i = j
	}
	return accepted, rejected
}

func parallel(n int, f func(int)) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			f(i)
		}()
	}
	wg.Wait()
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

func header(p page) string {
	return "# Code generated by go run ./cmd/spanner-corpus. DO NOT EDIT.\n" +
		"# Source: " + baseURL + p.name + "\n" +
		"# Examples from Google Cloud documentation, licensed under Apache 2.0.\n"
}

// writeTests writes accepted cases in the vendored testdata format. Source
// comments precede the option lines: comments after them would be part of
// the input.
//
// Case numbering changes on regeneration, so the metadata sidecar is removed;
// `go test ./parser -check-parse` re-creates it.
func writeTests(path string, p page, cases []testCase) error {
	for _, f := range []string{path, testfile.MetadataPath(path)} {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if len(cases) == 0 {
		return nil
	}
	var b strings.Builder
	for i, c := range cases {
		if i == 0 {
			b.WriteString(header(p))
		} else {
			b.WriteString("==\n")
		}
		fmt.Fprintf(&b, "# %s%s#%s\n", baseURL, p.name, c.section)
		if i == 0 {
			fmt.Fprintf(&b, "[default language_features=%s]\n", p.features)
			b.WriteString("[default no_show_parse_location_text]\n")
			// execute_query always reserves the GRAPH_TABLE keyword.
			b.WriteString("[default reserve_graph_table]\n")
		}
		if c.mode != "" {
			fmt.Fprintf(&b, "[mode=%s]\n", c.mode)
		}
		writePayload(&b, c.sql)
		b.WriteString("--\n")
		writePayload(&b, c.tree)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// writePayload writes text, escaping lines the test format would otherwise
// treat as comments, options or separators.
func writePayload(b *strings.Builder, text string) {
	for line := range strings.SplitSeq(text, "\n") {
		t := strings.TrimSpace(line)
		if line == "==" || line == "--" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "[") || strings.HasPrefix(line, `\`) {
			b.WriteByte('\\')
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

func writeRejected(path string, p page, rejected []rejection) error {
	if len(rejected) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	var b strings.Builder
	b.WriteString(header(p))
	b.WriteString("#\n# Example statements the reference parser rejects with\n")
	fmt.Fprintf(&b, "# --enabled_language_features=%s.\n", p.features)
	b.WriteString("# These have no golden parse tree; each needs triage (Spanner-only\n")
	b.WriteString("# syntax, a missing language feature, or a non-SQL doc snippet).\n")
	for _, r := range rejected {
		fmt.Fprintf(&b, "\n# %s%s#%s\n", baseURL, p.name, r.section)
		fmt.Fprintf(&b, "# %s\n", r.err)
		b.WriteString(r.sql)
		b.WriteString(";\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
