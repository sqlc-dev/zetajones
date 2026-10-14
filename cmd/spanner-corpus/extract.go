package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// fetch returns the page's HTML, downloading it into the gzipped cache if
// needed.
func fetch(p page, cacheDir string, refresh bool) (string, error) {
	path := filepath.Join(cacheDir, strings.ReplaceAll(p.path, "/", ".")+".html.gz")
	if !refresh {
		if data, err := os.ReadFile(path); err == nil {
			zr, err := gzip.NewReader(bytes.NewReader(data))
			if err == nil {
				if doc, err := io.ReadAll(zr); err == nil {
					return string(doc), nil
				}
			}
		}
	}
	fmt.Fprintf(os.Stderr, "Downloading %s...\n", p.url())
	resp, err := http.Get(p.url())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", p.url(), resp.StatusCode)
	}
	doc, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(doc)
	zw.Close()
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return "", err
	}
	return string(doc), nil
}

// navRE matches links to other Spanner docs pages.
var navRE = regexp.MustCompile(`href="/spanner/docs/([^"#?]+)"`)

// navLinks returns the Spanner docs page paths linked from doc.
func navLinks(doc string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range navRE.FindAllStringSubmatch(doc, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// block is one SQL example block on a page.
type block struct {
	section string // id of the nearest preceding heading
	sql     string
}

var (
	// tokenRE matches, in document order, the start and end of a tab set
	// (devsite-selector), a heading, or a <pre> block.
	tokenRE = regexp.MustCompile(`(?s)(<devsite-selector\b)|(</devsite-selector>)|<h[1-6]([^>]*)>(.*?)</h[1-6]>|<pre([^>]*)>(.*?)</pre>`)
	idAttr  = regexp.MustCompile(`\bid="([^"]*)"`)
	syntax  = regexp.MustCompile(`\bsyntax="([^"]*)"`)
	tagRE   = regexp.MustCompile(`<[^>]*>`)

	// resultTableRE matches the query-result tables the docs print as block
	// comments after examples ("/*---+---+ ... +---*/"). Some are closed with
	// a typo'd "/*" instead of "*/", which would swallow the rest of the
	// block, so both endings are accepted.
	resultTableRE = regexp.MustCompile(`(?m)^[ \t]*/\*[-+]+[^\n]*\n(?:[^\n]*\n)*?[^\n]*?(?:\*/|[-+]/\*)[ \t]*$`)
)

// extractBlocks returns the page's GoogleSQL example blocks. Copyable blocks
// (class devsite-click-to-copy) marked as SQL are examples; the reference's
// grammar blocks are plain <pre>s, and blocks with <var> placeholders are
// templates, so both are skipped. Guide pages show each example in GoogleSQL
// and PostgreSQL tabs; PostgreSQL ones are skipped.
func extractBlocks(doc string) []block {
	var blocks []block
	section, sectionTitle := "", ""
	var tabs []string // current tab title per open tab set
	for _, m := range tokenRE.FindAllStringSubmatch(doc, -1) {
		switch {
		case m[1] != "":
			tabs = append(tabs, "")
		case m[2] != "":
			if len(tabs) > 0 {
				tabs = tabs[:len(tabs)-1]
			}
		case m[6] != "" || m[5] != "":
			attrs, body := m[5], m[6]
			if !strings.Contains(attrs, "devsite-click-to-copy") || strings.Contains(attrs, "devsite-disable-click-to-copy") {
				continue
			}
			if strings.Contains(body, "<var") {
				continue
			}
			lang := ""
			if s := syntax.FindStringSubmatch(attrs); s != nil {
				lang = s[1]
			}
			tab := ""
			if len(tabs) > 0 {
				tab = tabs[len(tabs)-1]
			}
			if postgres(lang) || postgres(tab) || (tab == "" && postgres(sectionTitle)) {
				continue
			}
			if lang != "SQL" && lang != "GoogleSQL" && !(lang == "" && strings.Contains(tab, "GoogleSQL")) {
				continue
			}
			text := html.UnescapeString(tagRE.ReplaceAllString(body, ""))
			text = strings.ReplaceAll(text, "\r\n", "\n")
			text = strings.ReplaceAll(text, " ", " ")
			text = resultTableRE.ReplaceAllString(text, "")
			blocks = append(blocks, block{section: section, sql: text})
		default: // heading
			title := strings.TrimSpace(html.UnescapeString(tagRE.ReplaceAllString(m[4], "")))
			if len(tabs) > 0 {
				tabs[len(tabs)-1] = title
				continue
			}
			if id := idAttr.FindStringSubmatch(m[3]); id != nil {
				section, sectionTitle = id[1], title
			}
		}
	}
	return blocks
}

func postgres(s string) bool {
	return strings.Contains(strings.ToLower(s), "postgres") && !strings.Contains(s, "GoogleSQL")
}

// scan walks sql, calling code for each byte outside comments and literals
// and skip for each comment or quoted literal span [i, j). It returns nothing;
// callers build what they need from the callbacks.
func scan(sql string, code func(i int), literal func(i, j int), comment func(i, j int)) {
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == '-' && strings.HasPrefix(sql[i:], "--"), c == '#':
			j := skipLine(sql, i)
			comment(i, j)
			i = j
		case c == '/' && strings.HasPrefix(sql[i:], "/*"):
			j := len(sql)
			if end := strings.Index(sql[i+2:], "*/"); end >= 0 {
				j = i + 2 + end + 2
			}
			comment(i, j)
			i = j
		case c == '\'' || c == '"' || c == '`':
			j := skipQuoted(sql, i)
			literal(i, j)
			i = j
		default:
			code(i)
			i++
		}
	}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

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
	scan(sql,
		func(i int) {
			switch {
			case sql[i] == ';':
				flush(i)
				start = i + 1
			case !isSpace(sql[i]):
				hasCode = true
			}
		},
		func(i, j int) { hasCode = true },
		func(i, j int) {})
	flush(len(sql))
	return out
}

// codeOnly returns sql with comments removed and literals replaced by a
// placeholder, for pattern checks that must not look inside them.
func codeOnly(sql string) string {
	var b strings.Builder
	scan(sql,
		func(i int) { b.WriteByte(sql[i]) },
		func(i, j int) { b.WriteString("''") },
		func(i, j int) { b.WriteByte(' ') })
	return b.String()
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

// splitUnterminated splits a statement at each line that starts, in column
// 0, with the same keyword as its first line (CREATE ... CREATE ..., GRAPH
// ... GRAPH ...), for doc examples that list several statements without ";"
// between them. It returns nil if there is no such line.
func splitUnterminated(sql string) []string {
	lines := strings.Split(sql, "\n")
	first := firstWordRE.FindStringSubmatch(codeOnly(sql))
	if first == nil || !statementStarts[strings.ToUpper(first[1])] {
		return nil
	}
	var parts []string
	start := 0
	for i := 1; i < len(lines); i++ {
		m := firstWordRE.FindStringSubmatch(lines[i])
		if m == nil || isSpace(lines[i][0]) || !strings.EqualFold(m[1], first[1]) {
			continue
		}
		if part := strings.TrimSpace(strings.Join(lines[start:i], "\n")); part != "" {
			parts = append(parts, part)
		}
		start = i
	}
	if start == 0 {
		return nil
	}
	return append(parts, strings.TrimSpace(strings.Join(lines[start:], "\n")))
}
