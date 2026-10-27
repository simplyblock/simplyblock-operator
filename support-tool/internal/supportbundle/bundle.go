// Reading a support bundle: the .tar.gz that kubectl-simplyblock-bundle writes,
// into the operator's own types.
//
// The bundle is read with the operator's types rather than with a copy of them,
// so a field the operator adds is a field this tool sees. Probe reports are the
// one exception to taking the operator's reader as well: the operator refuses
// any report version but its own, and a support agent reads bundles from
// clusters running older releases. Reports of every version in
// SupportedReportVersions are decoded into the current type, which is a superset
// of the older ones.

package supportbundle

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// SupportedReportVersions are the probe report versions this tool reads.
var SupportedReportVersions = []int{9, 10, 11}

// MaxEntryBytes is the largest file a bundle may hold. A bundle is mostly YAML
// and logs, and a limit keeps a hostile or broken archive from exhausting
// memory.
var MaxEntryBytes int64 = 256 << 20

// MaxTotalBytes is the most a bundle may expand to, and MaxEntries the most
// files it may hold. Each entry under MaxEntryBytes is not enough: a gzip of
// many highly compressible files stays small on the wire and expands past any
// machine's memory.
var (
	MaxTotalBytes int64 = 1 << 30
	MaxEntries          = 20000
)

// Bundle is one support bundle, as far as it could be read.
type Bundle struct {
	// Name is the archive's top directory, simplyblock-bundle-<run>-<time>.
	Name string

	Run             *simplyblockv1alpha2.OperatorOps
	Reports         []Report
	Nodes           []corev1.Node
	CDC             *simplyblockv1alpha2.ClusterDeploymentConfig
	ControlPlanes   []simplyblockv1alpha2.ControlPlane
	StorageClusters []simplyblockv1alpha2.StorageCluster
	StorageNodes    []simplyblockv1alpha2.StorageNode

	// Errors is the bundle's errors.txt: what the collection could not read.
	Errors string
	// Events is the namespace's events, as kubectl printed them.
	Events string
	// KubectlVersion is kubectl's and the API server's version.
	KubectlVersion string

	// Problems are the parts of the bundle this tool could not read.
	Problems []string
}

// Report is one probe report of the bundle.
type Report struct {
	// File is the report's name in the bundle.
	File string
	// Version is the report's schema version.
	Version int
	// Report is the report, decoded into the current schema. A field a version
	// predates is empty.
	Report nodeprobe.Report
	// Err says why the report could not be read, and is empty when it could.
	Err string
}

// KernelReported reports whether the report's version carries the kernel facts,
// which arrived at version 10. Before it, an empty kernel is not a reading.
func (r Report) KernelReported() bool { return r.Version >= 10 }

// NetworkAttachmentsReported reports whether the report's version carries the
// NetworkAttachmentDefinitions, which arrived at version 11. Before it, an
// absent reading is not a cluster without Multus.
func (r Report) NetworkAttachmentsReported() bool { return r.Version >= 11 }

// Read reads a bundle from a .tar.gz stream.
//
// It fails only for an archive that is not a bundle at all: one that does not
// decompress, holds a file over MaxEntryBytes, or carries no operatorops.yaml.
// A file that is there and does not parse is recorded in Problems, and the rest
// is still read, because a bundle is collected best-effort and read the same way.
func Read(r io.Reader) (*Bundle, error) {
	files, top, err := unpack(r)
	if err != nil {
		return nil, err
	}
	if _, ok := files["operatorops.yaml"]; !ok {
		return nil, errors.New("the archive carries no operatorops.yaml, so it is not a support bundle")
	}

	b := &Bundle{
		Name:           top,
		Errors:         string(files["errors.txt"]),
		Events:         string(files["events.txt"]),
		KubectlVersion: string(files["version.txt"]),
	}

	var run simplyblockv1alpha2.OperatorOps
	if err := yaml.Unmarshal(files["operatorops.yaml"], &run); err != nil {
		b.problem("operatorops.yaml", err)
	} else {
		b.Run = &run
	}

	if data, ok := files["cdc.yaml"]; ok {
		var cdc simplyblockv1alpha2.ClusterDeploymentConfig
		if err := yaml.Unmarshal(data, &cdc); err != nil {
			b.problem("cdc.yaml", err)
		} else if cdc.Name != "" {
			b.CDC = &cdc
		}
	}

	if data, ok := files["nodes.yaml"]; ok {
		var nodes corev1.NodeList
		if err := yaml.Unmarshal(data, &nodes); err != nil {
			b.problem("nodes.yaml", err)
		} else {
			b.Nodes = nodes.Items
		}
	}

	if data, ok := files["deployment.yaml"]; ok {
		if err := b.readDeployment(data); err != nil {
			b.problem("deployment.yaml", err)
		}
	}

	var reports []string
	for name := range files {
		if strings.HasPrefix(name, "reports/") && strings.HasSuffix(name, ".json") {
			reports = append(reports, name)
		}
	}
	sort.Strings(reports)
	for _, name := range reports {
		b.Reports = append(b.Reports, readReport(path.Base(name), files[name]))
	}
	return b, nil
}

