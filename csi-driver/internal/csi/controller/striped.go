// Striped volumes on the controller side: a volume made of several logical
// volumes of equal size, each on a different storage node of one cluster, which
// the node service stripes into one block device with LVM
// (csi-driver/docs/striped-volumes.md).
//
// The control plane has no notion of such a volume. What holds the members
// together is the first member's identity and a naming rule: the first member
// carries the claim's volume name exactly as an ordinary volume would, and member
// i carries that name with -s{i} appended. The volume id is the first member's
// handle with the width appended (lvol.ParseStripedHandle), so the RPCs that
// receive nothing but the id know it is striped without asking anybody, and find
// the other members by name. A consistency group is not used for the grouping:
// one is confined to a single logical-volume store, and a striped volume's
// members live on different nodes by construction.
//
// Every operation here acts on all members or on none of them. A create or a
// snapshot that cannot finish removes what it made before it returns the error,
// and a retry finds what an earlier attempt left by name and makes only what is
// missing.
package controller

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	atlascp "github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvol"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog"

	"github.com/simplyblock/csi-driver/internal/clusters"
	"github.com/simplyblock/csi-driver/internal/controlplane"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

const (
	// defaultStripeChunkBytes is the chunk each member takes before the next one
	// does, when the class names none. 64KiB is LVM's own default and keeps a
	// 4KiB random write on one member.
	defaultStripeChunkBytes = int64(64 << 10)

	// The bounds on stripe_size. LVM takes any power of two from the page size
	// up. Past 1MiB a stripe stops spreading the I/O of a single request at all,
	// which is the reason to stripe.
	minStripeChunkBytes = int64(4 << 10)
	maxStripeChunkBytes = int64(1 << 20)

	// stripeMemberSuffix is what the second and later members' names carry
	// after the volume's own name, followed by the member's position.
	stripeMemberSuffix = "-s"

	// lvolOnline is the status the control plane reports for a storage node or
	// a logical volume that is serving.
	lvolOnline = "online"

	// stripeRollbackTimeout bounds the removal of what a failed create or
	// snapshot made. It runs on a context of its own, because the RPC's may
	// be what failed.
	stripeRollbackTimeout = 2 * time.Minute
)

// stripeSpec is what a class asks of a striped volume.
type stripeSpec struct {
	width      int
	chunkBytes int64
}

// striped reports whether the class asked for a striped volume at all.
func (s stripeSpec) striped() bool { return s.width > 1 }

// parseStripeSpec reads stripe_width and stripe_size from a class's parameters.
//
// An absent, empty, 0, or 1 width is an ordinary volume, and stripe_size is then
// not read at all, so a class that never asked for striping is provisioned
// exactly as before this existed. Anything else that is not a width from 2 to
// lvol.MaxStripeWidth, or a chunk that is not a power of two from 4KiB to 1MiB,
// is the class's mistake and InvalidArgument.
func parseStripeSpec(params map[string]string) (stripeSpec, error) {
	raw := strings.TrimSpace(params[kube.ParamStripeWidth])
	if raw == "" {
		return stripeSpec{width: 1}, nil
	}
	width, err := strconv.Atoi(raw)
	if err != nil || width < 0 {
		return stripeSpec{}, status.Errorf(codes.InvalidArgument,
			"%s must be a whole number of storage nodes, not %q", kube.ParamStripeWidth, raw)
	}
	if width <= 1 {
		return stripeSpec{width: 1}, nil
	}
	if width > lvol.MaxStripeWidth {
		return stripeSpec{}, status.Errorf(codes.InvalidArgument,
			"%s is %d, and LVM stripes across at most %d members", kube.ParamStripeWidth, width, lvol.MaxStripeWidth)
	}

	spec := stripeSpec{width: width, chunkBytes: defaultStripeChunkBytes}
	if rawChunk := strings.TrimSpace(params[kube.ParamStripeSize]); rawChunk != "" {
		q, err := resource.ParseQuantity(rawChunk)
		if err != nil {
			return stripeSpec{}, status.Errorf(codes.InvalidArgument,
				"%s must be a size such as 64Ki, not %q: %v", kube.ParamStripeSize, rawChunk, err)
		}
		chunk := q.Value()
		if chunk < minStripeChunkBytes || chunk > maxStripeChunkBytes || chunk&(chunk-1) != 0 {
			return stripeSpec{}, status.Errorf(codes.InvalidArgument,
				"%s is %d bytes, and has to be a power of two from 4Ki to 1Mi", kube.ParamStripeSize, chunk)
		}
		spec.chunkBytes = chunk
	}
	return spec, nil
}

// refuseStripedClass refuses the class features a striped volume cannot have
// yet. Each is a feature that acts on one logical volume where a striped volume
// has several, and would silently act on the first member alone.
//
// Encryption, QoS, the namespace sharing of max_namespace_per_subsys, and a
// placement annotation are not refused: each applies to every member on its own
// and means the same there.
func refuseStripedClass(req *csi.CreateVolumeRequest, pnfs bool) error {
	params := req.GetParameters()
	refuse := func(what string) error {
		return status.Errorf(codes.InvalidArgument,
			"%s is set, and %s is not supported for a striped volume", kube.ParamStripeWidth, what)
	}
	if pnfs {
		// The export is made of one backing volume's filesystem.
		return refuse("a pNFS export")
	}
	if vdoCompression, _ := strconv.ParseBool(params[kube.ParamClientCompression]); vdoCompression {
		return refuse("client-side compression")
	}
	if vdoDedup, _ := strconv.ParseBool(params[kube.ParamClientDeduplication]); vdoDedup {
		return refuse("client-side deduplication")
	}
	// Replication pairs one logical volume with one on another cluster, and a
	// fail-over of a striped volume would have to promote every member to the
	// same point in time, which nothing provides.
	if replicate, _ := strconv.ParseBool(params[kube.ParamReplication]); replicate {
		return refuse("replication")
	}
	return nil
}

