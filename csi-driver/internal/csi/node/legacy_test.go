// Naming a volume this driver did not stage.
//
// The cases here are all the same situation from different angles: an upgrade
// leaves volumes that are up on the host and were built by a version that wrote
// no stack record, and a teardown of one has to know which namespace it is
// allowed to detach. What it must never do is guess, which is why the refusal
// these tests bracket is worth keeping.

package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/layers"
)

// legacyContext is what a volume staged before the stack has when the stash
// never landed: nothing. The old node service wrote volume-context.json as the
// last step of NodeStageVolume, after the connect and after the mount, so a
// driver that died in that window left a staged volume with no context at all.
func legacyContext() map[string]string { return map[string]string{} }

// A volume that neither the record nor the stash names is named by the host:
// the staging path is a mount, the mount has a device under it, and the device
// says which subsystem and namespace it belongs to. That is evidence rather
// than a guess, which is the only thing that may lift the refusal.
func TestTeardownNamesALegacyVolumeFromTheHost(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())

	asked := ""
	ns.identifyStaged = func(_ context.Context, stagingTargetPath string) (lvol.Connection, error) {
		asked = stagingTargetPath
		return lvol.Connection{
			NQN:  "nqn.2023-02.io.simplyblock:cluster:lvol:abc",
			NSID: 1,
			UUID: "abc",
		}, nil
	}

	plan, err := ns.teardownPlan(
		context.Background(), pvcTestHandle, "/staging", legacyContext())
	if err != nil {
		t.Fatalf("teardownPlan refused a volume the host can name: %v", err)
	}
	if got := strings.Join(plan.Names(), " → "); got != plainShape {
		t.Errorf("the volume is released as %s, want the legacy plan", got)
	}
	if asked != "/staging" {
		t.Errorf("the host was asked about %q, want the staging path", asked)
	}
}

// The refusal stands where the host cannot name it either. Releasing a
// namespace nothing identifies detaches whichever one is found first, which on
// a node serving several volumes is somebody else's.
func TestTeardownRefusesAVolumeTheHostCannotName(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())
	ns.identifyStaged = func(context.Context, string) (lvol.Connection, error) {
		return lvol.Connection{}, errors.New("no device is mounted at /staging")
	}

	_, err := ns.teardownPlan(context.Background(), pvcTestHandle, "/staging", legacyContext())
	if err == nil {
		t.Fatal("a volume nothing identifies was accepted for release")
	}
	if !strings.Contains(err.Error(), "identifies") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

// Regression: 2026-09-21-host-reading-named-a-subsystem.
//
// A subsystem NQN with no namespace id names a subsystem rather than a volume,
// and a selector whose NSID is zero matches every namespace in it
// (atlas-lib/nvme: "namespace id, where 0 means any"). Releasing on that
// detaches whichever namespace ranked first, which on a subsystem holding
// several volumes is a co-tenant's, so it is refused exactly as an empty
// reading is.
func TestTeardownRefusesAHostReadingWithNoNamespace(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())
	ns.identifyStaged = func(context.Context, string) (lvol.Connection, error) {
		return lvol.Connection{NQN: "nqn.2023-02.io.simplyblock:cluster:lvol:abc"}, nil
	}

	if _, err := ns.teardownPlan(
		context.Background(), pvcTestHandle, "/staging", legacyContext()); err == nil {
		t.Fatal("a reading naming only a subsystem was accepted for release")
	}
}

// The namespace id is enough beside the NQN, and so is the UUID on its own:
// both name one namespace rather than a subsystem.
func TestTeardownAcceptsEitherWayOfNamingOneNamespace(t *testing.T) {
	for name, connection := range map[string]lvol.Connection{
		"nqn and nsid": {NQN: "nqn.2023-02.io.simplyblock:cluster:lvol:abc", NSID: 1},
		"uuid alone":   {UUID: "abc"},
	} {
		ns, _ := newStackedServer(t, newRecordingRunner())
		ns.identifyStaged = func(context.Context, string) (lvol.Connection, error) {
			return connection, nil
		}

		if _, err := ns.teardownPlan(
			context.Background(), pvcTestHandle, "/staging", legacyContext()); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
}

// A context that names the volume is not overridden by the host. The stash is
// what the volume was staged with, the host reading is a reconstruction, and
// the two agreeing is not something to rely on.
func TestTeardownPrefersTheStashOverTheHost(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())
	asked := false
	ns.identifyStaged = func(context.Context, string) (lvol.Connection, error) {
		asked = true
		return lvol.Connection{NQN: "nqn.from.the.host", NSID: 9}, nil
	}

	if _, err := ns.teardownPlan(
		context.Background(), pvcTestHandle, "/staging", stagedContext()); err != nil {
		t.Fatalf("teardownPlan: %v", err)
	}
	if asked {
		t.Error("the host was read for a volume whose stashed context already named it")
	}
}