func (b *Bundle) problem(file string, err error) {
	b.Problems = append(b.Problems, fmt.Sprintf("%s: %v", file, err))
}

// readDeployment reads the List of ControlPlanes, StorageClusters, and
// StorageNodes the bundle collected, each item by its kind.
func (b *Bundle) readDeployment(data []byte) error {
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	raw, err := yaml.YAMLToJSON(data)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	for _, item := range list.Items {
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(item, &meta); err != nil {
			return err
		}
		switch meta.Kind {
		case "ControlPlane":
			var o simplyblockv1alpha2.ControlPlane
			if err := json.Unmarshal(item, &o); err != nil {
				return err
			}
			b.ControlPlanes = append(b.ControlPlanes, o)
		case "StorageCluster":
			var o simplyblockv1alpha2.StorageCluster
			if err := json.Unmarshal(item, &o); err != nil {
				return err
			}
			b.StorageClusters = append(b.StorageClusters, o)
		case "StorageNode":
			var o simplyblockv1alpha2.StorageNode
			if err := json.Unmarshal(item, &o); err != nil {
				return err
			}
			b.StorageNodes = append(b.StorageNodes, o)
		}
	}
	return nil
}

// readReport decodes one probe report of a supported version.
func readReport(name string, data []byte) Report {
	out := Report{File: name}
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		out.Err = fmt.Sprintf("the report does not parse: %v", err)
		return out
	}
	out.Version = head.Version
	if !slices.Contains(SupportedReportVersions, head.Version) {
		out.Err = fmt.Sprintf("the report is version %d, and this tool reads versions %v",
			head.Version, SupportedReportVersions)
		return out
	}
	if err := json.Unmarshal(data, &out.Report); err != nil {
		out.Err = fmt.Sprintf("the report does not parse: %v", err)
	}
	return out
}

// unpack reads every regular file of the archive into memory, keyed by its path
// below the top directory, and returns that directory's name.
func unpack(r io.Reader) (map[string][]byte, string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, "", fmt.Errorf("the file is not a .tar.gz: %w", err)
	}
	defer func() { _ = gz.Close() }()

	files := map[string][]byte{}
	top := ""
	var total int64
	entries := 0
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("the archive does not read: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Size > MaxEntryBytes {
			return nil, "", fmt.Errorf("%s is %d bytes, over the %d this tool reads", hdr.Name, hdr.Size, MaxEntryBytes)
		}
		entries++
		total += hdr.Size
		if entries > MaxEntries {
			return nil, "", fmt.Errorf("the archive holds more than %d files, which no bundle does", MaxEntries)
		}
		if total > MaxTotalBytes {
			return nil, "", fmt.Errorf("the archive expands past %d bytes, which no bundle does", MaxTotalBytes)
		}
		first, rest, found := strings.Cut(path.Clean(hdr.Name), "/")
		// tar on macOS writes an AppleDouble ._<name> beside every file with
		// extended attributes. It holds Finder metadata, not bundle content.
		if !found || strings.HasPrefix(path.Base(rest), "._") || strings.HasPrefix(rest, "__MACOSX/") {
			continue
		}
		top = first
		data, err := io.ReadAll(io.LimitReader(tr, MaxEntryBytes+1))
		if err != nil {
			return nil, "", fmt.Errorf("%s does not read: %w", hdr.Name, err)
		}
		files[rest] = data
	}
	return files, top, nil
}
