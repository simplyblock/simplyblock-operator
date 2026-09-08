package cpinformer

import "strings"

// Scope identifies which watch a stream belongs to: the ordered tuple of path
// parameters the control-plane endpoint is scoped to. It mirrors the server's
// per-model watch_scope() (design doc §3.5):
//
//	Volume / Snapshot        Scope{clusterID, poolID}   // one stream per pool
//	Pool / StorageNode / Task Scope{clusterID}          // one stream per cluster
//	Cluster                  Scope{}                     // one stream total
type Scope []string

// Key returns a stable, comparable string form of the scope, usable as a map
// key. The empty scope (cluster-list / root) has the empty-string key.
func (s Scope) Key() string {
	return strings.Join(s, "/")
}

// ParseScope is the inverse of [Scope.Key].
func ParseScope(key string) Scope {
	if key == "" {
		return Scope{}
	}
	return Scope(strings.Split(key, "/"))
}
