// Whitebox tests for the by-id device scan behind Connect and Disconnect.
//
// The scan runs against a fixture directory of real symlinks pointing at real
// files, so filepath.Glob and filepath.EvalSymlinks do the actual work: what is
// under test is glob-pattern semantics against udev's naming, and a faked
// matcher would only ever test the fake. The main fixture reproduces
// /dev/disk/by-id of a simplyblock CSI node hosting three multi-namespace
// subsystems next to the node's local NVMe, LVM, and SCSI devices.
package util

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// realSubsystem is one NVMe-oF subsystem as a simplyblock node sees it. The
// model is the master lvol UUID taken from the subsystem NQN
// (nqn.2023-02.io.simplyblock:<cluster>:lvol:<model>), every namespace of the
// subsystem is a block device on the subsystem's controller, and udev names its
// links nvme-<model>_<infix>_<nsid>.
type realSubsystem struct {
	model string
	// infix is what udev puts between the model and the nsid: the volume's HA
	// type, "ha" for a replicated volume and "single" for a non-replicated one.
	// The scan has to find the namespace either way.
	infix string
	// controller is the block-device prefix: namespace N is <controller>n<N>.
	controller string
	// suffixlessTarget is the device the plain nvme-<model>_<infix> link points
	// at. udev repoints it whenever the subsystem changes, so it names an
	// arbitrary namespace — nvme2n2 out of five namespaces on the node below —
	// and must never be mistaken for the namespace the caller asked for.
	suffixlessTarget string
	// lvolIDs maps a namespace ID to that namespace's own lvol UUID, which udev
	// publishes as nvme-uuid.<lvolID> without an nsid suffix.
	lvolIDs map[int]string
}

func (s realSubsystem) namespaceIDs() []int {
	ids := make([]int, 0, len(s.lvolIDs))
	for nsID := range s.lvolIDs {
		ids = append(ids, nsID)
	}
	sort.Ints(ids)
	return ids
}

// namespaceLink is the udev by-id name of one namespace of the subsystem.
func (s realSubsystem) namespaceLink(nsID int) string {
	return fmt.Sprintf("nvme-%s_%s_%d", s.model, s.infix, nsID)
}

// suffixlessLink is the udev by-id name of the subsystem-wide alias.
func (s realSubsystem) suffixlessLink() string {
	return fmt.Sprintf("nvme-%s_%s", s.model, s.infix)
}

// blockDevice is the block device backing namespace nsID.
func (s realSubsystem) blockDevice(nsID int) string {
	return fmt.Sprintf("%sn%d", s.controller, nsID)
}

// realNodeSubsystems are the three simplyblock subsystems of the captured node,
// with the namespace counts that make the nsid matching interesting: 19
// namespaces mean the glob for namespace 1 has to reject namespaces 10 to 19.
var realNodeSubsystems = []realSubsystem{
	{
		model:            "beb0a31a-c35c-426f-8b3a-c839beab899b",
		infix:            "ha",
		controller:       "nvme0",
		suffixlessTarget: "nvme0n8",
		lvolIDs: map[int]string{
			1: "beb0a31a-c35c-426f-8b3a-c839beab899b", // master lvol: same UUID as the model
			2: "e0e54fcb-eb64-40be-a805-bb366ad2363c",
			3: "92b8b24d-16a4-4a33-81b6-81379afc66f5",
			4: "ca2101ba-ae38-4b70-9331-7a8548aec9cd",
			5: "22bb93f8-83ce-48f7-9865-992ccade796d",
			6: "b92a2be2-94c4-4a00-a0c2-fc65991e9e54",
			7: "ec7879b4-41ac-4252-8ad0-7de91d11479c",
			8: "3e9cb2e7-8e07-49bc-9cd1-af66d45be6ab",
		},
	},
	{
		model:            "747536ce-1ae0-4664-a28b-9fb1b3e91bfc",
		infix:            "ha",
		controller:       "nvme1",
		suffixlessTarget: "nvme1n19",
		lvolIDs: map[int]string{
			1:  "747536ce-1ae0-4664-a28b-9fb1b3e91bfc", // master lvol
			2:  "41470a26-95ab-47d0-9c53-1038b4cdeb86",
			3:  "df167778-50d2-4a22-9a1d-798ce9ff244b",
			4:  "819ccb8e-6329-4d95-8e79-ebee87d2597e",
			5:  "c9a486cb-ecea-4376-b00d-87789d24717f",
			6:  "c18546fe-07ea-48f4-8a53-66a50115dfdc",
			7:  "b88e38df-9bbf-42fe-af8b-25a5336b273c",
			8:  "3f39830f-5dca-4d4b-9e31-06e9f466fbd1",
			9:  "6d883e47-e4e2-463c-864d-8f5dbb4e9e5e",
			10: "b2943d9d-531f-44c6-9a8c-4d30220a886e",
			11: "95fb0ccd-0c73-4b91-bd7f-b4763f55eb4b",
			12: "2c826157-63e6-4173-a6bf-19d737e88eb9",
			13: "04e35496-0fce-4d87-a763-c78a714e2886",
			14: "f2c5450e-aaa2-4b96-8365-6b1a88cc8cfc",
			15: "30ea9cfa-a740-4162-b471-10bc07539d9f",
			16: "9207e60e-78b7-4b1c-8bd6-647c865987f3",
			17: "ba9c80c4-2916-4303-8476-2f9a45e094a8",
			18: "b23b22ed-78a9-4a22-b965-110f9a477348",
			19: "bf81a3d9-7fa1-49c5-a802-4c9486a25fce",
		},
	},
	{
		model:            "6078c089-d584-48a5-ad41-9bc4fdf3a74f",
		infix:            "ha",
		controller:       "nvme2",
		suffixlessTarget: "nvme2n2",
		lvolIDs: map[int]string{
			1: "6078c089-d584-48a5-ad41-9bc4fdf3a74f", // master lvol
			2: "0170bdf6-4796-4466-8110-aefb88eb2cf0",
			3: "9b7debcf-d285-41ef-8a1a-7850b06b5941",
			4: "9896459f-26ca-427e-ba21-b12bc38316cb",
			5: "f48eb4be-b572-44a2-a61d-73c9c618dda4",
		},
	},
}

