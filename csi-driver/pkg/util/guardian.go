package util

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/locks"
	atlasnvme "github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/ptr"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	sbkube "github.com/spdk/spdk-csi/pkg/kubernetes"
)

// defaultBrokenLvolGracePeriod is the default value for BrokenLvolGracePeriod.
const defaultBrokenLvolGracePeriod = 90 * time.Second

type GuardianConfig struct {
	NodeName         string
	PollInterval     time.Duration
	RestartBackoff   time.Duration
	GraceSeconds     int64
	OptInLabelKey    string
	OptInLabelValue  string
	OptOutLabelKey   string
	OptOutLabelValue string
	DryRun           bool

	// Minimum time a lvol must remain "broken" before we restart pods after cluster is active.
	MinBrokenFor time.Duration

	// BrokenLvolGracePeriod is how long to wait after the first broken lvol
	// is detected before checking cluster status. This gives the cluster time
	// to transition from active to suspended before the guardian evaluates
	// whether to restart pods.
	BrokenLvolGracePeriod time.Duration

	StatePath     string
	CSIDriverName string

	// DeviceResolver is used to look up which NVMe-oF subsystem a volume
	// belongs to and enumerate its co-tenants. Nil uses the default
	// sysfs-backed resolver.
	DeviceResolver atlasnvme.DeviceResolver
}

// NewDefaultGuardianConfig returns sane defaults.
func NewDefaultGuardianConfig(nodeName string) GuardianConfig {
	return GuardianConfig{
		NodeName:         nodeName,
		PollInterval:     5 * time.Minute,
		RestartBackoff:   10 * time.Minute,
		GraceSeconds:     0,
		OptInLabelKey:    "simplyblock.io/auto-restart-on-pathloss",
		OptInLabelValue:  "true",
		OptOutLabelKey:   "simplyblock.io/guardian-disable",
		OptOutLabelValue: "true",
		DryRun:           false,
		MinBrokenFor: parseDurationFromEnv(
			"GUARDIAN_MIN_BROKEN_FOR",
			30*time.Second,
		),
		BrokenLvolGracePeriod: parseDurationFromEnv(
			"GUARDIAN_BROKEN_LVOL_GRACE_PERIOD",
			defaultBrokenLvolGracePeriod,
		),
		StatePath:     "/var/run/simplyblock/guardian/state.json",
		CSIDriverName: "csi.simplyblock.io",
	}
}

type persistedLvolState struct {
	PodUIDs   []string  `json:"podUIDs,omitempty"`
	ClusterID string    `json:"clusterID,omitempty"`
	BrokenAt  time.Time `json:"brokenAt,omitempty"`
}

type guardianState struct {
	Lvols              map[string]persistedLvolState `json:"lvols"`
	LastRestart        map[string]time.Time          `json:"lastRestart,omitempty"`
	ClusterWasInactive map[string]bool               `json:"clusterWasInactive,omitempty"`
}

type LvolState struct {
	// podUID -> present
	PodUIDs map[string]struct{} `json:"-"` // persisted as []string

	// derived from NQN
	ClusterID string `json:"clusterID"`

	// zero value means "not broken"
	BrokenAt time.Time `json:"brokenAt,omitempty"`
}

// Guardian tracks which pod uses which lvol and restarts affected pods
// ONLY after cluster becomes active again.
type Guardian struct {
	cfg GuardianConfig

	// Kubernetes cache manager shared with the rest of the node plugin. It
	// serves PV/PVC reads from a watch-backed cache and transparently falls
	// back to the API, so the guardian needs no fallback of its own. Pods and
	// StorageClasses (which the manager does not cache) are read via its
	// Client.
	manager *sbkube.Manager
	cs      kubernetes.Interface

	// devices resolves a volume UUID to the local NVMe device and its
	// subsystem co-tenants, without any in-memory caching — reads are
	// against live sysfs.
	devices atlasnvme.DeviceResolver

	mu sync.Mutex

	// lvolID -> state
	lvols map[string]*LvolState

	// podUID -> last restart time
	lastRestart map[string]time.Time

	// cluster transition state
	clusterWasInactive map[string]bool
}

