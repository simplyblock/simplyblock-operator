// The note of an event is limited by the API server, which refuses a longer one
// and drops the event, so a long error text has to be clipped on the way in.
package utils

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEventNoteLeavesAShortNoteAlone(t *testing.T) {
	if got := EventNote("the promotion mounted nothing"); got != "the promotion mounted nothing" {
		t.Errorf("EventNote = %q", got)
	}
}

func TestEventNoteStaysWithinTheLimitAndSaysItWasClipped(t *testing.T) {
	long := strings.Repeat("connect failed: ", 200)
	got := EventNote(long)
	if len(got) > EventNoteLimit {
		t.Errorf("note is %d bytes, over the limit of %d", len(got), EventNoteLimit)
	}
	if !strings.HasSuffix(got, "...") || !strings.HasPrefix(got, "connect failed:") {
		t.Errorf("note = %q..., want the start of the text and a trailing mark", got[:40])
	}
}

func TestEventNoteDoesNotSplitACharacter(t *testing.T) {
	got := EventNote(strings.Repeat("é", 2000))
	if !utf8.ValidString(got) {
		t.Error("the clipped note is not valid UTF-8")
	}
	if len(got) > EventNoteLimit {
		t.Errorf("note is %d bytes, over the limit of %d", len(got), EventNoteLimit)
	}
}
