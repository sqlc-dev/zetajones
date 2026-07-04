// Package testfile parses the file-based test format used by ZetaSQL's
// parser test suite (github.com/google/file-based-test-driver, as consumed by
// googlesql/parser/testdata/*.test).
//
// A file contains test cases separated by lines containing exactly "==".
// Within a case, parts are separated by lines containing exactly "--". The
// first part holds optional leading comment lines (starting with "#"),
// optional option lines (e.g. "[default no_show_parse_location_text]"), and
// the SQL input. The second part is the expected parse tree debug string (or
// an "ERROR: ..." message). The third part, when present, is the expected
// unparse output. Payload lines beginning with "\" are unescaped by removing
// the backslash.
package testfile

import (
	"os"
	"strings"
)

// Case is a single test case within a .test file.
type Case struct {
	// Index is the 1-based position of the case within the file.
	Index int
	// Options are the effective bracketed options for this case, including
	// [default ...] options inherited from earlier cases (with the "default "
	// prefix removed).
	Options []string
	// SQL is the input, with lines joined by \n exactly as the test driver
	// presents it to the parser (byte offsets in expected output index into
	// this string).
	SQL string
	// Expected is the expected parse tree debug string, or an "ERROR: ..."
	// message for negative tests. It has no trailing newline.
	Expected string
	// Unparse is the expected unparse output, if present.
	Unparse string
	// ExtraParts holds any parts beyond the first three, verbatim.
	ExtraParts []string
	// HasAlternation is true if the case uses {{...|...}} alternations, which
	// expand to multiple sub-tests with per-group expected output.
	HasAlternation bool
}

// HasOption reports whether the case has the given option (exact match).
func (c *Case) HasOption(name string) bool {
	for _, opt := range c.Options {
		if opt == name {
			return true
		}
	}
	return false
}

// IsError reports whether the expected output is a parse error.
func (c *Case) IsError() bool {
	return strings.HasPrefix(c.Expected, "ERROR:")
}

// ParseFile reads and parses a .test file.
func ParseFile(path string) ([]*Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(string(data)), nil
}

// Parse parses the contents of a .test file.
func Parse(content string) []*Case {
	lines := strings.Split(content, "\n")
	// Drop a trailing empty line from the final newline so it does not become
	// part of the last case's payload.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	var cases []*Case
	var defaults []string
	var caseLines []string

	flush := func() {
		if len(caseLines) == 0 {
			return
		}
		c := parseCase(caseLines, &defaults)
		if c != nil {
			c.Index = len(cases) + 1
			cases = append(cases, c)
		}
		caseLines = nil
	}

	for _, line := range lines {
		if line == "==" {
			flush()
			continue
		}
		caseLines = append(caseLines, line)
	}
	flush()
	return cases
}

// parseCase parses the lines of one case. defaults accumulates [default ...]
// options across cases in file order.
func parseCase(lines []string, defaults *[]string) *Case {
	// Split into parts on "--" lines.
	var parts [][]string
	current := []string{}
	for _, line := range lines {
		if line == "--" {
			parts = append(parts, current)
			current = []string{}
			continue
		}
		current = append(current, line)
	}
	parts = append(parts, current)

	c := &Case{}

	// First part: comments, options, SQL. The test driver strips the leading
	// comment block (blank and "#" lines); the framework then extracts option
	// lines plus surrounding blank lines. Comment lines after the options are
	// part of the input: expected byte offsets index into them.
	part := parts[0]
	i := 0
	for i < len(part) {
		trimmed := strings.TrimSpace(part[i])
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			break
		}
		i++
	}
	for i < len(part) {
		trimmed := strings.TrimSpace(part[i])
		if trimmed == "" {
			i++
			continue
		}
		opts, ok := parseOptionsLine(trimmed)
		if !ok {
			break
		}
		for _, opt := range opts {
			if rest, isDefault := strings.CutPrefix(opt, "default "); isDefault {
				*defaults = append(*defaults, rest)
			} else {
				c.Options = append(c.Options, opt)
			}
		}
		i++
	}
	var sqlLines []string
	for _, line := range part[i:] {
		sqlLines = append(sqlLines, unescapeLine(line))
	}
	// Strip trailing blank lines from the SQL payload.
	for len(sqlLines) > 0 && strings.TrimSpace(sqlLines[len(sqlLines)-1]) == "" {
		sqlLines = sqlLines[:len(sqlLines)-1]
	}
	c.SQL = strings.Join(sqlLines, "\n")
	c.Options = append(append([]string{}, *defaults...), c.Options...)

	if c.SQL == "" && len(parts) == 1 {
		// A case with no SQL and no expected output (e.g. a comment-only
		// prologue before the first "==") is not a test.
		return nil
	}

	// joinPart extracts the payload text of an output part. Following
	// ParseNextTestCase in file_based_test_driver.cc, blank lines and "#"
	// comment lines at the start or end of a part are comment blocks
	// attached to the part rather than payload, while blank lines in the
	// middle belong to the payload.
	joinPart := func(part []string) string {
		var out, pending []string
		for _, line := range part {
			if line == "" || strings.HasPrefix(line, "#") {
				if len(out) == 0 {
					continue // leading comment block
				}
				pending = append(pending, line)
				continue
			}
			out = append(out, pending...)
			pending = pending[:0]
			out = append(out, unescapeLine(line))
		}
		return strings.Join(out, "\n")
	}

	if len(parts) > 1 {
		c.Expected = joinPart(parts[1])
	}
	if len(parts) > 2 {
		c.Unparse = joinPart(parts[2])
	}
	if len(parts) > 3 {
		for _, part := range parts[3:] {
			c.ExtraParts = append(c.ExtraParts, joinPart(part))
		}
	}

	c.HasAlternation = strings.Contains(c.SQL, "{{") ||
		strings.Contains(c.Expected, "ALTERNATION GROUP")
	return c
}

// parseOptionsLine parses a line of one or more bracketed options like
// "[default mode=statement][no_test_unparse]". Returns ok=false if the line
// is not entirely composed of bracketed options.
func parseOptionsLine(line string) ([]string, bool) {
	if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
		return nil, false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
	return strings.Split(inner, "]["), true
}

// unescapeLine removes the leading backslash used to escape payload lines
// that would otherwise be treated as comments or separators.
func unescapeLine(line string) string {
	if strings.HasPrefix(line, "\\") {
		return line[1:]
	}
	return line
}
