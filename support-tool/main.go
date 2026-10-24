// The simplyblock support tool: a local web application that reads a support
// bundle written by kubectl-simplyblock-bundle, shows the discovery run, its
// workers, and the cluster it captured, and builds ClusterDeploymentConfigs
// from them.
//
// It runs on the support agent's machine and serves only loopback, because a
// bundle is customer data.
//
// Usage:
//
//	go run ./support-tool [-listen 127.0.0.1:8099]
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/simplyblock/simplyblock-operator/support-tool/internal/server"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8099", "loopback address to serve on")
	flag.Parse()

	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		log.Fatalf("-listen %q: %v", *listen, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		fmt.Fprintf(os.Stderr, "-listen %s is not a loopback address: a bundle is customer data, so the tool serves this machine only\n", *listen)
		os.Exit(2)
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           server.New(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Printf("simplyblock support tool: open http://%s/\n", *listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
