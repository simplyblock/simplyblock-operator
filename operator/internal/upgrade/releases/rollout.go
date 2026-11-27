// The rollout's generic steps: wait for the operator, upgrade the control plane,
// roll every cluster's storage plane onto its desired images, and verify
// (design-release-upgrade.md §6.5, steps 4 to 7). Each step that changes
// anything does it by creating an operation the operator performs, named for
// the target release, so the operation is the record of how far the rollout got
// and a rerun skips what succeeded (§8). They live here, beside the catalog
// that places module steps between them.

package releases

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/crdtypes"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// The identities of the generic steps.
const (
	IDApplyCRDs           upgrade.ID = "release-apply-crds"
	IDAwaitOperator       upgrade.ID = "release-await-operator"
	IDUpgradeControlPlane upgrade.ID = "release-upgrade-control-plane"
	IDRollStoragePlane    upgrade.ID = "release-roll-storage-plane"
	IDVerify              upgrade.ID = "release-verify"
)

// The objects a deployment's rollout reads by name.
const (
	operatorDeployment = "simplyblock-operator"
	operatorContainer  = "manager"
	controlPlaneName   = "simplyblock"
)

// Options are what a rollout is told rather than what it reads.
type Options struct {
	// SimplyblockImage is the control-plane image of the target release. The
	// control-plane step needs it, and refuses without it.
	SimplyblockImage string

	// SpdkImage, when set, is the SPDK image the target control plane reports
	// as its default. The storage-plane step waits for every cluster to report
	// it before planning a walk, which is the exact form of its readiness gate.
	SpdkImage string

	// Settle is how long after the control-plane upgrade the storage-plane step
	// waits before trusting what the clusters report, when SpdkImage is not
	// set: long enough for each cluster's reconciler to read the new record.
	Settle time.Duration

	// Poll is how often a wait reads again, and Timeout how long one waits.
	Poll    time.Duration
	Timeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.Settle == 0 {
		o.Settle = 2 * time.Minute
	}
	if o.Poll == 0 {
		o.Poll = 10 * time.Second
	}
	if o.Timeout == 0 {
		o.Timeout = 24 * time.Hour
	}
	return o
}

// OperationName is the name of an operation a rollout to target creates, so a
// rerun finds the one an earlier run made.
func OperationName(target Version, parts ...string) string {
	release := strings.NewReplacer(".", "-", "+", "-").Replace(strings.ToLower(target.String()))
	return strings.Join(append([]string{"upgrade", release}, parts...), "-")
}

// step is the shape every generic step shares: it belongs to one stage, acts on
// the upgrade itself, and is described, applied, and verified by functions.
type step struct {
	id          upgrade.ID
	description string
	stage       upgrade.Stage

	// pending reports what is left to do as an action, or nil when the step's
	// effect is present.
	pending func(ctx context.Context, s *upgrade.Scope) (*upgrade.Action, error)
	// validate reports why the step cannot run, or nil.
	validate func(ctx context.Context, s *upgrade.Scope) error
	// apply performs the step and waits for it.
	apply func(ctx context.Context, s *upgrade.Scope) error
}

func (st step) ID() upgrade.ID         { return st.id }
func (st step) Description() string    { return st.description }
func (st step) Stage() upgrade.Stage   { return st.stage }
func (st step) Phase() upgrade.Phase   { return "" }
func (st step) Requires() []upgrade.ID { return nil }

func (st step) Describe(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) (*upgrade.Action, error) {
	if !subject.IsUpgrade() {
		return nil, nil
	}
	return st.pending(ctx, s)
}

func (st step) Done(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) (bool, error) {
	if !subject.IsUpgrade() {
		return false, nil
	}
	action, err := st.pending(ctx, s)
	return action == nil, err
}

func (st step) Validate(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	if !subject.IsUpgrade() || st.validate == nil {
		return nil
	}
	return st.validate(ctx, s)
}

func (st step) Apply(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	if !subject.IsUpgrade() {
		return nil
	}
	return st.apply(ctx, s)
}

func (st step) Verify(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	if !subject.IsUpgrade() {
		return nil
	}
	action, err := st.pending(ctx, s)
	if err != nil {
		return err
	}
	if action != nil {
		return fmt.Errorf("%s: still to do after it ran: %s", st.id, action.Detail)
	}
	return nil
}

