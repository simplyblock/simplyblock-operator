// U-54 to U-58, U-63 to U-67, and U-75: what happens when the reconcile meets a
// deployment a Helm release installed.
//
// The objects are seeded carrying the labels and annotations a live 26.2.7
// release writes, because the whole question is what the controller does with
// somebody else's claim on an object that is serving volumes.

package driver

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// helmMeta is what a release leaves behind, measured from the live namespace.
func helmMeta() (map[string]string, map[string]string) {
	return map[string]string{
			"app.kubernetes.io/managed-by": "Helm",
			"chart":                        "simplyblock-operator",
			"chartVersion":                 "26.2.7",
			"heritage":                     "Helm",
			"release":                      "simplyblock-operator",
			"revision":                     "1",
		}, map[string]string{
			"meta.helm.sh/release-name":      "simplyblock-operator",
			"meta.helm.sh/release-namespace": "simplyblock",
		}
}

// chartInstalledNodeDaemonSet is the object a chart install leaves running,
// under the name the derivation produces.
func chartInstalledNodeDaemonSet(d *simplyblockv1alpha2.SimplyblockDriver, driverName string) *appsv1.DaemonSet {
	labels, annotations := helmMeta()
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        names(d).nodeDaemonSet,
			Namespace:   d.Namespace,
			Labels:      labels,
			Annotations: annotations,
			UID:         "the-uid-it-already-had",
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": names(d).nodeDaemonSet}},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "csi-registrar",
				Args: []string{"--kubelet-registration-path=" + nodeRegistrationPath(driverName)},
			}}}},
		},
	}
}

// U-54 and U-55: the object is taken over rather than recreated. A new UID is
// an object that was deleted and reapplied, which for the node DaemonSet means
// every node plugin in the cluster restarted at once.
func TestAdoptionKeepsTheObject(t *testing.T) {
	scheme := reconcilerScheme(t)
	d := testDriver("simplyblock")
	existing := chartInstalledNodeDaemonSet(d, DefaultDriverName)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d, existing).WithStatusSubresource(d).Build()
	r := &SimplyblockDriverReconciler{Client: c, Scheme: scheme}

	fromHelm, existed, err := r.inspectExisting(context.Background(),
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
			Name: names(d).nodeDaemonSet, Namespace: d.Namespace}})
	if err != nil {
		t.Fatalf("inspectExisting: %v", err)
	}
	if !existed {
		t.Error("the running DaemonSet was not seen")
	}
	if !fromHelm {
		t.Error("the Helm metadata on a live release's object was not recognized")
	}
}

// U-56 and U-57: origin records what the first reconcile met, and nothing else.
func TestOrigin(t *testing.T) {
	tests := []struct {
		name       string
		seeded     []client.Object
		wantOrigin simplyblockv1alpha2.SimplyblockDriverOrigin
	}{
		{
			name:       "an empty namespace",
			wantOrigin: simplyblockv1alpha2.SimplyblockDriverOriginCreated,
		},
		{
			name:       "a namespace holding the chart's objects",
			seeded:     []client.Object{nil}, // replaced below, needs the driver
			wantOrigin: simplyblockv1alpha2.SimplyblockDriverOriginAdopted,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := reconcilerScheme(t)
			d := testDriver("simplyblock")

			objects := []client.Object{d}
			if tc.seeded != nil {
				objects = append(objects, chartInstalledNodeDaemonSet(d, DefaultDriverName))
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(objects...).WithStatusSubresource(d).Build()
			r := &SimplyblockDriverReconciler{Client: c, Scheme: scheme}

			metExisting := tc.seeded != nil
			if err := r.recordOrigin(context.Background(), d, metExisting); err != nil {
				t.Fatalf("recordOrigin: %v", err)
			}

			var got simplyblockv1alpha2.SimplyblockDriver
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), &got); err != nil {
				t.Fatalf("re-read: %v", err)
			}
			if got.Status.Origin != tc.wantOrigin {
				t.Errorf("origin = %q, want %q", got.Status.Origin, tc.wantOrigin)
			}
		})
	}
}

