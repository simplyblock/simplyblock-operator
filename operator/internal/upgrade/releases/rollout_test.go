// The generic checks and rollout steps against a fake client: what they find,
// what they create, and that a rerun skips what an earlier run finished
// (design-release-upgrade.md §6.5 and §8, test plan U-47 and U-52 to U-57).

package releases

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/simplyblock/atlas/crdtypes"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

const ns = "simplyblock"

func scopeWith(t *testing.T, objects ...client.Object) *upgrade.Scope {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, apiextensionsv1.AddToScheme, simplyblockv1alpha2.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&simplyblockv1alpha2.StorageClusterOps{}, &simplyblockv1alpha2.ControlPlaneOps{}).Build()
	return upgrade.NewScope(c, ns, upgrade.StageReleaseRollout, upgrade.Options{}, logr.Discard(), nil)
}

func aCluster(name string, phase simplyblockv1alpha2.StorageClusterPhase) *simplyblockv1alpha2.StorageCluster {
	c := &simplyblockv1alpha2.StorageCluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	c.Status.Phase = phase
	return c
}

func aNode(name, cluster string, phase simplyblockv1alpha2.StorageNodePhase) *simplyblockv1alpha2.StorageNode {
	n := &simplyblockv1alpha2.StorageNode{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: simplyblockv1alpha2.StorageNodeSpec{ClusterRef: cluster}}
	n.Status.Phase = phase
	n.Status.UUID = name + "-uuid"
	return n
}

func run(t *testing.T, s *upgrade.Scope, check func(context.Context, *upgrade.Scope) (upgrade.Findings, error)) upgrade.Findings {
	t.Helper()
	f, err := check(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// U-47: a cluster not online, a node not online, and a running operation each
// refuse the upgrade. A healthy deployment passes.
func TestTheGenericChecksRefuseAnUnhealthyDeployment(t *testing.T) {
	running := &simplyblockv1alpha2.StorageClusterOps{ObjectMeta: metav1.ObjectMeta{Name: "busy", Namespace: ns}}
	running.Status.Phase = simplyblockv1alpha2.StorageClusterOpsPhaseRunning
	s := scopeWith(t,
		aCluster("degraded", simplyblockv1alpha2.StorageClusterPhaseDegraded),
		aNode("down", "degraded", simplyblockv1alpha2.StorageNodePhaseOffline),
		running)

	for name, check := range map[string]func(context.Context, *upgrade.Scope) (upgrade.Findings, error){
		"clusters": clustersHealthy, "nodes": nodesOnline, "in flight": nothingInFlight,
	} {
		if f := run(t, s, check); !f.Blocked() {
			t.Errorf("%s: no refusal on an unhealthy deployment", name)
		}
	}

	healthy := scopeWith(t,
		aCluster("ok", simplyblockv1alpha2.StorageClusterPhaseOnline),
		aNode("up", "ok", simplyblockv1alpha2.StorageNodePhaseOnline))
	for name, check := range map[string]func(context.Context, *upgrade.Scope) (upgrade.Findings, error){
		"clusters": clustersHealthy, "nodes": nodesOnline, "in flight": nothingInFlight,
	} {
		if f := run(t, healthy, check); len(f) != 0 {
			t.Errorf("%s: %v on a healthy deployment", name, f)
		}
	}
}

// U-44: a deployment storing v1alpha1 is sent to the API migration first.
func TestAV1Alpha1StorageVersionIsRefused(t *testing.T) {
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "storageclusters.storage.simplyblock.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
			{Name: "v1alpha1", Storage: true}, {Name: "v1alpha2"},
		}},
	}
	if f := run(t, scopeWith(t, crd), noV1Alpha1Storage); !f.Blocked() {
		t.Error("a v1alpha1 storage version was not refused")
	}
	crd.Spec.Versions[0].Storage, crd.Spec.Versions[1].Storage = false, true
	if f := run(t, scopeWith(t, crd), noV1Alpha1Storage); len(f) != 0 {
		t.Errorf("%v on a v1alpha2 deployment", f)
	}
}

