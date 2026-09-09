// sb-mock is the Control Center's test backend: one process that impersonates
// the three upstreams the console proxies — the Kubernetes API (operator CRDs,
// Ramen CRDs and core objects), the operator HTTP API, and Prometheus.
//
// Reads are served from a generated world (--dataset, --seed); writes persist
// into the in-memory store and fire watch events, but perform no underlying
// change. A simulator advances Ops objects through their phases so the UI
// sees live transitions.
//
// Wire it in one of three ways:
//
//	console pod:  SB_K8S_API=http://sb-mock:8080  SB_OPERATOR_URL=http://sb-mock:8080
//	              SB_PROMETHEUS_URL=http://sb-mock:8080
//	helm:         --set controlCenter.enabled=true --set controlCenter.mock.enabled=true
//	local dev:    sb-mock --serve-ui ../   (serves the console assets itself,
//	              no nginx needed; open http://localhost:8080)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	var (
		listen      = flag.String("listen", ":8080", "listen address")
		dataset     = flag.String("dataset", "auto", "test data set: auto | "+datasetNames())
		seed        = flag.Uint64("seed", 0, "rng seed; 0 = random. Same seed + dataset = same world")
		namespace   = flag.String("namespace", "simplyblock", "namespace the simplyblock objects live in")
		simInterval = flag.Duration("sim-interval", 4*time.Second, "simulator tick interval; 0 disables the simulator")
		failRate    = flag.Float64("fail-rate", 0.1, "probability that a simulated operation fails")
		serveUI     = flag.String("serve-ui", "", "directory with the console's index.html; serves the UI and mounts the proxied paths (/k8s, /operator, /prometheus) so no nginx is needed")
		fixtures    = flag.String("operator-fixtures", "", "directory of JSON fixtures for the operator API (see mock/README.md)")
	)
	flag.Parse()

	if *seed == 0 {
		*seed = rand.Uint64()
	}
	sc, ok := scenarioByName(*dataset)
	if *dataset == "auto" {
		all := scenarios()
		sc, ok = all[rand.New(rand.NewPCG(*seed, *seed>>3)).IntN(int(len(all)))], true
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown dataset %q; available: auto %s\n", *dataset, datasetNames())
		os.Exit(2)
	}

	srv := newServer(sc, *seed, *namespace, *fixtures)
	log.Printf("dataset=%s seed=%d namespace=%s (%s)", sc.Name, *seed, *namespace, sc.Description)

	if *simInterval > 0 {
		sim := NewSimulator(srv.store, *namespace, *failRate, *seed)
		srv.sim = sim
		go sim.Run(context.Background(), *simInterval)
		log.Printf("simulator: every %s, fail-rate %.0f%%", *simInterval, *failRate*100)
	}

	mux := srv.routes(*serveUI)
	log.Printf("listening on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

func datasetNames() string {
	var names []string
	for _, s := range scenarios() {
		names = append(names, s.Name)
	}
	return strings.Join(names, " | ")
}

// server ties the store and the three API impersonations together.
type server struct {
	store    *Store
	k8s      *K8sAPI
	prom     *PromAPI
	operator *OperatorAPI
	sim      *Simulator

	dataset   string
	seed      uint64
	namespace string
	fixtures  string
}

func newServer(sc Scenario, seed uint64, namespace, fixtures string) *server {
	st := NewStore(seed)
	st.Register(allResourceDefs()...)
	Generate(st, sc, seed, namespace)
	return &server{
		store:     st,
		k8s:       &K8sAPI{store: st},
		prom:      &PromAPI{store: st, seed: seed},
		operator:  NewOperatorAPI(fixtures),
		dataset:   sc.Name,
		seed:      seed,
		namespace: namespace,
		fixtures:  fixtures,
	}
}

func (s *server) routes(serveUI string) *http.ServeMux {
	mux := http.NewServeMux()

	// admin
	mux.HandleFunc("GET /mockctl/info", s.handleInfo)
	mux.HandleFunc("POST /mockctl/reset", s.handleReset)
	mux.HandleFunc("POST /mockctl/advance", s.handleAdvance)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})

	// upstream impersonation at the root, for the console pod's env overrides
	mux.Handle("/v1/", s.operator)   // SB_OPERATOR_URL / SB_HELM_URL
	mux.Handle("/helm/", s.operator) // in case a deployment routes helm separately
	// Prometheus paths take precedence over the core K8s API by being the more
	// specific patterns: /api/v1/query is Prometheus, /api/v1/pods is Kubernetes.
	for _, p := range []string{"query", "query_range", "series", "labels", "rules", "alerts"} {
		mux.Handle("/api/v1/"+p, s.prom)
	}
	mux.Handle("/api/v1/label/", s.prom)
	// Kubernetes API
	mux.Handle("/api", s.k8s)
	mux.Handle("/api/", s.k8s)
	mux.Handle("/apis", s.k8s)
	mux.Handle("/apis/", s.k8s)
	mux.Handle("/version", s.k8s)

	// same-origin paths the console uses when the mock serves the UI itself
	mux.Handle("/k8s/", http.StripPrefix("/k8s", s.k8s))
	mux.Handle("/operator/", http.StripPrefix("/operator", s.operator))
	mux.Handle("/prometheus/api/v1/", http.StripPrefix("/prometheus", s.prom))

	if serveUI != "" {
		mux.HandleFunc("GET /config.js", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/javascript")
			w.Header().Set("Cache-Control", "no-store")
			fmt.Fprintf(w, `// Generated by sb-mock for local development.
window.SB_CONFIG = {
  k8sBase: "/k8s",
  operatorBase: "/operator/v1",
  helmBase: "/helm/v1",
  promBase: "/prometheus/api/v1",
  agentBase: "/operator/v1/agent",
  namespace: %q,
  logoUrl: "",
  authMode: "serviceaccount",
  token: "",
  mock: false
};
`, s.namespace)
		})
		mux.Handle("/", http.FileServer(http.Dir(serveUI)))
		log.Printf("serving console UI from %s", serveUI)
	}
	return mux
}