// nonHASubsystem is a volume of a cluster with ha_type "single", where udev
// spells the infix "single" instead of "ha". It is not part of the captured
// node and shares the fixture directory with it so that both namings are
// covered by the same sweeps.
var nonHASubsystem = realSubsystem{
	model:            "3c2a1f00-1111-4222-8333-444455556666",
	infix:            "single",
	controller:       "nvme3",
	suffixlessTarget: "nvme3n2",
	lvolIDs: map[int]string{
		1: "3c2a1f00-1111-4222-8333-444455556666", // master lvol
		2: "7d5e4c3b-2222-4333-8444-555566667777",
	},
}

// fixtureSubsystems are all NVMe-oF subsystems the by-id fixture holds.
var fixtureSubsystems = append(append([]realSubsystem{}, realNodeSubsystems...), nonHASubsystem)

// realNodeForeignLinks are the entries of the same by-id directory that belong
// to the node itself: local PCIe NVMe (whose QEMU links carry an "_1" namespace
// suffix of their own), the root LVM volumes and the SCSI disk with partitions.
var realNodeForeignLinks = map[string]string{
	"dm-name-rl-root": "dm-0",
	"dm-name-rl-swap": "dm-1",
	"dm-uuid-LVM-zAiXUYddRrel1Wh58HtIDf6uXb3we0ob3jVSJvO4XDwe3zwR0LL2FZpnDVgdqgoi": "dm-1",
	"dm-uuid-LVM-zAiXUYddRrel1Wh58HtIDf6uXb3we0obOj6CbmGmKb2PGAmT02TjlyrY8LwHEe2t": "dm-0",
	"lvm-pv-uuid-ASkWdD-TKlM-Ddht-R3qn-452S-fwQX-q4SYuA":                           "sda2",
	"nvme-QEMU_NVMe_Ctrl_1121":                                      "nvme5n1",
	"nvme-QEMU_NVMe_Ctrl_1121_1":                                    "nvme5n1",
	"nvme-QEMU_NVMe_Ctrl_1122":                                      "nvme11n1",
	"nvme-QEMU_NVMe_Ctrl_1122_1":                                    "nvme11n1",
	"nvme-QEMU_NVMe_Ctrl_1123":                                      "nvme10n1",
	"nvme-QEMU_NVMe_Ctrl_1123_1":                                    "nvme10n1",
	"nvme-nvme.1b36-31313231-51454d55204e564d65204374726c-00000001": "nvme5n1",
	"nvme-nvme.1b36-31313232-51454d55204e564d65204374726c-00000001": "nvme11n1",
	"nvme-nvme.1b36-31313233-51454d55204e564d65204374726c-00000001": "nvme10n1",
	"scsi-0QEMU_QEMU_HARDDISK_drive-scsi0":                          "sda",
	"scsi-0QEMU_QEMU_HARDDISK_drive-scsi0-part1":                    "sda1",
	"scsi-0QEMU_QEMU_HARDDISK_drive-scsi0-part2":                    "sda2",
}

const (
	// testModel and testLvolID identify a synthetic single-namespace volume,
	// used where a whole node layout would only obscure the case.
	testModel  = "b1e2c3d4-0000-1111-2222-333344445555"
	testLvolID = "aa11bb22-cc33-dd44-ee55-ff6677889900"

	// testPoll keeps the scan loops fast; production polls once a second.
	testPoll = time.Millisecond
)

// testLink returns the udev name of namespace nsID of the synthetic subsystem.
func testLink(model string, nsID int) string {
	return fmt.Sprintf("nvme-%s_ha_%d", model, nsID)
}

// deviceFixture is a fake /dev/disk/by-id: block devices are plain files in a
// sibling directory, by-id entries are relative symlinks to them, exactly as
// udev writes them ("../../nvme0n1").
type deviceFixture struct {
	t       *testing.T
	byIDDir string
	devDir  string
}

func newDeviceFixture(t *testing.T) *deviceFixture {
	t.Helper()
	root := t.TempDir()
	// A glob metacharacter in the fixture path would change what the patterns
	// under test mean, so fail loudly rather than report a confusing mismatch.
	if strings.ContainsAny(root, "*?[]\\") {
		t.Fatalf("fixture path %q contains a glob metacharacter", root)
	}
	f := &deviceFixture{
		t:       t,
		byIDDir: filepath.Join(root, "by-id"),
		devDir:  filepath.Join(root, "dev"),
	}
	for _, dir := range []string{f.byIDDir, f.devDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}
	return f
}

// addDevice creates the block device blockDev if needed and links link to it.
// It returns the full path of the link, which is what the scan returns.
func (f *deviceFixture) addDevice(link, blockDev string) string {
	f.t.Helper()
	target := filepath.Join(f.devDir, blockDev)
	if _, err := os.Stat(target); os.IsNotExist(err) {
		if err := os.WriteFile(target, []byte(blockDev), 0o600); err != nil {
			f.t.Fatalf("creating block device %s: %v", target, err)
		}
	}
	return f.link(link, blockDev)
}

// addDangling creates a by-id link whose block device does not exist, the way a
// stale link outlives an unclean disconnect.
func (f *deviceFixture) addDangling(link, blockDev string) string {
	f.t.Helper()
	return f.link(link, blockDev)
}

func (f *deviceFixture) link(link, blockDev string) string {
	f.t.Helper()
	linkPath := filepath.Join(f.byIDDir, link)
	if err := os.Symlink(filepath.Join("..", "dev", blockDev), linkPath); err != nil {
		f.t.Fatalf("linking %s: %v", linkPath, err)
	}
	return linkPath
}

func (f *deviceFixture) remove(link string) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.byIDDir, link)); err != nil {
		f.t.Fatalf("removing %s: %v", link, err)
	}
}

// removeAfter removes a link once the scan has had a chance to observe it.
func (f *deviceFixture) removeAfter(d time.Duration, link string) {
	go func() {
		time.Sleep(d)
		_ = os.Remove(filepath.Join(f.byIDDir, link))
	}()
}

// addDeviceAfter creates a device once the scan is already polling for it.
func (f *deviceFixture) addDeviceAfter(d time.Duration, link, blockDev string) {
	go func() {
		time.Sleep(d)
		target := filepath.Join(f.devDir, blockDev)
		_ = os.WriteFile(target, []byte(blockDev), 0o600)
		_ = os.Symlink(filepath.Join("..", "dev", blockDev), filepath.Join(f.byIDDir, link))
	}()
}

