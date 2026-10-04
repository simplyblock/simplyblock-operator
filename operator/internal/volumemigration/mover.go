// One volume's move, raised as whichever kind the deployment uses.
//
// Two kinds express the same operation. The registered VolumeMigration is
// namespaced, names its target by backend UUID, and reaches Completed. The
// redesigned PersistentVolumeOps is cluster-scoped, names its target as a
// StorageNode object, and reaches Succeeded. Three controllers raise moves —
// the auto-rebalancer, a node drain, and the pinned-volume controller — and
// none of them has any business knowing which kind a deployment runs.
//
// So they ask for a move and this decides. The interface is the four things
// every caller does: start one, find its own again, read an outcome, and reap
// it. What a caller cannot do through here is anything specific to one kind,
// which is deliberate: a caller that needed that would be a caller the gate is
// not hiding anything from.
//
// design-persistentvolumeops.md §10 is what the two kinds differ by.

package volumemigration

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// MovePhase is a move's progress, in the one vocabulary both kinds are read
// through.
type MovePhase string

const (
	// MovePending is a move nothing has started: admitted, holding no lock.
	MovePending MovePhase = "Pending"
	// MoveRunning is a move in flight, wherever in its own steps it is.
	MoveRunning MovePhase = "Running"
	// MoveSucceeded is a volume that moved.
	MoveSucceeded MovePhase = "Succeeded"
	// MoveFailed is one that did not.
	MoveFailed MovePhase = "Failed"
	// MoveAborted is one stopped on request, or stopped because its volume went
	// away.
	MoveAborted MovePhase = "Aborted"
)

// Terminal reports a phase the move never leaves.
func (p MovePhase) Terminal() bool {
	switch p {
	case MoveSucceeded, MoveFailed, MoveAborted:
		return true
	default:
		return false
	}
}

// Move is one volume's move, as either kind reports it.
type Move struct {
	Name string
	// Namespace is empty for the cluster-scoped kind, which is what a caller
	// reading a move back has to carry rather than assume.
	Namespace string
	PVName    string
	Phase     MovePhase
	// Message is why the phase is what it is, which a caller puts in the event
	// it raises about the move.
	Message string
}

// MoveRequest is one volume's move, as a caller asks for it.
type MoveRequest struct {
	// Name is what the move is called. A caller picks a name derived from the
	// volume rather than a generated one, which is what makes starting the same
	// move twice idempotent.
	Name string

	// Namespace is where a namespaced move goes. The cluster-scoped kind takes
	// it as the namespace its target node is looked for in rather than as its
	// own.
	Namespace string

	PVName string

	// TargetNodeUUID is the backend identifier of the node to move to, which is
	// what every caller holds. The cluster-scoped kind names the Kubernetes
	// object instead, and resolving the one to the other is this file's job.
	TargetNodeUUID string

	Labels map[string]string

	// Owner is the object that asked for the move, when there is one. The two
	// kinds carry it differently and that difference is not the caller's: a
	// namespaced move takes a controller reference, and a cluster-scoped one
	// cannot, because Kubernetes treats a namespaced owner of a cluster-scoped
	// object as unresolvable and garbage-collects the dependent.
	Owner client.Object
	// OwnerKind is the owner's kind, which the cluster-scoped kind records in
	// spec.creatorRef and in the managed-by label.
	OwnerKind string
	// Scheme resolves the owner's group and version for a controller
	// reference. Only the namespaced kind needs it.
	Scheme *runtime.Scheme
}

// Mover raises and tracks volume moves.
type Mover interface {
	// Start raises one move. A move that already exists is the state being
	// asked for and is not an error.
	Start(ctx context.Context, request MoveRequest) error

	// List finds the moves in a namespace carrying every one of the labels. The
	// namespace is ignored by the cluster-scoped kind, which has none.
	List(ctx context.Context, namespace string, labels map[string]string) ([]Move, error)

	// Get reads one move back. It returns a wrapped NotFound when there is
	// none, which every caller treats as "stop tracking it."
	Get(ctx context.Context, name, namespace string) (Move, error)

	// Delete reaps one. A move that is already gone is not an error.
	Delete(ctx context.Context, move Move) error
}

// MigrationMover raises the registered VolumeMigration.
type MigrationMover struct {
	client.Client
	Scheme *runtime.Scheme
}