func (g *Guardian) loadState() {
	if g.cfg.StatePath == "" {
		return
	}

	b, err := os.ReadFile(g.cfg.StatePath)
	if err != nil {
		if os.IsNotExist(err) {
			klog.Infof("Guardian: no prior state found at %s", g.cfg.StatePath)
			return
		}
		klog.Warningf("Guardian: failed to read state file %s: %v", g.cfg.StatePath, err)
		return
	}

	var st guardianState
	if err := json.Unmarshal(b, &st); err != nil {
		klog.Warningf("Guardian: failed to unmarshal state file %s: %v", g.cfg.StatePath, err)
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if st.Lvols != nil {
		g.lvols = make(map[string]*LvolState)
		for lvolID, pls := range st.Lvols {
			set := make(map[string]struct{})
			for _, uid := range pls.PodUIDs {
				if uid == "" {
					continue
				}
				set[uid] = struct{}{}
			}
			g.lvols[lvolID] = &LvolState{
				PodUIDs:   set,
				ClusterID: pls.ClusterID,
				BrokenAt:  pls.BrokenAt,
			}
		}
	}

	if st.LastRestart != nil {
		g.lastRestart = st.LastRestart
	}

	if st.ClusterWasInactive != nil {
		g.clusterWasInactive = st.ClusterWasInactive
	}

	klog.Infof("Guardian: loaded state: lvols=%d lastRestart=%d clusterWasInactive=%d",
		len(g.lvols), len(g.lastRestart), len(g.clusterWasInactive),
	)
}

// StartGuardian starts the guardian loop in a goroutine. The cache manager is
// shared with the rest of the node plugin so the guardian reads PV/PVC state
// from memory rather than issuing a Get per PVC per pod on every poll; it falls
// back to the API transparently, and a nil manager degrades to API-only reads.
func StartGuardian(ctx context.Context, cfg GuardianConfig, manager *sbkube.Manager) (*Guardian, error) {
	if cfg.NodeName == "" {
		return nil, fmt.Errorf("guardian requires NodeName")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Minute
	}
	if cfg.RestartBackoff <= 0 {
		cfg.RestartBackoff = 10 * time.Minute
	}
	if cfg.MinBrokenFor <= 0 {
		cfg.MinBrokenFor = 30 * time.Second
	}
	if cfg.BrokenLvolGracePeriod <= 0 {
		cfg.BrokenLvolGracePeriod = 90 * time.Second
	}
	if cfg.OptInLabelKey == "" {
		cfg.OptInLabelKey = "simplyblock.io/auto-restart-on-pathloss"
	}
	if cfg.OptInLabelValue == "" {
		cfg.OptInLabelValue = "true"
	}
	if cfg.CSIDriverName == "" {
		cfg.CSIDriverName = "csi.simplyblock.io"
	}

	if manager == nil {
		return nil, fmt.Errorf("guardian requires a Kubernetes cache manager")
	}

	devices := cfg.DeviceResolver
	if devices == nil {
		devices = atlasnvme.NewSysfsDeviceResolver(atlasnvme.SysfsConfig{})
	}

	g := &Guardian{
		cfg:                cfg,
		manager:            manager,
		cs:                 manager.Client(),
		devices:            devices,
		lvols:              make(map[string]*LvolState),
		lastRestart:        make(map[string]time.Time),
		clusterWasInactive: make(map[string]bool),
	}

	klog.Infof("Guardian started node=%s poll=%s backoff=%s minBrokenFor=%s dryRun=%v",
		cfg.NodeName, cfg.PollInterval, cfg.RestartBackoff, cfg.MinBrokenFor, cfg.DryRun)

	g.loadState()

	go g.loop(ctx)
	return g, nil
}

// RegisterPublish records that a volume (identified by its per-namespace lvol
// UUID) is published to a pod via targetPath.
func (g *Guardian) RegisterPublish(clusterID, lvolID, targetPath string) {
	podUID := podUIDFromTargetPath(targetPath)
	if lvolID == "" || podUID == "" || clusterID == "" {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	st, ok := g.lvols[lvolID]
	if !ok || st == nil {
		st = &LvolState{PodUIDs: map[string]struct{}{}}
		g.lvols[lvolID] = st
	}
	if st.PodUIDs == nil {
		st.PodUIDs = map[string]struct{}{}
	}
	st.PodUIDs[podUID] = struct{}{}
	st.ClusterID = clusterID

	if _, exists := g.clusterWasInactive[clusterID]; !exists {
		g.clusterWasInactive[clusterID] = true
	}

	g.persistLocked()
}

// RegisterUnpublish removes mapping. Call from NodeUnpublishVolume.
func (g *Guardian) RegisterUnpublishByTargetPath(targetPath string) {
	podUID := podUIDFromTargetPath(targetPath)
	if podUID == "" {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	for lvolID, st := range g.lvols {
		if st == nil || st.PodUIDs == nil {
			continue
		}
		delete(st.PodUIDs, podUID)

		// If no pods remain, drop the lvol entry entirely (and its BrokenAt).
		if len(st.PodUIDs) == 0 {
			delete(g.lvols, lvolID)
		}
	}

	g.persistLocked()
}

// MarkBrokenLvol marks lvol broken at time.Now() (first time only).
// Call this when you *know* both paths are gone / device removed.
func (g *Guardian) MarkBrokenLvol(lvolID string) {
	if lvolID == "" {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	st, ok := g.lvols[lvolID]
	if !ok || st == nil {
		klog.Warningf("Guardian: MarkBrokenLvol(%s) ignored: unknown lvol (not published yet?)", lvolID)
		return
	}

	if st.ClusterID == "" {
		klog.Warningf("Guardian: MarkBrokenLvol(%s) ignored: clusterID unknown (not published yet?)", lvolID)
		return
	}

	if st.BrokenAt.IsZero() {
		st.BrokenAt = time.Now().UTC()
		klog.Warningf("Guardian marked lvol broken: cluster=%s lvol=%s", st.ClusterID, lvolID)
	}

	if _, ok := g.clusterWasInactive[st.ClusterID]; !ok {
		g.clusterWasInactive[st.ClusterID] = true
	}

	g.persistLocked()
}

func (g *Guardian) loop(ctx context.Context) {
	t := time.NewTicker(g.cfg.PollInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.Infof("Guardian stopping: %v", ctx.Err())
			return
		case <-t.C:
			g.tick(ctx)
		}
	}
}

//nolint:gocyclo // TODO: decompose tick() into smaller helpers to reduce complexity
func (g *Guardian) tick(ctx context.Context) {
	clusters, err := g.loadClusterSecret()
	if err != nil || len(clusters.Clusters) == 0 {
		return
	}

	brokenAt, podsByLvol, clusterByLvol := g.snapshotBrokenLvols()

	earliestBroken := earliestBrokenPerCluster(brokenAt, clusterByLvol)
	activeNow := g.evaluateClusterStatuses(clusters, earliestBroken)
	if len(activeNow) == 0 {
		return
	}

	actionable := g.actionableLvols(brokenAt, clusterByLvol, activeNow)
	if len(actionable) == 0 {
		return
	}

	uidToPod, err := g.podsByUID(ctx)
	if err != nil {
		klog.Errorf("Guardian: list pods failed: %v", err)
		return
	}

	restarted := 0
	for cid, lvolIDs := range actionable {
		restarted += g.restartBrokenLvols(ctx, cid, lvolIDs, podsByLvol, uidToPod)
	}

	if restarted > 0 {
		klog.Infof("Guardian: restart cycle complete. restarted=%d", restarted)
	}

	locks.ViaLock(&g.mu, g.persistLocked)
}

func (g *Guardian) loadClusterSecret() (ClustersInfo, error) {
	secretFile := FromEnv("SPDKCSI_SECRET", "/etc/spdkcsi-secret/secret.json")
	var clusters ClustersInfo
	if err := ParseJSONFile(secretFile, &clusters); err != nil {
		klog.Errorf("Guardian: parse clusters secret failed: %v", err)
		return ClustersInfo{}, err
	}
	return clusters, nil
}

// snapshotBrokenLvols copies the subset of g.lvols needed for a tick under
// the mutex and returns three maps keyed by lvolID:
//   - brokenAt:     only lvols whose BrokenAt is non-zero
//   - podsByLvol:   all pod UIDs registered against each lvol
//   - clusterByLvol: the cluster ID each lvol belongs to
func (g *Guardian) snapshotBrokenLvols() (
	brokenAt map[string]time.Time,
	podsByLvol map[string][]string,
	clusterByLvol map[string]string,
) {
	g.mu.Lock()
	defer g.mu.Unlock()

	brokenAt = make(map[string]time.Time, len(g.lvols))
	podsByLvol = make(map[string][]string, len(g.lvols))
	clusterByLvol = make(map[string]string, len(g.lvols))

	for lvolID, st := range g.lvols {
		if st == nil {
			continue
		}
		clusterByLvol[lvolID] = st.ClusterID
		if !st.BrokenAt.IsZero() {
			brokenAt[lvolID] = st.BrokenAt
		}
		for podUID := range st.PodUIDs {
			podsByLvol[lvolID] = append(podsByLvol[lvolID], podUID)
		}
	}
	return
}

// earliestBrokenPerCluster returns the earliest BrokenAt timestamp seen
// across all lvols for each cluster. Used to enforce BrokenLvolGracePeriod
// before querying cluster status.
func earliestBrokenPerCluster(brokenAt map[string]time.Time, clusterByLvol map[string]string) map[string]time.Time {
	result := make(map[string]time.Time)
	for lvolID, ts := range brokenAt {
		cid := clusterByLvol[lvolID]
		if cid == "" {
			continue
		}
		if t, ok := result[cid]; !ok || ts.Before(t) {
			result[cid] = ts
		}
	}
	return result
}

// evaluateClusterStatuses checks the live status of every cluster, honouring
// the BrokenLvolGracePeriod before making API calls. It updates
// g.clusterWasInactive to track active↔inactive transitions and returns the
// set of cluster IDs that are currently active.
func (g *Guardian) evaluateClusterStatuses(clusters ClustersInfo, earliestBroken map[string]time.Time) map[string]bool {
	var clusterWasInactive map[string]bool
	locks.ViaLock(&g.mu, func() {
		clusterWasInactive = make(map[string]bool, len(g.clusterWasInactive))
		for cid, v := range g.clusterWasInactive {
			clusterWasInactive[cid] = v
		}
	})

	activeNow := make(map[string]bool)

	for _, c := range clusters.Clusters {
		cid := c.ClusterID
		if cid == "" {
			continue
		}

		// If any lvol on this cluster broke recently, wait for the grace period
		// before checking status — the cluster may still be transitioning to suspended.
		if firstBroken, hasBroken := earliestBroken[cid]; hasBroken {
			if time.Since(firstBroken) < g.cfg.BrokenLvolGracePeriod {
				klog.Infof(
					"Guardian: cluster=%s has broken lvols detected %.0fs ago, waiting for grace period (%.0fs) before status check",
					cid, time.Since(firstBroken).Seconds(), g.cfg.BrokenLvolGracePeriod.Seconds())
				continue
			}
		}

		active, realStatus, err := g.isClusterActiveByID(cid)
		if err != nil {
			klog.Warningf("Guardian: cluster status check failed cluster=%s err=%v (treating as inactive)", cid, err)
			active = false
			realStatus = "unknown"
		}

		wasInactive := clusterWasInactive[cid]
		if !active {
			clusterWasInactive[cid] = true
			continue
		}

		activeNow[cid] = true
		if wasInactive {
			klog.Warningf("Guardian: cluster=%s transitioned to %s; will evaluate pod restarts", cid, realStatus)
		}
		clusterWasInactive[cid] = false
	}

	locks.ViaLock(&g.mu, func() {
		for cid, v := range clusterWasInactive {
			g.clusterWasInactive[cid] = v
		}
	})

	return activeNow
}

// actionableLvols returns the broken lvol IDs grouped by cluster ID, filtered
// to those that have been broken for at least MinBrokenFor and belong to a
// currently active cluster.
func (g *Guardian) actionableLvols(
	brokenAt map[string]time.Time,
	clusterByLvol map[string]string,
	activeNow map[string]bool,
) map[string][]string {
	now := time.Now().UTC()
	result := make(map[string][]string)
	for lvolID, ts := range brokenAt {
		if now.Sub(ts) < g.cfg.MinBrokenFor {
			continue
		}
		cid := clusterByLvol[lvolID]
		if cid == "" || !activeNow[cid] {
			continue
		}
		result[cid] = append(result[cid], lvolID)
	}
	return result
}

// podsByUID lists all Running pods on this node and returns them indexed by UID.
func (g *Guardian) podsByUID(ctx context.Context) (map[string]v1.Pod, error) {
	pods, err := g.listRunningPodsOnNode(ctx, g.cfg.NodeName)
	if err != nil {
		return nil, err
	}
	m := make(map[string]v1.Pod, len(pods.Items))
	for _, p := range pods.Items {
		m[string(p.UID)] = p
	}
	return m, nil
}

// subsystemLvolIDs returns all lvolIDs that share the same NVMe-oF subsystem
// as lvolID by reading live sysfs via the atlas device resolver — no cached
// state. Returns nil when the device is not yet attached (treat the same as
// "not yet indexed": suppress individual restart, retry next tick). Returns a
// single-element slice for private subsystems (individual restart is safe) and
// multiple elements for shared ones (coordinated restart required).
func (g *Guardian) subsystemLvolIDs(ctx context.Context, lvolID string) []string {
	dev, err := g.devices.ByUUID(ctx, lvolID)
	if err != nil {
		if !errors.Is(err, errs.ErrNotFound) {
			klog.Warningf("Guardian: subsystem lookup for lvol=%s failed: %v", lvolID, err)
		}
		return nil
	}
	cotenants, err := dev.CoTenants(ctx)
	if err != nil {
		klog.Warningf("Guardian: CoTenants lookup for lvol=%s failed: %v", lvolID, err)
		return nil
	}
	ids := make([]string, 0, len(cotenants)+1)
	ids = append(ids, lvolID)
	for _, ct := range cotenants {
		if ct.Namespace.UUID != "" {
			ids = append(ids, ct.Namespace.UUID)
		}
	}
	return ids
}

// restartBrokenLvols processes all actionable lvol IDs for one cluster.
// Each lvol is routed to a coordinated restart (shared NQN) or individual
// restart (single-member or not-yet-indexed NQN). Returns the number of pods
// deleted this cycle.
func (g *Guardian) restartBrokenLvols(
	ctx context.Context,
	cid string,
	lvolIDs []string,
	podsByLvol map[string][]string,
	uidToPod map[string]v1.Pod,
) int {
	klog.Warningf("Guardian: cluster %s active; attempting restarts for broken lvols=%v", cid, lvolIDs)

	restarted := 0
	// handledLvols prevents double-processing lvols already covered by a
	// coordinated subsystem restart (one coordinatedSubsystemRestart call
	// handles all sibling lvolIDs at once).
	handledLvols := make(map[string]bool)

	for _, lvolID := range lvolIDs {
		if handledLvols[lvolID] {
			continue
		}

		klog.Warningf("Guardian debug: lvol=%s podUIDs=%v", lvolID, podsByLvol[lvolID])

		// Route based on the NQN index:
		//   nil      → not yet indexed; fall back to per-pod StorageClass check
		//   len == 1 → single-member subsystem; individual restart is safe
		//   len > 1  → shared subsystem; all pods must restart together
		siblings := g.subsystemLvolIDs(ctx, lvolID)
		if len(siblings) > 1 {
			restarted += g.coordinatedSubsystemRestart(ctx, cid, siblings, podsByLvol, uidToPod)
			for _, s := range siblings {
				handledLvols[s] = true
			}
			continue
		}

		for _, podUID := range podsByLvol[lvolID] {
			pod, ok := uidToPod[podUID]
			if !ok {
				continue
			}
			if g.restartIndividualPod(ctx, cid, lvolID, podUID, pod, siblings) {
				g.setLastRestart(podUID)
				restarted++
				locks.ViaLock(&g.mu, func() { g.removePodFromLvolLocked(lvolID, podUID) })
			}
		}
	}

	return restarted
}

// isPodRestartable returns true when pod passes all shared preconditions for
// an automatic restart: controller-managed, opted in, and outside the backoff
// window. It logs the specific reason when any check fails, so callers only
// need to log group-level context on a false return.
func (g *Guardian) isPodRestartable(ctx context.Context, pod *v1.Pod, podUID string) bool {
	if !controllerManaged(pod) {
		klog.Warningf("Guardian: pod %s/%s (uid=%s) not restartable: no owner controller",
			pod.Namespace, pod.Name, podUID)
		return false
	}
	if !g.podOptedInForAutoRestart(ctx, pod) {
		klog.Warningf("Guardian: pod %s/%s (uid=%s) not restartable: not opted in for auto-restart",
			pod.Namespace, pod.Name, podUID)
		return false
	}
	if last, ok := g.getLastRestart(podUID); ok && time.Since(last) < g.cfg.RestartBackoff {
		klog.Infof("Guardian: pod %s/%s (uid=%s) not restartable: in backoff (%.0fs remaining)",
			pod.Namespace, pod.Name, podUID,
			(g.cfg.RestartBackoff - time.Since(last)).Seconds())
		return false
	}
	return true
}

// restartIndividualPod runs eligibility checks for a single pod and, if they
// pass, deletes it. Returns true only when the pod was successfully deleted
// (or was already gone), so the caller can update restart state.
// siblings is the NQN index result for this pod's lvolID (nil = not indexed,
// len 1 = single-member); callers must not pass siblings with len > 1.
func (g *Guardian) restartIndividualPod(
	ctx context.Context,
	cid, lvolID, podUID string,
	pod v1.Pod,
	siblings []string,
) bool {
	if !g.isPodRestartable(ctx, &pod, podUID) {
		return false
	}
	// NQN index not yet populated: suppress if the StorageClass indicates a
	// shared subsystem to avoid tearing down a shared NQN that siblings may
	// still be using. Once reconnectSubsystems populates the index (next tick
	// or sooner), siblings has len 1 and this guard is skipped.
	if siblings == nil && g.podUsesNamespacedSubsystem(ctx, &pod) {
		klog.Warningf("Guardian: suppressing auto-restart for pod %s/%s (uid=%s, lvol=%s): "+
			"NVMe subsystem map not yet populated; coordinated restart unavailable this tick",
			pod.Namespace, pod.Name, podUID, lvolID)
		g.emitSharedSubsystemEvent(ctx, &pod)
		return false
	}

	klog.Warningf("Guardian: restarting pod %s/%s (uid=%s) due to broken lvol=%s cluster=%s",
		pod.Namespace, pod.Name, podUID, lvolID, cid)

	if g.cfg.DryRun {
		return false
	}

	err := g.cs.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &g.cfg.GraceSeconds,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		klog.Errorf("Guardian: delete pod %s/%s failed: %v", pod.Namespace, pod.Name, err)
		return false
	}
	return true
}

// coordinatedSubsystemRestart restarts all pods that share an NVMe-oF
// subsystem simultaneously. All candidates must pass every check before any
// pod is deleted — a single failure suppresses the whole group to prevent a
// partial teardown that would disconnect the shared subsystem while other
// pods are still using it. Returns the number of pods deleted.
func (g *Guardian) coordinatedSubsystemRestart(
	ctx context.Context,
	cid string,
	siblings []string,
	lvolPods map[string][]string,
	uidToPod map[string]v1.Pod,
) int {
	type podEntry struct {
		pod    v1.Pod
		lvolID string
	}
	var candidates []podEntry
	for _, sibLvolID := range siblings {
		for _, podUID := range lvolPods[sibLvolID] {
			pod, ok := uidToPod[podUID]
			if !ok {
				continue
			}
			candidates = append(candidates, podEntry{pod: pod, lvolID: sibLvolID})
		}
	}
	if len(candidates) == 0 {
		return 0
	}

	// Gate: every candidate must pass all checks before any pod is deleted.
	// isPodRestartable logs the pod-level reason; we log the group consequence.
	for _, c := range candidates {
		pod := c.pod
		podUID := string(pod.UID)
		if !g.isPodRestartable(ctx, &pod, podUID) {
			klog.Warningf(
				"Guardian: coordinated restart suppressed (cluster=%s, lvols=%v): "+
					"pod %s/%s did not pass restartability checks",
				cid, siblings, pod.Namespace, pod.Name)
			return 0
		}
	}

	klog.Warningf(
		"Guardian: coordinated restart: deleting %d pods on shared NVMe subsystem (cluster=%s, lvols=%v)",
		len(candidates), cid, siblings)

	deleted := 0
	for _, c := range candidates {
		pod := c.pod
		podUID := string(pod.UID)
		if !g.cfg.DryRun {
			err := g.cs.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
				GracePeriodSeconds: &g.cfg.GraceSeconds,
			})
			if err != nil && !apierrors.IsNotFound(err) {
				klog.Errorf(
					"Guardian: coordinated restart: delete pod %s/%s failed: %v",
					pod.Namespace, pod.Name, err)
				continue
			}
		}
		g.setLastRestart(podUID)
		deleted++
		locks.ViaLock(&g.mu, func() { g.removePodFromLvolLocked(c.lvolID, podUID) })
	}

	if deleted > 0 {
		klog.Warningf(
			"Guardian: coordinated restart complete: deleted %d/%d pods (cluster=%s, lvols=%v)",
			deleted, len(candidates), cid, siblings)
	}
	return deleted
}

