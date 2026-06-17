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

type pendingMigration struct {
	State          pendingMigrationState
	MigrationStart time.Time
	MigrationID    string // ID returned by CreateMigration
	ClusterUUID    string
	PoolUUID       string
	VolumeUUID     string
	StuckWarned    bool
}

type MigrationState struct {
	mu sync.Mutex
	// coolDownMap keys: "clusterUUID/volumeUUID" → expiry time
	coolDownMap map[string]time.Time
	// pendingMigrations keys: "clusterUUID/volumeUUID"
	pendingMigrations map[string]*pendingMigration
}

func NewMigrationState() *MigrationState {
	return &MigrationState{
		coolDownMap:       make(map[string]time.Time),
		pendingMigrations: make(map[string]*pendingMigration),
	}
}

func (ms *MigrationState) PushMigration(clusterUUID, poolUUID, volumeUUID, migrationId string, coolDownSecs int64) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	key := clusterUUID + "/" + volumeUUID
	ms.coolDownMap[key] = time.Now().Add(time.Duration(coolDownSecs) * time.Second)
	ms.pendingMigrations[key] = &pendingMigration{
		State:          pendingStateWaitingForCompletion,
		MigrationStart: time.Now(),
		MigrationID:    migrationId,
		ClusterUUID:    clusterUUID,
		PoolUUID:       poolUUID,
		VolumeUUID:     volumeUUID,
	}
}

func (ms *MigrationState) GetPendingMigration(clusterUUID, volumeUUID string) (*pendingMigration, bool) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	key := clusterUUID + "/" + volumeUUID
	pendingMigration, ok := ms.pendingMigrations[key]
	return pendingMigration, ok
}

func (ms *MigrationState) GetPendingMigrationByKey(key string) (*pendingMigration, bool) {
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
