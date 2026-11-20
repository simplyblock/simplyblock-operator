// The four per-volume QoS ceilings, the three generations of names they have
// been written under, and the one resolver that reads all three.
//
// They are collected here rather than left beside the other parameter keys in
// names.go because a ceiling's identity is now a list of spellings rather than a
// single key. A StorageClass's parameters are immutable in the Kubernetes API,
// so a class an older operator generated can never be rewritten into the current
// vocabulary and the older keys are read indefinitely rather than for a
// deprecation window. Keeping the generations in one file is what stops the
// oldest of them being dropped by somebody who only ever saw the newest.
//
// design-storagepool.md §5.1 is the specification, including why "bytes" is
// spelled out and why there is no read-only or write-only IOPS ceiling.

package kube

// The current StorageClass parameter keys. Each says the quantity, the
// direction, and the unit, which is what the qos_ generation did not: `rw` read
// as an access mode in a StorageClass of all places, and `mbytes` did not say
// per second.
//
// There is no read-only or write-only IOPS key, and the absence is the control
// plane's rather than an omission: it enforces one combined IOPS limit and
// nothing else.
const (
	// ParamMaxIOPS caps operations per second across both directions.
	ParamMaxIOPS = "max_iops"
	// ParamMaxMBytesPerSec caps throughput across both directions, in megabytes
	// per second.
	ParamMaxMBytesPerSec = "max_mbytes_per_sec"
	// ParamMaxReadMBytesPerSec caps read throughput, in megabytes per second.
	ParamMaxReadMBytesPerSec = "max_read_mbytes_per_sec"
	// ParamMaxWriteMBytesPerSec caps write throughput, in megabytes per second.
	ParamMaxWriteMBytesPerSec = "max_write_mbytes_per_sec"
)

// The claim annotations that override a class's ceilings, newest generation
// first. The current one takes the group's key prefix as well as the new names,
// since a rename is the one moment when correcting the prefix costs nothing
// extra.
const (
	AnnoMaxIOPS              = "storage.simplyblock.io/max-iops"
	AnnoMaxMBytesPerSec      = "storage.simplyblock.io/max-mbytes-per-sec"
	AnnoMaxReadMBytesPerSec  = "storage.simplyblock.io/max-read-mbytes-per-sec"
	AnnoMaxWriteMBytesPerSec = "storage.simplyblock.io/max-write-mbytes-per-sec"
)

// QoSCeiling names one of the four ceilings independently of how it is spelled.
// It is what a caller asks for, and the key lists below are what answers.
type QoSCeiling int

const (
	// CeilingIOPS is operations per second, both directions together.
	CeilingIOPS QoSCeiling = iota
	// CeilingMBytesPerSec is throughput, both directions together.
	CeilingMBytesPerSec
	// CeilingReadMBytesPerSec is read throughput.
	CeilingReadMBytesPerSec
	// CeilingWriteMBytesPerSec is write throughput.
	CeilingWriteMBytesPerSec
)

// qosParamKeys are the StorageClass parameter spellings of each ceiling, newest
// first. The order is the whole of the precedence rule.
var qosParamKeys = map[QoSCeiling][]string{
	CeilingIOPS:              {ParamMaxIOPS, ParamQoSRWIOPS},
	CeilingMBytesPerSec:      {ParamMaxMBytesPerSec, ParamQoSRWMBytes},
	CeilingReadMBytesPerSec:  {ParamMaxReadMBytesPerSec, ParamQoSRMBytes},
	CeilingWriteMBytesPerSec: {ParamMaxWriteMBytesPerSec, ParamQoSWMBytes},
}

// qosAnnotationKeys are the claim-annotation spellings of each ceiling, newest
// first. Three generations rather than the parameters' two, because the
// annotations were renamed once before the group prefix was settled.
var qosAnnotationKeys = map[QoSCeiling][]string{
	CeilingIOPS: {
		AnnoMaxIOPS, "simplyblock.io/qos-rw-iops", "simplybk/qos-rw-iops",
	},
	CeilingMBytesPerSec: {
		AnnoMaxMBytesPerSec, "simplyblock.io/qos-rw-mbps", "simplybk/qos-rw-mbytes",
	},
	CeilingReadMBytesPerSec: {
		AnnoMaxReadMBytesPerSec, "simplyblock.io/qos-r-mbps", "simplybk/qos-r-mbytes",
	},
	CeilingWriteMBytesPerSec: {
		AnnoMaxWriteMBytesPerSec, "simplyblock.io/qos-w-mbps", "simplybk/qos-w-mbytes",
	},
}

// QoSParamKeys returns every StorageClass parameter key one ceiling is read
// under, newest spelling first. The slice is a copy, so a caller may sort or
// append to it.
func QoSParamKeys(ceiling QoSCeiling) []string {
	return append([]string(nil), qosParamKeys[ceiling]...)
}

// QoSAnnotationKeys returns every claim-annotation key one ceiling is read
// under, newest spelling first. The slice is a copy.
func QoSAnnotationKeys(ceiling QoSCeiling) []string {
	return append([]string(nil), qosAnnotationKeys[ceiling]...)
}

// QoSCeilings is every ceiling, in the order they are worth reporting in: the
// combined limits first, then the per-direction ones.
func QoSCeilings() []QoSCeiling {
	return []QoSCeiling{
		CeilingIOPS, CeilingMBytesPerSec, CeilingReadMBytesPerSec, CeilingWriteMBytesPerSec,
	}
}

// FirstSet returns the value of the first key in keys that m has a non-empty
// value for, and "" when none does.
//
// It is the whole of the precedence rule for a vocabulary with more than one
// generation: pass the keys newest first and the newest spelling that is set
// wins. One helper serves both maps a ceiling can arrive in, because a class
// parameter and a claim annotation are the same problem with different keys.
func FirstSet(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != "" {
			return v
		}
	}
	return ""
}

// QoSParam returns one ceiling's value from a StorageClass's parameters,
// preferring the current spelling over the older one.
func QoSParam(params map[string]string, ceiling QoSCeiling) string {
	return FirstSet(params, qosParamKeys[ceiling]...)
}

// QoSAnnotation returns one ceiling's override from a claim's annotations,
// preferring the newest spelling.
func QoSAnnotation(annotations map[string]string, ceiling QoSCeiling) string {
	return FirstSet(annotations, qosAnnotationKeys[ceiling]...)
}

// QoSParamConflicts returns the parameter keys of every ceiling that params
// spells more than one way, newest key first within each ceiling.
//
// Carrying two spellings of one ceiling is a conflict rather than a merge: the
// two are separate keys stating separate numbers, and quietly preferring one is
// how a volume ends up throttled at a value nobody chose. The newest spelling
// still wins — a reader needs an answer — and this is what lets the caller say
// so rather than deciding it in silence.
func QoSParamConflicts(params map[string]string) [][]string {
	var out [][]string
	for _, ceiling := range QoSCeilings() {
		var set []string
		for _, k := range qosParamKeys[ceiling] {
			if v, ok := params[k]; ok && v != "" {
				set = append(set, k)
			}
		}
		if len(set) > 1 {
			out = append(out, set)
		}
	}
	return out
}
