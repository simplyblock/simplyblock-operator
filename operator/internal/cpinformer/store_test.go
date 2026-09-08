package cpinformer

import (
	"sort"
	"testing"
)

type item struct {
	ID   string
	Data string
}

func newItemStore() *Store[item] {
	return NewStore(func(i item) string { return i.ID })
}

func TestStoreUpsertGetList(t *testing.T) {
	s := newItemStore()
	scope := Scope{"c1", "p1"}

	s.Upsert(scope, item{ID: "a", Data: "1"})
	s.Upsert(scope, item{ID: "b", Data: "2"})
	s.Upsert(scope, item{ID: "a", Data: "1b"}) // replace

	if v, ok := s.Get(scope, "a"); !ok || v.Data != "1b" {
		t.Errorf("Get(a) = %+v, %v; want Data=1b", v, ok)
	}
	if _, ok := s.Get(scope, "missing"); ok {
		t.Error("Get(missing) returned ok")
	}
	// A different scope is isolated.
	if _, ok := s.Get(Scope{"c1", "p2"}, "a"); ok {
		t.Error("scope isolation broken")
	}
	if got := len(s.List(scope)); got != 2 {
		t.Errorf("List len = %d, want 2", got)
	}
}

func TestStoreRemove(t *testing.T) {
	s := newItemStore()
	scope := Scope{"c1"}
	s.Upsert(scope, item{ID: "a"})

	if !s.Remove(scope, "a") {
		t.Error("remove(a) = false, want true")
	}
	if s.Remove(scope, "a") {
		t.Error("remove(a) again = true, want false")
	}
}

func TestStoreReplaceDeltas(t *testing.T) {
	s := newItemStore()
	scope := Scope{"c1", "p1"}
	s.Upsert(scope, item{ID: "a"})
	s.Upsert(scope, item{ID: "b"})
	s.Upsert(scope, item{ID: "c"})

	// Snapshot drops "b", keeps "a", adds "d".
	present, removed := s.Replace(scope, []item{{ID: "a"}, {ID: "c"}, {ID: "d"}})

	sort.Strings(present)
	sort.Strings(removed)
	if got, want := present, []string{"a", "c", "d"}; !equal(got, want) {
		t.Errorf("present = %v, want %v", got, want)
	}
	if got, want := removed, []string{"b"}; !equal(got, want) {
		t.Errorf("removed = %v, want %v", got, want)
	}
	if _, ok := s.Get(scope, "b"); ok {
		t.Error("b should be gone after replace")
	}
	if _, ok := s.Get(scope, "d"); !ok {
		t.Error("d should be present after replace")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
