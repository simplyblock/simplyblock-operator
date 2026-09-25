// A captured machine's sysfs, decoded and replayed onto a temporary tree.
//
// The capture is written by hack/inventory/capture-host.py: one JSON document
// of four maps -- directories, file contents, symlink targets, and the udev
// links under /dev/disk -- with every path relative to /sys, plus the few files
// read from the host's own root filesystem and the machine name uname gives.
// This package is the reading half, so that a transcript taken off a real
// machine can be handed to a Config as its SysfsRoot and read by the ordinary
// readers.
//
// It lives here, outside a _test.go file, because two modules replay the same
// captures: this package's own tests, which check a reading against what a host
// exports, and the operator's discovery fixture generator, which turns a
// captured fleet into a deployment document. A second decoder would be a second
// definition of the format, and the format is defined by a script in neither
// module.
package transcript

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Host is one machine's captured sysfs.
type Host struct {
	Dirs  []string          `json:"dirs"`
	Files map[string]string `json:"files"`
	Links map[string]string `json:"links"`

	// DevLinks are the udev links under /dev/disk, keyed relative to /dev
	// rather than to /sys. They are a section of their own because /sys/dev is
	// itself a directory, so the two trees cannot share one namespace safely.
	//
	// A transcript captured before the section existed has none, which decodes
	// to nil and materializes as a host whose udev made no links.
	DevLinks map[string]string `json:"devlinks"`

	// Root are files captured from the host's own root filesystem rather than
	// from sysfs, keyed relative to it: etc/os-release and usr/lib/os-release,
	// which are what the OS reading takes. They are a section of their own for
	// the reason DevLinks is: a path relative to the host's filesystem and one
	// relative to /sys are two namespaces, and a capture that merged them would
	// have to guess which a path belonged to.
	//
	// A transcript captured before the section existed has none, and
	// materializes as a host whose distribution cannot be read, which is what
	// such a machine's capture could in fact answer.
	Root map[string]string `json:"root"`

	// Machine is what uname called the hardware: `x86_64`, `aarch64`. It is a
	// string on the transcript rather than a file in it, because it comes from
	// a system call and there is no tree to replay it from.
	//
	// It is empty on a transcript captured before it was recorded, and a caller
	// replaying one has to state the architecture itself.
	Machine string `json:"machine"`
}

// Load reads a capture from a file, gzipped or not.
func Load(path string) (Host, error) {
	handle, err := os.Open(path) //nolint:gosec // a fixture path the caller names
	if err != nil {
		return Host{}, fmt.Errorf("open the transcript %s: %w", path, err)
	}
	defer func() { _ = handle.Close() }()

	host, err := Decode(handle, strings.HasSuffix(path, ".gz"))
	if err != nil {
		return Host{}, fmt.Errorf("%s: %w", path, err)
	}
	return host, nil
}

// Decode reads a capture from a reader, decompressing it first when compressed.
func Decode(reader io.Reader, compressed bool) (Host, error) {
	if compressed {
		unzipped, err := gzip.NewReader(reader)
		if err != nil {
			return Host{}, fmt.Errorf("decompress the transcript: %w", err)
		}
		defer func() { _ = unzipped.Close() }()
		reader = unzipped
	}

	var host Host
	if err := json.NewDecoder(reader).Decode(&host); err != nil {
		return Host{}, fmt.Errorf("decode the transcript: %w", err)
	}
	return host, nil
}

// Materialize writes the transcript under root, which the caller owns and which
// is what a Config takes as its SysfsRoot. DevRootOf(root) is the device-node
// directory of the same tree, so one materialized host serves a reader pointed
// at either.
//
// A symlink whose target cannot be created is written anyway: sysfs is full of
// links pointing outside the captured subtree, and a reader that follows one
// gets the same "not found" it would get on a host where the target is a device
// that has since gone. Failing here instead would make a capture's usefulness
// depend on how much of the machine was walked.
func (h Host) Materialize(root string) error {
	for _, dir := range h.Dirs {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	for path, content := range h.Files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("create the parent of %s: %w", path, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil { //nolint:gosec // a fixture tree
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	for path, target := range h.Links {
		if err := link(filepath.Join(root, path), target); err != nil {
			return err
		}
	}
	for path, target := range h.DevLinks {
		if err := link(filepath.Join(DevRootOf(root), path), target); err != nil {
			return err
		}
	}
	for path, content := range h.Root {
		full := filepath.Join(HostRootOf(root), path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("create the parent of %s: %w", path, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil { //nolint:gosec // a fixture tree
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// link creates one symlink, making its parent first and tolerating a target
// that is not there. See Materialize.
func link(full, target string) error {
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("create the parent of %s: %w", full, err)
	}
	_ = os.Symlink(target, full)
	return nil
}

// DevRootOf is the device-node directory of a materialized host, which is what
// a reader takes as DevRoot. It is a directory that may not exist: a transcript
// with no udev links creates nothing, and a reader listing it gets the same
// "not found" it would get on a host where udev made none.
func DevRootOf(root string) string {
	return filepath.Join(root, "dev")
}

// HostRootOf is the host root of a materialized host, which is what a reader
// takes as HostRoot. Like DevRootOf, it is a directory that may not exist: a
// transcript that captured no root files creates nothing, and the OS reading
// then reports that the host root carries no os-release.
func HostRootOf(root string) string {
	return filepath.Join(root, "root")
}

// WithClassBlock returns the transcript with a class/block tree derived from
// the block/ links it already carries.
//
// It exists because the block-device scan reads class/block and the captures
// taken before this was noticed seed block/ instead. The two are the same
// symlink farm into devices/ — the kernel exposes every whole disk under both —
// so the entries can be derived rather than re-captured, which a transcript
// costs a machine to retake. Partitions are recovered from the device tree,
// where the kernel keeps them as children of the disk.
//
// A transcript that already carries class/block is returned unchanged: a
// capture taken by a walker that seeds it needs nothing derived, and deriving
// anyway would put this function's idea of the tree over the machine's.
func (h Host) WithClassBlock() Host {
	const (
		blockDir = "block/"
		classDir = "class/block/"
	)
	for captured := range h.Links {
		if strings.HasPrefix(captured, classDir) {
			return h
		}
	}

	links := make(map[string]string, len(h.Links))
	for captured, target := range h.Links {
		links[captured] = target
	}

	for captured, target := range h.Links {
		name, isDisk := strings.CutPrefix(captured, blockDir)
		if !isDisk || strings.Contains(name, "/") {
			continue
		}
		// One level deeper than block/, so one more step back to the root.
		links[classDir+name] = "../" + target

		// The kernel lists a disk's partitions beside it, as entries of
		// class/block in their own right. They are children of the disk in the
		// device tree, which is where the capture has them.
		device := strings.TrimPrefix(path.Clean(target), "../")
		for _, dir := range h.Dirs {
			part, isChild := strings.CutPrefix(dir, device+"/")
			if !isChild || strings.Contains(part, "/") || !strings.HasPrefix(part, name) {
				continue
			}
			links[classDir+part] = "../" + target + "/" + part
		}
	}

	return Host{Dirs: h.Dirs, Files: h.Files, Links: links, DevLinks: h.DevLinks}
}
