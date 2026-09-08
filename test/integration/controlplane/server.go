package cpsim

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/simplyblock/atlas/ptr"
)

// Server is the simulator. Register state with AddCluster and friends, then
// Start it and point a client at URL.
type Server struct {
	token string

	mu       sync.RWMutex
	clusters map[uuid.UUID]Cluster
	nodes    map[uuid.UUID]StorageNode
	pools    map[uuid.UUID]Pool
	volumes  map[uuid.UUID]Volume

	listener net.Listener
	http     *http.Server
}

// Option configures a Server.
type Option func(*Server)

// WithToken requires this bearer token on every request. Empty accepts any,
// which is the default.
func WithToken(token string) Option {
	return func(s *Server) { s.token = token }
}

// New creates a simulator that is not yet listening.
func New(opts ...Option) *Server {
	s := &Server{
		clusters: map[uuid.UUID]Cluster{},
		nodes:    map[uuid.UUID]StorageNode{},
		pools:    map[uuid.UUID]Pool{},
		volumes:  map[uuid.UUID]Volume{},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Start begins listening on all interfaces on a port the kernel picks.
//
// All interfaces, not loopback: a control plane the cluster's nodes cannot reach
// is only half a stand-in, and a QEMU guest reaches its host on the network's
// gateway address. See URLFor.
func (s *Server) Start() error {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	s.listener = l
	s.http = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.http.Serve(l) }()
	return nil
}

// Close stops listening.
func (s *Server) Close() error {
	if s.http == nil {
		return nil
	}
	err := s.http.Close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Port is the port it listens on, valid after Start.
func (s *Server) Port() int {
	if s.listener == nil {
		return 0
	}
	return s.listener.Addr().(*net.TCPAddr).Port
}

// URL is the base URL for a client in this process.
func (s *Server) URL() string { return fmt.Sprintf("http://127.0.0.1:%d", s.Port()) }

// URLFor is the base URL for a client that reaches this host at addr — the
// cluster network's gateway, for something running on a node.
func (s *Server) URLFor(addr string) string {
	return fmt.Sprintf("http://%s:%d", addr, s.Port())
}

// AddCluster registers a cluster.
func (s *Server) AddCluster(c Cluster) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clusters[c.ID] = c
}

// AddNode registers a storage node.
func (s *Server) AddNode(n StorageNode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[n.ID] = n
}

// AddPool registers a pool.
func (s *Server) AddPool(p Pool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pools[p.ID] = p
}

// AddVolume registers a volume.
func (s *Server) AddVolume(v Volume) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.volumes[v.ID] = v
}

// RemoveVolume deletes a volume, so a test can make one disappear underneath a
// client without tearing the fabric down.
func (s *Server) RemoveVolume(id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.volumes, id)
}

// Handler is the routing, exposed so the simulator can be mounted into another
// server or driven with httptest.
//
// The mux comes from the generated router, so the paths, methods and parameter
// decoding are the spec's. Registering them by hand would mean the simulator
// answered the endpoints it happened to know about and 404'd the rest, which is
// indistinguishable from a missing volume.
func (s *Server) Handler() http.Handler {
	return s.authenticated(HandlerFromMux(s, http.NewServeMux()))
}

// authenticated rejects a request without the bearer token, when one is set.
func (s *Server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// notImplemented answers an endpoint the simulator does not simulate. The stubs
// in unimplemented.gen.go call it. A 501 rather than a 404: "the simulator does
// not do this" and "the volume is not there" are different answers, and a caller
// that cannot tell them apart debugs the wrong one.
func notImplemented(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "cpsim does not implement "+r.Method+" "+r.URL.Path, http.StatusNotImplemented)
}

// hostError is a host-authorization failure, which the control plane reports as
// a 404 carrying the message.
type hostError string

func (e hostError) Error() string { return string(e) }

func poolDTO(p Pool) StoragePoolDTO {
	return StoragePoolDTO{
		Id:        p.ID,
		ClusterId: p.ClusterID,
		Name:      p.Name,
		MaxSize:   p.MaxSize,
		Status:    StoragePoolDTOStatus("active"),
	}
}

func nodeDTO(n StorageNode) StorageNodeDTO {
	return StorageNodeDTO{
		Id:        n.ID,
		ClusterId: n.ClusterID,
		Hostname:  n.Hostname,
		MgmtIp:    n.MgmtIP,
		Status:    StorageNodeDTOStatus(n.Status),
	}
}

func volumeDTO(v Volume) VolumeDTO {
	nodes := make([]string, 0, len(v.Nodes))
	for _, id := range v.Nodes {
		nodes = append(nodes, id.String())
	}
	return VolumeDTO{
		Id:               v.ID,
		ClusterId:        v.ClusterID,
		PoolUuid:         v.PoolID.String(),
		Name:             v.Name,
		PoolName:         v.PoolName,
		Nqn:              v.NQN,
		NsId:             v.NSID,
		Size:             int(v.SizeBytes), //nolint:gosec // test-scale sizes
		Fabric:           v.Fabric,
		Port:             v.Port,
		Nodes:            nodes,
		HighAvailability: v.HighAvailability(),
		AllowedHosts:     v.allowedHostNQNs(),
		Namespace:        fmt.Sprintf("nsid%d", v.NSID),
		Hostname:         hostnameOf(nodes),
	}
}

func hostnameOf(nodes []string) string {
	if len(nodes) == 0 {
		return ""
	}
	return nodes[0]
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError answers in the control plane's validation-error shape, which is
// what a generated client decodes a 422 into.
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, HTTPValidationError{
		Detail: ptr.To([]ValidationError{{
			Loc: []ValidationError_Loc_Item{},
			Msg: err.Error(),
			Type: strings.ToLower(
				strings.ReplaceAll(http.StatusText(status), " ", "_")),
		}}),
	})
}