func (g *Guardian) isClusterActiveByID(clusterID string) (ok bool, realStatus string, err error) {
	client, err := NewsimplyBlockClient(context.Background(), clusterID, "")
	if err != nil {
		return false, "", err
	}

	raw, err := client.API.do(context.Background(), http.MethodGet, client.API.v2cluster(), nil)
	if err != nil {
		return false, "", err
	}

	var status ClusterStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return false, "", err
	}

	realStatus = strings.ToLower(strings.TrimSpace(status.Status))
	ok = (realStatus == "active" || realStatus == "degraded")
	return ok, realStatus, nil
}

func (g *Guardian) listRunningPodsOnNode(ctx context.Context, nodeName string) (*v1.PodList, error) {
	selector := fields.AndSelectors(
		fields.OneTermEqualSelector("spec.nodeName", nodeName),
		fields.OneTermEqualSelector("status.phase", string(v1.PodRunning)),
	).String()

	return g.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: selector})
}

func controllerManaged(pod *v1.Pod) bool {
	for _, r := range pod.OwnerReferences {
		if ptr.BoolFromOrFalse(r.Controller) {
			return true
		}
	}
	return false
}

func (g *Guardian) getLastRestart(podUID string) (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.lastRestart[podUID]
	return t, ok
}

func (g *Guardian) setLastRestart(podUID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastRestart[podUID] = time.Now()
}

