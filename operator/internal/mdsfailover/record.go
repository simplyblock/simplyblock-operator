// The active record of a storage cluster's metadata server: which pod is
// active, which boot of it, and the epoch of its promotion
// (design-pnfs-mds-standby.md §4.3).
//
// It lives in this package, beside the decisions that write it, because the
// record is the takeover's write-ahead. Once it names a new active, every later
// pass promotes that pod at that epoch until it is active.

package mdsfailover

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/simplyblock/simplyblock-operator/internal/mdsstate"
)

const (
	// AnnotationActive names the active pod, and is empty before the first
	// promotion.
	AnnotationActive = mdsstate.ActiveAnnotation
	// AnnotationActiveInstance is the active's pod UID and runner container ID,
	// in the assembledBy format.
	AnnotationActiveInstance = mdsstate.ActiveInstanceAnnotation
	// AnnotationEpoch is the current epoch, a decimal integer that is 1 at the
	// first promotion.
	AnnotationEpoch = mdsstate.EpochAnnotation
	// AnnotationStateVolume is the state volume's handle. The workload writes
	// it and this package only reads it.
	AnnotationStateVolume = mdsstate.StateVolumeAnnotation
)

// ErrConflict is a record write that lost to another writer. The loser does not
// promote.
var ErrConflict = errors.New("the active record moved")

// Record is the active record.
type Record struct {
	Active   string
	Instance string
	Epoch    uint64
}

// Empty reports whether no promotion has been recorded yet.
func (r Record) Empty() bool { return r.Active == "" }

// ParseRecord reads the record from a StatefulSet's annotations. A missing epoch
// is 0. One that is not a decimal integer is an error: a record a user can edit
// is not trusted to be well formed.
func ParseRecord(annotations map[string]string) (Record, error) {
	rec := Record{
		Active:   annotations[AnnotationActive],
		Instance: annotations[AnnotationActiveInstance],
	}
	if raw := annotations[AnnotationEpoch]; raw != "" {
		epoch, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return Record{}, fmt.Errorf("annotation %s is %q, not an epoch: %w", AnnotationEpoch, raw, err)
		}
		rec.Epoch = epoch
	}
	return rec, nil
}

// apply writes the record's three annotations into m.
func (r Record) apply(m map[string]string) {
	m[AnnotationActive] = r.Active
	m[AnnotationActiveInstance] = r.Instance
	m[AnnotationEpoch] = strconv.FormatUint(r.Epoch, 10)
}

// Store writes the record. The write is conditioned on the resourceVersion the
// decision was made on, and a write that lost returns ErrConflict. It returns
// the version the write produced, so a second write in the same pass can be
// conditioned on it.
type Store interface {
	Write(ctx context.Context, version string, rec Record) (string, error)
}

// KubeStore is the Store over the StatefulSet's annotations.
type KubeStore struct {
	Client client.Client
	Key    client.ObjectKey
}

// Write updates the three annotations in one update. It refuses early when the
// StatefulSet is no longer at version, and the update's own resourceVersion
// refuses a write that raced the read.
func (s *KubeStore) Write(ctx context.Context, version string, rec Record) (string, error) {
	var sts appsv1.StatefulSet
	if err := s.Client.Get(ctx, s.Key, &sts); err != nil {
		return "", fmt.Errorf("reading StatefulSet %s: %w", s.Key.Name, err)
	}
	if sts.ResourceVersion != version {
		return "", ErrConflict
	}
	if sts.Annotations == nil {
		sts.Annotations = map[string]string{}
	}
	rec.apply(sts.Annotations)
	if err := s.Client.Update(ctx, &sts); err != nil {
		if apierrors.IsConflict(err) {
			return "", ErrConflict
		}
		return "", fmt.Errorf("writing the active record of %s: %w", s.Key.Name, err)
	}
	return sts.ResourceVersion, nil
}