// resolve returns the block device name a by-id link ends up at.
func (f *deviceFixture) resolve(linkPath string) string {
	f.t.Helper()
	resolved, err := filepath.EvalSymlinks(linkPath)
	if err != nil {
		f.t.Fatalf("resolving %s: %v", linkPath, err)
	}
	return filepath.Base(resolved)
}

// newRealNodeFixture reproduces the captured by-id directory: for every
// namespace of every subsystem the nsid link and the nvme-uuid.<lvolID> link,
// the subsystem-wide suffixless alias, and all of the node's own devices.
func newRealNodeFixture(t *testing.T) *deviceFixture {
	t.Helper()
	f := newDeviceFixture(t)
	for _, sub := range fixtureSubsystems {
		for _, nsID := range sub.namespaceIDs() {
			blockDev := sub.blockDevice(nsID)
			f.addDevice(sub.namespaceLink(nsID), blockDev)
			f.addDevice("nvme-uuid."+sub.lvolIDs[nsID], blockDev)
		}
		f.addDevice(sub.suffixlessLink(), sub.suffixlessTarget)
	}
	for link, blockDev := range realNodeForeignLinks {
		f.addDevice(link, blockDev)
	}
	return f
}

// brokenDisconnectGlob reproduces the pattern Disconnect built before the fix:
// the format arguments were swapped, so the model landed in the nsid position
// and the "%s" verb survived into the pattern itself.
func brokenDisconnectGlob(byIDDir, model string) string {
	return filepath.Join(byIDDir, fmt.Sprintf("*%s*_%s", "%s*_[0-9]*", model))
}

// --- the real node ---------------------------------------------------------

// TestMatchNamespaceDeviceOnRealNode sweeps every namespace of every subsystem
// of the captured node: each volume has to resolve to its own block device,
// through its own nsid link, with 30 other simplyblock links and the node's own
// devices in the same directory.
func TestMatchNamespaceDeviceOnRealNode(t *testing.T) {
	f := newRealNodeFixture(t)

	for _, sub := range fixtureSubsystems {
		for _, nsID := range sub.namespaceIDs() {
			t.Run(fmt.Sprintf("%s namespace %d", sub.controller, nsID), func(t *testing.T) {
				got, err := matchNamespaceDevice(
					context.Background(), f.byIDDir, sub.model, sub.lvolIDs[nsID], nsID, testPoll,
				)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if want := filepath.Join(f.byIDDir, sub.namespaceLink(nsID)); got != want {
					t.Fatalf("got link %q, want %q", got, want)
				}
				if resolved, want := f.resolve(got), sub.blockDevice(nsID); resolved != want {
					t.Errorf("link %q resolves to %q, want %q", got, resolved, want)
				}
			})
		}
	}
}

// TestMatchNamespaceDeviceRejectsNeighbouringNamespaces is the case the glob fix
// is about: on a 19-namespace subsystem the pattern for namespace 1 must not be
// satisfied by namespaces 10 to 19, and vice versa. Every namespace of every
// subsystem in the fixture is checked, with all of its siblings present.
func TestMatchNamespaceDeviceRejectsNeighbouringNamespaces(t *testing.T) {
	f := newRealNodeFixture(t)
	if got := len(realNodeSubsystems[1].lvolIDs); got != 19 {
		t.Fatalf("fixture changed: subsystem has %d namespaces, expected 19", got)
	}

	for _, sub := range fixtureSubsystems {
		for _, nsID := range sub.namespaceIDs() {
			t.Run(fmt.Sprintf("%s namespace %d", sub.controller, nsID), func(t *testing.T) {
				matches, err := filepath.Glob(namespaceDeviceGlob(f.byIDDir, sub.model, nsID))
				if err != nil {
					t.Fatalf("glob: %v", err)
				}
				want := []string{filepath.Join(f.byIDDir, sub.namespaceLink(nsID))}
				if len(matches) != 1 || matches[0] != want[0] {
					t.Errorf("glob for namespace %d matched %v, want exactly %v", nsID, matches, want)
				}
			})
		}
	}
}

// TestNamespaceGlobRejectsLongerNsid checks the prefix collision on its own, as
// a full cross product: the pattern for a namespace matches that namespace's
// link and no other, so 1 rejects 10 to 19, 10 rejects 100 to 199, and 9 rejects
// 19 and 99. It holds because filepath.Match anchors the whole name, which
// leaves the trailing "_<nsid>" of the pattern anchored at its end — the
// property a pattern that merely looks right would break silently, handing the
// caller another volume's block device.
func TestNamespaceGlobRejectsLongerNsid(t *testing.T) {
	const dir = "/dev/disk/by-id"
	sub := realNodeSubsystems[1]
	nsIDs := []int{1, 2, 9, 10, 11, 12, 19, 20, 21, 90, 99, 100, 101, 110, 199, 200, 1000}

	for _, globNsID := range nsIDs {
		glob := namespaceDeviceGlob(dir, sub.model, globNsID)
		for _, linkNsID := range nsIDs {
			link := filepath.Join(dir, sub.namespaceLink(linkNsID))
			got, err := filepath.Match(glob, link)
			if err != nil {
				t.Fatalf("Match(%q, %q): %v", glob, link, err)
			}
			if want := globNsID == linkNsID; got != want {
				t.Errorf("namespace %d glob against namespace %d link: got %v, want %v",
					globNsID, linkNsID, got, want)
			}
		}
	}
}

// TestNamespaceGlobRejectsLongerNsidOnDisk repeats the cross product against a
// directory that really holds all of those links, so the result covers Glob's
// own matching and not just filepath.Match.
func TestNamespaceGlobRejectsLongerNsidOnDisk(t *testing.T) {
	f := newDeviceFixture(t)
	nsIDs := []int{1, 2, 9, 10, 11, 12, 19, 20, 21, 90, 99, 100, 101, 110, 199, 200, 1000}
	sub := realSubsystem{model: testModel, infix: "ha", controller: "nvme0"}

	for _, nsID := range nsIDs {
		f.addDevice(sub.namespaceLink(nsID), sub.blockDevice(nsID))
	}

	for _, nsID := range nsIDs {
		matches, err := filepath.Glob(namespaceDeviceGlob(f.byIDDir, sub.model, nsID))
		if err != nil {
			t.Fatalf("glob: %v", err)
		}
		want := filepath.Join(f.byIDDir, sub.namespaceLink(nsID))
		if len(matches) != 1 || matches[0] != want {
			t.Fatalf("glob for namespace %d matched %v, want exactly [%s]", nsID, matches, want)
		}
		if resolved := f.resolve(matches[0]); resolved != sub.blockDevice(nsID) {
			t.Errorf("namespace %d resolved to %q, want %q", nsID, resolved, sub.blockDevice(nsID))
		}
	}
}

