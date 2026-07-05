package testfile

import "strings"

// Alternation-group ({{a|b|c}}) expansion.
//
// The file_based_test_driver expands a case containing one or more {{...}}
// groups into the cartesian product of the groups' alternatives. The
// alternatives of the last group vary fastest (row-major enumeration). Each
// expansion is introduced in the expected section by a header line:
//
//	ALTERNATION GROUP: <label>          (exactly one label)
//	ALTERNATION GROUPS:                 (two or more labels, one per indented
//	    <label>                          line; the driver coalesces consecutive
//	    <label>                          expansions with identical output)
//
// followed by that expansion's normal expected output. A label is the chosen
// alternatives joined with ",", with leading empty components stripped; the
// all-empty combination is printed as "<empty>". Byte offsets in each
// expansion's expected tree are relative to that expansion's substituted SQL.
//
// This mirrors the driver empirically; nothing here modifies the vendored
// .test goldens. If the structure cannot be confidently expanded (group count
// vs. expected-block count mismatch, or a computed label does not match the
// golden), the case is marked AltUnexpandable and skipped rather than run,
// so the harness never reports a false pass.

const altHeaderPrefix = "ALTERNATION GROUP"

// newlinePrefix marks an expected output block produced under a specific
// newline convention (see file_based_test_driver's [NEWLINE X] annotation,
// emitted when the newline type changes the parser output).
const newlinePrefix = "[NEWLINE "

// expandAlternations populates c.AltCases from the case's alternation groups
// (which may appear in the option lines and/or the SQL) and the raw body parts
// (the "--"-separated segments after the SQL). optionLines are this case's raw
// bracketed option lines (with groups intact); inheritedDefaults are the
// effective [default ...] options carried in from earlier cases. On any
// inconsistency it sets c.AltUnexpandable instead of guessing.
//
// The driver treats groups in appearance order: option-line groups precede SQL
// groups, and the last group varies fastest. Each expansion substitutes its
// option and SQL groups independently, re-parses the substituted option lines
// into per-expansion options (so e.g. language_features alternatives take
// effect), and keeps SQL byte offsets relative to the substituted SQL.
func expandAlternations(c *Case, inheritedDefaults, optionLines []string, bodyParts [][]string) {
	optText := strings.Join(optionLines, "\n")
	optLits, optGroups, ok1 := splitAlternationGroups(optText)
	sqlLits, sqlGroups, ok2 := splitAlternationGroups(c.SQL)
	if !ok1 || !ok2 {
		c.AltUnexpandable = true
		return
	}
	nOpt := len(optGroups)
	allGroups := append(append([][]string(nil), optGroups...), sqlGroups...)
	if len(allGroups) == 0 {
		c.AltUnexpandable = true
		return
	}

	// Enumerate the cartesian product with the last group varying fastest.
	type expansion struct {
		sql     string
		label   string
		options []string
	}
	var expansions []expansion
	idx := make([]int, len(allGroups))
	for {
		comps := make([]string, len(allGroups))
		for i := range allGroups {
			comps[i] = allGroups[i][idx[i]]
		}
		substOpt := substitute(optLits, optGroups, idx[:nOpt])
		substSQL := substitute(sqlLits, sqlGroups, idx[nOpt:])
		options := append(append([]string(nil), inheritedDefaults...), parseChildOptions(substOpt)...)
		expansions = append(expansions, expansion{
			sql:     substSQL,
			label:   alternationLabel(comps),
			options: options,
		})

		// Increment the mixed-radix counter from the rightmost group.
		k := len(allGroups) - 1
		for k >= 0 {
			idx[k]++
			if idx[k] < len(allGroups[k]) {
				break
			}
			idx[k] = 0
			k--
		}
		if k < 0 {
			break
		}
	}

	// Special case: when every expansion produces identical output, the driver
	// omits the "ALTERNATION GROUP" headers entirely and prints the shared
	// output once (like a normal case). Detect that and share it across all.
	hasHeader := false
	for _, p := range bodyParts {
		if isAltHeader(p) {
			hasHeader = true
			break
		}
	}
	if !hasHeader {
		if len(bodyParts) == 0 {
			c.AltUnexpandable = true
			return
		}
		shared := joinPart(bodyParts[0])
		children := make([]*Case, len(expansions))
		for i, e := range expansions {
			children[i] = &Case{
				Index:    i + 1,
				Options:  e.options,
				SQL:      e.sql,
				Expected: shared,
				AltLabel: e.label,
			}
		}
		c.AltCases = children
		return
	}

	// Parse the expected body into a label->output map. The driver coalesces
	// every expansion with identical output under one "ALTERNATION GROUPS:"
	// block, and those expansions need not be adjacent in enumeration order, so
	// match by label rather than by position.
	expected, ok := parseAltBlocks(bodyParts)
	if !ok {
		c.AltUnexpandable = true
		return
	}
	// A label may legitimately repeat when a group lists the same alternative
	// twice (e.g. "{{|w|AS w|}}" has two empty alternatives): distinct
	// combinations then share a label and, being equivalent, the same output.
	// Allow repeats only when the output agrees; a label appearing with two
	// different outputs is a genuine ambiguity we cannot resolve.
	byLabel := make(map[string]string, len(expected))
	for _, e := range expected {
		if prev, dup := byLabel[e.label]; dup {
			if prev != e.output {
				c.AltUnexpandable = true
				return
			}
			continue
		}
		byLabel[e.label] = e.output
	}
	// Require the golden's set of labels to equal the set our enumeration
	// produces, so we neither miss expected blocks nor invent labels.
	expLabels := make(map[string]bool, len(expansions))
	for _, e := range expansions {
		expLabels[e.label] = true
	}
	if len(byLabel) != len(expLabels) {
		c.AltUnexpandable = true
		return
	}

	children := make([]*Case, len(expansions))
	for i, e := range expansions {
		output, found := byLabel[e.label]
		if !found {
			// The computed label is absent from the golden: our enumeration or
			// label logic disagrees with the driver. Refuse to guess.
			c.AltUnexpandable = true
			return
		}
		children[i] = &Case{
			Index:    i + 1,
			Options:  e.options,
			SQL:      e.sql,
			Expected: output,
			AltLabel: e.label,
		}
	}
	c.AltCases = children
}

