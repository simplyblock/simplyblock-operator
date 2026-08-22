package utils

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Kubernetes object names derived from things that are not name-safe. Centralized
// here because both the volume-migration path Jobs and the storage-node latency
// baseline Jobs name themselves after a node, and a Job whose name is not a valid
// DNS-1123 label is a Job that is never created.

// nonLabelChars matches everything not allowed inside a DNS-1123 label.
var nonLabelChars = regexp.MustCompile(`[^a-z0-9-]`)

// SafeNodeID produces a DNS-label-safe suffix from a node UUID.
func SafeNodeID(nodeUUID string) string {
	s := strings.ReplaceAll(nodeUUID, "-", "")
	if len(s) > 20 {
		s = s[:20]
	}
	return s
}

// NodeNameSuffix produces a DNS-label-safe, collision-resistant suffix for a node
// name. Node names can be long FQDNs and are not label-safe, so the short host part
// is kept for readability and a hash of the full name for uniqueness.
func NodeNameSuffix(nodeName string) string {
	sum := sha256.Sum256([]byte(nodeName))
	short := strings.ToLower(strings.SplitN(nodeName, ".", 2)[0])
	short = nonLabelChars.ReplaceAllString(short, "")
	if len(short) > 16 {
		short = short[:16]
	}
	if short == "" {
		return hex.EncodeToString(sum[:6])
	}
	return short + "-" + hex.EncodeToString(sum[:4])
}
