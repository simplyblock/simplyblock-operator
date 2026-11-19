// sb-mock is the Control Center's test backend: one process that impersonates
// the upstreams the console proxies, for one or several Kubernetes contexts:
// the Kubernetes API (operator CRDs, Ramen CRDs, and core objects), the
// control plane v2 REST API, the operator HTTP API, and Prometheus.
//
// Reads are served from generated worlds (--dataset, --seed), one per context
// of a fleet. Writes persist into the in-memory store and fire watch events,
// but perform no underlying change. A simulator per world advances Ops
// objects and control plane tasks through their phases and writes the status
// transitions into the control plane event log, so the UI sees live
// transitions on every surface.
//
// Wire it in one of three ways:
//
//	console pod:  SB_K8S_API=http://sb-mock:8080  SB_CP_URL=http://sb-mock:8080
//	              SB_OPERATOR_URL=http://sb-mock:8080  SB_PROMETHEUS_URL=http://sb-mock:8080
//	helm:         --set controlCenter.enabled=true --set controlCenter.mock.enabled=true
//	local dev:    sb-mock --serve-ui ../ --dataset fleet-edge-sites
//	              (serves the console assets itself, no nginx needed; open http://localhost:8080)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// crdDirOverride is the --crd-dir flag. Empty means auto-detect (see crds.go).
var crdDirOverride string

