package main

// tenancy.go models the hub-side namespace layout, hierarchy labels, scope
// model and RBAC role vocabulary. None of this is implemented on main;
// PARADIGM.md records what is faithful and what is a stand-in.
//
// Namespace plan (tenant-centric, per the "namespaces reflect tenants" model):
//   simplyblock-system   control-plane workloads
//   sb-mc-<managed>      ManagedCluster transport: projected intent + status
//   sb-<tenant>          the RBAC target — holds one or more whole clusters plus
//                        the tenant's DR-policies and DR-applications. Global
//                        admins create and manage these; a tenant-admin
//                        provisions objects into them.
//   <application>        k8s workload namespace (recipe editor discovery)

const (
	// Label keys — hierarchy is encoded in labels, not names (§2.2), because
	// namespace names are DNS labels (no dots). These drive RoleBinding
	// propagation, the scope tree, and admission bindings.
	labScopeKind      = "simplyblock.io/scope-kind"
	labTenant         = "simplyblock.io/tenant"
	labManagedCluster = "simplyblock.io/managed-cluster"
	labStorageCluster = "simplyblock.io/storage-cluster"
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
func nsTenant(tenant string) string     { return "sb-" + tenant }

// scopeKind values on namespaces (and the scope tree the UI renders).
const (
	scopeManagedCluster = "managed-cluster"
	scopeTenant         = "tenant"      // sb-<tenant>: the RBAC target, holds clusters + DR
	scopeApplication    = "application" // k8s workload namespace (recipe editor discovery)
	scopeCluster        = "cluster"     // sb:infra-admin, no namespace (global)
)

// The three "main objects" a tenant contains, each with its sub-objects. The
// role model is one full-admin (CRUD across all of them) plus one read-only
// role per main object — covering that object AND its sub-objects.
var (
	clusterTree = []string{
		"storageclusters", "storagenodes", "storagedevices", "storagenodesets",
		"storagepools", "storagebackups", "backuppolicies", "backuprestores", "backupimports",
		"storageclusterops", "storagenodeops", "storagedeviceops", "storagepoolops", "storagebackupops",
		"volumemigrations", "tasks",
	}
	drPolicyTree = []string{
		"replicationpairs", "replicationpolicies", "replicationslots", "replicationops",
	}
	drAppTree = []string{
		"protectedapplications", "applicationfailovers",
	}
)

func tenantAdminResources() []string {
	out := append([]string{}, clusterTree...)
	out = append(out, drPolicyTree...)
	out = append(out, drAppTree...)
	return out
}

// sbRole is a role the console can grant. Scope is where a grant of it binds:
// `cluster` (global, sb:infra-admin) or `tenant` (a RoleBinding in the tenant
// namespace). Write=true is the full-admin (CRUD) role; the readers are
// read-only. Verbs/resources drive the mock's SelfSubjectRulesReview /
// SubjectAccessReview.
type sbRole struct {
	Name      string
	Scope     string
	Resources []string
	Write     bool
	AggLabel  string
}

// sbRoles is the product-facing role model:
//   - sb:infra-admin        global admin — creates/manages tenant namespaces and does the bindings
//   - sb:tenant-admin       full CRUD on every object in a tenant (the provisioning role)
//   - sb:cluster-reader          read-only: cluster main object + its sub-objects
//   - sb:dr-policy-reader         read-only: DR-policy main object + its sub-objects
//   - sb:dr-application-reader    read-only: DR-application main object + its sub-objects
func sbRoles() []sbRole {
	return []sbRole{
		{"sb:infra-admin", scopeCluster, []string{
			"namespaces", "nodepoolallocations", "managedclusters", "storageclusterclasses",
			"accessgrants", "clusterroles", "rolebindings",
		}, true, "simplyblock.io/aggregate-to-infra-admin"},
		{"sb:tenant-admin", scopeTenant, tenantAdminResources(), true, "simplyblock.io/aggregate-to-tenant-admin"},
		{"sb:cluster-reader", scopeTenant, clusterTree, false, "simplyblock.io/aggregate-to-cluster-reader"},
		{"sb:dr-policy-reader", scopeTenant, drPolicyTree, false, "simplyblock.io/aggregate-to-dr-policy-reader"},
		{"sb:dr-application-reader", scopeTenant, drAppTree, false, "simplyblock.io/aggregate-to-dr-application-reader"},
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
