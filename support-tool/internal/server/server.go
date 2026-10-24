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
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/simplyblock/simplyblock-operator/support-tool/internal/analysis"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/cdcedit"
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

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadBytes)
	file, _, err := r.FormFile("bundle")
	if err != nil {
		fail(w, http.StatusBadRequest, "the request carries no bundle file: "+err.Error())
		return
	}
	defer func() { _ = file.Close() }()

	b, err := supportbundle.Read(file)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	id := s.keep(b)
	reply(w, map[string]any{"id": id, "analysis": analysis.Analyze(b)})
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
	var req cdcedit.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "the request does not parse: "+err.Error())
		return
	}
	cdc, err := cdcedit.Build(b, req)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	view := analysis.ViewOf(cdc)
	view.Findings = analysis.CheckCDC(b, cdc)
	reply(w, view)
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
