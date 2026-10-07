// The local image factory, checked without a cluster: what talosctl's
// disk-image preset asks it for, and that a rebuilt image is a new schematic.

package cluster

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeDiskImage(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "metal-arm64.raw.zst")
	if err := os.WriteFile(img, []byte("first image"), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := serveDiskImage(img)
	if err != nil {
		t.Fatalf("serveDiskImage: %v", err)
	}
	defer f.Close()

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(f.URL + path) //nolint:noctx // loopback test server
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	t.Run("the preset's path gets the image", func(t *testing.T) {
		code, body := get("/image/" + f.Schematic + "/v1.14.2/metal-arm64.raw.zst")
		if code != http.StatusOK || body != "first image" {
			t.Fatalf("GET = %d %q, want 200 with the image", code, body)
		}
	})

	t.Run("another schematic or file is not found", func(t *testing.T) {
		for _, p := range []string{
			"/image/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba/v1.14.2/metal-arm64.raw.zst",
			"/image/" + f.Schematic + "/v1.14.2/metal-arm64.iso",
		} {
			if code, _ := get(p); code != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404", p, code)
			}
		}
	})

	t.Run("a rebuilt image is a new schematic, so talosctl's cache cannot serve the old one", func(t *testing.T) {
		if err := os.WriteFile(img, []byte("second image"), 0o600); err != nil {
			t.Fatal(err)
		}
		g, err := serveDiskImage(img)
		if err != nil {
			t.Fatalf("serveDiskImage: %v", err)
		}
		defer g.Close()
		if g.Schematic == f.Schematic {
			t.Fatalf("both images are schematic %s", g.Schematic)
		}
	})

	t.Run("an uncompressed image is refused by name", func(t *testing.T) {
		_, err := serveDiskImage(filepath.Join(dir, "metal-arm64.raw"))
		if err == nil || !strings.Contains(err.Error(), "raw.zst") {
			t.Fatalf("serveDiskImage(raw) = %v, want a refusal naming raw.zst", err)
		}
	})
}
