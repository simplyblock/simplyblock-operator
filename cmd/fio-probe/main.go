// fio-probe measures NVMe-oF write latency via fio and exposes results in two modes:
//
//	--mode=baseline  One-shot measurement. Writes {"p50_ns":...,"p99_ns":...} to
//	                 --termination-log and exits. Used by the one-shot Kubernetes Job.
//
//	--mode=probe     Long-running. Measures latency on --interval and exposes the
//	                 results via a Prometheus /metrics endpoint on --metrics-addr.
//	                 No node-exporter or textfile collector required.
//
// All flag values fall back to the corresponding environment variable when the flag
// is not set explicitly (see the flag definitions below).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ── connection config ──────────────────────────────────────────────────────────

type connConfig struct {
	Addr string
	Port string
	NQN  string
}

// ── fio JSON structures ────────────────────────────────────────────────────────

type fioOutput struct {
	Jobs []struct {
		Write struct {
			ClatNS struct {
				Percentile map[string]int64 `json:"percentile"`
			} `json:"clat_ns"`
		} `json:"write"`
	} `json:"jobs"`
}

type latencyResult struct {
	P50NS int64 `json:"p50_ns"`
	P99NS int64 `json:"p99_ns"`
}

// ── nvme list JSON structures ──────────────────────────────────────────────────

type nvmeListOutput struct {
	Devices []struct {
		Subsystems []struct {
			SubsystemNQN string `json:"SubsystemNQN"`
			Namespaces   []struct {
				NameSpace string `json:"NameSpace"`
			} `json:"Namespaces"`
		} `json:"Subsystems"`
	} `json:"Devices"`
}

// probeNodeConfig matches one element of the JSON array the operator writes to
// the fio-bench ConfigMap (keyed by k8s hostname). Used by --config in probe mode.
type probeNodeConfig struct {
	NQN         string `json:"nqn"`
	Addr        string `json:"addr"`
	Port        int32  `json:"port"`
	NodeUUID    string `json:"nodeUUID"`
	ClusterUUID string `json:"clusterUUID"`
}

// ── main ───────────────────────────────────────────────────────────────────────

func main() {
	mode := flag.String("mode", "", "baseline or probe")

	// Connection flags — used when --config is not provided.
	addr := flag.String("addr", os.Getenv("FIO_NODE_ADDR"), "NVMe-oF TCP address")
	port := flag.String("port", os.Getenv("FIO_NODE_PORT"), "NVMe-oF TCP port")
	nqn := flag.String("nqn", os.Getenv("FIO_VOLUME_NQN"), "NQN of the benchmark volume")

	// Baseline-only.
	terminationLog := flag.String("termination-log", "/tmp/termination-log", "path to write JSON result (baseline)")

	// Probe-only.
	configFile := flag.String("config", "",
		"path to JSON config file (probe); when set, direct connection flags are ignored")
	nodeUUID := flag.String("node-uuid", os.Getenv("NODE_UUID"),
		"storage node UUID for Prometheus labels (probe, no --config)")
	clusterUUID := flag.String("cluster-uuid", os.Getenv("CLUSTER_UUID"),
		"cluster UUID for Prometheus labels (probe, no --config)")
	metricsAddr := flag.String("metrics-addr", ":9199", "address for the Prometheus /metrics endpoint (probe)")
	interval := flag.Duration("interval", 5*time.Minute, "measurement interval (probe)")

	flag.Parse()

	switch *mode {
	case "baseline":
		if *addr == "" || *port == "" || *nqn == "" {
			log.Fatal("--addr, --port and --nqn (or FIO_NODE_ADDR/FIO_NODE_PORT/FIO_VOLUME_NQN) are required")
		}
		baseline(connConfig{Addr: *addr, Port: *port, NQN: *nqn}, *terminationLog)

	case "probe":
		if *configFile != "" {
			probe(*configFile, *metricsAddr, *interval)
		} else {
			if *addr == "" || *port == "" || *nqn == "" || *nodeUUID == "" || *clusterUUID == "" {
				log.Fatal("probe mode without --config requires --addr, --port, --nqn, --node-uuid and --cluster-uuid")
			}
			portNum, err := strconv.ParseInt(*port, 10, 32)
			if err != nil {
				log.Fatalf("invalid --port %q: %v", *port, err)
			}
			probeNodes([]probeNodeConfig{{
				NQN: *nqn, Addr: *addr, Port: int32(portNum),
				NodeUUID: *nodeUUID, ClusterUUID: *clusterUUID,
			}}, *metricsAddr, *interval)
		}

	default:
		log.Fatalf("--mode must be baseline or probe, got %q", *mode)
	}
}

