package kube

import (
	cryptorand "crypto/rand"
	"math/big"
	"strings"
)

// maxDNSLabel is the maximum length of a Kubernetes object name that must be a
// DNS label (RFC 1123).
const maxDNSLabel = 63

// Short-id alphabets. Both sets are DNS-label safe (lowercase alphanumeric).
// The first character excludes 0 so an id never leads with a zero; the
// remaining characters may be any lowercase letter or digit.
//
//	id ~= [1-9a-z][a-z0-9]*
const (
	idFirstAlphabet = "123456789abcdefghijklmnopqrstuvwxyz"
	idRestAlphabet  = "0123456789abcdefghijklmnopqrstuvwxyz"

	// DefaultShortIDLength is the default length of a generated short id.
	DefaultShortIDLength = 6
)

// NewShortID returns a random, DNS-label-safe identifier of length n matching
// [1-9a-z][a-z0-9]{n-1}. It is drawn from crypto/rand, so ids are unpredictable
// but — being random rather than derived — not guaranteed unique: callers that
// use it in an object name should retry on a name collision. n < 1 falls back to
// DefaultShortIDLength.
func NewShortID(n int) string {
	if n < 1 {
		n = DefaultShortIDLength
	}
	b := make([]byte, n)
	b[0] = randChar(idFirstAlphabet)
	for i := 1; i < n; i++ {
		b[i] = randChar(idRestAlphabet)
	}
	return string(b)
}

// NameWithID returns "<prefix>-<id>" where id is a random short id of the
// default length. See NameWithIDN.
func NameWithID(prefix string) string {
	return NameWithIDN(prefix, DefaultShortIDLength)
}

// NameWithIDN returns "<prefix>-<id>" where id is a random short id of length n.
// The prefix is lowercased and, if necessary, truncated so the whole name stays
// within the 63-character DNS-label limit. Because the id is random (not
// derived), the result is not guaranteed unique — callers using it as an object
// name should retry with a fresh call on a name collision.
func NameWithIDN(prefix string, n int) string {
	if n < 1 {
		n = DefaultShortIDLength
	}
	id := NewShortID(n)
	prefix = strings.ToLower(strings.TrimRight(prefix, "-."))

	room := maxDNSLabel - len(id) - 1 // reserve one char for the "-" separator
	if room < 1 {
		return id
	}
	if len(prefix) > room {
		prefix = strings.TrimRight(prefix[:room], "-.")
	}
	if prefix == "" {
		return id
	}
	return prefix + "-" + id
}

// randChar returns a uniformly random byte from alphabet using crypto/rand.
func randChar(alphabet string) byte {
	idx, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(len(alphabet))))
	if err != nil {
		// crypto/rand should never fail; degrade deterministically rather than
		// panic inside a library helper.
		return alphabet[0]
	}
	return alphabet[idx.Int64()]
}