// Regression: 2026-09-21-legacy-identity-read-by-device-path.
//
// The adapter itself, against a sysfs tree rather than a stub.
//
// The three tests above replace identifyStaged, so they say when the host is
// consulted and never what it answers. This one runs the production function:
// a namespace whose recorded device number is the one the staging path resolves
// to, and the fields of the connection it builds out of what sysfs said.
func TestStagedIdentityReadsTheNamespaceOffSysfs(t *testing.T) {
	staged := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatalf("make the staging directory: %v", err)
	}
	number, err := nvme.DeviceNumberAt(staged)
	if err != nil {
		t.Fatalf("read the device number of the staging path: %v", err)
	}

	const (
		nqn  = "nqn.2023-02.io.simplyblock:cluster:lvol:9b1deb4d"
		uuid = "9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d"
	)
	sysRoot := writeNamespaceFixture(t, nqn, uuid, "2", number)

	connection, err := stagedIdentity(nvme.SysfsConfig{SysRoot: sysRoot, DevRoot: "/dev"})(
		context.Background(), staged)
	if err != nil {
		t.Fatalf("stagedIdentity: %v", err)
	}

	if connection.NQN != nqn {
		t.Errorf("NQN = %q, want the subsystem the namespace belongs to", connection.NQN)
	}
	if connection.NSID != 2 {
		t.Errorf("NSID = %d, want 2", connection.NSID)
	}
	if connection.UUID != uuid {
		t.Errorf("UUID = %q, want the namespace's", connection.UUID)
	}
}

// A staging path whose device number no namespace carries is an error rather
// than an empty connection, because an empty one reads as "not encrypted, not
// identified" to everything downstream.
func TestStagedIdentityFailsWhenNoNamespaceCarriesTheNumber(t *testing.T) {
	staged := t.TempDir()
	sysRoot := writeNamespaceFixture(t, "nqn.example", "some-uuid", "1", "999:999")

	if _, err := stagedIdentity(nvme.SysfsConfig{SysRoot: sysRoot, DevRoot: "/dev"})(
		context.Background(), staged); err == nil {
		t.Fatal("a staging path no namespace backs was named anyway")
	}
}

// writeNamespaceFixture writes the sysfs a single-namespace subsystem presents.
func writeNamespaceFixture(t *testing.T, nqn, uuid, nsid, deviceNumber string) string {
	t.Helper()

	root := t.TempDir()
	sub := filepath.Join(root, "class", "nvme-subsystem", "nvme-subsys0")
	ns := filepath.Join(sub, "nvme0n1")
	for path, content := range map[string]string{
		filepath.Join(sub, "subsysnqn"):  nqn,
		filepath.Join(sub, "model"):      uuid,
		filepath.Join(sub, "subsystype"): "nvm",
		filepath.Join(ns, "nsid"):        nsid,
		filepath.Join(ns, "uuid"):        uuid,
		filepath.Join(ns, "dev"):         deviceNumber,
		filepath.Join(ns, "size"):        "20971520",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("make the fixture directory: %v", err)
		}
		if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
			t.Fatalf("write the fixture file: %v", err)
		}
	}
	return root
}

