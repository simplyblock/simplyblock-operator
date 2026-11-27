// The 26.4.0-beta7 module's rules against a fake client (design-release-upgrade.md
// §6.6, test plan U-60 to U-66).

package r26_4_0_beta7

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

const ns = "simplyblock"

func scopeWith(t *testing.T, objects ...client.Object) *upgrade.Scope {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, simplyblockv1alpha2.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return upgrade.NewScope(c, ns, upgrade.StageReleaseRollout, upgrade.Options{}, logr.Discard(), nil)
}

func cluster(name string, class simplyblockv1alpha2.StorageClusterDeviceClass) *simplyblockv1alpha2.StorageCluster {
	c := &simplyblockv1alpha2.StorageCluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	c.Spec.DeviceClass = class
	return c
}

// U-60, U-61: a lblk cluster draws the reweight warning, another does not, and
// the warning never refuses the upgrade.
func TestTheReweightWarningNamesOnlyLblkClusters(t *testing.T) {
	s := scopeWith(t,
		cluster("lblk", simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock),
		cluster("nvme", ""))
	findings, err := reweightPending(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings.Blocked() || len(findings[0].Objects) != 1 ||
		findings[0].Objects[0].Name != "lblk" {
		t.Errorf("findings = %v, want one warning naming the lblk cluster only", findings)
	}

	none, err := reweightPending(context.Background(), scopeWith(t, cluster("nvme", "")))
	if err != nil || len(none) != 0 {
		t.Errorf("findings = %v, %v; want none without a lblk cluster", none, err)
	}
}

// helmRelease is a deployed Helm release Secret carrying the given values.
func helmRelease(t *testing.T, values map[string]any) *corev1.Secret {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"name": "simplyblock-operator", "namespace": ns, "version": 1, "manifest": "", "config": values,
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.simplyblock-operator.v1", Namespace: ns,
			Labels: map[string]string{"owner": "helm", "status": "deployed", "name": "simplyblock-operator"}},
		Type: "helm.sh/release.v1",
		Data: map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(buf.Bytes()))},
	}
}

// U-66: the removed values are a warning that names them.
func TestTheRemovedValuesAreAWarning(t *testing.T) {
	s := scopeWith(t, helmRelease(t, map[string]any{
		"driver": map[string]any{"pnfs": map[string]any{"mds": map[string]any{"stateSize": "10Gi"}}},
	}))
	findings, err := helmValuesRemovedKeys(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings.Blocked() || !strings.Contains(findings[0].Summary, "driver.pnfs.mds.stateSize") {
		t.Errorf("findings = %v, want one warning naming the key", findings)
	}

	clean, err := helmValuesRemovedKeys(context.Background(), scopeWith(t, helmRelease(t, map[string]any{})))
	if err != nil || len(clean) != 0 {
		t.Errorf("findings = %v, %v; want none", clean, err)
	}
}

func legacyStatefulSet(withState bool) *appsv1.StatefulSet {
	set := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-csi-pnfs-mds-4f2c8a11", Namespace: ns}}
	if withState {
		set.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: legacyStateClaim}}}
	}
	return set
}

// U-63: the beta6 state is described and removed, and the step is done after.
func TestTheBeta6MetadataServerStateIsRemoved(t *testing.T) {
	class := "simplyblock-csi-4f2c8a11" + legacyStateSuffix
	s := scopeWith(t,
		legacyStatefulSet(true),
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "state-simplyblock-csi-pnfs-mds-4f2c8a11-0", Namespace: ns},
			Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To(class)}},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: class}},
		&admissionregistrationv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-csi" + legacyStateSuffix}},
		&admissionregistrationv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-csi" + legacyStateSuffix}},
	)
	st := pnfsMDSLegacyState{}
	ctx, subject := context.Background(), upgrade.TheUpgrade(ns)

	action, err := st.Describe(ctx, s, subject)
	if err != nil || action == nil || action.Verb != upgrade.VerbDelete {
		t.Fatalf("action = %+v, %v; want the legacy state deleted", action, err)
	}
	if err := st.Apply(ctx, s, subject); err != nil {
		t.Fatal(err)
	}
	if done, err := st.Done(ctx, s, subject); err != nil || !done {
		t.Errorf("done = %v, %v; want nothing left", done, err)
	}
	if err := s.Client.Get(ctx, client.ObjectKey{Name: "simplyblock-csi" + legacyStateSuffix},
		&admissionregistrationv1.ValidatingAdmissionPolicyBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("the binding is still there: %v", err)
	}
}

// U-64: a metadata server without the state template, and no metadata server
// at all, are nothing to do.
func TestAMetadataServerWithoutTheStateTemplateIsLeftAlone(t *testing.T) {
	for name, s := range map[string]*upgrade.Scope{
		"without the template": scopeWith(t, legacyStatefulSet(false)),
		"no metadata server":   scopeWith(t),
	} {
		action, err := pnfsMDSLegacyState{}.Describe(context.Background(), s, upgrade.TheUpgrade(ns))
		if err != nil || action != nil {
			t.Errorf("%s: action = %+v, %v; want nothing", name, action, err)
		}
	}
}

// U-65: only a cluster on v1 is switched.
func TestOnlyAClusterOnV1IsSwitched(t *testing.T) {
	v1 := cluster("old", "")
	v1.Status.WriteProtection = simplyblockv1alpha2.WriteProtectionV1
	v2 := cluster("new", "")
	v2.Status.WriteProtection = simplyblockv1alpha2.WriteProtectionV2

	action, err := writeProtectionV2{}.Describe(context.Background(), scopeWith(t, v1, v2), upgrade.TheUpgrade(ns))
	if err != nil || action == nil || !strings.Contains(action.Detail, "old") || strings.Contains(action.Detail, "new") {
		t.Errorf("action = %+v, %v; want the v1 cluster switched and the v2 one left", action, err)
	}
	none, err := writeProtectionV2{}.Describe(context.Background(), scopeWith(t, v2), upgrade.TheUpgrade(ns))
	if err != nil || none != nil {
		t.Errorf("action = %+v, %v; want nothing when every cluster is on v2", none, err)
	}
}
