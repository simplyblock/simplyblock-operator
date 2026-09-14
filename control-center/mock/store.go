package main

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// GVR identifies a resource type.
type GVR struct{ Group, Version, Resource string }

func (g GVR) apiVersion() string {
	if g.Group == "" {
		return g.Version
	}
	return g.Group + "/" + g.Version
}

// ResourceDef declares one resource type the mock serves. The registry in
// registry.go is the full list.
type ResourceDef struct {
	GVR
	Kind       string
	Namespaced bool
}

func (d ResourceDef) listKind() string { return d.Kind + "List" }

// WatchEvent is one entry on a watch stream.
type WatchEvent struct {
	Type   string         `json:"type"` // ADDED | MODIFIED | DELETED
	Object map[string]any `json:"object"`
}

type watcher struct {
	gvr GVR
	ns  string // "" = all namespaces
	ch  chan WatchEvent
}

// Store is the in-memory object database: Kubernetes API semantics
// (resourceVersion, generation, uid, watch) with no reconciler behind it —
// writes persist and notify watchers, and nothing real ever happens.
type Store struct {
	mu       sync.RWMutex
	rv       uint64
	defs     map[GVR]ResourceDef
	byGR     map[string]ResourceDef // group+"/"+resource -> def (any version)
	objs     map[GVR]map[string]map[string]any
	watchers map[GVR]map[*watcher]struct{}
	rng      *rand.Rand
	rngMu    sync.Mutex
	now      func() time.Time
}

func NewStore(seed uint64) *Store {
	return &Store{
		defs:     map[GVR]ResourceDef{},
		byGR:     map[string]ResourceDef{},
		objs:     map[GVR]map[string]map[string]any{},
		watchers: map[GVR]map[*watcher]struct{}{},
		rng:      rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		now:      time.Now,
	}
}

func (s *Store) Register(defs ...ResourceDef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range defs {
		s.defs[d.GVR] = d
		s.byGR[d.Group+"/"+d.Resource] = d
		if s.objs[d.GVR] == nil {
			s.objs[d.GVR] = map[string]map[string]any{}
		}
	}
}

// Lookup resolves a group+resource to its definition; the empty bool means the
// mock does not serve that type.
func (s *Store) Lookup(group, version, resource string) (ResourceDef, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.byGR[group+"/"+resource]
	if !ok || d.Version != version {
		return ResourceDef{}, false
	}
	return d, true
}

func (s *Store) Defs() []ResourceDef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ResourceDef, 0, len(s.defs))
	for _, d := range s.defs {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return a.Resource < b.Resource
	})
	return out
}

func (s *Store) uuid() string {
	s.rngMu.Lock()
	defer s.rngMu.Unlock()
	return uuidFrom(s.rng)
}

func key(ns, name string) string { return ns + "/" + name }

// apiError carries a Kubernetes Status through the handler layers.
type apiError struct {
	Code    int
	Reason  string
	Message string
}

func (e *apiError) Error() string { return e.Message }

func errNotFound(d ResourceDef, name string) *apiError {
	return &apiError{404, "NotFound", fmt.Sprintf("%s.%s %q not found", d.Resource, d.Group, name)}
}

func errConflict(d ResourceDef, name string) *apiError {
	return &apiError{409, "AlreadyExists", fmt.Sprintf("%s.%s %q already exists", d.Resource, d.Group, name)}
}

// Get returns a deep copy.
func (s *Store) Get(d ResourceDef, ns, name string) (map[string]any, *apiError) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.objs[d.GVR][key(ns, name)]
	if !ok {
		return nil, errNotFound(d, name)
	}
	return deepCopy(o).(map[string]any), nil
}

// List returns deep copies filtered by namespace ("" = all) and selectors.
func (s *Store) List(d ResourceDef, ns, labelSelector, fieldSelector string) ([]map[string]any, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []map[string]any
	for _, o := range s.objs[d.GVR] {
		if ns != "" && getStr(o, "metadata.namespace") != ns {
			continue
		}
		if !matchLabelSelector(o, labelSelector) || !matchFieldSelector(o, fieldSelector) {
			continue
		}
		out = append(out, deepCopy(o).(map[string]any))
	}
	sort.Slice(out, func(i, j int) bool {
		a := getStr(out[i], "metadata.namespace") + "/" + getStr(out[i], "metadata.name")
		b := getStr(out[j], "metadata.namespace") + "/" + getStr(out[j], "metadata.name")
		return a < b
	})
	return out, s.rv
}