// TestMatchNamespaceDeviceIgnoresSuffixlessAlias pins that the subsystem-wide
// nvme-<model>_ha link is never selected. It points at an arbitrary namespace —
// nvme2n2 on the captured node — so honoring it would hand the caller another
// volume's block device.
func TestMatchNamespaceDeviceIgnoresSuffixlessAlias(t *testing.T) {
	f := newRealNodeFixture(t)
	sub := realNodeSubsystems[2] // suffixless alias points at namespace 2 of five

	for _, nsID := range sub.namespaceIDs() {
		got, err := matchNamespaceDevice(
			context.Background(), f.byIDDir, sub.model, sub.lvolIDs[nsID], nsID, testPoll,
		)
		if err != nil {
			t.Fatalf("namespace %d: unexpected error: %v", nsID, err)
		}
		if got == filepath.Join(f.byIDDir, sub.suffixlessLink()) {
			t.Errorf("namespace %d selected the subsystem-wide alias %q", nsID, got)
		}
		if resolved, want := f.resolve(got), sub.blockDevice(nsID); resolved != want {
			t.Errorf("namespace %d resolved to %q, want %q", nsID, resolved, want)
		}
	}
}

// TestMatchNamespaceDeviceIgnoresLocalNvme keeps the node's own PCIe NVMe out of
// the picture: those links carry an "_1" namespace suffix of the same shape.
func TestMatchNamespaceDeviceIgnoresLocalNvme(t *testing.T) {
	f := newRealNodeFixture(t)
	sub := realNodeSubsystems[0]

	got, err := matchNamespaceDevice(context.Background(), f.byIDDir, sub.model, sub.lvolIDs[1], 1, testPoll)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved := f.resolve(got); resolved != sub.blockDevice(1) {
		t.Errorf("resolved to %q, want %q", resolved, sub.blockDevice(1))
	}
	for _, foreign := range []string{"nvme5n1", "nvme10n1", "nvme11n1", "sda", "dm-0"} {
		if f.resolve(got) == foreign {
			t.Errorf("selected the node's own device %q", foreign)
		}
	}
}

// TestDisconnectGlobOnRealNode checks what Disconnect sees per subsystem: every
// nsid link of its own subsystem and nothing else, so a subsystem whose other
// namespaces are still connected is left alone.
func TestDisconnectGlobOnRealNode(t *testing.T) {
	f := newRealNodeFixture(t)

	for _, sub := range fixtureSubsystems {
		t.Run(sub.controller, func(t *testing.T) {
			matches, err := filepath.Glob(anyNamespaceDeviceGlob(f.byIDDir, sub.model))
			if err != nil {
				t.Fatalf("glob: %v", err)
			}

			var want []string
			for _, nsID := range sub.namespaceIDs() {
				want = append(want, filepath.Join(f.byIDDir, sub.namespaceLink(nsID)))
			}
			sort.Strings(want)
			if len(matches) != len(want) {
				t.Fatalf("glob matched %d links %v, want the %d namespace links", len(matches), matches, len(want))
			}
			for i := range want {
				if matches[i] != want[i] {
					t.Errorf("match %d = %q, want %q", i, matches[i], want[i])
				}
			}

			// The suffixless alias and the per-namespace uuid links carry no
			// nsid suffix, so they must stay out of the disconnect set.
			for _, match := range matches {
				name := filepath.Base(match)
				if name == sub.suffixlessLink() {
					t.Errorf("disconnect set contains the subsystem-wide alias %q", name)
				}
				if strings.HasPrefix(name, "nvme-uuid.") {
					t.Errorf("disconnect set contains the namespace uuid link %q", name)
				}
			}

			if _, shared := selectDisconnectTarget(matches); !shared {
				t.Errorf("subsystem with %d connected namespaces must not be torn down", len(want))
			}
		})
	}
}

// TestDisconnectGlobOnLastNamespace covers the teardown case: once a single
// namespace of the subsystem is left, its controller may go.
func TestDisconnectGlobOnLastNamespace(t *testing.T) {
	f := newRealNodeFixture(t)
	sub := realNodeSubsystems[2]

	// Unstage every namespace but the third.
	for _, nsID := range sub.namespaceIDs() {
		if nsID != 3 {
			f.remove(sub.namespaceLink(nsID))
		}
	}

	matches, err := filepath.Glob(anyNamespaceDeviceGlob(f.byIDDir, sub.model))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	got, shared := selectDisconnectTarget(matches)
	if shared {
		t.Fatalf("the last namespace must be disconnected, glob matched %v", matches)
	}
	if want := filepath.Join(f.byIDDir, sub.namespaceLink(3)); got != want {
		t.Errorf("selected %q for disconnect, want %q", got, want)
	}
}

// TestBrokenDisconnectGlobOnRealNode is the regression itself: against the real
// layout the pre-fix pattern matched nothing at all, so Disconnect never tore
// down a controller and the host stayed connected to the volume.
func TestBrokenDisconnectGlobOnRealNode(t *testing.T) {
	f := newRealNodeFixture(t)
	sub := realNodeSubsystems[1]

	broken := brokenDisconnectGlob(f.byIDDir, sub.model)
	if !strings.Contains(broken, "%s") {
		t.Fatalf("helper no longer reproduces the broken pattern: %q", broken)
	}
	matches, err := filepath.Glob(broken)
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("broken glob matched %v, expected it to be inert", matches)
	}
	if path, shared := selectDisconnectTarget(matches); path != "" || shared {
		t.Errorf("an empty match set must select nothing, got (%q, %v)", path, shared)
	}

	fixed, err := filepath.Glob(anyNamespaceDeviceGlob(f.byIDDir, sub.model))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(fixed) != len(sub.lvolIDs) {
		t.Errorf("fixed glob matched %d links, want %d", len(fixed), len(sub.lvolIDs))
	}
}

