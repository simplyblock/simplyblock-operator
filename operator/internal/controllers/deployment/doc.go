// Package deployment reconciles the two kinds a simplyblock deployment is
// described and produced by: ClusterDeploymentConfig, the reviewable document,
// and OperatorOps, whose discovery action writes one.
//
// The two belong together because one produces the other. A discovery run
// inspects the workers of a cluster and writes a draft; a reviewer approves it;
// the expansion turns it into a StorageCluster and its StorageNodes. Splitting
// them would put the two halves of that handoff in separate packages with
// nothing between them but the API.
//
// # Why this sits beside internal/controller rather than in it
//
// design-crd-model.md §7.10 gives the target layout: one package per domain
// under internal/controllers, where a kind's design document, its controller
// package, and its band of the model name the same thing. Everything written
// before that decision is in the flat internal/controller package and is being
// moved a domain at a time; this is the first domain to land, so the two
// packages coexist and the flat one is not the pattern to copy.
//
// operator/docs/designs/crd-redesign/design-clusterdeploymentconfig.md is what
// the kinds here are specified by.
package deployment