// U-58: origin is decided once. A later reconcile that creates an object the
// deployment was missing does not turn an adopted deployment into a created
// one.
func TestOriginIsNotRevised(t *testing.T) {
	scheme := reconcilerScheme(t)
	d := testDriver("simplyblock")
	d.Status.Origin = simplyblockv1alpha2.SimplyblockDriverOriginAdopted

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d).WithStatusSubresource(d).Build()
	r := &SimplyblockDriverReconciler{Client: c, Scheme: scheme}

	if err := r.recordOrigin(context.Background(), d, false); err != nil {
		t.Fatalf("recordOrigin: %v", err)
	}

	var got simplyblockv1alpha2.SimplyblockDriver
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), &got); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.Status.Origin != simplyblockv1alpha2.SimplyblockDriverOriginAdopted {
		t.Errorf("origin = %q, want it to have stayed Adopted", got.Status.Origin)
	}
}

// U-66 and U-67: the one comparison adoption can refuse on, and the case it
// must not refuse.
func TestAdoptionRefusesOnADriverNameItCannotChange(t *testing.T) {
	tests := []struct {
		name          string
		runningDriver string
		specDriver    string
		wantRefused   bool
	}{
		{
			name:          "the names agree",
			runningDriver: DefaultDriverName,
			specDriver:    DefaultDriverName,
			wantRefused:   false,
		},
		{
			name:          "the deployment registered under another name",
			runningDriver: altDriverName,
			specDriver:    DefaultDriverName,
			wantRefused:   true,
		},
		{
			name:          "the spec names what is running, which is not the default",
			runningDriver: altDriverName,
			specDriver:    altDriverName,
			wantRefused:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := reconcilerScheme(t)
			d := testDriver("simplyblock")
			d.Spec.DriverName = tc.specDriver

			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(d, chartInstalledNodeDaemonSet(d, tc.runningDriver)).
				WithStatusSubresource(d).Build()
			r := &SimplyblockDriverReconciler{Client: c, Scheme: scheme}

			message, refused, err := r.adoptionRefusal(context.Background(), d)
			if err != nil {
				t.Fatalf("adoptionRefusal: %v", err)
			}
			if refused != tc.wantRefused {
				t.Fatalf("refused = %v, want %v (%s)", refused, tc.wantRefused, message)
			}
			if !refused {
				return
			}
			for _, want := range []string{tc.runningDriver, tc.specDriver} {
				if !contains(message, want) {
					t.Errorf("the refusal %q does not name %q", message, want)
				}
			}
		})
	}
}

// TLS's spec surface replaced an unconditional refusal (once-a-#432-style
// TestAdoptionRefusesConfigurationTheSpecCannotExpress case) with a
// comparison against spec.tls, the same shape as the driver-name check above:
// agreement adopts, disagreement in either direction refuses.
func TestAdoptionRefusesOnATLSModeMismatch(t *testing.T) {
	tests := []struct {
		name        string
		runningEnv  []corev1.EnvVar
		specTLS     simplyblockv1alpha2.DriverTLS
		wantRefused bool
	}{
		{
			name:        "both plaintext",
			wantRefused: false,
		},
		{
			name:        "running is TLS, spec says plaintext",
			runningEnv:  []corev1.EnvVar{{Name: "SB_TLS_CONNECT", Value: "anonymous"}},
			wantRefused: true,
		},
		{
			name:        "running is plaintext, spec asks for TLS",
			specTLS:     simplyblockv1alpha2.DriverTLS{EnableTLS: ptr.To(true)},
			wantRefused: true,
		},
		{
			name:        "both TLS, anonymous",
			runningEnv:  []corev1.EnvVar{{Name: "SB_TLS_CONNECT", Value: "anonymous"}},
			specTLS:     simplyblockv1alpha2.DriverTLS{EnableTLS: ptr.To(true)},
			wantRefused: false,
		},
		{
			name:       "running is TLS anonymous, spec asks for mutual",
			runningEnv: []corev1.EnvVar{{Name: "SB_TLS_CONNECT", Value: "anonymous"}},
			specTLS: simplyblockv1alpha2.DriverTLS{
				EnableTLS: ptr.To(true), EnableMutualTLS: ptr.To(true),
			},
			wantRefused: true,
		},
		{
			name:       "both TLS, mutual",
			runningEnv: []corev1.EnvVar{{Name: "SB_TLS_CONNECT", Value: "authenticated"}},
			specTLS: simplyblockv1alpha2.DriverTLS{
				EnableTLS: ptr.To(true), EnableMutualTLS: ptr.To(true),
			},
			wantRefused: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := reconcilerScheme(t)
			d := testDriver("simplyblock")
			d.Spec.TLS = tc.specTLS

			node := chartInstalledNodeDaemonSet(d, DefaultDriverName)
			node.Spec.Template.Spec.Containers[0].Env = tc.runningEnv

			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(d, node).WithStatusSubresource(d).Build()
			r := &SimplyblockDriverReconciler{Client: c, Scheme: scheme}

			message, refused, err := r.adoptionRefusal(context.Background(), d)
			if err != nil {
				t.Fatalf("adoptionRefusal: %v", err)
			}
			if refused != tc.wantRefused {
				t.Fatalf("refused = %v, want %v (%s)", refused, tc.wantRefused, message)
			}
		})
	}
}

