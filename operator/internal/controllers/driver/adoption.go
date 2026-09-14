// Taking over a deployment a Helm release installed.
//
// Every cluster that ran simplyblock before this kind existed already holds the
// driver's objects, so the first reconcile there does not create a deployment,
// it meets one with volumes attached and workloads running on them. What makes
// that a handover rather than a rebuild is that nothing here deletes an object:
// recreating the node DaemonSet restarts every node plugin in the cluster at
// once, and recreating the registration takes the cluster's ability to attach a
// volume away for as long as it is absent.
//
// Specified by operator/docs/designs/crd-redesign/design-simplyblockdriver.md
// §4.3.

package driver

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// resourcePolicyAnnotation makes Helm skip an object's deletion. Helm reads
	// it from the live object rather than from the stored release manifest, so
	// writing it here is enough to keep a prune or an uninstall away from a
	// deployment this controller has taken over.
	resourcePolicyAnnotation = "helm.sh/resource-policy"
	resourcePolicyKeep       = "keep"
)

// helmLabels and helmAnnotations are what a release leaves on the objects it
// wrote. Once the handover is done they record a release that no longer
// contains the object, and something later reads a leftover claim as a live
// one, so they are removed. helm.sh/resource-policy is deliberately not in
// either list: what it says, that this object outlives the release, is the part
// that became permanently true.
var (
	helmLabels = []string{
		"app.kubernetes.io/managed-by",
		"heritage",
		"release",
		"revision",
		"chart",
		"chartVersion",
	}
	helmAnnotations = []string{
		"meta.helm.sh/release-name",
		"meta.helm.sh/release-namespace",
	}
)

// carriesHelmMetadata reports whether an object was written by a Helm release
// and has not been handed over yet.
func carriesHelmMetadata(obj client.Object) bool {
	annotations := obj.GetAnnotations()
	for _, key := range helmAnnotations {
		if annotations[key] != "" {
			return true
		}
	}
	return obj.GetLabels()["app.kubernetes.io/managed-by"] == "Helm"
}

// keepThroughHelm puts the resource policy on an object about to be applied, so
// that a release still tracking it cannot prune it out from under the
// controller. The installer does this before `helm upgrade` for the supported
// path; doing it here as well is what makes an adoption by hand safe, since an
// administrator creating the object against a chart-installed deployment has
// had no such step.
func keepThroughHelm(obj client.Object) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[resourcePolicyAnnotation] = resourcePolicyKeep
	obj.SetAnnotations(annotations)
}

// helmMetadataRemovalPatch is a merge patch that deletes the release's labels
// and annotations and nothing else.
//
// It is a patch rather than part of the apply because a server-side apply only
// governs the fields it sets: a label this controller never writes stays owned
// by whoever did write it, so the keys have to be nulled explicitly.
func helmMetadataRemovalPatch() []byte {
	var b strings.Builder
	b.WriteString(`{"metadata":{"labels":{`)
	for i, key := range helmLabels {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"` + key + `":null`)
	}
	b.WriteString(`},"annotations":{`)
	for i, key := range helmAnnotations {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"` + key + `":null`)
	}
	b.WriteString(`}}}`)
	return []byte(b.String())
}

// inexpressible is the configuration a running plugin can carry that this
// kind has no field for. Adoption reconciles toward the spec, so a plugin
// carrying one of these would come out of the handover without it: the apply
// lists env, volumes, and volumeMounts explicitly, and an entry the spec does
// not name is an entry the apply removes.
//
// Refusing is the only safe answer. Dropping csi-link is an agent that stops
// reaching the operator, which is not a change an administrator asked for by
// writing a SimplyblockDriver.
//
// It is off by default, which is why the driver could move out of the chart at
// all. It needs a spec surface before it can be adopted, which is the TODO in
// workloads.go. TLS had the same shape here until spec.tls existed;
// tlsAdoptionMismatch below is what replaced it, since TLS's refusal now
// compares against what the spec asks for rather than refusing outright.
var inexpressible = []struct {
	what   string
	envVar string
	arg    string
}{
	{what: "csi-link", arg: "--link"},
}

// unsupportedConfiguration reports the first thing a running node plugin
// carries that the spec cannot express.
func unsupportedConfiguration(ds *appsv1.DaemonSet) (string, bool) {
	if ds == nil {
		return "", false
	}
	for _, c := range ds.Spec.Template.Spec.Containers {
		for _, want := range inexpressible {
			if want.envVar != "" {
				for _, e := range c.Env {
					if e.Name == want.envVar {
						return want.what, true
					}
				}
			}
			if want.arg != "" {
				for _, a := range c.Args {
					if a == want.arg {
						return want.what, true
					}
				}
			}
		}
	}
	return "", false
}

// runningTLSConnectMode reads SB_TLS_CONNECT off a running node plugin, or
// "disabled" if it carries none — the same string tls.go's tlsConnectMode
// computes from spec.tls, so the two are the direct comparison
// tlsAdoptionMismatch makes.
func runningTLSConnectMode(ds *appsv1.DaemonSet) string {
	for _, c := range ds.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "SB_TLS_CONNECT" {
				return e.Value
			}
		}
	}
	return "disabled"
}

// tlsAdoptionMismatch compares a running node plugin's TLS mode against what
// spec.tls would produce, on the same reasoning runningDriverName's caller
// documents: adoption reconciles toward the spec, so a spec that does not yet
// describe the running configuration is one the next reconcile silently
// changes rather than adopts. Getting this wrong in either direction is a
// data path that stops being encrypted, or a plugin dialing with TLS nothing
// provisioned a certificate for, so a disagreement is refused rather than
// applied.
func tlsAdoptionMismatch(d *simplyblockv1alpha2.SimplyblockDriver, ds *appsv1.DaemonSet) (string, bool) {
	if ds == nil {
		return "", false
	}
	running := runningTLSConnectMode(ds)
	wanted := tlsConnectMode(d)
	if running == wanted {
		return "", false
	}
	return fmt.Sprintf(
		"the running node plugin is configured for TLS mode %q (SB_TLS_CONNECT) and spec.tls "+
			"describes %q; adopting it would reconcile the deployment to what spec.tls asks for, "+
			"so this either turns TLS off on a live data path or on for a plugin nothing has "+
			"provisioned a certificate for",
		running, wanted), true
}

// runningDriverName reads the driver name a deployed node plugin registers
// under, out of the kubelet registration path its registrar was given.
//
// This is the one fact adoption has to compare and cannot change.
// spec.driverName is immutable, every PersistentVolume records it in
// spec.csi.driver, and a deployment whose object declares one name while the
// cluster attaches volumes through another is not repairable by an edit,
// because the edit is the one admission rejects.
func runningDriverName(ds *appsv1.DaemonSet) (string, bool) {
	if ds == nil {
		return "", false
	}
	for _, c := range ds.Spec.Template.Spec.Containers {
		for _, arg := range c.Args {
			path, found := strings.CutPrefix(arg, "--kubelet-registration-path=")
			if !found {
				continue
			}
			// .../plugins/<driverName>/csi.sock
			rest, ok := strings.CutPrefix(path, kubeletPluginsDir+"/")
			if !ok {
				continue
			}
			name, _, ok := strings.Cut(rest, "/")
			if !ok || name == "" {
				continue
			}
			return name, true
		}
	}
	return "", false
}