// readConfig reads the JSON array from path. Returns nil when the file does not
// exist yet or is empty — callers should retry rather than treat this as fatal.
func readConfig(path string) []probeNodeConfig {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var nodes []probeNodeConfig
	if err := json.Unmarshal(data, &nodes); err != nil {
		log.Printf("parse config %s: %v", path, err)
		return nil
	}
	return nodes
}

// ── baseline mode ──────────────────────────────────────────────────────────────

func baseline(conn connConfig, terminationLog string) {
	ctx := context.Background()

	device, disconnect, err := connectAndWait(ctx, conn)
	if err != nil {
		log.Fatalf("nvme connect: %v", err)
	}
	defer disconnect()

	result, err := measure(ctx, device)
	if err != nil {
		log.Fatalf("fio: %v", err)
	}

	out, _ := json.Marshal(result)
	if err := os.WriteFile(terminationLog, out, 0o644); err != nil {
		log.Fatalf("write termination log: %v", err)
	}
	log.Printf("baseline complete: p50=%dns p99=%dns", result.P50NS, result.P99NS)
}

// ── probe mode ─────────────────────────────────────────────────────────────────

func newPrometheusServer(metricsAddr string) (*prometheus.GaugeVec, *prometheus.GaugeVec, *http.Server) {
	reg := prometheus.NewRegistry()
	p50 := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "simplyblock_node_fio_write_latency_p50_ns",
		Help: "fio 4K randwrite p50 write latency (ns)",
	}, []string{"cluster", "node"})
	p99 := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "simplyblock_node_fio_write_latency_p99_ns",
		Help: "fio 4K randwrite p99 write latency (ns)",
	}, []string{"cluster", "node"})
	reg.MustRegister(p50, p99)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	srv := &http.Server{Addr: metricsAddr, Handler: mux}
	go func() {
		log.Printf("metrics endpoint: http://%s/metrics", metricsAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("metrics server: %v", err)
		}
	}()
	return p50, p99, srv
}

// probe watches configFile for new entries and starts a probeNode goroutine for
// each node UUID as it appears. A node entry only appears in the ConfigMap once
// its baseline measurement has completed, so this naturally enforces the
// baseline-before-probe ordering without any additional coordination.
// Checks for new entries every 30 seconds; exits cleanly on SIGTERM/SIGINT.
func probe(configFile, metricsAddr string, interval time.Duration) {
	p50, p99, srv := newPrometheusServer(metricsAddr)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	var wg sync.WaitGroup
	active := make(map[string]bool) // node UUIDs with a running goroutine

	log.Printf("probe: watching %s for baseline-complete nodes (interval=%s)", configFile, interval)

	for {
		for _, n := range readConfig(configFile) {
			if active[n.NodeUUID] {
				continue
			}
			active[n.NodeUUID] = true
			log.Printf("baseline complete for node %s — starting probe", n.NodeUUID)
			wg.Add(1)
			go func(n probeNodeConfig) {
				defer wg.Done()
				probeNode(ctx, n, p50, p99, interval)
			}(n)
		}
		if len(active) == 0 {
			log.Printf("waiting for baseline to complete (%s) ...", configFile)
		}

		select {
		case <-ctx.Done():
			log.Println("shutting down")
			wg.Wait()
			srv.Shutdown(context.Background()) //nolint:errcheck
			return
		case <-time.After(30 * time.Second):
		}
	}
}

// probeNodes starts one goroutine per node immediately (used when connection
// params are supplied directly via flags rather than a config file).
func probeNodes(nodes []probeNodeConfig, metricsAddr string, interval time.Duration) {
	p50, p99, srv := newPrometheusServer(metricsAddr)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func(n probeNodeConfig) {
			defer wg.Done()
			probeNode(ctx, n, p50, p99, interval)
		}(n)
	}

	wg.Wait()
	log.Println("shutting down")
	srv.Shutdown(context.Background()) //nolint:errcheck
}

