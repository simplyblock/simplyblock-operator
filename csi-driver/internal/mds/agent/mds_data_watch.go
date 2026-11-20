// A watch over the data pNFS clients send through the guest's nfsd instead of
// their layouts' devices, read from /proc and written to the agent's log,
// which reaches the pod log the way the nfsd watchdog's does.
//
// A client that cannot use its layout's device keeps working: its READs and
// WRITEs go through this server, and nothing fails. In SFAM-2850 one client
// did that for the rest of a run, and the server said nothing about which
// client it was. The guest kernel counts, per client, the bytes it carries
// outside the grace period and the layout hold (kernel patch 0027, in
// /proc/fs/nfsd/clients/<id>/info), and this watch names a client whose count
// grows. It only reads /proc.

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultMDSDataWatchInterval is the time between two looks at the
	// clients' counts.
	DefaultMDSDataWatchInterval = time.Minute

	// mdsDataThreshold is how many bytes a client may send through the server
	// between two looks before it is named. A node agent's layout probe is a
	// block, and some data goes through the server by design, unaligned
	// direct I/O among it.
	mdsDataThreshold = 1 << 20
)

// MDSDataWatch names the pNFS clients whose data goes through this server.
// RunMDSDataWatch ticks it from one goroutine.
type MDSDataWatch struct {
	// ProcRoot is /proc, or a test's tree.
	ProcRoot string
	Log      Logger

	looked bool
	last   map[string]clientMDSData
}

// clientMDSData is one client's counts as its info file showed them.
type clientMDSData struct {
	Address, Name         string
	ReadBytes, WriteBytes int64
}

// readClientMDSData reads every client's counts under <root>/fs/nfsd/clients,
// keyed by the client's directory. A client whose info carries no counts, on
// a kernel without them, is left out, and so is one that goes away while it is
// read.
func readClientMDSData(root string) map[string]clientMDSData {
	dirs, err := os.ReadDir(filepath.Join(root, "fs", "nfsd", "clients"))
	if err != nil {
		return nil
	}
	out := map[string]clientMDSData{}
	for _, dir := range dirs {
		data, err := os.ReadFile(filepath.Join(root, "fs", "nfsd", "clients", dir.Name(), "info"))
		if err != nil {
			continue
		}
		var c clientMDSData
		var haveRead, haveWrite bool
		for _, line := range strings.Split(string(data), "\n") {
			key, value, ok := strings.Cut(line, ": ")
			if !ok {
				continue
			}
			switch key {
			case "address":
				c.Address = unquote(value)
			case "name":
				c.Name = unquote(value)
			case "pnfs mds read bytes":
				c.ReadBytes, err = strconv.ParseInt(value, 10, 64)
				haveRead = err == nil
			case "pnfs mds write bytes":
				c.WriteBytes, err = strconv.ParseInt(value, 10, 64)
				haveWrite = err == nil
			}
		}
		if haveRead && haveWrite {
			out[dir.Name()] = c
		}
	}
	return out
}

// unquote strips the quotes the kernel puts around an address or a name, and
// leaves a value without them as it is.
func unquote(value string) string {
	if s, err := strconv.Unquote(value); err == nil {
		return s
	}
	return value
}

// Tick looks at every client's count once, and names each client that sent
// mdsDataThreshold bytes or more through the server since the last look. The
// first look names nobody: what the server carried before it may lie long in
// the past. A client first seen after it is counted from zero.
func (w *MDSDataWatch) Tick() {
	root := w.ProcRoot
	if root == "" {
		root = "/proc"
	}
	now := readClientMDSData(root)
	if w.looked {
		for id, c := range now {
			prev := w.last[id]
			read, written := c.ReadBytes-prev.ReadBytes, c.WriteBytes-prev.WriteBytes
			if read < 0 || written < 0 || read+written < mdsDataThreshold {
				continue
			}
			w.Log.Warningf("pnfs: client %s at %s sent %d bytes of READs and %d bytes of WRITEs "+
				"through the metadata server since the last look, instead of its layout's device",
				c.Name, c.Address, read, written)
		}
	}
	w.looked = true
	w.last = now
}

// RunMDSDataWatch ticks w every interval until ctx ends. A zero interval
// turns the watch off.
func RunMDSDataWatch(ctx context.Context, w *MDSDataWatch, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.Tick()
		}
	}
}