// TestMatchNamespaceDeviceFallbackNeedsNsidSuffix documents the reach of the
// lvol-UUID fallback glob. udev publishes the namespace UUID as
// nvme-uuid.<lvolID> with no nsid suffix, which the fallback cannot match; it
// only helps for links that do carry one.
func TestMatchNamespaceDeviceFallbackNeedsNsidSuffix(t *testing.T) {
	t.Run("uuid link without nsid suffix is not found", func(t *testing.T) {
		f := newDeviceFixture(t)
		f.addDevice("nvme-uuid."+testLvolID, "nvme0n2")

		if _, err := matchNamespaceDevice(
			context.Background(), f.byIDDir, testModel, testLvolID, 2, testPoll,
		); err == nil {
			t.Error("expected the suffixless uuid link to be ignored")
		}
	})

	t.Run("lvol named link with nsid suffix is found", func(t *testing.T) {
		f := newDeviceFixture(t)
		want := f.addDevice(testLink(testLvolID, 2), "nvme0n2")

		got, err := matchNamespaceDevice(context.Background(), f.byIDDir, testModel, testLvolID, 2, testPoll)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// TestMatchNamespaceDevicePrefersModelOverLvol pins the order of the two globs:
// with both link styles present the model link wins.
func TestMatchNamespaceDevicePrefersModelOverLvol(t *testing.T) {
	f := newDeviceFixture(t)
	want := f.addDevice(testLink(testModel, 1), "nvme0n1")
	f.addDevice(testLink(testLvolID, 1), "nvme1n1")

	got, err := matchNamespaceDevice(context.Background(), f.byIDDir, testModel, testLvolID, 1, testPoll)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want the model link %q", got, want)
	}
}

// --- glob construction -----------------------------------------------------

func TestNamespaceDeviceGlob(t *testing.T) {
	tests := []struct {
		name    string
		byIDDir string
		id      string
		nsID    int
		want    string
	}{
		{
			name:    "namespace one",
			byIDDir: "/dev/disk/by-id",
			id:      testModel,
			nsID:    1,
			want:    "/dev/disk/by-id/*" + testModel + "*_1",
		},
		{
			name:    "namespace two",
			byIDDir: "/dev/disk/by-id",
			id:      testModel,
			nsID:    2,
			want:    "/dev/disk/by-id/*" + testModel + "*_2",
		},
		{
			name:    "two digit namespace",
			byIDDir: "/dev/disk/by-id",
			id:      testModel,
			nsID:    19,
			want:    "/dev/disk/by-id/*" + testModel + "*_19",
		},
		{
			name:    "lvol id as identifier",
			byIDDir: "/dev/disk/by-id",
			id:      testLvolID,
			nsID:    3,
			want:    "/dev/disk/by-id/*" + testLvolID + "*_3",
		},
		{
			name:    "trailing separator in directory",
			byIDDir: "/dev/disk/by-id/",
			id:      testModel,
			nsID:    1,
			want:    "/dev/disk/by-id/*" + testModel + "*_1",
		},
		{
			name:    "fixture directory",
			byIDDir: "/tmp/by-id",
			id:      testModel,
			nsID:    7,
			want:    "/tmp/by-id/*" + testModel + "*_7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := namespaceDeviceGlob(tt.byIDDir, tt.id, tt.nsID)
			if got != tt.want {
				t.Errorf("namespaceDeviceGlob(%q, %q, %d) = %q, want %q", tt.byIDDir, tt.id, tt.nsID, got, tt.want)
			}
		})
	}
}

func TestAnyNamespaceDeviceGlob(t *testing.T) {
	got := anyNamespaceDeviceGlob("/dev/disk/by-id", testModel)
	want := "/dev/disk/by-id/*" + testModel + "*_[0-9]*"
	if got != want {
		t.Errorf("anyNamespaceDeviceGlob() = %q, want %q", got, want)
	}
}

// TestDeviceGlobsHaveNoUnexpandedVerb guards the bug class that broke both
// globs: a format string fed the wrong arguments leaves a "%s" verb, or a
// "%!d(string=...)" marker, inside a pattern that then matches nothing at all.
func TestDeviceGlobsHaveNoUnexpandedVerb(t *testing.T) {
	globs := map[string]string{
		"namespace":     namespaceDeviceGlob("/dev/disk/by-id", testModel, 2),
		"any namespace": anyNamespaceDeviceGlob("/dev/disk/by-id", testModel),
	}
	for name, glob := range globs {
		if strings.Contains(glob, "%") {
			t.Errorf("%s glob %q still contains a format verb", name, glob)
		}
		if _, err := filepath.Match(glob, "irrelevant"); err != nil {
			t.Errorf("%s glob %q is not a valid pattern: %v", name, glob, err)
		}
	}
}

// TestDeviceGlobsMatchLinkNames walks the by-id names of a real node past both
// patterns one by one.
func TestDeviceGlobsMatchLinkNames(t *testing.T) {
	const dir = "/dev/disk/by-id"
	sub := realNodeSubsystems[1] // 19 namespaces
	other := realNodeSubsystems[2]

	tests := []struct {
		name string
		glob string
		link string
		want bool
	}{
		// The namespace glob has to single out exactly one namespace.
		{
			name: "namespace glob matches its own namespace",
			glob: namespaceDeviceGlob(dir, sub.model, 2),
			link: sub.namespaceLink(2),
			want: true,
		},
		{
			name: "namespace glob rejects another namespace",
			glob: namespaceDeviceGlob(dir, sub.model, 1),
			link: sub.namespaceLink(2),
			want: false,
		},
		{
			name: "namespace one does not match namespace ten",
			glob: namespaceDeviceGlob(dir, sub.model, 1),
			link: sub.namespaceLink(10),
			want: false,
		},
		{
			name: "namespace one does not match namespace nineteen",
			glob: namespaceDeviceGlob(dir, sub.model, 1),
			link: sub.namespaceLink(19),
			want: false,
		},
		{
			name: "namespace nineteen matches namespace nineteen",
			glob: namespaceDeviceGlob(dir, sub.model, 19),
			link: sub.namespaceLink(19),
			want: true,
		},
		{
			name: "namespace nine does not match namespace nineteen",
			glob: namespaceDeviceGlob(dir, sub.model, 9),
			link: sub.namespaceLink(19),
			want: false,
		},
		{
			name: "namespace glob rejects another subsystem",
			glob: namespaceDeviceGlob(dir, sub.model, 1),
			link: other.namespaceLink(1),
			want: false,
		},
		{
			name: "namespace glob rejects the subsystem wide alias",
			glob: namespaceDeviceGlob(dir, sub.model, 1),
			link: sub.suffixlessLink(),
			want: false,
		},
		{
			name: "namespace glob rejects the namespace uuid link",
			glob: namespaceDeviceGlob(dir, sub.model, 1),
			link: "nvme-uuid." + sub.lvolIDs[1],
			want: false,
		},
		{
			name: "namespace glob rejects a local nvme link",
			glob: namespaceDeviceGlob(dir, sub.model, 1),
			link: "nvme-QEMU_NVMe_Ctrl_1121_1",
			want: false,
		},
		{
			name: "namespace glob rejects a partition of its own namespace",
			glob: namespaceDeviceGlob(dir, sub.model, 1),
			link: sub.namespaceLink(1) + "-part1",
			want: false,
		},
		{
			name: "namespace glob matches a non replicated volume",
			glob: namespaceDeviceGlob(dir, nonHASubsystem.model, 2),
			link: nonHASubsystem.namespaceLink(2),
			want: true,
		},
		{
			name: "namespace glob rejects another namespace of a non replicated volume",
			glob: namespaceDeviceGlob(dir, nonHASubsystem.model, 1),
			link: nonHASubsystem.namespaceLink(2),
			want: false,
		},

		// The any-namespace glob has to cover every namespace of the subsystem.
		{
			name: "any namespace glob matches namespace one",
			glob: anyNamespaceDeviceGlob(dir, sub.model),
			link: sub.namespaceLink(1),
			want: true,
		},
		{
			name: "any namespace glob matches a two digit namespace",
			glob: anyNamespaceDeviceGlob(dir, sub.model),
			link: sub.namespaceLink(19),
			want: true,
		},
		{
			name: "any namespace glob rejects another subsystem",
			glob: anyNamespaceDeviceGlob(dir, sub.model),
			link: other.namespaceLink(1),
			want: false,
		},
		{
			name: "any namespace glob rejects the subsystem wide alias",
			glob: anyNamespaceDeviceGlob(dir, sub.model),
			link: sub.suffixlessLink(),
			want: false,
		},
		{
			name: "any namespace glob rejects the namespace uuid link",
			glob: anyNamespaceDeviceGlob(dir, sub.model),
			link: "nvme-uuid." + sub.lvolIDs[5],
			want: false,
		},
		{
			name: "any namespace glob rejects a local nvme link",
			glob: anyNamespaceDeviceGlob(dir, sub.model),
			link: "nvme-QEMU_NVMe_Ctrl_1121_1",
			want: false,
		},
		{
			name: "any namespace glob matches a non replicated volume",
			glob: anyNamespaceDeviceGlob(dir, nonHASubsystem.model),
			link: nonHASubsystem.namespaceLink(2),
			want: true,
		},
		{
			name: "any namespace glob rejects the non replicated subsystem alias",
			glob: anyNamespaceDeviceGlob(dir, nonHASubsystem.model),
			link: nonHASubsystem.suffixlessLink(),
			want: false,
		},
		{
			// Known limitation, kept explicit: "_[0-9]*" cannot be anchored at
			// the end of the name, so a partition link counts as an extra match
			// and makes Disconnect treat the subsystem as still shared.
			name: "any namespace glob also matches partition links",
			glob: anyNamespaceDeviceGlob(dir, sub.model),
			link: sub.namespaceLink(1) + "-part1",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := filepath.Match(tt.glob, filepath.Join(dir, tt.link))
			if err != nil {
				t.Fatalf("Match(%q, %q): %v", tt.glob, tt.link, err)
			}
			if got != tt.want {
				t.Errorf("Match(%q, %q) = %v, want %v", tt.glob, tt.link, got, tt.want)
			}
		})
	}
}

