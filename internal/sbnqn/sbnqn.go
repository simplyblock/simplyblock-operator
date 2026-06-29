// Package sbnqn provides typed parsing for SimplyBlock NVMe Qualified Names.
//
// An SBNQN is a constrained subset of NVMe Qualified Names used within the
// simplyblock storage stack. All SBNQNs share the prefix
// "nqn.<date>.io.simplyblock:" followed by kind-specific segments:
//
//   - ClusterNQN:     nqn.<date>.io.simplyblock:<cluster-uuid>
//   - VolumeNQN:      nqn.<date>.io.simplyblock:<cluster-uuid>:lvol:<lvol-uuid>
//   - TransferhubNQN: nqn.<date>.io.simplyblock:<cluster-uuid>:transferhub:<name>
//   - DevNQN:         nqn.<date>.io.simplyblock:<name>:dev:<device-uuid>
//   - HostNQN:        nqn.<date>.io.simplyblock:uuid:<node-uid>
package sbnqn

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// SBNQN is the sealed interface satisfied by all SimplyBlock NQN types.
type SBNQN interface {
	String() string
	sealed()
}

// base holds the date segment common to all SBNQN kinds and provides the
// sealed() method so that external packages cannot implement the interface.
type base struct {
	Date string
}

func (b base) sealed() {}

// IsZero reports whether the NQN is the zero value (unset).
func (b base) IsZero() bool { return b.Date == "" }

func (b base) prefix() string {
	return "nqn." + b.Date + ".io.simplyblock:"
}

// ClusterNQN represents a cluster-level NQN: nqn.<date>.io.simplyblock:<cluster-uuid>
type ClusterNQN struct {
	base
	ClusterID uuid.UUID
}

func (n ClusterNQN) String() string {
	if n.IsZero() {
		return ""
	}
	return n.prefix() + n.ClusterID.String()
}

func (n ClusterNQN) MarshalJSON() ([]byte, error) {
	return json.Marshal(n.String())
}

func (n *ClusterNQN) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*n = ClusterNQN{}
		return nil
	}
	parsed, err := Parse(s)
	if err != nil {
		return err
	}
	v, ok := parsed.(ClusterNQN)
	if !ok {
		return fmt.Errorf("sbnqn: expected ClusterNQN, got %T", parsed)
	}
	*n = v
	return nil
}

// VolumeNQN represents a volume NQN: nqn.<date>.io.simplyblock:<cluster-uuid>:lvol:<lvol-uuid>
type VolumeNQN struct {
	base
	ClusterID uuid.UUID
	LvolID    uuid.UUID
}

func (n VolumeNQN) String() string {
	if n.IsZero() {
		return ""
	}
	return n.prefix() + n.ClusterID.String() + ":lvol:" + n.LvolID.String()
}

func (n VolumeNQN) MarshalJSON() ([]byte, error) {
	return json.Marshal(n.String())
}

func (n *VolumeNQN) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*n = VolumeNQN{}
		return nil
	}
	parsed, err := Parse(s)
	if err != nil {
		return err
	}
	v, ok := parsed.(VolumeNQN)
	if !ok {
		return fmt.Errorf("sbnqn: expected VolumeNQN, got %T", parsed)
	}
	*n = v
	return nil
}

// TransferhubNQN represents a transferhub NQN: nqn.<date>.io.simplyblock:<cluster-uuid>:transferhub:<name>
type TransferhubNQN struct {
	base
	ClusterID uuid.UUID
	Name      string
}

func (n TransferhubNQN) String() string {
	if n.IsZero() {
		return ""
	}
	return n.prefix() + n.ClusterID.String() + ":transferhub:" + n.Name
}

func (n TransferhubNQN) MarshalJSON() ([]byte, error) {
	return json.Marshal(n.String())
}

func (n *TransferhubNQN) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*n = TransferhubNQN{}
		return nil
	}
	parsed, err := Parse(s)
	if err != nil {
		return err
	}
	v, ok := parsed.(TransferhubNQN)
	if !ok {
		return fmt.Errorf("sbnqn: expected TransferhubNQN, got %T", parsed)
	}
	*n = v
	return nil
}