// await polls until done reports true, and returns its last word on timeout.
func await(ctx context.Context, o Options, what string, done func(ctx context.Context) (bool, string, error)) error {
	last := ""
	err := wait.PollUntilContextTimeout(ctx, o.Poll, o.Timeout, true, func(ctx context.Context) (bool, error) {
		ok, why, err := done(ctx)
		last = why
		return ok, err
	})
	if err != nil {
		if last != "" {
			return fmt.Errorf("waiting for %s: %s: %w", what, last, err)
		}
		return fmt.Errorf("waiting for %s: %w", what, err)
	}
	return nil
}

// awaitOperator waits until the operator Deployment runs the target release and
// is available. An image tag that is not a version (a digest, latest) cannot be
// compared, and availability alone decides then.
func awaitOperator(target Version, o Options) upgrade.Step {
	ready := func(ctx context.Context, s *upgrade.Scope) (bool, string, error) {
		var d appsv1.Deployment
		if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: operatorDeployment}, &d); err != nil {
			return false, "", fmt.Errorf("read the operator Deployment: %w", err)
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			if c.Name != operatorContainer {
				continue
			}
			if v, err := ParseVersion(imageTag(c.Image)); err == nil && !v.Equal(target) {
				return false, fmt.Sprintf("the operator runs %s", c.Image), nil
			}
		}
		if d.Status.ObservedGeneration < d.Generation || d.Status.UpdatedReplicas < replicas(d) ||
			d.Status.AvailableReplicas < replicas(d) {
			return false, "the operator Deployment has not finished rolling out", nil
		}
		return true, "", nil
	}
	return step{
		id:          IDAwaitOperator,
		description: "waits until the operator runs the target release and is available",
		stage:       upgrade.StageReleaseRollout,
		pending: func(ctx context.Context, s *upgrade.Scope) (*upgrade.Action, error) {
			ok, why, err := ready(ctx, s)
			if err != nil || ok {
				return nil, err
			}
			return &upgrade.Action{Rule: IDAwaitOperator, Verb: upgrade.VerbAwait,
				Object: upgrade.ObjectRef{Namespace: s.Namespace, Name: operatorDeployment},
				Detail: fmt.Sprintf("%s -> the operator at %s", why, target)}, nil
		},
		apply: func(ctx context.Context, s *upgrade.Scope) error {
			return await(ctx, o, "the operator", func(ctx context.Context) (bool, string, error) { return ready(ctx, s) })
		},
	}
}