// Create fills in the metadata a real apiserver would (uid, resourceVersion,
// generation, creationTimestamp) and announces the object to watchers.
func (s *Store) Create(d ResourceDef, ns string, obj map[string]any) (map[string]any, *apiError) {
	obj = deepCopy(obj).(map[string]any)
	obj["apiVersion"] = d.apiVersion()
	obj["kind"] = d.Kind
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		obj["metadata"] = meta
	}
	name, _ := meta["name"].(string)
	if name == "" {
		if gen, _ := meta["generateName"].(string); gen != "" {
			name = gen + randSuffix(s)
			meta["name"] = name
		} else {
			return nil, &apiError{422, "Invalid", "metadata.name or metadata.generateName is required"}
		}
	}
	if d.Namespaced {
		meta["namespace"] = ns
	} else {
		delete(meta, "namespace")
		ns = ""
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.objs[d.GVR][key(ns, name)]; exists {
		return nil, errConflict(d, name)
	}
	s.rv++
	// The generator pre-sets identity fields for objects that must look old;
	// anything a client creates gets fresh ones.
	if _, ok := meta["uid"]; !ok {
		meta["uid"] = uuidFrom(s.rng)
	}
	meta["resourceVersion"] = fmt.Sprint(s.rv)
	if _, ok := meta["generation"]; !ok {
		meta["generation"] = float64(1)
	}
	if _, ok := meta["creationTimestamp"]; !ok {
		meta["creationTimestamp"] = s.now().UTC().Format(time.RFC3339)
	}
	s.objs[d.GVR][key(ns, name)] = obj
	s.notifyLocked(d.GVR, WatchEvent{"ADDED", deepCopy(obj).(map[string]any)})
	return deepCopy(obj).(map[string]any), nil
}

// Update replaces the object, preserving identity fields and bumping
// generation when spec changed.
func (s *Store) Update(d ResourceDef, ns string, obj map[string]any) (map[string]any, *apiError) {
	obj = deepCopy(obj).(map[string]any)
	name := getStr(obj, "metadata.name")

	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.objs[d.GVR][key(ns, name)]
	if !ok {
		return nil, errNotFound(d, name)
	}
	obj["apiVersion"] = d.apiVersion()
	obj["kind"] = d.Kind
	meta := obj["metadata"].(map[string]any)
	oldMeta := old["metadata"].(map[string]any)
	meta["uid"] = oldMeta["uid"]
	meta["creationTimestamp"] = oldMeta["creationTimestamp"]
	if d.Namespaced {
		meta["namespace"] = ns
	}
	s.rv++
	meta["resourceVersion"] = fmt.Sprint(s.rv)
	gen, _ := oldMeta["generation"].(float64)
	if gen == 0 {
		gen = 1
	}
	if !reflect.DeepEqual(old["spec"], obj["spec"]) {
		gen++
	}
	meta["generation"] = gen
	s.objs[d.GVR][key(ns, name)] = obj
	s.notifyLocked(d.GVR, WatchEvent{"MODIFIED", deepCopy(obj).(map[string]any)})
	return deepCopy(obj).(map[string]any), nil
}

// Delete removes the object and announces it.
func (s *Store) Delete(d ResourceDef, ns, name string) (map[string]any, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.objs[d.GVR][key(ns, name)]
	if !ok {
		return nil, errNotFound(d, name)
	}
	delete(s.objs[d.GVR], key(ns, name))
	s.rv++
	s.notifyLocked(d.GVR, WatchEvent{"DELETED", deepCopy(old).(map[string]any)})
	return deepCopy(old).(map[string]any), nil
}

// Watch registers a stream. Semantics follow list-then-watch: no synthetic
// initial events; the caller lists first. Stop with the returned cancel.
func (s *Store) Watch(d ResourceDef, ns string) (<-chan WatchEvent, func()) {
	w := &watcher{gvr: d.GVR, ns: ns, ch: make(chan WatchEvent, 256)}
	s.mu.Lock()
	if s.watchers[d.GVR] == nil {
		s.watchers[d.GVR] = map[*watcher]struct{}{}
	}
	s.watchers[d.GVR][w] = struct{}{}
	s.mu.Unlock()
	cancel := func() {
		s.mu.Lock()
		if _, ok := s.watchers[d.GVR][w]; ok {
			delete(s.watchers[d.GVR], w)
			close(w.ch)
		}
		s.mu.Unlock()
	}
	return w.ch, cancel
}