// substitute rebuilds text from splitAlternationGroups literals by choosing
// groups[i][idx[i]] between each literal. With no groups it returns the single
// literal unchanged.
func substitute(literals []string, groups [][]string, idx []int) string {
	var b strings.Builder
	for i := range groups {
		b.WriteString(literals[i])
		b.WriteString(groups[i][idx[i]])
	}
	b.WriteString(literals[len(literals)-1])
	return b.String()
}

// parseChildOptions parses substituted option-line text into a flat option
// list, stripping any "default " prefix so the option applies to this
// expansion. Blank lines are ignored.
func parseChildOptions(optText string) []string {
	var out []string
	for _, line := range strings.Split(optText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		opts, ok := parseOptionsLine(line)
		if !ok {
			continue
		}
		for _, opt := range opts {
			out = append(out, strings.TrimPrefix(opt, "default "))
		}
	}
	return out
}

// splitAlternationGroups splits sql into literal segments and alternation
// groups. literals has len(groups)+1 entries interleaved with the groups, so
// the substituted SQL for a combination is literals[0] + choice0 +
// literals[1] + choice1 + ... + literals[n]. Each group is the "|"-split
// alternatives, exactly as the driver splits them (empty alternatives are
// preserved). Returns ok=false if a "{{" has no matching "}}".
func splitAlternationGroups(sql string) (literals []string, groups [][]string, ok bool) {
	rest := sql
	for {
		open := strings.Index(rest, "{{")
		if open < 0 {
			literals = append(literals, rest)
			return literals, groups, true
		}
		close := strings.Index(rest[open+2:], "}}")
		if close < 0 {
			return nil, nil, false
		}
		literals = append(literals, rest[:open])
		inner := rest[open+2 : open+2+close]
		groups = append(groups, strings.Split(inner, "|"))
		rest = rest[open+2+close+2:]
	}
}

