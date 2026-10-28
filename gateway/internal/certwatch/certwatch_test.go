// Checks that Changes signals when a watched file's contents change and stays
// quiet when they do not, using real temporary files on a short interval.

package certwatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	testInterval = 10 * time.Millisecond
	testWait     = time.Second
	testQuiet    = 100 * time.Millisecond
)

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func TestChangesSignalsWhenAWatchedFileIsRewritten(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeFile(t, cert, "cert-1")
	writeFile(t, key, "key-1")

	changes := Changes(t.Context(), testInterval, cert, key)
	writeFile(t, key, "key-2")

	select {
	case <-changes:
	case <-time.After(testWait):
		t.Fatal("no signal after the key was rewritten")
	}
}

func TestChangesStaysQuietWhileTheFilesAreUnchanged(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "tls.crt")
	writeFile(t, cert, "cert-1")

	changes := Changes(t.Context(), testInterval, cert)
	writeFile(t, cert, "cert-1")

	select {
	case <-changes:
		t.Fatal("signal although the contents did not change")
	case <-time.After(testQuiet):
	}
}

func TestChangesSignalsWhenAMissingFileAppears(t *testing.T) {
	cert := filepath.Join(t.TempDir(), "tls.crt")

	changes := Changes(t.Context(), testInterval, cert)
	writeFile(t, cert, "cert-1")

	select {
	case <-changes:
	case <-time.After(testWait):
		t.Fatal("no signal after the missing file appeared")
	}
}

func TestChangesStopsWhenTheContextIsDone(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "tls.crt")
	writeFile(t, cert, "cert-1")

	ctx, cancel := context.WithCancel(t.Context())
	changes := Changes(ctx, testInterval, cert)
	cancel()
	time.Sleep(testQuiet)
	writeFile(t, cert, "cert-2")

	select {
	case <-changes:
		t.Fatal("signal after the context was done")
	case <-time.After(testQuiet):
	}
}
