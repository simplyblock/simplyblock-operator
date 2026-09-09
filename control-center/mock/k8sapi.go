package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// K8sAPI serves Kubernetes API conventions — discovery, list/get/create/
// update/patch/delete, /status subresources, pod logs and streaming watches —
// against the Store. It expects paths relative to the API root (/api, /apis,
// /version); mount it behind http.StripPrefix when proxied under /k8s.
type K8sAPI struct {
	store *Store
}

func (a *K8sAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path)
	if len(parts) == 0 {
		writeJSON(w, 200, map[string]any{"paths": []string{"/api", "/apis", "/version"}})
		return
	}
	switch parts[0] {
	case "version":
		writeJSON(w, 200, map[string]any{
			"major": "1", "minor": "31", "gitVersion": "v1.31.0-sbmock",
			"platform": "linux/amd64",
		})
	case "healthz", "livez", "readyz":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	case "api":
		if len(parts) == 1 {
			writeJSON(w, 200, map[string]any{"kind": "APIVersions", "versions": []string{"v1"}})
			return
		}
		if parts[1] != "v1" {
			a.writeStatus(w, &apiError{404, "NotFound", "unknown core version " + parts[1]})
			return
		}
		a.serveGroup(w, r, "", "v1", parts[2:])
	case "apis":
		switch len(parts) {
		case 1:
			a.serveGroupList(w)
		case 2:
			a.serveAPIGroup(w, parts[1])
		default:
			a.serveGroup(w, r, parts[1], parts[2], parts[3:])
		}
	default:
		a.writeStatus(w, &apiError{404, "NotFound", "the server could not find the requested resource"})
	}
}

// ---- discovery ---------------------------------------------------------------

func (a *K8sAPI) groupVersions() map[string][]string {
	gv := map[string][]string{}
	for _, d := range a.store.Defs() {
		if d.Group == "" {
			continue
		}
		found := false
		for _, v := range gv[d.Group] {
			if v == d.Version {
				found = true
			}
		}
		if !found {
			gv[d.Group] = append(gv[d.Group], d.Version)
		}
	}
	for _, vs := range gv {
		sort.Strings(vs)
	}
	return gv
}

