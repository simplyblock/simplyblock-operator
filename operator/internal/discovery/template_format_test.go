// What the draft says about formatting the devices it names.
//
// A storage node takes a device by formatting it, so the flag is not an unusual
// request. It is still the difference between a document that fails on a disk
// with something on it and one that wipes it, and the document is where that
// difference is decided: stating it in the draft is what puts it in front of the
// reviewer who approves, and what lets them strike it before they do.
//
// Leaving it to a default further down would apply it without the document ever
// saying so, and the field is immutable once the cluster exists — so a default
// nobody saw could not be undone either.

package discovery

import (
	"strings"
	"testing"
)

func TestTheDraftStatesThatDevicesAreFormatted(t *testing.T) {
	template := ClusterTemplateFor("a-cluster", Plan{})

	if template.Template.EnableDriveFormat == nil {
		t.Fatal("the draft leaves the format flag unstated")
	}
	if !*template.Template.EnableDriveFormat {
		t.Error("the draft states that devices are not formatted")
	}

	mentioned := false
	for _, note := range template.Notes {
		if strings.Contains(strings.ToLower(note), "format") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Error("nothing in the notes tells a reviewer the disks will be formatted")
	}
}
