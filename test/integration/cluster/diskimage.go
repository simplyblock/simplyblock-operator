// Serving a locally built disk image to talosctl as if an Image Factory held it.
//
// talosctl boots a disk image two ways: the `qemu` subcommand's disk-image preset,
// which downloads from an Image Factory, and the dev subcommand, which takes a
// path. Only the first works on a macOS host. dev hands the node a config URL
// built from the address its config server listens on, [::], which the node
// cannot reach, and its alternative injection method is refused on macOS.
//
// So a local image goes through the preset too, from a factory that is this
// file: an HTTP server on the loopback answering the one path the preset asks
// for. Everything else about the boot is what every other run does.
package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
)

// localFactory is a running stand-in for the Image Factory.
type localFactory struct {
	// URL is what talosctl takes as --image-factory-url.
	URL string

	// Schematic is what it takes as --schematic-id, and is derived from the
	// image's content. talosctl caches a download under its URL and never
	// fetches that URL again, so a schematic that stayed the same across
	// rebuilds would boot the first image built forever.
	Schematic string

	server *http.Server
}

// serveDiskImage starts a factory serving path. The preset asks for
// /image/<schematic>/<version>/metal-<arch>.raw.zst. Any request of that shape
// for this schematic gets the file, whatever version or architecture it
// names, because the image is the only one there is and talosctl, not this
// server, decides which version it wants.
func serveDiskImage(path string) (*localFactory, error) {
	if !strings.HasSuffix(path, ".raw.zst") {
		return nil, fmt.Errorf("disk image %s: want the imager's compressed metal image, "+
			"metal-<arch>.raw.zst; talosctl decompresses it itself and asks for it by that name", path)
	}
	sum, err := fileDigest(path)
	if err != nil {
		return nil, fmt.Errorf("disk image %s: %w", path, err)
	}
	schematic := "local-" + sum[:16]

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for the local image factory: %w", err)
	}

	prefix := "/image/" + schematic + "/"
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, ".raw.zst") {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, path)
	})
	f := &localFactory{
		URL:       "http://" + listener.Addr().String(),
		Schematic: schematic,
		server:    &http.Server{Handler: mux}, //nolint:gosec // loopback only, lives for one create
	}
	go func() { _ = f.server.Serve(listener) }()
	return f, nil
}

// Close stops the server.
func (f *localFactory) Close() {
	if err := f.server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "talos: stop the local image factory: %v\n", err)
	}
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // the caller's own image
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
