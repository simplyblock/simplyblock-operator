// Whether this node can host the pNFS metadata server's guest: the probe that
// opens /dev/kvm, and the node label that publishes the answer
// (design-pnfs-mds-vm.md §5.4).
//
// It follows the VDO probe in capability.go, which is the established shape
// for a capability the plugin can see and the scheduler needs to: probe once at
// start, publish a label, and leave a hand-set label alone. The plugin runs
// privileged with the host's /dev, so it opens the same device the metadata
// server's runner will.

package node

import (
	"context"
	"os"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/errs/deferrers"
	"github.com/simplyblock/atlas/kube"
)

// kvmDevice is the device the metadata server's runner opens.
const kvmDevice = "/dev/kvm"

// AdvertiseKVMCapability probes this node's /dev/kvm and sets nodeName's
// kvm-capable label to match. Runs once at process start, like the VDO probe.
func AdvertiseKVMCapability(ctx context.Context, kubeClient kubernetes.Interface, nodeName string) error {
	return advertiseKVMCapability(ctx, kubeClient, nodeName, openReadWrite)
}

func advertiseKVMCapability(
	ctx context.Context, kubeClient kubernetes.Interface, nodeName string, open func(string) error,
) error {
	capable := true
	if err := open(kvmDevice); err != nil {
		klog.Infof("kvm probe: node %s cannot open %s: %v", nodeName, kvmDevice, err)
		capable = false
	} else {
		klog.Infof("kvm probe: node %s opened %s", nodeName, kvmDevice)
	}
	return publishCapability(ctx, kubeClient, nodeName, capabilityLabel{
		probe:        "kvm",
		label:        kube.LabelKVMCapable,
		managedBy:    kube.AnnoKVMCapableManagedBy,
		managedValue: kube.AnnoKVMCapableManagedByAutoDetect,
	}, capable)
}

// openReadWrite opens path the way QEMU's -enable-kvm does.
func openReadWrite(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	deferrers.Close(f)
	return nil
}