// alternationLabel builds the driver's label for a combination: the chosen
// alternatives joined with ",", with leading empty components dropped. The
// all-empty combination yields "" (which the driver renders as "<empty>";
// parseAltBlocks normalizes that placeholder back to "").
func alternationLabel(comps []string) string {
	start := 0
	for start < len(comps) && comps[start] == "" {
		start++
	}
	if start == len(comps) {
		return ""
	}
	return strings.Join(comps[start:], ",")
}

// altExpected is one expansion's label and its cleaned expected output.
type altExpected struct {
	label  string
	output string
}

// parseAltBlocks walks the "--"-separated body segments and returns one
// altExpected per expansion, in output order. A header segment beginning with
// "ALTERNATION GROUP" starts a block; the immediately following segment is the
// block's expected parse tree/error, shared by every label the header lists
// (the driver coalesces expansions with identical output under one block).
// Any segments after that (e.g. the unparse output) are skipped until the next
// header. Returns ok=false if the structure is unexpected.
func parseAltBlocks(parts [][]string) ([]altExpected, bool) {
	var out []altExpected
	i := 0
	for i < len(parts) {
		if !isAltHeader(parts[i]) {
			return nil, false
		}
		labels := parseAltHeaderLabels(parts[i])
		if len(labels) == 0 || i+1 >= len(parts) {
			return nil, false
		}
		output := joinPart(parts[i+1])
		i += 2
		// A [NEWLINE X] block is one of several outputs the driver emits for the
		// same expansion, one per newline convention, separated by "--". Coalesce
		// the consecutive [NEWLINE ...] sub-blocks into a single expected output
		// (runCase reproduces the same multi-run join). Ordinary expansions have
		// a single output block here, so this leaves them unchanged.
		if strings.HasPrefix(output, newlinePrefix) {
			for i < len(parts) && !isAltHeader(parts[i]) {
				block := joinPart(parts[i])
				if !strings.HasPrefix(block, newlinePrefix) {
					break
				}
				output += "\n--\n" + block
				i++
			}
		}
		for _, lab := range labels {
			out = append(out, altExpected{label: lab, output: output})
		}
		for i < len(parts) && !isAltHeader(parts[i]) {
			i++
		}
	}
	return out, true
}

// isAltHeader reports whether a body segment is an ALTERNATION GROUP(S) header.
// A header may be preceded by blank comment lines within the segment.
func isAltHeader(part []string) bool {
	for _, line := range part {
		if strings.TrimSpace(line) == "" {
			continue
		}
		return strings.HasPrefix(line, altHeaderPrefix)
	}
	return false
}

// parseAltHeaderLabels extracts the labels from a header segment. The singular
// "ALTERNATION GROUP: <label>" form carries one label on the header line
// (trailing whitespace preserved, since alternatives may end in a space). The
// plural "ALTERNATION GROUPS:" form lists each label on its own indented line.
// The "<empty>" placeholder is normalized to "". Leading blank lines within the
// segment are ignored.
func parseAltHeaderLabels(part []string) []string {
	j := 0
	for j < len(part) && strings.TrimSpace(part[j]) == "" {
		j++
	}
	if j >= len(part) {
		return nil
	}
	first := part[j]
	if _, ok := strings.CutPrefix(first, "ALTERNATION GROUPS:"); ok {
		var labels []string
		for _, line := range part[j+1:] {
			if strings.TrimSpace(line) == "" {
				continue
			}
			labels = append(labels, normalizeAltLabel(strings.TrimPrefix(line, "    ")))
		}
		return labels
	}
	if rest, ok := strings.CutPrefix(first, "ALTERNATION GROUP: "); ok {
		return []string{normalizeAltLabel(rest)}
	}
	return nil
}

func normalizeAltLabel(label string) string {
	if label == "<empty>" {
		return ""
	}
	return label
}
