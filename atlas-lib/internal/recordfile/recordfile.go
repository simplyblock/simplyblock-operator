// Package recordfile names the file a record keyed by a volume handle is kept
// in, for the packages that persist one per volume on a node: the volume
// stack's records (volstack) and a striped volume's freeze holds (freeze). It
// is one rule in one place because the two kinds of record sit side by side
// under one host directory and are found again by the same handle.
package recordfile

import (
	"path/filepath"
	"strings"
)

// Path is the record of key in dir: the key with every character a handle may
// carry beyond letters, digits, dashes, and dots replaced by an underscore, and
// the JSON extension appended. It never leaves dir, whatever key is: one made
// of nothing but dots, or of nothing at all, is named with a single underscore.
func Path(dir, key string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		default:
			return '_'
		}
	}, key)
	if strings.Trim(safe, ".") == "" {
		safe = "_"
	}
	return filepath.Join(dir, safe+".json")
}