// refuseStriped answers an RPC that cannot act on a striped volume, before it
// acts on the first member alone.
func refuseStriped(volumeID, what string) error {
	if width := csicommon.StripeWidth(volumeID); width > 1 {
		return status.Errorf(codes.FailedPrecondition,
			"volume %s is striped across %d storage nodes, and %s is not supported for striped volumes",
			volumeID, width, what)
	}
	return nil
}

// stripeMemberName is what member i of a striped volume named name is called on
// the control plane. The first member carries the volume's own name, which is
// what keeps an ordinary volume's idempotency and its PersistentVolume lookups
// working unchanged for it.
func stripeMemberName(name string, i int) string {
	if i == 0 {
		return name
	}
	return name + stripeMemberSuffix + strconv.Itoa(i)
}

// stripeMemberBytes is the size of each member of a striped volume asked to hold
// capacity bytes: an equal share, rounded up to the control plane's GiB. The
// volume reports width times that, which is at least what was asked for.
//
// LVM keeps a little of each member for its own metadata, at most a few MiB, so
// the striped device is that much short of the reported size. The rounding up to
// whole GiB per member covers it for every width that does not divide the
// request evenly.
func stripeMemberBytes(capacity int64, width int) int64 {
	share := (capacity + int64(width) - 1) / int64(width)
	return alignToGiBBytes(share)
}

// setStripeContext writes what the node service stages a striped volume from
// into its volume context. The member order written here is the order the node
// stripes them in and is never rewritten.
//
// XFS gets the stripe's geometry from the node's filesystem layer, which reads
// it off the striped logical volume, so nothing about it is written here.
func setStripeContext(vc map[string]string, members []string, spec stripeSpec) {
	vc[csicommon.CtxStripeWidth] = strconv.Itoa(spec.width)
	vc[csicommon.CtxStripeMembers] = strings.Join(members, ",")
	vc[csicommon.CtxStripeChunkBytes] = strconv.FormatInt(spec.chunkBytes, 10)
}

// listStorageNodes reads a cluster's storage nodes from its control plane.
func listStorageNodes(ctx context.Context, clusterID string) ([]atlascp.StorageNode, error) {
	client, err := clusters.ReplicationClient(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	return client.ListStorageNodes(ctx, clusterID)
}

// onlineStorageNodes is the storage nodes of the cluster that a member may be
// placed on: those the control plane reports online.
func (cs *Server) onlineStorageNodes(ctx context.Context, clusterID string) ([]atlascp.StorageNode, error) {
	list := cs.storageNodes
	if list == nil {
		list = listStorageNodes
	}
	nodes, err := list(ctx, clusterID)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable,
			"list the storage nodes of cluster %s to place a striped volume: %v", clusterID, err)
	}
	online := make([]atlascp.StorageNode, 0, len(nodes))
	for _, n := range nodes {
		if strings.EqualFold(strings.TrimSpace(n.Status), lvolOnline) {
			online = append(online, n)
		}
	}
	return online, nil
}

// pickStripeHosts chooses the storage node every member of a striped volume is
// created on, one node per member and no node twice.
//
// taken holds the members that already exist and the node each is on, which a
// retry has to keep. pinned is the node an explicit placement asks for, which
// goes to the first member, since the first member is the one a placement
// annotation has always meant.
//
// The rest is deterministic, so a retried create chooses the same nodes: the
// nodes are taken round robin across failure domains, so consecutive members
// land in different ones where the cluster has them, and the starting point is
// a hash of the volume's name, so many striped volumes do not all start on the
// same node.
func pickStripeHosts(
	nodes []atlascp.StorageNode, width int, name, pinned string, taken map[int]string,
) ([]string, error) {
	hosts := make([]string, width)
	used := make(map[string]bool, width)
	for i, id := range taken {
		if used[id] {
			return nil, status.Errorf(codes.Internal,
				"two existing members of striped volume %s are on storage node %s", name, id)
		}
		hosts[i], used[id] = id, true
	}

	online := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		online[n.ID] = true
	}
	if pinned != "" && hosts[0] == "" {
		if !online[pinned] {
			return nil, status.Errorf(codes.InvalidArgument,
				"striped volume %s is pinned to storage node %s, which is not an online storage node of the cluster",
				name, pinned)
		}
		if used[pinned] {
			return nil, status.Errorf(codes.InvalidArgument,
				"striped volume %s is pinned to storage node %s, which already holds another of its members",
				name, pinned)
		}
		hosts[0], used[pinned] = pinned, true
	}

	order := spreadAcrossFailureDomains(nodes)
	if len(order) > 0 {
		h := fnv.New32a()
		_, _ = h.Write([]byte(name))
		start := int(h.Sum32() % uint32(len(order)))
		order = append(order[start:], order[:start]...)
	}
	candidates := slices.DeleteFunc(order, func(id string) bool { return used[id] })

	missing := 0
	for _, host := range hosts {
		if host == "" {
			missing++
		}
	}
	if missing > len(candidates) {
		return nil, status.Errorf(codes.InvalidArgument,
			"%s is %d, and cluster has %d online storage nodes to place them on; "+
				"a striped volume needs one storage node per member",
			kube.ParamStripeWidth, width, len(used)+len(candidates))
	}
	for i := range hosts {
		if hosts[i] == "" {
			hosts[i], candidates = candidates[0], candidates[1:]
		}
	}
	return hosts, nil
}

// spreadAcrossFailureDomains orders node ids so that consecutive ones come from
// different failure domains for as long as there are domains left: one from
// each domain in turn, the domains in ascending order and the nodes in each by
// id. A cluster without failure domains reports every node in domain zero, and
// the order is then just the ids sorted.
func spreadAcrossFailureDomains(nodes []atlascp.StorageNode) []string {
	byDomain := map[int][]string{}
	for _, n := range nodes {
		byDomain[n.FailureDomain] = append(byDomain[n.FailureDomain], n.ID)
	}
	domains := make([]int, 0, len(byDomain))
	for d, ids := range byDomain {
		slices.Sort(ids)
		domains = append(domains, d)
	}
	slices.Sort(domains)

	order := make([]string, 0, len(nodes))
	for round := 0; len(order) < len(nodes); round++ {
		for _, d := range domains {
			if round < len(byDomain[d]) {
				order = append(order, byDomain[d][round])
			}
		}
	}
	return order
}

