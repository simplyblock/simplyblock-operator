// Tests for spec.serviceClasses: what the steady-state sync sends to the
// control plane, when it sends nothing, and what it reports.

package cluster

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	atlascp "github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func withServiceClasses(c *simplyblockv1alpha2.StorageCluster) {
	c.Spec.ServiceClasses = &simplyblockv1alpha2.ServiceClassesSpec{
		DefaultWeight: ptr.To(int32(50)),
		Classes: []simplyblockv1alpha2.ServiceClass{
			{Name: "gold", Weight: 30},
			{Name: "bronze", Weight: 5},
		},
		ReplicationShare: ptr.To(int32(15)),
	}
}

func appliedConfig() ServiceClassConfig {
	return ServiceClassConfig{
		DefaultWeight: 50,
		// The control plane's order, which is by id rather than the spec's.
		Classes: []ServiceClassWire{
			{ID: 1, Name: "bronze", Weight: 5},
			{ID: 2, Name: "gold", Weight: 30},
		},
		ReplicationWeight: 15,
	}
}

func TestServiceClassesThatDifferAreApplied(t *testing.T) {
	current := ServiceClassConfig{DefaultWeight: 100, ReplicationWeight: 10}
	var sent []ServiceClassConfig
	api := &fakeControlPlane{
		cluster:        func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		serviceClasses: func(string) (ServiceClassConfig, error) { return current, nil },
		setServiceClasses: func(_ string, cfg ServiceClassConfig) error {
			sent = append(sent, cfg)
			current = appliedConfig()
			return nil
		},
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec, newTestCluster(withServiceClasses))

	cluster := reconcileCluster(t, r, 1)

	if len(sent) != 1 {
		t.Fatalf("the control plane was sent %d configurations, want 1", len(sent))
	}
	got := sent[0]
	if got.DefaultWeight != 50 || got.ReplicationWeight != 15 || len(got.Classes) != 2 {
		t.Errorf("sent %+v, want default 50, replication 15, and two classes", got)
	}
	if !rec.has(ServiceClassesApplied) {
		t.Error("an applied change emitted no ServiceClassesApplied")
	}
	want := map[string]int32{"gold": 2, "bronze": 1}
	if len(cluster.Status.ServiceClasses) != len(want) {
		t.Fatalf("status.serviceClasses = %+v, want the two classes", cluster.Status.ServiceClasses)
	}
	for _, c := range cluster.Status.ServiceClasses {
		if want[c.Name] != c.ID {
			t.Errorf("status reports %s as id %d, want %d", c.Name, c.ID, want[c.Name])
		}
	}
}

func TestServiceClassesThatMatchAreNotRewritten(t *testing.T) {
	api := &fakeControlPlane{
		cluster:        func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		serviceClasses: func(string) (ServiceClassConfig, error) { return appliedConfig(), nil },
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec, newTestCluster(withServiceClasses))

	reconcileCluster(t, r, 2)

	if api.setServiceClassesCalls != 0 {
		t.Errorf("a matching configuration was sent %d times", api.setServiceClassesCalls)
	}
	if rec.has(ServiceClassesApplied) {
		t.Error("nothing changed and ServiceClassesApplied was emitted")
	}
}

func TestOmittedDefaultsAreTheDocumentedOnes(t *testing.T) {
	var sent ServiceClassConfig
	api := &fakeControlPlane{
		cluster:        func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		serviceClasses: func(string) (ServiceClassConfig, error) { return ServiceClassConfig{}, nil },
		setServiceClasses: func(_ string, cfg ServiceClassConfig) error {
			sent = cfg
			return nil
		},
	}
	r := newClusterReconciler(t, api, &recorder{}, newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.ServiceClasses = &simplyblockv1alpha2.ServiceClassesSpec{}
	}))

	reconcileCluster(t, r, 1)

	if sent.DefaultWeight != 100 || sent.ReplicationWeight != 10 {
		t.Errorf("sent default %d and replication %d, want 100 and 10",
			sent.DefaultWeight, sent.ReplicationWeight)
	}
	if sent.Classes == nil {
		t.Error("no classes was sent as null; the control plane reads an empty list as remove all")
	}
}

