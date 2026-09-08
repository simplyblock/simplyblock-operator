// Where a probe leaves its report, and how the discovery step finds it again.
//
// A ConfigMap, and not the pod's termination message that the fio baseline Job
// uses. The kubelet caps a termination message at 4 KiB, and a report of a
// worker with two dozen disks is past that before the interfaces are counted,
// so the existing mechanism would truncate exactly the deployments it matters
// most on. A ConfigMap holds a mebibyte, survives the pod, and is readable with
// kubectl by whoever is trying to work out why their disk was not a candidate.
//
// The name is derived rather than looked up so that a probe pod restarted by
// its Job writes over its own report instead of leaving two.
//
// Neither the name nor the labels can be read back as the values that produced
// them. Both are sanitized, and both are truncated — the name to what an object
// name may be and a label to the 63 characters a label value may be — so a node
// called ip-10-0-1-23.eu-central-1.compute.internal survives in neither. The
// labels are for selecting a run's reports; the node a report is about is the
// node field inside the report itself, which is the only place it appears
// exactly as the cluster spells it.

package nodeprobe

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ReportKey is the ConfigMap key the report's JSON is stored under.
	ReportKey = "report.json"

	// LabelRun and LabelNode carry the run and the worker a report belongs to,
	// so the discovery step lists a run's reports rather than reconstructing
	// their names.
	LabelRun  = "storage.simplyblock.io/nodeprobe-run"
	LabelNode = "storage.simplyblock.io/nodeprobe-node"

	// LabelComponent marks everything the probe creates, which is what a
	// cleanup that has lost track of a run deletes by.
	LabelComponent = "storage.simplyblock.io/component"

	// ComponentNodeProbe is LabelComponent's value for a probe's objects.
	ComponentNodeProbe = "nodeprobe"

	// namePrefix opens every object name the probe generates.
	namePrefix = "sb-nodeprobe-"

	// nameHashLength is how much of the digest a generated name carries. Eight
	// hex characters is 32 bits, which is not a cryptographic claim: the digest
	// is there to keep two truncated node names apart, and the pair it
	// disambiguates is always within one namespace and one run.
	nameHashLength = 8

	// maxNameLength is the DNS-subdomain limit a ConfigMap name has to fit, and
	// maxStemLength leaves room for the prefix, the digest, and the dash
	// between them.
	maxNameLength = 253
	maxStemLength = maxNameLength - len(namePrefix) - nameHashLength - 1
)

// unsafeForName matches everything a DNS subdomain may not carry. A node is
// named ip-10-0-1-23.eu-central-1.compute.internal on one cloud and
// worker_3 on somebody's laboratory, and only the first of those is already a
// legal object name.
var unsafeForName = regexp.MustCompile(`[^a-z0-9.-]+`)

// ObjectName is the ConfigMap a run's report for one node goes into.
//
// It is deterministic in both arguments, so the probe and the operator compute
// the same name without either telling the other, and a re-run under a new run
// name writes new objects rather than overwriting reports somebody may have
// already read.
func ObjectName(run, node string) string {
	stem := unsafeForName.ReplaceAllString(strings.ToLower(run+"-"+node), "-")
	stem = strings.Trim(stem, ".-")
	if len(stem) > maxStemLength {
		stem = strings.TrimRight(stem[:maxStemLength], ".-")
	}

	digest := sha256.Sum256([]byte(run + "\x00" + node))
	return namePrefix + stem + "-" + hex.EncodeToString(digest[:])[:nameHashLength]
}

// ConfigMap renders a report as the object the probe writes.
//
// The owner is optional and is how a run's reports are cleaned up: with it, the
// garbage collector removes them when the run is deleted, and without it they
// stay until something deletes them by label. The probe is given the reference
// rather than looking one up, so that it needs no read access to the object
// that owns it.
func ConfigMap(namespace, run string, owner *metav1.OwnerReference, report Report) (*corev1.ConfigMap, error) {
	if namespace == "" {
		return nil, fmt.Errorf("a report needs a namespace to be written into")
	}
	if run == "" {
		return nil, fmt.Errorf("a report needs the name of the run it belongs to")
	}
	if report.Node == "" {
		return nil, fmt.Errorf("a report needs the node it describes")
	}

	encoded, err := Encode(report)
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxReportBytes {
		return nil, fmt.Errorf(
			"the report for node %s is %d bytes, and a ConfigMap holds %d: "+
				"the worker has more devices than one object can carry",
			report.Node, len(encoded), maxReportBytes)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ObjectName(run, report.Node),
			Namespace: namespace,
			Labels: map[string]string{
				LabelComponent: ComponentNodeProbe,
				LabelRun:       labelValue(run),
				LabelNode:      labelValue(report.Node),
			},
		},
		Data: map[string]string{ReportKey: string(encoded)},
	}
	if owner != nil {
		cm.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return cm, nil
}

// maxReportBytes is the room a report has. A ConfigMap may hold a mebibyte in
// total, and the margin is for the object's own metadata, which the API server
// counts against the same limit.
const maxReportBytes = 1<<20 - 8<<10

// ReportFromConfigMap reads back what the probe wrote.
func ReportFromConfigMap(cm *corev1.ConfigMap) (Report, error) {
	if cm == nil {
		return Report{}, fmt.Errorf("no ConfigMap to read a report from")
	}
	data, ok := cm.Data[ReportKey]
	if !ok {
		return Report{}, fmt.Errorf(
			"the ConfigMap %s/%s carries no %s, so the probe did not finish writing it",
			cm.Namespace, cm.Name, ReportKey)
	}
	report, err := Decode([]byte(data))
	if err != nil {
		return Report{}, fmt.Errorf("%s/%s: %w", cm.Namespace, cm.Name, err)
	}
	return report, nil
}

// ReportSelector is the label selector for every report of one run.
func ReportSelector(run string) map[string]string {
	return map[string]string{
		LabelComponent: ComponentNodeProbe,
		LabelRun:       labelValue(run),
	}
}

// maxLabelValueLength is the length a label value may have.
const maxLabelValueLength = 63

// labelValue makes a value a label may hold.
//
// A node name is a DNS subdomain and may be 253 characters, where a label value
// may be 63, so a long one is truncated. That is why the labels are for
// selecting a run's reports and the ConfigMap's own data is what says which
// node a report is about: the label may have lost the end of the name, and the
// report has not.
func labelValue(v string) string {
	v = unsafeForName.ReplaceAllString(strings.ToLower(v), "-")
	v = strings.Trim(v, ".-_")
	if len(v) > maxLabelValueLength {
		v = strings.Trim(v[:maxLabelValueLength], ".-_")
	}
	return v
}
