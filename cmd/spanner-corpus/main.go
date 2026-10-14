// Command spanner-corpus builds golden parser tests from the SQL examples in
// Cloud Spanner's documentation: the GoogleSQL reference
// (https://docs.cloud.google.com/spanner/docs/reference/standard-sql/) and
// the guide pages with GoogleSQL examples (see pages.go).
//
// For each page it downloads the HTML (cached under .spanner-docs/), extracts
// the copyable GoogleSQL example blocks (grammar blocks, templates and
// PostgreSQL tabs are skipped, as are query-result comments), splits them
// into statements, and runs each through the reference parser (see
// internal/refparse) with Spanner's language features enabled.
//
//   - Statements the reference accepts become cases in
//     parser/testdata/spanner/<page>.test, in the same format as the vendored
//     googlesql testdata, so the parser tests run them (with parse_todo
//     tracking in a .metadata.json sidecar like any other file).
//   - Statements the reference rejects are triaged. Grammar fragments are
//     recognized by heuristics (see fragmentReason) and dropped; the rest are
//     looked up by id in triage.txt:
//     "spanner" (valid Spanner syntax GoogleSQL's parser lacks) goes to
//     <page>.spanner.sql, the backlog of Spanner-only syntax; "typo" and
//     "fragment" are dropped. Untriaged rejects are written to
//     <page>.rejected.sql, each with its id, for someone to classify in
//     triage.txt.
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
//	go run ./cmd/spanner-corpus -pages reference/standard-sql/dml-syntax
//	go run ./cmd/spanner-corpus -refresh         # re-download pages
//	go run ./cmd/spanner-corpus -discover        # list guide pages not in pages.go
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/sqlc-dev/zetajones/internal/refparse"
	"github.com/sqlc-dev/zetajones/internal/testfile"
)

const docsURL = "https://docs.cloud.google.com/spanner/docs/"

// features are the language features Spanner's dialect needs from the
// reference parser, including the SQL graph features.
const features = "MAXIMUM,+SPANNER_LEGACY_DDL,+TTL,+CREATE_LOCALITY_GROUP,+SQL_GRAPH,+SQL_GRAPH_ADVANCED_QUERY,+SQL_GRAPH_BOUNDED_PATH_QUANTIFICATION,+SQL_GRAPH_PATH_TYPE,+SQL_GRAPH_PATH_MODE"

// page is a Spanner docs page, by its path under docsURL.
type page struct {
	path string
}

func (p page) url() string { return docsURL + p.path }

// out is the page's output path (without extension) relative to the output
// directory: reference pages by name, guides under guides/.
func (p page) out() string {
	if name, ok := strings.CutPrefix(p.path, "reference/standard-sql/"); ok {
		return name
	}
	return filepath.Join("guides", strings.ReplaceAll(p.path, "/", "."))
}

func allPages() []page { return append(slices.Clone(referencePages), guidePages...) }

func main() {
	only := flag.String("pages", "", "comma-separated page paths to regenerate (default: all)")
	refresh := flag.Bool("refresh", false, "re-download cached pages")
	discover := flag.Bool("discover", false, "list guide pages with GoogleSQL examples that pages.go lacks")
	cacheDir := flag.String("cache", ".spanner-docs", "directory for downloaded HTML")
	outDir := flag.String("out", filepath.Join("parser", "testdata", "spanner"), "output directory")
	verbose := flag.Bool("v", false, "print the rejected statements dropped as fragments")
	triagePath := flag.String("triage", filepath.Join("cmd", "spanner-corpus", "triage.txt"), "triage file")
	flag.Parse()

	if *discover {
		runDiscover(*cacheDir, *refresh)
		return
	}

	pages := allPages()
	selected := pages
	if *only != "" {
		names := strings.Split(*only, ",")
		selected = nil
		for _, p := range pages {
			if slices.Contains(names, p.path) {
				selected = append(selected, p)
			}
		}
		if len(selected) != len(names) {
			fatalf("unknown page in -pages %q", *only)
		}
	}

	tri, err := loadTriage(*triagePath)
	if err != nil {
		fatalf("%v", err)
	}
	bin, err := refparse.EnsureBinary()
	if err != nil {
		fatalf("setting up execute_query: %v", err)
	}

	// A full run regenerates everything, so files for pages no longer listed
	// do not linger.
	if *only == "" {
		if err := os.RemoveAll(*outDir); err != nil {
			fatalf("%v", err)
		}
	}

	// Statements are deduplicated across pages in page order, so a selected
	// subset must still see the earlier pages' statements.
	seen := map[string]bool{}
	used := map[string]bool{}
	var total counts
	for _, p := range pages {
		doc, err := fetch(p, *cacheDir, *refresh && slices.Contains(selected, p))
		if err != nil {
			fatalf("%s: %v", p.path, err)
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
		accepted, rejected := runReference(bin, blocks, seen)

		var c counts
		c.accepted = len(accepted)
		var spanner, untriaged []rejection
		for _, r := range rejected {
			r.id = stmtID(r.sql)
			if v, ok := tri[r.id]; ok {
				used[r.id] = true
				switch v.kind {
				case verdictSpanner:
					r.note = v.note
					spanner = append(spanner, r)
				case verdictTypo:
					c.typos++
				default:
					c.fragments++
				}
				continue
			}
			if reason := fragmentReason(r.sql); reason != "" {
				if *verbose {
					fmt.Printf("fragment (%s) %s#%s\n%s\n\n", reason, p.url(), r.section, r.sql)
				}
				c.fragments++
				continue
			}
			untriaged = append(untriaged, r)
		}
		c.spanner, c.untriaged = len(spanner), len(untriaged)
		total.add(c)

		base := filepath.Join(*outDir, p.out())
		if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
			fatalf("%v", err)
		}
		if err := writeTests(base+".test", p, accepted); err != nil {
			fatalf("%v", err)
		}
		if err := writeRejects(base+".spanner.sql", p, spanner, spannerHeader); err != nil {
			fatalf("%v", err)
		}
		if err := writeRejects(base+".rejected.sql", p, untriaged, rejectedHeader); err != nil {
			fatalf("%v", err)
		}
		fmt.Printf("%-50s %s\n", p.path, c)
	}
	fmt.Printf("%-50s %s\n", "TOTAL", total)

	if *only == "" {
		var stale []string
		for id := range tri {
			if !used[id] {
				stale = append(stale, id)
			}
		}
		sort.Strings(stale)
		for _, id := range stale {
			fmt.Fprintf(os.Stderr, "warning: triage.txt entry %s matches no rejected statement\n", id)
		}
	}
	if total.untriaged > 0 {
		fmt.Fprintf(os.Stderr, "%d rejected statements need triage; see %s/**/*.rejected.sql\n", total.untriaged, *outDir)
	}
}

