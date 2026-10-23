// Package switchover moves the leadership of a volume of a stretched,
// sync-replicated cluster to a site (design-stretched-sync-replication-ramen.md
// §9.3).
//
// A volume of a sync cluster is served from the site its store is led from.
// When a pod stages on the other site, its node plugin asks its site's operator
// to move the volume there, and the operator serves this package's Server as
// SwitchoverService on its links. The server is the one caller of the
// backend's sync verbs: it demotes the site the volume leaves, then promotes the
// site it moves to.
//
// It never waits on the backend. A promote still running is IN_PROGRESS and the
// node asks again, which is the non-blocking wait the stage already makes, with
// the server as the one place that issues the verbs. Moves are serialized per
// store: a second move of a store whose first is under way is IN_PROGRESS, so
// two pods of one application staging together produce one demote and one
// promote.
package switchover

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/link/sitev1"
	"github.com/simplyblock/atlas/lvol"
)

// Backend is the control plane's sync verbs. *controlplane.Client is one.
type Backend interface {
	Volume(ctx context.Context, h lvol.VolumeHandle) (lvol.Volume, error)
	SyncStatus(ctx context.Context, h lvol.VolumeHandle) (controlplane.VolumeSyncStatus, error)
	SyncDemoteVolume(ctx context.Context, h lvol.VolumeHandle, site string) error
	SyncPromoteVolume(ctx context.Context, h lvol.VolumeHandle, site string, planned bool) (controlplane.SyncPromoteResult, error)
}

// Server serves SwitchoverService.
type Server struct {
	sitev1.UnimplementedSwitchoverServiceServer

	// OnForced is told when a move escalated to a forced promote: the volume,
	// the site it was served from, and the site it moved to. The operator
	// raises the SyncPeerSiteOffline event from it.
	OnForced func(volume, from, to string)

	backend Backend
	log     *slog.Logger

	mu     sync.Mutex
	moving map[string]bool // stores with a move under way
}

// New returns a server issuing its verbs to backend.
func New(backend Backend, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{backend: backend, log: logger, moving: map[string]bool{}}
}

// Register adds SwitchoverService to a server, such as a link session's.
func (s *Server) Register(srv grpc.ServiceRegistrar) { sitev1.RegisterSwitchoverServiceServer(srv, s) }

// Move moves the volume's leadership to the requested site, or reports where
// that stands.
func (s *Server) Move(ctx context.Context, req *sitev1.MoveRequest) (*sitev1.MoveResponse, error) {
	h := lvol.VolumeHandle(req.GetVolume())
	to := req.GetToSite()
	if to == "" {
		return nil, status.Error(codes.InvalidArgument, "switchover: no target site")
	}
	if _, _, _, err := h.Split(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "switchover: volume %q: %v", h, err)
	}

	st, err := s.backend.SyncStatus(ctx, h)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "switchover: status of %s: %v", h, err)
	}
	if st.Site == to {
		return moved(to), nil
	}

	vol, err := s.backend.Volume(ctx, h)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "switchover: volume %s: %v", h, err)
	}
	store := vol.StorageNodeID
	if !s.claim(store) {
		return inProgress(st.Site), nil
	}
	defer s.release(store)

	from := st.Site
	if from != "" {
		if err := s.backend.SyncDemoteVolume(ctx, h, from); err != nil {
			var refused *controlplane.SyncRefusedError
			switch {
			case errors.As(err, &refused):
				return refusedBy(from, refused), nil
			case errors.Is(err, controlplane.ErrSyncPeerSiteOffline):
				// The old site cannot be reached to demote. The promote
				// decides: the backend refuses a planned one against a site
				// it has not proven lost, and allows a forced one once it has.
				s.log.Info("switchover: the old site cannot be demoted; promoting", "volume", h, "from", from, "to", to)
			default:
				return nil, status.Errorf(codes.Unavailable, "switchover: demote %s on %s: %v", h, from, err)
			}
		}
	}

	res, err := s.backend.SyncPromoteVolume(ctx, h, to, true)
	if errors.Is(err, controlplane.ErrSyncPeerSiteOffline) && req.GetAllowForced() {
		s.log.Warn("switchover: the volume's site is not online; forcing the promote", "volume", h, "from", from, "to", to)
		if s.OnForced != nil {
			s.OnForced(string(h), from, to)
		}
		res, err = s.backend.SyncPromoteVolume(ctx, h, to, false)
	}
	if err != nil {
		var refused *controlplane.SyncRefusedError
		if errors.As(err, &refused) {
			return refusedBy(from, refused), nil
		}
		if errors.Is(err, controlplane.ErrSyncPeerSiteOffline) {
			return &sitev1.MoveResponse{State: sitev1.MoveResponse_STATE_REFUSED, LedFrom: from,
				Reason: fmt.Sprintf("site %s is not online and the move may not be forced", from)}, nil
		}
		return nil, status.Errorf(codes.Unavailable, "switchover: promote %s on %s: %v", h, to, err)
	}
	if res.InProgress {
		return inProgress(from), nil
	}
	s.log.Info("switchover: moved", "volume", h, "from", from, "to", to)
	return moved(to), nil
}

// claim marks a store's move under way, and reports false when one already is.
func (s *Server) claim(store string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.moving[store] {
		return false
	}
	s.moving[store] = true
	return true
}

func (s *Server) release(store string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.moving, store)
}

func moved(site string) *sitev1.MoveResponse {
	return &sitev1.MoveResponse{State: sitev1.MoveResponse_STATE_MOVED, LedFrom: site}
}

func inProgress(ledFrom string) *sitev1.MoveResponse {
	return &sitev1.MoveResponse{State: sitev1.MoveResponse_STATE_IN_PROGRESS, LedFrom: ledFrom}
}

func refusedBy(ledFrom string, e *controlplane.SyncRefusedError) *sitev1.MoveResponse {
	return &sitev1.MoveResponse{
		State: sitev1.MoveResponse_STATE_REFUSED, LedFrom: ledFrom,
		Reason: e.Message, BlockingVolumes: e.Volumes,
	}
}