// membersByName finds the members of the striped volume named name that exist,
// by the naming rule, in member order, with nil for one that does not.
func membersByName(
	ctx context.Context, sbclient controlplane.ClusterAPI, name string, width int,
) ([]*controlplane.LvolResp, error) {
	volumes, err := sbclient.ListVolumes(ctx)
	if err != nil {
		return nil, err
	}
	members := make([]*controlplane.LvolResp, width)
	for i := range members {
		want := stripeMemberName(name, i)
		for _, v := range volumes {
			if v.Name == want {
				members[i] = v
				break
			}
		}
	}
	return members, nil
}

// resolveStripeMembers finds the members of the striped volume whose first
// member is firstID: the first by its id, the rest by the name the first one
// carries. It reports controlplane.ErrVolumeNotFound when the first member is
// gone, which is a volume already deleted, since a deletion removes the first
// member last.
func resolveStripeMembers(
	ctx context.Context, sbclient controlplane.ClusterAPI, firstID string, width int,
) ([]*controlplane.LvolResp, error) {
	volumes, err := sbclient.ListVolumes(ctx)
	if err != nil {
		return nil, err
	}
	var first *controlplane.LvolResp
	for _, v := range volumes {
		if v.UUID == firstID {
			first = v
			break
		}
	}
	if first == nil {
		return nil, fmt.Errorf("first member %s of a striped volume: %w", firstID, controlplane.ErrVolumeNotFound)
	}
	members := make([]*controlplane.LvolResp, width)
	members[0] = first
	for i := 1; i < width; i++ {
		want := stripeMemberName(first.Name, i)
		for _, v := range volumes {
			if v.Name == want {
				members[i] = v
				break
			}
		}
	}
	return members, nil
}

// requireAllMembers refuses an operation on a striped volume one of whose
// members is missing. The volume cannot serve without it, and resizing or
// snapshotting the rest would only make the members disagree further.
func requireAllMembers(volumeID string, members []*controlplane.LvolResp) error {
	for i, m := range members {
		if m == nil {
			return status.Errorf(codes.FailedPrecondition,
				"striped volume %s has no member %d on the control plane", volumeID, i)
		}
	}
	return nil
}

// divideQoS splits each of the class's ceilings evenly across the members,
// rounded up, so the volume as a whole is held to what the class says rather
// than to width times it. A ceiling that is unset or not a number is left as it
// is, which is unset or the control plane's to refuse.
func divideQoS(data *controlplane.CreateLVolData, width int) {
	for _, ceiling := range []*string{&data.MaxRWIOPS, &data.MaxRWmBytes, &data.MaxRmBytes, &data.MaxWmBytes} {
		v, err := strconv.ParseInt(strings.TrimSpace(*ceiling), 10, 64)
		if err != nil || v <= 0 {
			continue
		}
		*ceiling = strconv.FormatInt((v+int64(width)-1)/int64(width), 10)
	}
}

// provisionVolume makes the volume a CreateVolume asks for: a striped one when
// the class sets a stripe_width of 2 or more, and an ordinary one, by exactly
// the path it always took, otherwise.
func (cs *Server) provisionVolume(
	ctx context.Context,
	req *csi.CreateVolumeRequest,
	sbclient *controlplane.ClusterClient,
	pnfs bool,
) (*csi.Volume, error) {
	spec, err := parseStripeSpec(req.GetParameters())
	if err != nil {
		return nil, err
	}
	if !spec.striped() {
		return cs.createVolume(ctx, req, sbclient)
	}
	if err := refuseStripedClass(req, pnfs); err != nil {
		return nil, err
	}
	return cs.createStripedVolume(ctx, req, sbclient, spec)
}

// createStripedVolume creates every member of a striped volume, or clones every
// member of a striped source, and returns the volume they make.
//
// It publishes every member but the first, which CreateVolume publishes along
// with reading its connection, as it does for an ordinary volume.
func (cs *Server) createStripedVolume(
	ctx context.Context,
	req *csi.CreateVolumeRequest,
	sbclient *controlplane.ClusterClient,
	spec stripeSpec,
) (*csi.Volume, error) {
	// A stretched cluster leads every volume from one site, and the gate that
	// keeps it there knows one volume where this has several.
	if birthSite(sbclient.ClusterID()) != "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"cluster %s is a stretched, sync-replicated cluster, and striped volumes are not supported on one",
			sbclient.ClusterID())
	}

	size := req.GetCapacityRange().GetRequiredBytes()
	if size == 0 {
		size = gib
	}
	memberBytes := stripeMemberBytes(size, spec.width)

	vol := &csi.Volume{
		CapacityBytes: memberBytes * int64(spec.width),
		VolumeContext: req.GetParameters(),
		ContentSource: req.GetVolumeContentSource(),
	}
	if vol.VolumeContext == nil {
		vol.VolumeContext = map[string]string{}
	}

	var (
		members []string
		home    *controlplane.ClusterClient
		err     error
	)
	if req.GetVolumeContentSource() != nil {
		members, home, err = cs.cloneStripedMembers(ctx, req, spec, memberBytes)
	} else {
		members, err = cs.createStripedMembers(ctx, req, sbclient, spec, memberBytes, vol.VolumeContext)
		home = sbclient
	}
	if err != nil {
		return nil, err
	}
	for i, id := range members[1:] {
		if err := home.PublishVolume(ctx, id); err != nil {
			return nil, fmt.Errorf("publish member %d (%s) of striped volume %s: %w", i+1, id, req.GetName(), err)
		}
	}

	first := lvol.Handle{ClusterID: home.ClusterID(), PoolRef: home.PoolID(), VolumeID: members[0]}
	vol.VolumeId = first.StripedString(spec.width)
	setStripeContext(vol.VolumeContext, members, spec)
	klog.Infof("created striped volume %s: %d members of %d bytes: %v",
		vol.GetVolumeId(), spec.width, memberBytes, members)
	return vol, nil
}

