// Where a conversion puts what the other version cannot hold.
//
// A conversion is a representation change and nothing else, and the two shapes
// are rarely the same size: a field the hub declares may have no counterpart
// here, and a field this version declares may have none there. Dropping either
// one is what design-api-upgrade.md §6.2 forbids, because the object is read
// and written back by clients of both versions and a drop truncates it
// silently:
//
//	Information that cannot be represented in both versions MUST NOT silently
//	disappear. Where a field has no counterpart, the conversion preserves it in
//	an annotation keyed storage.simplyblock.io/conversion-<field> on the way
//	down and restores it on the way up, so a v1alpha1 client that reads and
//	writes an object back does not truncate it.
//
// The stash runs in both directions, because the loss does. While an upgrade
// holds storage at v1alpha1, a controller writing v1alpha2 has its object
// converted down to be stored and back up on the next read, so a hub-only field
// is stashed going down and taken back going up. Once the storage rewrite flips
// the version the same thing happens to a v1alpha1-only field in the opposite
// direction. One mechanism covers both, and the field names say which way.
//
// An annotation rather than a field added to the older version, and the
// difference matters: this version is being retired, and growing its schema to
// carry the newer one's state would put a field in a CRD nothing will serve for
// long, in a shape no controller of this version ever wrote.
//
// What is not stashed is a distinction that carries no information. A group the
// hub states as a pointer, present but with every field empty, converts down to
// nothing and comes back absent, and that is the intended reading rather than a
// loss: an empty group says nothing an absent one does not. The same argument
// is made at greater length in controlplane_conversion.go, for the empty
// managed-source block.

package v1alpha1

import (
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// conversionStashPrefix is the key every stashed field is written under. The
// field name follows it, spelled as the JSON path the value came from, so that
// somebody reading `kubectl get -o yaml` can tell what the annotation is
// standing in for.
const conversionStashPrefix = "storage.simplyblock.io/conversion-"

// stashString records a value the other version has no field for. An empty
// value stashes nothing and clears any earlier stash, so a field that was set
// and then cleared does not come back on the next read.
func stashString(meta *metav1.ObjectMeta, field, value string) {
	key := conversionStashPrefix + field
	if value == "" {
		delete(meta.Annotations, key)
		dropEmptyAnnotations(meta)
		return
	}
	if meta.Annotations == nil {
		meta.Annotations = map[string]string{}
	}
	meta.Annotations[key] = value
}

// dropEmptyAnnotations replaces a map the stash has emptied with no map at all.
//
// Without it, an object that carried nothing but a stash comes back from a round
// trip with an empty map where it had none, and an object that never carried
// anything gains one on the way through. Neither is a difference that says
// something: an empty annotation map and an absent one are the same statement,
// which is the reading this file's header takes for an empty group as well.
func dropEmptyAnnotations(meta *metav1.ObjectMeta) {
	if len(meta.Annotations) == 0 {
		meta.Annotations = nil
	}
}

// takeString reads a stashed value and removes it, because the value's home is
// the field it is being restored into and leaving the annotation behind would
// show the same fact twice and let the two disagree.
func takeString(meta *metav1.ObjectMeta, field string) string {
	key := conversionStashPrefix + field
	value, stashed := meta.Annotations[key]
	if !stashed {
		return ""
	}
	delete(meta.Annotations, key)
	dropEmptyAnnotations(meta)
	return value
}

// stashJSON records a value that is not a string. It is used for the structured
// fields one version carries and the other does not, where there is no spelling
// of the value an annotation could hold directly.
//
// A value that cannot be encoded is dropped rather than failing the conversion.
// Refusing here would make the object unreadable at the other version, which is
// a worse outcome than losing a field the encoder could not represent, and
// every value this is called with is a plain Go structure that always encodes.
func stashJSON(meta *metav1.ObjectMeta, field string, value any) {
	encoded, err := json.Marshal(value)
	if err != nil || isEmptyJSON(encoded) {
		stashString(meta, field, "")
		return
	}
	stashString(meta, field, string(encoded))
}

// isEmptyJSON reports an encoding that carries no value. A nil slice encodes as
// a JSON null and an empty one as an empty array, and stashing either would put
// a key on every object of the kind for a field none of them has set.
func isEmptyJSON(encoded []byte) bool {
	switch string(encoded) {
	case "null", "[]", "{}", `""`:
		return true
	default:
		return false
	}
}

// takeJSON reads a stashed structured value into out and removes the
// annotation. A value that cannot be decoded is reported, because unlike an
// encode failure it means somebody edited the annotation by hand and the caller
// has to decide whether to care.
func takeJSON(meta *metav1.ObjectMeta, field string, out any) error {
	encoded := takeString(meta, field)
	if encoded == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(encoded), out); err != nil {
		return fmt.Errorf("decode the stashed %s: %w", field, err)
	}
	return nil
}

// stashInt64 and takeInt64 are the numeric pair. Zero stashes nothing, which is
// the same reading omitempty gives the fields these carry.
func stashInt64(meta *metav1.ObjectMeta, field string, value int64) {
	if value == 0 {
		stashString(meta, field, "")
		return
	}
	stashString(meta, field, fmt.Sprintf("%d", value))
}

func takeInt64(meta *metav1.ObjectMeta, field string) int64 {
	encoded := takeString(meta, field)
	if encoded == "" {
		return 0
	}
	var value int64
	if _, err := fmt.Sscanf(encoded, "%d", &value); err != nil {
		return 0
	}
	return value
}
