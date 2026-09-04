// Tests for the node server's PersistentVolumeClaim lookup: resolving the claim
// that owns a staged CSI volume, and reading the on-disk-filesystem annotation
// off it. Both routes to the claim are covered, since a volume provisioned
// before the provisioner stashed the claim's identity in the volume context can
// only be resolved through its PersistentVolume.
package spdk

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	sbkube "github.com/spdk/spdk-csi/pkg/kubernetes"
)

const (
	pvcTestCluster = "8ffac363-0c46-4714-a71b-f9c0b58a1269"
	pvcTestPool    = "df34f16c-1a2b-3c4d-5e6f-7a8b9c0d1e2f"
	pvcTestLvol    = "8e2dcb9d-1b2c-4f3a-9d4e-5f6a7b8c9d0e"
	pvcTestHandle  = pvcTestCluster + ":" + pvcTestPool + ":" + pvcTestLvol

	// The claim every test resolves to. Nothing in the lookup depends on the
	// namespace or the name, so one identity serves all of them.
	pvcTestNamespace = "apps"
	pvcTestName      = "data"
)

// newPVCTestNodeServer builds a node server whose only wiring is a Kubernetes
// client and a cache manager over the given objects, which is all the claim
// lookup and the annotation write-back need. The client is returned so a test
// can assert on the writes that reached it.
func newPVCTestNodeServer(t *testing.T, objects ...runtime.Object) (*nodeServer, *fake.Clientset) {
	t.Helper()

	client := fake.NewSimpleClientset(objects...)
	manager := sbkube.NewManager(client)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	manager.Start(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for !manager.HasSynced() {
		if time.Now().After(deadline) {
			t.Fatal("kubernetes cache did not sync within timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}

	return &nodeServer{kubeClient: client, manager: manager}, client
}

// annotatedPVC returns the test claim carrying the on-disk-filesystem
// annotation, or a bare claim when value is empty.
func annotatedPVC(value string) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: pvcTestNamespace, Name: pvcTestName},
	}
	if value != "" {
		pvc.Annotations = map[string]string{annotationOnDiskFilesystem: value}
	}
	return pvc
}

// boundPV returns a PersistentVolume bound to the test claim and carrying the
// test volume handle, which is how the fallback route finds the claim.
func boundPV() *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-" + pvcTestName},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       testDriverName,
					VolumeHandle: pvcTestHandle,
				},
			},
			ClaimRef: &corev1.ObjectReference{Namespace: pvcTestNamespace, Name: pvcTestName},
		},
	}
}

func TestPersistentVolumeClaimForVolume_FromVolumeContext(t *testing.T) {
	ns, _ := newPVCTestNodeServer(t, annotatedPVC("xfs"))

	pvc, err := ns.persistentVolumeClaimForVolume(context.Background(), pvcTestHandle, map[string]string{
		CSIStorageNamespaceKey: pvcTestNamespace,
		CSIStorageNameKey:      pvcTestName,
	})
	if err != nil {
		t.Fatalf("persistentVolumeClaimForVolume: unexpected error %v", err)
	}
	if pvc.Namespace != pvcTestNamespace || pvc.Name != pvcTestName {
		t.Fatalf("persistentVolumeClaimForVolume = %s/%s, want %s/%s",
			pvc.Namespace, pvc.Name, pvcTestNamespace, pvcTestName)
	}
}

func TestPersistentVolumeClaimForVolume_FromPersistentVolumeClaimRef(t *testing.T) {
	ns, _ := newPVCTestNodeServer(t, annotatedPVC("xfs"), boundPV())

	// No claim identity in the volume context: the claim has to be found through
	// the PersistentVolume that carries this volume handle.
	pvc, err := ns.persistentVolumeClaimForVolume(context.Background(), pvcTestHandle, map[string]string{})
	if err != nil {
		t.Fatalf("persistentVolumeClaimForVolume: unexpected error %v", err)
	}
	if pvc.Namespace != pvcTestNamespace || pvc.Name != pvcTestName {
		t.Fatalf("persistentVolumeClaimForVolume = %s/%s, want %s/%s",
			pvc.Namespace, pvc.Name, pvcTestNamespace, pvcTestName)
	}
}

func TestPersistentVolumeClaimForVolume_ClaimGone(t *testing.T) {
	ns, _ := newPVCTestNodeServer(t)

	_, err := ns.persistentVolumeClaimForVolume(context.Background(), pvcTestHandle, map[string]string{
		CSIStorageNamespaceKey: pvcTestNamespace,
		CSIStorageNameKey:      pvcTestName,
	})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("persistentVolumeClaimForVolume error = %v, want NotFound", err)
	}
}

func TestPersistentVolumeClaimForVolume_UnresolvableHandle(t *testing.T) {
	ns, _ := newPVCTestNodeServer(t)

	if _, err := ns.persistentVolumeClaimForVolume(context.Background(), "not-a-handle", nil); err == nil {
		t.Fatal("persistentVolumeClaimForVolume on an unparsable handle = nil error, want an error")
	}
}

