// What a byte range of a file is made of, laid over its extent map: the
// classification is pure and tested here on any OS, and the ioctl that reads
// the map is tested against a real file in read_linux_test.go.

package fiemap

import (
	"reflect"
	"testing"
)

const mib = 1 << 20

func TestARangeOverAWrittenExtentIsWritten(t *testing.T) {
	got := Classify([]Extent{{Logical: 0, Physical: 100 * mib, Length: 4 * mib}}, mib, mib)
	want := []Piece{{Offset: mib, Length: mib, Kind: Written, Physical: 101 * mib}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Classify = %+v, want %+v", got, want)
	}
}

// The three cases a block reads back as zeros through: never allocated, still
// unwritten, or delalloc the server has not flushed. Each is told apart.
func TestARangeIsSplitIntoHoleUnwrittenAndDelalloc(t *testing.T) {
	extents := []Extent{
		{Logical: 1 * mib, Physical: 50 * mib, Length: mib, Flags: FlagUnwritten},
		{Logical: 2 * mib, Length: mib, Flags: FlagDelalloc | FlagUnknown},
		{Logical: 3 * mib, Physical: 70 * mib, Length: mib},
	}
	got := Classify(extents, 0, 4*mib)
	want := []Piece{
		{Offset: 0, Length: mib, Kind: Hole},
		{Offset: mib, Length: mib, Kind: Unwritten, Physical: 50 * mib},
		{Offset: 2 * mib, Length: mib, Kind: Delalloc},
		{Offset: 3 * mib, Length: mib, Kind: Written, Physical: 70 * mib},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Classify = %+v, want %+v", got, want)
	}
}

// A range running past the last extent ends in a hole, and one inside a single
// extent is clipped to the range at both ends.
func TestARangeIsClippedAtBothEndsAndEndsInAHole(t *testing.T) {
	extents := []Extent{{Logical: 0, Physical: 10 * mib, Length: 2 * mib}}
	got := Classify(extents, mib+4096, 2*mib)
	want := []Piece{
		{Offset: mib + 4096, Length: mib - 4096, Kind: Written, Physical: 11*mib + 4096},
		{Offset: 2 * mib, Length: mib + 4096, Kind: Hole},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Classify = %+v, want %+v", got, want)
	}
}

func TestAnEmptyRangeHasNoPieces(t *testing.T) {
	if got := Classify([]Extent{{Logical: 0, Length: mib}}, 0, 0); len(got) != 0 {
		t.Errorf("Classify of an empty range = %+v, want none", got)
	}
}
