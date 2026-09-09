package main

import (
	"testing"
	"time"
)

func testDef() ResourceDef {
	return def(sbGroup, "v1alpha1", "storagenodeops", "StorageNodeOps", true)
}

func newTestStore() *Store {
	st := NewStore(42)
	st.Register(allResourceDefs()...)
	return st
}

func TestCreateGetUpdateDelete(t *testing.T) {
	st := newTestStore()
	d := testDef()

	created, err := st.Create(d, "sb", map[string]any{
		"metadata": map[string]any{"name": "op-1"},
		"spec":     map[string]any{"action": "restart"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if getStr(created, "metadata.uid") == "" || getStr(created, "metadata.resourceVersion") == "" {
		t.Fatalf("create did not fill identity fields: %v", created["metadata"])
	}
	if getStr(created, "apiVersion") != "storage.simplyblock.io/v1alpha1" {
		t.Fatalf("wrong apiVersion %q", getStr(created, "apiVersion"))
	}

	if _, err := st.Create(d, "sb", map[string]any{"metadata": map[string]any{"name": "op-1"}}); err == nil || err.Code != 409 {
		t.Fatalf("duplicate create should 409, got %v", err)
	}

	got, err := st.Get(d, "sb", "op-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// mutations of the returned copy must not leak into the store
	setPath(got, "spec.action", "shutdown")
	again, _ := st.Get(d, "sb", "op-1")
	if getStr(again, "spec.action") != "restart" {
		t.Fatal("store copy was mutated through a returned object")
	}

	setPath(got, "spec.action", "suspend")
	updated, err := st.Update(d, "sb", got)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if gen := updated["metadata"].(map[string]any)["generation"].(float64); gen != 2 {
		t.Fatalf("spec change should bump generation to 2, got %v", gen)
	}
	// status-only change must not bump generation
	setPath(updated, "status.phase", "Running")
	updated2, _ := st.Update(d, "sb", updated)
	if gen := updated2["metadata"].(map[string]any)["generation"].(float64); gen != 2 {
		t.Fatalf("status change must not bump generation, got %v", gen)
	}

	if _, err := st.Delete(d, "sb", "op-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.Get(d, "sb", "op-1"); err == nil || err.Code != 404 {
		t.Fatalf("get after delete should 404, got %v", err)
	}
}

func TestListSelectors(t *testing.T) {
	st := newTestStore()
	d, _ := st.Lookup("", "v1", "pods")
	mk := func(ns, name string, labels map[string]any) {
		if _, err := st.Create(d, ns, map[string]any{
			"metadata": map[string]any{"name": name, "labels": labels},
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("a", "p1", map[string]any{"app": "db", "tier": "prod"})
	mk("a", "p2", map[string]any{"app": "web"})
	mk("b", "p3", map[string]any{"app": "db"})

	cases := []struct {
		ns, label, field string
		want             int
	}{
		{"", "", "", 3},
		{"a", "", "", 2},
		{"", "app=db", "", 2},
		{"", "app==db", "", 2},
		{"", "app!=db", "", 1},
		{"", "app in (db, web)", "", 3},
		{"", "app notin (web)", "", 2},
		{"", "tier", "", 1},
		{"", "!tier", "", 2},
		{"", "app=db,tier=prod", "", 1},
		{"", "", "metadata.name=p3", 1},
		{"", "", "metadata.namespace!=a", 1},
	}
	for _, c := range cases {
		got, _ := st.List(d, c.ns, c.label, c.field)
		if len(got) != c.want {
			t.Errorf("ns=%q label=%q field=%q: want %d got %d", c.ns, c.label, c.field, c.want, len(got))
		}
	}
}

func TestWatch(t *testing.T) {
	st := newTestStore()
	d := testDef()
	ch, cancel := st.Watch(d, "sb")
	defer cancel()

	st.Create(d, "sb", map[string]any{"metadata": map[string]any{"name": "w-1"}})
	st.Create(d, "other", map[string]any{"metadata": map[string]any{"name": "w-2"}}) // filtered out
	st.Delete(d, "sb", "w-1")

	want := []string{"ADDED", "DELETED"}
	for i, typ := range want {
		select {
		case ev := <-ch:
			if ev.Type != typ || getStr(ev.Object, "metadata.name") != "w-1" {
				t.Fatalf("event %d: want %s w-1, got %s %s", i, typ, ev.Type, getStr(ev.Object, "metadata.name"))
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s", typ)
		}
	}
}

func TestMergePatch(t *testing.T) {
	target := map[string]any{
		"spec": map[string]any{"a": "1", "b": "2"},
	}
	patch := map[string]any{
		"spec": map[string]any{"b": nil, "c": "3"},
	}
	got := mergePatch(target, patch).(map[string]any)
	spec := got["spec"].(map[string]any)
	if spec["a"] != "1" || spec["c"] != "3" {
		t.Fatalf("merge lost fields: %v", spec)
	}
	if _, ok := spec["b"]; ok {
		t.Fatalf("null should delete key b: %v", spec)
	}
	// target untouched
	if _, ok := target["spec"].(map[string]any)["b"]; !ok {
		t.Fatal("mergePatch mutated its input")
	}
}
