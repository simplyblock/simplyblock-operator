// Package storagerpc serves a node's storage over a link, and reaches another
// node's over one.
//
// It is [storage.Accessor] remoted, in both directions. On the node, [NewServer]
// wraps a [storage.Local] and registers as the link agent's services. In the
// operator, [Remote] fills in the same [storage.Accessor] with clients that reach
// that node, so code written against the local resolvers runs unchanged against
// a node somewhere else in the cluster.
//
// On the node:
//
//	srv, err := storagerpc.NewServer(storage.Local(nvme.SysfsConfig{}))
//	agent, err := link.NewAgent(link.AgentConfig{
//	    Register:     srv.Register,
//	    Capabilities: storagerpc.Capabilities(),
//	    // ...
//	})
//
// In the operator:
//
//	conn, err := hub.Registry().Conn(link.NodePeer(nodeName))
//	if errors.Is(err, link.ErrNoSession) {
//	    return ctrl.Result{RequeueAfter: backoff}, nil  // not a failure
//	}
//	dev, err := storagerpc.Remote(conn).DeviceByUUID(ctx, lvolUUID)  // errs.ErrNotFound as usual
//
// # What crosses and what does not
//
// The nvme types are immutable snapshots of sysfs with no behaviour attached,
// so the wire form is a complete copy rather than a summary. Everything derived
// from a snapshot is therefore just as true on the far side:
// [nvme.Device.Accessible], [nvme.IsSibling], [nvme.CoTenants] and the rest are
// pure functions of the fields that crossed. The [storage.Accessor] questions that
// re-scan work too, by asking the same node again — at the cost of a round
// trip each.
//
// What does not cross is anything that reads the caller's own machine. The
// composition helpers in nvmeof are the ones to watch: [nvmeof.WaitForDevice]
// resolves device symlinks against the filesystem it runs on, so running it in
// the operator against a Remote silently consults the operator's /dev. Those
// compositions belong on the node, behind their own RPC, not assembled across a
// link out of these primitives.
//
// # Cost
//
// Each call is a round trip. A caller that wants several answers about the same
// node should List once and use the package-level filters in nvme over the
// snapshot. Polling loops in particular do not belong on this side of the link.
//
// Everything here is read-only. Attaching and detaching fabrics is a separate,
// mutating service, so that a credential can be granted this and not that.
package storagerpc
