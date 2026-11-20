// The fleet agent's entry point: the process the simplyblock fleet add-on runs
// on every member cluster. Its one job is the site end of the site link
// (design-stretched-sync-replication-ramen.md §7.3): it links to the relay on the
// hub as its site, and its site's operator and CSI controller link to it.
//
// It runs in the member, beside OCM's own agents, with two sets of credentials:
// its in-cluster ServiceAccount for the member's API server, and the hub
// kubeconfig the add-on's registration mounts, which it uses for nothing but
// reading the relay endpoint and requesting its link token.

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/simplyblock/atlas/link"

	"github.com/simplyblock/fleet/internal/forwarder"
)

func main() {
	var (
		site = flag.String("site", os.Getenv("CLUSTER_NAME"),
			"The member's name: its site, and its namespace on the hub.")
		namespace = flag.String("namespace", os.Getenv("POD_NAMESPACE"),
			"The agent's namespace on the member.")
		hubKubeconfig = flag.String("hub-kubeconfig", "/managed/hub-kubeconfig/kubeconfig",
			"The hub kubeconfig the add-on registration mounts.")
		listenAddr = flag.String("listen", ":9443",
			"Where the site's operator and CSI controller link to.")
		serviceName = flag.String("service-name", "simplyblock-site-link",
			"The Service the site's peers reach the agent by.")
		peerAccounts = flag.String("peer-service-accounts",
			"operator=simplyblock/simplyblock-operator,controller=simplyblock/simplyblock-csi-controller-sa",
			"kind=namespace/name pairs that may link to the agent, comma separated.")
	)
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	if err := run(*site, *namespace, *hubKubeconfig, *listenAddr, *serviceName, *peerAccounts, log); err != nil {
		log.Error("fleet agent stopped", "error", err)
		os.Exit(1)
	}
}

func run(site, namespace, hubKubeconfig, listenAddr, serviceName, peerAccounts string, log *slog.Logger) error {
	if site == "" || namespace == "" {
		return fmt.Errorf("the site (CLUSTER_NAME) and the namespace (POD_NAMESPACE) are both required")
	}
	accounts, err := parsePeerAccounts(peerAccounts)
	if err != nil {
		return err
	}

	localConfig, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("the member's API server: %w", err)
	}
	local, err := kubernetes.NewForConfig(localConfig)
	if err != nil {
		return err
	}
	hubConfig, err := clientcmd.BuildConfigFromFlags("", hubKubeconfig)
	if err != nil {
		return fmt.Errorf("the hub kubeconfig %s: %w", hubKubeconfig, err)
	}
	hub, err := kubernetes.NewForConfig(hubConfig)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	return forwarder.Run(ctx, forwarder.Config{
		Site:        site,
		Namespace:   namespace,
		InstanceUID: os.Getenv("POD_UID"),
		ListenAddr:  listenAddr,
		DNSNames: []string{
			serviceName,
			serviceName + "." + namespace,
			serviceName + "." + namespace + ".svc",
			serviceName + "." + namespace + ".svc.cluster.local",
		},
		PeerServiceAccounts: accounts,
		Hub:                 hub,
		Local:               local,
		Logger:              log,
	})
}

// parsePeerAccounts reads "kind=namespace/name,..." into the authenticator's map.
func parsePeerAccounts(s string) (map[link.PeerKind][]string, error) {
	out := map[link.PeerKind][]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		kind, account, ok := strings.Cut(pair, "=")
		if !ok || kind == "" || !strings.Contains(account, "/") {
			return nil, fmt.Errorf("peer service account %q: want kind=namespace/name", pair)
		}
		out[link.PeerKind(kind)] = append(out[link.PeerKind(kind)], account)
	}
	return out, nil
}