func (a *K8sAPI) serveGroupList(w http.ResponseWriter) {
	gv := a.groupVersions()
	groups := make([]string, 0, len(gv))
	for g := range gv {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	var list []any
	for _, g := range groups {
		list = append(list, apiGroupObj(g, gv[g]))
	}
	writeJSON(w, 200, map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": list})
}

func (a *K8sAPI) serveAPIGroup(w http.ResponseWriter, group string) {
	gv := a.groupVersions()
	versions, ok := gv[group]
	if !ok {
		a.writeStatus(w, &apiError{404, "NotFound", "unknown API group " + group})
		return
	}
	writeJSON(w, 200, apiGroupObj(group, versions))
}

func apiGroupObj(group string, versions []string) map[string]any {
	var gvs []any
	for _, v := range versions {
		gvs = append(gvs, map[string]any{"groupVersion": group + "/" + v, "version": v})
	}
	return map[string]any{
		"kind": "APIGroup", "apiVersion": "v1", "name": group,
		"versions": gvs, "preferredVersion": gvs[0],
	}
}

func (a *K8sAPI) serveResourceList(w http.ResponseWriter, group, version string) {
	gvPath := version
	if group != "" {
		gvPath = group + "/" + version
	}
	var resources []any
	for _, d := range a.store.Defs() {
		if d.Group != group || d.Version != version {
			continue
		}
		resources = append(resources, map[string]any{
			"name": d.Resource, "singularName": strings.ToLower(d.Kind), "kind": d.Kind,
			"namespaced": d.Namespaced,
			"verbs":      []string{"create", "delete", "get", "list", "patch", "update", "watch"},
		})
		resources = append(resources, map[string]any{
			"name": d.Resource + "/status", "singularName": "", "kind": d.Kind,
			"namespaced": d.Namespaced, "verbs": []string{"get", "patch", "update"},
		})
	}
	writeJSON(w, 200, map[string]any{
		"kind": "APIResourceList", "apiVersion": "v1", "groupVersion": gvPath, "resources": resources,
	})
}

// ---- resource routing ---------------------------------------------------------

// serveGroup handles everything after /api/v1 or /apis/{g}/{v}.
func (a *K8sAPI) serveGroup(w http.ResponseWriter, r *http.Request, group, version string, rest []string) {
	if len(rest) == 0 {
		a.serveResourceList(w, group, version)
		return
	}

	ns, resource, name, sub := "", "", "", ""
	if rest[0] == "namespaces" && len(rest) >= 3 {
		// /..../namespaces/{ns}/{resource}[/{name}[/{sub}]]
		ns, resource = rest[1], rest[2]
		if len(rest) >= 4 {
			name = rest[3]
		}
		if len(rest) >= 5 {
			sub = rest[4]
		}
	} else {
		// /..../{resource}[/{name}[/{sub}]] — cluster-scoped, or an all-namespace
		// list; includes /api/v1/namespaces itself.
		resource = rest[0]
		if len(rest) >= 2 {
			name = rest[1]
		}
		if len(rest) >= 3 {
			sub = rest[2]
		}
	}

	d, ok := a.store.Lookup(group, version, resource)
	if !ok {
		a.writeStatus(w, &apiError{404, "NotFound", fmt.Sprintf("the server could not find resource %q in %s/%s", resource, group, version)})
		return
	}
	if d.Namespaced && ns == "" && name != "" && r.Method != http.MethodGet {
		a.writeStatus(w, &apiError{405, "MethodNotAllowed", "namespace required for writes to namespaced resources"})
		return
	}

	switch {
	case name == "":
		a.serveCollection(w, r, d, ns)
	case sub == "":
		a.serveObject(w, r, d, ns, name)
	case sub == "status":
		a.serveObject(w, r, d, ns, name) // the mock applies status writes to the object itself
	case sub == "log" && d.Resource == "pods":
		a.servePodLog(w, d, ns, name)
	default:
		a.writeStatus(w, &apiError{404, "NotFound", "subresource " + sub + " is not served by the mock"})
	}
}

func (a *K8sAPI) serveCollection(w http.ResponseWriter, r *http.Request, d ResourceDef, ns string) {
	q := r.URL.Query()
	switch r.Method {
	case http.MethodGet:
		if q.Get("watch") == "true" || q.Get("watch") == "1" {
			a.serveWatch(w, r, d, ns)
			return
		}
		items, rv := a.store.List(d, ns, q.Get("labelSelector"), q.Get("fieldSelector"))
		if lim := q.Get("limit"); lim != "" {
			if n, err := strconv.Atoi(lim); err == nil && n >= 0 && n < len(items) {
				items = items[:n]
			}
		}
		if items == nil {
			items = []map[string]any{}
		}
		writeJSON(w, 200, map[string]any{
			"kind": d.listKind(), "apiVersion": d.apiVersion(),
			"metadata": map[string]any{"resourceVersion": fmt.Sprint(rv)},
			"items":    items,
		})
	case http.MethodPost:
		obj, err := readBody(r)
		if err != nil {
			a.writeStatus(w, err)
			return
		}
		created, err := a.store.Create(d, ns, obj)
		if err != nil {
			a.writeStatus(w, err)
			return
		}
		writeJSON(w, 201, created)
	default:
		a.writeStatus(w, &apiError{405, "MethodNotAllowed", r.Method + " is not allowed on collections"})
	}
}

func (a *K8sAPI) serveObject(w http.ResponseWriter, r *http.Request, d ResourceDef, ns, name string) {
	switch r.Method {
	case http.MethodGet:
		obj, err := a.store.Get(d, ns, name)
		if err != nil {
			a.writeStatus(w, err)
			return
		}
		writeJSON(w, 200, obj)
	case http.MethodPut:
		obj, err := readBody(r)
		if err != nil {
			a.writeStatus(w, err)
			return
		}
		if getStr(obj, "metadata.name") == "" {
			setPath(obj, "metadata.name", name)
		}
		updated, err := a.store.Update(d, ns, obj)
		if err != nil {
			a.writeStatus(w, err)
			return
		}
		writeJSON(w, 200, updated)
	case http.MethodPatch:
		a.servePatch(w, r, d, ns, name)
	case http.MethodDelete:
		old, err := a.store.Delete(d, ns, name)
		if err != nil {
			a.writeStatus(w, err)
			return
		}
		writeJSON(w, 200, old)
	default:
		a.writeStatus(w, &apiError{405, "MethodNotAllowed", r.Method + " is not allowed"})
	}
}

func (a *K8sAPI) servePatch(w http.ResponseWriter, r *http.Request, d ResourceDef, ns, name string) {
	body, ioErr := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if ioErr != nil {
		a.writeStatus(w, &apiError{400, "BadRequest", ioErr.Error()})
		return
	}
	current, err := a.store.Get(d, ns, name)
	ct := r.Header.Get("Content-Type")

	// Server-side apply creates on missing objects; the other patch types 404.
	if err != nil {
		if strings.Contains(ct, "apply-patch") {
			var obj map[string]any
			if jerr := json.Unmarshal(body, &obj); jerrCheck(jerr) {
				a.writeStatus(w, &apiError{400, "BadRequest", "invalid apply patch: " + jerr.Error()})
				return
			}
			setPath(obj, "metadata.name", name)
			created, cerr := a.store.Create(d, ns, obj)
			if cerr != nil {
				a.writeStatus(w, cerr)
				return
			}
			writeJSON(w, 201, created)
			return
		}
		a.writeStatus(w, err)
		return
	}

	var patched map[string]any
	switch {
	case strings.Contains(ct, "json-patch"): // RFC 6902
		var ops []jsonPatchOp
		if jerr := json.Unmarshal(body, &ops); jerrCheck(jerr) {
			a.writeStatus(w, &apiError{400, "BadRequest", "invalid JSON patch: " + jerr.Error()})
			return
		}
		res, perr := applyJSONPatch(current, ops)
		if perr != nil {
			a.writeStatus(w, &apiError{422, "Invalid", perr.Error()})
			return
		}
		patched = res
	default:
		// merge-patch, strategic-merge-patch and apply all reduce to a JSON
		// merge for the mock; strategic list semantics are not reproduced.
		var patch any
		if jerr := json.Unmarshal(body, &patch); jerrCheck(jerr) {
			a.writeStatus(w, &apiError{400, "BadRequest", "invalid merge patch: " + jerr.Error()})
			return
		}
		patched = mergePatch(current, patch).(map[string]any)
	}
	setPath(patched, "metadata.name", name)
	updated, uerr := a.store.Update(d, ns, patched)
	if uerr != nil {
		a.writeStatus(w, uerr)
		return
	}
	writeJSON(w, 200, updated)
}

func (a *K8sAPI) servePodLog(w http.ResponseWriter, d ResourceDef, ns, name string) {
	if _, err := a.store.Get(d, ns, name); err != nil {
		a.writeStatus(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	now := time.Now().UTC()
	for i := 60; i > 0; i-- {
		ts := now.Add(-time.Duration(i) * 7 * time.Second).Format(time.RFC3339)
		level := "INFO"
		msg := "reactor poll cycle complete"
		switch i % 9 {
		case 0:
			level, msg = "WARNING", "RPC latency above threshold: 1873us"
		case 4:
			msg = "bdev io stats flushed"
		case 7:
			msg = "nvmf subsystem heartbeat ok"
		}
		fmt.Fprintf(w, "%s %s [%s] %s\n", ts, level, name, msg)
	}
}

// serveWatch streams newline-delimited watch events until the client leaves.
func (a *K8sAPI) serveWatch(w http.ResponseWriter, r *http.Request, d ResourceDef, ns string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		a.writeStatus(w, &apiError{500, "InternalError", "streaming unsupported"})
		return
	}
	ch, cancel := a.store.Watch(d, ns)
	defer cancel()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	flusher.Flush()

	enc := json.NewEncoder(w)
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-ch:
			if !open {
				// Watcher was dropped for falling behind; end the stream the way
				// an apiserver does so the client relists.
				enc.Encode(WatchEvent{"ERROR", map[string]any{
					"kind": "Status", "apiVersion": "v1", "status": "Failure",
					"reason": "Expired", "message": "too old resource version", "code": float64(410),
				}})
				return
			}
			if q := r.URL.Query(); !matchLabelSelector(ev.Object, q.Get("labelSelector")) ||
				!matchFieldSelector(ev.Object, q.Get("fieldSelector")) {
				continue
			}
			if err := enc.Encode(ev); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			// keep intermediaries from timing the stream out
			if _, err := fmt.Fprint(w, "\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---- helpers ------------------------------------------------------------------

func jerrCheck(err error) bool { return err != nil }

func readBody(r *http.Request) (map[string]any, *apiError) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return nil, &apiError{400, "BadRequest", err.Error()}
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, &apiError{400, "BadRequest", "invalid JSON body: " + err.Error()}
	}
	return obj, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (a *K8sAPI) writeStatus(w http.ResponseWriter, err *apiError) {
	writeJSON(w, err.Code, map[string]any{
		"kind": "Status", "apiVersion": "v1", "status": "Failure",
		"message": err.Message, "reason": err.Reason, "code": err.Code,
	})
}

// ---- RFC 6902 JSON patch (add / remove / replace / test) ----------------------

type jsonPatchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

func applyJSONPatch(doc map[string]any, ops []jsonPatchOp) (map[string]any, error) {
	cur := deepCopy(doc).(map[string]any)
	for _, op := range ops {
		var val any
		if len(op.Value) > 0 {
			if err := json.Unmarshal(op.Value, &val); err != nil {
				return nil, fmt.Errorf("bad value for %s %s: %w", op.Op, op.Path, err)
			}
		}
		if err := applyPatchOp(cur, op.Op, op.Path, val); err != nil {
			return nil, err
		}
	}
	return cur, nil
}

func applyPatchOp(doc map[string]any, op, path string, val any) error {
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i := range segs {
		segs[i] = strings.ReplaceAll(strings.ReplaceAll(segs[i], "~1", "/"), "~0", "~")
	}
	parent := any(doc)
	for _, seg := range segs[:len(segs)-1] {
		switch p := parent.(type) {
		case map[string]any:
			next, ok := p[seg]
			if !ok {
				if op == "add" {
					next = map[string]any{}
					p[seg] = next
				} else {
					return fmt.Errorf("path %s does not exist", path)
				}
			}
			parent = next
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(p) {
				return fmt.Errorf("bad array index in %s", path)
			}
			parent = p[idx]
		default:
			return fmt.Errorf("path %s traverses a scalar", path)
		}
	}
	last := segs[len(segs)-1]
	switch p := parent.(type) {
	case map[string]any:
		switch op {
		case "add", "replace":
			p[last] = val
		case "remove":
			delete(p, last)
		case "test":
			if fmt.Sprint(p[last]) != fmt.Sprint(val) {
				return fmt.Errorf("test failed at %s", path)
			}
		default:
			return fmt.Errorf("unsupported patch op %q", op)
		}
	case []any:
		// Array element ops other than append are rare from the console; the
		// mock supports append ("-") and index replace/test.
		idx := -1
		if last != "-" {
			var err error
			if idx, err = strconv.Atoi(last); err != nil || idx < 0 || idx >= len(p) {
				return fmt.Errorf("bad array index in %s", path)
			}
		}
		switch op {
		case "add":
			return fmt.Errorf("array insert is not supported by the mock (append via a full replace)")
		case "replace":
			p[idx] = val
		case "test":
			if fmt.Sprint(p[idx]) != fmt.Sprint(val) {
				return fmt.Errorf("test failed at %s", path)
			}
		default:
			return fmt.Errorf("unsupported patch op %q on array", op)
		}
	default:
		return fmt.Errorf("path %s traverses a scalar", path)
	}
	return nil
}
