// Package controlplane is the client for the simplyblock control-plane
// API. Both the operator (reconciling desired state) and the CSI
// controller service (create/delete/publish volumes) talk to the same
// API, so the client and its request/response types live here once.
package controlplane
