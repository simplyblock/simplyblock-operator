// The durable record of a migration. §22.1 decides its shape: almost every
// position determination is a read of the cluster, because an owner reference
// points at the cluster or at the set and a renamed kind's object exists or
// does not, and only three things cannot be derived that way. Those three live
// here, in a ConfigMap in the operator namespace.
//
// A ConfigMap and not a custom resource, because the migration is a command
// rather than a controller and nothing is watching on its behalf. It outlives
// every object the migration touches, including the StorageCluster, which a
// migration may have to be diagnosed after.

package upgrade

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/statemachine"
)

// RecordName is the ConfigMap the migration keeps its position in. It is fixed
// rather than derived, because two concurrent runs holding two differently
// named records would each believe they held the migration.
const RecordName = "simplyblock-api-migration"

// The keys the record's data carries. They are separate keys rather than one
// blob so that a human diagnosing a stopped migration can read the position
// with kubectl and without a decoder.
const (
	recordKeyPhase     = "phase"
	recordKeyDeadline  = "phaseDeadline"
	recordKeyStep      = "step"
	recordKeyPreflight = "preflight"
	recordKeyCapture   = "capture"
)

// Record is the migration's position and the three facts nothing can derive
// from the cluster (§22.1).
type Record struct {
	// Snapshot is the state machine's position: the phase, and when it expires.
	// Restoring from it runs no entry hook, so a resumed run does not repeat
	// the side effect of the phase it resumes into.
	Snapshot statemachine.Snapshot[Phase]

	// Step is the step that was in flight when a run was killed, for the one
	// case where a side effect is not visible in the object it acted on.
	Step ID

	// Preflight attests that the checks passed, and against which chart and CRD
	// version, which is not a question the cluster answers.
	Preflight *PreflightAttestation

	// Capture is the pre-upgrade state of the objects the release handover
	// annotated, which §12.3 diffs the operator's first reconcile against. It
	// is opaque here: the handover owns its shape, and the record only has to
	// carry it across a process boundary.
	Capture json.RawMessage

	// resourceVersion is the optimistic lock. Two concurrent runs cannot both
	// hold the migration, because the second one's write is rejected against
	// the version the first one already moved past.
	resourceVersion string
}

// PreflightAttestation records that the preflight passed, and what it passed
// against. A migration run against a chart other than the one the preflight
// examined has not been checked, whatever the record says.
type PreflightAttestation struct {
	// Passed is when the preflight completed without a blocking finding.
	Passed metav1.Time `json:"passed"`

	// ChartVersion is the version of the chart the upgrade was performed with.
	ChartVersion string `json:"chartVersion,omitempty"`

	// OperatorImage is the image the upgrade deployed.
	OperatorImage string `json:"operatorImage,omitempty"`

	// Skipped names the checks the run was told not to perform, so a later
	// reader knows what was never asked.
	Skipped []ID `json:"skipped,omitempty"`
}

// RecordStore reads and writes the record. It is an interface so a test can
// hold the position in memory, and so a later release can move it without every
// caller learning where it went.
type RecordStore interface {
	// Load returns the record, and whether one existed. A cluster with no
	// record is a migration that has not started, which is not an error.
	Load(ctx context.Context) (*Record, bool, error)

	// Save writes the record, and fails when another run has written since the
	// one being saved was loaded.
	Save(ctx context.Context, record *Record) error

	// Delete removes the record, which is what a successful migration does.
	Delete(ctx context.Context) error
}

// ConfigMapRecordStore keeps the record in a ConfigMap in one namespace.
type ConfigMapRecordStore struct {
	Client    client.Client
	Namespace string
}

// NewRecordStore builds a store over the installation's namespace.
func NewRecordStore(c client.Client, namespace string) *ConfigMapRecordStore {
	return &ConfigMapRecordStore{Client: c, Namespace: namespace}
}

// Load reads the record.
func (s *ConfigMapRecordStore) Load(ctx context.Context) (*Record, bool, error) {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: s.Namespace, Name: RecordName}
	if err := s.Client.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading the migration record: %w", err)
	}

	record := &Record{resourceVersion: cm.ResourceVersion}
	kube := statemachine.KubeSnapshot{State: cm.Data[recordKeyPhase]}
	if raw := cm.Data[recordKeyDeadline]; raw != "" {
		var deadline metav1.Time
		if err := deadline.UnmarshalJSON([]byte(`"` + raw + `"`)); err != nil {
			return nil, false, fmt.Errorf("the migration record's deadline is not a timestamp: %w", err)
		}
		kube.Deadline = &deadline
	}
	record.Snapshot = statemachine.FromKube[Phase](kube)
	record.Step = ID(cm.Data[recordKeyStep])

	if raw := cm.Data[recordKeyPreflight]; raw != "" {
		var attestation PreflightAttestation
		if err := json.Unmarshal([]byte(raw), &attestation); err != nil {
			return nil, false, fmt.Errorf("the migration record's preflight attestation is unreadable: %w", err)
		}
		record.Preflight = &attestation
	}
	if raw := cm.Data[recordKeyCapture]; raw != "" {
		record.Capture = json.RawMessage(raw)
	}
	return record, true, nil
}

// Save writes the record back, holding the optimistic lock. A record loaded and
// then saved after another run has written reports a conflict rather than
// overwriting, which is what keeps two concurrent migrations from both
// believing they hold the cluster.
func (s *ConfigMapRecordStore) Save(ctx context.Context, record *Record) error {
	data, err := record.data()
	if err != nil {
		return err
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            RecordName,
			Namespace:       s.Namespace,
			ResourceVersion: record.resourceVersion,
		},
		Data: data,
	}

	if record.resourceVersion == "" {
		if err := s.Client.Create(ctx, cm); err != nil {
			return fmt.Errorf("creating the migration record: %w", err)
		}
	} else if err := s.Client.Update(ctx, cm); err != nil {
		return fmt.Errorf("writing the migration record: %w", err)
	}
	record.resourceVersion = cm.ResourceVersion
	return nil
}

// Delete removes the record, which a successful migration does so that a later
// run starts from the cluster rather than from a completed position.
func (s *ConfigMapRecordStore) Delete(ctx context.Context) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: RecordName, Namespace: s.Namespace}}
	if err := s.Client.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("removing the migration record: %w", err)
	}
	return nil
}

// data renders the record as the ConfigMap's keys.
func (r *Record) data() (map[string]string, error) {
	kube := statemachine.ToKube(r.Snapshot)

	data := map[string]string{recordKeyPhase: kube.State}
	if kube.Deadline != nil {
		encoded, err := kube.Deadline.MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("encoding the phase deadline: %w", err)
		}
		// MarshalJSON quotes the timestamp, and a ConfigMap value is a string
		// rather than a JSON document.
		data[recordKeyDeadline] = string(encoded[1 : len(encoded)-1])
	}
	if r.Step != "" {
		data[recordKeyStep] = string(r.Step)
	}
	if r.Preflight != nil {
		encoded, err := json.Marshal(r.Preflight)
		if err != nil {
			return nil, fmt.Errorf("encoding the preflight attestation: %w", err)
		}
		data[recordKeyPreflight] = string(encoded)
	}
	if len(r.Capture) > 0 {
		data[recordKeyCapture] = string(r.Capture)
	}
	return data, nil
}