// Extract pod UID from kubelet targetPath.
// Example: /var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~csi/.../mount
func podUIDFromTargetPath(p string) string {
	const marker = "/pods/"
	i := strings.Index(p, marker)
	if i < 0 {
		return ""
	}
	rest := p[i+len(marker):]
	j := strings.Index(rest, "/")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func (g *Guardian) persistLocked() {
	if g.cfg.StatePath == "" {
		return
	}

	st := guardianState{
		Lvols:              make(map[string]persistedLvolState),
		LastRestart:        make(map[string]time.Time),
		ClusterWasInactive: make(map[string]bool),
	}

	for lvolID, lvs := range g.lvols {
		if lvs == nil {
			continue
		}
		pls := persistedLvolState{
			ClusterID: lvs.ClusterID,
			BrokenAt:  lvs.BrokenAt,
		}
		for uid := range lvs.PodUIDs {
			pls.PodUIDs = append(pls.PodUIDs, uid)
		}
		st.Lvols[lvolID] = pls
	}

	for uid, t := range g.lastRestart {
		st.LastRestart[uid] = t
	}
	for cid, v := range g.clusterWasInactive {
		st.ClusterWasInactive[cid] = v
	}

	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		klog.Errorf("Guardian: marshal state: %v", err)
		return
	}

	dir := filepath.Dir(g.cfg.StatePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		klog.Errorf("Guardian: mkdir state dir %s: %v", dir, err)
		return
	}
	if err := os.WriteFile(g.cfg.StatePath, b, 0o600); err != nil {
		klog.Errorf("Guardian: write state: %v", err)
	}
}

