// Clipping an event's note to what the API server accepts, for the events whose
// text is an error from the control plane or the guest and can run long.
package utils

import "unicode/utf8"

// EventNoteLimit is the longest note, in bytes, the API server accepts on an event.
const EventNoteLimit = 1024

// EventNote returns note, clipped to EventNoteLimit with a trailing mark.
func EventNote(note string) string {
	const mark = "..."
	if len(note) <= EventNoteLimit {
		return note
	}
	cut := EventNoteLimit - len(mark)
	for cut > 0 && !utf8.RuneStart(note[cut]) {
		cut--
	}
	return note[:cut] + mark
}