func fastOptions() Options {
	return Options{SimplyblockImage: "quay.io/simplyblock-io/simplyblock:26.4.0-beta7",
		Poll: time.Millisecond, Timeout: 50 * time.Millisecond, Settle: time.Nanosecond}
}

// sourceControlPlane is a local control plane still on the source release.
func sourceControlPlane() *simplyblockv1alpha2.ControlPlane {
	const image crdtypes.ImageReference = "quay.io/simplyblock-io/simplyblock:26.4.0-beta6.1"
	cp := &simplyblockv1alpha2.ControlPlane{ObjectMeta: metav1.ObjectMeta{Name: controlPlaneName, Namespace: ns}}
	cp.Spec.Source.Local = &simplyblockv1alpha2.LocalControlPlane{Image: image}
	return cp
}

// U-52: no operation yet is a control-plane upgrade to create. U-53: one that
// succeeded is skipped.
func TestTheControlPlaneUpgradeIsCreatedOnceAndThenSkipped(t *testing.T) {
	target := MustVersion("26.4.0-beta7")
	st := upgradeControlPlane(target, fastOptions())
	s := scopeWith(t, sourceControlPlane())

	action, err := st.Describe(context.Background(), s, upgrade.TheUpgrade(ns))
	if err != nil || action == nil || action.Verb != upgrade.VerbCreate {
		t.Fatalf("action = %+v, %v; want the operation to create", action, err)
	}

	done := &simplyblockv1alpha2.ControlPlaneOps{ObjectMeta: metav1.ObjectMeta{Name: OperationName(target), Namespace: ns}}
	done.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseSucceeded
	s = scopeWith(t, sourceControlPlane(), done)
	if action, err := st.Describe(context.Background(), s, upgrade.TheUpgrade(ns)); err != nil || action != nil {
		t.Errorf("action = %+v, %v; a succeeded upgrade is skipped on a rerun", action, err)
	}
}

// U-55: a failed operation stops the rollout with its message.
func TestAFailedOperationStopsTheRollout(t *testing.T) {
	target := MustVersion("26.4.0-beta7")
	failed := &simplyblockv1alpha2.ControlPlaneOps{ObjectMeta: metav1.ObjectMeta{Name: OperationName(target), Namespace: ns}}
	failed.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseFailed
	failed.Status.Message = "the control plane did not come up"
	s := scopeWith(t, sourceControlPlane(), failed)

	err := upgradeControlPlane(target, fastOptions()).Apply(context.Background(), s, upgrade.TheUpgrade(ns))
	if err == nil || !strings.Contains(err.Error(), "did not come up") {
		t.Errorf("err = %v, want the operation's failure carried", err)
	}
}

// The control-plane step refuses to run without the target image.
func TestTheControlPlaneUpgradeNeedsTheTargetImage(t *testing.T) {
	s := scopeWith(t, sourceControlPlane())
	err := upgradeControlPlane(MustVersion("26.4.0-beta7"), Options{}).Validate(context.Background(), s, upgrade.TheUpgrade(ns))
	if err == nil || !strings.Contains(err.Error(), "--simplyblock-image") {
		t.Errorf("err = %v, want the missing flag named", err)
	}
}

// U-57: no clusters is nothing to roll.
func TestNoClustersIsNothingToRoll(t *testing.T) {
	action, err := rollStoragePlane(MustVersion("26.4.0-beta7"), fastOptions()).
		Describe(context.Background(), scopeWith(t), upgrade.TheUpgrade(ns))
	if err != nil || action != nil {
		t.Errorf("action = %+v, %v; want nothing", action, err)
	}
}

