# The storage CRDs the console reads

The authority for every field name the console shows is the operator's CRD set in
`operator/config/crd/bases`, at the storage version each kind declares:
`storage.simplyblock.io/v1alpha2` for StorageCluster, StorageNode, StorageDevice,
StoragePool, StorageBackup, StorageBackupPolicy, StorageGroupBackup, ControlPlane,
SimplyblockDriver, ClusterDeploymentConfig, StretchedCluster, StorageSiteDeployment,
and every Ops kind; `v1alpha1` only for BackupImport, BackupPolicy, BackupRestore,
ReplicationPair, ReplicationPolicy, ReplicationSlot, ReplicationOps, Task, and
VolumeMigration. `k8s-client.jsx` carries that version per kind.

The console reads the CRDs themselves at run time (apiextensions) for field
descriptions, enums, and immutability rules, so the two-tier resource view
(`cr-view.jsx`) explains a field in the operator's own words. The headline tables
(`cr-headlines.jsx`) name fields by their CRD path, and
`deploy/build/check-crd-paths.py` fails when one of them is not in the CRD.

What the console shows about operations, and where each value comes from, is in
[OPERATIONS.md](OPERATIONS.md).
