// pvcNameFor against the two edges design §4.1 names: the DNS object-name
// limit, and the gap between the S3 bucket grammar and the DNS-1123
// subdomain grammar a PVC name must satisfy.

package objectstore

import (
	"strings"
	"testing"

	atlaskube "github.com/simplyblock/atlas/kube"
)

// U-17: a bucket name at the S3 grammar's own 63-character ceiling (its
// longest legal form) still produces a valid, collision-free object name,
// once the "s3bucket-" prefix is added on top of it.
func TestPVCNameForBucketAtTheLengthLimit(t *testing.T) {
	bucketName := "a" + strings.Repeat("b", 61) + "c" // 63 chars, the S3 grammar's own ceiling
	name := pvcNameFor(bucketName)

	if errs := atlaskube.Validate(atlaskube.ObjectName, name); len(errs) != 0 {
		t.Errorf("pvcNameFor(%q) = %q, not a valid object name: %v", bucketName, name, errs)
	}
	if !strings.HasPrefix(name, "s3bucket-"+bucketName) {
		t.Errorf("pvcNameFor(%q) = %q, want the name to survive unmangled: the input fits the object-name limit on its own", bucketName, name)
	}
}

// U-18: two bucket names that differ only in a run of dots the S3 grammar
// allows between labels but a DNS-1123 subdomain does not (an empty label)
// still produce distinct, individually valid PVC names.
func TestPVCNameForDistinguishesNamesDNSLabelsCannot(t *testing.T) {
	a := "team-a..reports" // the empty label between the dots is S3-legal, DNS-illegal
	b := "team-a.-reports" // a label starting with '-' after the dot, same story

	nameA, nameB := pvcNameFor(a), pvcNameFor(b)

	if nameA == nameB {
		t.Fatalf("pvcNameFor(%q) and pvcNameFor(%q) both produced %q", a, b, nameA)
	}
	for _, name := range []string{nameA, nameB} {
		if errs := atlaskube.Validate(atlaskube.ObjectName, name); len(errs) != 0 {
			t.Errorf("%q is not a valid object name: %v", name, errs)
		}
	}
}
