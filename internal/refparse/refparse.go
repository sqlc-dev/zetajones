// Package refparse runs GoogleSQL's reference parser, using the prebuilt
// execute_query binary published with googlesql releases
// (https://github.com/google/googlesql/releases), to mint golden parse trees
// for queries that are not in the vendored testdata.
package refparse

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Release is the googlesql release used to generate golden output. It must
// match the release the vendored parser/testdata files were taken from;
// update both together.
const Release = "2026.01.1"

// binDir is where the downloaded binary is cached, relative to the repository
// root (gitignored).
const binDir = ".execute_query"

// Options configure one reference parse.
type Options struct {
	// LanguageFeatures is an --enabled_language_features spec such as
	// "MAXIMUM,+SPANNER_LEGACY_DDL". Empty uses the binary's default.
	LanguageFeatures string
	// Mode is the --sql_mode: "" or "query" for a single statement,
	// "script", or "expression".
	Mode string
}

// Parse runs execute_query in parse mode and returns the parse tree debug
// string (without location text). A parse failure is returned as an error
// carrying the reference's message.
func Parse(bin, sql string, opts Options) (string, error) {
	// The parser test driver prints explicit ASC as "ASC EXPLICITLY";
	// execute_query only does so with this flag.
	args := []string{"--mode=parse", "--output_asc_explicitly"}
	if opts.LanguageFeatures != "" {
		args = append(args, "--enabled_language_features="+opts.LanguageFeatures)
	}
	if opts.Mode != "" {
		args = append(args, "--sql_mode="+opts.Mode)
	}
	// "--" ends flag parsing, so SQL that starts with a "--" comment is not
	// taken for a flag.
	args = append(args, "--", sql)
	cmd := exec.Command(bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("execute_query: %s", msg)
	}
	// Parse errors are printed to stdout with a zero exit status.
	out := strings.TrimSpace(stdout.String())
	if msg, ok := strings.CutPrefix(out, "ERROR: "); ok {
		return "", fmt.Errorf("execute_query: %s", msg)
	}
	return out, nil
}

// EnsureBinary downloads the pinned execute_query release binary if needed
// and returns its path.
func EnsureBinary() (string, error) {
	var asset string
	switch runtime.GOOS {
	case "linux":
		asset = "execute_query_linux"
	case "darwin":
		asset = "execute_query_macos"
	default:
		return "", fmt.Errorf("no prebuilt execute_query binary for %s", runtime.GOOS)
	}

	bin := filepath.Join(binDir, "execute_query-"+Release)
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}

	url := fmt.Sprintf("https://github.com/google/googlesql/releases/download/%s/%s", Release, asset)
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
