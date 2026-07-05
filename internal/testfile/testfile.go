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
	// AltCases holds the concrete sub-cases for an alternation case, in the
	// test driver's cartesian expansion order (the last group varies fastest).
	// Each sub-case has its SQL substituted and Expected set to that
	// expansion's parse tree (or error). Nil when the case has no alternations
	// or could not be expanded (see AltUnexpandable). Each sub-case's Index is
	// its 1-based expansion number and AltLabel is the driver's group label.
	AltCases []*Case
	// AltUnexpandable is set when a case uses alternations but the harness
	// could not confidently expand it (e.g. the number of expected ALTERNATION
	// GROUP blocks did not match the cartesian product, or a label did not
	// match). Such cases are skipped rather than run, to avoid false passes.
	AltUnexpandable bool
	// AltLabel is the driver's alternation-group label for a sub-case (the
	// comma-joined chosen alternatives, leading-empty components stripped, or
	// "" for the all-empty combination that the driver prints as "<empty>").
	AltLabel string
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

// BoolOption resolves a boolean option that may be toggled by "name" and
// "no_<name>" option lines (including via inherited [default ...] options),
// returning the value of the last occurrence. This matches the driver's
// last-wins semantics; e.g. [default parse_multiple] followed later by
// [default no_parse_multiple] yields false. Defaults to false.
func (c *Case) BoolOption(name string) bool {
	val := false
	no := "no_" + name
	for _, opt := range c.Options {
		switch opt {
		case name:
			val = true
		case no:
			val = false
		}
	}
	return val
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
	inheritedDefaults := append([]string(nil), *defaults...)
	var optionLines []string
	for i < len(part) {
		trimmed := strings.TrimSpace(part[i])
		if trimmed == "" {
			i++
			continue
		}
		if _, ok := parseOptionsLine(trimmed); !ok {
			break
		}
		optionLines = append(optionLines, trimmed)
		i++
	}
	// Parse pure (non-alternation) option lines into effective options and
	// inherited defaults. Option lines containing "{{" hold alternation groups
	// and are expanded per-expansion instead (see expandAlternations); they are
	// not treated as fixed options here.
	optHasGroup := false
	for _, line := range optionLines {
		if strings.Contains(line, "{{") {
			optHasGroup = true
			// The alternation expands the declaring case (via expandAlternations
			// below), but for inheritance by *subsequent* cases the driver leaves
			// the default resolved to the last-iterated alternative (the last
			// alternative of each group, since the last group varies fastest).
			// Inherit that resolved value so later cases carry the right options.
			resolved := resolveLastAlternative(line)
			if opts, ok := parseOptionsLine(resolved); ok {
				for _, opt := range opts {
					if rest, isDefault := strings.CutPrefix(opt, "default "); isDefault {
						*defaults = append(*defaults, rest)
					}
				}
			}
			continue
		}
		opts, _ := parseOptionsLine(line)
		for _, opt := range opts {
			if rest, isDefault := strings.CutPrefix(opt, "default "); isDefault {
				*defaults = append(*defaults, rest)
			} else {
				c.Options = append(c.Options, opt)
			}
		}
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

	c.HasAlternation = optHasGroup || strings.Contains(c.SQL, "{{") ||
		strings.Contains(joinAll(parts[1:]), "ALTERNATION GROUP")

	if c.HasAlternation {
		// Alternation cases have a non-standard body: the expected section is
		// a sequence of "ALTERNATION GROUP(S):" blocks, each with its own
		// parse tree/error, separated by "--". Expand them rather than using
		// the normal three-part split (which would mangle the many "--"s).
		// Groups may appear in the option lines and/or the SQL.
		expandAlternations(c, inheritedDefaults, optionLines, parts[1:])
		return c
	}

	// Under parse_multiple the driver parses each ";"-separated statement and
	// emits one output per statement, all "--"-separated with no unparse output
	// (see TestMulti in run_parser_test.cc). Every output part is therefore an
	// expected parse tree; join them so runCase can reproduce the same sequence.
	if c.BoolOption("parse_multiple") {
		var outs []string
		for _, part := range parts[1:] {
			outs = append(outs, joinPart(part))
		}
		c.Expected = strings.Join(outs, "\n--\n")
		return c
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

	return c
}

// resolveLastAlternative substitutes each {{a|b|c}} group in a single option
// line with the group's last alternative. This mirrors the file_based_test
// driver leaving a [default ...{{...}}...] value resolved to the final
// enumerated combination (the last group varies fastest) for inheritance by
// subsequent cases.
func resolveLastAlternative(line string) string {
	literals, groups, ok := splitAlternationGroups(line)
	if !ok {
		return line
	}
	idx := make([]int, len(groups))
	for i := range groups {
		idx[i] = len(groups[i]) - 1
	}
	return substitute(literals, groups, idx)
}

// joinAll concatenates the raw lines of all parts, used only for cheap
// substring detection.
func joinAll(parts [][]string) string {
	var b strings.Builder
	for _, part := range parts {
		for _, line := range part {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// joinPart extracts the payload text of an output part. Following
// ParseNextTestCase in file_based_test_driver.cc, blank lines and "#"
// comment lines at the start or end of a part are comment blocks
// attached to the part rather than payload, while blank lines in the
// middle belong to the payload.
func joinPart(part []string) string {
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
