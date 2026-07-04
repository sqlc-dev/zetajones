// Command regenerate-parse runs GoogleSQL's reference implementation to
// produce golden parse trees for new test queries, using the prebuilt
// execute_query binary published with googlesql releases
// (https://github.com/google/googlesql/releases).
//
// The vendored files in parser/testdata come directly from the googlesql
// repository and should never be regenerated or modified. This tool exists to
// mint expected output for NEW queries (e.g. from bug reports) so they can be
// added as additional test cases.
//
// Usage:
//
//	go run ./cmd/regenerate-parse -sql "select 1 + 2"
//	go run ./cmd/regenerate-parse -file query.sql
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Required googlesql release for generating golden files. This must match the
// release the vendored parser/testdata files were taken from; update both
// together.
const requiredRelease = "2026.01.1"

const binDir = ".execute_query"

func main() {
	sql := flag.String("sql", "", "SQL statement to parse")
	file := flag.String("file", "", "File containing the SQL statement to parse")
	flag.Parse()

	if (*sql == "") == (*file == "") {
		fmt.Fprintln(os.Stderr, "exactly one of -sql or -file is required")
		os.Exit(1)
	}
	if *file != "" {
		data, err := os.ReadFile(*file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading %s: %v\n", *file, err)
			os.Exit(1)
		}
		*sql = string(data)
	}

	bin, err := ensureBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "setting up execute_query: %v\n", err)
		os.Exit(1)
	}

	out, err := runParse(bin, *sql)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	fmt.Println(out)
}

// runParse runs execute_query in parse mode and returns the parse tree debug
// string.
func runParse(bin, sql string) (string, error) {
	cmd := exec.Command(bin, "--mode=parse", sql)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("execute_query: %s", msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// ensureBinary downloads the pinned execute_query release binary if needed
// and returns its path.
func ensureBinary() (string, error) {
	var asset string
	switch runtime.GOOS {
	case "linux":
		asset = "execute_query_linux"
	case "darwin":
		asset = "execute_query_macos"
	default:
		return "", fmt.Errorf("no prebuilt execute_query binary for %s", runtime.GOOS)
	}

	bin := filepath.Join(binDir, "execute_query-"+requiredRelease)
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}

	url := fmt.Sprintf("https://github.com/google/googlesql/releases/download/%s/%s", requiredRelease, asset)
	fmt.Fprintf(os.Stderr, "Downloading %s...\n", url)

	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("downloading: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	tmp := bin + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("writing binary: %w", err)
	}
	out.Close()
	if err := os.Rename(tmp, bin); err != nil {
		os.Remove(tmp)
		return "", err
	}
	fmt.Fprintf(os.Stderr, "Installed %s\n", bin)
	return bin, nil
}
