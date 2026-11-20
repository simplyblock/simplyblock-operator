// Tests for the key inventory. What matters is that a conflict is found where
// one exists and not invented where none does, since the second kind of mistake
// blocks an upgrade on an object that is perfectly fine.

package keys

import (
	"strings"
	"testing"
)

// backupPolicy is a row with a legacy spelling, which is the awkward case.
func backupPolicy() Key {
	for _, key := range Moved() {
		if key.Name == "backup-policy" {
			return key
		}
	}
	panic("backup-policy is no longer in the inventory")
}

// poolFamily is the prefixed row.
func poolFamily() Key {
	for _, key := range Moved() {
		if key.Name == "pool." {
			return key
		}
	}
	panic("the pool key family is no longer in the inventory")
}

func TestKey_SpellsBothPrefixes(t *testing.T) {
	key := backupPolicy()

	if got := key.Old(); got != "simplyblock.io/backup-policy" {
		t.Errorf("Old = %q", got)
	}
	if got := key.New(); got != "storage.simplyblock.io/backup-policy" {
		t.Errorf("New = %q", got)
	}
	if got := key.LegacyOld(); got != "simplybk/backup-policy" {
		t.Errorf("LegacyOld = %q", got)
	}
}

func TestKey_NoLegacySpellingIsEmptyRatherThanAPrefixAlone(t *testing.T) {
	// A row with no legacy name must not report the bare prefix, which would
	// match nothing and read as a key.
	key := Key{Name: "guardian-disable"}
	if got := key.LegacyOld(); got != "" {
		t.Fatalf("LegacyOld = %q, want empty", got)
	}
}

func TestConflicts_TwoSpellingsDisagreeing(t *testing.T) {
	conflicts := backupPolicy().Conflicts(nil, map[string]string{
		"simplyblock.io/backup-policy":         "nightly",
		"storage.simplyblock.io/backup-policy": "hourly",
	})

	if len(conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1: the two spellings disagree and the "+
			"rewrite cannot choose", len(conflicts))
	}
	if conflicts[0].OldValue != "nightly" || conflicts[0].NewValue != "hourly" {
		t.Fatalf("conflict = %+v, want both values so a user can pick", conflicts[0])
	}
}

func TestConflicts_TwoSpellingsAgreeingIsNotAConflict(t *testing.T) {
	// An object the rewrite has already reached, or one somebody wrote both
	// spellings on deliberately. There is nothing to decide, and reporting it
	// would make the rewrite fail its own second run.
	conflicts := backupPolicy().Conflicts(nil, map[string]string{
		"simplyblock.io/backup-policy":         "nightly",
		"storage.simplyblock.io/backup-policy": "nightly",
	})

	if len(conflicts) != 0 {
		t.Fatalf("got %d conflicts on an object whose spellings agree", len(conflicts))
	}
}

func TestConflicts_OnlyTheOldSpellingIsNotAConflict(t *testing.T) {
	conflicts := backupPolicy().Conflicts(nil, map[string]string{
		"simplyblock.io/backup-policy": "nightly",
	})

	if len(conflicts) != 0 {
		t.Fatalf("got %d conflicts on an object carrying one spelling, which is "+
			"every object that has not been migrated", len(conflicts))
	}
}

func TestConflicts_TheLegacySpellingCountsAsAnOldOne(t *testing.T) {
	// A cluster old enough to carry simplybk/ has three spellings of one thing,
	// and the legacy one disagreeing with the new one is the same problem.
	conflicts := backupPolicy().Conflicts(nil, map[string]string{
		"simplybk/backup-policy":               "nightly",
		"storage.simplyblock.io/backup-policy": "hourly",
	})

	if len(conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1", len(conflicts))
	}
	if conflicts[0].OldKey != "simplybk/backup-policy" {
		t.Fatalf("OldKey = %q, want the legacy spelling that was found", conflicts[0].OldKey)
	}
}

func TestConflicts_ALegacyNameThatDiffersStillPairsCorrectly(t *testing.T) {
	// simplybk/qos-rw-mbytes became simplyblock.io/qos-rw-mbps, so the new key
	// cannot be derived by swapping the prefix.
	var qos Key
	for _, key := range Moved() {
		if key.Name == "qos-rw-mbps" {
			qos = key
		}
	}

	conflicts := qos.Conflicts(nil, map[string]string{
		"simplybk/qos-rw-mbytes":             "100",
		"storage.simplyblock.io/qos-rw-mbps": "200",
	})
	if len(conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1: a renamed legacy key still pairs with "+
			"the row's new spelling", len(conflicts))
	}
	if conflicts[0].NewKey != "storage.simplyblock.io/qos-rw-mbps" {
		t.Fatalf("NewKey = %q", conflicts[0].NewKey)
	}
}

func TestConflicts_APrefixedFamilyPairsBySuffix(t *testing.T) {
	conflicts := poolFamily().Conflicts(map[string]string{
		"simplyblock.io/pool.ns.cluster-a.gold":           "allowed",
		"storage.simplyblock.io/pool.ns.cluster-a.gold":   "denied",
		"simplyblock.io/pool.ns.cluster-a.silver":         "allowed",
		"storage.simplyblock.io/pool.ns.cluster-a.silver": "allowed",
	}, nil)

	if len(conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1: only the gold pool disagrees\n%+v",
			len(conflicts), conflicts)
	}
	if !strings.HasSuffix(conflicts[0].OldKey, "gold") {
		t.Fatalf("OldKey = %q, want the pool whose spellings disagree", conflicts[0].OldKey)
	}
}

func TestConflicts_LabelsAndAnnotationsAreBothExamined(t *testing.T) {
	key := backupPolicy()

	labels := map[string]string{
		"simplyblock.io/backup-policy":         "a",
		"storage.simplyblock.io/backup-policy": "b",
	}
	if got := key.Conflicts(labels, nil); len(got) != 1 {
		t.Fatalf("a conflict in the labels was not found")
	}
	if got := key.Conflicts(nil, labels); len(got) != 1 {
		t.Fatalf("a conflict in the annotations was not found")
	}
}

func TestConflicts_AnObjectWithNoMetadataIsFine(t *testing.T) {
	if got := backupPolicy().Conflicts(nil, nil); len(got) != 0 {
		t.Fatalf("got %d conflicts on an object carrying nothing", len(got))
	}
}

func TestMoved_EveryRowIsDistinctAndSpelledUnderBothPrefixes(t *testing.T) {
	seen := make(map[string]bool)
	for _, key := range Moved() {
		if key.Name == "" {
			t.Error("a row has no name")
			continue
		}
		if seen[key.Name] {
			t.Errorf("%q is in the inventory twice, and the check would report it twice", key.Name)
		}
		seen[key.Name] = true

		if !strings.HasPrefix(key.Old(), OldPrefix) || !strings.HasPrefix(key.New(), NewPrefix) {
			t.Errorf("%q does not spell under both prefixes", key.Name)
		}
		if key.Carried == "" {
			t.Errorf("%q says nothing about what carries it, so a report cannot "+
				"tell a user where to look", key.Name)
		}
	}
}