// createStripedMembers creates the members of a new striped volume, each on a
// different storage node, and returns their ids in member order.
//
// A member an earlier attempt already made is found by name and kept, with the
// node it is on, and only the missing ones are created. A member left behind
// that is not online will never be, and is replaced. If any member cannot be
// created, every member is removed before the error is returned, so a failed
// create leaves nothing for anybody to find.
func (cs *Server) createStripedMembers(
	ctx context.Context,
	req *csi.CreateVolumeRequest,
	sbclient *controlplane.ClusterClient,
	spec stripeSpec,
	memberBytes int64,
	vc map[string]string,
) ([]string, error) {
	base, podAffinitive, err := cs.prepareCreateVolumeReq(ctx, req, memberBytes)
	if err != nil {
		return nil, err
	}
	if base.ConsistencyGroup != "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"the claim joins consistency group %q, and a striped volume cannot be a member of one: "+
				"a consistency group is confined to one storage node's volume store", base.ConsistencyGroup)
	}
	pinned := base.HostID
	if podAffinitive && pinned == "" {
		pinned = coLocatedHostID(req.GetAccessibilityRequirements(), sbclient.ClusterID())
	}

	// What the class asked for, for the whole volume, before the members are
	// given their shares of it.
	vc["qos_rw_iops"] = base.MaxRWIOPS
	vc["qos_rw_mbytes"] = base.MaxRWmBytes
	vc["qos_r_mbytes"] = base.MaxRmBytes
	vc["qos_w_mbytes"] = base.MaxWmBytes
	divideQoS(base, spec.width)

	name := req.GetName()
	existing, err := membersByName(ctx, sbclient, name, spec.width)
	if err != nil {
		return nil, err
	}
	taken := map[int]string{}
	for i, m := range existing {
		if m == nil {
			continue
		}
		if !strings.EqualFold(m.Status, lvolOnline) {
			klog.Warningf("striped volume %s: deleting member %d (%s), left %s by an earlier attempt",
				name, i, m.UUID, m.Status)
			if err := sbclient.DeleteVolume(ctx, m.UUID); err != nil && !errors.Is(err, controlplane.ErrVolumeNotFound) {
				return nil, err
			}
			existing[i] = nil
			continue
		}
		if m.LvolSize != memberBytes {
			return nil, status.Errorf(codes.AlreadyExists,
				"striped volume %q exists with member %d of %d bytes, and this request needs %d",
				name, i, m.LvolSize, memberBytes)
		}
		info, err := sbclient.VolumeNodeInfo(ctx, m.UUID)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable,
				"read which storage node member %d of striped volume %s is on: %v", i, name, err)
		}
		taken[i] = info.NodeID
	}

	nodes, err := cs.onlineStorageNodes(ctx, sbclient.ClusterID())
	if err != nil {
		return nil, err
	}
	hosts, err := pickStripeHosts(nodes, spec.width, name, pinned, taken)
	if err != nil {
		return nil, err
	}

	members := make([]string, spec.width)
	for i, m := range existing {
		if m != nil {
			members[i] = m.UUID
		}
	}
	for i := range members {
		if members[i] != "" {
			continue
		}
		data := *base
		data.LvolName = stripeMemberName(name, i)
		data.HostID = hosts[i]
		if i > 0 {
			// An lvol id an annotation asks for names one volume, and that is
			// the first member.
			data.LvolID = ""
		}
		id, err := sbclient.CreateVolume(ctx, &data)
		if err != nil {
			cs.removeStripeMembers(sbclient, name, members)
			if errors.Is(err, controlplane.ErrSiteConflict) {
				return nil, status.Errorf(codes.FailedPrecondition, "create member %d of %s: %v", i, name, err)
			}
			return nil, fmt.Errorf("create member %d of striped volume %s on storage node %s: %w", i, name, hosts[i], err)
		}
		members[i] = id
	}
	return members, nil
}

// removeStripeMembers deletes the members a failed operation made, last member
// first, and logs what it could not delete rather than masking the error that
// made it necessary.
func (cs *Server) removeStripeMembers(sbclient controlplane.ClusterAPI, name string, members []string) {
	ctx, cancel := context.WithTimeout(context.Background(), stripeRollbackTimeout)
	defer cancel()
	for i := len(members) - 1; i >= 0; i-- {
		if members[i] == "" {
			continue
		}
		if err := sbclient.DeleteVolume(ctx, members[i]); err != nil && !errors.Is(err, controlplane.ErrVolumeNotFound) {
			klog.Errorf("striped volume %s: could not remove member %d (%s) after a failed create: %v",
				name, i, members[i], err)
		}
	}
}

// deleteStripedVolume deletes every member of a striped volume, the first one
// last, so that an interrupted delete is retried with the first member still
// there to find the rest by. Members that are already gone are skipped, which
// is what makes a retry finish.
func (cs *Server) deleteStripedVolume(
	ctx context.Context, volumeID string, handle *lvol.Handle, width int,
) (*csi.DeleteVolumeResponse, error) {
	sbclient, err := clusters.Client(ctx, handle.ClusterID, handle.PoolRef)
	if err != nil {
		if errors.Is(err, controlplane.ErrClusterNotFound) {
			klog.Warningf("cluster for striped volume %s no longer managed, treating as already deleted: %v",
				volumeID, err)
			return &csi.DeleteVolumeResponse{}, nil
		}
		return nil, classifyDeleteVolumeError(err)
	}
	members, err := resolveStripeMembers(ctx, sbclient, handle.VolumeID, width)
	if errors.Is(err, controlplane.ErrVolumeNotFound) {
		klog.Warningf("striped volume %s: first member is gone, treating as already deleted", volumeID)
		return &csi.DeleteVolumeResponse{}, nil
	}
	if err != nil {
		return nil, classifyDeleteVolumeError(err)
	}
	for i := len(members) - 1; i >= 0; i-- {
		m := members[i]
		if m == nil {
			continue
		}
		err := sbclient.UnpublishVolume(ctx, m.UUID)
		gone := errors.Is(err, controlplane.ErrVolumeUnpublished) || errors.Is(err, controlplane.ErrVolumeNotFound)
		if err != nil && !gone {
			klog.Errorf("striped volume %s: unpublish member %d (%s): %v", volumeID, i, m.UUID, err)
			return nil, classifyDeleteVolumeError(err)
		}
		if err := sbclient.DeleteVolume(ctx, m.UUID); err != nil && !errors.Is(err, controlplane.ErrVolumeNotFound) {
			klog.Errorf("striped volume %s: delete member %d (%s): %v", volumeID, i, m.UUID, err)
			return nil, classifyDeleteVolumeError(err)
		}
	}
	return &csi.DeleteVolumeResponse{}, nil
}

