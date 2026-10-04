// The annotation and label keys this product puts on Kubernetes objects, each
// in every spelling it has ever had.
//
// The keys are moving from the bare simplyblock.io/ prefix to the API group's
// own storage.simplyblock.io/, which is design-crd-model.md §9.4. A flag day is
// not available: the keys sit on PersistentVolumes, claims, StorageClasses, and
// worker Nodes that outlive every operator upgrade, and some of them a person
// typed. So the move is done the only way it can be — **every spelling is read,
// and one is written** — and this type is what makes that a property of the key
// rather than of whichever call site somebody remembered to update.
//
// It is here rather than in a consumer because the operator writes these keys
// and the CSI driver reads them. Two inventories would be two answers to what a
// claim is annotated with, and the answer that mattered would be whichever
// process looked.

package kube

// The prefixes a key has been spelled with.
const (
	// GroupPrefix is the API group's own, which every key is written under.
	GroupPrefix = "storage.simplyblock.io/"

	// BarePrefix is what shipped, and what an object written before the move
	// still carries.
	BarePrefix = "simplyblock.io/"

	// LegacyPrefix predates BarePrefix and survives on a handful of keys. A
	// cluster old enough to carry it carries a third spelling of one thing.
	LegacyPrefix = "simplybk/"
)

// Key is one annotation or label, in every spelling it is read under, newest
// first. The first is the one that is written.
//
// The order is the whole of the precedence rule: an object carrying two
// spellings is answered by the newer, because that is the one this product
// wrote and the older is what it is migrating away from.
type Key []string

// String is the spelling to write. A Key with no spellings yields the empty
// string, which is a programming error rather than a state to handle: every Key
// in this package is a literal.
func (k Key) String() string {
	if len(k) == 0 {
		return ""
	}
	return k[0]
}

// Get returns the value under the newest spelling the object carries, and
// whether it carries any.
//
// Presence rather than non-emptiness decides, because an empty value is a value:
// a key set to the empty string is how a toggle is turned off, and reading past
// it to an older spelling would answer with something the object has stopped
// saying.
func (k Key) Get(values map[string]string) (string, bool) {
	for _, spelling := range k {
		if value, carried := values[spelling]; carried {
			return value, true
		}
	}
	return "", false
}

// Has reports whether the object carries this key under any spelling.
func (k Key) Has(values map[string]string) bool {
	_, carried := k.Get(values)
	return carried
}

// Set writes the current spelling and removes every older one, returning the
// map so a caller can assign it back where there was none.
//
// Clearing the old spellings is what stops an object this product has touched
// from carrying two answers to one question — which is the state §19.10's sixth
// check exists to find, and one a write that only added would keep creating.
func (k Key) Set(values map[string]string, value string) map[string]string {
	if values == nil {
		values = map[string]string{}
	}
	k.Delete(values)
	if current := k.String(); current != "" {
		values[current] = value
	}
	return values
}

// Delete removes every spelling.
//
// Every one, because removing only the current spelling would leave an older
// one behind and the next read would answer with the value the delete was meant
// to retract.
func (k Key) Delete(values map[string]string) {
	for _, spelling := range k {
		delete(values, spelling)
	}
}

// Spellings flattens keys into every string they are read under, newest of each
// first, for a caller whose interface takes strings: a removal that patches the
// keys to null, or a lookup that walks an ordered list.
func Spellings(keys ...Key) []string {
	var out []string
	for _, key := range keys {
		out = append(out, key...)
	}
	return out
}

// The keys that are moving. Each is declared with the group prefix first, so
// that writing it is writing the new spelling and reading it is reading
// whichever the object has.
//
// The bare constants beside them in names.go are the same strings, kept because
// a caller that only writes has no use for the list. Where a caller reads, it
// reads through the Key.
var (
	// KeyVolumeHandle records a volume's normalized handle (§16.4).
	KeyVolumeHandle = Key{AnnoVolumeHandle, BarePrefix + "volume-handle"}

	// KeyPool records the source pool on a PersistentVolume, for observability.
	KeyPool = Key{AnnoPool, BarePrefix + "pool"}

	// KeySelectedStorageNode is the pin: the storage node a claim's volume is
	// held on. A person writes this one, which is why the old spelling has to
	// keep working for as long as claims carrying it exist.
	KeySelectedStorageNode = Key{
		AnnoSelectedStorageNode, BarePrefix + "selected-storage-node",
	}

	// KeySelectedStorageNodeApplied records the pin the controller has acted
	// on, which is the diff that stops its own write re-triggering a migration.
	KeySelectedStorageNodeApplied = Key{
		AnnoSelectedStorageNodeApplied, BarePrefix + "selected-storage-node-applied",
	}

	// KeySelectedStorageNodeRejected records a pin the controller refused, so a
	// warning about it is not repeated every reconcile.
	KeySelectedStorageNodeRejected = Key{
		AnnoSelectedStorageNodeRejected, BarePrefix + "selected-storage-node-rejected",
	}

	// KeyPlacementHint is the one-shot placement the webhook chose, which the
	// CSI controller consumes and removes.
	KeyPlacementHint = Key{AnnoPlacementHint, BarePrefix + "placement-hint"}

	// KeyHostID is the pre-pin placement annotation, honored as the lowest
	// priority fallback and never rewritten. It carries three spellings because
	// it was renamed once before the group prefix was settled.
	KeyHostID = Key{AnnoHostID, BarePrefix + "host-id", LegacyPrefix + "host-id"}

	// KeyFinalizer guards a PersistentVolume or claim while its logical volume
	// still exists.
	KeyFinalizer = Key{Finalizer, BarePrefix + "lvol-protection"}
)

// MovedKeys is the inventory, which is what lets a test assert the move as a
// property of the product rather than of one key.
func MovedKeys() []Key {
	return []Key{
		KeyVolumeHandle,
		KeyPool,
		KeySelectedStorageNode,
		KeySelectedStorageNodeApplied,
		KeySelectedStorageNodeRejected,
		KeyPlacementHint,
		KeyHostID,
		KeyFinalizer,
	}
}
