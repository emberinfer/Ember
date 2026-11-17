package gateway

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"unicode"
)

// LoadExecExcludeFile accepts opaque public IDs, including non-hex demo IDs.
// IDs must contain a letter or digit; internal whitespace/control characters
// are rejected with a line number. Other punctuation is accepted for opaque IDs;
// only full-line comments are supported, so malformed entries never vanish silently.
func LoadExecExcludeFile(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]bool{}
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		id := strings.TrimSpace(scanner.Text())
		if id == "" || strings.HasPrefix(id, "#") {
			continue
		}
		if !strings.ContainsFunc(id, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) || strings.ContainsFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
			return nil, fmt.Errorf("%s:%d: invalid node id (requires letter/digit and no whitespace/control characters)", path, line)
		}
		out[id] = true
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}
