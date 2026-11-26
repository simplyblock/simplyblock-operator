package v1alpha2

// storageNodeStatusInCreation is the lifecycle the control plane reports for a
// node it has recorded and is still creating.
const storageNodeStatusInCreation = "in_creation"

// AddInFlight reports whether the control plane is working on this node's add
// right now: the add has been asked for and has not finished.
//
// The add starts when the node takes its claim (Posting) and is not over when
// the node gets its UUID. The control plane writes the node record when
// add_node begins, with status in_creation, and finishes minutes later, so a
// node with a UUID that is still in_creation is in the second half of the same
// add.
//
// Three nodes that look like they are being added are not. A failed node keeps
// the step it failed in, and nothing is working on it. A node whose worker is
// away has left for AwaitingWorker, and what it waits for is the worker's
// drain and reboot. A node being deleted has no add left to finish.
func (n *StorageNode) AddInFlight() bool {
	if !n.DeletionTimestamp.IsZero() || n.Status.Phase == StorageNodePhaseFailed {
		return false
	}
	switch StorageNodeStep(n.Status.Step.State) {
	case StorageNodeStepPosting, StorageNodeStepResolving:
		return true
	case StorageNodeStepAwaitingWorker:
		return false
	}
	return n.Status.UUID != "" && n.Status.Status == storageNodeStatusInCreation
}