func (g *Guardian) removePodFromLvolLocked(lvolID, podUID string) {
	st := g.lvols[lvolID]
	if st == nil || st.PodUIDs == nil {
		return
	}
	delete(st.PodUIDs, podUID)
	if len(st.PodUIDs) == 0 {
		delete(g.lvols, lvolID)
		delete(g.lastRestart, podUID)
	}
}

func hasOptInMetadata(labels map[string]string, annotations map[string]string, key, want string) bool {
	if key == "" {
		return false
	}
	if labels != nil && labels[key] == want {
		return true
	}
	if annotations != nil && annotations[key] == want {
		return true
	}
	return false
}

func storageClassOptedIn(sc *storagev1.StorageClass, key, want string) bool {
	if sc == nil {
		return false
	}
	return hasOptInMetadata(sc.Labels, sc.Annotations, key, want)
}

func (g *Guardian) podOptedInForAutoRestart(ctx context.Context, pod *v1.Pod) bool {
	if hasOptInMetadata(pod.Labels, pod.Annotations, g.cfg.OptInLabelKey, g.cfg.OptInLabelValue) {
		return true
	}

	ok, err := g.podUsesOptedInSimplyBlockStorageClass(ctx, pod)
	if err != nil {
		klog.Warningf("Guardian: failed checking StorageClass opt-in for pod %s/%s: %v",
			pod.Namespace, pod.Name, err)
		return false
	}

	return ok
}

