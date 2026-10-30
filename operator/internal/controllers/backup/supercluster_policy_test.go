// A backup policy on a supercluster (design-superclusters.md §9.5): one
// backend policy in each member, recorded per member, and each claim attached
// to the policy of the member its volume is in.

package backup

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/errs"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	backupSupercluster = "prod"
	memberAName        = "prod-a"
	memberAID          = "aaaa0000-0000-4000-8000-00000000000a"
	memberBName        = "prod-b"
	memberBID          = "bbbb0000-0000-4000-8000-00000000000b"
)

// memberBackups is a control plane serving several clusters, each with its
// own backup policies.
type memberBackups struct {
	mu       sync.Mutex
	policies map[string][]controlplane.BackupPolicy
	attached []string // cluster/policy/volume
	detached []string
	deleted  []string // cluster/policy
}

// ConsistencyGroupByName finds no group: these tests cover per-claim
// policies, and a group policy is covered with the single-cluster fake.
func (m *memberBackups) ConsistencyGroupByName(_ context.Context, clusterID, name string) (string, error) {
	return "", fmt.Errorf("no consistency group named %q in cluster %s: %w", name, clusterID, errs.ErrNotFound)
}

func (m *memberBackups) AttachBackupPolicyToConsistencyGroup(context.Context, string, string, string) error {
	return nil
}

func newMemberBackups() *memberBackups {
	return &memberBackups{policies: map[string][]controlplane.BackupPolicy{}}
}

func (m *memberBackups) ListBackupPolicies(_ context.Context, cluster string) ([]controlplane.BackupPolicy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]controlplane.BackupPolicy(nil), m.policies[cluster]...), nil
}

func (m *memberBackups) BackupPolicyByName(_ context.Context, cluster, name string) (controlplane.BackupPolicy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.policies[cluster] {
		if p.Name == name {
			return p, nil
		}
	}
	return controlplane.BackupPolicy{}, notFoundError{}
}

func (m *memberBackups) CreateBackupPolicy(
	_ context.Context, cluster string, params controlplane.CreateBackupPolicyParams,
) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := fmt.Sprintf("policy-%s", cluster[:4])
	m.policies[cluster] = append(m.policies[cluster], controlplane.BackupPolicy{ID: id, Name: params.Name})
	return id, nil
}

func (m *memberBackups) DeleteBackupPolicy(_ context.Context, cluster, policy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = append(m.deleted, cluster+"/"+policy)
	return nil
}

func (m *memberBackups) AttachBackupPolicy(_ context.Context, cluster, policy, volume string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attached = append(m.attached, cluster+"/"+policy+"/"+volume)
	return nil
}

func (m *memberBackups) DetachBackupPolicy(_ context.Context, cluster, policy, volume string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.detached = append(m.detached, cluster+"/"+policy+"/"+volume)
	return nil
}

func backupSuperclusterObjects() []client.Object {
	supercluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: objectMeta(backupSupercluster),
		Spec:       simplyblockv1alpha2.StorageClusterSpec{Supercluster: &simplyblockv1alpha2.SuperclusterSpec{}},
		Status:     simplyblockv1alpha2.StorageClusterStatus{UUID: "0d6f1b2e-1f0a-4c71-9a0e-5b7d2c3e4f50"},
	}
	member := func(name, uuid string) *simplyblockv1alpha2.StorageCluster {
		return &simplyblockv1alpha2.StorageCluster{
			ObjectMeta: objectMeta(name),
			Spec:       simplyblockv1alpha2.StorageClusterSpec{SuperclusterRef: backupSupercluster},
			Status:     simplyblockv1alpha2.StorageClusterStatus{UUID: uuid},
		}
	}
	policy := policyObject(&metav1.LabelSelector{MatchLabels: map[string]string{"backup": "nightly"}})
	policy.Spec.ClusterRef = backupSupercluster
	return []client.Object{supercluster, member(memberAName, memberAID), member(memberBName, memberBID), policy}
}

