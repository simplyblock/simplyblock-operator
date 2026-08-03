package cpinformer

import "sync"

// Lister is the read side reconcilers use in place of the control-plane HTTP
// client. Reads are served from memory; there is no network round-trip.
//
// It is deliberately not linearizable (design doc §6.2): a value may lag a very
// recent mutation until the corresponding SSE event lands. Callers that just
// mutated the control plane should seed the Store from the mutation response.
type Lister[T any] interface {
	// Get returns the object with the given id in the given scope.
	Get(scope Scope, id string) (T, bool)
	// List returns every object currently cached for the given scope.
	List(scope Scope) []T
	// Find searches every scope for the object with the given id, returning the
	// scope it was found in. It suits reconcilers that receive an id (a globally
	// unique control-plane uuid) without its scope.
	Find(id string) (Scope, T, bool)
}

// Store is a thread-safe cache of control-plane objects, partitioned by [Scope]
// and keyed within a scope by control-plane id. It satisfies [Lister].
type Store[T any] struct {
	idOf  func(T) string
	mu    sync.RWMutex
	items map[string]map[string]T // scopeKey -> id -> object
}

// NewStore returns an empty Store. idOf extracts an object's control-plane id
// (the within-scope key); it must be stable for a given object.
func NewStore[T any](idOf func(T) string) *Store[T] {
	return &Store[T]{idOf: idOf, items: map[string]map[string]T{}}
}

// Get implements [Lister].
func (s *Store[T]) Get(scope Scope, id string) (T, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.items[scope.Key()]; ok {
		v, ok := m[id]
		return v, ok
	}
	var zero T
	return zero, false
}

// List implements [Lister]. The returned slice is a fresh copy; callers may
// retain it.
func (s *Store[T]) List(scope Scope) []T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := s.items[scope.Key()]
	out := make([]T, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// Find implements [Lister]. It scans scopes; control-plane ids are globally
// unique, so at most one scope holds a given id.
func (s *Store[T]) Find(id string) (Scope, T, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for key, m := range s.items {
		if v, ok := m[id]; ok {
			return ParseScope(key), v, true
		}
	}
	var zero T
	return nil, zero, false
}

// Upsert inserts or replaces one object in a scope.
func (s *Store[T]) Upsert(scope Scope, obj T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := scope.Key()
	m := s.items[key]
	if m == nil {
		m = map[string]T{}
		s.items[key] = m
	}
	m[s.idOf(obj)] = obj
}

// Remove deletes one object from a scope; it reports whether the object was
// present.
func (s *Store[T]) Remove(scope Scope, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.items[scope.Key()]
	if _, ok := m[id]; !ok {
		return false
	}
	delete(m, id)
	return true
}

// Replace swaps a scope's contents for objs and reports the delta: the ids now
// present, and the ids that were removed (present before, absent now). This is
// the snapshot relist with delete-detection (design doc §6.3).
func (s *Store[T]) Replace(scope Scope, objs []T) (present, removed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := scope.Key()
	old := s.items[key]

	next := make(map[string]T, len(objs))
	for _, o := range objs {
		next[s.idOf(o)] = o
	}
	s.items[key] = next

	present = make([]string, 0, len(next))
	for id := range next {
		present = append(present, id)
	}
	for id := range old {
		if _, ok := next[id]; !ok {
			removed = append(removed, id)
		}
	}
	return present, removed
}
