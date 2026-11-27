// The update rules of the generated CRDs, exercised against a real API server:
// which spec changes after creation the schema refuses. It lives next to the
// CRDs because it tests what they declare, whichever controller reads the
// field.

package bases

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

var (
	ruleEnvOnce   sync.Once
	ruleEnvClient client.Client
	ruleEnvErr    error
)

// ruleServer starts one API server with every CRD in this directory installed.
func ruleServer(t *testing.T) client.Client {
	t.Helper()
	ruleEnvOnce.Do(func() {
		scheme := runtime.NewScheme()
		if ruleEnvErr = simplyblockv1alpha2.AddToScheme(scheme); ruleEnvErr != nil {
			return
		}
		env := &envtest.Environment{
			CRDDirectoryPaths:     []string{"."},
			ErrorIfCRDPathMissing: true,
			BinaryAssetsDirectory: envtestAssets(),
		}
		cfg, err := env.Start()
		if err != nil {
			ruleEnvErr = err
			return
		}
		ruleEnvClient, ruleEnvErr = client.New(cfg, client.Options{Scheme: scheme})
	})
	if ruleEnvErr != nil {
		t.Fatalf("starting the test API server: %v", ruleEnvErr)
	}
	return ruleEnvClient
}

// envtestAssets is KUBEBUILDER_ASSETS when the Makefile sets it, else the first
// version under the repository-root .bin, so the test also runs from an editor.
func envtestAssets() string {
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
		return dir
	}
	matches, _ := filepath.Glob(filepath.Join("..", "..", "..", "..", ".bin", "k8s", "*"))
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}

// transition is one update after creation and whether the schema refuses it.
type transition struct {
	name   string
	create client.Object
	mutate func(client.Object)
	// denied is a substring of the refusal; empty means the update is admitted.
	denied string
}

func runTransitions(t *testing.T, cases []transition) {
	t.Helper()
	c := ruleServer(t)
	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := tc.create.DeepCopyObject().(client.Object)
			obj.SetNamespace("default")
			obj.SetGenerateName("rule-")
			if err := c.Create(ctx, obj); err != nil {
				t.Fatalf("create: %v", err)
			}
			tc.mutate(obj)
			err := c.Update(ctx, obj)
			switch {
			case tc.denied == "" && err != nil:
				t.Fatalf("update refused, want it admitted: %v", err)
			case tc.denied != "" && err == nil:
				t.Fatalf("update admitted, want it refused with %q", tc.denied)
			case tc.denied != "" && !strings.Contains(err.Error(), tc.denied):
				t.Fatalf("update refused with %v, want %q", err, tc.denied)
			}
		})
	}
}

func nodeOps(action simplyblockv1alpha2.StorageNodeOpsAction,
	spec func(*simplyblockv1alpha2.StorageNodeOpsSpec)) *simplyblockv1alpha2.StorageNodeOps {
	ops := &simplyblockv1alpha2.StorageNodeOps{
		Spec: simplyblockv1alpha2.StorageNodeOpsSpec{NodeRef: "node-1", Action: action},
	}
	if spec != nil {
		spec(&ops.Spec)
	}
	return ops
}

func asNodeOps(f func(*simplyblockv1alpha2.StorageNodeOpsSpec)) func(client.Object) {
	return func(o client.Object) { f(&o.(*simplyblockv1alpha2.StorageNodeOps).Spec) }
}