// upgradeControlPlane moves a local control plane to the target image through
// a ControlPlaneOps Upgrade, which drains, applies, and verifies. A managed
// control plane is its hub's to upgrade, and the step only checks it is new
// enough.
func upgradeControlPlane(target Version, o Options) upgrade.Step {
	name := OperationName(target)
	return step{
		id:          IDUpgradeControlPlane,
		description: "upgrades a local control plane to the target image with a ControlPlaneOps Upgrade, and checks a managed one is new enough",
		stage:       upgrade.StageReleaseRollout,
		pending: func(ctx context.Context, s *upgrade.Scope) (*upgrade.Action, error) {
			cp, err := readControlPlane(ctx, s)
			if err != nil || cp == nil || cp.Spec.Source.Local == nil {
				return nil, err
			}
			ops, err := readOps[simplyblockv1alpha2.ControlPlaneOps](ctx, s, name)
			if err != nil {
				return nil, err
			}
			if ops != nil && ops.Status.Phase == simplyblockv1alpha2.ControlPlaneOpsPhaseSucceeded {
				return nil, nil
			}
			if ops == nil && o.SimplyblockImage != "" && string(cp.Spec.Source.Local.Image) == o.SimplyblockImage {
				return nil, nil
			}
			verb, detail := upgrade.VerbCreate, fmt.Sprintf("%s -> %s", cp.Spec.Source.Local.Image, o.SimplyblockImage)
			if ops != nil {
				verb, detail = upgrade.VerbAwait, fmt.Sprintf("%s is %s", name, phaseOrPending(string(ops.Status.Phase)))
			}
			return &upgrade.Action{Rule: IDUpgradeControlPlane, Verb: verb,
				Object: upgrade.ObjectRef{Namespace: s.Namespace, Name: name}, Detail: detail}, nil
		},
		validate: func(ctx context.Context, s *upgrade.Scope) error {
			cp, err := readControlPlane(ctx, s)
			if err != nil || cp == nil {
				return err
			}
			if cp.Spec.Source.Managed != nil {
				if v, err := ParseVersion(cp.Status.Version); err == nil && v.LessThan(target) {
					return fmt.Errorf("the managed control plane reports %s, older than %s: "+
						"upgrade it from its hub first", cp.Status.Version, target)
				}
				return nil
			}
			if o.SimplyblockImage == "" {
				return fmt.Errorf("the target control-plane image is not known: pass --simplyblock-image")
			}
			return nil
		},
		apply: func(ctx context.Context, s *upgrade.Scope) error {
			ops, err := readOps[simplyblockv1alpha2.ControlPlaneOps](ctx, s, name)
			if err != nil {
				return err
			}
			if ops == nil {
				ops = &simplyblockv1alpha2.ControlPlaneOps{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace},
					Spec: simplyblockv1alpha2.ControlPlaneOpsSpec{
						ControlPlaneRef: controlPlaneName,
						Action:          simplyblockv1alpha2.ControlPlaneOpsActionUpgrade,
						Upgrade:         &simplyblockv1alpha2.UpgradeSpec{Image: crdtypes.ImageReference(o.SimplyblockImage)},
					},
				}
				if err := s.Client.Create(ctx, ops); err != nil && !apierrors.IsAlreadyExists(err) {
					return fmt.Errorf("create %s: %w", name, err)
				}
			}
			return awaitControlPlaneOps(ctx, s, o, name)
		},
	}
}

func awaitControlPlaneOps(ctx context.Context, s *upgrade.Scope, o Options, name string) error {
	return await(ctx, o, name, func(ctx context.Context) (bool, string, error) {
		ops, err := readOps[simplyblockv1alpha2.ControlPlaneOps](ctx, s, name)
		if err != nil || ops == nil {
			return false, "", err
		}
		switch ops.Status.Phase {
		case simplyblockv1alpha2.ControlPlaneOpsPhaseSucceeded:
			return true, "", nil
		case simplyblockv1alpha2.ControlPlaneOpsPhaseFailed, simplyblockv1alpha2.ControlPlaneOpsPhaseAborted:
			return false, "", fmt.Errorf("%s %s: %s", name, ops.Status.Phase, ops.Status.Message)
		}
		return false, fmt.Sprintf("%s is %s: %s", name, phaseOrPending(string(ops.Status.Phase)), ops.Status.Message), nil
	})
}