func TestAClusterWithoutServiceClassesLeavesTheControlPlaneAlone(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
	}
	r := newClusterReconciler(t, api, &recorder{}, newTestCluster())

	reconcileCluster(t, r, 1)

	if api.serviceClassesCalls != 0 || api.setServiceClassesCalls != 0 {
		t.Errorf("read %d and wrote %d times with spec.serviceClasses unset",
			api.serviceClassesCalls, api.setServiceClassesCalls)
	}
}

func TestARefusedServiceClassChangeCarriesTheControlPlanesReason(t *testing.T) {
	api := &fakeControlPlane{
		cluster:        func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		serviceClasses: func(string) (ServiceClassConfig, error) { return ServiceClassConfig{}, nil },
		setServiceClasses: func(string, ServiceClassConfig) error {
			return &ControlPlaneError{Status: http.StatusConflict, Body: "classes still used by volumes: silver"}
		},
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec, newTestCluster(withServiceClasses))

	cluster := reconcileCluster(t, r, 1)

	if rec.typeFor(ServiceClassesRefused) != corev1.EventTypeWarning {
		t.Fatal("a refused change emitted no ServiceClassesRefused warning")
	}
	if note := rec.noteFor(ServiceClassesRefused); !strings.Contains(note, "silver") {
		t.Errorf("the event does not carry the control plane's reason: %q", note)
	}
	if cluster.Status.Phase != simplyblockv1alpha2.StorageClusterPhaseOnline {
		t.Errorf("a refused class change moved the cluster to %s", cluster.Status.Phase)
	}
}

func TestAControlPlaneWithoutServiceClassesIsReported(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		serviceClasses: func(string) (ServiceClassConfig, error) {
			return ServiceClassConfig{}, &ControlPlaneError{Status: http.StatusNotFound, Body: "Not Found"}
		},
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec, newTestCluster(withServiceClasses))

	reconcileCluster(t, r, 1)

	if !rec.has(ServiceClassesUnsupported) {
		t.Error("a control plane without the endpoint emitted no ServiceClassesUnsupported")
	}
	if api.setServiceClassesCalls != 0 {
		t.Error("the operator wrote to an endpoint the control plane does not have")
	}
}

func TestStorageClusterServiceClassesAdmission(t *testing.T) {
	apiClient := apiServer(t)

	classes := func(n int) []simplyblockv1alpha2.ServiceClass {
		out := make([]simplyblockv1alpha2.ServiceClass, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, simplyblockv1alpha2.ServiceClass{Name: fmt.Sprintf("c%d", i), Weight: 1})
		}
		return out
	}
	for _, tc := range []struct {
		name       string
		spec       simplyblockv1alpha2.ServiceClassesSpec
		wantDenied string
	}{
		{name: "five classes and a share", spec: simplyblockv1alpha2.ServiceClassesSpec{
			Classes: classes(5), ReplicationShare: ptr.To(int32(60))}},
		{name: "a sixth class", spec: simplyblockv1alpha2.ServiceClassesSpec{Classes: classes(6)},
			wantDenied: "must have at most 5 items"},
		{name: "a class named Default", spec: simplyblockv1alpha2.ServiceClassesSpec{
			Classes: []simplyblockv1alpha2.ServiceClass{{Name: "Default", Weight: 1}}},
			wantDenied: "Default is the built-in class"},
		{name: "a share above the ceiling", spec: simplyblockv1alpha2.ServiceClassesSpec{
			ReplicationShare: ptr.To(int32(75))}, wantDenied: "less than or equal to 74"},
		{name: "a zero weight", spec: simplyblockv1alpha2.ServiceClassesSpec{
			Classes: []simplyblockv1alpha2.ServiceClass{{Name: "gold"}}},
			wantDenied: "greater than or equal to 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.spec
			cluster := &simplyblockv1alpha2.StorageCluster{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "svcclass-", Namespace: "default"},
				Spec: simplyblockv1alpha2.StorageClusterSpec{
					MaxSubsystemCount: ptr.To(int32(10)),
					VCPUCount:         ptr.To(int32(4)),
					ServiceClasses:    &spec,
				},
			}
			err := apiClient.Create(context.Background(), cluster)
			if tc.wantDenied == "" {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				if got := cluster.Spec.ServiceClasses.DefaultWeight; got == nil || *got != 100 {
					t.Errorf("defaultWeight = %v, want the default of 100", got)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.wantDenied) {
				t.Fatalf("rejected for the wrong reason: %v", err)
			}
		})
	}
}
