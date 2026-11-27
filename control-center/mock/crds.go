package main

// The operator's CRDs, served as apiextensions.k8s.io/v1 objects so the
// console can read field descriptions, enums, and immutability rules from the
// same files the operator installs (operator/config/crd/bases). The mock does
// not validate objects against them; it only serves them.
//
// The directory is polled rather than loaded once, so a `make manifests` in
// the operator reaches a running mock: a changed file updates its CRD, a new
// one adds it, and a removed one deletes it, each with the watch event a real
// API server sends. Polling rather than file-system notification, because
// notifications do not reach a process through a Docker Desktop bind mount,
// and a stat of a few dozen files every couple of seconds costs nothing.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

const apiextGroup = "apiextensions.k8s.io"

// crdSync keeps the CRDs in one or more stores equal to the YAML files in a
// directory. Every world of a fleet serves the same CRDs.
type crdSync struct {
	dir    string
	stores []*Store
	// seen is the modification time and size each file had when it was last
	// read, and owns is the CRD name that file produced, so a removed file
	// says which CRD to delete.
	seen map[string]fileStamp
	owns map[string]string
}

type fileStamp struct {
	mod  time.Time
	size int64
}

func newCRDSync(dir string, stores ...*Store) *crdSync {
	return &crdSync{dir: dir, stores: stores, seen: map[string]fileStamp{}, owns: map[string]string{}}
}

// pass brings the stores in line with the directory and reports how many CRDs
// it added, updated, or deleted. A file that does not parse is skipped and its
// CRD keeps the last version that did: an editor or a generator caught
// mid-write is the usual cause, and the next pass reads the file again. Its
// error is returned after every other file has been handled.
func (c *crdSync) pass() (int, error) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return 0, err
	}
	changed := 0
	var errs []error
	present := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		present[e.Name()] = true
		info, err := e.Info()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		stamp := fileStamp{mod: info.ModTime(), size: info.Size()}
		if old, ok := c.seen[e.Name()]; ok && old == stamp {
			continue
		}
		n, err := c.load(e.Name())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		c.seen[e.Name()] = stamp
		changed += n
	}
	for file, name := range c.owns {
		if present[file] {
			continue
		}
		delete(c.owns, file)
		delete(c.seen, file)
		c.remove(name)
		changed++
	}
	return changed, errors.Join(errs...)
}

// load reads one file and upserts its CRD into every store. A file holding
// something other than a CRD is remembered as read and serves nothing.
func (c *crdSync) load(file string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(c.dir, file))
	if err != nil {
		return 0, err
	}
	var obj map[string]any
	if err := yaml.Unmarshal(raw, &obj); err != nil {
		return 0, fmt.Errorf("%s: %w", file, err)
	}
	if getStr(obj, "kind") != "CustomResourceDefinition" {
		return 0, nil
	}
	name := getStr(obj, "metadata.name")
	if name == "" {
		return 0, fmt.Errorf("%s: the CRD has no metadata.name", file)
	}
	// A file that now holds a different CRD than it did drops the old one.
	if old, ok := c.owns[file]; ok && old != name {
		c.remove(old)
	}
	for _, st := range c.stores {
		d, ok := st.Lookup(apiextGroup, "v1", "customresourcedefinitions")
		if !ok {
			return 0, errors.New("customresourcedefinitions are not registered")
		}
		var apiErr *apiError
		if _, exists := st.Get(d, "", name); exists == nil {
			_, apiErr = st.Update(d, "", obj)
		} else {
			_, apiErr = st.Create(d, "", obj)
		}
		if apiErr != nil {
			return 0, fmt.Errorf("%s: %s", file, apiErr.Message)
		}
	}
	c.owns[file] = name
	return 1, nil
}

func (c *crdSync) remove(name string) {
	for _, st := range c.stores {
		if d, ok := st.Lookup(apiextGroup, "v1", "customresourcedefinitions"); ok {
			_, _ = st.Delete(d, "", name)
		}
	}
}

// run passes over the directory every interval until ctx ends, logging what
// changed and what did not parse.
func (c *crdSync) run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := c.pass()
			if n > 0 {
				log.Printf("CRDs reloaded from %s: %d changed", c.dir, n)
			}
			if err != nil {
				log.Printf("CRDs in %s not reloaded: %v", c.dir, err)
			}
		}
	}
}

// crdSyncFor is the sync over every world of the server.
func (s *server) crdSyncFor(dir string) *crdSync {
	stores := make([]*Store, 0, len(s.order))
	for _, name := range s.order {
		stores = append(stores, s.worlds[name].store)
	}
	return newCRDSync(dir, stores...)
}