type counts struct {
	accepted, spanner, typos, fragments, untriaged int
}

func (c *counts) add(o counts) {
	c.accepted += o.accepted
	c.spanner += o.spanner
	c.typos += o.typos
	c.fragments += o.fragments
	c.untriaged += o.untriaged
}

func (c counts) String() string {
	return fmt.Sprintf("%4d accepted %3d spanner-only %3d typos %3d fragments %3d untriaged",
		c.accepted, c.spanner, c.typos, c.fragments, c.untriaged)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "spanner-corpus: "+format+"\n", args...)
	os.Exit(1)
}

// runDiscover crawls the Spanner docs navigation for guide pages not in
// pages.go and prints those with GoogleSQL example statements.
func runDiscover(cacheDir string, refresh bool) {
	nav, err := fetch(page{"secondary-indexes"}, cacheDir, refresh)
	if err != nil {
		fatalf("%v", err)
	}
	known := map[string]bool{}
	for _, p := range allPages() {
		known[p.path] = true
	}
	var candidates []page
	for _, path := range navLinks(nav) {
		if known[path] || strings.HasPrefix(path, "reference/") || strings.Contains(path, "postgres") {
			continue
		}
		candidates = append(candidates, page{path})
	}
	found := make([]int, len(candidates))
	parallel(len(candidates), func(i int) {
		doc, err := fetch(candidates[i], cacheDir, refresh)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", candidates[i].path, err)
			return
		}
		for _, b := range extractBlocks(doc) {
			found[i] += len(splitStatements(b.sql))
		}
	})
	for i, p := range candidates {
		if found[i] > 0 {
			fmt.Printf("%s\t%d\n", p.path, found[i])
		}
	}
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
	id      string
	note    string
}

// result is one reference parse.
type result struct {
	tree string
	err  error
}

type unit struct {
	block   int
	section string
	sql     string
}

// runReference runs every new statement through the reference parser. A
// block whose statements do not all parse individually is retried as one
// script, which covers procedural blocks (BEGIN ... END, procedure bodies)
// whose inner ";"s the naive splitter cuts. A statement that still fails is
// retried as an expression, which covers the function pages' bare call and
// literal examples.
func runReference(bin string, blocks []block, seen map[string]bool) ([]testCase, []rejection) {
	parse := func(sql, mode string) result {
		tree, err := refparse.Parse(bin, sql+"\n", refparse.Options{LanguageFeatures: features, Mode: mode})
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
			} else if parts := splitUnterminated(u.sql); len(parts) > 1 && allParse(parts, parse) {
				// The docs left out the ";"s between statements.
				for _, part := range parts {
					if !seen[part] {
						seen[part] = true
						accepted = append(accepted, testCase{section: u.section, sql: part, tree: parse(part, "").tree})
					}
				}
			} else {
				rejected = append(rejected, rejection{section: u.section, sql: u.sql, err: firstLine(stmt[k].err.Error())})
			}
		}
		i = j
	}
	return accepted, rejected
}

// allParse reports whether every part parses as a statement.
func allParse(parts []string, parse func(sql, mode string) result) bool {
	for _, part := range parts {
		if parse(part, "").err != nil {
			return false
		}
	}
	return true
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
		"# Source: " + p.url() + "\n" +
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
		fmt.Fprintf(&b, "# %s#%s\n", p.url(), c.section)
		if i == 0 {
			fmt.Fprintf(&b, "[default language_features=%s]\n", features)
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

const spannerHeader = `#
# Spanner syntax the reference parser rejects with
# --enabled_language_features=` + features + `
# (classified "spanner" in cmd/spanner-corpus/triage.txt). There is no
# golden parse tree for these.
`

const rejectedHeader = `#
# Example statements the reference parser rejects that are not yet triaged.
# Classify each by id in cmd/spanner-corpus/triage.txt as "spanner"
# (Spanner-only syntax), "typo" (broken in the docs) or "fragment" (not a
# complete statement), then regenerate.
`

func writeRejects(path string, p page, rejects []rejection, preamble string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(rejects) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString(header(p))
	b.WriteString(preamble)
	for _, r := range rejects {
		fmt.Fprintf(&b, "\n# %s#%s\n", p.url(), r.section)
		if r.note != "" {
			fmt.Fprintf(&b, "# id %s: %s\n", r.id, r.note)
		} else {
			fmt.Fprintf(&b, "# id %s\n", r.id)
		}
		fmt.Fprintf(&b, "# %s\n", r.err)
		b.WriteString(r.sql)
		b.WriteString(";\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