// rollStoragePlane rolls every storage cluster onto its nodes' desired images,
// one cluster after the other, with a RollingRestart that restarts only the
// nodes not yet on them.
//
// A walk is planned from what each node reports, so it is not created before
// the clusters report what the upgraded control plane says: its default images
// reach a cluster's status on the cluster reconciler's next pass, and the
// nodes' desired images on theirs. Planned earlier, a walk would find every
// node current against the old default and restart nothing.
func rollStoragePlane(target Version, o Options) upgrade.Step {
	return step{
		id:          IDRollStoragePlane,
		description: "rolls each storage cluster onto its nodes' desired images with a RollingRestart of the out-of-date nodes",
		stage:       upgrade.StageReleaseRollout,
		pending: func(ctx context.Context, s *upgrade.Scope) (*upgrade.Action, error) {
			clusters, err := listClusters(ctx, s)
			if err != nil {
				return nil, err
			}
			var todo []string
			for i := range clusters {
				name := OperationName(target, clusters[i].Name, "restart")
				ops, err := readOps[simplyblockv1alpha2.StorageClusterOps](ctx, s, name)
				if err != nil {
					return nil, err
				}
				if ops == nil || ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
					todo = append(todo, clusters[i].Name)
				}
			}
			if len(todo) == 0 {
				return nil, nil
			}
			return &upgrade.Action{Rule: IDRollStoragePlane, Verb: upgrade.VerbCreate,
				Object: upgrade.ObjectRef{Namespace: s.Namespace, Name: OperationName(target, "storage-plane")},
				Detail: "RollingRestart, selection OutOfDate, of " + strings.Join(todo, ", ")}, nil
		},
		apply: func(ctx context.Context, s *upgrade.Scope) error {
			clusters, err := listClusters(ctx, s)
			if err != nil {
				return err
			}
			for i := range clusters {
				if err := rollCluster(ctx, s, target, o, clusters[i].Name); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func rollCluster(ctx context.Context, s *upgrade.Scope, target Version, o Options, cluster string) error {
	name := OperationName(target, cluster, "restart")
	ops, err := readOps[simplyblockv1alpha2.StorageClusterOps](ctx, s, name)
	if err != nil {
		return err
	}
	if ops == nil {
		if err := await(ctx, o, "cluster "+cluster+" to report the target's images",
			func(ctx context.Context) (bool, string, error) { return clusterSettled(ctx, s, target, o, cluster) }); err != nil {
			return err
		}
		ops = &simplyblockv1alpha2.StorageClusterOps{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace},
			Spec: simplyblockv1alpha2.StorageClusterOpsSpec{
				ClusterRef: cluster,
				Action:     simplyblockv1alpha2.StorageClusterOpsActionRollingRestart,
				RollingRestart: &simplyblockv1alpha2.RollingRestartSpec{
					RefreshSNodeAPI: true,
					Selection:       simplyblockv1alpha2.RollingRestartSelectionOutOfDate,
				},
			},
		}
		if err := s.Client.Create(ctx, ops); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %s: %w", name, err)
		}
	}
	return AwaitClusterOps(ctx, s, o, name)
}

// AwaitClusterOps waits for a StorageClusterOps to succeed, and fails with its
// message when it fails or is aborted.
func AwaitClusterOps(ctx context.Context, s *upgrade.Scope, o Options, name string) error {
	return await(ctx, o.withDefaults(), name, func(ctx context.Context) (bool, string, error) {
		ops, err := readOps[simplyblockv1alpha2.StorageClusterOps](ctx, s, name)
		if err != nil || ops == nil {
			return false, "", err
		}
		switch ops.Status.Phase {
		case simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded:
			return true, "", nil
		case simplyblockv1alpha2.StorageClusterOpsPhaseFailed, simplyblockv1alpha2.StorageClusterOpsPhaseAborted:
			return false, "", fmt.Errorf("%s %s: %s", name, ops.Status.Phase, ops.Status.Message)
		}
		return false, fmt.Sprintf("%s is %s: %s", name, phaseOrPending(string(ops.Status.Phase)), ops.Status.Message), nil
	})
}

// clusterSettled is the storage-plane step's readiness gate for one cluster.
func clusterSettled(
	ctx context.Context, s *upgrade.Scope, target Version, o Options, name string,
) (bool, string, error) {
	var cluster simplyblockv1alpha2.StorageCluster
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, &cluster); err != nil {
		return false, "", fmt.Errorf("read cluster %s: %w", name, err)
	}

	if o.SpdkImage != "" {
		if cluster.Status.DefaultSpdkImage != o.SpdkImage {
			return false, fmt.Sprintf("the cluster reports %q as the default SPDK image, not %q",
				cluster.Status.DefaultSpdkImage, o.SpdkImage), nil
		}
	} else {
		ops, err := readOps[simplyblockv1alpha2.ControlPlaneOps](ctx, s, OperationName(target))
		if err != nil {
			return false, "", err
		}
		if ops != nil && ops.Status.CompletedAt != nil && time.Since(ops.Status.CompletedAt.Time) < o.Settle {
			return false, "letting the clusters read the upgraded control plane", nil
		}
	}

	var nodes simplyblockv1alpha2.StorageNodeList
	if err := s.Client.List(ctx, &nodes, client.InNamespace(s.Namespace)); err != nil {
		return false, "", fmt.Errorf("list the storage nodes: %w", err)
	}
	var clusterSpdk crdtypes.ImageReference
	if cluster.Spec.StorageNodes != nil {
		clusterSpdk = cluster.Spec.StorageNodes.SpdkImage
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Spec.ClusterRef != name || node.Status.UUID == "" {
			continue
		}
		want := firstSet(node.Spec.Config.SpdkImage, string(clusterSpdk), cluster.Status.DefaultSpdkImage)
		if node.Status.DesiredSpdkImage != want {
			return false, fmt.Sprintf("node %s has not resolved its desired image yet", node.Name), nil
		}
	}
	return true, "", nil
}