// An empty namespace has nothing to refuse over.
func TestNothingToAdoptDoesNotRefuse(t *testing.T) {
	scheme := reconcilerScheme(t)
	d := testDriver("simplyblock")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d).WithStatusSubresource(d).Build()
	r := &SimplyblockDriverReconciler{Client: c, Scheme: scheme}

	if _, refused, err := r.adoptionRefusal(context.Background(), d); err != nil || refused {
		t.Fatalf("refused = %v, err = %v, want a fresh install to proceed", refused, err)
	}
}

// U-63 and U-64: the release's keys go and the resource policy stays, since
// what it says became permanently true.
func TestHelmMetadataRemovalPatch(t *testing.T) {
	patch := string(helmMetadataRemovalPatch())

	for _, key := range append(append([]string{}, helmLabels...), helmAnnotations...) {
		if !contains(patch, `"`+key+`":null`) {
			t.Errorf("the patch does not clear %q: %s", key, patch)
		}
	}
	if contains(patch, resourcePolicyAnnotation) {
		t.Errorf("the patch clears the resource policy, which has to outlive the release: %s", patch)
	}
}

// U-65: an object nobody claimed is not annotated on this controller's way past.
func TestOnlyHelmsObjectsAreKept(t *testing.T) {
	labels, annotations := helmMeta()

	fromHelm := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations}}
	if !carriesHelmMetadata(fromHelm) {
		t.Error("a release's object was not recognized")
	}

	ours := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{"app": "simplyblock-csi-node"}}}
	if carriesHelmMetadata(ours) {
		t.Error("an object this controller created reads as a release's")
	}

	// A release name alone is enough: an object may carry the annotation
	// without the label if somebody edited the labels.
	annotatedOnly := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{"meta.helm.sh/release-name": "simplyblock-operator"}}}
	if !carriesHelmMetadata(annotatedOnly) {
		t.Error("an object carrying only the release annotation was not recognized")
	}
}

func TestKeepThroughHelmIsAdditive(t *testing.T) {
	_, annotations := helmMeta()
	obj := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}

	keepThroughHelm(obj)

	if obj.Annotations[resourcePolicyAnnotation] != resourcePolicyKeep {
		t.Errorf("resource policy = %q, want keep", obj.Annotations[resourcePolicyAnnotation])
	}
	if obj.Annotations["meta.helm.sh/release-name"] == "" {
		t.Error("an existing annotation was dropped, and the strip has not run yet")
	}
}

// The driver name is read out of the registration path the running plugin was
// given, which is where it actually takes effect.
func TestRunningDriverName(t *testing.T) {
	d := testDriver("simplyblock")

	got, found := runningDriverName(chartInstalledNodeDaemonSet(d, altDriverName))
	if !found || got != altDriverName {
		t.Errorf("runningDriverName = %q, %v; want %q", got, found, altDriverName)
	}

	if _, found := runningDriverName(nil); found {
		t.Error("a missing DaemonSet reported a driver name")
	}

	bare := &appsv1.DaemonSet{Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "csi-registrar", Args: []string{"--v=5"}}}},
	}}}
	if _, found := runningDriverName(bare); found {
		t.Error("a registrar with no registration path reported a driver name")
	}
}