func (m *MigrationMover) Start(ctx context.Context, request MoveRequest) error {
	migration := &simplyblockv1alpha1.VolumeMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      request.Name,
			Namespace: request.Namespace,
			Labels:    request.Labels,
		},
		Spec: simplyblockv1alpha1.VolumeMigrationSpec{
			PVName:         request.PVName,
			TargetNodeUUID: request.TargetNodeUUID,
		},
	}
	if request.Owner != nil {
		scheme := request.Scheme
		if scheme == nil {
			scheme = m.Scheme
		}
		if err := controllerutil.SetControllerReference(request.Owner, migration, scheme); err != nil {
			return fmt.Errorf("own the migration of %s: %w", request.PVName, err)
		}
	}
	if err := m.Create(ctx, migration); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create the migration of %s: %w", request.PVName, err)
	}
	return nil
}

func (m *MigrationMover) List(
	ctx context.Context, namespace string, labels map[string]string,
) ([]Move, error) {
	var migrations simplyblockv1alpha1.VolumeMigrationList
	options := []client.ListOption{client.InNamespace(namespace)}
	if len(labels) > 0 {
		options = append(options, client.MatchingLabels(labels))
	}
	if err := m.Client.List(ctx, &migrations, options...); err != nil {
		return nil, fmt.Errorf("list the volume migrations: %w", err)
	}
	moves := make([]Move, 0, len(migrations.Items))
	for i := range migrations.Items {
		moves = append(moves, migrationMove(&migrations.Items[i]))
	}
	return moves, nil
}

func (m *MigrationMover) Get(ctx context.Context, name, namespace string) (Move, error) {
	var migration simplyblockv1alpha1.VolumeMigration
	if err := m.Client.Get(ctx,
		types.NamespacedName{Name: name, Namespace: namespace}, &migration); err != nil {
		return Move{}, err
	}
	return migrationMove(&migration), nil
}

func (m *MigrationMover) Delete(ctx context.Context, move Move) error {
	migration := &simplyblockv1alpha1.VolumeMigration{
		ObjectMeta: metav1.ObjectMeta{Name: move.Name, Namespace: move.Namespace},
	}
	if err := m.Client.Delete(ctx, migration); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete the migration %s: %w", move.Name, err)
	}
	return nil
}

// migrationMove reads the registered kind's merged phase-and-step enum into the
// shared vocabulary. Validating is one of its values and is a step rather than
// an outcome, which is the confusion the redesign's split exists to end.
func migrationMove(migration *simplyblockv1alpha1.VolumeMigration) Move {
	phase := MovePending
	switch migration.Status.Phase {
	case simplyblockv1alpha1.VolumeMigrationPhaseCompleted:
		phase = MoveSucceeded
	case simplyblockv1alpha1.VolumeMigrationPhaseFailed:
		phase = MoveFailed
	case simplyblockv1alpha1.VolumeMigrationPhaseAborted:
		phase = MoveAborted
	case simplyblockv1alpha1.VolumeMigrationPhaseRunning,
		simplyblockv1alpha1.VolumeMigrationPhaseValidating:
		phase = MoveRunning
	}
	return Move{
		Name:      migration.Name,
		Namespace: migration.Namespace,
		PVName:    migration.Spec.PVName,
		Phase:     phase,
		Message:   migration.Status.ErrorMessage,
	}
}

// OperationMover raises the redesigned PersistentVolumeOps.
type OperationMover struct {
	client.Client
	Scheme *runtime.Scheme
}