func main() {
	var (
		listen      = flag.String("listen", ":8080", "listen address")
		dataset     = flag.String("dataset", "auto", "test data set or fleet: auto | "+datasetNames())
		seed        = flag.Uint64("seed", 0, "rng seed; 0 = random. Same seed + dataset = same world")
		namespace   = flag.String("namespace", "simplyblock", "namespace the simplyblock control plane lives in")
		simInterval = flag.Duration("sim-interval", 4*time.Second, "simulator tick interval; 0 disables the simulator")
		failRate    = flag.Float64("fail-rate", 0.1, "probability that a simulated operation fails")
		serveUI     = flag.String("serve-ui", "", "directory with the console's index.html; serves the UI and mounts the proxied paths (/k8s, /cp, /operator, /prometheus) so no nginx is needed")
		uiMock      = flag.Bool("ui-mock", false, "serve config.js with mock:true so the console uses its own in-browser fixture backend (fully populated, create flows work) instead of this server's operator API")
		fixtures    = flag.String("operator-fixtures", "", "directory of JSON fixtures for the operator API (see mock/README.md)")
		viewer      = flag.String("viewer", "global", "authorization stand-in for the impersonated user: global | none | <sb:role>@<namespace>[,...] (see PARADIGM.md)")
		crdDir      = flag.String("crd-dir", "", "directory with the operator's CRD YAML (default: operator/config/crd/bases found relative to the working directory)")
	)
	flag.Parse()
	crdDirOverride = *crdDir

	if *seed == 0 {
		*seed = rand.Uint64()
	}
	var fleet Fleet
	ok := false
	if *dataset == "auto" {
		all := scenarios()
		fleet, ok = oneContextFleet(all[rand.New(rand.NewPCG(*seed, *seed>>3)).IntN(len(all))]), true
	} else {
		fleet, ok = fleetByName(*dataset)
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown dataset %q; available: auto %s\n", *dataset, datasetNames())
		os.Exit(2)
	}

	srv := newFleetServer(fleet, *seed, *namespace, *fixtures, *viewer)
	srv.simInterval, srv.failRate = *simInterval, *failRate
	log.Printf("dataset=%s seed=%d systemNamespace=%s viewer=%s (%s)", fleet.Name, *seed, *namespace, *viewer, fleet.Description)
	for _, name := range srv.order {
		wd := srv.worlds[name]
		log.Printf("context %s: scenario=%s seed=%d labels=%v", name, wd.scenario.Name, wd.seed, wd.labels)
	}
	if srv.crdDir == "" {
		log.Printf("crds: no operator/config/crd/bases found; customresourcedefinitions list is empty (use --crd-dir)")
	}

	if *simInterval > 0 {
		srv.startSimulators()
		log.Printf("simulator: every %s, fail-rate %.0f%%", *simInterval, *failRate*100)
	}

	srv.uiMock = *uiMock
	mux := srv.routes(*serveUI)
	log.Printf("listening on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

// world is one Kubernetes context: its own store and the API impersonations
// over it. The console reaches it under `/k8s/<name>/` and `/cp/<name>/`.
type world struct {
	name     string
	labels   map[string]string
	scenario Scenario
	seed     uint64

	store *Store
	k8s   *K8sAPI
	prom  *PromAPI
	cp    *CPAPI
	sim   *Simulator
	authz *AuthZ

	stopSim context.CancelFunc
}

// server ties the worlds and the operator API stub together.
type server struct {
	mu     sync.RWMutex
	fleet  Fleet
	worlds map[string]*world
	order  []string
	// local is the world the root routes (`/api`, `/apis`, `/k8s/`, `/cp/local/`)
	// serve: the context named local when there is one, else the first.
	local *world

	operator *OperatorAPI

	seed        uint64
	namespace   string
	fixtures    string
	viewer      string
	crdDir      string
	simInterval time.Duration
	failRate    float64
	uiMock      bool
}

// reservedContextNames cannot name a context: under `/k8s/` and `/cp/` they are
// API path segments of the local world.
var reservedContextNames = map[string]bool{
	"api": true, "apis": true, "version": true, "openapi": true, "healthz": true,
	"livez": true, "readyz": true, "metrics": true, "local": true,
}

// newServer builds a one-context server for a plain scenario.
func newServer(sc Scenario, seed uint64, namespace, fixtures, viewer string) *server {
	return newFleetServer(oneContextFleet(sc), seed, namespace, fixtures, viewer)
}

func newFleetServer(f Fleet, seed uint64, namespace, fixtures, viewer string) *server {
	s := &server{
		fleet:     f,
		worlds:    map[string]*world{},
		operator:  NewOperatorAPI(fixtures),
		seed:      seed,
		namespace: namespace,
		fixtures:  fixtures,
		viewer:    viewer,
		crdDir:    findCRDDir(crdDirOverride),
		failRate:  0.1,
	}
	for _, fc := range f.Contexts {
		if fc.Name != "local" && reservedContextNames[fc.Name] {
			panic("fleet " + f.Name + " uses the reserved context name " + fc.Name)
		}
		s.addWorld(fc)
	}
	s.operator.authz = s.local.authz
	return s
}

// worldSeed derives a context's seed: the local world keeps --seed so a plain
// dataset reproduces exactly as before. Every other context mixes its name in.
func (s *server) worldSeed(fc FleetContext) uint64 {
	if fc.Seed != 0 {
		return fc.Seed
	}
	if fc.Name == "local" {
		return s.seed
	}
	h := fnv.New64a()
	h.Write([]byte(fc.Name))
	return s.seed ^ h.Sum64()
}

func (s *server) buildStore(fc FleetContext, seed uint64) *Store {
	st := NewStore(seed)
	st.Register(allResourceDefs()...)
	loadCRDs(st, s.crdDir)
	Generate(st, fc.Scenario, seed, s.namespace)
	return st
}

// addWorld renders a context and registers it. The first one, or the one
// named local, becomes the root world.
func (s *server) addWorld(fc FleetContext) *world {
	seed := s.worldSeed(fc)
	st := s.buildStore(fc, seed)
	authz := &AuthZ{store: st, viewer: parseViewer(s.viewer)}
	wd := &world{
		name: fc.Name, labels: fc.Labels, scenario: fc.Scenario, seed: seed,
		store: st,
		k8s:   &K8sAPI{store: st, authz: authz},
		prom:  &PromAPI{store: st, seed: seed},
		cp:    NewCPAPI(st, seed),
		sim:   NewSimulator(st, s.namespace, s.failRate, seed),
		authz: authz,
	}
	if wd.labels == nil {
		wd.labels = map[string]string{}
	}
	s.worlds[fc.Name] = wd
	s.order = append(s.order, fc.Name)
	if s.local == nil || fc.Name == "local" {
		s.local = wd
	}
	return wd
}

// startSimulators runs every world's simulator at the configured interval.
func (s *server) startSimulators() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range s.order {
		s.startSimulator(s.worlds[name])
	}
}

func (s *server) startSimulator(wd *world) {
	if s.simInterval <= 0 || wd.stopSim != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	wd.stopSim = cancel
	go wd.sim.Run(ctx, s.simInterval)
}

// world resolves a context name. The name local is the root world.
func (s *server) world(name string) *world {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if name == "local" {
		return s.local
	}
	return s.worlds[name]
}

// rootWorld is the world behind the context-less routes.
func (s *server) rootWorld() *world {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.local
}

// rootHandler serves the root world's handler chosen by pick, looked up per
// request so a reset that replaces worlds takes effect.
func (s *server) rootHandler(pick func(*world) http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pick(s.rootWorld()).ServeHTTP(w, r)
	})
}

