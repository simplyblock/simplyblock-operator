// §16's application-level migration: the changes no CRD conversion can carry.
//
// Two of them are done here and the rest describe what they would do. The split
// is not about which are important. It is about which need a type that §29.1
// has not written. A kind that is renamed or absorbed needs its target type to
// be constructed, and every one of those targets is a v1alpha2 kind that does
// not exist. Rewriting an annotation key needs nothing but the key, and
// deleting a retired kind needs nothing at all.
//
// Every step here describes object by object either way, so the plan holds the
// claims, the policies, and the volumes a run would touch rather than a
// sentence about them.

package steps

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	atlaskube "github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/keys"
)

// The identities of §16's steps.
const (
	IDRewriteKeys        upgrade.ID = "rewrite-annotation-keys"
	IDDeleteBackupImport upgrade.ID = "delete-backup-imports"
	IDCopyBackupPolicies upgrade.ID = "copy-backup-policies"
	IDAbsorbMigrations   upgrade.ID = "absorb-volume-migrations"
	IDAbsorbRestores     upgrade.ID = "absorb-backup-restores"
	IDNormalizeHandles   upgrade.ID = "normalize-volume-handles"
)

// What the unimplemented halves wait on.
const (
	needsTargetTypes = "the v1alpha2 target type does not exist yet (§29.1), so the object cannot be constructed"
	needsResolver    = "resolving a pool name to its UUID needs the control-plane client of §16.4"
)

// Migrate returns §16's steps.
func Migrate() []upgrade.Step {
	return []upgrade.Step{
		rewriteKeys{},
		deleteBackupImports{},
		renameKind{
			described: described{id: IDCopyBackupPolicies, blocked: needsTargetTypes},
			summary:   "copies each BackupPolicy to a StorageBackupPolicy of the same name",
			from:      "BackupPolicy",
			to:        "StorageBackupPolicy",
		},
		renameKind{
			described: described{id: IDAbsorbMigrations, blocked: needsTargetTypes},
			summary:   "absorbs each VolumeMigration into a cluster-scoped PersistentVolumeOps",
			from:      "VolumeMigration",
			to:        "PersistentVolumeOps",
			as:        "Migrate",
		},
		renameKind{
			described: described{id: IDAbsorbRestores, blocked: needsTargetTypes},
			summary:   "absorbs each BackupRestore into a StorageBackupOps",
			from:      "BackupRestore",
			to:        "StorageBackupOps",
			as:        "Restore",
		},
		normalizeHandles{
			described: described{id: IDNormalizeHandles, blocked: needsResolver},
		},
	}
}

// rewriteKeys writes the new spelling of every §16.3 key an object carries.
//
// It is done rather than described because it needs nothing that does not
// exist. The inventory is data, the objects are discovered, and the write is
// additive: §16.3 leaves the old key in place for the deprecation window, so
// an operator still reading it keeps working and the step is safe to run
// before the release that stops.
type rewriteKeys struct{}

func (rewriteKeys) ID() upgrade.ID         { return IDRewriteKeys }
func (rewriteKeys) Stage() upgrade.Stage   { return upgrade.StageMigrate }
func (rewriteKeys) Phase() upgrade.Phase   { return upgrade.PhaseTransforming }
func (rewriteKeys) Requires() []upgrade.ID { return nil }

func (rewriteKeys) Description() string {
	return "writes the storage.simplyblock.io spelling of every key an object still carries under the old prefix"
}

// Describe reports the keys this object is missing the new spelling of.
func (r rewriteKeys) Describe(_ context.Context, _ *upgrade.Scope, subject upgrade.Subject) (*upgrade.Action, error) {
	missing := r.missing(subject)
	if len(missing) == 0 {
		return nil, nil
	}
	return &upgrade.Action{
		Rule:   IDRewriteKeys,
		Verb:   upgrade.VerbAnnotate,
		Object: subject.Ref,
		Detail: fmt.Sprintf("%d key(s): %s", len(missing), missing[0].oldKey+" → "+missing[0].newKey),
	}, nil
}

// Done claims an object that carries every new spelling it needs.
func (r rewriteKeys) Done(_ context.Context, _ *upgrade.Scope, subject upgrade.Subject) (bool, error) {
	if subject.IsUpgrade() || subject.Object == nil {
		return false, nil
	}
	return len(r.missing(subject)) == 0 && len(r.carried(subject)) > 0, nil
}

// Validate has nothing to refuse. The write adds a key beside one that is
// already there, so there is no state it can arrive in that makes it unsafe.
func (rewriteKeys) Validate(context.Context, *upgrade.Scope, upgrade.Subject) error { return nil }