func (g *Guardian) podUsesOptedInSimplyBlockStorageClass(ctx context.Context, pod *v1.Pod) (bool, error) {
	seenPVCs := make(map[string]struct{})
	seenSCs := make(map[string]struct{})

	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim == nil {
			continue
		}

		pvcName := strings.TrimSpace(vol.PersistentVolumeClaim.ClaimName)
		if pvcName == "" {
			continue
		}

		pvcKey := pod.Namespace + "/" + pvcName
		if _, seen := seenPVCs[pvcKey]; seen {
			continue
		}
		seenPVCs[pvcKey] = struct{}{}

		pvc, err := g.manager.PersistentVolumeClaimByNamespaceAndName(ctx, pod.Namespace, pvcName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				klog.Warningf("Guardian: PVC %s not found for pod %s/%s", pvcKey, pod.Namespace, pod.Name)
				continue
			}
			return false, fmt.Errorf("get pvc %s: %w", pvcKey, err)
		}

		if hasOptInMetadata(pvc.Labels, pvc.Annotations, g.cfg.OptInLabelKey, g.cfg.OptInLabelValue) {
			klog.Infof("Guardian: pod %s/%s opted in via PVC %s", pod.Namespace, pod.Name, pvcKey)
			return true, nil
		}

		pvName := strings.TrimSpace(pvc.Spec.VolumeName)
		if pvName == "" {
			continue
		}

		pv, err := g.manager.PersistentVolumeByName(ctx, pvName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				klog.Warningf("Guardian: PV %s not found for PVC %s", pvName, pvcKey)
				continue
			}
			return false, fmt.Errorf("get pv %s: %w", pvName, err)
		}

		if pv.Spec.CSI == nil {
			continue
		}
		if strings.TrimSpace(pv.Spec.CSI.Driver) != g.cfg.CSIDriverName {
			continue
		}

		scName := ptr.From(pvc.Spec.StorageClassName, "")
		if scName == "" {
			scName = strings.TrimSpace(pvc.Annotations["volume.beta.kubernetes.io/storage-class"])
		}
		if scName == "" {
			continue
		}

		if _, seen := seenSCs[scName]; seen {
			continue
		}
		seenSCs[scName] = struct{}{}

		sc, err := g.cs.StorageV1().StorageClasses().Get(ctx, scName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				klog.Warningf("Guardian: StorageClass %s not found for PVC %s", scName, pvcKey)
				continue
			}
			return false, fmt.Errorf("get storageclass %s: %w", scName, err)
		}

		if storageClassOptedIn(sc, g.cfg.OptInLabelKey, g.cfg.OptInLabelValue) {
			klog.Infof("Guardian: pod %s/%s opted in via Simplyblock StorageClass %s (driver=%s)",
				pod.Namespace, pod.Name, sc.Name, g.cfg.CSIDriverName)
			return true, nil
		}
	}

	return false, nil
}

