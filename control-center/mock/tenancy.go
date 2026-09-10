package main

import "fmt"

// tenancy.go models the hub-side namespace layout, hierarchy labels, scope
// model and RBAC role vocabulary from the multi-cluster RBAC design
// (uploads/simplyblock-multicluster-rbac-design.md, §2). None of this is
// implemented on main; PARADIGM.md records what is faithful and what is a
// stand-in.
//
// Namespace plan (§2.1):
//   simplyblock-system     control-plane workloads
//   sb-mc-<managed>        ManagedCluster artifacts, projected intent + status (transport)
//   sb-sc-<cluster>        StorageCluster, StorageNode, StorageDevice (RBAC target, user-facing)
//   sb-sp-<pool>-<hash>    StoragePool + volumes/snapshots/backups (only when a tenant boundary)
//   sb-dr-system           DRPolicy, DRCluster
//   <application>          ProtectedApplication, ApplicationFailover

const (
	drSystemNs = "sb-dr-system"

	// Label keys — hierarchy is encoded in labels, not names (§2.2), because
	// namespace names are DNS labels (no dots). These drive RoleBinding
	// propagation, the scope tree, and admission bindings.
	labScopeKind      = "simplyblock.io/scope-kind"
	labManagedCluster = "simplyblock.io/managed-cluster"
	labStorageCluster = "simplyblock.io/storage-cluster"
	labStoragePool    = "simplyblock.io/storage-pool"
	labManagedBy      = "simplyblock.io/managed-by"

	// Actor-stamp annotations (§3.2) — written by a mutating webhook on the
	// hub from AdmissionRequest.userInfo. The mock stamps them at generation
	// time; see PARADIGM.md for why real capture cannot be reproduced.
	annActorUser   = "simplyblock.io/actor-username"
	annActorUID    = "simplyblock.io/actor-uid"
	annActorGroups = "simplyblock.io/actor-groups"
	annActorTime   = "simplyblock.io/actor-timestamp"
)

func nsManagedCluster(mc string) string { return "sb-mc-" + mc }
func nsStorageCluster(sc string) string { return "sb-sc-" + sc }
func nsStoragePool(pool, hash string) string {
	return fmt.Sprintf("sb-sp-%s-%s", pool, hash)
}

// scopeKind values on namespaces (and the scope tree the UI renders).
const (
	scopeManagedCluster = "managed-cluster"
	scopeStorageCluster = "storage-cluster"
	scopeStoragePool    = "storage-pool"
	scopeDR             = "dr"
	scopeApplication    = "application"
	scopeCluster        = "cluster" // sb:infra-admin, no namespace
)

// sbRole is one of the eight aggregated ClusterRoles (§2.3). boundNs is where
// a grant of the role lives; verbs/resources are what the aggregated role
// grants (used by the mock's SelfSubjectRulesReview/SubjectAccessReview).
type sbRole struct {
	Name      string
	Scope     string
	Resources []string
	Write     bool // admin (write verbs) vs reader (read-only)
	AggLabel  string
}

func sbRoles() []sbRole {
	return []sbRole{
		{"sb:infra-admin", scopeCluster, []string{"nodepoolallocations", "managedclusters", "storageclusterclasses", "accessgrants"}, true, "simplyblock.io/aggregate-to-infra-admin"},
		{"sb:cluster-admin", scopeStorageCluster, []string{"storageclusters", "storagenodes", "storagedevices", "storageclusterops", "storagenodeops", "storagedeviceops"}, true, "simplyblock.io/aggregate-to-cluster-admin"},
		{"sb:cluster-reader", scopeStorageCluster, []string{"storageclusters", "storagenodes", "storagedevices"}, false, "simplyblock.io/aggregate-to-cluster-reader"},
		{"sb:pool-admin", scopeStoragePool, []string{"storagepools", "storagebackups", "backuppolicies", "backuprestores", "storagepoolops", "storagebackupops"}, true, "simplyblock.io/aggregate-to-pool-admin"},
		{"sb:pool-reader", scopeStoragePool, []string{"storagepools", "storagebackups", "backuppolicies"}, false, "simplyblock.io/aggregate-to-pool-reader"},
		{"sb:dr-admin", scopeDR, []string{"replicationpairs", "replicationpolicies", "replicationslots", "replicationops"}, true, "simplyblock.io/aggregate-to-dr-admin"},
		{"sb:dr-reader", scopeDR, []string{"replicationpairs", "replicationpolicies", "replicationslots"}, false, "simplyblock.io/aggregate-to-dr-reader"},
		{"sb:app-admin", scopeApplication, []string{"protectedapplications", "applicationfailovers"}, true, "simplyblock.io/aggregate-to-app-admin"},
	}
}

func sbRoleByName(name string) (sbRole, bool) {
	for _, r := range sbRoles() {
		if r.Name == name {
			return r, true
		}
	}
	return sbRole{}, false
}

func adminVerbs() []any { return []any{"get", "list", "watch", "create", "update", "patch", "delete"} }
func readVerbs() []any  { return []any{"get", "list", "watch"} }

// ---- paradigm stamping helpers ------------------------------------------------
//
// The design mandates (design-crd-model.md §7.9) that every simplyblock kind
// carries status.observedGeneration and a standard conditions surface, and
// that hub-authored objects carry the actor annotations and the managed-by
// label. These helpers apply that uniformly so the mock's world matches the
// target model even though main does not.

// stampActor writes the actor-stamp annotations a mutating webhook would set.
func stampActor(meta map[string]any, user, uid, groups, ts string) {
	ann, _ := meta["annotations"].(map[string]any)
	if ann == nil {
		ann = map[string]any{}
		meta["annotations"] = ann
	}
	ann[annActorUser] = user
	ann[annActorUID] = uid
	ann[annActorGroups] = groups
	ann[annActorTime] = ts
}

// readyCondition returns a standard metav1.Condition slice with observedGeneration
// carried on the condition too, as the design's condition convention requires.
func readyCondition(condType, status, reason, msg, ts string, gen int) []any {
	return []any{map[string]any{
		"type": condType, "status": status, "reason": reason, "message": msg,
		"lastTransitionTime": ts, "observedGeneration": float64(gen),
	}}
}