// DevNQN represents a device NQN: nqn.<date>.io.simplyblock:<name>:dev:<device-uuid>
type DevNQN struct {
	base
	Name     string
	DeviceID uuid.UUID
}

func (n DevNQN) String() string {
	if n.IsZero() {
		return ""
	}
	return n.prefix() + n.Name + ":dev:" + n.DeviceID.String()
}

func (n DevNQN) MarshalJSON() ([]byte, error) {
	return json.Marshal(n.String())
}

func (n *DevNQN) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*n = DevNQN{}
		return nil
	}
	parsed, err := Parse(s)
	if err != nil {
		return err
	}
	v, ok := parsed.(DevNQN)
	if !ok {
		return fmt.Errorf("sbnqn: expected DevNQN, got %T", parsed)
	}
	*n = v
	return nil
}

// HostNQN represents a host NQN: nqn.<date>.io.simplyblock:uuid:<node-uid>
type HostNQN struct {
	base
	NodeUID uuid.UUID
}

func (n HostNQN) String() string {
	if n.IsZero() {
		return ""
	}
	return n.prefix() + "uuid:" + n.NodeUID.String()
}

func (n HostNQN) MarshalJSON() ([]byte, error) {
	return json.Marshal(n.String())
}

func (n *HostNQN) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*n = HostNQN{}
		return nil
	}
	parsed, err := Parse(s)
	if err != nil {
		return err
	}
	v, ok := parsed.(HostNQN)
	if !ok {
		return fmt.Errorf("sbnqn: expected HostNQN, got %T", parsed)
	}
	*n = v
	return nil
}

// NewClusterNQN constructs a ClusterNQN with the given date and cluster UUID.
func NewClusterNQN(date string, clusterID uuid.UUID) ClusterNQN {
	return ClusterNQN{base: base{Date: date}, ClusterID: clusterID}
}

// NewVolumeNQN constructs a VolumeNQN with the given date, cluster UUID, and lvol UUID.
func NewVolumeNQN(date string, clusterID, lvolID uuid.UUID) VolumeNQN {
	return VolumeNQN{base: base{Date: date}, ClusterID: clusterID, LvolID: lvolID}
}

// NewTransferhubNQN constructs a TransferhubNQN with the given date, cluster UUID, and name.
func NewTransferhubNQN(date string, clusterID uuid.UUID, name string) TransferhubNQN {
	return TransferhubNQN{base: base{Date: date}, ClusterID: clusterID, Name: name}
}

// NewDevNQN constructs a DevNQN with the given date, name, and device UUID.
func NewDevNQN(date string, name string, deviceID uuid.UUID) DevNQN {
	return DevNQN{base: base{Date: date}, Name: name, DeviceID: deviceID}
}

// NewHostNQN constructs a HostNQN with the given date and node UID.
func NewHostNQN(date string, nodeUID uuid.UUID) HostNQN {
	return HostNQN{base: base{Date: date}, NodeUID: nodeUID}
}

// ExtractLvolID extracts the lvol UUID string from a raw NQN string.
// It returns the lvol ID and true if the input is a valid volume NQN,
// or ("", false) otherwise. This is a drop-in replacement for lvolIDFromNQN.
func ExtractLvolID(raw string) (string, bool) {
	parsed, err := Parse(raw)
	if err != nil {
		return "", false
	}
	v, ok := parsed.(VolumeNQN)
	if !ok {
		return "", false
	}
	return v.LvolID.String(), true
}

// Compile-time interface satisfaction checks.
var (
	_ SBNQN = ClusterNQN{}
	_ SBNQN = VolumeNQN{}
	_ SBNQN = TransferhubNQN{}
	_ SBNQN = DevNQN{}
	_ SBNQN = HostNQN{}
	_ SBNQN = (*ClusterNQN)(nil)
)

// Ensure the interface cannot be externally satisfied.
var _ fmt.Stringer = (SBNQN)(nil)
