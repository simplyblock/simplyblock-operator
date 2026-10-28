// Package certwatch tells the gateway when its serving certificate was
// rotated on disk. cert-manager and OpenShift's service CA both rewrite the
// mounted TLS Secret in place, and embedgw loads the certificate once at
// startup, re-reading it only when its SigHup channel fires. Changes is what
// feeds that channel, so a renewed certificate is served without a restart.
//
// Contents are compared, not modification times: a Secret volume swaps a
// symlink to a new directory, and a byte comparison of two small files is
// exact where an mtime is only as precise as the filesystem keeps it.

package certwatch

import (
	"bytes"
	"context"
	"os"
	"time"
)

// Changes reads files every interval until ctx is done and sends on the
// returned channel when any of them differs from the previous read. A file
// that cannot be read counts as empty, so one that appears later signals.
// The channel holds one pending signal, so changes the receiver has not
// picked up yet collapse into one reload rather than blocking the watch.
func Changes(ctx context.Context, interval time.Duration, files ...string) <-chan struct{} {
	changes := make(chan struct{}, 1)
	last := readAll(files)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			current := readAll(files)
			if equal(last, current) {
				continue
			}
			last = current
			select {
			case changes <- struct{}{}:
			default:
			}
		}
	}()
	return changes
}

func readAll(files []string) [][]byte {
	contents := make([][]byte, len(files))
	for i, file := range files {
		contents[i], _ = os.ReadFile(file)
	}
	return contents
}

func equal(a, b [][]byte) bool {
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
