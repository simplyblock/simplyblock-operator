// Package backup holds the data-protection band: the three controllers that
// reconcile StorageBackupPolicy, StorageBackup, and StorageBackupOps.
//
// They are one package because they are one loop. A policy tells the control
// plane which claims to back up and how often, the control plane takes the
// copies and writes them to the cluster's store, and the mirror turns what the
// store holds into one StorageBackup object each. A StorageBackupOps reads one of
// those objects back into a claim. Splitting them would put two halves of one
// loop in two packages, which is the same reasoning that keeps the device mirror
// beside the node that discovers it.
//
// Three properties are worth knowing before reading any of the files here.
//
// A StorageBackup is an observation and not a request. Nothing in this package
// asks for a backup to be taken: a policy does that indirectly by telling the
// control plane to, and every object is created by the mirror from what the
// control plane reports. The webhooks refuse a create and a delete from anybody
// but the operator, so the only way to make one appear is to put a copy in the
// store (design-storagebackup.md §5.1).
//
// Deleting a StorageBackup object never deletes the copy. The copy is an object
// in somebody's bucket, governed by that bucket's lifecycle policy and by the
// retention the control plane applies. Nothing here prunes (§9).
//
// A restore's product is not owned by the operation that produced it. Nobody
// restores a backup in order to keep a StorageBackupOps, so the claim is an
// ordinary PersistentVolumeClaim from the moment it exists and deleting the
// audit record leaves the data alone (§8).
package backup
