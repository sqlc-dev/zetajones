package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Verdicts for statements the reference parser rejects, recorded in
// triage.txt.
const (
	// verdictSpanner is valid Spanner syntax that GoogleSQL's parser does not
	// accept. These are written to <page>.spanner.sql as the backlog of
	// Spanner-only syntax.
	verdictSpanner = "spanner"
	// verdictTypo is an example that is broken in the docs. Dropped.
	verdictTypo = "typo"
	// verdictFragment is a grammar template or partial statement the
	// fragment heuristics miss. Dropped.
	verdictFragment = "fragment"
)

type verdict struct {
	kind string
	note string
}

// stmtID is the stable id triage.txt uses for a statement.
func stmtID(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:])[:12]
}

// loadTriage reads triage.txt: one "<id> <verdict> <note...>" per line, with
// "#" comments.
func loadTriage(path string) (map[string]verdict, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]verdict{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("%s:%d: want \"<id> <verdict> [note]\"", path, n)
		}
		switch fields[1] {
		case verdictSpanner, verdictTypo, verdictFragment:
		default:
			return nil, fmt.Errorf("%s:%d: unknown verdict %q", path, n, fields[1])
		}
		out[fields[0]] = verdict{kind: fields[1], note: strings.Join(fields[2:], " ")}
	}
	return out, sc.Err()
}

// statementStarts are the keywords a complete statement or script can begin
// with. A rejected example starting with anything else (an identifier, a
// clause keyword like MATCH or OPTIONS, "|>") is a fragment.
var statementStarts = map[string]bool{}

func init() {
	for _, kw := range strings.Fields(`
		ABORT ALTER ANALYZE ASSERT BEGIN BREAK CALL CASE CLONE COMMIT CONTINUE
		CREATE DECLARE DEFINE DELETE DESCRIBE DROP EXECUTE EXPLAIN EXPORT FOR
		FROM GRANT GRAPH IF IMPORT INSERT ITERATE LEAVE LOAD LOOP MERGE RAISE
		RENAME REPEAT RETURN REVOKE ROLLBACK RUN SELECT SET SHOW START TABLE
		TRUNCATE UNDROP UPDATE VALUES WHILE WITH`) {
		statementStarts[kw] = true
	}
}

// grammarMarkers match reference-grammar notation in code (outside literals
// and comments): ellipses, optional "[AS]"-style brackets, "{a | b}"
// alternations, <lowercase placeholders>, and markdown fences leaked into a
// code block.
var grammarMarkers = []*regexp.Regexp{
	regexp.MustCompile(`\.\.\.|…`),
	regexp.MustCompile(`(?i)\[\s*(?:,|\[|AS\b|NOT\b|OR\b|IF\b|FROM\b|COLUMN\b|ONLY\b|RECURSIVE\b|DISTINCT\b|ALL\b|VECTOR\b|PARENT\b)`),
	regexp.MustCompile(`\{[^{}]*\|[^{}]*\}`),
	regexp.MustCompile(`<[a-z][a-z0-9]*[_ ][a-z0-9_ ]*>`),
	regexp.MustCompile("```"),
	regexp.MustCompile(`//`),                 // pseudo-comments like "// INVALID"
	regexp.MustCompile(`\$[A-Za-z_]`),        // $placeholder macros
	regexp.MustCompile(`^\s*@\{[^}]*\}\s*$`), // a hint on its own
}

// documentedErrorRE matches a leading comment that marks an example as
// intentionally invalid.
var documentedErrorRE = regexp.MustCompile(`(?i)^\s*(?:--|#)[^\n]*\b(?:error|unsupported|invalid|not supported)\b`)

// graphPatternRE matches a bare graph pattern such as "(p:Person)",
// "(a)-[e]->(b)" or "((a))(-[t]->(b))": parentheses, then an optional
// variable and a label, property spec, WHERE, edge or closing paren.
var graphPatternRE = regexp.MustCompile(`(?i)^\s*\(+\s*(?:-|\w*\s*(?::|\bIS\b|\{|\bWHERE\b|\)))`)

var firstWordRE = regexp.MustCompile(`^\s*([A-Za-z_]+|\S)`)

// fragmentReason reports why a rejected statement looks like a grammar
// fragment rather than a complete statement, or "" if it does not.
func fragmentReason(sql string) string {
	if documentedErrorRE.MatchString(sql) {
		return "documented error example"
	}
	code := codeOnly(sql)
	if graphPatternRE.MatchString(code) {
		return "bare graph pattern"
	}
	for _, re := range grammarMarkers {
		if m := re.FindString(code); m != "" {
			return fmt.Sprintf("grammar notation %q", m)
		}
	}
	m := firstWordRE.FindStringSubmatch(code)
	if m == nil {
		return "no code"
	}
	switch w := m[1]; {
	case w == "(" || w == "@":
		return ""
	case !statementStarts[strings.ToUpper(w)]:
		return fmt.Sprintf("starts with %q", w)
	}
	return ""
}
