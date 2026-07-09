// Package atlas is the root of the simplyblock shared library used by the
// Kubernetes operator and the CSI driver.
//
// It provides node-level storage primitives that both consumers need but
// neither should re-implement: NVMe device discovery, NVMe-oF fabric
// connection management, NQN handling, and the mapping between a logical
// volume and the local NVMe namespace that backs it.
//
// Public packages, each one cohesive concern:
//
//	nvme         Discover and look up local NVMe controllers/namespaces.
//	nvmeof       Connect/disconnect NVMe-oF (TCP) targets.
//	nqn          Build and parse NVMe Qualified Names.
//	lvol         Logical-volume identity and lvol -> NVMe device mapping.
//	controlplane Client for the simplyblock control-plane API.
//	log          Structured logging shared by both consumers.
//	errs         Shared sentinel errors.
//
// Everything under internal/ (sysfs scanning, command execution, build
// metadata) is implementation detail and carries no compatibility
// guarantee.
package atlas