// podUsesNamespacedSubsystem returns true if any of the pod's simplyblock PVCs
// use a StorageClass with max_namespace_per_subsys > 1. Such volumes share an
// NVMe subsystem with other volumes, so restarting the pod would tear down the
// shared subsystem and disrupt every other volume on it.
func (g *Guardian) podUsesNamespacedSubsystem(ctx context.Context, pod *v1.Pod) bool {
	seenPVCs := make(map[string]struct{})
	seenSCs := make(map[string]struct{})

	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim == nil {
			continue
		}
		pvcName := strings.TrimSpace(vol.PersistentVolumeClaim.ClaimName)
		if pvcName == "" {
			continue
		}
		pvcKey := pod.Namespace + "/" + pvcName
		if _, seen := seenPVCs[pvcKey]; seen {
			continue
		}
		seenPVCs[pvcKey] = struct{}{}

		pvc, err := g.manager.PersistentVolumeClaimByNamespaceAndName(ctx, pod.Namespace, pvcName)
		if err != nil {
			klog.Warningf("Guardian: podUsesNamespacedSubsystem: PVC %s: %v", pvcKey, err)
			continue
		}

		pvName := strings.TrimSpace(pvc.Spec.VolumeName)
		if pvName == "" {
			continue
		}

		pv, err := g.manager.PersistentVolumeByName(ctx, pvName)
		if err != nil {
			klog.Warningf("Guardian: podUsesNamespacedSubsystem: PV %s: %v", pvName, err)
			continue
		}
		if pv.Spec.CSI == nil || strings.TrimSpace(pv.Spec.CSI.Driver) != g.cfg.CSIDriverName {
			continue
		}

		scName := ptr.From(pvc.Spec.StorageClassName, "")
		if scName == "" {
			scName = strings.TrimSpace(pvc.Annotations["volume.beta.kubernetes.io/storage-class"])
		}
		if scName == "" {
			continue
		}
		if _, seen := seenSCs[scName]; seen {
			continue
		}
		seenSCs[scName] = struct{}{}

		sc, err := g.cs.StorageV1().StorageClasses().Get(ctx, scName, metav1.GetOptions{})
		if err != nil {
			klog.Warningf("Guardian: podUsesNamespacedSubsystem: StorageClass %s: %v", scName, err)
			continue
		}

		if v, ok := sc.Parameters["max_namespace_per_subsys"]; ok {
			n := 0
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 1 {
				return true
			}
		}
	}
	return false
}

