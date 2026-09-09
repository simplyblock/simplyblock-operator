// The inventory, and how a key is matched on an object.
//
// The rows are read from the code rather than from the design, which counts the
// keys without listing them. Some of them have already half moved: the chart
// writes simplyblock.io/replication-policy while the operator reads
// storage.simplyblock.io/replication-policy, so a claim can already carry both,
// which is exactly the state §19.10's sixth check exists to find.

package keys

import (
	"maps"
	"slices"
	"strings"
)

// The two prefixes. The move is from one to the other, and nothing else about a
// key changes.
const (
	// OldPrefix is the bare prefix that shipped.
	OldPrefix = "simplyblock.io/"

	// NewPrefix is what the target model uses, matching the API group.
	NewPrefix = "storage.simplyblock.io/"

	// LegacyPrefix predates OldPrefix and is still honored on a few keys. A
	// cluster old enough to carry it carries a third spelling of the same
	// thing, so it is matched with the rest rather than left for a reader to
	// discover.
	LegacyPrefix = "simplybk/"
)

// Key is one annotation or label key that moves prefix.
type Key struct {
	// Name is the part after the prefix, which the move does not change.
	Name string

	// Legacy is the name this key had under [LegacyPrefix], where that differs
	// from Name or where the legacy spelling exists at all. It is empty for a
	// key that never had one.
	Legacy string

	// Prefixed marks a key whose Name is the start of a family rather than a
	// whole key, such as simplyblock.io/pool.<namespace>.<cluster>.<pool>.
	// Matching one means comparing the suffixes as well.
	Prefixed bool

	// Carried says what objects hold it, for a report that has to tell a user
	// where to look.
	Carried string
}

// Old is the spelling that shipped.
func (k Key) Old() string { return OldPrefix + k.Name }

// New is the spelling the target model uses.
func (k Key) New() string { return NewPrefix + k.Name }

// LegacyOld is the pre-OldPrefix spelling, and the empty string for a key that
// never had one.
func (k Key) LegacyOld() string {
	if k.Legacy == "" {
		return ""
	}
	return LegacyPrefix + k.Legacy
}

// Moved is the inventory.
//
// It holds twenty-seven keys where §9.4 of design-crd-model.md counts
// twenty-eight. The difference is not reconciled and the list is what the code
// actually writes, so a key found later is added here rather than the count
// being trusted over the grep that produced it.
func Moved() []Key {
	return []Key{
		{Name: "auto-restart-on-pathloss", Carried: "a StorageClass or a claim"},
		{Name: "backup-policy", Legacy: "backup-policy", Carried: "a StorageClass or a claim"},
		{Name: "component", Carried: "the objects the chart installs"},
		{Name: "drain-node", Carried: "a storage or control-plane Pod"},
		{Name: "fdb-node", Carried: "a FoundationDB Pod"},
		{Name: "fio-baseline", Carried: "a worker Node"},
		{Name: "fio-baseline-node", Carried: "a worker Node"},
		{Name: "guardian-disable", Carried: "a claim"},
		{Name: "host-id", Legacy: "host-id", Carried: "a claim"},
		{Name: "lvol-id", Legacy: "lvol-id", Carried: "a PersistentVolume"},
		{Name: "lvol-protection", Carried: "a PersistentVolume or claim, as a finalizer"},
		{Name: "managed-by", Carried: "the objects a controller creates"},
		{Name: "nvmf-model-id", Legacy: "nvmf-model-id", Carried: "a StorageClass"},
		{Name: "placement-hint", Carried: "a claim"},
		{Name: "pod-affinity", Carried: "a claim"},
		{Name: "pool", Carried: "a PersistentVolume"},
		{Name: "pool.", Prefixed: true, Carried: "a worker Node, one key per pool"},
		{Name: "qos-r-mbps", Legacy: "qos-r-mbytes", Carried: "a StorageClass"},
		{Name: "qos-rw-iops", Legacy: "qos-rw-iops", Carried: "a StorageClass"},
		{Name: "qos-rw-mbps", Legacy: "qos-rw-mbytes", Carried: "a StorageClass"},
		{Name: "qos-w-mbps", Legacy: "qos-w-mbytes", Carried: "a StorageClass"},
		{Name: "replication-policy", Carried: "a StorageClass or a claim"},
		{Name: "selected-storage-node", Carried: "a claim"},
		{Name: "selected-storage-node-applied", Carried: "a claim"},
		{Name: "selected-storage-node-rejected", Carried: "a claim"},
		{Name: "simplyblock-rebalancer-injected", Carried: "a Pod"},
		{Name: "storage", Carried: "a Node or a Pod"},
		{Name: "storage-node-uuid.", Prefixed: true, Carried: "a worker Node, one key per slot"},
		{Name: "trigger-realignment", Carried: "a StorageCluster"},
		{Name: "volume-handle", Carried: "a PersistentVolume"},
	}
}

// Conflict is one key an object carries under two spellings that disagree.
type Conflict struct {
	// Key is the inventory row.
	Key Key

	// OldKey and NewKey are the two keys as they appear on the object, which
	// for a prefixed row are the full keys rather than the family.
	OldKey string
	NewKey string

	// OldValue and NewValue are what they hold.
	OldValue string
	NewValue string
}

// Conflicts reports the keys this object carries under two spellings holding
// two different values.
//
// Two spellings holding one value are not reported. That is an object the
// rewrite has already reached, or one a user wrote both spellings on
// deliberately, and either way there is nothing to decide. The rewrite is
// idempotent for the same reason.
//
// The result is ordered by the old key, so two runs over one object report the
// same thing in the same order.
func (k Key) Conflicts(labels, annotations map[string]string) []Conflict {
	out := make([]Conflict, 0, 2)
	for _, held := range []map[string]string{labels, annotations} {
		out = append(out, k.conflictsIn(held)...)
	}
	slices.SortFunc(out, func(a, b Conflict) int { return strings.Compare(a.OldKey, b.OldKey) })
	return out
}

// conflictsIn examines one map.
func (k Key) conflictsIn(held map[string]string) []Conflict {
	if len(held) == 0 {
		return nil
	}

	var out []Conflict
	for _, oldKey := range k.oldKeysIn(held) {
		newKey := NewPrefix + strings.TrimPrefix(strings.TrimPrefix(oldKey, OldPrefix), LegacyPrefix)
		if k.Legacy != "" && strings.HasPrefix(oldKey, LegacyPrefix) {
			// The legacy spelling may carry a different name from the current
			// one, so the new key is derived from the row rather than from the
			// key that was found.
			newKey = k.New()
		}

		newValue, carried := held[newKey]
		if !carried || newValue == held[oldKey] {
			continue
		}
		out = append(out, Conflict{
			Key:      k,
			OldKey:   oldKey,
			NewKey:   newKey,
			OldValue: held[oldKey],
			NewValue: newValue,
		})
	}
	return out
}

// oldKeysIn returns the keys on the object this row matches under an older
// spelling, sorted so the walk is deterministic.
func (k Key) oldKeysIn(held map[string]string) []string {
	var out []string
	if k.Prefixed {
		for key := range maps.Keys(held) {
			if strings.HasPrefix(key, k.Old()) {
				out = append(out, key)
			}
		}
		slices.Sort(out)
		return out
	}

	for _, candidate := range []string{k.Old(), k.LegacyOld()} {
		if candidate == "" {
			continue
		}
		if _, carried := held[candidate]; carried {
			out = append(out, candidate)
		}
	}
	return out
}