// Start resolves the target node's backend UUID to the StorageNode object the
// kind names, and records the creator rather than owning the object.
//
// The creator reference is what replaces the owner reference a cluster-scoped
// object cannot have. It carries the UID, for the reason pv.spec.claimRef does:
// a creator deleted and recreated under the same name must not inherit a
// fan-out it did not issue.
func (m *OperationMover) Start(ctx context.Context, request MoveRequest) error {
	node, err := m.nodeReporting(ctx, request.Namespace, request.TargetNodeUUID)
	if err != nil {
		return err
	}

	labels := map[string]string{}
	for key, value := range request.Labels {
		labels[key] = value
	}

	ops := &simplyblockv1alpha2.PersistentVolumeOps{
		ObjectMeta: metav1.ObjectMeta{Name: request.Name, Labels: labels},
		Spec: simplyblockv1alpha2.PersistentVolumeOpsSpec{
			PersistentVolumeName: request.PVName,
			Action:               simplyblockv1alpha2.PersistentVolumeOpsActionMigrate,
			Migrate: &simplyblockv1alpha2.MigrateVolumeSpec{
				TargetNodeRef: simplyblockv1alpha2.StorageNodeReference{
					Namespace: node.Namespace,
					Name:      node.Name,
				},
			},
		},
	}

	if request.Owner != nil {
		ops.Spec.CreatorRef = &simplyblockv1alpha2.CreatorReference{
			Kind:      request.OwnerKind,
			Namespace: request.Owner.GetNamespace(),
			Name:      request.Owner.GetName(),
			UID:       request.Owner.GetUID(),
		}
		// A reference cannot be selected on, so the kind travels as a label
		// too: the label finds the operations some creator raised, and the UID
		// says which one.
		labels[simplyblockv1alpha2.PersistentVolumeOpsManagedByLabel] = request.OwnerKind
	}

	if err := m.Create(ctx, ops); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create the operation moving %s: %w", request.PVName, err)
	}
	return nil
}

// nodeReporting finds the StorageNode whose status reports this backend UUID.
//
// A UUID nothing reports is refused rather than guessed at. The kind names a
// Kubernetes object, so an operation written against a node that has none would
// be refused at admission with a worse message than this one.
func (m *OperationMover) nodeReporting(
	ctx context.Context, namespace, uuid string,
) (*simplyblockv1alpha2.StorageNode, error) {
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := m.Client.List(ctx, &nodes, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list the storage nodes of namespace %s: %w", namespace, err)
	}
	for i := range nodes.Items {
		if nodes.Items[i].Status.UUID == uuid {
			return &nodes.Items[i], nil
		}
	}
	return nil, fmt.Errorf("no StorageNode in namespace %s reports node %s", namespace, uuid)
}

func (m *OperationMover) List(
	ctx context.Context, _ string, labels map[string]string,
) ([]Move, error) {
	var operations simplyblockv1alpha2.PersistentVolumeOpsList
	var options []client.ListOption
	if len(labels) > 0 {
		options = append(options, client.MatchingLabels(labels))
	}
	if err := m.Client.List(ctx, &operations, options...); err != nil {
		return nil, fmt.Errorf("list the volume operations: %w", err)
	}
	moves := make([]Move, 0, len(operations.Items))
	for i := range operations.Items {
		moves = append(moves, operationMove(&operations.Items[i]))
	}
	return moves, nil
}

func (m *OperationMover) Get(ctx context.Context, name, _ string) (Move, error) {
	var ops simplyblockv1alpha2.PersistentVolumeOps
	if err := m.Client.Get(ctx, types.NamespacedName{Name: name}, &ops); err != nil {
		return Move{}, err
	}
	return operationMove(&ops), nil
}

func (m *OperationMover) Delete(ctx context.Context, move Move) error {
	ops := &simplyblockv1alpha2.PersistentVolumeOps{
		ObjectMeta: metav1.ObjectMeta{Name: move.Name},
	}
	if err := m.Client.Delete(ctx, ops); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete the operation %s: %w", move.Name, err)
	}
	return nil
}

func operationMove(ops *simplyblockv1alpha2.PersistentVolumeOps) Move {
	phase := MovePending
	switch ops.Status.Phase {
	case simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded:
		phase = MoveSucceeded
	case simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed:
		phase = MoveFailed
	case simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted:
		phase = MoveAborted
	case simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning:
		phase = MoveRunning
	}
	return Move{
		Name:    ops.Name,
		PVName:  ops.Spec.PersistentVolumeName,
		Phase:   phase,
		Message: ops.Status.Message,
	}
}

// NewMover builds the mover a deployment uses.
//
// The redesigned kind is the default and the registered one is opt-in, which is
// the way round it is because a deployment that says nothing should be running
// the kind this API group documents. The legacy kind is what an upgrade turns
// back on while migrations raised against it drain, since a rename and a scope
// change make a new CRD rather than a new version and an in-flight migration
// cannot be carried across.
func NewMover(c client.Client, scheme *runtime.Scheme, legacy bool) Mover {
	if legacy {
		return &MigrationMover{Client: c, Scheme: scheme}
	}
	return &OperationMover{Client: c, Scheme: scheme}
}