// probeNode runs the measurement loop for a single storage node.
func probeNode(ctx context.Context, n probeNodeConfig, p50, p99 *prometheus.GaugeVec, interval time.Duration) {
	conn := connConfig{Addr: n.Addr, Port: fmt.Sprintf("%d", n.Port), NQN: n.NQN}
	log.Printf("probe node=%s cluster=%s interval=%s", n.NodeUUID, n.ClusterUUID, interval)

	for {
		device, disconnect, err := connectAndWait(ctx, conn)
		if err != nil {
			log.Printf("node %s: connect error: %v", n.NodeUUID, err)
		} else {
			result, err := measure(ctx, device)
			disconnect()
			if err != nil {
				log.Printf("node %s: fio error: %v", n.NodeUUID, err)
			} else {
				p50.WithLabelValues(n.ClusterUUID, n.NodeUUID).Set(float64(result.P50NS))
				p99.WithLabelValues(n.ClusterUUID, n.NodeUUID).Set(float64(result.P99NS))
				log.Printf("node %s: p50=%dns p99=%dns", n.NodeUUID, result.P50NS, result.P99NS)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// ── NVMe helpers ───────────────────────────────────────────────────────────────

func nvmeConnect(ctx context.Context, conn connConfig) error {
	log.Printf("nvme connect addr=%s port=%s nqn=%s", conn.Addr, conn.Port, conn.NQN)
	out, err := exec.CommandContext(ctx,
		"sudo", "nvme", "connect",
		"--fast_io_fail_tmo=1", "--nr-io-queues=3", "--keep-alive-tmo=4",
		"-t", "tcp",
		"-a", conn.Addr,
		"-s", conn.Port,
		"-n", conn.NQN,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("nvme connect: %w\n%s", err, out)
	}
	log.Printf("nvme connect ok")
	return nil
}

func nvmeDisconnect(nqn string) {
	log.Printf("nvme disconnect nqn=%s", nqn)
	if err := exec.Command("sudo", "nvme", "disconnect", "-n", nqn).Run(); err != nil {
		log.Printf("nvme disconnect error: %v", err)
	}
}

func connectAndWait(ctx context.Context, conn connConfig) (device string, disconnect func(), err error) {
	// Always disconnect first to ensure a clean device state that supports O_DIRECT.
	nvmeDisconnect(conn.NQN)
	log.Printf("nvme reconnect after disconnect")
	if err := nvmeConnect(ctx, conn); err != nil {
		return "", nil, err
	}

	disconnect = func() { nvmeDisconnect(conn.NQN) }

	for i := range 30 {
		dev, findErr := findDevice(ctx, conn.NQN)
		if findErr == nil && dev != "" {
			path := "/dev/" + dev
			if isBlockDevice(path) {
				log.Printf("found %s (attempt %d)", path, i+1)
				return path, disconnect, nil
			}
			// Device node exists but is not a block device (e.g. a stub left by
			// SPDK). Remove it so the host's devtmpfs/udevd can create the
			// proper block special file in its place.
			log.Printf("found %s but it is not a block device — removing stub (attempt %d)", path, i+1)
			_ = os.Remove(path)
		}
		select {
		case <-ctx.Done():
			disconnect()
			return "", nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}

	disconnect()
	return "", nil, fmt.Errorf("device for NQN %s not found after 30s", conn.NQN)
}

func isBlockDevice(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice == 0
}

func findDevice(ctx context.Context, nqn string) (string, error) {
	out, err := exec.CommandContext(ctx, "sudo", "nvme", "list", "--output-format=json", "--verbose").Output()
	if err != nil {
		return "", err
	}
	var list nvmeListOutput
	if err := json.Unmarshal(out, &list); err != nil {
		return "", err
	}
	for _, dev := range list.Devices {
		for _, sub := range dev.Subsystems {
			if sub.SubsystemNQN == nqn && len(sub.Namespaces) > 0 {
				return sub.Namespaces[0].NameSpace, nil
			}
		}
	}
	return "", nil
}

// ── fio ────────────────────────────────────────────────────────────────────────

func measure(ctx context.Context, device string) (*latencyResult, error) {
	out, err := exec.CommandContext(ctx,
		"sudo", "fio",
		"--allow_file_create=0",
		"--name=latency",
		"--size=512M",
		"--filename="+device,
		"--ioengine=libaio",
		"--direct=1",
		"--rw=randwrite",
		"--bs=4k",
		"--blockalign=4k",
		"--numjobs=1",
		"--iodepth=1",
		"--time_based",
		"--runtime=30",
		"--group_reporting",
		"--percentile_list=50:99",
		"--output-format=json",
	).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("fio exec: %w\n%s", err, out)
	}

	var fio fioOutput
	if err := json.Unmarshal(out, &fio); err != nil {
		return nil, fmt.Errorf("parse fio output: %w", err)
	}
	if len(fio.Jobs) == 0 {
		return nil, fmt.Errorf("fio returned no jobs")
	}

	pct := fio.Jobs[0].Write.ClatNS.Percentile
	return &latencyResult{
		P50NS: pct["50.000000"],
		P99NS: pct["99.000000"],
	}, nil
}