func TestStorageNodeOpsUpdateRules(t *testing.T) {
	withPattern := func(pattern string) func(*simplyblockv1alpha2.StorageNodeOpsSpec) {
		return func(s *simplyblockv1alpha2.StorageNodeOpsSpec) {
			s.Remove = &simplyblockv1alpha2.RemoveSpec{SystemVolumeFilterRegex: ptr.To(pattern)}
		}
	}
	withTarget := func(worker string, drives ...string) func(*simplyblockv1alpha2.StorageNodeOpsSpec) {
		return func(s *simplyblockv1alpha2.StorageNodeOpsSpec) {
			s.Migrate = &simplyblockv1alpha2.MigrateSpec{TargetWorkerNode: worker, NewSsdPcie: drives}
		}
	}
	runTransitions(t, []transition{
		{
			name:   "widening the system-volume pattern of a removal",
			create: nodeOps(simplyblockv1alpha2.StorageNodeOpsActionRemove, withPattern("^sys-")),
			mutate: asNodeOps(withPattern(".*")),
			denied: "spec.remove is immutable",
		},
		{
			name:   "dropping the parameters of a removal",
			create: nodeOps(simplyblockv1alpha2.StorageNodeOpsActionRemove, withPattern("^sys-")),
			mutate: asNodeOps(func(s *simplyblockv1alpha2.StorageNodeOpsSpec) { s.Remove = nil }),
			denied: "spec.remove is immutable",
		},
		{
			name:   "adding removal parameters to a running removal",
			create: nodeOps(simplyblockv1alpha2.StorageNodeOpsActionRemove, nil),
			mutate: asNodeOps(withPattern(".*")),
			denied: "spec.remove is immutable",
		},
		{
			name:   "changing the drives a migration binds",
			create: nodeOps(simplyblockv1alpha2.StorageNodeOpsActionMigrate, withTarget("worker-2", "0000:01:00.0")),
			mutate: asNodeOps(withTarget("worker-2", "0000:02:00.0")),
			denied: "spec.migrate is immutable",
		},
		{
			name:   "dropping the parameters of a migration",
			create: nodeOps(simplyblockv1alpha2.StorageNodeOpsActionMigrate, withTarget("worker-2")),
			mutate: asNodeOps(func(s *simplyblockv1alpha2.StorageNodeOpsSpec) { s.Migrate = nil }),
			denied: "spec.migrate is immutable",
		},
		{
			name:   "forcing a restart after creation",
			create: nodeOps(simplyblockv1alpha2.StorageNodeOpsActionRestart, nil),
			mutate: asNodeOps(func(s *simplyblockv1alpha2.StorageNodeOpsSpec) { s.Force = ptr.To(true) }),
			denied: "spec.force is immutable",
		},
		{
			name: "withdrawing the force of a restart",
			create: nodeOps(simplyblockv1alpha2.StorageNodeOpsActionRestart,
				func(s *simplyblockv1alpha2.StorageNodeOpsSpec) { s.Force = ptr.To(true) }),
			mutate: asNodeOps(func(s *simplyblockv1alpha2.StorageNodeOpsSpec) { s.Force = ptr.To(false) }),
			denied: "spec.force is immutable",
		},
		{
			name:   "asking a restart to reattach volumes after creation",
			create: nodeOps(simplyblockv1alpha2.StorageNodeOpsActionRestart, nil),
			mutate: asNodeOps(func(s *simplyblockv1alpha2.StorageNodeOpsSpec) { s.ReattachVolume = ptr.To(true) }),
			denied: "spec.reattachVolume is immutable",
		},
		{
			name:   "aborting a removal",
			create: nodeOps(simplyblockv1alpha2.StorageNodeOpsActionRemove, withPattern("^sys-")),
			mutate: asNodeOps(func(s *simplyblockv1alpha2.StorageNodeOpsSpec) { s.Abort = true }),
		},
	})
}

func clusterOps(action simplyblockv1alpha2.StorageClusterOpsAction,
	spec func(*simplyblockv1alpha2.StorageClusterOpsSpec)) *simplyblockv1alpha2.StorageClusterOps {
	ops := &simplyblockv1alpha2.StorageClusterOps{
		Spec: simplyblockv1alpha2.StorageClusterOpsSpec{ClusterRef: "cluster-1", Action: action},
	}
	if spec != nil {
		spec(&ops.Spec)
	}
	return ops
}

func asClusterOps(f func(*simplyblockv1alpha2.StorageClusterOpsSpec)) func(client.Object) {
	return func(o client.Object) { f(&o.(*simplyblockv1alpha2.StorageClusterOps).Spec) }
}

