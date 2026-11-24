// Tests of the CRD directory sync: a regenerated CRD file reaches the store,
// and its watchers, without restarting the mock.

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func crdFile(name, description string) string {
	return `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: ` + name + `
spec:
  group: storage.simplyblock.io
  names:
    kind: Widget
    plural: widgets
  scope: Namespaced
  versions:
  - name: v1alpha2
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        description: ` + description + `
        type: object
`
}

// write puts content into dir/file and moves its modification time forward by
// step, so two writes within one clock tick still read as a change.
func write(t *testing.T, dir, file, content string, step int) {
	t.Helper()
	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(time.Duration(step) * time.Second)
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

func crdDef(t *testing.T, st *Store) ResourceDef {
	t.Helper()
	d, ok := st.Lookup(apiextGroup, "v1", "customresourcedefinitions")
	if !ok {
		t.Fatal("customresourcedefinitions are not registered")
	}
	return d
}

func description(t *testing.T, st *Store, name string) string {
	t.Helper()
	obj, err := st.Get(crdDef(t, st), "", name)
	if err != nil {
		return "<absent>"
	}
	// getPath walks maps only, so the version list is stepped into here
	versions, _ := getPath(obj, "spec.versions").([]any)
	if len(versions) == 0 {
		return "<no version>"
	}
	return getStr(versions[0], "schema.openAPIV3Schema.description")
}

// next reads the next watch event, or fails when none arrives.
func next(t *testing.T, events <-chan WatchEvent) WatchEvent {
	t.Helper()
	select {
	case ev := <-events:
		return ev
	case <-time.After(time.Second):
		t.Fatal("no watch event")
		return WatchEvent{}
	}
}

func TestCRDSyncFollowsTheDirectory(t *testing.T) {
	dir := t.TempDir()
	st := newTestStore()
	events, stop := st.Watch(crdDef(t, st), "")
	defer stop()
	sync := newCRDSync(dir, st)

	write(t, dir, "storage.simplyblock.io_widgets.yaml", crdFile("widgets.storage.simplyblock.io", "first"), 1)
	if _, err := sync.pass(); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if got := description(t, st, "widgets.storage.simplyblock.io"); got != "first" {
		t.Fatalf("after the first pass the CRD reads %q, want first", got)
	}
	if ev := next(t, events); ev.Type != "ADDED" {
		t.Fatalf("first pass sent %s, want ADDED", ev.Type)
	}

	write(t, dir, "storage.simplyblock.io_widgets.yaml", crdFile("widgets.storage.simplyblock.io", "second"), 2)
	if _, err := sync.pass(); err != nil {
		t.Fatalf("pass after an edit: %v", err)
	}
	if got := description(t, st, "widgets.storage.simplyblock.io"); got != "second" {
		t.Fatalf("after an edit the CRD reads %q, want second", got)
	}
	if ev := next(t, events); ev.Type != "MODIFIED" {
		t.Fatalf("an edit sent %s, want MODIFIED", ev.Type)
	}

	if err := os.Remove(filepath.Join(dir, "storage.simplyblock.io_widgets.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := sync.pass(); err != nil {
		t.Fatalf("pass after a removal: %v", err)
	}
	if got := description(t, st, "widgets.storage.simplyblock.io"); got != "<absent>" {
		t.Fatalf("after a removal the CRD reads %q, want it gone", got)
	}
	if ev := next(t, events); ev.Type != "DELETED" {
		t.Fatalf("a removal sent %s, want DELETED", ev.Type)
	}
}

func TestCRDSyncLeavesAnUnchangedFileAlone(t *testing.T) {
	dir := t.TempDir()
	st := newTestStore()
	sync := newCRDSync(dir, st)
	write(t, dir, "a.yaml", crdFile("widgets.storage.simplyblock.io", "first"), 1)
	if _, err := sync.pass(); err != nil {
		t.Fatal(err)
	}
	before, _ := st.Get(crdDef(t, st), "", "widgets.storage.simplyblock.io")

	changed, err := sync.pass()
	if err != nil {
		t.Fatal(err)
	}
	after, _ := st.Get(crdDef(t, st), "", "widgets.storage.simplyblock.io")
	if changed != 0 || getStr(before, "metadata.resourceVersion") != getStr(after, "metadata.resourceVersion") {
		t.Fatalf("a pass over an unchanged file changed %d CRD(s), resourceVersion %s -> %s",
			changed, getStr(before, "metadata.resourceVersion"), getStr(after, "metadata.resourceVersion"))
	}
}

// A file caught mid-write does not parse. The CRD keeps its last good version,
// the other files are still read, and the next pass picks the file up again.
func TestCRDSyncKeepsTheLastGoodVersionOfAFileThatDoesNotParse(t *testing.T) {
	dir := t.TempDir()
	st := newTestStore()
	sync := newCRDSync(dir, st)
	write(t, dir, "a.yaml", crdFile("widgets.storage.simplyblock.io", "good"), 1)
	if _, err := sync.pass(); err != nil {
		t.Fatal(err)
	}

	write(t, dir, "a.yaml", "kind: CustomResourceDefinition\nmetadata: [unterminated", 2)
	write(t, dir, "b.yaml", crdFile("gadgets.storage.simplyblock.io", "other"), 2)
	if _, err := sync.pass(); err == nil {
		t.Fatal("a pass over a file that does not parse reported no error")
	}
	if got := description(t, st, "widgets.storage.simplyblock.io"); got != "good" {
		t.Fatalf("the CRD of a file that does not parse reads %q, want its last good version", got)
	}
	if got := description(t, st, "gadgets.storage.simplyblock.io"); got != "other" {
		t.Fatalf("a good file next to a broken one reads %q, want other", got)
	}

	write(t, dir, "a.yaml", crdFile("widgets.storage.simplyblock.io", "fixed"), 3)
	if _, err := sync.pass(); err != nil {
		t.Fatal(err)
	}
	if got := description(t, st, "widgets.storage.simplyblock.io"); got != "fixed" {
		t.Fatalf("after the file was fixed the CRD reads %q, want fixed", got)
	}
}

// Every world of a fleet serves the same CRDs.
func TestCRDSyncUpdatesEveryStore(t *testing.T) {
	dir := t.TempDir()
	a, b := newTestStore(), newTestStore()
	sync := newCRDSync(dir, a, b)
	write(t, dir, "a.yaml", crdFile("widgets.storage.simplyblock.io", "first"), 1)
	if _, err := sync.pass(); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "a.yaml", crdFile("widgets.storage.simplyblock.io", "second"), 2)
	if _, err := sync.pass(); err != nil {
		t.Fatal(err)
	}
	for i, st := range []*Store{a, b} {
		if got := description(t, st, "widgets.storage.simplyblock.io"); got != "second" {
			t.Fatalf("store %d reads %q, want second", i, got)
		}
	}
}
