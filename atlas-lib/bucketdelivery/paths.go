// The one path both sides of bucket delivery have to agree on without
// calling each other: the CSI node plugin, which bind-mounts a delivered
// bucket there (bucketdeliveryrpc.Deliverer.Deliver), and the operator's
// ObjectStoreReconciler, which has to mount every gateway pod's hostPath at
// the identical path for the bind mount to ever become visible inside one
// (design-bucket-provisioning.md §5.1, §5.4). Living in atlas-lib rather
// than in either consumer is what keeps the two from drifting apart: a
// change here is one edit, not two coordinated ones.

package bucketdelivery

// GatewayMountRoot is the one shared, fixed hostPath every gateway pod of
// every ObjectStore mounts with mountPropagation: HostToContainer. The CSI
// node plugin's own NodePublishVolume (driven by DeliverBucket) places a
// delivered bucket's mount at a deterministic subdirectory of this path, and
// that mount then propagates into every already-running gateway pod on the
// same node without restarting any of them.
//
// A bucket's actual target path is this root plus the delivery's own
// MountPrefix (bucketdeliveryrpc.Bucket.MountPrefix) plus the bucket name:
// every gateway pod still mounts the same root, but each ObjectStore's
// gateway is only ever given GATEWAY_ROOT_DIR set to its own prefixed
// subdirectory of it, so one ObjectStore's buckets are invisible to
// another's gateway process sharing the same node
// (issues/19-mc-smoke-test-bucket-not-found.md, tasks.md D4). The root
// itself carries no isolation. The prefix is what supplies it.
//
// It is a subdirectory of /var/lib/kubelet/plugins, one of the three
// hostPath roots the CSI node plugin's own DaemonSet pod template mounts
// Bidirectional (0004-direct-csi-mount-spike-result.md): a path outside
// those three stays inside the node plugin's own mount namespace and
// reaches neither the host nor any other pod.
const GatewayMountRoot = "/var/lib/kubelet/plugins/csi.simplyblock.io/s3-gateway-mounts"