func TestStorageClusterOpsUpdateRules(t *testing.T) {
	withTask := func(id string) func(*simplyblockv1alpha2.StorageClusterOpsSpec) {
		return func(s *simplyblockv1alpha2.StorageClusterOpsSpec) {
			s.CancelTask = &simplyblockv1alpha2.CancelTaskSpec{TaskID: id}
		}
	}
	runTransitions(t, []transition{
		{
			name:   "dropping the task a cancel waits on",
			create: clusterOps(simplyblockv1alpha2.StorageClusterOpsActionCancelTask, withTask("task-1")),
			mutate: asClusterOps(func(s *simplyblockv1alpha2.StorageClusterOpsSpec) { s.CancelTask = nil }),
			denied: "spec.cancelTask is immutable",
		},
		{
			name: "toggling the pod refresh of a rolling restart mid-walk",
			create: clusterOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart,
				func(s *simplyblockv1alpha2.StorageClusterOpsSpec) {
					s.RollingRestart = &simplyblockv1alpha2.RollingRestartSpec{RefreshSNodeAPI: true}
				}),
			mutate: asClusterOps(func(s *simplyblockv1alpha2.StorageClusterOpsSpec) {
				s.RollingRestart = &simplyblockv1alpha2.RollingRestartSpec{}
			}),
			denied: "spec.rollingRestart is immutable",
		},
		{
			name:   "aborting a cancel",
			create: clusterOps(simplyblockv1alpha2.StorageClusterOpsActionCancelTask, withTask("task-1")),
			mutate: asClusterOps(func(s *simplyblockv1alpha2.StorageClusterOpsSpec) { s.Abort = true }),
		},
	})
}

func TestStorageBackupOpsUpdateRules(t *testing.T) {
	restoreOps := func(restore *simplyblockv1alpha2.RestoreSpec) *simplyblockv1alpha2.StorageBackupOps {
		return &simplyblockv1alpha2.StorageBackupOps{Spec: simplyblockv1alpha2.StorageBackupOpsSpec{
			ClusterRef: simplyblockv1alpha2.NamespacedDnsReference{Name: "cluster-1"},
			BackupRef:  simplyblockv1alpha2.NamespacedReference{Name: "backup-1"},
			Action:     simplyblockv1alpha2.StorageBackupOpsActionRestore,
			Restore:    restore,
		}}
	}
	asOps := func(f func(*simplyblockv1alpha2.StorageBackupOpsSpec)) func(client.Object) {
		return func(o client.Object) { f(&o.(*simplyblockv1alpha2.StorageBackupOps).Spec) }
	}
	target := &simplyblockv1alpha2.RestoreSpec{ClaimName: "restored", TargetPool: "pool-1"}
	runTransitions(t, []transition{
		{
			name:   "dropping the parameters of a running restore",
			create: restoreOps(target),
			mutate: asOps(func(s *simplyblockv1alpha2.StorageBackupOpsSpec) { s.Restore = nil }),
			denied: "spec.restore is immutable",
		},
		{
			name:   "labelling the restored claim after creation",
			create: restoreOps(target),
			mutate: asOps(func(s *simplyblockv1alpha2.StorageBackupOpsSpec) {
				s.Restore.ClaimLabels = map[string]string{"team": "storage"}
			}),
			denied: "spec.restore is immutable",
		},
		{
			name:   "aborting a restore",
			create: restoreOps(target),
			mutate: asOps(func(s *simplyblockv1alpha2.StorageBackupOpsSpec) { s.Abort = true }),
		},
	})
}

