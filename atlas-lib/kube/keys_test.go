package kube

import (
	"strings"
	"testing"
)

// The property the whole type exists for: an object written before the move
// keeps working, and an object written after it carries one spelling.
func TestAKeyReadsEverySpellingAndWritesOne(t *testing.T) {
	key := Key{"storage.simplyblock.io/x", "simplyblock.io/x", "simplybk/x"}

	for _, tc := range []struct {
		name string
		on   map[string]string
		want string
	}{
		{name: "nothing", on: map[string]string{}},
		{name: "the current spelling", on: map[string]string{"storage.simplyblock.io/x": "a"}, want: "a"},
		{name: "the previous one", on: map[string]string{"simplyblock.io/x": "b"}, want: "b"},
		{name: "the oldest one", on: map[string]string{"simplybk/x": "c"}, want: "c"},
		{
			name: "the newest wins where two are carried",
			on: map[string]string{
				"storage.simplyblock.io/x": "new", "simplyblock.io/x": "old", "simplybk/x": "older",
			},
			want: "new",
		},
		{
			name: "and the newest of the two that are there",
			on:   map[string]string{"simplyblock.io/x": "old", "simplybk/x": "older"},
			want: "old",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, found := key.Get(tc.on)
			if got != tc.want {
				t.Errorf("Get = %q, want %q", got, tc.want)
			}
			if found != (tc.want != "") {
				t.Errorf("found = %v, want %v", found, tc.want != "")
			}
		})
	}
}

// An empty value is a value. A key set to the empty string is carried
// deliberately — it is how a user turns a toggle off — so reporting it as absent
// would read the next spelling down and answer with something the object stopped
// saying.
func TestAnEmptyValueIsStillTheAnswer(t *testing.T) {
	key := Key{"storage.simplyblock.io/x", "simplyblock.io/x"}

	got, found := key.Get(map[string]string{
		"storage.simplyblock.io/x": "", "simplyblock.io/x": "stale",
	})
	if !found || got != "" {
		t.Errorf("Get = %q, %v; want the empty current value to win over the old one",
			got, found)
	}
}

// Setting writes the current spelling and clears every older one, so an object
// the operator touches stops carrying two answers to one question.
func TestSetWritesOneSpellingAndClearsTheRest(t *testing.T) {
	key := Key{"storage.simplyblock.io/x", "simplyblock.io/x", "simplybk/x"}

	on := key.Set(map[string]string{"simplyblock.io/x": "old", "other": "untouched"}, "new")

	if got := on["storage.simplyblock.io/x"]; got != "new" {
		t.Errorf("the current spelling is %q, want %q", got, "new")
	}
	if _, stale := on["simplyblock.io/x"]; stale {
		t.Error("the old spelling survived the write, so the object carries two answers")
	}
	if on["other"] != "untouched" {
		t.Error("Set touched a key that is not this one")
	}
}

// Set on a nil map returns one rather than panicking, because the object it is
// called on often has no annotations yet.
func TestSetBuildsAMapWhereThereIsNone(t *testing.T) {
	key := Key{"storage.simplyblock.io/x"}
	if on := key.Set(nil, "v"); on["storage.simplyblock.io/x"] != "v" {
		t.Errorf("Set on a nil map produced %v", on)
	}
}

// Deleting removes every spelling. Removing only the current one would leave the
// old one behind, and the next read would answer with the value the delete was
// meant to retract.
func TestDeleteRemovesEverySpelling(t *testing.T) {
	key := Key{"storage.simplyblock.io/x", "simplyblock.io/x", "simplybk/x"}

	on := map[string]string{
		"storage.simplyblock.io/x": "a", "simplyblock.io/x": "b", "simplybk/x": "c", "other": "d",
	}
	key.Delete(on)

	if _, found := key.Get(on); found {
		t.Error("a spelling survived the delete, so the value it retracted is still readable")
	}
	if on["other"] != "d" {
		t.Error("Delete touched a key that is not this one")
	}
}

// The declared keys all move to the group's own prefix and are all read under
// the bare one they shipped with. The inventory is what makes the move a fact
// about the product rather than about whichever call site somebody remembered.
func TestEveryDeclaredKeyWritesTheGroupPrefixAndReadsTheOldOne(t *testing.T) {
	for _, key := range MovedKeys() {
		if len(key) < 2 {
			t.Errorf("%q is declared with one spelling, so nothing reads what it used to be "+
				"called and an object written before the move stops being understood", key)
			continue
		}
		if got := key.String(); !strings.HasPrefix(got, GroupPrefix) {
			t.Errorf("%q is written under %q, and the move is to %q", key, got, GroupPrefix)
		}
		var bare bool
		for _, spelling := range key[1:] {
			if strings.HasPrefix(spelling, BarePrefix) {
				bare = true
			}
		}
		if !bare {
			t.Errorf("%q reads no %q spelling, so an object written before the move is not "+
				"understood", key, BarePrefix)
		}
	}
}

// No two keys share a spelling. Two keys reading one string is two questions
// answered by one value, and whichever is written last wins.
func TestNoTwoKeysShareASpelling(t *testing.T) {
	seen := map[string]Key{}
	for _, key := range MovedKeys() {
		for _, spelling := range key {
			if other, taken := seen[spelling]; taken {
				t.Errorf("%q is read by both %v and %v", spelling, other, key)
			}
			seen[spelling] = key
		}
	}
}