// Apply writes the new keys, preserving each value verbatim.
func (r rewriteKeys) Apply(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	obj, ok := subject.Object.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("%T is not a Kubernetes object", subject.Object)
	}

	labels, annotations := obj.GetLabels(), obj.GetAnnotations()
	for _, rewrite := range r.missing(subject) {
		if rewrite.inLabels {
			labels = withKey(labels, rewrite.newKey, rewrite.value)
			continue
		}
		annotations = withKey(annotations, rewrite.newKey, rewrite.value)
	}
	obj.SetLabels(labels)
	obj.SetAnnotations(annotations)

	if err := s.Client.Update(ctx, obj); err != nil {
		return fmt.Errorf("writing the new keys: %w", err)
	}
	s.Adopt(obj)
	return nil
}

// Verify re-reads the object and checks the new spellings are on it.
func (r rewriteKeys) Verify(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	fresh, ok := subject.Object.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("%T is not a Kubernetes object", subject.Object)
	}
	if err := s.Client.Get(ctx, subject.Ref.Key(), fresh); err != nil {
		return fmt.Errorf("re-reading it: %w", err)
	}

	for _, rewrite := range r.carried(upgrade.Subject{Ref: subject.Ref, Object: fresh}) {
		if !rewrite.mirrored {
			return fmt.Errorf("%s was not written", rewrite.newKey)
		}
	}
	return nil
}

// rewrite is one key an object carries, and whether the new spelling is beside
// it.
type rewrite struct {
	oldKey   string
	newKey   string
	value    string
	inLabels bool
	mirrored bool
}

// carried reports every §16.3 key this object holds under an older spelling.
func (rewriteKeys) carried(subject upgrade.Subject) []rewrite {
	if subject.IsUpgrade() || subject.Object == nil {
		return nil
	}

	var out []rewrite
	for _, key := range keys.Moved() {
		for _, held := range []struct {
			values   map[string]string
			inLabels bool
		}{
			{subject.Object.GetLabels(), true},
			{subject.Object.GetAnnotations(), false},
		} {
			for _, found := range key.Carrying(held.values) {
				_, mirrored := held.values[found.NewKey]
				out = append(out, rewrite{
					oldKey:   found.OldKey,
					newKey:   found.NewKey,
					value:    found.Value,
					inLabels: held.inLabels,
					mirrored: mirrored,
				})
			}
		}
	}
	return out
}

// missing narrows that to the ones whose new spelling is not there yet.
func (r rewriteKeys) missing(subject upgrade.Subject) []rewrite {
	var out []rewrite
	for _, found := range r.carried(subject) {
		if !found.mirrored {
			out = append(out, found)
		}
	}
	return out
}

// withKey sets a key on a map that may be nil.
func withKey(values map[string]string, key, value string) map[string]string {
	if values == nil {
		values = map[string]string{}
	}
	values[key] = value
	return values
}

// deleteBackupImports removes the kind §16.2 retires without replacing it,
// because the object store is the inventory and the objects describe nothing
// the target model keeps.
type deleteBackupImports struct{}

func (deleteBackupImports) ID() upgrade.ID         { return IDDeleteBackupImport }
func (deleteBackupImports) Stage() upgrade.Stage   { return upgrade.StageMigrate }
func (deleteBackupImports) Phase() upgrade.Phase   { return upgrade.PhaseDeleting }
func (deleteBackupImports) Requires() []upgrade.ID { return nil }

func (deleteBackupImports) Description() string {
	return "deletes each BackupImport, which §16.2 retires because the store is the inventory"
}

func (deleteBackupImports) Describe(_ context.Context, _ *upgrade.Scope, subject upgrade.Subject) (*upgrade.Action, error) {
	if subject.Ref.GVK.Kind != "BackupImport" {
		return nil, nil
	}
	return &upgrade.Action{
		Rule:   IDDeleteBackupImport,
		Verb:   upgrade.VerbDelete,
		Object: subject.Ref,
		Detail: "retired, and nothing replaces it",
	}, nil
}

// Done is false for an object the graph still holds. A deletion is the one
// change the graph cannot show, so the live read belongs in Verify.
func (deleteBackupImports) Done(context.Context, *upgrade.Scope, upgrade.Subject) (bool, error) {
	return false, nil
}

func (deleteBackupImports) Validate(context.Context, *upgrade.Scope, upgrade.Subject) error {
	return nil
}

func (deleteBackupImports) Apply(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	if err := s.Client.Delete(ctx, subject.Object); err != nil {
		return fmt.Errorf("deleting it: %w", err)
	}
	return nil
}