func TestControlPlaneUpdateRules(t *testing.T) {
	plane := func(fdb *simplyblockv1alpha2.FoundationDBSpec) *simplyblockv1alpha2.ControlPlane {
		return &simplyblockv1alpha2.ControlPlane{Spec: simplyblockv1alpha2.ControlPlaneSpec{
			Source: simplyblockv1alpha2.ControlPlaneSource{Local: &simplyblockv1alpha2.LocalControlPlane{
				Image: "registry.example/simplyblock:1", FoundationDB: fdb,
			}},
		}}
	}
	withFDB := func(fdb *simplyblockv1alpha2.FoundationDBSpec) func(client.Object) {
		return func(o client.Object) { o.(*simplyblockv1alpha2.ControlPlane).Spec.Source.Local.FoundationDB = fdb }
	}
	runTransitions(t, []transition{
		{
			name:   "moving the database to another storage class",
			create: plane(&simplyblockv1alpha2.FoundationDBSpec{StorageClassName: "gp3"}),
			mutate: withFDB(&simplyblockv1alpha2.FoundationDBSpec{StorageClassName: "io2"}),
			denied: "storageClassName is immutable",
		},
		{
			name:   "naming a class for a database created on the default one",
			create: plane(nil),
			mutate: withFDB(&simplyblockv1alpha2.FoundationDBSpec{StorageClassName: "gp3"}),
			denied: "storageClassName is immutable",
		},
		{
			name:   "resizing the database",
			create: plane(&simplyblockv1alpha2.FoundationDBSpec{StorageClassName: "gp3"}),
			mutate: withFDB(&simplyblockv1alpha2.FoundationDBSpec{StorageClassName: "gp3", Replicas: ptr.To(int32(5))}),
		},
	})
}

func TestOperatorOpsUpdateRules(t *testing.T) {
	discovery := func() *simplyblockv1alpha2.OperatorOps {
		return &simplyblockv1alpha2.OperatorOps{Spec: simplyblockv1alpha2.OperatorOpsSpec{
			Action:   simplyblockv1alpha2.OperatorOpsActionDiscover,
			Discover: &simplyblockv1alpha2.DiscoverSpec{Workers: []string{"worker-1"}},
		}}
	}
	asOps := func(f func(*simplyblockv1alpha2.OperatorOpsSpec)) func(client.Object) {
		return func(o client.Object) { f(&o.(*simplyblockv1alpha2.OperatorOps).Spec) }
	}
	runTransitions(t, []transition{
		{
			name:   "pointing a discovery at other workers",
			create: discovery(),
			mutate: asOps(func(s *simplyblockv1alpha2.OperatorOpsSpec) { s.Discover.Workers = []string{"worker-9"} }),
			denied: "spec.discover is immutable",
		},
		{
			name:   "dropping the parameters of a discovery",
			create: discovery(),
			mutate: asOps(func(s *simplyblockv1alpha2.OperatorOpsSpec) { s.Discover = nil }),
			denied: "spec.discover is immutable",
		},
		{
			name: "adding parameters to a discovery created without them",
			create: &simplyblockv1alpha2.OperatorOps{Spec: simplyblockv1alpha2.OperatorOpsSpec{
				Action: simplyblockv1alpha2.OperatorOpsActionDiscover,
			}},
			mutate: asOps(func(s *simplyblockv1alpha2.OperatorOpsSpec) {
				s.Discover = &simplyblockv1alpha2.DiscoverSpec{Workers: []string{"worker-9"}}
			}),
			denied: "spec.discover is immutable",
		},
		{
			name:   "aborting a discovery",
			create: discovery(),
			mutate: asOps(func(s *simplyblockv1alpha2.OperatorOpsSpec) { s.Abort = true }),
		},
	})
}

func siteDeployment() *simplyblockv1alpha2.StorageSiteDeployment {
	return &simplyblockv1alpha2.StorageSiteDeployment{
		Spec: simplyblockv1alpha2.StorageSiteDeploymentSpec{Cluster: "site-a", SiteNamespace: "simplyblock"},
	}
}

func asSiteDeployment(f func(*simplyblockv1alpha2.StorageSiteDeploymentSpec)) func(client.Object) {
	return func(o client.Object) { f(&o.(*simplyblockv1alpha2.StorageSiteDeployment).Spec) }
}

func stretched() *simplyblockv1alpha2.StretchedCluster {
	return &simplyblockv1alpha2.StretchedCluster{Spec: simplyblockv1alpha2.StretchedClusterSpec{
		ClusterName: "stretched",
		Sites: []simplyblockv1alpha2.StretchedSite{
			{Cluster: "site-a", Site: "a", Role: simplyblockv1alpha2.StretchedRoleLeader, SiteNamespace: "simplyblock"},
			{Cluster: "site-b", Site: "b", Role: simplyblockv1alpha2.StretchedRoleFollower, SiteNamespace: "simplyblock"},
		},
	}}
}