// contextHandler serves /<prefix>/<ctx>/... from the named world, stripping
// the prefix and the context. A first segment that names no context falls
// through to the root world with only the prefix stripped, so `/k8s/apis/...`
// keeps working as the local context.
func (s *server) contextHandler(prefix string, pick func(*world) http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		first, _, _ := strings.Cut(strings.TrimPrefix(rest, "/"), "/")
		if wd := s.world(first); wd != nil && first != "" {
			http.StripPrefix(prefix+"/"+first, pick(wd)).ServeHTTP(w, r)
			return
		}
		http.StripPrefix(prefix, pick(s.rootWorld())).ServeHTTP(w, r)
	})
}

func (s *server) routes(serveUI string) *http.ServeMux {
	mux := http.NewServeMux()
	k8s := func(wd *world) http.Handler { return wd.k8s }
	prom := func(wd *world) http.Handler { return wd.prom }
	cp := func(wd *world) http.Handler { return wd.cp }

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
	mux.Handle("/helm/", s.operator) // in case a deployment routes Helm separately
	// Prometheus paths take precedence over the core K8s API by being the more
	// specific patterns: /api/v1/query is Prometheus, /api/v1/pods is Kubernetes.
	for _, p := range []string{"query", "query_range", "series", "labels", "rules", "alerts"} {
		mux.Handle("/api/v1/"+p, s.rootHandler(prom))
	}
	mux.Handle("/api/v1/label/", s.rootHandler(prom))
	// the control plane v2 API at the root (SB_CP_URL=http://sb-mock:8080)
	mux.Handle("/api/v2/", s.rootHandler(cp))
	// Kubernetes API
	mux.Handle("/api", s.rootHandler(k8s))
	mux.Handle("/api/", s.rootHandler(k8s))
	mux.Handle("/apis", s.rootHandler(k8s))
	mux.Handle("/apis/", s.rootHandler(k8s))
	mux.Handle("/version", s.rootHandler(k8s))

	// same-origin paths the console uses: per context under `/k8s/<ctx>/` and
	// `/cp/<ctx>/`, the local context also directly under `/k8s/`
	mux.Handle("/k8s/", s.contextHandler("/k8s", k8s))
	mux.Handle("/cp/", s.contextHandler("/cp", cp))
	mux.Handle("/operator/", http.StripPrefix("/operator", s.operator))
	mux.Handle("/prometheus/api/v1/", http.StripPrefix("/prometheus", s.rootHandler(prom)))

	if serveUI != "" {
		mux.HandleFunc("GET /config.js", s.handleConfigJS)
		mux.Handle("/", http.FileServer(http.Dir(serveUI)))
		log.Printf("serving console UI from %s", serveUI)
	}
	return mux
}

// consoleContext is one entry of config.js contexts: what the console needs
// to reach a Kubernetes context and its control plane through the proxy.
type consoleContext struct {
	Name       string            `json:"name"`
	Labels     map[string]string `json:"labels"`
	Namespaces []string          `json:"namespaces"`
	K8sBase    string            `json:"k8sBase"`
	CpBase     string            `json:"cpBase"`
	Local      bool              `json:"local"`
}

