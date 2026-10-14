package parser_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sqlc-dev/zetajones/parser"
)

// spannerDDLFeatures is the feature set the Spanner-only backlog is parsed
// with: the corpus's reference features plus zetajones's SPANNER_DDL.
const spannerDDLFeatures = "MAXIMUM,+SPANNER_LEGACY_DDL,+TTL,+CREATE_LOCALITY_GROUP,+SQL_GRAPH,+SPANNER_DDL"

// spannerOnlyStatement is one statement of a generated *.spanner.sql file.
type spannerOnlyStatement struct {
	id, sql string
}

// readSpannerOnlyStatements splits a *.spanner.sql file (written by
// writeRejects in cmd/spanner-corpus) into its statements. Each block is a
// "# <url>" line, a "# id <id>[: note]" line, a "# <error>" line, then the SQL
// followed by ";".
func readSpannerOnlyStatements(t *testing.T, path string) []spannerOnlyStatement {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []spannerOnlyStatement
	blocks := strings.Split(string(data), "\n# https://")
	for _, block := range blocks[1:] {
		lines := strings.SplitN(block, "\n", 4)
		if len(lines) < 4 || !strings.HasPrefix(lines[1], "# id ") {
			t.Fatalf("%s: malformed block:\n%s", path, block)
		}
		id, _, _ := strings.Cut(strings.TrimPrefix(lines[1], "# id "), ":")
		sql := strings.TrimSuffix(strings.TrimRight(lines[3], "\n"), ";")
		// The id is a hash of the SQL (stmtID in cmd/spanner-corpus), so this
		// catches any drift between the file format and this reader.
		sum := sha256.Sum256([]byte(sql))
		if got := hex.EncodeToString(sum[:])[:12]; got != id {
			t.Fatalf("%s: id %s does not match its SQL (hash %s):\n%s", path, id, got, sql)
		}
		out = append(out, spannerOnlyStatement{id: id, sql: sql})
	}
	return out
}

// TestSpannerOnlySyntax checks that every statement in the Spanner-only
// backlog (testdata/spanner/**/*.spanner.sql, syntax the reference parser
// rejects) parses with SPANNER_DDL and is still rejected without it. Expected
// trees for this syntax live in testdata/extensions/spanner.
func TestSpannerOnlySyntax(t *testing.T) {
	var files []string
	err := filepath.WalkDir(filepath.Join("testdata", "spanner"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(path, ".spanner.sql") {
			files = append(files, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no .spanner.sql files found")
	}
	with := parser.Options{Features: parser.ParseFeatureSet(spannerDDLFeatures)}
	without := parser.Options{Features: parser.ParseFeatureSet(spannerDDLFeatures + ",-SPANNER_DDL")}
	for _, path := range files {
		for _, st := range readSpannerOnlyStatements(t, path) {
			name := strings.TrimSuffix(strings.TrimPrefix(path, filepath.Join("testdata", "spanner")+string(filepath.Separator)), ".spanner.sql") + "/" + st.id
			t.Run(name, func(t *testing.T) {
				// The corpus runs these in script mode when they have
				// comments or several statements; script mode accepts both.
				if _, err := parser.ParseScriptWithOptions(st.sql+"\n", with); err != nil {
					t.Errorf("parse with SPANNER_DDL failed:\n%s\n\n%v", st.sql, err)
				}
				if _, err := parser.ParseScriptWithOptions(st.sql+"\n", without); err == nil {
					t.Errorf("parse without SPANNER_DDL succeeded:\n%s", st.sql)
				}
			})
		}
	}
}
