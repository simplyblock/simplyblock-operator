// The host NQN of each metadata server pod. The takeover sends it with every
// call and the allowed-hosts sync authorizes it on every volume, so both read
// it from here rather than derive it twice.

package mdsstate

import (
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/atlas/nqn"
)

// HostNQN is the host NQN of one pod of a metadata server StatefulSet:
//
//	nqn.Host(uuid.NewSHA1(<StatefulSet UID>, "<ordinal>"))
//
// It is stable across the pod's restarts and changes when the StatefulSet is
// recreated, and each ordinal is its own host at the target.
func HostNQN(statefulSetUID types.UID, ordinal int32) (string, error) {
	space, err := uuid.Parse(string(statefulSetUID))
	if err != nil {
		return "", fmt.Errorf("host NQN of a metadata server pod: the StatefulSet UID %q is not a UUID: %w",
			statefulSetUID, err)
	}
	if ordinal < 0 {
		return "", fmt.Errorf("host NQN of a metadata server pod: ordinal %d is negative", ordinal)
	}
	return nqn.Host(uuid.NewSHA1(space, []byte(strconv.Itoa(int(ordinal)))).String()), nil
}
