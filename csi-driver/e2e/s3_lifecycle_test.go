// Unit tests for the S3 request helpers the S3 specs share, which run without
// a cluster.

package e2e

import "testing"

func TestCopySourceEscapesWhatAServerWouldDecode(t *testing.T) {
	// The randomized spec puts a space, a plus, and a non-ASCII letter in its
	// keys. Unescaped, a server decodes the plus as a space and reads the
	// source as a key that does not exist.
	got := copySource("bucket", "dir +x/ü.f~_-")
	want := "bucket/dir%20%2Bx/%C3%BC.f~_-"
	if got != want {
		t.Fatalf("copySource = %q, want %q", got, want)
	}
}