// emitSharedSubsystemEvent records a Warning event on the pod explaining that
// auto-restart was skipped because the volume shares an NVMe subsystem.
func (g *Guardian) emitSharedSubsystemEvent(ctx context.Context, pod *v1.Pod) {
	now := metav1.Now()
	event := &v1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "guardian-shared-subsystem-",
			Namespace:    pod.Namespace,
		},
		InvolvedObject: v1.ObjectReference{
			Kind:       "Pod",
			Namespace:  pod.Namespace,
			Name:       pod.Name,
			UID:        pod.UID,
			APIVersion: "v1",
		},
		Reason:  "AutoRestartSuppressed",
		Message: "Guardian auto-restart skipped: volume shares an NVMe subsystem (max_namespace_per_subsys > 1). Restarting this pod would tear down the shared NVMe-oF paths used by other volumes on the same subsystem. Resolve the pathloss manually.", //nolint:lll // unwrappable string/log/signature
		Type:    v1.EventTypeWarning,
		Source: v1.EventSource{
			Component: "guardian",
		},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}
	if _, err := g.cs.CoreV1().Events(pod.Namespace).Create(ctx, event, metav1.CreateOptions{}); err != nil {
		klog.Warningf("Guardian: failed to emit AutoRestartSuppressed event for pod %s/%s: %v",
			pod.Namespace, pod.Name, err)
	}
}
