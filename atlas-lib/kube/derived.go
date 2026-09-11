// Derived Kubernetes identifiers: the object names, label values, and label keys
// this product builds out of names a user, or a cloud, chose. It lives here
// rather than in either consumer because the operator derives the names and the
// upgrade tooling has to predict them, and a second implementation of the
// formula would let the two disagree about what an object is called.
//
// The rule the package enforces is that a derived identifier always fits the
// limit that binds it. Where a name travels into a label, the label's 63 bytes
// bind it rather than the 253 an object name may be, so the limit is a property
// of the formula and not of the object the value is written on.

package kube

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// The two limits every derived identifier is measured against. They are the
// API server's own, restated here so a formula can be checked without a live
// cluster.
const (
	// MaxLabelValueLength bounds a label value, and the name part of a label
	// key after the slash.
	MaxLabelValueLength = validation.LabelValueMaxLength

	// MaxObjectNameLength bounds an object name, custom resources and
	// ConfigMap, Secret, DaemonSet, and StorageClass alike.
	MaxObjectNameLength = validation.DNS1123SubdomainMaxLength
)

// digestLength is how much of the SHA-256 a truncated identifier carries. Eight
// hex characters is 32 bits, which is not a cryptographic claim: the digest is
// there to keep two truncated names apart, and the inputs it disambiguates are
// always within one cluster.
const digestLength = 8

// Kind is what a derived identifier is going to be used as, which decides the
// limit that binds it and the characters it may carry.
type Kind int

const (
	// ObjectName is a metadata.name, bounded at 253 bytes and holding to
	// DNS-1123 subdomain syntax.
	ObjectName Kind = iota

	// LabelValue is a label or annotation value used as a label, bounded at 63
	// bytes. It admits upper case and the underscore that an object name does
	// not.
	LabelValue

	// LabelKeyName is the part of a label key after the slash, bounded at 63
	// bytes and holding to the same syntax as a label value.
	LabelKeyName
)

// limit reports the byte budget the kind is held to.
func (k Kind) limit() int {
	if k == ObjectName {
		return MaxObjectNameLength
	}
	return MaxLabelValueLength
}

// unsafeForObjectName matches everything a DNS-1123 subdomain may not carry. A
// worker is named ip-10-0-1-23.eu-central-1.compute.internal on one cloud and
// worker_3 in somebody's laboratory, and only the first of those is already a
// legal object name.
var unsafeForObjectName = regexp.MustCompile(`[^a-z0-9.-]+`)

// unsafeForLabel matches everything a label value may not carry. It is the
// looser of the two, since a label value keeps its case and admits underscores.
var unsafeForLabel = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitize replaces every character the kind forbids and trims the ends, which
// may not carry a separator whichever kind it is.
func (k Kind) sanitize(s string) string {
	if k == ObjectName {
		return strings.Trim(unsafeForObjectName.ReplaceAllString(strings.ToLower(s), "-"), ".-")
	}
	return strings.Trim(unsafeForLabel.ReplaceAllString(s, "-"), "._-")
}

// validate reports what the API server would say about the value, using the
// API server's own helpers rather than a second opinion about them.
func (k Kind) validate(s string) []string {
	switch k {
	case ObjectName:
		return validation.IsDNS1123Subdomain(s)
	case LabelKeyName:
		return validation.IsQualifiedName(s)
	default:
		return validation.IsValidLabelValue(s)
	}
}

// Formula describes how one derived identifier is built out of the parts handed
// to [Formula.Derive]: a fixed prefix, the parts joined on a separator, and a
// fixed suffix. Declaring it as data rather than as a function is what lets the
// upgrade tooling enumerate every formula in the product and report the input
// length each one admits.
//
// The zero Formula derives an object name from the parts alone.
type Formula struct {
	// Prefix is written before the parts, and Suffix after them. Both count
	// against the limit, and neither is sanitized: a formula's own literal text
	// is expected to be legal already.
	Prefix string
	Suffix string

	// Separator joins the parts. It defaults to a dash, which is what every
	// object-name formula in this product uses. The label keys built from a
	// namespace, a cluster, and a pool join on a dot instead.
	Separator string

	// Kind decides the limit and the syntax. It defaults to [ObjectName].
	Kind Kind

	// Limit overrides the limit the kind implies, for a name that travels
	// somewhere tighter than the object it is written on. A Job's name is
	// copied into its pods as a label, so a formula that names a Job sets 63
	// here while remaining an ObjectName.
	Limit int
}