func asStretched(f func(*simplyblockv1alpha2.StretchedClusterSpec)) func(client.Object) {
	return func(o client.Object) { f(&o.(*simplyblockv1alpha2.StretchedCluster).Spec) }
}

func TestStorageSiteDeploymentUpdateRules(t *testing.T) {
	runTransitions(t, []transition{
		{
			name:   "moving the site's objects to another namespace",
			create: siteDeployment(),
			mutate: asSiteDeployment(func(s *simplyblockv1alpha2.StorageSiteDeploymentSpec) { s.SiteNamespace = "other" }),
			denied: "siteNamespace is immutable",
		},
		{
			name:   "resizing an approved deployment",
			create: approvedSiteDeployment(),
			mutate: asSiteDeployment(func(s *simplyblockv1alpha2.StorageSiteDeploymentSpec) {
				s.Sizing = &simplyblockv1alpha2.StorageSiteSizing{VCPUCount: ptr.To(int32(8))}
			}),
			denied: "sizing cannot change once",
		},
		{
			name:   "rediscovering an approved deployment",
			create: approvedSiteDeployment(),
			mutate: asSiteDeployment(func(s *simplyblockv1alpha2.StorageSiteDeploymentSpec) {
				s.Discover.Workers = []string{"worker-9"}
			}),
			denied: "discover is immutable",
		},
		{
			name:   "rediscovering a deployment under review",
			create: siteDeployment(),
			mutate: asSiteDeployment(func(s *simplyblockv1alpha2.StorageSiteDeploymentSpec) {
				s.Discover.Workers = []string{"worker-9"}
			}),
			denied: "discover is immutable",
		},
		{
			name:   "resizing a deployment under review",
			create: siteDeployment(),
			mutate: asSiteDeployment(func(s *simplyblockv1alpha2.StorageSiteDeploymentSpec) {
				s.Sizing = &simplyblockv1alpha2.StorageSiteSizing{VCPUCount: ptr.To(int32(8))}
			}),
		},
	})
}

func approvedSiteDeployment() *simplyblockv1alpha2.StorageSiteDeployment {
	d := siteDeployment()
	d.Spec.Approved = true
	d.Spec.Sizing = &simplyblockv1alpha2.StorageSiteSizing{VCPUCount: ptr.To(int32(4))}
	return d
}

func approvedStretched() *simplyblockv1alpha2.StretchedCluster {
	c := stretched()
	c.Spec.Approved = true
	c.Spec.Sizing = &simplyblockv1alpha2.StorageSiteSizing{VCPUCount: ptr.To(int32(4))}
	return c
}

func TestStretchedClusterUpdateRules(t *testing.T) {
	runTransitions(t, []transition{
		{
			name:   "moving one site's objects to another namespace",
			create: stretched(),
			mutate: asStretched(func(s *simplyblockv1alpha2.StretchedClusterSpec) { s.Sites[1].SiteNamespace = "other" }),
			denied: "siteNamespace is immutable",
		},
		{
			name:   "resizing an approved stretched cluster",
			create: approvedStretched(),
			mutate: asStretched(func(s *simplyblockv1alpha2.StretchedClusterSpec) { s.Sizing = nil }),
			denied: "sizing cannot change once",
		},
		{
			name:   "pinning another SPDK image on an approved stretched cluster",
			create: approvedStretched(),
			mutate: asStretched(func(s *simplyblockv1alpha2.StretchedClusterSpec) {
				s.Images = &simplyblockv1alpha2.DeploymentImages{
					SPDK: &simplyblockv1alpha2.ImageSpec{Image: "registry.example/spdk:2"}}
			}),
			denied: "images cannot change once",
		},
		{
			name:   "swapping the roles",
			create: stretched(),
			mutate: asStretched(func(s *simplyblockv1alpha2.StretchedClusterSpec) {
				s.Sites[0].Role = simplyblockv1alpha2.StretchedRoleFollower
				s.Sites[1].Role = simplyblockv1alpha2.StretchedRoleLeader
			}),
		},
	})
}