// expandStripedVolume grows every member to an equal share of the new capacity
// and reports what the members now add up to. A member already at its share is
// left alone, so a retry after a partial expansion resizes only the rest.
func (cs *Server) expandStripedVolume(
	ctx context.Context, volumeID string, handle *lvol.Handle, width int, required int64,
) (*csi.ControllerExpandVolumeResponse, error) {
	sbclient, err := clusters.Client(ctx, handle.ClusterID, handle.PoolRef)
	if err != nil {
		return nil, err
	}
	members, err := resolveStripeMembers(ctx, sbclient, handle.VolumeID, width)
	if err != nil {
		return nil, classifyControllerExpandVolumeError(err)
	}
	if err := requireAllMembers(volumeID, members); err != nil {
		return nil, err
	}

	memberBytes := stripeMemberBytes(required, width)
	for i, m := range members {
		if m.LvolSize >= memberBytes {
			continue
		}
		if err := sbclient.ResizeVolume(ctx, m.UUID, memberBytes); err != nil {
			klog.Errorf("striped volume %s: resize member %d (%s) to %d: %v", volumeID, i, m.UUID, memberBytes, err)
			return nil, classifyControllerExpandVolumeError(err)
		}
	}

	// The node grows the physical volumes, then the striped logical volume
	// across them, then the filesystem.
	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         memberBytes * int64(width),
		NodeExpansionRequired: true,
	}, nil
}

// memberSnapshot is one member's snapshot as a striped snapshot holds it. A
// snapshot found rather than taken, which a crash or a timed-out call left, has
// its UUID and an error: it is removed with the rest and never used.
type memberSnapshot struct {
	csiID string
	uuid  string
	err   error
}

// createStripedSnapshot snapshots every member of a striped volume at one point
// in its write stream.
//
// The members are on different storage nodes, and the control plane has no
// snapshot that is atomic across them: a consistency group is confined to one
// node's volume store. So the consistency comes from the host instead. The node
// that stages the volume suspends the stripe's device for the length of the
// snapshot (underFreeze): I/O in flight drains, new I/O waits, and a mounted
// filesystem is frozen and flushed. Every member then holds the same moment, and
// the member snapshots, taken concurrently while it lasts, are a crash-consistent
// image of the whole volume. A volume staged nowhere has no writer and needs no
// freeze.
//
// Anything short of that is not returned as a snapshot. If the freeze cannot be
// taken, if a member's snapshot fails, or if the freeze ended before every
// member was snapshotted, the member snapshots are deleted and the error is one
// the snapshotter retries.
//
// Every attempt takes every member afresh. Member snapshots an earlier attempt
// left behind were not necessarily taken under the same freeze, so they are
// deleted first rather than reused.
func (cs *Server) createStripedSnapshot(
	ctx context.Context, req *csi.CreateSnapshotRequest, handle *lvol.Handle, width int,
) (*csi.CreateSnapshotResponse, error) {
	volumeID := req.GetSourceVolumeId()
	sbclient, err := clusters.Client(ctx, handle.ClusterID, handle.PoolRef)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	members, err := resolveStripeMembers(ctx, sbclient, handle.VolumeID, width)
	if err != nil {
		return nil, classifyCreateSnapshotError(err)
	}
	if err := requireAllMembers(volumeID, members); err != nil {
		return nil, err
	}
	if err := clearMemberSnapshots(ctx, sbclient, req.GetName(), members); err != nil {
		return nil, err
	}

	taken := make([]memberSnapshot, width)
	err = cs.underFreeze(ctx, volumeID, func(ctx context.Context) error {
		var wg sync.WaitGroup
		for i, m := range members {
			wg.Go(func() { taken[i] = takeMemberSnapshot(ctx, sbclient, m.UUID, stripeMemberName(req.GetName(), i)) })
		}
		wg.Wait()
		for i, t := range taken {
			if t.err != nil {
				return fmt.Errorf("member %d of %s: %w", i, volumeID, t.err)
			}
		}
		return nil
	})
	if err != nil {
		klog.Errorf("striped snapshot %s of %s: %v", req.GetName(), volumeID, err)
		cs.removeMemberSnapshots(sbclient, req.GetName(), taken)
		if _, isStatus := status.FromError(err); isStatus {
			return nil, err
		}
		return nil, classifyCreateSnapshotError(err)
	}

	var size int64
	for _, m := range members {
		size += m.LvolSize
	}
	return &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			SizeBytes:      size,
			SnapshotId:     lvol.WithStripeMarker(taken[0].csiID, width),
			SourceVolumeId: handle.VolumeID,
			CreationTime:   timestamppb.Now(),
			ReadyToUse:     true,
		},
	}, nil
}

