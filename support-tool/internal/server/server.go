// The HTTP side of the support tool: the page, and the JSON API the page calls
// to upload a bundle, read its analysis, and build a ClusterDeploymentConfig.
//
// It runs on the support agent's own machine and holds customer data, so it
// answers only to requests addressed to a loopback name. Listening on loopback
// keeps other machines out, and checking the Host header keeps out a web page
// that resolves its own domain to 127.0.0.1 to read what the agent uploaded.
// Bundles are kept in memory only, and the oldest is dropped past a limit.

package server

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/simplyblock/simplyblock-operator/support-tool/internal/analysis"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/cdcedit"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/helmvalues"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/opsedit"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/supportbundle"
)

// MaxUploadBytes is the largest bundle the server accepts.
const MaxUploadBytes = 512 << 20

// maxBundles is how many uploaded bundles are kept before the oldest is
// dropped.
const maxBundles = 16

//go:embed web
var web embed.FS

// Server serves the page and the API.
type Server struct {
	mux *http.ServeMux

	mu      sync.Mutex
	bundles map[string]*supportbundle.Bundle
	order   []string
}

// New returns a server holding no bundles.
func New() *Server {
	s := &Server{mux: http.NewServeMux(), bundles: map[string]*supportbundle.Bundle{}}
	page, err := fs.Sub(web, "web")
	if err != nil {
		panic(err)
	}
	s.mux.Handle("GET /", http.FileServer(http.FS(page)))
	s.mux.HandleFunc("POST /api/bundles", s.upload)
	s.mux.HandleFunc("GET /api/bundles/{id}", s.analysis)
	s.mux.HandleFunc("GET /api/bundles/{id}/cdc/prefill", s.prefill)
	s.mux.HandleFunc("POST /api/bundles/{id}/cdc", s.build)
	s.mux.HandleFunc("POST /api/bundles/{id}/cdc/stripes", s.stripes)
	s.mux.HandleFunc("GET /api/meta", s.meta)
	s.mux.HandleFunc("GET /api/operatorops/prefill", s.opsPrefill)
	s.mux.HandleFunc("POST /api/operatorops", s.opsBuild)
	s.mux.HandleFunc("GET /api/helm", s.helmChart)
	s.mux.HandleFunc("POST /api/helm/values", s.helmValues)
	s.mux.HandleFunc("POST /api/helm/import", s.helmImport)
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !loopbackHost(r.Host) {
		http.Error(w, "this server answers only to localhost", http.StatusForbidden)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	s.mux.ServeHTTP(w, r)
}

// loopbackHost reports whether a Host header names this machine.
func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// upload reads the bundle part of the request as it arrives. FormFile would
// buffer the request first, and past its 32 MiB budget into a temporary file,
// which is customer data on the agent's disk.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadBytes)
	parts, err := r.MultipartReader()
	if err != nil {
		fail(w, http.StatusBadRequest, "the request is not a file upload: "+err.Error())
		return
	}
	for {
		part, err := parts.NextPart()
		if err != nil {
			fail(w, http.StatusBadRequest, "the request carries no bundle file")
			return
		}
		if part.FormName() != "bundle" {
			continue
		}
		b, err := supportbundle.Read(part)
		_ = part.Close()
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		id := s.keep(b)
		reply(w, map[string]any{"id": id, "analysis": analysis.Analyze(b)})
		return
	}
}

func (s *Server) analysis(w http.ResponseWriter, r *http.Request) {
	if b := s.bundle(w, r); b != nil {
		reply(w, analysis.Analyze(b))
	}
}

func (s *Server) prefill(w http.ResponseWriter, r *http.Request) {
	if b := s.bundle(w, r); b != nil {
		reply(w, cdcedit.Prefill(b))
	}
}

// build answers with the document the request describes and every finding
// about it, or with why no document can state the request.
func (s *Server) build(w http.ResponseWriter, r *http.Request) {
	b := s.bundle(w, r)
	if b == nil {
		return
	}
	req, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	cdc, err := cdcedit.Build(b, req)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	view := analysis.ViewOf(cdc)
	view.Findings = analysis.CheckCDC(b, cdc)
	// A document has no field for a forced journal: forcing it is accepting
	// the tie, so the warning is the editor's to drop.
	if !req.Cluster.ForceJournalDevice {
		view.Findings = append(view.Findings, analysis.JournalTies(b, cdc)...)
	}
	reply(w, view)
}

