// Loading the captured device images the content tests read.
//
// The images are the head and tail regions of devices that real tools formatted,
// captured by hack/blockdev/capture-image.sh. They are evidence rather than
// construction: a fixture built from the offsets the decoder uses would assert
// only that the decoder agrees with itself, and would pass while both it and the
// catalog were wrong about what mkfs writes.

package blockdev

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// imageDir is where capture-image.sh writes and where the tests read.
const imageDir = "testdata/images"

// manifest is the provenance a capture records beside its regions.
//
// The tool version is load-bearing rather than bookkeeping: the ext feature
// words decide which family a reading names, and they differ across e2fsprogs
// generations, so a fixture that does not say which tool wrote it cannot answer
// why it decodes the way it does.
type manifest struct {
	Name             string `json:"name"`
	Tool             string `json:"tool"`
	ToolVersion      string `json:"tool_version"`
	Command          string `json:"command"`
	DeviceSize       int64  `json:"device_size"`
	LogicalBlockSize uint32 `json:"logical_block_size"`
	RegionSize       int64  `json:"region_size"`
	Captured         string `json:"captured"`
	CapturedOn       string `json:"captured_on"`
	Blkid            string `json:"blkid"`
	HeadSHA256       string `json:"head_sha256"`
	TailSHA256       string `json:"tail_sha256"`
	Note             string `json:"note"`
}

// image is one captured device: its two regions and the provenance of both.
type image struct {
	manifest
	head []byte
	tail []byte
}

// Device rebuilds the Device value the prober is handed for this image, so a
// test reads the capture through exactly the fields a live device would supply.
func (im image) Device() Device {
	return Device{
		Path:              "/dev/fixture/" + im.Name,
		Name:              im.Name,
		LogicalBlockSize:  im.LogicalBlockSize,
		PhysicalBlockSize: im.LogicalBlockSize,
		SizeBytes:         uint64(im.DeviceSize),
	}
}

// Reader serves the image's regions the way a device serves them, and fails any
// read that falls between them: the regions are what was captured, and a prober
// reaching outside them is a prober reading something the fixture cannot vouch
// for.
func (im image) Reader() Reader { return &imageReader{im: im} }

type imageReader struct{ im image }

func (r *imageReader) Close() error { return nil }

func (r *imageReader) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	size := r.im.DeviceSize
	region := r.im.RegionSize
	switch {
	case off >= 0 && off+int64(len(p)) <= region:
		return copy(p, r.im.head[off:]), nil
	case off >= size-region && off+int64(len(p)) <= size:
		return copy(p, r.im.tail[off-(size-region):]), nil
	default:
		return 0, errors.New("fixture: read outside the captured regions")
	}
}

// loadImage reads one capture and verifies it against its own manifest, so a
// fixture that was edited by hand fails the test that reads it rather than
// quietly changing what the test asserts.
func loadImage(t *testing.T, name string) image {
	t.Helper()
	dir := filepath.Join(imageDir, name)

	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest for %s: %v", name, err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse manifest for %s: %v", name, err)
	}

	im := image{manifest: m}
	im.head = readRegion(t, filepath.Join(dir, "head.bin.gz"), m.HeadSHA256, name+" head")
	im.tail = readRegion(t, filepath.Join(dir, "tail.bin.gz"), m.TailSHA256, name+" tail")
	return im
}

func readRegion(t *testing.T, path, wantSHA, what string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", what, err)
	}
	defer func() { _ = f.Close() }()

	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("decompress %s: %v", what, err)
	}
	defer func() { _ = zr.Close() }()

	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read %s: %v", what, err)
	}
	if got := hex.EncodeToString(sum(data)); got != wantSHA {
		t.Fatalf("%s does not match the checksum its manifest records:\n got %s\nwant %s\n"+
			"the fixture was edited, or the manifest was: recapture it rather than adjusting either",
			what, got, wantSHA)
	}
	return data
}

func sum(b []byte) []byte { h := sha256.Sum256(b); return h[:] }

// imageNames lists every capture on disk, so a fixture that is added without a
// scenario row still gets its provenance checked.
func imageNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(imageDir)
	if err != nil {
		t.Fatalf("read %s: %v", imageDir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatalf("no captured images under %s; run hack/blockdev/capture-image.sh", imageDir)
	}
	return names
}

// U-62: every fixture's bytes match the checksum its manifest records, and every
// fixture says which tool wrote it.
func TestFixturesMatchTheirManifests(t *testing.T) {
	for _, name := range imageNames(t) {
		t.Run(name, func(t *testing.T) {
			im := loadImage(t, name) // fails on a checksum mismatch

			if int64(len(im.head)) != im.RegionSize || int64(len(im.tail)) != im.RegionSize {
				t.Errorf("regions are %d and %d bytes, want %d each",
					len(im.head), len(im.tail), im.RegionSize)
			}
			if im.DeviceSize < 2*im.RegionSize {
				t.Errorf("device size %d is smaller than two regions, so the capture overlaps itself",
					im.DeviceSize)
			}
			if im.LogicalBlockSize == 0 {
				t.Error("no logical block size recorded, so block-relative offsets cannot be resolved")
			}
			if im.Name != name {
				t.Errorf("manifest names %q but the directory is %q", im.Name, name)
			}
			if im.Tool == "" || im.ToolVersion == "" {
				t.Errorf("provenance is incomplete: tool %q version %q", im.Tool, im.ToolVersion)
			}
			if im.CapturedOn == "" {
				t.Error("no capture host recorded")
			}
		})
	}
}

// The fixture tree is committed, so a missing directory is a broken checkout
// rather than a skipped test.
func TestFixtureTreeIsPresent(t *testing.T) {
	if _, err := os.Stat(imageDir); errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s is missing; the captured images are committed and are not regenerated by the test", imageDir)
	}
}