// --- ambiguous and missing devices -----------------------------------------

// TestMatchNamespaceDeviceAmbiguousMatch covers a glob that matches two
// different block devices: either one may belong to another volume, so the scan
// has to fail instead of guessing.
func TestMatchNamespaceDeviceAmbiguousMatch(t *testing.T) {
	f := newDeviceFixture(t)
	f.addDevice(testLink(testModel, 2), "nvme0n2")
	f.addDevice("nvme-"+testModel+"_other_2", "nvme1n2")

	_, err := matchNamespaceDevice(context.Background(), f.byIDDir, testModel, testLvolID, 2, testPoll)
	if err == nil {
		t.Fatal("expected an error for two matches resolving to different devices")
	}
	if !strings.Contains(err.Error(), "different devices") {
		t.Errorf("error %q does not report the ambiguity", err)
	}
}

// TestMatchNamespaceDeviceAmbiguityResolves covers the transient form of the
// same situation: a stale link from a previous connect goes away while the scan
// is still polling, so the connect must survive it.
func TestMatchNamespaceDeviceAmbiguityResolves(t *testing.T) {
	f := newDeviceFixture(t)
	want := f.addDevice(testLink(testModel, 2), "nvme0n2")
	stale := "nvme-" + testModel + "_stale_2"
	f.addDevice(stale, "nvme9n2")
	f.removeAfter(5*time.Millisecond, stale)

	got, err := matchNamespaceDevice(context.Background(), f.byIDDir, testModel, testLvolID, 2, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("got device %q, want %q", got, want)
	}
}

// TestMatchNamespaceDeviceAliasesToSameDevice covers several by-id names for one
// namespace, which is normal and must not be treated as ambiguous.
func TestMatchNamespaceDeviceAliasesToSameDevice(t *testing.T) {
	f := newDeviceFixture(t)
	links := map[string]bool{
		f.addDevice(testLink(testModel, 2), "nvme0n2"):       true,
		f.addDevice("nvme-"+testModel+"_alias_2", "nvme0n2"): true,
	}

	got, err := matchNamespaceDevice(context.Background(), f.byIDDir, testModel, testLvolID, 2, testPoll)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Either name is a correct answer — they are the same namespace — so what
	// matters is that the scan returns one of them and it leads to the device.
	if !links[got] {
		t.Errorf("got device %q, want one of %v", got, links)
	}
	if resolved := f.resolve(got); resolved != "nvme0n2" {
		t.Errorf("link %q resolves to %q, want nvme0n2", got, resolved)
	}
}