// notifyLocked fans out; a watcher that cannot keep up is dropped (closed
// channel = client relists), which is what a real apiserver's "too old" does.
func (s *Store) notifyLocked(gvr GVR, ev WatchEvent) {
	ns := getStr(ev.Object, "metadata.namespace")
	for w := range s.watchers[gvr] {
		if w.ns != "" && w.ns != ns {
			continue
		}
		select {
		case w.ch <- ev:
		default:
			delete(s.watchers[gvr], w)
			close(w.ch)
		}
	}
}

func randSuffix(s *Store) string {
	const alphabet = "bcdfghjklmnpqrstvwxz2456789"
	s.rngMu.Lock()
	defer s.rngMu.Unlock()
	b := make([]byte, 5)
	for i := range b {
		b[i] = alphabet[s.rng.IntN(int(len(alphabet)))]
	}
	return string(b)
}

// ---- selectors --------------------------------------------------------------

// matchLabelSelector supports the forms clients actually send: equality
// (k=v, k==v, k!=v), existence (k, !k) and set-based (k in (a,b), k notin (a,b)).
func matchLabelSelector(o map[string]any, sel string) bool {
	sel = strings.TrimSpace(sel)
	if sel == "" {
		return true
	}
	labels := map[string]string{}
	if lm, ok := getPath(o, "metadata.labels").(map[string]any); ok {
		for k, v := range lm {
			labels[k] = fmt.Sprint(v)
		}
	}
	for _, term := range splitSelectorTerms(sel) {
		if !matchLabelTerm(labels, term) {
			return false
		}
	}
	return true
}

// splitSelectorTerms splits on commas that are not inside "in (a,b)" parens.
func splitSelectorTerms(sel string) []string {
	var out []string
	depth, start := 0, 0
	for i, c := range sel {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(sel[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(sel[start:]))
	return out
}

func matchLabelTerm(labels map[string]string, term string) bool {
	if term == "" {
		return true
	}
	if lower := strings.ToLower(term); strings.Contains(lower, " notin ") || strings.Contains(lower, " in ") {
		neg := strings.Contains(lower, " notin ")
		sep := " in "
		if neg {
			sep = " notin "
		}
		idx := strings.Index(lower, sep)
		k := strings.TrimSpace(term[:idx])
		vals := strings.Trim(strings.TrimSpace(term[idx+len(sep):]), "()")
		v, ok := labels[k]
		found := false
		for _, cand := range strings.Split(vals, ",") {
			if ok && strings.TrimSpace(cand) == v {
				found = true
			}
		}
		return found != neg
	}
	if i := strings.Index(term, "!="); i >= 0 {
		v, ok := labels[strings.TrimSpace(term[:i])]
		return !ok || v != strings.TrimSpace(term[i+2:])
	}
	if i := strings.Index(term, "=="); i >= 0 {
		return labels[strings.TrimSpace(term[:i])] == strings.TrimSpace(term[i+2:])
	}
	if i := strings.Index(term, "="); i >= 0 {
		return labels[strings.TrimSpace(term[:i])] == strings.TrimSpace(term[i+1:])
	}
	if strings.HasPrefix(term, "!") {
		_, ok := labels[strings.TrimSpace(term[1:])]
		return !ok
	}
	_, ok := labels[term]
	return ok
}

// matchFieldSelector supports dotted-path equality and inequality, which
// covers metadata.name/namespace and event involvedObject lookups.
func matchFieldSelector(o map[string]any, sel string) bool {
	sel = strings.TrimSpace(sel)
	if sel == "" {
		return true
	}
	for _, term := range strings.Split(sel, ",") {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		neg := false
		var k, v string
		if i := strings.Index(term, "!="); i >= 0 {
			neg, k, v = true, term[:i], term[i+2:]
		} else if i := strings.Index(term, "=="); i >= 0 {
			k, v = term[:i], term[i+2:]
		} else if i := strings.Index(term, "="); i >= 0 {
			k, v = term[:i], term[i+1:]
		} else {
			continue
		}
		got := getPath(o, strings.TrimSpace(k))
		eq := got != nil && fmt.Sprint(got) == strings.TrimSpace(v)
		if got == nil && strings.TrimSpace(v) == "" {
			eq = true
		}
		if eq == neg {
			return false
		}
	}
	return true
}
