package main

// Several Kubernetes contexts in one process. A world is one context: its
// own store, generated from its own scenario with its own seed, and the API
// impersonations over it. The console reaches a world under `/k8s/<name>/`;
// the first world is also the local one behind the context-less routes. The
// labels a world carries are what config.js hands the console as the
// context's configured labels (a lifecycle stage, a tenant), which is what an
// administrator would configure on a real console pod.

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"
	"strings"
	"time"
)

type world struct {
	name     string
	labels   map[string]string
	scenario Scenario
	seed     uint64
	store    *Store
	k8s      *K8sAPI
	prom     *PromAPI
	sim      *Simulator
	authz    *AuthZ
	stopSim  context.CancelFunc
}

// contextSpec is one entry of --contexts: name=dataset[:label=value,...].
type contextSpec struct {
	Name    string
	Dataset string
	Labels  map[string]string
}

// parseContexts reads a --contexts value such as `dev=small-healthy,uat=medium-degraded:stage=uat`.
func parseContexts(arg string) ([]contextSpec, error) {
	var out []contextSpec
	for _, part := range strings.Split(arg, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, rest, ok := strings.Cut(part, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("context %q: want name=dataset", part)
		}
		dataset, labelPart, _ := strings.Cut(rest, ":")
		if _, ok := scenarioByName(dataset); !ok {
			return nil, fmt.Errorf("context %q: unknown dataset %q", name, dataset)
		}
		labels := map[string]string{"stage": name}
		if labelPart != "" {
			labels = map[string]string{}
			for _, kv := range strings.Split(labelPart, ";") {
				k, v, _ := strings.Cut(kv, "=")
				if k != "" {
					labels[k] = v
				}
			}
		}
		if reservedContextNames[name] {
			return nil, fmt.Errorf("context %q: the name is an API path segment", name)
		}
		out = append(out, contextSpec{Name: name, Dataset: dataset, Labels: labels})
	}
	return out, nil
}

// reservedContextNames cannot name a context: under `/k8s/` they are API path
// segments of the local world.
var reservedContextNames = map[string]bool{
	"api": true, "apis": true, "version": true, "openapi": true, "healthz": true, "livez": true, "readyz": true, "metrics": true,
}

// worldSeed keeps --seed for the first world so a plain dataset reproduces
// as before, and mixes the name in for every other one.
func worldSeed(seed uint64, name string, first bool) uint64 {
	if first {
		return seed
	}
	h := fnv.New64a()
	h.Write([]byte(name))
	return seed ^ h.Sum64()
}

func (s *server) addWorld(spec contextSpec, first bool, viewer string) *world {
	sc, _ := scenarioByName(spec.Dataset)
	seed := worldSeed(s.seed, spec.Name, first)
	st := NewStore(seed)
	st.Register(allResourceDefs()...)
	Generate(st, sc, seed, s.namespace)
	authz := &AuthZ{store: st, viewer: parseViewer(viewer)}
	wd := &world{name: spec.Name, labels: spec.Labels, scenario: sc, seed: seed, store: st,
		k8s: &K8sAPI{store: st, authz: authz}, prom: &PromAPI{store: st, seed: seed}, authz: authz}
	s.worlds[spec.Name] = wd
	s.order = append(s.order, spec.Name)
	return wd
}

func (s *server) world(name string) *world { return s.worlds[name] }

// startSimulators runs one simulator per world at the interval.
func (s *server) startSimulators(interval time.Duration, failRate float64) {
	for _, name := range s.order {
		wd := s.worlds[name]
		wd.sim = NewSimulator(wd.store, s.namespace, failRate, wd.seed)
		if name == s.order[0] {
			s.sim = wd.sim
		}
		if interval > 0 {
			ctx, cancel := context.WithCancel(context.Background())
			wd.stopSim = cancel
			go wd.sim.Run(ctx, interval)
		}
	}
}

// contextHandler serves `/k8s/<ctx>/...` from the named world; a first segment
// that names no world falls through to the local one with the prefix
// stripped, so `/k8s/apis/...` keeps working as the local context.
func (s *server) contextHandler(prefix string, pick func(*world) http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		first, _, _ := strings.Cut(strings.TrimPrefix(rest, "/"), "/")
		if wd := s.world(first); wd != nil && first != "" {
			http.StripPrefix(prefix+"/"+first, pick(wd)).ServeHTTP(w, r)
			return
		}
		http.StripPrefix(prefix, pick(s.worlds[s.order[0]])).ServeHTTP(w, r)
	})
}

// consoleContext is one entry of config.js contexts.
type consoleContext struct {
	Name       string            `json:"name"`
	Labels     map[string]string `json:"labels"`
	Namespaces []string          `json:"namespaces"`
	K8sBase    string            `json:"k8sBase"`
	Local      bool              `json:"local"`
}

// clusterNamespaces lists the namespaces holding a StorageCluster, sorted.
func clusterNamespaces(st *Store) []string {
	d, ok := st.LookupResource(sbGroup, "storageclusters")
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for _, c := range mustList(st, d) {
		seen[getStr(c, "metadata.namespace")] = true
	}
	var out []string
	for ns := range seen {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

func (s *server) consoleContexts() []consoleContext {
	var out []consoleContext
	for i, name := range s.order {
		wd := s.worlds[name]
		cc := consoleContext{Name: name, Labels: wd.labels, Namespaces: clusterNamespaces(wd.store), K8sBase: "/k8s/" + name, Local: i == 0}
		if i == 0 {
			cc.K8sBase = "/k8s"
		}
		out = append(out, cc)
	}
	return out
}