// takeMemberSnapshot snapshots one member. A conflict on the name means a
// snapshot of that name appeared after clearMemberSnapshots ran, which only a
// call from an earlier attempt that completed late does: it was not taken under
// this freeze, so it is recorded for removal and the attempt fails.
func takeMemberSnapshot(ctx context.Context, sbclient controlplane.ClusterAPI, lvolID, name string) memberSnapshot {
	id, err := sbclient.CreateSnapshot(ctx, lvolID, name)
	if err != nil {
		if !classifyCreateSnapshotError(err).IsIdempotent() {
			return memberSnapshot{err: err}
		}
		existing, lerr := existingMemberSnapshot(ctx, sbclient, lvolID, name)
		if lerr != nil {
			return memberSnapshot{err: lerr}
		}
		ms := memberSnapshot{err: status.Errorf(codes.Aborted,
			"snapshot %q appeared during the snapshot, from an earlier attempt", name)}
		if parsed, perr := parseSnapshotID(existing); perr == nil {
			ms.uuid = parsed.snapshotID
		}
		return ms
	}
	ms := memberSnapshot{csiID: id}
	if parsed, perr := parseSnapshotID(id); perr == nil {
		ms.uuid = parsed.snapshotID
	}
	return ms
}

// clearMemberSnapshots deletes the member snapshots of this name an earlier
// attempt left. One of that name taken of anything else is a real conflict.
func clearMemberSnapshots(
	ctx context.Context, sbclient controlplane.ClusterAPI, name string, members []*controlplane.LvolResp,
) error {
	snaps, err := sbclient.ListSnapshots(ctx)
	if err != nil {
		return classifyCreateSnapshotError(err)
	}
	for i, m := range members {
		want := stripeMemberName(name, i)
		for _, s := range snaps {
			if s.Name != want {
				continue
			}
			if lvolIDFromURL(s.LvolURL) != m.UUID {
				return status.Errorf(codes.AlreadyExists,
					"snapshot %q already exists with a different source volume", want)
			}
			klog.Warningf("striped snapshot %s: deleting member %d (%s) left by an earlier attempt", name, i, s.UUID)
			if err := sbclient.DeleteSnapshot(ctx, s.UUID); err != nil {
				return classifyCreateSnapshotError(err)
			}
		}
	}
	return nil
}

// existingMemberSnapshot is the snapshot named name of member lvolID that the
// control plane answered a conflict for. One of that name taken of anything
// else is a real conflict.
func existingMemberSnapshot(
	ctx context.Context, sbclient controlplane.ClusterAPI, lvolID, name string,
) (string, error) {
	snaps, err := sbclient.ListSnapshots(ctx)
	if err != nil {
		return "", err
	}
	for _, s := range snaps {
		if s.Name != name {
			continue
		}
		if lvolIDFromURL(s.LvolURL) != lvolID {
			return "", status.Errorf(codes.AlreadyExists,
				"snapshot %q already exists with a different source volume", name)
		}
		return fmt.Sprintf("%s:%s:%s", sbclient.ClusterID(), sbclient.PoolID(), s.UUID), nil
	}
	return "", status.Errorf(codes.Internal, "snapshot %q reported as existing but was not found", name)
}

// removeMemberSnapshots deletes the member snapshots a failed striped snapshot
// took or found.
func (cs *Server) removeMemberSnapshots(sbclient controlplane.ClusterAPI, name string, taken []memberSnapshot) {
	ctx, cancel := context.WithTimeout(context.Background(), stripeRollbackTimeout)
	defer cancel()
	for i := len(taken) - 1; i >= 0; i-- {
		if taken[i].uuid == "" {
			continue
		}
		if err := sbclient.DeleteSnapshot(ctx, taken[i].uuid); err != nil {
			klog.Errorf("striped snapshot %s: could not remove member %d (%s) after a failed snapshot: %v",
				name, i, taken[i].uuid, err)
		}
	}
}

// resolveMemberSnapshots finds the member snapshots of the striped snapshot
// whose first member snapshot is firstID, in member order, with nil for one
// that is gone. It reports controlplane.ErrSnapshotNotFound when the first one
// is gone.
func resolveMemberSnapshots(
	ctx context.Context, sbclient controlplane.ClusterAPI, firstID string, width int,
) ([]*controlplane.SnapshotResp, error) {
	snaps, err := sbclient.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	var first *controlplane.SnapshotResp
	for _, s := range snaps {
		if s.UUID == firstID {
			first = s
			break
		}
	}
	if first == nil {
		return nil, fmt.Errorf("first member %s of a striped snapshot: %w", firstID, controlplane.ErrSnapshotNotFound)
	}
	members := make([]*controlplane.SnapshotResp, width)
	members[0] = first
	for i := 1; i < width; i++ {
		want := stripeMemberName(first.Name, i)
		for _, s := range snaps {
			if s.Name == want {
				members[i] = s
				break
			}
		}
	}
	return members, nil
}

