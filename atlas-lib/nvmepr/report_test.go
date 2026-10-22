// The Reservation Report decoder, against buffers built byte by byte from the
// layout in the NVMe base specification. The offsets are written out here and
// not shared with the decoder, so a decoder that read the wrong field would
// disagree with them.
package nvmepr

import (
	"encoding/binary"
	"errors"
	"testing"
)

const (
	reportHeaderLen = 64
	reportEntryLen  = 64
)

type entry struct {
	cntlid uint16
	holds  bool
	key    uint64
	host   [16]byte
}

// buildReport lays out the extended Reservation Status data structure: a
// 64-byte header (GEN at 0, RTYPE at 4, REGCTL at 5), then one 64-byte entry per
// registered controller (CNTLID at 0, RCSTS at 2, RKEY at 8, HOSTID at 16).
func buildReport(gen uint32, rtype uint8, entries ...entry) []byte {
	buf := make([]byte, reportHeaderLen+reportEntryLen*len(entries))
	binary.LittleEndian.PutUint32(buf[0:], gen)
	buf[4] = rtype
	binary.LittleEndian.PutUint16(buf[5:], uint16(len(entries)))
	for i, e := range entries {
		b := buf[reportHeaderLen+reportEntryLen*i:]
		binary.LittleEndian.PutUint16(b[0:], e.cntlid)
		if e.holds {
			b[2] = 1
		}
		binary.LittleEndian.PutUint64(b[8:], e.key)
		copy(b[16:], e.host[:])
	}
	return buf
}

func host(b byte) (h [16]byte) {
	for i := range h {
		h[i] = b + byte(i)
	}
	return h
}

func TestParseReportReadsEveryField(t *testing.T) {
	buf := buildReport(102, 4,
		entry{cntlid: 0xffff, holds: true, key: 0x0100000000000001, host: host(0x10)},
		entry{cntlid: 3, key: 0x6ad8d693350aea19, host: host(0x20)},
	)
	got, err := ParseReport(buf)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if got.Generation != 102 || got.Type != TypeExclusiveAccessRegOnly || len(got.Registrants) != 2 {
		t.Fatalf("header = gen %d type %d registrants %d", got.Generation, got.Type, len(got.Registrants))
	}
	first := got.Registrants[0]
	if first.ControllerID != 0xffff || !first.HoldsReservation || first.Key != 0x0100000000000001 || first.HostID != host(0x10) {
		t.Errorf("first registrant = %+v", first)
	}
	second := got.Registrants[1]
	if second.ControllerID != 3 || second.HoldsReservation || second.Key != 0x6ad8d693350aea19 || second.HostID != host(0x20) {
		t.Errorf("second registrant = %+v", second)
	}
}

func TestHolderIsTheRegistrantThatHoldsTheReservation(t *testing.T) {
	r, err := ParseReport(buildReport(1, 4,
		entry{key: 7, host: host(1)},
		entry{key: 8, holds: true, host: host(2)},
	))
	if err != nil {
		t.Fatal(err)
	}
	h, ok := r.Holder()
	if !ok || h.Key != 8 {
		t.Fatalf("Holder() = %+v, %v; want key 8", h, ok)
	}
}

func TestHolderIsAbsentWithoutAReservation(t *testing.T) {
	r, err := ParseReport(buildReport(1, 0, entry{key: 7, host: host(1)}))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Holder(); ok {
		t.Error("Holder() reported a holder on a namespace with no reservation")
	}
}

func TestParseReportOfANamespaceWithNoRegistrants(t *testing.T) {
	r, err := ParseReport(buildReport(5, 0))
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if len(r.Registrants) != 0 {
		t.Errorf("registrants = %d, want 0", len(r.Registrants))
	}
}

func TestParseReportRefusesATruncatedBuffer(t *testing.T) {
	full := buildReport(1, 4, entry{key: 7, host: host(1)}, entry{key: 8, host: host(2)})
	for _, n := range []int{0, 10, reportHeaderLen - 1, reportHeaderLen + reportEntryLen, len(full) - 1} {
		if _, err := ParseReport(full[:n]); !errors.Is(err, ErrShortReport) {
			t.Errorf("a %d-byte buffer: err = %v, want ErrShortReport", n, err)
		}
	}
}

// A Reservation Report as SPDK answered it on 2026-10-21, with the extended
// data structure requested (the first 96 bytes of `nvme resv-report -e -b`,
// REGCTL cut to the one entry captured). The extended structure's header is 64
// bytes, and the first entry starts at byte 64. Reading the layout from the
// specification's figures alone put it at byte 24, and the decoder then read
// every entry as zeros.
func TestParseReportOfARealTargetsAnswer(t *testing.T) {
	capture := []byte{
		0xbe, 0x00, 0x00, 0x00, 0x04, 0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0xff, 0xff, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
		0xee, 0x38, 0xe4, 0x24, 0xe3, 0xfd, 0x4d, 0xaf, 0xbe, 0x4a, 0x9b, 0xed, 0x01, 0xfe, 0x26, 0xf8,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	// The second half of the entry is reserved, so the buffer is a full entry long.
	r, err := ParseReport(capture[:128])
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if len(r.Registrants) != 1 {
		t.Fatalf("registrants = %d, want 1", len(r.Registrants))
	}
	got := r.Registrants[0]
	wantHost := [16]byte{0xee, 0x38, 0xe4, 0x24, 0xe3, 0xfd, 0x4d, 0xaf, 0xbe, 0x4a, 0x9b, 0xed, 0x01, 0xfe, 0x26, 0xf8}
	if got.Key != 0x0100000000000000 || got.HostID != wantHost || !got.HoldsReservation || got.ControllerID != 0xffff {
		t.Errorf("registrant = %+v", got)
	}
	if r.Generation != 190 || r.Type != TypeExclusiveAccessRegOnly {
		t.Errorf("header = gen %d type %d", r.Generation, r.Type)
	}
}
