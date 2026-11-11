// Package nvmepr drives the persistent reservations of an NVMe namespace: it
// registers a key, acquires and preempts the reservation, and reads back who is
// registered under which host.
//
// It is separate from package nvme because that package only reads kernel
// state, and a reservation command changes what the target lets a host do. The
// first caller is the metadata server's guest agent, which fences every device
// before it mounts one (design-pnfs-mds-standby.md §6.4). The CSI node plugin's
// client keys are a likely second.
//
// The report names each registrant by its host identifier, which the kernel's
// own pr_ops cannot return, so Report goes through an NVMe passthrough command
// and the three actions through the block layer's persistent-reservation
// ioctls.
package nvmepr

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrShortReport is a buffer too short for the registrants its header counts.
var ErrShortReport = errors.New("short reservation report")

// Type is a reservation type, with the values of the kernel's enum pr_type.
type Type uint32

// TypeExclusiveAccessRegOnly is the type nfsd reserves a device with: every
// registered host may read and write, and no other host may do either.
const TypeExclusiveAccessRegOnly Type = 4

// Registrant is one entry of a Report.
type Registrant struct {
	ControllerID     uint16
	HostID           [16]byte
	Key              uint64
	HoldsReservation bool
}

// Report is a namespace's reservation status.
type Report struct {
	Generation  uint32
	Type        Type
	Registrants []Registrant
}

const (
	headerSize = 64
	entrySize  = 64
)

// ParseReport decodes the extended data structure of an NVMe Reservation
// Report, the one that carries each registrant's host identifier. The header
// is 64 bytes (generation, reservation type, registrant count, then reserved
// space), followed by one 64-byte entry per registered controller.
func ParseReport(buf []byte) (Report, error) {
	if len(buf) < headerSize {
		return Report{}, fmt.Errorf("%w: %d bytes, header needs %d", ErrShortReport, len(buf), headerSize)
	}
	count := reportedRegistrants(buf)
	if need := headerSize + count*entrySize; len(buf) < need {
		return Report{}, fmt.Errorf("%w: %d bytes, %d registrants need %d", ErrShortReport, len(buf), count, need)
	}
	r := Report{
		Generation:  binary.LittleEndian.Uint32(buf[0:]),
		Type:        Type(buf[4]),
		Registrants: make([]Registrant, count),
	}
	for i := range r.Registrants {
		e := buf[headerSize+i*entrySize:]
		reg := Registrant{
			ControllerID:     binary.LittleEndian.Uint16(e[0:]),
			HoldsReservation: e[2]&1 != 0,
			Key:              binary.LittleEndian.Uint64(e[8:]),
		}
		copy(reg.HostID[:], e[16:32])
		r.Registrants[i] = reg
	}
	return r, nil
}

// Holder returns the registrant that holds the reservation, when one does.
func (r Report) Holder() (Registrant, bool) {
	for _, reg := range r.Registrants {
		if reg.HoldsReservation {
			return reg, true
		}
	}
	return Registrant{}, false
}

// reportedRegistrants is the registrant count a report's header claims, which
// can exceed what the buffer held. The buffer must hold the header.
func reportedRegistrants(buf []byte) int {
	return int(binary.LittleEndian.Uint16(buf[5:]))
}