// The storage plane is not rolled before every node has resolved its desired
// image against what its cluster reports, and with --spdk-image not before the
// cluster reports that image.
func TestTheStoragePlaneWaitsForTheClustersToReportTheTarget(t *testing.T) {
	target := MustVersion("26.4.0-beta7")
	o := fastOptions()
	o.SpdkImage = "quay.io/simplyblock-io/ultra:26.4.0-beta7"

	cluster := aCluster("prod", simplyblockv1alpha2.StorageClusterPhaseOnline)
	cluster.Status.DefaultSpdkImage = "quay.io/simplyblock-io/ultra:26.4.0-beta6.1"
	node := aNode("n1", "prod", simplyblockv1alpha2.StorageNodePhaseOnline)
	node.Status.DesiredSpdkImage = cluster.Status.DefaultSpdkImage

	ok, why, err := clusterSettled(context.Background(), scopeWith(t, cluster, node), target, o, "prod")
	if err != nil || ok || !strings.Contains(why, "default SPDK image") {
		t.Errorf("settled = %v, %q, %v; want it waiting for the cluster to report the target", ok, why, err)
	}

	cluster.Status.DefaultSpdkImage = o.SpdkImage
	ok, why, err = clusterSettled(context.Background(), scopeWith(t, cluster, node), target, o, "prod")
	if err != nil || ok || !strings.Contains(why, "n1") {
		t.Errorf("settled = %v, %q, %v; want it waiting for the node to resolve", ok, why, err)
	}

	node.Status.DesiredSpdkImage = o.SpdkImage
	if ok, why, err := clusterSettled(context.Background(), scopeWith(t, cluster, node), target, o, "prod"); err != nil || !ok {
		t.Errorf("settled = %v, %q, %v; want it settled", ok, why, err)
	}
}

// A rolling restart that succeeded is not created again.
func TestASucceededRollingRestartIsSkipped(t *testing.T) {
	target := MustVersion("26.4.0-beta7")
	done := &simplyblockv1alpha2.StorageClusterOps{ObjectMeta: metav1.ObjectMeta{
		Name: OperationName(target, "prod", "restart"), Namespace: ns}}
	done.Status.Phase = simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded
	s := scopeWith(t, aCluster("prod", simplyblockv1alpha2.StorageClusterPhaseOnline), done)

	action, err := rollStoragePlane(target, fastOptions()).Describe(context.Background(), s, upgrade.TheUpgrade(ns))
	if err != nil || action != nil {
		t.Errorf("action = %+v, %v; a succeeded walk is skipped on a rerun", action, err)
	}
}

// The operator step reads the image tag and the rollout state.
func TestTheOperatorIsReadyAtTheTargetOnly(t *testing.T) {
	target := MustVersion("26.4.0-beta7")
	deployment := func(tag string, available int32) *appsv1.Deployment {
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: operatorDeployment, Namespace: ns}}
		d.Spec.Template.Spec.Containers = []corev1.Container{{Name: operatorContainer,
			Image: "quay.io/simplyblock-io/simplyblock-operator:" + tag}}
		d.Status.UpdatedReplicas, d.Status.AvailableReplicas = available, available
		return d
	}
	st := awaitOperator(target, fastOptions())
	for _, tc := range []struct {
		tag       string
		available int32
		ready     bool
	}{
		{"v26.4.0-beta6.2", 1, false},
		{"v26.4.0-beta7", 0, false},
		{"v26.4.0-beta7", 1, true},
	} {
		action, err := st.Describe(context.Background(), scopeWith(t, deployment(tc.tag, tc.available)), upgrade.TheUpgrade(ns))
		if err != nil || (action == nil) != tc.ready {
			t.Errorf("%s with %d available: action = %+v, %v; want ready=%v", tc.tag, tc.available, action, err, tc.ready)
		}
	}
}

func TestOperationNamesCarryTheRelease(t *testing.T) {
	if got := OperationName(MustVersion("26.4.0-beta7"), "prod", "restart"); got != "upgrade-26-4-0-beta7-prod-restart" {
		t.Errorf("name = %q", got)
	}
	if got := imageTag("registry:5000/ns/image:v1.2.3@sha256:abc"); got != "v1.2.3" {
		t.Errorf("tag = %q", got)
	}
	if got := imageTag("registry:5000/ns/image@sha256:abc"); got != "" {
		t.Errorf("tag of a digest-only reference = %q, want none", got)
	}
}
