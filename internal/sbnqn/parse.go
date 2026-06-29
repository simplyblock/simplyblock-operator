package sbnqn

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const nqnPrefix = "nqn."
const domainSuffix = ".io.simplyblock:"

// Parse parses a raw NQN string into one of the concrete SBNQN types.
// It returns an error if the string does not match any known SBNQN format.
func Parse(raw string) (SBNQN, error) {
	if !strings.HasPrefix(raw, nqnPrefix) {
		return nil, fmt.Errorf("sbnqn: missing %q prefix: %q", nqnPrefix, raw)
	}

	// Find the domain suffix to extract the date segment.
	domainIdx := strings.Index(raw, domainSuffix)
	if domainIdx < 0 {
		return nil, fmt.Errorf("sbnqn: missing %q in: %q", domainSuffix, raw)
	}

	date := raw[len(nqnPrefix):domainIdx]
	if date == "" {
		return nil, fmt.Errorf("sbnqn: empty date segment in: %q", raw)
	}

	body := raw[domainIdx+len(domainSuffix):]
	if body == "" {
		return nil, fmt.Errorf("sbnqn: empty body after prefix in: %q", raw)
	}

	segments := strings.Split(body, ":")
	b := base{Date: date}

	switch len(segments) {
	case 1:
		// ClusterNQN: <cluster-uuid>
		clusterID, err := uuid.Parse(segments[0])
		if err != nil {
			return nil, fmt.Errorf("sbnqn: invalid cluster UUID %q: %w", segments[0], err)
		}
		return ClusterNQN{base: b, ClusterID: clusterID}, nil

	case 2:
		// HostNQN: uuid:<node-uid>
		if segments[0] != "uuid" {
			return nil, fmt.Errorf("sbnqn: unknown 2-segment tag %q in: %q", segments[0], raw)
		}
		nodeUID, err := uuid.Parse(segments[1])
		if err != nil {
			return nil, fmt.Errorf("sbnqn: invalid node UID %q: %w", segments[1], err)
		}
		return HostNQN{base: b, NodeUID: nodeUID}, nil

	case 3:
		tag := segments[1]
		switch tag {
		case "lvol":
			// VolumeNQN: <cluster-uuid>:lvol:<lvol-uuid>
			clusterID, err := uuid.Parse(segments[0])
			if err != nil {
				return nil, fmt.Errorf("sbnqn: invalid cluster UUID %q: %w", segments[0], err)
			}
			lvolID, err := uuid.Parse(segments[2])
			if err != nil {
				return nil, fmt.Errorf("sbnqn: invalid lvol UUID %q: %w", segments[2], err)
			}
			return VolumeNQN{base: b, ClusterID: clusterID, LvolID: lvolID}, nil

		case "transferhub":
			// TransferhubNQN: <cluster-uuid>:transferhub:<name>
			clusterID, err := uuid.Parse(segments[0])
			if err != nil {
				return nil, fmt.Errorf("sbnqn: invalid cluster UUID %q: %w", segments[0], err)
			}
			if segments[2] == "" {
				return nil, fmt.Errorf("sbnqn: empty transferhub name in: %q", raw)
			}
			return TransferhubNQN{base: b, ClusterID: clusterID, Name: segments[2]}, nil

		case "dev":
			// DevNQN: <name>:dev:<device-uuid>
			if segments[0] == "" {
				return nil, fmt.Errorf("sbnqn: empty device name in: %q", raw)
			}
			deviceID, err := uuid.Parse(segments[2])
			if err != nil {
				return nil, fmt.Errorf("sbnqn: invalid device UUID %q: %w", segments[2], err)
			}
			return DevNQN{base: b, Name: segments[0], DeviceID: deviceID}, nil

		default:
			return nil, fmt.Errorf("sbnqn: unknown 3-segment tag %q in: %q", tag, raw)
		}

	default:
		return nil, fmt.Errorf("sbnqn: unexpected segment count %d in: %q", len(segments), raw)
	}
}
