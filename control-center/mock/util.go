package main

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

// deepCopy copies the JSON-shaped value (maps, slices, scalars) so callers can
// never mutate the store's copy or race against it.
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, vv := range t {
			m[k] = deepCopy(vv)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, vv := range t {
			s[i] = deepCopy(vv)
		}
		return s
	default:
		return v
	}
}

// getPath resolves a dotted path ("metadata.name") in a JSON-shaped object.
// Returns nil when any segment is missing.
func getPath(o any, path string) any {
	cur := o
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[seg]
		if !ok {
			return nil
		}
	}
	return cur
}

// setPath sets a dotted path, creating intermediate maps.
func setPath(o map[string]any, path string, v any) {
	segs := strings.Split(path, ".")
	cur := o
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[seg] = next
		}
		cur = next
	}
	cur[segs[len(segs)-1]] = v
}

func getStr(o any, path string) string {
	if s, ok := getPath(o, path).(string); ok {
		return s
	}
	return ""
}

// mergePatch applies an RFC 7386 JSON merge patch and returns the result.
// Neither input is mutated.
func mergePatch(target, patch any) any {
	p, ok := patch.(map[string]any)
	if !ok {
		return deepCopy(patch)
	}
	var t map[string]any
	if tm, ok := target.(map[string]any); ok {
		t = deepCopy(tm).(map[string]any)
	} else {
		t = map[string]any{}
	}
	for k, v := range p {
		if v == nil {
			delete(t, k)
		} else {
			t[k] = mergePatch(t[k], v)
		}
	}
	return t
}

// uuidFrom renders 16 random bytes from the rng as a version-4-shaped UUID.
// Seeded rng in, reproducible identities out.
func uuidFrom(r *rand.Rand) string {
	b := make([]byte, 16)
	for i := range b {
		b[i] = byte(r.UintN(256))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
