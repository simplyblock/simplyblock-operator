package volumemigration

import (
	"strings"
	"sync"
	"time"
)

// pendingMigrationState tracks a volume through the async migration lifecycle.
type pendingMigrationState string

const (
	// pendingStateWaitingForCompletion is set immediately after CreateMigration.
	// The reconciler polls GetMigration until CompletedAt > 0.
	pendingStateWaitingForCompletion pendingMigrationState = "waiting_for_completion"
)

type PendingMigration struct {
	State          pendingMigrationState
	MigrationStart time.Time
	// CRName / CRNamespace identify the VolumeMigration CR that drives this
	// migration. The CR controller owns the backend CreateMigration/
	// ContinueMigration/poll lifecycle; the rebalancer tracks completion via the
	// CR's status.phase.
	CRName      string
	CRNamespace string
	ClusterUUID string
	PoolUUID    string
	VolumeUUID  string
	StuckWarned bool
}

type MigrationState struct {
	mu sync.Mutex
	// coolDownMap keys: "clusterUUID/volumeUUID" → expiry time
	coolDownMap map[string]time.Time
	// pendingMigrations keys: "clusterUUID/volumeUUID"
	pendingMigrations map[string]*PendingMigration
}

func NewMigrationState() *MigrationState {
	return &MigrationState{
		coolDownMap:       make(map[string]time.Time),
		pendingMigrations: make(map[string]*PendingMigration),
	}
}

func (ms *MigrationState) PushMigration(clusterUUID, poolUUID, volumeUUID, crName, crNamespace string, coolDownSecs int64) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	key := clusterUUID + "/" + volumeUUID
	ms.coolDownMap[key] = time.Now().Add(time.Duration(coolDownSecs) * time.Second)
	ms.pendingMigrations[key] = &PendingMigration{
		State:          pendingStateWaitingForCompletion,
		MigrationStart: time.Now(),
		CRName:         crName,
		CRNamespace:    crNamespace,
		ClusterUUID:    clusterUUID,
		PoolUUID:       poolUUID,
		VolumeUUID:     volumeUUID,
	}
}

func (ms *MigrationState) GetPendingMigration(clusterUUID, volumeUUID string) (*PendingMigration, bool) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	key := clusterUUID + "/" + volumeUUID
	pendingMigration, ok := ms.pendingMigrations[key]
	return pendingMigration, ok
}

func (ms *MigrationState) GetPendingMigrationByKey(key string) (*PendingMigration, bool) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	pendingMigration, ok := ms.pendingMigrations[key]
	return pendingMigration, ok
}

func (ms *MigrationState) MarkMigrationStuck(clusterUUID, volumeUUID string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	key := clusterUUID + "/" + volumeUUID
	if _, ok := ms.pendingMigrations[key]; ok {
		ms.pendingMigrations[key].StuckWarned = true
	}
}

func (ms *MigrationState) GetPendingMigrationKeysWithPrefix(prefix string) []string {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	keys := make([]string, 0, len(ms.pendingMigrations))
	for key := range ms.pendingMigrations {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	return keys
}

func (ms *MigrationState) GetPendingMigrationKeys() []string {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	keys := make([]string, 0, len(ms.pendingMigrations))
	for key := range ms.pendingMigrations {
		keys = append(keys, key)
	}
	return keys
}

func (ms *MigrationState) HasPendingMigrationForCluster(clusterUUID string) bool {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	prefix := clusterUUID + "/"
	for k := range ms.pendingMigrations {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

func (ms *MigrationState) GetCooldownCountByCluster(clusterUUID string, now time.Time) int {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	prefix := clusterUUID + "/"
	count := 0
	for k, expiry := range ms.coolDownMap {
		if strings.HasPrefix(k, prefix) && now.Before(expiry) {
			count++
		}
	}
	return count
}

func (ms *MigrationState) DeletePendingMigration(clusterUUID, volumeUUID string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	key := clusterUUID + "/" + volumeUUID
	delete(ms.pendingMigrations, key)
}

func (ms *MigrationState) IsVolumeCooledDown(clusterUUID string, volumeUUID string, before time.Time) bool {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	key := clusterUUID + "/" + volumeUUID
	if expiry, ok := ms.coolDownMap[key]; ok && before.Before(expiry) {
		return true
	}
	return false
}
