package testfile

import (
	"encoding/json"
	"os"
	"strings"
)

// Metadata is the sidecar state for one vendored .test file, stored next to
// it as <name>.metadata.json. It tracks which cases the parser does not
// handle yet. The vendored .test files themselves are never modified.
type Metadata struct {
	// ParseTodo maps "case_N" to true for cases whose parse tree (or error)
	// does not yet match the expected output.
	ParseTodo map[string]bool `json:"parse_todo,omitempty"`
	// Alternations maps "case_N" to true for cases using {{...}} alternation
	// groups, which the harness cannot expand yet.
	Alternations map[string]bool `json:"alternations,omitempty"`
	// Skip disables the whole file, with a reason (e.g. options the harness
	// cannot model).
	Skip string `json:"skip,omitempty"`
}

// MetadataPath returns the metadata path for a .test file path.
func MetadataPath(testPath string) string {
	return strings.TrimSuffix(testPath, ".test") + ".metadata.json"
}

// LoadMetadata reads the metadata sidecar for a .test file. Returns an empty
// Metadata and found=false if none exists.
func LoadMetadata(testPath string) (meta *Metadata, found bool, err error) {
	data, err := os.ReadFile(MetadataPath(testPath))
	if os.IsNotExist(err) {
		return &Metadata{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var m Metadata
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false, err
	}
	return &m, true, nil
}

// SaveMetadata writes the metadata sidecar for a .test file.
func SaveMetadata(testPath string, meta *Metadata) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(MetadataPath(testPath), append(data, '\n'), 0o644)
}
