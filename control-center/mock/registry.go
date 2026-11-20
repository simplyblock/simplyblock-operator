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

		// ---- simplyblock: the operator's CRDs on main, at their storage version.
		// The kinds that moved to v1alpha2 are served there only; the API
		// server would also serve v1alpha1 through conversion, but the console
		// never asks for it.
		def(sbGroup, "v1alpha2", "storageclusters", "StorageCluster", true),
		def(sbGroup, "v1alpha2", "storagenodes", "StorageNode", true),
		def(sbGroup, "v1alpha2", "storagedevices", "StorageDevice", true),
		def(sbGroup, "v1alpha2", "storagepools", "StoragePool", true),
		def(sbGroup, "v1alpha2", "storagebackups", "StorageBackup", true),
		def(sbGroup, "v1alpha2", "storagebackuppolicies", "StorageBackupPolicy", true),
		def(sbGroup, "v1alpha2", "storagegroupbackups", "StorageGroupBackup", true),
		def(sbGroup, "v1alpha2", "controlplanes", "ControlPlane", true),
		def(sbGroup, "v1alpha2", "simplyblockdrivers", "SimplyblockDriver", true),
		def(sbGroup, "v1alpha2", "clusterdeploymentconfigs", "ClusterDeploymentConfig", true),
		def(sbGroup, "v1alpha2", "stretchedclusters", "StretchedCluster", true),
		def(sbGroup, "v1alpha2", "storagesitedeployments", "StorageSiteDeployment", true),
		def(sbGroup, "v1alpha2", "nfsexports", "NFSExport", true),
		def(sbGroup, "v1alpha2", "objectstores", "ObjectStore", true),
		def(sbGroup, "v1alpha2", "s3buckets", "S3Bucket", true),
		def(sbGroup, "v1alpha2", "storagefenceremediations", "StorageFenceRemediation", true),
		def(sbGroup, "v1alpha2", "storagefenceremediationtemplates", "StorageFenceRemediationTemplate", true),
		def(sbGroup, "v1alpha2", "testfailovers", "TestFailover", true),
		// one-shot operations
		def(sbGroup, "v1alpha2", "storageclusterops", "StorageClusterOps", true),
		def(sbGroup, "v1alpha2", "storagenodeops", "StorageNodeOps", true),
		def(sbGroup, "v1alpha2", "storagedeviceops", "StorageDeviceOps", true),
		def(sbGroup, "v1alpha2", "storagepoolops", "StoragePoolOps", true),
		def(sbGroup, "v1alpha2", "storagebackupops", "StorageBackupOps", true),
		def(sbGroup, "v1alpha2", "controlplaneops", "ControlPlaneOps", true),
		def(sbGroup, "v1alpha2", "persistentvolumeops", "PersistentVolumeOps", false), // a PersistentVolume has no namespace
		def(sbGroup, "v1alpha2", "operatorops", "OperatorOps", true),
		def(sbGroup, "v1alpha2", "volumegroupsnapshotops", "VolumeGroupSnapshotOps", true),
		// the kinds that only exist in v1alpha1
		def(sbGroup, "v1alpha1", "backuppolicies", "BackupPolicy", true),
		def(sbGroup, "v1alpha1", "backuprestores", "BackupRestore", true),
		def(sbGroup, "v1alpha1", "backupimports", "BackupImport", true),
		def(sbGroup, "v1alpha1", "replicationpairs", "ReplicationPair", true),
		def(sbGroup, "v1alpha1", "replicationpolicies", "ReplicationPolicy", true),
		def(sbGroup, "v1alpha1", "replicationslots", "ReplicationSlot", true),
		def(sbGroup, "v1alpha1", "replicationops", "ReplicationOps", true),
		def(sbGroup, "v1alpha1", "volumemigrations", "VolumeMigration", true),
		def(sbGroup, "v1alpha1", "tasks", "Task", true),
		def(sbGroup, "v1alpha1", "storagenodesets", "StorageNodeSet", true),
		// the CRDs themselves (crds.go)
		def(apiextGroup, "v1", "customresourcedefinitions", "CustomResourceDefinition", false),

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