func (deleteBackupImports) Verify(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	fresh, ok := subject.Object.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("%T is not a Kubernetes object", subject.Object)
	}
	if err := s.Client.Get(ctx, subject.Ref.Key(), fresh); err == nil {
		return fmt.Errorf("it still exists")
	}
	return nil
}

// renameKind describes a kind copied to another kind, which §16.2 does four
// times. The copy is a create and a delete rather than a conversion, because no
// conversion webhook is invoked across kinds.
type renameKind struct {
	described

	summary string
	from    string
	to      string

	// as is the action the target's spec carries, for a kind absorbed into an
	// Ops rather than renamed.
	as string
}

func (r renameKind) ID() upgrade.ID       { return r.id }
func (r renameKind) Description() string  { return r.summary }
func (renameKind) Stage() upgrade.Stage   { return upgrade.StageMigrate }
func (renameKind) Phase() upgrade.Phase   { return upgrade.PhaseTransforming }
func (renameKind) Requires() []upgrade.ID { return nil }

// Describe names the object that would be created, which needs the source and
// not the target type.
func (r renameKind) Describe(_ context.Context, _ *upgrade.Scope, subject upgrade.Subject) (*upgrade.Action, error) {
	if subject.Ref.GVK.Kind != r.from {
		return nil, nil
	}

	detail := fmt.Sprintf("→ %s/%s", r.to, subject.Ref.Name)
	if r.as != "" {
		detail += fmt.Sprintf(", action %s", r.as)
	}
	return &upgrade.Action{
		Rule:   r.id,
		Verb:   upgrade.VerbCreate,
		Object: subject.Ref,
		Detail: detail,
	}, nil
}

// Done is false while the source object is still there. Whether the target
// exists is a question its implementation will ask of a kind that does not yet
// exist to be asked about.
func (renameKind) Done(context.Context, *upgrade.Scope, upgrade.Subject) (bool, error) {
	return false, nil
}

// normalizeHandles describes the volumes whose handle carries a pool name
// rather than a UUID.
//
// Detecting one needs no control plane: §16.4's shape is
// clusterID:poolID:volumeID, and lvol.ParseHandle accepts a pool segment that
// is not a UUID precisely because both spellings occur. What needs the control
// plane is the other half, which is the UUID the name resolves to.
type normalizeHandles struct {
	described
}

func (n normalizeHandles) ID() upgrade.ID       { return n.id }
func (normalizeHandles) Stage() upgrade.Stage   { return upgrade.StageMigrate }
func (normalizeHandles) Phase() upgrade.Phase   { return upgrade.PhaseHandles }
func (normalizeHandles) Requires() []upgrade.ID { return nil }

func (normalizeHandles) Description() string {
	return "records the normalized handle on each volume whose pool segment carries a name rather than a UUID"
}

func (n normalizeHandles) Describe(_ context.Context, _ *upgrade.Scope, subject upgrade.Subject) (*upgrade.Action, error) {
	handle, legacy := legacyHandle(subject)
	if !legacy || normalized(subject) {
		return nil, nil
	}
	return &upgrade.Action{
		Rule:   n.id,
		Verb:   upgrade.VerbAnnotate,
		Object: subject.Ref,
		Detail: fmt.Sprintf("pool %q is a name and not a UUID, and resolves into %s",
			handle.PoolRef, atlaskube.AnnoVolumeHandle),
	}, nil
}

// Done claims a volume this step is about and has already normalized.
func (normalizeHandles) Done(_ context.Context, _ *upgrade.Scope, subject upgrade.Subject) (bool, error) {
	_, legacy := legacyHandle(subject)
	return legacy && normalized(subject), nil
}

// normalized reports a volume that already carries the resolved handle.
func normalized(subject upgrade.Subject) bool {
	if subject.Object == nil {
		return false
	}
	_, carried := subject.Object.GetAnnotations()[atlaskube.AnnoVolumeHandle]
	return carried
}

// legacyHandle reports the handle of a PersistentVolume whose pool segment is
// not a UUID.
func legacyHandle(subject upgrade.Subject) (lvol.Handle, bool) {
	pv, ok := subject.Object.(*corev1.PersistentVolume)
	if !ok || !atlaskube.IsManaged(pv) {
		return lvol.Handle{}, false
	}

	raw, err := atlaskube.VolumeHandleFromPV(pv)
	if err != nil {
		return lvol.Handle{}, false
	}
	handle, wellFormed := lvol.ParseHandle(raw)
	if !wellFormed || lvol.IsCanonicalUUID(handle.PoolRef) {
		return lvol.Handle{}, false
	}
	return handle, true
}