// memberClaim is a bound claim whose volume is in the given cluster.
func claimInMember(name, cluster, lvol string) []client.Object {
	return []client.Object{
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: map[string]string{"backup": "nightly"}},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-" + name},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pv-" + name},
			Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: cluster + ":" + testPoolID + ":" + lvol},
			}},
		},
	}
}

func readPolicy(t *testing.T, r *StorageBackupPolicyReconciler) *simplyblockv1alpha2.StorageBackupPolicy {
	t.Helper()
	var policy simplyblockv1alpha2.StorageBackupPolicy
	if err := r.Get(context.Background(), policyRequest().NamespacedName, &policy); err != nil {
		t.Fatal(err)
	}
	return &policy
}

// A policy on a supercluster creates a backend policy in every member and
// records each, and has no backend policy of its own.
func TestAPolicyOnASuperclusterIsCreatedInEveryMember(t *testing.T) {
	api := newMemberBackups()
	r := policyReconciler(t, api, backupSuperclusterObjects()...)

	if _, err := r.Reconcile(context.Background(), policyRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	policy := readPolicy(t, r)
	want := []simplyblockv1alpha2.BackupPolicyMemberStatus{
		{Cluster: memberAName, ClusterID: memberAID, PolicyID: "policy-aaaa"},
		{Cluster: memberBName, ClusterID: memberBID, PolicyID: "policy-bbbb"},
	}
	if !slices.Equal(policy.Status.Members, want) {
		t.Errorf("status.members = %+v, want %+v", policy.Status.Members, want)
	}
	if policy.Status.PolicyID != "" || policy.Status.ClusterID != "" {
		t.Errorf("status names policy %q in cluster %q, want none of its own", policy.Status.PolicyID, policy.Status.ClusterID)
	}
	if policy.Status.Phase != simplyblockv1alpha2.StorageBackupPolicyPhaseActive {
		t.Errorf("phase = %s (%s), want Active", policy.Status.Phase, policy.Status.Message)
	}
}

// Each claim is attached to the policy of the member its volume is in, and a
// claim in a cluster outside the supercluster is not covered.
func TestEachClaimIsAttachedInItsOwnMember(t *testing.T) {
	api := newMemberBackups()
	objects := backupSuperclusterObjects()
	objects = append(objects, claimInMember("in-a", memberAID, "a1111111-1111-4111-8111-111111111111")...)
	objects = append(objects, claimInMember("in-b", memberBID, "b2222222-2222-4222-8222-222222222222")...)
	objects = append(objects, claimInMember("elsewhere", testClusterID, testLvolID)...)
	r := policyReconciler(t, api, objects...)

	if _, err := r.Reconcile(context.Background(), policyRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	slices.Sort(api.attached)
	want := []string{
		memberAID + "/policy-aaaa/a1111111-1111-4111-8111-111111111111",
		memberBID + "/policy-bbbb/b2222222-2222-4222-8222-222222222222",
	}
	if !slices.Equal(api.attached, want) {
		t.Errorf("attached %v, want %v", api.attached, want)
	}
	policy := readPolicy(t, r)
	if len(policy.Status.AttachedClaims) != 2 {
		t.Errorf("attachedClaims = %+v, want the two members' claims", policy.Status.AttachedClaims)
	}
}

// Deleting the policy detaches each claim in its member and deletes every
// member's policy.
func TestDeletingAPolicyOnASuperclusterTearsDownEveryMember(t *testing.T) {
	api := newMemberBackups()
	objects := backupSuperclusterObjects()
	objects = append(objects, claimInMember("in-a", memberAID, "a1111111-1111-4111-8111-111111111111")...)
	r := policyReconciler(t, api, objects...)
	if _, err := r.Reconcile(context.Background(), policyRequest()); err != nil {
		t.Fatal(err)
	}

	if err := r.Delete(context.Background(), readPolicy(t, r)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), policyRequest()); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(api.detached, []string{memberAID + "/policy-aaaa/a1111111-1111-4111-8111-111111111111"}) {
		t.Errorf("detached %v, want the claim in its member", api.detached)
	}
	slices.Sort(api.deleted)
	if !slices.Equal(api.deleted, []string{memberAID + "/policy-aaaa", memberBID + "/policy-bbbb"}) {
		t.Errorf("deleted %v, want both members' policies", api.deleted)
	}
}

// A supercluster without a member that has a backend cluster yet holds the
// policy Pending.
func TestAPolicyOnASuperclusterWithoutMembersWaits(t *testing.T) {
	api := newMemberBackups()
	objects := backupSuperclusterObjects()
	objects = append(objects[:1], objects[3])
	r := policyReconciler(t, api, objects...)

	if _, err := r.Reconcile(context.Background(), policyRequest()); err != nil {
		t.Fatal(err)
	}

	policy := readPolicy(t, r)
	if policy.Status.Phase != simplyblockv1alpha2.StorageBackupPolicyPhasePending ||
		!strings.Contains(policy.Status.Message, backupSupercluster) {
		t.Errorf("phase = %s (%q), want Pending naming the supercluster", policy.Status.Phase, policy.Status.Message)
	}
}

// fakeChooser answers Choose with a fixed member and remembers the requests.
type fakeChooser struct {
	member string
	asked  []string // pool/volume/size
}

func (c *fakeChooser) Choose(
	_ context.Context, _ *simplyblockv1alpha2.StorageCluster, pool, volume string, size int64,
) (string, error) {
	c.asked = append(c.asked, fmt.Sprintf("%s/%s/%d", pool, volume, size))
	return c.member, nil
}

// A restore into a supercluster is placed among the members (design §9.5):
// the member chosen and that member's backend pool are recorded before the
// control plane is asked for anything, so a restart restores into the same
// member.
func TestARestoreIntoASuperclusterIsPlacedAmongTheMembers(t *testing.T) {
	objects := backupSuperclusterObjects()[:3]
	superclusterPool := &simplyblockv1alpha2.StoragePool{
		ObjectMeta: objectMeta(testPoolCR),
		Spec:       simplyblockv1alpha2.StoragePoolSpec{ClusterRef: backupSupercluster},
		Status: simplyblockv1alpha2.StoragePoolStatus{Members: []simplyblockv1alpha2.PoolMemberStatus{
			{Cluster: memberAName, UUID: "pool-in-a", Phase: simplyblockv1alpha2.StoragePoolPhaseReady},
			{Cluster: memberBName, UUID: "pool-in-b", Phase: simplyblockv1alpha2.StoragePoolPhaseReady},
		}},
	}
	ops := restoreOps(testOpsName)
	ops.Spec.ClusterRef.Name = backupSupercluster
	objects = append(objects, superclusterPool, availableBackup(), ops)
	api := &fakeControlPlane{restoredID: testRestoreID}
	r := opsReconciler(t, api, objects...)
	chooser := &fakeChooser{member: memberBID}
	r.Placement = chooser

	for range 3 {
		if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}

	var got simplyblockv1alpha2.StorageBackupOps
	if err := r.Get(context.Background(), opsRequest(testOpsName).NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ClusterID != memberBID || got.Status.PoolUUID != "pool-in-b" {
		t.Errorf("status records cluster %q and pool %q, want %s and pool-in-b (message %q)",
			got.Status.ClusterID, got.Status.PoolUUID, memberBID, got.Status.Message)
	}
	if len(chooser.asked) != 1 || chooser.asked[0] != fmt.Sprintf("%s/restored-claim/%d", testPoolCR, 1<<30) {
		t.Errorf("placement was asked %v, want once for the backup's size", chooser.asked)
	}
}
