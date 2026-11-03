// The gate a local install passes before its first step: which cluster add-ons
// this control plane needs, given its spec, and whether each is present,
// installed now, or missing with its install turned off.

package controlplane

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/prerequisites"
)

// wantedPrerequisite pairs an add-on with whether a missing one is installed
// and whether the control-plane install has to wait for it.
type wantedPrerequisite struct {
	prerequisites.Prerequisite
	install  bool
	blocking bool
}

// wantedPrerequisites lists the add-ons this control plane needs. cert-manager
// only when it issues the TLS certificate, and local-path only when the volumes
// name no StorageClass of their own. The snapshot controller is for the CSI
// driver, so it is installed but never waited for.
func wantedPrerequisites(cp *simplyblockv1alpha2.ControlPlane) []wantedPrerequisite {
	local := cp.Spec.Source.Local
	p := local.Prerequisites
	wanted := []wantedPrerequisite{{prerequisites.FoundationDBCRDs, p.InstallsFoundationDBCRDs(), true}}
	if local.NeedsCertManager() {
		wanted = append(wanted, wantedPrerequisite{prerequisites.CertManager, p.InstallsCertManager(), true})
	}
	if fdb := foundationDBSpecOf(cp); fdb == nil || fdb.StorageClassName == "" {
		wanted = append(wanted, wantedPrerequisite{prerequisites.LocalPathProvisioner, p.InstallsLocalPathProvisioner(), true})
	}
	return append(wanted, wantedPrerequisite{prerequisites.SnapshotController, p.InstallsSnapshotController(), false})
}

// ensurePrerequisites installs every missing add-on its flag allows, upgrades
// the outdated ones the operator installed, and reports whether the install has
// to hold, with a message naming why.
func (r *ControlPlaneReconciler) ensurePrerequisites(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane,
) (bool, string, error) {
	opts := prerequisites.InstallOptions{ImageRegistry: r.ImageMirrorRegistry, Namespace: cp.Namespace}
	var waiting, missing []string
	for _, w := range wantedPrerequisites(cp) {
		state, err := w.State(ctx, r.Client, opts)
		if err != nil {
			return false, "", err
		}
		switch {
		case state == prerequisites.Absent && w.install:
			log.FromContext(ctx).Info("installing a prerequisite", "prerequisite", w.Name)
			if err := w.Install(ctx, r.Client, opts); err != nil {
				return false, "", err
			}
		case state == prerequisites.Outdated && w.AutoUpgrade:
			// The add-on is running already, so a failed upgrade is reported
			// rather than stopping the reconcile of the control plane it serves.
			log.FromContext(ctx).Info("upgrading a prerequisite", "prerequisite", w.Name)
			if err := w.Install(ctx, r.Client, opts); err != nil {
				r.emit(cp, corev1.EventTypeWarning, PrerequisiteOutdated,
					fmt.Sprintf("upgrading %s failed: %v", w.Name, err))
			}
		case state == prerequisites.Outdated:
			r.emit(cp, corev1.EventTypeWarning, PrerequisiteOutdated, fmt.Sprintf(
				"%s was installed by an earlier operator release; upgrade it along its supported path", w.Name))
		}

		served, err := w.Present(ctx, r.Client, r.RESTMapper())
		if err != nil {
			return false, "", err
		}
		switch {
		case served:
		case !w.blocking:
			if state == prerequisites.Absent && !w.install {
				r.emit(cp, corev1.EventTypeWarning, PrerequisiteMissing, fmt.Sprintf(
					"%s is missing and its install is turned off; volume snapshots will not work", w.Name))
			}
		case state == prerequisites.Absent && !w.install:
			missing = append(missing, fmt.Sprintf("%s (%s)", w.Name, w.Probe()))
		default:
			waiting = append(waiting, w.Name)
		}
	}

	// The issuer chain needs cert-manager's webhook, so it is applied only once
	// nothing is held. A rejection while that webhook starts is a wait, not an error.
	if len(waiting) == 0 && len(missing) == 0 && cp.Spec.Source.Local.NeedsCertManager() {
		if err := prerequisites.EnsureCertManagerIssuerChain(ctx, r.Client, cp.Namespace); err != nil {
			return true, "waiting for cert-manager to accept the simplyblock issuer chain: " + err.Error(), nil
		}
	}

	var parts []string
	if len(waiting) > 0 {
		parts = append(parts, "installing "+strings.Join(waiting, ", ")+"; waiting for the cluster to serve them")
	}
	if len(missing) > 0 {
		parts = append(parts, "missing, and their install is turned off in spec.source.local.prerequisites: "+
			strings.Join(missing, ", "))
	}
	return len(parts) > 0, strings.Join(parts, "; "), nil
}