// Derived is one identifier a [Formula] produced, carrying enough to report a
// violation as well as to use the value: what the formula would have produced
// unbounded, what it produced, and the limit that bound it.
type Derived struct {
	// Value is the identifier to use. It equals Natural whenever Natural fits,
	// so a name that was already legal is never rewritten.
	Value string

	// Natural is the formula's output with nothing cut. It is what a report
	// names when it explains why a value did not fit.
	Natural string

	// Digest is the eight hex characters taken over the whole input, present
	// whether or not the value was truncated. Two formulas that produce one
	// Natural from different parts produce different digests.
	Digest string

	// Limit is the byte budget Value was held to.
	Limit int

	// Truncated records that Natural did not fit and Value carries the digest.
	Truncated bool

	// Kind is the formula's kind, so a caller can revalidate without it.
	Kind Kind
}

// Fits reports whether the formula's natural output was already within its
// limit, which is the question a preflight check asks. A false Fits with a
// legal [Derived.Value] is a name the migration would have to rewrite.
func (d Derived) Fits() bool { return !d.Truncated }

// Errors reports what the API server would refuse about [Derived.Value], and is
// empty for every value this package produces. It exists so a caller that builds
// an identifier some other way can be checked against the same helpers.
func (d Derived) Errors() []string { return d.Kind.validate(d.Value) }

// Validate reports what the API server would refuse about a value used as this
// kind, using the API server's own helpers rather than a second opinion about
// them. It is exported for the caller that has to check a value a formula did
// not produce, which is every value this product already wrote before the
// formulas were written down.
func Validate(kind Kind, value string) []string { return kind.validate(value) }

// Derive builds the identifier for these parts.
//
// It is deterministic in its inputs, so two processes derive the same value
// without coordinating, and the digest covers the parts individually rather
// than the string they join into, so neither truncation nor a separator that is
// legal inside a part can bring two distinct inputs to one value.
func (f Formula) Derive(parts ...string) Derived {
	separator := f.Separator
	if separator == "" {
		separator = "-"
	}
	limit := f.Limit
	if limit <= 0 {
		limit = f.Kind.limit()
	}

	sanitized := make([]string, 0, len(parts))
	for _, part := range parts {
		sanitized = append(sanitized, f.Kind.sanitize(part))
	}
	stem := strings.Join(sanitized, separator)

	derived := Derived{
		Natural: f.Prefix + stem + f.Suffix,
		Digest:  digestOf(parts),
		Limit:   limit,
		Kind:    f.Kind,
	}
	if len(derived.Natural) <= limit {
		derived.Value = derived.Natural
		return derived
	}

	// The digest and the dash before it are spent first, then whatever the
	// prefix and suffix need, and the stem takes what is left. A formula whose
	// fixed text alone exceeds the limit is a bug in the formula rather than in
	// its input, and it yields an empty stem rather than a panic.
	room := limit - len(f.Prefix) - len(f.Suffix) - digestLength - 1
	if room < 0 {
		room = 0
	}
	if len(stem) > room {
		stem = stem[:room]
	}
	stem = strings.TrimRight(stem, "._-")

	derived.Truncated = true
	derived.Value = f.Prefix + stem + "-" + derived.Digest + f.Suffix
	return derived
}

// digestOf hashes the parts as they were handed in, before sanitization and
// before truncation, separating them with a NUL that no part can contain.
func digestOf(parts []string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:digestLength]
}
