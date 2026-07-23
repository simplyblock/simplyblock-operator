package kube

import (
	"strings"
	"testing"
)

func TestNewShortID(t *testing.T) {
	const first = idFirstAlphabet
	const rest = idRestAlphabet
	for i := 0; i < 200; i++ {
		id := NewShortID(6)
		if len(id) != 6 {
			t.Fatalf("len = %d, want 6 (%q)", len(id), id)
		}
		if !strings.ContainsRune(first, rune(id[0])) {
			t.Fatalf("first char %q not in %q (id=%q)", id[0], first, id)
		}
		for j := 1; j < len(id); j++ {
			if !strings.ContainsRune(rest, rune(id[j])) {
				t.Fatalf("char %q not in %q (id=%q)", id[j], rest, id)
			}
		}
	}
}

func TestNewShortIDDefaultsOnNonPositive(t *testing.T) {
	if got := len(NewShortID(0)); got != DefaultShortIDLength {
		t.Fatalf("len = %d, want default %d", got, DefaultShortIDLength)
	}
}

func TestNameWithID(t *testing.T) {
	name := NameWithID("simplyblock-node")
	if !strings.HasPrefix(name, "simplyblock-node-") {
		t.Fatalf("missing prefix: %q", name)
	}
	if len(name) > maxDNSLabel {
		t.Fatalf("name exceeds DNS label limit: %d", len(name))
	}
	// distinct across calls (random suffix)
	seen := map[string]struct{}{}
	for i := 0; i < 50; i++ {
		seen[NameWithID("p")] = struct{}{}
	}
	if len(seen) < 50 {
		t.Fatalf("expected 50 distinct names, got %d", len(seen))
	}
}

func TestNameWithIDTruncatesLongPrefix(t *testing.T) {
	name := NameWithIDN(strings.Repeat("a", 100), 6)
	if len(name) > maxDNSLabel {
		t.Fatalf("len = %d, want <= %d", len(name), maxDNSLabel)
	}
	if strings.HasSuffix(name, "-") || strings.HasPrefix(name, "-") {
		t.Fatalf("name has dangling hyphen: %q", name)
	}
}