// deleteStripedSnapshot deletes every member snapshot, the first one last, for
// the reason deleteStripedVolume deletes the first member last.
func (cs *Server) deleteStripedSnapshot(
	ctx context.Context, csiSnapshotID string, snapshot *spdkSnapshot, width int,
) (*csi.DeleteSnapshotResponse, error) {
	sbclient, err := clusters.Client(ctx, snapshot.clusterID, snapshot.poolID)
	if err != nil {
		if errors.Is(err, controlplane.ErrClusterNotFound) {
			klog.Warningf("cluster for striped snapshot %s no longer managed, treating as already deleted: %v",
				csiSnapshotID, err)
			return &csi.DeleteSnapshotResponse{}, nil
		}
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	members, err := resolveMemberSnapshots(ctx, sbclient, snapshot.snapshotID, width)
	if errors.Is(err, controlplane.ErrSnapshotNotFound) {
		klog.Warningf("striped snapshot %s: first member is gone, treating as already deleted", csiSnapshotID)
		return &csi.DeleteSnapshotResponse{}, nil
	}
	if err != nil {
		return nil, classifyDeleteSnapshotError(err)
	}
	for i := len(members) - 1; i >= 0; i-- {
		if members[i] == nil {
			continue
		}
		if err := sbclient.DeleteSnapshot(ctx, members[i].UUID); err != nil {
			if d := classifyDeleteSnapshotError(err); !d.IsSuccess() {
				klog.Errorf("striped snapshot %s: delete member %d (%s): %v", csiSnapshotID, i, members[i].UUID, err)
				return nil, d
			}
		}
	}
	return &csi.DeleteSnapshotResponse{}, nil
}

// stripedSnapshotView is a listed snapshot as ListSnapshots reports it: the
// first member snapshot of a striped one stands for all of them.
type stripedSnapshotView struct {
	entry *controlplane.SnapshotResp
	width int
	size  int64
}

// foldStripedSnapshots folds the member snapshots of every striped snapshot into
// its first one, so a listing reports one snapshot where the control plane holds
// several, with the width it was taken at and the size of all of them.
//
// A snapshot is a member when its name is another listed snapshot's in the same
// pool with -s{i} appended. The external snapshotter names every snapshot
// snapshot-{uuid}, so that suffix is never one it chose for a snapshot of its
// own.
func foldStripedSnapshots(entries []*controlplane.SnapshotResp) []stripedSnapshotView {
	type key struct{ cluster, pool, name string }
	byName := make(map[key]*controlplane.SnapshotResp, len(entries))
	for _, e := range entries {
		byName[key{e.ClusterID, e.PoolID, e.Name}] = e
	}

	views := make([]stripedSnapshotView, 0, len(entries))
	for _, e := range entries {
		if base, ok := stripeMemberBase(e.Name); ok {
			if _, isMember := byName[key{e.ClusterID, e.PoolID, base}]; isMember {
				continue
			}
		}
		view := stripedSnapshotView{entry: e, width: 1, size: e.Size}
		for i := 1; i < lvol.MaxStripeWidth; i++ {
			member, ok := byName[key{e.ClusterID, e.PoolID, stripeMemberName(e.Name, i)}]
			if !ok {
				break
			}
			view.width++
			view.size += member.Size
		}
		views = append(views, view)
	}
	return views
}

// stripeMemberBase is the name of the volume or snapshot a member named name
// belongs to, and reports whether name is a member's name at all.
func stripeMemberBase(name string) (string, bool) {
	cut := strings.LastIndex(name, stripeMemberSuffix)
	if cut <= 0 {
		return "", false
	}
	digits := name[cut+len(stripeMemberSuffix):]
	if digits == "" || digits[0] == '0' {
		return "", false
	}
	if _, err := strconv.Atoi(digits); err != nil {
		return "", false
	}
	return name[:cut], true
}

// cloneStripedMembers makes the members of a striped volume from a striped
// snapshot or a striped volume, member i from member i of the source, and
// returns their ids with the client of the cluster they are in.
//
// A clone is placed by the control plane beside what it was taken from, so the
// members of the clone are on distinct storage nodes because the source's are.
// The source has to be as wide as the class asks for: a stripe is a fixed
// mapping of offsets to members, and data striped across three members cannot
// be read back as a stripe across two.
func (cs *Server) cloneStripedMembers(
	ctx context.Context, req *csi.CreateVolumeRequest, spec stripeSpec, memberBytes int64,
) ([]string, *controlplane.ClusterClient, error) {
	source := req.GetVolumeContentSource()
	var (
		sourceIDs []string
		sbclient  *controlplane.ClusterClient
		clone     func(ctx context.Context, sourceID, name, size, pvc string) (string, error)
		// liveSource is the volume a clone is taken of while it may be in use,
		// which is cloned under its freeze.
		liveSource string
	)

	switch {
	case source.GetSnapshot() != nil:
		snapshotID := source.GetSnapshot().GetSnapshotId()
		base, width, ok := lvol.SplitStripeMarker(snapshotID)
		if !ok {
			return nil, nil, status.Errorf(codes.NotFound, "snapshot %q not found: not a snapshot id", snapshotID)
		}
		if width != spec.width {
			return nil, nil, status.Errorf(codes.InvalidArgument,
				"snapshot %s is striped across %d members and the class asks for %s=%d; "+
					"a striped volume is restored into a class of the same width", snapshotID, width,
				kube.ParamStripeWidth, spec.width)
		}
		snapshot, err := parseSnapshotID(base)
		if err != nil {
			return nil, nil, status.Errorf(codes.NotFound, "snapshot %q not found: %v", snapshotID, err)
		}
		if sbclient, err = clusters.Client(ctx, snapshot.clusterID, req.GetParameters()["pool_name"]); err != nil {
			return nil, nil, status.Error(codes.Unavailable, err.Error())
		}
		members, err := resolveMemberSnapshots(ctx, sbclient, snapshot.snapshotID, width)
		if err != nil {
			return nil, nil, status.Errorf(codes.NotFound, "snapshot %q not found: %v", snapshotID, err)
		}
		for i, m := range members {
			if m == nil {
				return nil, nil, status.Errorf(codes.NotFound, "striped snapshot %s has no member %d", snapshotID, i)
			}
			sourceIDs = append(sourceIDs, m.UUID)
		}
		clone = func(ctx context.Context, sourceID, name, size, pvc string) (string, error) {
			return sbclient.CloneSnapshot(ctx, sourceID, name, size, pvc, "")
		}

	case source.GetVolume() != nil:
		volumeID := source.GetVolume().GetVolumeId()
		handle, width, ok := lvol.ParseStripedHandle(lvol.VolumeHandle(volumeID))
		if !ok {
			return nil, nil, status.Errorf(codes.NotFound, "source volume %q not found: not a volume handle", volumeID)
		}
		if width != spec.width {
			return nil, nil, status.Errorf(codes.InvalidArgument,
				"volume %s is striped across %d members and the class asks for %s=%d; "+
					"a striped volume is cloned into a class of the same width", volumeID, width,
				kube.ParamStripeWidth, spec.width)
		}
		var err error
		if sbclient, err = clusters.Client(ctx, handle.ClusterID, handle.PoolRef); err != nil {
			return nil, nil, status.Error(codes.Unavailable, err.Error())
		}
		members, err := resolveStripeMembers(ctx, sbclient, handle.VolumeID, width)
		if err != nil {
			return nil, nil, status.Errorf(codes.NotFound, "source volume %q not found: %v", volumeID, err)
		}
		if err := requireAllMembers(volumeID, members); err != nil {
			return nil, nil, err
		}
		for _, m := range members {
			sourceIDs = append(sourceIDs, m.UUID)
		}
		clone = func(ctx context.Context, sourceID, name, size, pvc string) (string, error) {
			return sbclient.CloneVolume(ctx, sourceID, name, size, pvc)
		}
		liveSource = volumeID

	default:
		return nil, nil, status.Errorf(codes.InvalidArgument, "%v not a proper volume source", source)
	}

	pvc, err := cs.cloneClaim(ctx, req.GetParameters())
	if err != nil {
		return nil, nil, err
	}

	name := req.GetName()
	existing, err := membersByName(ctx, sbclient, name, spec.width)
	if err != nil {
		return nil, nil, err
	}
	size := strconv.FormatInt(memberBytes, 10)
	if liveSource != "" {
		members, err := cs.cloneLiveStripe(ctx, sbclient, liveSource, name, size, pvc, sourceIDs, existing, clone)
		return members, sbclient, err
	}

	members := make([]string, spec.width)
	for i, m := range existing {
		switch {
		case m == nil:
		case !strings.EqualFold(m.Status, lvolOnline):
			klog.Warningf("striped clone %s: deleting member %d (%s), left %s by an earlier attempt",
				name, i, m.UUID, m.Status)
			if err := sbclient.DeleteVolume(ctx, m.UUID); err != nil && !errors.Is(err, controlplane.ErrVolumeNotFound) {
				return nil, nil, err
			}
		case m.LvolSize != memberBytes:
			return nil, nil, status.Errorf(codes.AlreadyExists,
				"striped volume %q exists with member %d of %d bytes, and this request needs %d",
				name, i, m.LvolSize, memberBytes)
		default:
			members[i] = m.UUID
		}
	}

	for i := range members {
		if members[i] != "" {
			continue
		}
		id, err := clone(ctx, sourceIDs[i], stripeMemberName(name, i), size, pvc)
		if err != nil {
			cs.removeStripeMembers(sbclient, name, members)
			return nil, nil, fmt.Errorf("clone member %d of striped volume %s from %s: %w", i, name, sourceIDs[i], err)
		}
		members[i] = id
	}
	return members, sbclient, nil
}

// cloneClaim is the claim a clone is recorded against, namespace/name, and
// refuses one carrying the consistency-group label: a restore claim with it
// asks for a new group formed from the clones, which a striped volume cannot
// join.
func (cs *Server) cloneClaim(ctx context.Context, params map[string]string) (string, error) {
	pvc := params[csicommon.CSIStorageNameKey]
	ns := params[csicommon.CSIStorageNamespaceKey]
	if ns == "" || pvc == "" {
		return pvc, nil
	}
	if _, labels, err := cs.fetchPVCMeta(ctx, pvc, ns); err == nil && labels[consistencyGroupLabel] != "" {
		return "", status.Errorf(codes.InvalidArgument,
			"the claim joins consistency group %q, and a striped volume cannot be a member of one",
			labels[consistencyGroupLabel])
	}
	return ns + "/" + pvc, nil
}

// refuseStripedSourceForPlainClass refuses to make an ordinary volume out of a
// striped snapshot or volume, which would be one member's share of the data.
func refuseStripedSourceForPlainClass(source *csi.VolumeContentSource) error {
	var id string
	switch {
	case source.GetSnapshot() != nil:
		id = source.GetSnapshot().GetSnapshotId()
	case source.GetVolume() != nil:
		id = source.GetVolume().GetVolumeId()
	default:
		return nil
	}
	if _, width, ok := lvol.SplitStripeMarker(id); ok && width > 1 {
		return status.Errorf(codes.InvalidArgument,
			"%s is striped across %d members, and is restored or cloned only into a class with %s=%d",
			id, width, kube.ParamStripeWidth, width)
	}
	return nil
}

// cloneLiveStripe clones every member of a striped volume that may be in use,
// at one point in its write stream: the members are cloned concurrently under
// the source's freeze (underFreeze), as its snapshots are, so the clone is a
// crash-consistent copy of the source rather than members from different
// moments.
//
// Members an earlier attempt left were not necessarily cloned under the same
// freeze, so they are deleted and every member is cloned afresh. A clone that
// cannot finish, or whose freeze did not last, removes what it made.
func (cs *Server) cloneLiveStripe(
	ctx context.Context,
	sbclient *controlplane.ClusterClient,
	sourceVolumeID, name, size, pvc string,
	sourceIDs []string,
	existing []*controlplane.LvolResp,
	clone func(ctx context.Context, sourceID, name, size, pvc string) (string, error),
) ([]string, error) {
	for i, m := range existing {
		if m == nil {
			continue
		}
		klog.Warningf("striped clone %s: deleting member %d (%s) left by an earlier attempt", name, i, m.UUID)
		if err := sbclient.DeleteVolume(ctx, m.UUID); err != nil && !errors.Is(err, controlplane.ErrVolumeNotFound) {
			return nil, err
		}
	}

	members := make([]string, len(sourceIDs))
	errs := make([]error, len(sourceIDs))
	err := cs.underFreeze(ctx, sourceVolumeID, func(ctx context.Context) error {
		var wg sync.WaitGroup
		for i := range sourceIDs {
			wg.Go(func() { members[i], errs[i] = clone(ctx, sourceIDs[i], stripeMemberName(name, i), size, pvc) })
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				return fmt.Errorf("clone member %d of striped volume %s from %s: %w", i, name, sourceIDs[i], err)
			}
		}
		return nil
	})
	if err != nil {
		cs.removeStripeMembers(sbclient, name, members)
		return nil, err
	}
	return members, nil
}