// A claim that names no filesystem is the only reading that lets staging carry
// on with the filesystem the volume capability asked for. Every other reading
// that is not a filesystem this driver creates fails staging instead, since a
// device that may already hold data must not be handed to mkfs.
func TestAnnotatedFilesystem(t *testing.T) {
	cases := []struct {
		name       string
		annotation string
		want       string
		wantError  bool
	}{
		{name: "xfs requested", annotation: "xfs", want: "xfs"},
		{name: "ext4 requested", annotation: "ext4", want: "ext4"},
		{name: "surrounding whitespace and case", annotation: "  XFS ", want: "xfs"},
		{name: "no annotation", annotation: "", want: ""},
		{name: "unsupported filesystem", annotation: "btrfs", wantError: true},
		{name: "not a filesystem at all", annotation: "; rm -rf /", wantError: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, _ := newPVCTestNodeServer(t, annotatedPVC(tc.annotation))

			got, err := ns.annotatedFilesystem(context.Background(), pvcTestHandle, map[string]string{
				CSIStorageNamespaceKey: pvcTestNamespace,
				CSIStorageNameKey:      pvcTestName,
			})
			if tc.wantError {
				if err == nil {
					t.Fatalf("annotatedFilesystem(%q) = %q, nil, want an error", tc.annotation, got)
				}
				if got != "" {
					t.Fatalf("annotatedFilesystem(%q) = %q alongside its error, want %q", tc.annotation, got, "")
				}
				return
			}
			if err != nil {
				t.Fatalf("annotatedFilesystem(%q): unexpected error %v", tc.annotation, err)
			}
			if got != tc.want {
				t.Fatalf("annotatedFilesystem(%q) = %q, want %q", tc.annotation, got, tc.want)
			}
		})
	}
}

// A claim that cannot be read leaves it unsettled whether the blank probe means
// a blank device or a failed read, which is the doubt the annotation exists to
// resolve. Staging fails there rather than formatting through it.
func TestAnnotatedFilesystem_NoClaim(t *testing.T) {
	ns, _ := newPVCTestNodeServer(t)

	got, err := ns.annotatedFilesystem(context.Background(), pvcTestHandle, nil)
	if err == nil {
		t.Fatalf("annotatedFilesystem without a claim = %q, nil, want an error", got)
	}
	if got != "" {
		t.Fatalf("annotatedFilesystem without a claim = %q alongside its error, want %q", got, "")
	}
}

// Formatting is irreversible, so the only device staging may format is one it
// positively read as blank. These cases cover the two readings that are not
// that, and must therefore fail staging rather than fall through to mkfs.
func TestClassifyDiskFormat(t *testing.T) {
	cases := []struct {
		name      string
		fs        string
		probeErr  error
		want      string
		wantError bool
	}{
		{name: "blank device", fs: "", want: ""},
		{name: "already formatted", fs: "ext4", want: "ext4"},
		{name: "unreadable device", probeErr: errors.New("blkid: broken pipe"), wantError: true},
		{name: "partition table, no filesystem", fs: unknownDiskFormat, wantError: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classifyDiskFormat("/dev/nvme9n1", tc.fs, tc.probeErr)
			if tc.wantError {
				if err == nil {
					t.Fatalf("classifyDiskFormat(%q, %v) = %q, nil, want an error", tc.fs, tc.probeErr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("classifyDiskFormat(%q, %v): unexpected error %v", tc.fs, tc.probeErr, err)
			}
			if got != tc.want {
				t.Fatalf("classifyDiskFormat(%q, %v) = %q, want %q", tc.fs, tc.probeErr, got, tc.want)
			}
		})
	}
}

// patchedFilesystems returns the on-disk-filesystem value of every claim patch
// the node server sent, in order, so a test can assert both what was written and
// that nothing was written at all.
func patchedFilesystems(t *testing.T, client *fake.Clientset) []string {
	t.Helper()

	var written []string
	for _, action := range client.Actions() {
		patch, ok := action.(k8stesting.PatchAction)
		if !ok || action.GetResource().Resource != "persistentvolumeclaims" {
			continue
		}
		var body struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(patch.GetPatch(), &body); err != nil {
			t.Fatalf("claim patch is not valid JSON: %v", err)
		}
		written = append(written, body.Metadata.Annotations[annotationOnDiskFilesystem])
	}
	return written
}

func TestRecordOnDiskFilesystem(t *testing.T) {
	cases := []struct {
		name       string
		annotation string
		staged     string
		want       []string
	}{
		{name: "claim not annotated yet", annotation: "", staged: "ext4", want: []string{"ext4"}},
		{name: "claim asked for what was staged", annotation: "xfs", staged: "xfs", want: nil},
		{name: "claim asked for something else", annotation: "ext4", staged: "xfs", want: []string{"xfs"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, client := newPVCTestNodeServer(t, annotatedPVC(tc.annotation))

			ns.recordOnDiskFilesystem(context.Background(), pvcTestHandle, map[string]string{
				CSIStorageNamespaceKey: pvcTestNamespace,
				CSIStorageNameKey:      pvcTestName,
			}, tc.staged)

			got := patchedFilesystems(t, client)
			if len(got) != len(tc.want) {
				t.Fatalf("recordOnDiskFilesystem wrote %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("recordOnDiskFilesystem wrote %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// The volume is staged and mounted by the time the annotation is written, so a
// claim that cannot be reached is a bookkeeping loss, never a staging failure.
func TestRecordOnDiskFilesystem_NoClaim(t *testing.T) {
	ns, client := newPVCTestNodeServer(t)

	ns.recordOnDiskFilesystem(context.Background(), pvcTestHandle, nil, "ext4")

	if got := patchedFilesystems(t, client); len(got) != 0 {
		t.Fatalf("recordOnDiskFilesystem without a claim wrote %v, want no writes", got)
	}
}
