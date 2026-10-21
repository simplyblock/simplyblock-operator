//go:build linux

// The runner's half of the guest's path to its own node (design-pnfs-mds-vm.md
// §8.5): the forwarder on the bridge, and the redirects in front of it, kept
// in step with the addresses the mds-proxy reports.

package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"

	"k8s.io/klog"

	"github.com/simplyblock/csi-driver/internal/mds/hostpath"
	"github.com/simplyblock/csi-driver/internal/mds/netsetup"
)

// startHostPath serves the forwarder and keeps the redirects current until ctx
// ends. The bridge has to exist, since the forwarder listens on its address.
func startHostPath(ctx context.Context, plan netsetup.Plan, proxy string) error {
	listener, err := net.Listen("tcp4", plan.ForwarderAddress().String())
	if err != nil {
		return fmt.Errorf("listening for the guest's connections to its own node: %w", err)
	}
	var dialer net.Dialer
	forwarder := &hostpath.Forwarder{
		Proxy:       proxy,
		Dial:        dialer.DialContext,
		OriginalDst: hostpath.OriginalDestination,
		Log:         klog.Warningf,
	}
	go func() {
		if err := forwarder.Serve(ctx, listener); err != nil {
			klog.Errorf("host path forwarder: %v", err)
		}
	}()

	refresher := &hostpath.Refresher{
		Query: func(ctx context.Context) ([]netip.Addr, error) {
			return hostpath.QueryAddrs(ctx, dialer.DialContext, proxy)
		},
		Apply: func(ctx context.Context, addrs []netip.Addr) error {
			if err := netsetup.ReplaceChain(ctx, netsetup.ExecIPTables, "nat", netsetup.HostPathChain,
				plan.HostPathRules(addrs)); err != nil {
				return err
			}
			klog.Infof("the guest reaches %v through the mds-proxy at %s", addrs, proxy)
			return nil
		},
		Log: klog.Warningf,
	}
	go refresher.Run(ctx, hostpath.RefreshInterval)
	return nil
}