// A driver that never held the deployment takes nothing with it when it goes.
// The cluster-scoped names carry no namespace, so a second driver of the same
// name derives the holder's twelve exactly, and a finalizer on it would delete
// the running deployment's RBAC and registration.
func TestDeletingADuplicateLeavesTheHoldersObjects(t *testing.T) {
	scheme := reconcilerScheme(t)

	holder := testDriver("simplyblock")
	holder.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))

	duplicate := testDriver("simplyblock")
	duplicate.Namespace = "tenant-b"
	duplicate.CreationTimestamp = metav1.NewTime(time.Now())
	duplicate.Finalizers = []string{driverFinalizer}
	deleted := metav1.NewTime(time.Now())
	duplicate.DeletionTimestamp = &deleted

	// One object, derived identically by both, and marked by this controller
	// because the holder applied it.
	shared := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
		Name:   names(holder).clusterRole(nodeComponent),
		Labels: map[string]string{managedByLabel: managedByValue},
	}}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(holder, duplicate, shared).
		WithStatusSubresource(holder, duplicate).Build()
	r := &SimplyblockDriverReconciler{Client: c, Scheme: scheme}

	if err := r.finalize(context.Background(), duplicate); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	var survived rbacv1.ClusterRole
	if err := c.Get(context.Background(), client.ObjectKey{Name: shared.Name}, &survived); err != nil {
		t.Fatalf("the duplicate deleted the holder's %s: %v", shared.Name, err)
	}
}

// A deployment carrying configuration the spec has no field for is refused
// rather than reconciled into one that has lost it.
func TestAdoptionRefusesConfigurationTheSpecCannotExpress(t *testing.T) {
	tests := []struct {
		name string
		env  []corev1.EnvVar
		args []string
		want string
	}{
		{name: "csi-link", args: []string{"--link"}, want: "csi-link"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := reconcilerScheme(t)
			d := testDriver("simplyblock")
			node := chartInstalledNodeDaemonSet(d, DefaultDriverName)
			node.Spec.Template.Spec.Containers[0].Env = tc.env
			node.Spec.Template.Spec.Containers[0].Args = append(
				node.Spec.Template.Spec.Containers[0].Args, tc.args...)

			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(d, node).WithStatusSubresource(d).Build()
			r := &SimplyblockDriverReconciler{Client: c, Scheme: scheme}

			message, refused, err := r.adoptionRefusal(context.Background(), d)
			if err != nil {
				t.Fatalf("adoptionRefusal: %v", err)
			}
			if !refused {
				t.Fatalf("adopted a deployment configured with %s, which the spec cannot express", tc.want)
			}
			if !contains(message, tc.want) {
				t.Errorf("the refusal %q does not name %q", message, tc.want)
			}
		})
	}
}

// A plain deployment carries none of it and is adopted.
func TestAdoptionProceedsWithoutInexpressibleConfiguration(t *testing.T) {
	scheme := reconcilerScheme(t)
	d := testDriver("simplyblock")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(d, chartInstalledNodeDaemonSet(d, DefaultDriverName)).
		WithStatusSubresource(d).Build()
	r := &SimplyblockDriverReconciler{Client: c, Scheme: scheme}

	if message, refused, err := r.adoptionRefusal(context.Background(), d); err != nil || refused {
		t.Fatalf("refused = %v (%s), err = %v; want a plain deployment adopted", refused, message, err)
	}
}

// The snapshot class is the finalizer's whether or not the toggle currently
// asks for it, since one applied while snapshots were on is one nothing else
// would remove.
func TestFinalizerReachesADisabledSnapshotClass(t *testing.T) {
	scheme := reconcilerScheme(t)
	r := &SimplyblockDriverReconciler{Scheme: scheme}

	off := false
	d := testDriver("simplyblock")
	d.Spec.EnableVolumeSnapshots = &off

	found := false
	for _, obj := range r.ownedClusterScoped(d) {
		if obj.GetName() == names(d).snapshotClass {
			found = true
		}
		if obj.GetNamespace() != "" {
			t.Errorf("%s is namespaced and the garbage collector already has it", obj.GetName())
		}
	}
	if !found {
		t.Error("a snapshot class applied before the toggle went false would outlive the object")
	}
}
