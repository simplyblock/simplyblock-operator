package mock

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

type RouteResponse struct {
	Status  int
	Body    string
	Headers map[string]string
}

type routeKey struct {
	method string
	path   string
}

type specDoc struct {
	Paths map[string]map[string]json.RawMessage `json:"paths"`
}

type SpecServer struct {
	t            *testing.T
	spec         specDoc
	allowUnknown bool

	mu     sync.Mutex
	routes map[routeKey][]RouteResponse
	reqs   []RecordedRequest
	server *httptest.Server
}

type RecordedRequest struct {
	Method  string
	Path    string
	Headers map[string]string
	Body    []byte
}

func NewSpecServerFromFile(t *testing.T, specPath string, allowUnknown bool) *SpecServer {
	t.Helper()

	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("failed to read openapi spec %q: %v", specPath, err)
	}

	var doc specDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("failed to parse openapi spec %q: %v", specPath, err)
	}

	s := &SpecServer{
		t:            t,
		spec:         doc,
		allowUnknown: allowUnknown,
		routes:       make(map[routeKey][]RouteResponse),
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	return s
}

func (s *SpecServer) Close() {
	s.server.Close()
}

func (s *SpecServer) URL() string {
	return s.server.URL
}

func (s *SpecServer) Requests() []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]RecordedRequest, len(s.reqs))
	for i, req := range s.reqs {
		var copied RecordedRequest
		copied.Method = req.Method
		copied.Path = req.Path

		if req.Headers != nil {
			copied.Headers = make(map[string]string, len(req.Headers))
			for k, v := range req.Headers {
				copied.Headers[k] = v
			}
		}

		if req.Body != nil {
			copied.Body = make([]byte, len(req.Body))
			copy(copied.Body, req.Body)
		}

		out[i] = copied
	}
	return out
}

func (s *SpecServer) Register(method, path string, responses ...RouteResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := routeKey{
		method: strings.ToUpper(strings.TrimSpace(method)),
		path:   normalizePath(path),
	}
	s.routes[key] = append(s.routes[key], responses...)
}

func (s *SpecServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.t.Helper()

	method := strings.ToUpper(r.Method)
	path := normalizePath(r.URL.Path)
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	headers := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}

	if !s.allowUnknown && !s.pathInSpec(method, path) {
		msg := fmt.Sprintf("request %s %s is not defined in openapi spec", method, path)
		s.t.Log(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	key := routeKey{method: method, path: path}

	s.mu.Lock()
	s.reqs = append(s.reqs, RecordedRequest{
		Method:  method,
		Path:    path,
		Headers: headers,
		Body:    body,
	})
	responses := s.routes[key]
	var resp RouteResponse
	if len(responses) > 0 {
		resp = responses[0]
		if len(responses) == 1 {
			s.routes[key] = responses
		} else {
			s.routes[key] = responses[1:]
		}
	}
	s.mu.Unlock()

	if len(responses) == 0 {
		msg := fmt.Sprintf("no mock response registered for %s %s", method, path)
		s.t.Log(msg)
		http.Error(w, msg, http.StatusNotImplemented)
		return
	}

	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	if resp.Status == 0 {
		resp.Status = http.StatusOK
	}
	w.WriteHeader(resp.Status)
	if resp.Body != "" {
		_, _ = w.Write([]byte(resp.Body))
	}
}

func (s *SpecServer) pathInSpec(method, path string) bool {
	for template, ops := range s.spec.Paths {
		if !templateMatchesPath(template, path) {
			continue
		}
		if _, ok := ops[strings.ToLower(method)]; ok {
			return true
		}
	}
	return false
}

func templateMatchesPath(template, path string) bool {
	tpl := splitPath(template)
	req := splitPath(path)
	if len(tpl) != len(req) {
		return false
	}
	for i := range tpl {
		part := tpl[i]
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			continue
		}
		if part != req[i] {
			return false
		}
	}
	return true
}

func splitPath(p string) []string {
	normalized := normalizePath(p)
	if normalized == "/" {
		return []string{""}
	}
	return strings.Split(strings.TrimPrefix(normalized, "/"), "/")
}

func normalizePath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
		if p == "" {
			p = "/"
		}
	}
	return p
}