// stripes answers with every supported scheme and whether the request's
// workers and devices carry it, by the operator's own checks. A request no
// document can state has no options, and says why.
func (s *Server) stripes(w http.ResponseWriter, r *http.Request) {
	b := s.bundle(w, r)
	if b == nil {
		return
	}
	req, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	options := []analysis.StripeOption{}
	cdc, err := cdcedit.Build(b, req)
	if err != nil {
		reply(w, map[string]any{"options": options, "error": err.Error()})
		return
	}
	if found := analysis.StripeOptions(b, cdc); found != nil {
		options = found
	}
	reply(w, map[string]any{"options": options})
}

// opsPrefill is a discovery run to start from: the defaults, or the settings of
// the run in the bundle the query names.
func (s *Server) opsPrefill(w http.ResponseWriter, r *http.Request) {
	var b *supportbundle.Bundle
	if id := r.URL.Query().Get("bundle"); id != "" {
		s.mu.Lock()
		b = s.bundles[id]
		s.mu.Unlock()
	}
	reply(w, opsedit.Prefill(b))
}

// opsBuild answers with the run a request describes, as YAML, and every reason
// the API server would refuse it.
func (s *Server) opsBuild(w http.ResponseWriter, r *http.Request) {
	var req opsedit.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "the request does not parse: "+err.Error())
		return
	}
	ops, err := opsedit.Build(req)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := opsedit.Render(ops)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	findings := opsedit.Check(ops)
	if findings == nil {
		findings = []opsedit.Finding{}
	}
	reply(w, map[string]any{"yaml": string(out), "findings": findings})
}

// helmChart is the chart the values editor edits: its name, its version, and
// every value with its default and its documentation.
func (s *Server) helmChart(w http.ResponseWriter, _ *http.Request) {
	name, version := helmvalues.Chart()
	reply(w, map[string]any{
		"chart":  map[string]string{"name": name, "version": version},
		"fields": helmvalues.Fields(),
	})
}

// helmValues answers with the overrides as a values file, every problem with
// them, and the commands that install or upgrade the chart with it.
func (s *Server) helmValues(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Overrides map[string]any `json:"overrides"`
		// YAML is the values the editor takes as YAML text, by dotted path.
		YAML map[string]string `json:"yaml"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "the request does not parse: "+err.Error())
		return
	}
	if req.Overrides == nil {
		req.Overrides = map[string]any{}
	}
	for path, text := range req.YAML {
		if err := helmvalues.SetYAML(req.Overrides, path, text); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	out, err := helmvalues.Render(req.Overrides)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	name, version := helmvalues.Chart()
	reply(w, map[string]any{
		"yaml":     string(out),
		"findings": helmvalues.Check(req.Overrides),
		"commands": []string{
			"helm repo add simplyblock https://simplyblock.github.io/helm-charts/charts",
			"helm repo update",
			fmt.Sprintf("helm install %s simplyblock/%s --namespace simplyblock --create-namespace --version %s -f values.yaml", name, name, version),
			fmt.Sprintf("helm upgrade %s simplyblock/%s --namespace simplyblock --version %s -f values.yaml", name, name, version),
		},
	})
}

// helmImport reads a values file into overrides, with every problem the chart's
// schema and values find in it: what support checks a customer's file with.
func (s *Server) helmImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "the request does not parse: "+err.Error())
		return
	}
	overrides, err := helmvalues.Import(req.Text)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	reply(w, map[string]any{"overrides": overrides, "findings": helmvalues.Check(overrides)})
}

// meta is what the editor offers that does not depend on a bundle.
func (s *Server) meta(w http.ResponseWriter, _ *http.Request) {
	reply(w, map[string]any{
		"environments": cdcedit.Environments,
		"collect": map[string]any{
			"command": CollectCommand, "withOptions": CollectCommandWithOptions, "manual": ManualCollection(),
		},
	})
}

func decodeRequest(w http.ResponseWriter, r *http.Request) (cdcedit.Request, bool) {
	var req cdcedit.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "the request does not parse: "+err.Error())
		return req, false
	}
	return req, true
}

func (s *Server) keep(b *supportbundle.Bundle) string {
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	id := hex.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.bundles[id] = b
	s.order = append(s.order, id)
	for len(s.order) > maxBundles {
		delete(s.bundles, s.order[0])
		s.order = s.order[1:]
	}
	return id
}

func (s *Server) bundle(w http.ResponseWriter, r *http.Request) *supportbundle.Bundle {
	s.mu.Lock()
	b, ok := s.bundles[r.PathValue("id")]
	s.mu.Unlock()
	if !ok {
		fail(w, http.StatusNotFound, "no such bundle: upload it again")
		return nil
	}
	return b
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
