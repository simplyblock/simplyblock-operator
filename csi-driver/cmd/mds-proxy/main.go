// The mds-proxy: one pod per node the pNFS metadata server may run on, on the
// host's network and without privilege. It opens the metadata server guest's
// connections to its own node's storage node from the host, which the storage
// node's interface-bound listeners accept, and carries them
// (design-pnfs-mds-vm.md §8.5).

package main

import (
	"context"
	"flag"
	"net"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/klog"

	"github.com/simplyblock/csi-driver/internal/mds/hostpath"
)

func main() {
	listen := flag.String("listen", "",
		"host:port to serve on, the node's address (downward API status.hostIP) and the proxy's port")
	portsFile := flag.String("ports-file", "",
		"File listing the storage cluster's listener ports, which may be dialed on other nodes; "+
			"empty allows the node's own addresses only")
	klog.InitFlags(nil)
	if err := flag.Set("logtostderr", "true"); err != nil {
		klog.Exitf("failed to set logtostderr flag: %v", err)
	}
	flag.Parse()
	if *listen == "" {
		klog.Exit("--listen is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	ln, err := net.Listen("tcp4", *listen)
	if err != nil {
		klog.Exitf("listening on %s: %v", *listen, err)
	}
	addrs, err := hostpath.LocalAddrs()
	if err != nil {
		klog.Exitf("%v", err)
	}
	klog.Infof("mds-proxy serving on %s for %v", ln.Addr(), addrs)

	var dialer net.Dialer
	proxy := &hostpath.Proxy{
		Addrs: hostpath.LocalAddrs,
		Dial:  dialer.DialContext,
		Log:   klog.Warningf,
	}
	if *portsFile != "" {
		// Read on every request: the operator rewrites the file as storage
		// nodes come and go, and kubelet replaces it in place.
		proxy.Ports = func() ([]uint16, error) { return hostpath.ReadPorts(*portsFile) }
	}
	if err := proxy.Serve(ctx, ln); err != nil {
		klog.Errorf("mds-proxy: %v", err)
		klog.Flush()
		os.Exit(1)
	}
}