// consoleFeatures turns on the fixture-only views in the design preview. A
// real deployment leaves them off.
var consoleFeatures = map[string]bool{
	"zones": true, "buckets": true, "cgroups": true, "migrationPaths": true,
	"fileStorage": true, "objectStorage": true, "fdbBackups": true, "accessRoles": true,
}

func (s *server) consoleContexts() []consoleContext {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []consoleContext
	for _, name := range s.order {
		wd := s.worlds[name]
		cc := consoleContext{
			Name: name, Labels: wd.labels, Namespaces: clusterNamespaces(wd.store),
			K8sBase: "/k8s/" + name, CpBase: "/cp/" + name, Local: wd == s.local,
		}
		if name == "local" {
			cc.K8sBase = "/k8s"
		}
		out = append(out, cc)
	}
	return out
}

func (s *server) handleConfigJS(w http.ResponseWriter, _ *http.Request) {
	contexts, _ := json.Marshal(s.consoleContexts())
	features, _ := json.Marshal(consoleFeatures)
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `// Generated by sb-mock for local development.
window.SB_CONFIG = {
  k8sBase: "/k8s",
  cpBase: "/cp/local",
  operatorBase: "/operator/v1",
  helmBase: "/helm/v1",
  promBase: "/prometheus/api/v1",
  agentBase: "/operator/v1/agent",
  namespace: %q,
  contexts: %s,
  features: %s,
  logoUrl: "",
  authMode: "serviceaccount",
  token: "",
  mock: %v
};
`, s.namespace, contexts, features, s.uiMock)
}

