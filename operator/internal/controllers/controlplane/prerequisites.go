// The gate a local install passes before its first step: which cluster add-ons
// this control plane needs, given its spec and the cluster it runs on, and
// whether each is present, installed now, or missing with its install turned
// off.

package controlplane

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/inventory"

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
// only when it issues the TLS certificate. When the volumes name no
// StorageClass of their own, an EKS cluster gets the EBS CSI driver and a gp3
// class of it (chooseEBSClass), and any other cluster local-path. The snapshot
// controller is for the CSI driver, so it is installed but never waited for.
func wantedPrerequisites(cp *simplyblockv1alpha2.ControlPlane, chooseEBSClass bool) []wantedPrerequisite {
	local := cp.Spec.Source.Local
	p := local.Prerequisites
	wanted := []wantedPrerequisite{{prerequisites.FoundationDBCRDs, p.InstallsFoundationDBCRDs(), true}}
	if local.NeedsCertManager() {
		wanted = append(wanted, wantedPrerequisite{prerequisites.CertManager, p.InstallsCertManager(), true})
	}
	switch {
	case chooseEBSClass:
		wanted = append(wanted,
			wantedPrerequisite{prerequisites.EBSCSIDriver, p.InstallsEBSCSIDriver(), true},
			wantedPrerequisite{prerequisites.EBSStorageClass, p.InstallsEBSStorageClass(), true})
	case storageClassOf(cp) == "":
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
	local := cp.Spec.Source.Local
	opts := prerequisites.InstallOptions{
		ImageRegistry: r.ImageMirrorRegistry,
		Namespace:     cp.Namespace,
		Tolerations:   local.Tolerations,
		AWSRoleARN:    local.EKSRoleARN,
	}
	chooseEBSClass, err := r.choosesEBSClass(ctx, cp)
	if err != nil {
		return false, "", err
	}
	var waiting, missing []string
	for _, w := range wantedPrerequisites(cp, chooseEBSClass) {
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

	// The class is recorded as soon as a gp3 class of the driver exists. The
	// database's volumes are claimed only once nothing above holds, the driver
	// included, so recording it early claims nothing from a missing driver.
	if chooseEBSClass {
		if err := r.recordStorageClass(ctx, cp); err != nil {
			return false, "", err
		}
	}

	// The issuer chain needs cert-manager's webhook, so it is applied only once
	// nothing is held. A rejection while that webhook starts is a wait, not an error.
	if len(waiting) == 0 && len(missing) == 0 && local.NeedsCertManager() {
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

// choosesEBSClass reports whether this control plane's volumes are to come
// from a gp3 class of the EBS CSI driver: on Amazon EKS, when neither the spec
// nor an earlier choice names a class, and only before the database exists. A
// control plane installed before the operator chose classes keeps the class
// its volumes were claimed from, because they cannot move.
func (r *ControlPlaneReconciler) choosesEBSClass(ctx context.Context, cp *simplyblockv1alpha2.ControlPlane) (bool, error) {
	if storageClassOf(cp) != "" {
		return false, nil
	}
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return false, fmt.Errorf("listing nodes to tell the Kubernetes distribution: %w", err)
	}
	if inventory.DetectEnvironment(nil, nodes.Items).Distribution != inventory.DistributionEKS {
		return false, nil
	}
	fdb := &unstructured.Unstructured{}
	fdb.SetGroupVersionKind(fdbClusterGVK)
	err := r.Get(ctx, client.ObjectKey{Namespace: cp.Namespace, Name: ComponentFDBCluster}, fdb)
	switch {
	case err == nil:
		return false, nil
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		// The FoundationDB CRDs are themselves a prerequisite, so on a fresh
		// cluster the kind may not be served yet, and nothing of it exists.
		return true, nil
	default:
		return false, fmt.Errorf("reading the FoundationDB cluster: %w", err)
	}
}

// recordStorageClass writes the gp3 class chosen for the control plane's
// volumes to its status, where storageClassOf reads it from.
func (r *ControlPlaneReconciler) recordStorageClass(ctx context.Context, cp *simplyblockv1alpha2.ControlPlane) error {
	name, err := prerequisites.GP3StorageClassName(ctx, r.Client)
	if err != nil || name == "" {
		return err
	}
	base := cp.DeepCopy()
	cp.Status.StorageClassName = name
	if err := r.Status().Patch(ctx, cp, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("recording the StorageClass %s of the control plane's volumes: %w", name, err)
	}
	log.FromContext(ctx).Info("chose the StorageClass of the control plane's volumes", "storageClass", name)
	return nil
}
