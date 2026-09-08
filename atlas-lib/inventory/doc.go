// Package inventory is what there is to deploy simplyblock on, read from the
// cluster and the machines themselves.
//
// It answers the questions a storage node's placement and sizing rest on, and
// it answers them for a fleet that is not yet running one: how many CPUs each
// worker has and whether they are hyperthreaded, how much huge-page memory is
// already allocated and on which NUMA nodes, which network interfaces exist and
// how fast their links are, which disks nothing is using, and which Kubernetes
// distribution installed the kubelet. A discovery run asks exactly this, and
// the answers become the ClusterDeploymentConfig a reviewer approves.
//
// One call gathers all of it. [Collect] is the entry point, and the readers it
// composes — [ReadCPU], [ReadHugePages], [ReadInterfaces], the blockdev scan,
// and [DetectEnvironment] — are exported for the caller that wants one of them
// alone.
//
//	inv, err := inventory.Collect(ctx, inventory.Config{
//	    Kubernetes: inventory.KubernetesSources{
//	        Discovery: clientset.Discovery(),
//	        Nodes:     nodes.Items,
//	    },
//	})
//	if err != nil {
//	    // inv still holds whatever was readable; see Collect.
//	}
//	inv.Environment.Distribution   // OpenShift / Talos / K3s / Rancher / Vanilla
//	inv.AvailableDevices()         // the disks nothing is using
//	inv.CPU.HyperThreading
//
// # NUMA is the join, not a detail
//
// A storage node is pinned to a socket, and it needs its SPDK cores, its
// huge-page memory, its data NIC, and its disks all on the same side of the
// interconnect. Every reading therefore carries the memory node it belongs to,
// and [Inventory.ByNUMANode] is the join that makes the question answerable
// before anything is deployed:
//
//	for _, node := range inv.ByNUMANode() {
//	    // node.CPUs, node.HugePages, node.Interfaces, node.Devices
//	}
//
// # Reading a host that is not this one
//
// Every reader takes a [Config] naming the sysfs, procfs, and device roots,
// which default to /sys, /proc, and /dev. That is what makes the package
// testable against a captured tree, and it is also how a container that mounts
// a host's /sys somewhere else reads the host rather than its own namespace.
//
// # What a block device is, is not decided here
//
// The disks are read through [github.com/simplyblock/atlas/blockdev], which
// owns what a block device is, who is already using it, and whether it may be
// handed over. This package composes that reading in and does not repeat it:
// [Inventory.Devices] holds blockdev.Candidate values, each with the grounds it
// was refused on.
//
// # Partial answers are answers
//
// A reader that finds nothing where nothing is expected reports nothing and no
// error: a kernel built without hugetlbfs has no huge pages, and a tree with no
// class/net has no interfaces. A reader that cannot read what must be there
// fails, and [Collect] keeps going and returns both halves, because a worker
// whose CPU tree is unreadable still has disks and NICs worth reporting.
package inventory
