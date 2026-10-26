// Reading the two bundle fixtures under testdata, one per supported report
// version, and the archives that are not bundles.

package supportbundle

import (
	"bytes"
	"strings"
	"testing"

	"github.com/simplyblock/simplyblock-operator/support-tool/internal/bundletest"
)

func read(t *testing.T, data []byte) *Bundle {
	t.Helper()
	b, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

func TestReadAVersion10Bundle(t *testing.T) {
	b := read(t, bundletest.Archive(t, "../../testdata/bundle-v10", nil))

	if b.Name != "simplyblock-bundle-discover-fleet-02-20261024T090000Z" {
		t.Errorf("the bundle is named %q", b.Name)
	}
	if b.Run == nil || b.Run.Name != "discover-fleet-02" {
		t.Fatalf("the run is %+v", b.Run)
	}
	if len(b.Nodes) != 3 {
		t.Errorf("read %d nodes, want 3", len(b.Nodes))
	}
	if b.CDC == nil || b.CDC.Name != "discovered-discover-fleet-02" {
		t.Errorf("the CDC is %+v", b.CDC)
	}
	if len(b.Reports) != 3 {
		t.Fatalf("read %d reports, want 3", len(b.Reports))
	}
	for _, r := range b.Reports {
		if r.Err != "" || r.Version != 10 || !r.KernelReported() {
			t.Errorf("report %s: version %d, error %q", r.File, r.Version, r.Err)
		}
		if r.Report.HostOS.Kernel.Release == "" {
			t.Errorf("report %s carries no kernel release", r.File)
		}
	}
	if len(b.Problems) != 0 {
		t.Errorf("problems reading a complete bundle: %v", b.Problems)
	}
}

// Version 11 adds the network attachments. The earlier versions stay readable,
// because a support agent reads bundles from clusters on older releases.
func TestReadAVersion11Bundle(t *testing.T) {
	b := read(t, bundletest.Archive(t, "../../testdata/bundle-v11", nil))

	if len(b.Reports) != 3 {
		t.Fatalf("read %d reports, want 3", len(b.Reports))
	}
	for _, r := range b.Reports {
		if r.Err != "" || r.Version != 11 {
			t.Errorf("report %s: version %d, error %q", r.File, r.Version, r.Err)
		}
		if !r.KernelReported() || !r.NetworkAttachmentsReported() {
			t.Errorf("report %s of version 11 claims no kernel facts or no network attachments", r.File)
		}
		attachments := r.Report.NetworkAttachments
		if attachments == nil || attachments.State != "Served" || len(attachments.Definitions) != 2 {
			t.Errorf("report %s carries the network attachments %+v", r.File, attachments)
		}
	}
}

// Before version 11 an absent attachment reading is no reading at all, not a
// cluster without Multus.
func TestAVersion10ReportCarriesNoNetworkAttachmentReading(t *testing.T) {
	b := read(t, bundletest.Archive(t, "../../testdata/bundle-v10", nil))
	for _, r := range b.Reports {
		if r.NetworkAttachmentsReported() {
			t.Errorf("report %s of version 10 claims network attachments", r.File)
		}
	}
}

func TestReadAVersion9Bundle(t *testing.T) {
	b := read(t, bundletest.Archive(t, "../../testdata/bundle-v9", nil))

	if len(b.Reports) != 3 {
		t.Fatalf("read %d reports, want 3", len(b.Reports))
	}
	for _, r := range b.Reports {
		if r.Err != "" || r.Version != 9 {
			t.Errorf("report %s: version %d, error %q", r.File, r.Version, r.Err)
		}
		if r.KernelReported() {
			t.Errorf("report %s of version 9 claims kernel facts", r.File)
		}
		if r.Report.Node == "" || len(r.Report.Devices) == 0 {
			t.Errorf("report %s decoded without its node or devices", r.File)
		}
	}
}

func TestAReportOfAnotherVersionIsUnreadable(t *testing.T) {
	b := read(t, bundletest.Archive(t, "../../testdata/bundle-v10", func(name string, data []byte) ([]byte, bool) {
		if strings.HasSuffix(name, "worker-02.json") {
			return bytes.Replace(data, []byte(`"version": 10`), []byte(`"version": 8`), 1), true
		}
		return data, true
	}))

	var unread int
	for _, r := range b.Reports {
		if r.Err != "" {
			unread++
			if !strings.Contains(r.Err, "version 8") {
				t.Errorf("the error does not name the version: %q", r.Err)
			}
		}
	}
	if unread != 1 {
		t.Errorf("%d reports unreadable, want the one of version 8", unread)
	}
}

func TestABundleWithoutACDCHasNone(t *testing.T) {
	b := read(t, bundletest.Archive(t, "../../testdata/bundle-v10", func(name string, data []byte) ([]byte, bool) {
		return data, name != "cdc.yaml"
	}))
	if b.CDC != nil {
		t.Errorf("a CDC was read from a bundle without one: %s", b.CDC.Name)
	}
	if len(b.Problems) != 0 {
		t.Errorf("a missing CDC is a problem: %v", b.Problems)
	}
}

func TestAFileThatDoesNotParseIsAProblem(t *testing.T) {
	b := read(t, bundletest.Archive(t, "../../testdata/bundle-v10", func(name string, data []byte) ([]byte, bool) {
		if name == "nodes.yaml" {
			return []byte("items: [this is not: a node"), true
		}
		return data, true
	}))
	if len(b.Problems) == 0 || !strings.Contains(strings.Join(b.Problems, " "), "nodes.yaml") {
		t.Errorf("an unreadable nodes.yaml is not named among the problems: %v", b.Problems)
	}
	if b.Run == nil {
		t.Error("one unreadable file stopped the rest from being read")
	}
}

func TestMacOSMetadataEntriesAreNotReports(t *testing.T) {
	// tar on macOS adds an AppleDouble ._<name> entry beside every file that
	// carries extended attributes, which every file a browser downloaded does.
	b := read(t, bundletest.Archive(t, "../../testdata/bundle-v10", func(name string, data []byte) ([]byte, bool) {
		return data, true
	}, "reports/._nodeprobe-discover-fleet-02-worker-01.json", "._cdc.yaml"))

	if len(b.Reports) != 3 {
		t.Errorf("read %d reports, want the 3 real ones", len(b.Reports))
	}
	if len(b.Problems) != 0 {
		t.Errorf("metadata entries were read as files: %v", b.Problems)
	}
}

func TestAnArchiveThatIsNotABundleIsRefused(t *testing.T) {
	if _, err := Read(strings.NewReader("this is not gzip")); err == nil {
		t.Error("random bytes read as a bundle")
	}
	empty := bundletest.Archive(t, t.TempDir(), nil)
	if _, err := Read(bytes.NewReader(empty)); err == nil {
		t.Error("an archive without a run read as a bundle")
	}
}

func TestAnEntryLargerThanTheLimitIsRefused(t *testing.T) {
	saved := MaxEntryBytes
	MaxEntryBytes = 1024
	defer func() { MaxEntryBytes = saved }()

	if _, err := Read(bytes.NewReader(bundletest.Archive(t, "../../testdata/bundle-v10", nil))); err == nil {
		t.Error("an entry over the limit was read")
	}
}

func TestAnArchiveThatExpandsPastTheTotalIsRefused(t *testing.T) {
	saved := MaxTotalBytes
	MaxTotalBytes = 8 << 10 // about half the fixture
	defer func() { MaxTotalBytes = saved }()

	// Every entry is under MaxEntryBytes; together they are over the total, the
	// shape of a gzip of many highly compressible files.
	if _, err := Read(bytes.NewReader(bundletest.Archive(t, "../../testdata/bundle-v10", nil))); err == nil {
		t.Error("an archive expanding past the total was read")
	}
}

func TestAnArchiveOfTooManyEntriesIsRefused(t *testing.T) {
	saved := MaxEntries
	MaxEntries = 5
	defer func() { MaxEntries = saved }()

	if _, err := Read(bytes.NewReader(bundletest.Archive(t, "../../testdata/bundle-v10", nil))); err == nil {
		t.Error("an archive of more entries than the limit was read")
	}
}
