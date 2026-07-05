package parser_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sqlc-dev/zetajones/internal/testfile"
	"github.com/sqlc-dev/zetajones/parser"
)

const benchSimpleSQL = `SELECT id, name FROM users WHERE status = 'active' ORDER BY created_at DESC LIMIT 10`

const benchComplexSQL = `
WITH revenue AS (
  SELECT o.customer_id, SUM(oi.quantity * oi.unit_price) AS total,
         COUNT(DISTINCT o.order_id) AS orders
  FROM orders AS o
  JOIN order_items AS oi ON o.order_id = oi.order_id
  WHERE o.created_at BETWEEN @start AND @end
    AND o.status NOT IN ('cancelled', 'refunded')
  GROUP BY o.customer_id
  HAVING SUM(oi.quantity * oi.unit_price) > 1000
)
SELECT c.name,
       r.total,
       r.orders,
       RANK() OVER (PARTITION BY c.region ORDER BY r.total DESC) AS region_rank,
       CASE WHEN r.total > 10000 THEN 'gold' WHEN r.total > 5000 THEN 'silver' ELSE 'bronze' END AS tier,
       (SELECT MAX(created_at) FROM logins l WHERE l.customer_id = c.id) AS last_login
FROM customers AS c
JOIN revenue AS r ON r.customer_id = c.id
LEFT JOIN UNNEST(c.tags) AS tag
WHERE EXISTS (SELECT 1 FROM subscriptions s WHERE s.customer_id = c.id AND s.active)
QUALIFY region_rank <= 100
ORDER BY r.total DESC`

func BenchmarkParseSimple(b *testing.B) {
	b.SetBytes(int64(len(benchSimpleSQL)))
	for b.Loop() {
		if _, err := parser.ParseStatement(benchSimpleSQL); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseComplex(b *testing.B) {
	b.SetBytes(int64(len(benchComplexSQL)))
	for b.Loop() {
		if _, err := parser.ParseStatement(benchComplexSQL); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkParseCorpus parses every statement-mode case in the golden corpus
// once per iteration, giving a broad-coverage throughput number across the
// whole grammar (errors included: error paths are part of the workload).
func BenchmarkParseCorpus(b *testing.B) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.test"))
	if err != nil || len(files) == 0 {
		b.Fatal("no testdata files")
	}
	type unit struct {
		sql  string
		opts parser.Options
	}
	var units []unit
	var totalBytes int64
	for _, path := range files {
		cases, err := testfile.ParseFile(path)
		if err != nil {
			b.Fatal(err)
		}
		for _, c := range cases {
			mode := "statement"
			var opts parser.Options
			opts.Features = parser.ParseFeatureSet("NONE")
			for _, opt := range c.Options {
				if m, ok := strings.CutPrefix(opt, "mode="); ok {
					mode = m
				}
				if spec, ok := strings.CutPrefix(opt, "language_features="); ok {
					opts.Features = parser.ParseFeatureSet(spec)
				}
				if opt == "reserve_graph_table" {
					opts.ReserveGraphTable = true
				}
			}
			if mode != "statement" || c.HasAlternation {
				continue
			}
			units = append(units, unit{sql: c.SQL, opts: opts})
			totalBytes += int64(len(c.SQL))
		}
	}
	if len(units) == 0 {
		b.Fatal("no benchmark units")
	}
	b.SetBytes(totalBytes)
	b.ResetTimer()
	for b.Loop() {
		for _, u := range units {
			// Errors are expected for negative cases; the parse attempt is
			// the work being measured either way.
			_, _ = parser.ParseStatementWithOptions(u.sql, u.opts)
		}
	}
}
