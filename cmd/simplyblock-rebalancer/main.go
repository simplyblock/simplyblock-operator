// simplyblock-rebalancer measures NVMe-oF write latency via fio and exposes results in two modes:
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
	"slices"
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
// the simplyblock-rebalancer ConfigMap (keyed by k8s hostname). Used by --config in probe mode.
type probeNodeConfig struct {
	NQN         string `json:"nqn"`
	Addr        string `json:"addr"`
	Port        int32  `json:"port"`
	NodeUUID    string `json:"nodeUUID"`
	ClusterUUID string `json:"clusterUUID"`
}

// ── main ───────────────────────────────────────────────────────────────────────

func main() {
	mode := flag.String("mode", "", "baseline, probe, or validate-migration")

	// Connection flags — used when --config is not provided.
	addr := flag.String("addr", os.Getenv("FIO_NODE_ADDR"), "NVMe-oF TCP address")
	port := flag.String("port", os.Getenv("FIO_NODE_PORT"), "NVMe-oF TCP port")
	nqn := flag.String("nqn", os.Getenv("FIO_VOLUME_NQN"), "NQN of the benchmark volume")

	// Baseline-only.
	terminationLog := flag.String("termination-log", "/tmp/termination-log", "path to write JSON result (baseline)")
	baselineSamples := flag.Int("baseline-samples", 5,
		"number of fio measurements to take for the baseline (the high/low outliers are "+
			"dropped and the rest averaged, so the baseline reflects steady state, not a "+
			"single noisy sample)")
	baselineInterval := flag.Duration("baseline-interval", 15*time.Second,
		"delay between successive baseline measurements")

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
	case "validate-migration":
		validateMigration()

	case "baseline":
		if *addr == "" || *port == "" || *nqn == "" {
			log.Fatal("--addr, --port and --nqn (or FIO_NODE_ADDR/FIO_NODE_PORT/FIO_VOLUME_NQN) are required")
		}
		baseline(connConfig{Addr: *addr, Port: *port, NQN: *nqn}, *terminationLog,
			*baselineSamples, *baselineInterval)

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
		log.Fatalf("--mode must be baseline, probe, or validate-migration, got %q", *mode)
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

func baseline(conn connConfig, terminationLog string, samples int, interval time.Duration) {
	ctx := context.Background()

	if samples < 1 {
		samples = 1
	}

	device, disconnect, err := connectAndWait(ctx, conn)
	if err != nil {
		log.Fatalf("nvme connect: %v", err)
	}
	defer disconnect()

	// Take several measurements in succession; a single fio run is a noisy sample of a
	// spiky latency distribution, so we aggregate (trimmed mean) for a stable baseline.
	var p50s, p99s []int64
	for i := 0; i < samples; i++ {
		if i > 0 {
			time.Sleep(interval)
		}
		r, mErr := measure(ctx, device)
		if mErr != nil {
			log.Printf("baseline sample %d/%d failed: %v", i+1, samples, mErr)
			continue
		}
		p50s = append(p50s, r.P50NS)
		p99s = append(p99s, r.P99NS)
		log.Printf("baseline sample %d/%d: p50=%dns p99=%dns", i+1, samples, r.P50NS, r.P99NS)
	}
	if len(p50s) == 0 {
		log.Fatalf("fio: all %d baseline samples failed", samples)
	}

	result := &latencyResult{P50NS: trimmedMean(p50s), P99NS: trimmedMean(p99s)}
	out, _ := json.Marshal(result)
	if err := os.WriteFile(terminationLog, out, 0o644); err != nil {
		log.Fatalf("write termination log: %v", err)
	}
	log.Printf("baseline complete (%d/%d samples): p50=%dns p99=%dns",
		len(p50s), samples, result.P50NS, result.P99NS)
}

// trimmedMean drops the single lowest and highest value (when there are >=3 samples)
// to reject outliers, then averages the rest. With 1–2 samples it averages all.
func trimmedMean(vals []int64) int64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	s := append([]int64(nil), vals...)
	slices.Sort(s)
	if n >= 3 {
		s = s[1 : n-1] // drop min and max
	}
	var sum int64
	for _, v := range s {
		sum += v
	}
	return sum / int64(len(s))
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
	// running maps each active node UUID to the cancel func that stops its probe.
	// The set is reconciled against the config every loop: new nodes are started and
	// nodes that have disappeared (e.g. removed, or a cluster reinstall replaced the
	// ConfigMap which is keyed by the constant cluster *name*) are stopped — otherwise
	// a dead node keeps getting probed forever and leaves a stale gauge in Prometheus.
	running := make(map[string]context.CancelFunc)

	log.Printf("probe: watching %s for baseline-complete nodes (interval=%s)", configFile, interval)

	for {
		desired := make(map[string]bool)
		for _, n := range readConfig(configFile) {
			desired[n.NodeUUID] = true
			if _, ok := running[n.NodeUUID]; ok {
				continue
			}
			nodeCtx, nodeCancel := context.WithCancel(ctx)
			running[n.NodeUUID] = nodeCancel
			log.Printf("baseline complete for node %s — starting probe", n.NodeUUID)
			wg.Add(1)
			go func(n probeNodeConfig) {
				defer wg.Done()
				probeNode(nodeCtx, n, p50, p99, interval)
			}(n)
		}
		// Stop probes for nodes no longer present in the config.
		for uuid, stop := range running {
			if !desired[uuid] {
				log.Printf("node %s no longer in config — stopping probe", uuid)
				stop()
				delete(running, uuid)
			}
		}
		if len(running) == 0 {
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

	// When this probe stops (node removed from config / shutdown), drop its gauge series
	// so a dead node/cluster does not leave a stale latency value in Prometheus.
	defer func() {
		p50.DeleteLabelValues(n.ClusterUUID, n.NodeUUID)
		p99.DeleteLabelValues(n.ClusterUUID, n.NodeUUID)
	}()

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

	// Retry the connect rather than failing the whole run on the first attempt. The
	// volume's NVMe-oF target is often not yet accepting connections the instant this
	// runs (the Job/probe can start before the subsystem listener is ready), so the
	// first attempt fails fast with "connection refused" / "no such subsystem". Without
	// this, a baseline Job errors out and only succeeds after several controller-driven
	// recreations — the long-standing "jobs need multiple iterations" behaviour.
	var connErr error
	connected := false
	for i := range 30 {
		if connErr = nvmeConnect(ctx, conn); connErr == nil {
			connected = true
			break
		}
		log.Printf("nvme connect not ready (attempt %d): %v", i+1, connErr)
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if !connected {
		return "", nil, fmt.Errorf("nvme connect failed after retries: %w", connErr)
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
