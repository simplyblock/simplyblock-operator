// Package lvol models simplyblock logical-volume identity and resolves a
// logical volume two ways:
//
//	Resolver  control-plane lookup: volume metadata and the NVMe-oF
//	          Connection (NQN + endpoints) needed to attach it.
//	Mapper    node-local lookup: the /dev/nvmeXnY device backing an
//	          already-attached volume.
//
// It sits one layer above package nvme (which it imports) and is the
// natural home for the lvol <-> NVMe mapping logic shared by the operator
// and CSI driver. The Resolver interface is implemented by
// controlplane.Client; Mapper is wired by the consumer over a
// nvme.DeviceResolver.
package lvol