// userStorageClusters lists a world's user-facing StorageClusters, leaving
// out the hub's sb-mc-* projections.
func userStorageClusters(st *Store) []map[string]any {
	d, ok := st.Lookup(sbGroup, "v1alpha1", "storageclusters")
	if !ok {
		return nil
	}
	items, _ := st.List(d, "", "", "")
	var out []map[string]any
	for _, c := range items {
		if isProjection(c) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func isProjection(c map[string]any) bool {
	ann, _ := getPath(c, "metadata.annotations").(map[string]any)
	if _, ok := ann["simplyblock.io/projection-of"]; ok {
		return true
	}
	return strings.HasPrefix(getStr(c, "metadata.namespace"), "sb-mc-")
}

// clusterNamespaces lists the namespaces that hold StorageClusters, sorted.
func clusterNamespaces(st *Store) []string {
	seen := map[string]bool{}
	for _, c := range userStorageClusters(st) {
		seen[getStr(c, "metadata.namespace")] = true
	}
	out := make([]string, 0, len(seen))
	for ns := range seen {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

func objectCounts(st *Store) map[string]int {
	counts := map[string]int{}
	for _, d := range st.Defs() {
		if d.Internal {
			continue
		}
		items, _ := st.List(d, "", "", "")
		if len(items) > 0 {
			counts[d.Group+"/"+d.Resource] = len(items)
		}
	}
	return counts
}

func (s *server) handleInfo(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	contexts := map[string]any{}
	for _, name := range s.order {
		wd := s.worlds[name]
		var clusters []map[string]any
		for _, c := range userStorageClusters(wd.store) {
			clusters = append(clusters, map[string]any{
				"name": getStr(c, "metadata.name"), "namespace": getStr(c, "metadata.namespace"),
				"uuid": getStr(c, "status.uuid"), "labels": getPath(c, "metadata.labels"),
			})
		}
		contexts[name] = map[string]any{
			"labels":   wd.labels,
			"scenario": wd.scenario.Name,
			"seed":     fmt.Sprint(wd.seed),
			"objects":  objectCounts(wd.store),
			"clusters": clusters,
			"k8sBase":  "/k8s/" + name,
			"cpBase":   "/cp/" + name,
		}
	}
	writeJSON(w, 200, map[string]any{
		"dataset":   s.fleet.Name,
		"seed":      fmt.Sprint(s.seed),
		"namespace": s.namespace,
		"objects":   objectCounts(s.local.store),
		"contexts":  contexts,
		"crdDir":    s.crdDir,
		// distinct operator-API requests with no fixture behind them: the
		// to-do list for new fixture files
		"unmockedOperatorRequests": s.operator.UnknownRequests(),
	})
}

// handleReset rebuilds every world, optionally with a new dataset or fleet
// and seed. End-to-end suites call this between test cases for a clean,
// reproducible state. Contexts that stay get their store contents swapped
// (watchers are closed so clients relist). Contexts that come or go are
// added or removed together with their simulators.
func (s *server) handleReset(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dataset := q.Get("dataset")
	if dataset == "" {
		dataset = s.fleet.Name
	}
	f, ok := fleetByName(dataset)
	if !ok {
		writeJSON(w, 400, map[string]any{"error": "unknown dataset " + dataset})
		return
	}
	seed := s.seed
	if qs := q.Get("seed"); qs != "" {
		fmt.Sscan(qs, &seed)
	}

	s.mu.Lock()
	s.fleet, s.seed = f, seed
	keep := map[string]bool{}
	var order []string
	s.local = nil
	for _, fc := range f.Contexts {
		keep[fc.Name] = true
		order = append(order, fc.Name)
		if wd, exists := s.worlds[fc.Name]; exists {
			wd.seed, wd.scenario, wd.labels = s.worldSeed(fc), fc.Scenario, fc.Labels
			if wd.labels == nil {
				wd.labels = map[string]string{}
			}
			replaceStore(wd.store, s.buildStore(fc, wd.seed))
			wd.prom.seed, wd.cp.seed = wd.seed, wd.seed
			if s.local == nil || fc.Name == "local" {
				s.local = wd
			}
			continue
		}
		wd := s.addWorld(fc)
		s.startSimulator(wd)
	}
	for name, wd := range s.worlds {
		if !keep[name] {
			if wd.stopSim != nil {
				wd.stopSim()
			}
			delete(s.worlds, name)
		}
	}
	s.order = order
	s.operator.authz = s.local.authz
	s.mu.Unlock()

	log.Printf("reset: dataset=%s seed=%d", f.Name, seed)
	writeJSON(w, 200, map[string]any{"dataset": f.Name, "seed": fmt.Sprint(seed)})
}

// replaceStore swaps dst's contents for src's in place, so handlers and the
// simulator holding dst keep working. Every watch stream is closed so clients
// relist against the new world.
func replaceStore(dst, src *Store) {
	dst.mu.Lock()
	dst.objs = src.objs
	dst.rv = src.rv
	for gvr, ws := range dst.watchers {
		for wtr := range ws {
			close(wtr.ch)
		}
		dst.watchers[gvr] = map[*watcher]struct{}{}
	}
	dst.mu.Unlock()
	dst.logMu.Lock()
	dst.cplog = src.cplog
	dst.logMu.Unlock()
}

// handleAdvance runs simulator ticks on demand, for deterministic e2e tests
// that run with --sim-interval=0. ?ctx= limits it to one world.
func (s *server) handleAdvance(w http.ResponseWriter, r *http.Request) {
	n := 1
	if qs := r.URL.Query().Get("ticks"); qs != "" {
		fmt.Sscan(qs, &n)
	}
	if n < 1 || n > 1000 {
		n = 1
	}
	s.mu.RLock()
	var worlds []*world
	if ctx := r.URL.Query().Get("ctx"); ctx != "" {
		if wd := s.worlds[ctx]; wd != nil {
			worlds = append(worlds, wd)
		}
	} else {
		for _, name := range s.order {
			worlds = append(worlds, s.worlds[name])
		}
	}
	s.mu.RUnlock()
	for _, wd := range worlds {
		for i := 0; i < n; i++ {
			wd.sim.Tick()
		}
	}
	writeJSON(w, 200, map[string]any{"advanced": n, "contexts": len(worlds)})
}
