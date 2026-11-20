// Package spine builds the ownership spine out of the discovered graph, and
// says what the target model does to each edge in it.
//
// §16.1 of design-api-upgrade.md is the change that defines the migration:
//
//	StorageCluster                 StorageCluster
//	      │                              ├── StorageNode A
//	StorageNodeSet            →          ├── StorageNode B
//	      ├── StorageNode A              └── StorageNode C
//	      ├── StorageNode B
//	      └── StorageNode C
//
// No CRD conversion can express it. A StorageNodeSet owns its StorageNode
// objects by controller reference, which is the one real ownership edge in the
// spine, and it also owns the workload that runs the storage nodes. All of it
// becomes children of the StorageCluster, and until that reparenting is done
// the retirement cannot proceed, because deleting a StorageNodeSet today is
// what tears those objects down.
//
// The package is separate from the checks that read it because two things do.
// The preflight asks whether the spine is well formed and whether the
// reparenting has somewhere to put every dependent, and the migration's
// ownership phase walks the same structure to perform it. A second traversal
// written for the second caller is a second answer to the question of what
// depends on what.
//
// Nothing here reports findings or writes to the cluster. It builds a structure
// and records what it could not resolve, and the caller decides what that means.
package spine
