package main

// The registry is the union of everything the console's ClusterRole touches
// (helm-charts/.../templates/control-center-rbac.yaml): the real simplyblock
// CRDs shipped in the chart, the *proposed* kinds the console was designed
// against that have no CRD yet (they serve empty lists until they exist), the
// Ramen and OCM kinds, and the core objects. A kind registered here answers
// discovery, CRUD and watch even when no dataset puts objects into it — an
// empty list beats a 404 for a UI.

const (
	sbGroup    = "storage.simplyblock.io"
	ramenGroup = "ramendr.openshift.io"
	ocmGroup   = "cluster.open-cluster-management.io"
	kvGroup    = "kubevirt.io"
	rbacGroup  = "rbac.authorization.k8s.io"
	authzGroup = "authorization.k8s.io"
)

func def(group, version, resource, kind string, namespaced bool) ResourceDef {
	return ResourceDef{GVR: GVR{group, version, resource}, Kind: kind, Namespaced: namespaced}
}

func allResourceDefs() []ResourceDef {
	return []ResourceDef{
		// ---- core ----
		def("", "v1", "namespaces", "Namespace", false),
		def("", "v1", "nodes", "Node", false),
		def("", "v1", "persistentvolumes", "PersistentVolume", false),
		def("", "v1", "persistentvolumeclaims", "PersistentVolumeClaim", true),
		def("", "v1", "pods", "Pod", true),
		def("", "v1", "events", "Event", true),
		def("", "v1", "secrets", "Secret", true),
		def("", "v1", "configmaps", "ConfigMap", true),
		def("", "v1", "services", "Service", true),

		// ---- workloads (recipe editor discovery) ----
		def("apps", "v1", "deployments", "Deployment", true),
		def("apps", "v1", "statefulsets", "StatefulSet", true),
		def("apps", "v1", "daemonsets", "DaemonSet", true),
		def("apps", "v1", "replicasets", "ReplicaSet", true),
		def("networking.k8s.io", "v1", "ingresses", "Ingress", true),

		// ---- storage ----
		def("storage.k8s.io", "v1", "storageclasses", "StorageClass", false),
		def("snapshot.storage.k8s.io", "v1", "volumesnapshots", "VolumeSnapshot", true),
		def("snapshot.storage.k8s.io", "v1", "volumesnapshotcontents", "VolumeSnapshotContent", false),
		def("snapshot.storage.k8s.io", "v1", "volumesnapshotclasses", "VolumeSnapshotClass", false),

		// ---- simplyblock: CRDs that exist in helm-charts/.../crds ----
		def(sbGroup, "v1alpha1", "storageclusters", "StorageCluster", true),
		def(sbGroup, "v1alpha1", "storagenodes", "StorageNode", true),
		def(sbGroup, "v1alpha1", "storagenodesets", "StorageNodeSet", true),
		def(sbGroup, "v1alpha1", "storagedevices", "StorageDevice", true),
		def(sbGroup, "v1alpha1", "storagepools", "StoragePool", true),
		def(sbGroup, "v1alpha1", "storagebackups", "StorageBackup", true),
		def(sbGroup, "v1alpha1", "backuppolicies", "BackupPolicy", true),
		def(sbGroup, "v1alpha1", "backuprestores", "BackupRestore", true),
		def(sbGroup, "v1alpha1", "backupimports", "BackupImport", true),
		def(sbGroup, "v1alpha1", "controlplanes", "ControlPlane", true),
		def(sbGroup, "v1alpha1", "replicationpairs", "ReplicationPair", true),
		def(sbGroup, "v1alpha1", "replicationpolicies", "ReplicationPolicy", true),
		def(sbGroup, "v1alpha1", "replicationslots", "ReplicationSlot", true),
		def(sbGroup, "v1alpha1", "volumemigrations", "VolumeMigration", true),
		def(sbGroup, "v1alpha1", "tasks", "Task", true),
		def(sbGroup, "v1alpha1", "storageclusterops", "StorageClusterOps", true),
		def(sbGroup, "v1alpha1", "storagenodeops", "StorageNodeOps", true),
		def(sbGroup, "v1alpha1", "replicationops", "ReplicationOps", true),
		def(sbGroup, "v1alpha2", "clusterdeploymentconfigs", "ClusterDeploymentConfig", true),
		def(sbGroup, "v1alpha2", "operatorops", "OperatorOps", true),

		// ---- simplyblock: proposed kinds (console RBAC, no CRD yet) ----
		def(sbGroup, "v1alpha1", "storagedeviceops", "StorageDeviceOps", true),
		def(sbGroup, "v1alpha1", "storagepoolops", "StoragePoolOps", true),
		def(sbGroup, "v1alpha1", "storagebackupops", "StorageBackupOps", true),
		def(sbGroup, "v1alpha1", "controlplaneops", "ControlPlaneOps", true),
		def(sbGroup, "v1alpha1", "persistentvolumeops", "PersistentVolumeOps", true),
		def(sbGroup, "v1alpha1", "simplyblockdrivers", "SimplyblockDriver", true),

		// ---- multi-cluster / multi-tenant paradigm (design-only; see PARADIGM.md) ----
		// Hub tenancy + RBAC surface from the multi-cluster RBAC design. None of
		// these exist on main; they are modelled here so the console's access,
		// hub/agent and drift views can be exercised.
		def(sbGroup, "v1alpha1", "managedclusters", "ManagedCluster", false),         // hub registration (cluster-scoped)
		def(sbGroup, "v1alpha1", "nodepoolallocations", "NodePoolAllocation", false), // privileged-op envelope (cluster-scoped)
		def(sbGroup, "v1alpha1", "storageclusterclasses", "StorageClusterClass", false),
		def(sbGroup, "v1alpha1", "accessgrants", "AccessGrant", true), // (subject, role, scope) → RoleBindings
		def(sbGroup, "v1alpha1", "protectedapplications", "ProtectedApplication", true),
		def(sbGroup, "v1alpha1", "applicationfailovers", "ApplicationFailover", true),

		// ---- Kubernetes RBAC (the hub is the single policy decision point) ----
		def(rbacGroup, "v1", "clusterroles", "ClusterRole", false),
		def(rbacGroup, "v1", "roles", "Role", true),
		def(rbacGroup, "v1", "rolebindings", "RoleBinding", true),
		def(rbacGroup, "v1", "clusterrolebindings", "ClusterRoleBinding", false),
		def("", "v1", "serviceaccounts", "ServiceAccount", true),

		// ---- Ramen ----
		def(ramenGroup, "v1alpha1", "drpolicies", "DRPolicy", false),
		def(ramenGroup, "v1alpha1", "drclusters", "DRCluster", false),
		def(ramenGroup, "v1alpha1", "drclusterconfigs", "DRClusterConfig", false),
		def(ramenGroup, "v1alpha1", "drplacementcontrols", "DRPlacementControl", true),
		def(ramenGroup, "v1alpha1", "volumereplicationgroups", "VolumeReplicationGroup", true),
		def(ramenGroup, "v1alpha1", "recipes", "Recipe", true),

		// ---- OCM / KubeVirt (read-only in the console) ----
		def(ocmGroup, "v1", "managedclusters", "ManagedCluster", false),
		def(kvGroup, "v1", "virtualmachines", "VirtualMachine", true),
		def(kvGroup, "v1", "virtualmachineinstances", "VirtualMachineInstance", true),
	}
}
