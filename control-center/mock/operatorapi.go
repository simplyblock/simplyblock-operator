package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// OperatorAPI stubs the operator's HTTP API (the /operator/v1/* surface the
// console uses for kinds no CRD models yet, the Helm release view and the
// observability reads).
//
// That surface is still settling (see control-center/README.md, "The API
// surface is still partly inferred"), so this handler is fixture-driven
// rather than hand-modeled: a request for /v1/foo/bar serves
// <fixtures>/v1/foo/bar.json when the file exists, falling back to
// <fixtures>/v1/foo/bar/index.json, and POST/PUT/PATCH/DELETE first try a
// method-suffixed file (bar.post.json). Anything without a fixture answers
// {"status":"success","data":[]} and is recorded, so /mockctl/info shows
// exactly which endpoints the UI called that nobody has modeled yet — that
// list is the to-do list for new fixtures.
type OperatorAPI struct {
	fixturesDir string

	mu       sync.Mutex
	unknown  []string
	unknownN map[string]int
}

func NewOperatorAPI(fixturesDir string) *OperatorAPI {
	return &OperatorAPI{fixturesDir: fixturesDir, unknownN: map[string]int{}}
}

func (o *OperatorAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(filepath.ToSlash(r.URL.Path), "/")
	if o.fixturesDir != "" {
		candidates := []string{
			rel + "." + strings.ToLower(r.Method) + ".json",
			rel + ".json",
			rel + "/index.json",
		}
		for _, c := range candidates {
			full := filepath.Join(o.fixturesDir, filepath.FromSlash(c))
			if data, err := os.ReadFile(full); err == nil {
				w.Header().Set("Content-Type", "application/json")
				w.Write(data)
				return
			}
		}
	}
	o.record(r.Method + " /" + rel)
	writeJSON(w, 200, map[string]any{"status": "success", "data": []any{}})
}

func (o *OperatorAPI) record(req string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.unknownN[req] == 0 {
		if len(o.unknown) < 500 {
			o.unknown = append(o.unknown, req)
		}
	}
	o.unknownN[req]++
}

// UnknownRequests lists distinct un-fixtured requests with hit counts.
func (o *OperatorAPI) UnknownRequests() map[string]int {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make(map[string]int, len(o.unknownN))
	for k, v := range o.unknownN {
		out[k] = v
	}
	return out
}