func TestStorageBackupPolicyUpdateRules(t *testing.T) {
	policy := func(group string, selector *metav1.LabelSelector) *simplyblockv1alpha2.StorageBackupPolicy {
		return &simplyblockv1alpha2.StorageBackupPolicy{Spec: simplyblockv1alpha2.StorageBackupPolicySpec{
			ClusterRef: "cluster-1", ConsistencyGroupName: group, ClaimSelector: selector,
		}}
	}
	asPolicy := func(f func(*simplyblockv1alpha2.StorageBackupPolicySpec)) func(client.Object) {
		return func(o client.Object) { f(&o.(*simplyblockv1alpha2.StorageBackupPolicy).Spec) }
	}
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"backup": "daily"}}
	runTransitions(t, []transition{
		{
			name:   "pointing a policy at another consistency group",
			create: policy("group-a", nil),
			mutate: asPolicy(func(s *simplyblockv1alpha2.StorageBackupPolicySpec) { s.ConsistencyGroupName = "group-b" }),
			denied: "consistencyGroupName is immutable",
		},
		{
			name:   "switching a claim policy to a consistency group",
			create: policy("", selector),
			mutate: asPolicy(func(s *simplyblockv1alpha2.StorageBackupPolicySpec) {
				s.ClaimSelector, s.ConsistencyGroupName = nil, "group-a"
			}),
			denied: "consistencyGroupName is immutable",
		},
		{
			name:   "widening the claims a policy selects",
			create: policy("", selector),
			mutate: asPolicy(func(s *simplyblockv1alpha2.StorageBackupPolicySpec) {
				s.ClaimSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"backup": "hourly"}}
			}),
		},
	})
}

// A bucket of zero or negative capacity has nothing to provision: the schema
// refuses it rather than leaving the PVC create to fail on every pass.
func TestS3BucketCapacityMustBePositive(t *testing.T) {
	c := ruleServer(t)
	for _, capacity := range []int64{0, -1} {
		bucket := &simplyblockv1alpha2.S3Bucket{Spec: simplyblockv1alpha2.S3BucketSpec{
			ObjectStoreRef: "store", BucketName: "data", CapacityBytes: capacity, StorageClassName: "standard",
		}}
		bucket.SetNamespace("default")
		bucket.SetGenerateName("rule-")
		err := c.Create(context.Background(), bucket)
		if err == nil || !strings.Contains(err.Error(), "capacityBytes") {
			t.Errorf("capacityBytes %d: create error = %v, want a refusal naming capacityBytes", capacity, err)
		}
	}
}

// A ClusterDeploymentConfig is frozen by its approval and by nothing before it:
// the webhook refuses every spec change to an approved document, and a draft
// takes any edit. So the schema carries no update rule but the one that keeps
// approval one-way. A rule anywhere below spec, usually arriving through a type
// the template shares with the StorageCluster, would freeze a setting on a
// draft nobody has reviewed yet; such a rule belongs on the StorageCluster.
func TestTheDeploymentConfigSchemaCarriesNoUpdateRuleButApproval(t *testing.T) {
	const approval = "!has(oldSelf.approved) || !oldSelf.approved || self.approved"
	raw, err := FS.ReadFile("storage.simplyblock.io_clusterdeploymentconfigs.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	var walk func(node any, path string)
	walk = func(node any, path string) {
		rules, _ := dig(node, "x-kubernetes-validations").([]any)
		for _, r := range rules {
			rule, _ := dig(r, "rule").(string)
			if strings.Contains(rule, "oldSelf") && (path != ".spec" || rule != approval) {
				t.Errorf("%s carries the update rule %q", path, rule)
			}
		}
		props, _ := dig(node, "properties").(map[string]any)
		for name, child := range props {
			walk(child, path+"."+name)
		}
		if items := dig(node, "items"); items != nil {
			walk(items, path+"[]")
		}
		if extra, ok := dig(node, "additionalProperties").(map[string]any); ok {
			walk(extra, path+"{}")
		}
	}
	versions, _ := dig(crd, "spec", "versions").([]any)
	for _, v := range versions {
		walk(dig(v, "schema", "openAPIV3Schema"), "")
	}
}
