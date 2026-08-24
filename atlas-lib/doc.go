// Package atlas is the root of the simplyblock shared library used by the
// Kubernetes operator and the CSI driver.
//
// It holds what both consumers need and neither should re-implement. Most of it
// is node-level storage: NVMe device discovery, NVMe-oF fabric connection
// management, NQN handling, and the mapping between a logical volume and the
// local NVMe namespace that backs it. The rest is the vocabulary around that —
// the control-plane client, the classification of a failure into what a caller
// should do about it, and the small helpers both sides would otherwise write
// twice.
//
// Public packages, each one cohesive concern:
//
//	nvme            Discover and look up local NVMe controllers and namespaces.
//	nvmeof          Connect and disconnect NVMe-oF (TCP) targets.
//	nqn             Build and parse NVMe Qualified Names.
//	lvol            Logical-volume identity, and lvol -> NVMe device mapping.
//	kube            Map a logical volume to the Kubernetes objects representing it.
//	controlplane    Client for the simplyblock control-plane API.
//	errs            Sentinel errors shared across atlas, matched with errors.Is.
//	errs/class      Classify a failure: the gRPC status to answer with, and
//	                whether retrying can help.
//	errs/deferrers  Deferred cleanup that logs its error instead of dropping it.
//	locks           Scope a mutex to one function call, always unlocked by defer.
//	statemachine    Deterministic state machine declared as data, with entry
//	                hooks and context deadlines.
//	net             Reject URLs that are unsafe to forward to the backend.
//	ptr             Pointers to values, for the optional fields of generated
//	                request bodies and Kubernetes types.
//
// Everything under internal/ — sysfs scanning, the generated control-plane API
// client, and build metadata — is implementation detail and carries no
// compatibility guarantee.
//
// README.md next to this file carries the worked flows both consumers actually
// perform — the idiomatic call sequence for each, a file-level index of every
// package, and a note on which patterns are already wired at a live call site.
// Read it before writing a helper.
//
// A new public package belongs in the list above, and a new flow in the README:
// they are what the next person reads before deciding to write their own copy of
// something.
package atlas
