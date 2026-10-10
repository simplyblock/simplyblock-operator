//go:build linux

// `mds-runner extents`: a one-shot subcommand, run with kubectl exec in the
// runner container, that prints what a byte range of a file in an export is
// made of. The guest has no shell, and the runner is what shares its network.

package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
	"github.com/simplyblock/csi-driver/internal/mds/netsetup"
	"github.com/simplyblock/csi-driver/internal/mds/runner"
)

func runExtents(args []string) int {
	fs := flag.NewFlagSet("extents", flag.ContinueOnError)
	agentAddr := fs.String("agent", net.JoinHostPort(netsetup.DefaultPlan("").Guest.String(),
		strconv.Itoa(netsetup.AgentPort)), "The guest agent's address")
	exportPath := fs.String("export", "", "The export's path in the guest")
	file := fs.String("file", "", "A file directly inside the export, by name")
	offset := fs.Uint64("offset", 0, "Start of the byte range")
	length := fs.Uint64("length", 0, "Length of the byte range")
	timeout := fs.Duration("timeout", 30*time.Second, "How long to wait for the agent")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *exportPath == "" || *file == "" || *length == 0 {
		fmt.Fprintln(os.Stderr, "extents: -export, -file and -length are required")
		return 2
	}
	conn, err := grpc.NewClient(*agentAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "extents: %v\n", err)
		return 1
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := runner.WriteExtentReport(ctx, nfsexportrpc.Remote(conn), os.Stdout,
		*exportPath, *file, *offset, *length); err != nil {
		fmt.Fprintf(os.Stderr, "extents: %v\n", err)
		return 1
	}
	return 0
}
