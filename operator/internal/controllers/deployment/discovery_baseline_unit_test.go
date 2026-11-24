// Tests of the baseline the discovery cases are recorded against.
//
// The case tree relies on these helpers to be lossless for the document: a
// recorded delta applied to the baseline has to give back exactly what the run
// wrote, or a case would pass against a document nobody recorded. The notes are
// the one place the baseline is allowed to drop something, and the tests pin
// down which lines that is.

package deployment

import (
	"reflect"
	"testing"
)

func TestDiscoveryBaselineKeepsWhatMostDocumentsShare(t *testing.T) {
	documents := []map[string]any{
		{"spec": map[string]any{"cluster": map[string]any{
			"maxSubsystemCount": 30, "clusterMode": "Single", "name": "a",
		}}},
		{"spec": map[string]any{"cluster": map[string]any{
			"maxSubsystemCount": 30, "clusterMode": "Single", "name": "b",
		}}},
		{"spec": map[string]any{"cluster": map[string]any{
			"maxSubsystemCount": 30, "clusterMode": "Dual", "name": "c",
		}}},
		{"spec": map[string]any{"cluster": map[string]any{
			"maxSubsystemCount": 10, "name": "d",
		}}},
	}

	got := discoveryBaseline(documents)

	want := map[string]any{"spec": map[string]any{"cluster": map[string]any{
		"maxSubsystemCount": 30,
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("baseline is %v, want %v: a value is shared by more than half of "+
			"the documents or it stays in the cases", got, want)
	}
}

func TestDiscoveryDeltaRoundTrips(t *testing.T) {
	baseline := map[string]any{"spec": map[string]any{"cluster": map[string]any{
		"maxSubsystemCount": float64(30),
		"network":           map[string]any{"spdkOnHostNetwork": true},
	}}}
	document := map[string]any{
		"metadata": map[string]any{"name": "discovered-x"},
		"spec": map[string]any{"cluster": map[string]any{
			"maxSubsystemCount": float64(30),
			"vcpuCount":         float64(5),
		}},
	}

	delta, err := discoveryDelta(baseline, document)
	if err != nil {
		t.Fatalf("delta: %v", err)
	}

	wantDelta := map[string]any{
		"metadata": map[string]any{"name": "discovered-x"},
		"spec": map[string]any{"cluster": map[string]any{
			"network":   nil,
			"vcpuCount": float64(5),
		}},
	}
	if !reflect.DeepEqual(delta, wantDelta) {
		t.Errorf("delta is %v, want %v: only what differs from the baseline, and a "+
			"baseline field the document lacks as null", delta, wantDelta)
	}

	back, err := applyDiscoveryDelta(baseline, delta)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !reflect.DeepEqual(back, document) {
		t.Errorf("the delta applied to the baseline gives %v, want the document %v", back, document)
	}
}

func TestDiscoveryNotesRecordOnlyWhatDiffersFromTheBaseline(t *testing.T) {
	baseline := map[string]any{"spec": map[string]any{"cluster": map[string]any{
		"maxSubsystemCount": float64(30),
		"vcpuCount":         float64(5),
	}}}
	document := map[string]any{"spec": map[string]any{
		"cluster": map[string]any{
			"maxSubsystemCount": float64(30),
			"vcpuCount":         float64(8),
			"clusterMode":       "Dual",
		},
		"hostOS": map[string]any{"distro": "ubuntu"},
	}}
	notes := []string{
		"maxSubsystemCount is 30, the same as the baseline",
		"vcpuCount is 8, not the baseline's 5",
		"clusterMode is Dual, which the baseline does not carry",
		"enableJournalDevice is left unset, as it is in the baseline",
		"hostOS is ubuntu, which the baseline does not carry",
		"wrote discovered-x in Draft",
	}

	got := recordedDiscoveryNotes(notes, document, baseline)

	want := []string{
		"vcpuCount is 8, not the baseline's 5",
		"clusterMode is Dual, which the baseline does not carry",
		"hostOS is ubuntu, which the baseline does not carry",
		"wrote discovered-x in Draft",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recorded notes are\n%q\nwant\n%q", got, want)
	}
}

func TestDiscoveryNotesAreAllRecordedWithoutADocument(t *testing.T) {
	baseline := map[string]any{"spec": map[string]any{"cluster": map[string]any{
		"maxSubsystemCount": float64(30),
	}}}
	notes := []string{"maxSubsystemCount is 30"}

	got := recordedDiscoveryNotes(notes, nil, baseline)

	if !reflect.DeepEqual(got, notes) {
		t.Errorf("recorded notes are %q, want every note %q: a run that wrote no "+
			"document has no values to compare a note's field by", got, notes)
	}
}