func (s *server) handleInfo(w http.ResponseWriter, _ *http.Request) {
	counts := map[string]int{}
	for _, d := range s.store.Defs() {
		items, _ := s.store.List(d, "", "", "")
		if len(items) > 0 {
			counts[d.Group+"/"+d.Resource] = len(items)
		}
	}
	writeJSON(w, 200, map[string]any{
		"dataset":   s.dataset,
		"seed":      fmt.Sprint(s.seed),
		"namespace": s.namespace,
		"objects":   counts,
		// distinct operator-API requests with no fixture behind them — the
		// to-do list for new fixture files
		"unmockedOperatorRequests": s.operator.UnknownRequests(),
	})
}

// handleReset rebuilds the world, optionally with a new dataset and seed —
// e2e suites call this between test cases for a clean, reproducible state.
func (s *server) handleReset(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dataset := q.Get("dataset")
	if dataset == "" {
		dataset = s.dataset
	}
	sc, ok := scenarioByName(dataset)
	if !ok {
		writeJSON(w, 400, map[string]any{"error": "unknown dataset " + dataset})
		return
	}
	seed := s.seed
	if qs := q.Get("seed"); qs != "" {
		fmt.Sscan(qs, &seed)
	}

	fresh := NewStore(seed)
	fresh.Register(allResourceDefs()...)
	Generate(fresh, sc, seed, s.namespace)

	// swap the contents, not the pointer: handlers and simulator keep working
	s.store.mu.Lock()
	old := s.store
	old.objs = fresh.objs
	old.rv = fresh.rv
	// close every watch stream so clients relist against the new world
	for gvr, ws := range old.watchers {
		for wtr := range ws {
			close(wtr.ch)
		}
		old.watchers[gvr] = map[*watcher]struct{}{}
	}
	s.store.mu.Unlock()

	s.dataset, s.seed = sc.Name, seed
	log.Printf("reset: dataset=%s seed=%d", sc.Name, seed)
	writeJSON(w, 200, map[string]any{"dataset": sc.Name, "seed": fmt.Sprint(seed)})
}

// handleAdvance runs simulator ticks on demand, for deterministic e2e tests
// that run with --sim-interval=0.
func (s *server) handleAdvance(w http.ResponseWriter, r *http.Request) {
	if s.sim == nil {
		s.sim = NewSimulator(s.store, s.namespace, 0, s.seed)
	}
	n := 1
	if qs := r.URL.Query().Get("ticks"); qs != "" {
		fmt.Sscan(qs, &n)
	}
	if n < 1 || n > 1000 {
		n = 1
	}
	for i := 0; i < n; i++ {
		s.sim.Tick()
	}
	writeJSON(w, 200, map[string]any{"advanced": n})
}