// TestMatchNamespaceDeviceDanglingLink covers a by-id link whose block device is
// gone, left behind by an unclean disconnect.
func TestMatchNamespaceDeviceDanglingLink(t *testing.T) {
	f := newDeviceFixture(t)
	f.addDevice(testLink(testModel, 2), "nvme0n2")
	f.addDangling("nvme-"+testModel+"_stale_2", "nvme9n2")

	_, err := matchNamespaceDevice(context.Background(), f.byIDDir, testModel, testLvolID, 2, testPoll)
	if err == nil {
		t.Fatal("expected an error for an unresolvable device link")
	}
	if !strings.Contains(err.Error(), "failed to resolve device path") {
		t.Errorf("error %q does not report the unresolvable link", err)
	}
}

// TestMatchNamespaceDeviceMissing covers the namespace that never shows up: the
// error has to name both globs so a bad pattern is visible in the logs.
func TestMatchNamespaceDeviceMissing(t *testing.T) {
	f := newRealNodeFixture(t)
	sub := realNodeSubsystems[0]
	const absent = 42

	_, err := matchNamespaceDevice(context.Background(), f.byIDDir, sub.model, testLvolID, absent, testPoll)
	if err == nil {
		t.Fatal("expected an error when the namespace device never shows up")
	}
	for _, want := range []string{
		namespaceDeviceGlob(f.byIDDir, sub.model, absent),
		namespaceDeviceGlob(f.byIDDir, testLvolID, absent),
		"timed out waiting device ready",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestMatchNamespaceDeviceReportsBothFailures pins that the model glob's reason
// survives next to the fallback's: an ambiguous match and an empty directory are
// different problems and only the first one names a stale link.
func TestMatchNamespaceDeviceReportsBothFailures(t *testing.T) {
	f := newDeviceFixture(t)
	f.addDevice(testLink(testModel, 2), "nvme0n2")
	f.addDevice("nvme-"+testModel+"_other_2", "nvme1n2")

	_, err := matchNamespaceDevice(context.Background(), f.byIDDir, testModel, testLvolID, 2, testPoll)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "different devices") {
		t.Errorf("error %q lost the model glob's reason", err)
	}
	if !strings.Contains(err.Error(), namespaceDeviceGlob(f.byIDDir, testLvolID, 2)) {
		t.Errorf("error %q does not mention the fallback glob", err)
	}
}

// TestMatchNamespaceDeviceAppearsLate covers the wait itself: udev creates the
// symlink some time after nvme connect returns.
func TestMatchNamespaceDeviceAppearsLate(t *testing.T) {
	f := newDeviceFixture(t)
	link := testLink(testModel, 2)
	f.addDeviceAfter(15*time.Millisecond, link, "nvme0n2")

	got, err := matchNamespaceDevice(context.Background(), f.byIDDir, testModel, testLvolID, 2, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := filepath.Join(f.byIDDir, link); got != want {
		t.Errorf("got device %q, want %q", got, want)
	}
}

func TestMatchNamespaceDeviceContextCanceled(t *testing.T) {
	f := newDeviceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := matchNamespaceDevice(ctx, f.byIDDir, testModel, testLvolID, 2, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not wrap context.Canceled", err)
	}
}

// TestMatchNamespaceDeviceCanceledMidWaitDoesNotHang guards that a canceled
// context ends the scan instead of running the full attempt budget.
func TestMatchNamespaceDeviceCanceledMidWaitDoesNotHang(t *testing.T) {
	f := newDeviceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := matchNamespaceDevice(ctx, f.byIDDir, testModel, testLvolID, 2, time.Hour); err == nil {
		t.Fatal("expected an error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("scan took %s, expected it to end with the context", elapsed)
	}
}

// --- disconnect target selection -------------------------------------------

func TestSelectDisconnectTarget(t *testing.T) {
	tests := []struct {
		name       string
		matches    []string
		wantPath   string
		wantShared bool
	}{
		{
			name:       "no device left",
			matches:    nil,
			wantPath:   "",
			wantShared: false,
		},
		{
			name:       "empty slice",
			matches:    []string{},
			wantPath:   "",
			wantShared: false,
		},
		{
			name:       "single namespace",
			matches:    []string{"/dev/disk/by-id/nvme-model_ha_1"},
			wantPath:   "/dev/disk/by-id/nvme-model_ha_1",
			wantShared: false,
		},
		{
			name: "two namespaces of the same subsystem",
			matches: []string{
				"/dev/disk/by-id/nvme-model_ha_1",
				"/dev/disk/by-id/nvme-model_ha_2",
			},
			wantPath:   "",
			wantShared: true,
		},
		{
			name: "many namespaces of the same subsystem",
			matches: []string{
				"/dev/disk/by-id/nvme-model_ha_1",
				"/dev/disk/by-id/nvme-model_ha_2",
				"/dev/disk/by-id/nvme-model_ha_19",
			},
			wantPath:   "",
			wantShared: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPath, gotShared := selectDisconnectTarget(tt.matches)
			if gotPath != tt.wantPath || gotShared != tt.wantShared {
				t.Errorf("selectDisconnectTarget(%v) = (%q, %v), want (%q, %v)",
					tt.matches, gotPath, gotShared, tt.wantPath, tt.wantShared)
			}
		})
	}
}

// --- wait loops ------------------------------------------------------------

func TestWaitForDeviceReadyNoAttemptsScansOnce(t *testing.T) {
	f := newDeviceFixture(t)
	want := f.addDevice(testLink(testModel, 1), "nvme0n1")
	glob := namespaceDeviceGlob(f.byIDDir, testModel, 1)

	got, err := waitForDeviceReady(context.Background(), glob, 0, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// A single scan must not wait out the interval when nothing matches.
	start := time.Now()
	if _, err := waitForDeviceReady(context.Background(), glob+"-absent", 0, time.Hour); err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a zero attempt scan waited %s", elapsed)
	}
}

func TestWaitForDeviceReadyScansAttemptsTimes(t *testing.T) {
	f := newDeviceFixture(t)
	glob := namespaceDeviceGlob(f.byIDDir, testModel, 1)

	// Two attempts sleep once between them, so the whole call is bounded by the
	// interval times the number of attempts.
	start := time.Now()
	if _, err := waitForDeviceReady(context.Background(), glob, 2, 20*time.Millisecond); err == nil {
		t.Fatal("expected a timeout error")
	}
	elapsed := time.Since(start)
	if elapsed < 40*time.Millisecond {
		t.Errorf("gave up after %s, expected it to use its two intervals", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %s for three scans", elapsed)
	}
}

func TestWaitForDeviceReadyBadPattern(t *testing.T) {
	_, err := waitForDeviceReady(context.Background(), "/dev/disk/by-id/[", 1, testPoll)
	if !errors.Is(err, filepath.ErrBadPattern) {
		t.Errorf("error %v is not filepath.ErrBadPattern", err)
	}
}

func TestWaitForDeviceGoneAlreadyGone(t *testing.T) {
	f := newDeviceFixture(t)
	glob := anyNamespaceDeviceGlob(f.byIDDir, testModel)

	start := time.Now()
	if err := waitForDeviceGone(context.Background(), glob, deviceGoneAttempts, time.Hour); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %s for an already empty glob", elapsed)
	}
}

func TestWaitForDeviceGoneAfterRemoval(t *testing.T) {
	f := newDeviceFixture(t)
	link := testLink(testModel, 2)
	f.addDevice(link, "nvme0n2")
	f.removeAfter(10*time.Millisecond, link)

	glob := anyNamespaceDeviceGlob(f.byIDDir, testModel)
	if err := waitForDeviceGone(context.Background(), glob, deviceGoneAttempts, 5*time.Millisecond); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestWaitForDeviceGoneNeverGone covers a device the kernel keeps around, which
// has to surface as an error instead of a silent success.
func TestWaitForDeviceGoneNeverGone(t *testing.T) {
	f := newDeviceFixture(t)
	f.addDevice(testLink(testModel, 2), "nvme0n2")

	glob := anyNamespaceDeviceGlob(f.byIDDir, testModel)
	err := waitForDeviceGone(context.Background(), glob, 3, testPoll)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out waiting device gone") {
		t.Errorf("error %q does not report the timeout", err)
	}
	if !strings.Contains(err.Error(), glob) {
		t.Errorf("error %q does not mention the glob", err)
	}
}

// TestWaitForDeviceGoneSiblingNamespaceKeepsMatching pins that the wait watches
// the whole subsystem: while any namespace link is left, the glob still matches.
func TestWaitForDeviceGoneSiblingNamespaceKeepsMatching(t *testing.T) {
	f := newRealNodeFixture(t)
	sub := realNodeSubsystems[2]
	f.remove(sub.namespaceLink(3))

	glob := anyNamespaceDeviceGlob(f.byIDDir, sub.model)
	if err := waitForDeviceGone(context.Background(), glob, 2, testPoll); err == nil {
		t.Error("expected an error while the other namespaces are still connected")
	}
}

func TestWaitForDeviceGoneContextCanceled(t *testing.T) {
	f := newDeviceFixture(t)
	f.addDevice(testLink(testModel, 2), "nvme0n2")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitForDeviceGone(ctx, anyNamespaceDeviceGlob(f.byIDDir, testModel), deviceGoneAttempts, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not wrap context.Canceled", err)
	}
}

func TestWaitForDeviceGoneBadPattern(t *testing.T) {
	err := waitForDeviceGone(context.Background(), "/dev/disk/by-id/[", 1, testPoll)
	if !errors.Is(err, filepath.ErrBadPattern) {
		t.Errorf("error %v is not filepath.ErrBadPattern", err)
	}
}

// --- resolveToSameDevice ---------------------------------------------------

func TestResolveToSameDevice(t *testing.T) {
	t.Run("single match", func(t *testing.T) {
		f := newDeviceFixture(t)
		link := f.addDevice(testLink(testModel, 1), "nvme0n1")

		got, err := resolveToSameDevice([]string{link})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != link {
			t.Errorf("got %q, want %q", got, link)
		}
	})

	t.Run("aliases of one device", func(t *testing.T) {
		f := newDeviceFixture(t)
		first := f.addDevice(testLink(testModel, 1), "nvme0n1")
		second := f.addDevice("nvme-"+testModel+"_alias_1", "nvme0n1")
		third := f.addDevice("nvme-uuid."+testLvolID, "nvme0n1")

		got, err := resolveToSameDevice([]string{first, second, third})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != first {
			t.Errorf("got %q, want the first match %q", got, first)
		}
	})

	t.Run("different devices", func(t *testing.T) {
		f := newDeviceFixture(t)
		first := f.addDevice(testLink(testModel, 1), "nvme0n1")
		second := f.addDevice("nvme-"+testModel+"_other_1", "nvme1n1")

		_, err := resolveToSameDevice([]string{first, second})
		if err == nil {
			t.Fatal("expected an error for diverging devices")
		}
		if !strings.Contains(err.Error(), "different devices") {
			t.Errorf("error %q does not report diverging devices", err)
		}
	})

	t.Run("third match diverges", func(t *testing.T) {
		f := newDeviceFixture(t)
		first := f.addDevice(testLink(testModel, 1), "nvme0n1")
		second := f.addDevice("nvme-"+testModel+"_alias_1", "nvme0n1")
		third := f.addDevice("nvme-"+testModel+"_other_1", "nvme1n1")

		if _, err := resolveToSameDevice([]string{first, second, third}); err == nil {
			t.Fatal("expected an error for a diverging third match")
		}
	})

	t.Run("dangling link", func(t *testing.T) {
		f := newDeviceFixture(t)
		first := f.addDevice(testLink(testModel, 1), "nvme0n1")
		stale := f.addDangling("nvme-"+testModel+"_stale_1", "nvme9n1")

		_, err := resolveToSameDevice([]string{first, stale})
		if err == nil {
			t.Fatal("expected an error for an unresolvable link")
		}
		if !strings.Contains(err.Error(), "failed to resolve device path") {
			t.Errorf("error %q does not report the unresolvable link", err)
		}
	})
}