// verify waits until every storage cluster is online and every node runs its
// desired images.
func verify() upgrade.Step {
	check := func(ctx context.Context, s *upgrade.Scope) (bool, string, error) {
		clusters, err := listClusters(ctx, s)
		if err != nil {
			return false, "", err
		}
		for i := range clusters {
			if clusters[i].Status.Phase != simplyblockv1alpha2.StorageClusterPhaseOnline {
				return false, fmt.Sprintf("cluster %s is %s", clusters[i].Name, clusters[i].Status.Phase), nil
			}
		}
		var nodes simplyblockv1alpha2.StorageNodeList
		if err := s.Client.List(ctx, &nodes, client.InNamespace(s.Namespace)); err != nil {
			return false, "", fmt.Errorf("list the storage nodes: %w", err)
		}
		for i := range nodes.Items {
			node := &nodes.Items[i]
			if node.Status.Phase == simplyblockv1alpha2.StorageNodePhaseRemoved {
				continue
			}
			if node.Status.Phase != simplyblockv1alpha2.StorageNodePhaseOnline {
				return false, fmt.Sprintf("node %s is %s", node.Name, node.Status.Phase), nil
			}
			if node.Status.ImagesCurrent != nil && !*node.Status.ImagesCurrent {
				return false, fmt.Sprintf("node %s does not run its desired images", node.Name), nil
			}
		}
		return true, "", nil
	}
	o := Options{}.withDefaults()
	o.Timeout = 30 * time.Minute
	return step{
		id:          IDVerify,
		description: "waits until every storage cluster is online and every node runs its desired images",
		stage:       upgrade.StageReleaseRollout,
		pending: func(ctx context.Context, s *upgrade.Scope) (*upgrade.Action, error) {
			ok, why, err := check(ctx, s)
			if err != nil || ok {
				return nil, err
			}
			return &upgrade.Action{Rule: IDVerify, Verb: upgrade.VerbVerify,
				Object: upgrade.ObjectRef{Namespace: s.Namespace, Name: "deployment"}, Detail: why}, nil
		},
		apply: func(ctx context.Context, s *upgrade.Scope) error {
			return await(ctx, o, "the deployment to settle", func(ctx context.Context) (bool, string, error) { return check(ctx, s) })
		},
	}
}

func readControlPlane(ctx context.Context, s *upgrade.Scope) (*simplyblockv1alpha2.ControlPlane, error) {
	var cp simplyblockv1alpha2.ControlPlane
	err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: controlPlaneName}, &cp)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the ControlPlane: %w", err)
	}
	return &cp, nil
}

// readOps reads an operation by name, and nil when there is none.
func readOps[T any, P interface {
	*T
	client.Object
}](ctx context.Context, s *upgrade.Scope, name string) (P, error) {
	obj := P(new(T))
	err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, obj)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	return obj, nil
}

func listClusters(ctx context.Context, s *upgrade.Scope) ([]simplyblockv1alpha2.StorageCluster, error) {
	var list simplyblockv1alpha2.StorageClusterList
	if err := s.Client.List(ctx, &list, client.InNamespace(s.Namespace)); err != nil {
		return nil, fmt.Errorf("list the storage clusters: %w", err)
	}
	return list.Items, nil
}

// imageTag is the tag of an image reference, empty for a digest-only one.
func imageTag(image string) string {
	image, _, _ = strings.Cut(image, "@")
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon <= slash {
		return ""
	}
	return image[colon+1:]
}

func replicas(d appsv1.Deployment) int32 {
	if d.Spec.Replicas == nil {
		return 1
	}
	return *d.Spec.Replicas
}

func phaseOrPending(phase string) string {
	if phase == "" {
		return "Pending"
	}
	return phase
}

func firstSet(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