// legacyStashedContext is the volume context the previous node service wrote,
// recorded from a cluster rather than invented: every key below appeared in the
// stash of a volume staged by that service, including the ones it left empty.
//
// devicePath is a /dev/disk/by-id link because that is what its initiator
// returned and therefore what it stashed, and there is no key naming the
// filesystem because nothing recorded one until the volume stack did.
func legacyStashedContext() map[string]string {
	const (
		cluster = "53a636db-8c9f-4cc2-952b-ea9207d8c048"
		lvol    = "d6d0fda8-4d78-4f3d-856c-1193546b337f"
	)
	return map[string]string{
		"cluster_id":                       cluster,
		"connections":                      `[{"ip":"10.0.2.3","port":4426},{"ip":"10.0.2.4","port":4426}]`,
		"csi.storage.k8s.io/pv/name":       "pvc-d17cf115-b1e7-40ba-b840-859e3a97515a",
		"csi.storage.k8s.io/pvc/name":      "guardian-pvc",
		"csi.storage.k8s.io/pvc/namespace": "spdkcsi-7383",
		"ctrlLossTmo":                      "3600",
		"devicePath":                       "/dev/disk/by-id/nvme-SPDK_Controller1_SPDK00000000000001",
		"hostIface":                        "",
		"max_namespace_per_subsys":         "1",
		"model":                            lvol,
		"name":                             lvol,
		"nqn":                              "nqn.2023-02.io.simplyblock:" + cluster + ":lvol:" + lvol,
		"nrIoQueues":                       "3",
		"nsId":                             "1",
		"pool_name":                        "pool1",
		"qos_r_mbytes":                     "",
		"qos_rw_iops":                      "",
		"qos_rw_mbytes":                    "",
		"qos_w_mbytes":                     "",
		"reconnectDelay":                   "2",
		"targetType":                       "tcp",
		"uuid":                             lvol,
	}
}

// The path every upgraded volume takes: the stash the previous node service
// wrote, no stack record, and the plan a teardown rebuilds out of the two.
//
// The hand-made fixture beside this one states the four keys the derivation
// reads. This one states everything that service actually wrote, which is what
// says the derivation still finds them among twenty-two keys, reads the
// namespace id from the camel-cased nsId that service chose, and is untroubled
// by the by-id device path it stashed and by the values it left empty.
func TestALegacyStashIsDerivedIntoATeardownPlan(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())
	ns.identifyStaged = func(context.Context, string) (lvol.Connection, error) {
		t.Error("the host was read for a volume whose stash names it")
		return lvol.Connection{}, errors.New("not reached")
	}

	vc := legacyStashedContext()
	plan, err := ns.teardownPlan(context.Background(), pvcTestHandle, "/staging", vc)
	if err != nil {
		t.Fatalf("teardownPlan over a legacy stash: %v", err)
	}
	if got := strings.Join(plan.Names(), " → "); got != plainShape {
		t.Errorf("the volume is released as %s, want the legacy plan", got)
	}

	// The shape alone would pass on a derivation that read none of the keys:
	// the UUID is enough to build a plan, so a namespace id fetched from a
	// misspelled key leaves a selector matching every namespace of the
	// subsystem and a plan that looks perfectly well formed. What the release
	// acts on is these three values.
	fabric := fabricOf(t, plan)
	if fabric.NQN != vc["nqn"] {
		t.Errorf("the plan releases subsystem %q, want %q", fabric.NQN, vc["nqn"])
	}
	if fabric.NSID != 1 {
		t.Errorf("the plan releases namespace %d, want the 1 the stash names in nsId", fabric.NSID)
	}
}

// fabricOf is what the plan's fabric layer would be rebuilt from, which is the
// only place a plan states which namespace it acts on.
func fabricOf(t *testing.T, plan volstack.Plan) layers.FabricParams {
	t.Helper()
	for _, layer := range plan {
		if layer.Name() != "fabric" {
			continue
		}
		recorder, ok := layer.(volstack.Recorder)
		if !ok {
			t.Fatal("the fabric layer records no parameters")
		}
		params, ok := recorder.Params().(layers.FabricParams)
		if !ok {
			t.Fatalf("the fabric layer records %T, want FabricParams", recorder.Params())
		}
		return params
	}
	t.Fatal("the plan has no fabric layer")
	return layers.FabricParams{}
}

// The same stash with no filesystem key, which is every volume of that era:
// nothing recorded one until the stack did. The release unmounts whatever is at
// the staging path, so a plan that cannot name the filesystem is still a plan
// that takes it down, and the teardown must not refuse for the want of a name
// it was never going to have.
func TestALegacyStashWithoutAFilesystemStillReleases(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())

	vc := legacyStashedContext()
	if _, stated := vc[stagedFsTypeKey]; stated {
		t.Fatalf("the fixture states %s, and the volumes it stands for did not", stagedFsTypeKey)
	}

	if _, err := ns.teardownPlan(
		context.Background(), pvcTestHandle, "/staging", vc); err != nil {
		t.Fatalf("teardownPlan over a stash naming no filesystem: %v", err)
	}
}
