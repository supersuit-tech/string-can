package canon

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func normalizeFiles(files []File) ([]File, error) {
	if len(files) == 0 {
		return nil, nil
	}
	if len(files) > MaxFiles {
		return nil, fmt.Errorf("too many files")
	}
	out := make([]File, len(files))
	seen := map[string]bool{}
	for i, f := range files {
		name, err := SafeName(f.Name)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate file name %s", name)
		}
		seen[name] = true
		if f.Size < 0 || f.Size > ProtocolMaxFileBytes {
			return nil, fmt.Errorf("invalid file size")
		}
		if err := CheckType(f.Type); err != nil {
			return nil, err
		}
		sum, err := NormalizeSHA(f.SHA256)
		if err != nil {
			return nil, err
		}
		out[i] = File{Name: name, Size: f.Size, Type: f.Type, SHA256: sum}
	}
	return out, nil
}

// SafeName reports whether name is a single path segment with no traversal.
// The returned name is the name that may be stored. Unsafe names are rejected,
// not rewritten, so the signed name and the saved name stay the same.
func SafeName(name string) (string, error) {
	if name == "" || len(name) > 255 || !utf8.ValidString(name) {
		return "", fmt.Errorf("invalid file name")
	}
	if name == "." || name == ".." {
		return "", fmt.Errorf("invalid file name")
	}
	if strings.ContainsAny(name, "/\\") || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("invalid file name")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("invalid file name")
		}
	}
	if filepath.Base(name) != name || filepath.Clean(name) != name {
		return "", fmt.Errorf("invalid file name")
	}
	return name, nil
}

// CheckType reports whether t is a bare content type with no parameters.
func CheckType(t string) error {
	if t == "" || len(t) > 128 || !utf8.ValidString(t) {
		return fmt.Errorf("invalid content type")
	}
	for _, r := range t {
		if r <= 0x20 || r >= 0x7f || r == '"' || r == '\\' || r == ';' {
			return fmt.Errorf("invalid content type")
		}
	}
	return nil
}

// NormalizeSHA lowercases a 64-character hex SHA-256 digest.
func NormalizeSHA(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) != shaLen {
		return "", fmt.Errorf("invalid sha256")
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return "", fmt.Errorf("invalid sha256")
		}
	}
	return s, nil
}
