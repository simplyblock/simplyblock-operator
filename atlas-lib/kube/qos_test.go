package kube_test

import (
	"testing"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/kube"
)

// storageClassWith builds a class this driver provisions, carrying params.
func storageClassWith(params map[string]string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "under-test"},
		Provisioner: kube.DriverName,
		Parameters:  params,
	}
}

// The current spelling wins over the older one, which is the whole of the
// precedence rule: the lists are ordered newest first and the first key that is
// set is taken.
func TestQoSParamPrefersTheCurrentSpelling(t *testing.T) {
	params := map[string]string{
		kube.ParamMaxIOPS:   "1000",
		kube.ParamQoSRWIOPS: "5000",
	}
	if got := kube.QoSParam(params, kube.CeilingIOPS); got != "1000" {
		t.Errorf("QoSParam = %q, want the current spelling's 1000", got)
	}
}

// A class an older operator generated can never be rewritten, because a
// StorageClass's parameters are immutable. Its keys are therefore read
// indefinitely rather than for a deprecation window.
func TestQoSParamReadsTheOlderSpelling(t *testing.T) {
	params := map[string]string{
		kube.ParamQoSRWIOPS:   "5000",
		kube.ParamQoSRWMBytes: "500",
		kube.ParamQoSRMBytes:  "300",
		kube.ParamQoSWMBytes:  "200",
	}
	for _, tc := range []struct {
		ceiling kube.QoSCeiling
		want    string
	}{
		{kube.CeilingIOPS, "5000"},
		{kube.CeilingMBytesPerSec, "500"},
		{kube.CeilingReadMBytesPerSec, "300"},
		{kube.CeilingWriteMBytesPerSec, "200"},
	} {
		if got := kube.QoSParam(params, tc.ceiling); got != tc.want {
			t.Errorf("QoSParam(%v) = %q, want %q", tc.ceiling, got, tc.want)
		}
	}
}

// An empty value is not a value. A class that sets the current key to "" and the
// older one to a number states the number.
func TestQoSParamTreatsAnEmptyValueAsUnset(t *testing.T) {
	params := map[string]string{
		kube.ParamMaxIOPS:   "",
		kube.ParamQoSRWIOPS: "5000",
	}
	if got := kube.QoSParam(params, kube.CeilingIOPS); got != "5000" {
		t.Errorf("QoSParam = %q, want 5000", got)
	}
}

// Three generations of claim annotation are live, and the newest that is set
// wins. The oldest is the one most likely to be forgotten, so it is checked
// explicitly.
func TestQoSAnnotationReadsAllThreeGenerations(t *testing.T) {
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		want        string
	}{
		{
			name:        "the current spelling",
			annotations: map[string]string{kube.AnnoMaxIOPS: "1000"},
			want:        "1000",
		},
		{
			name:        "the middle spelling",
			annotations: map[string]string{"simplyblock.io/qos-rw-iops": "2000"},
			want:        "2000",
		},
		{
			name:        "the oldest spelling",
			annotations: map[string]string{"simplybk/qos-rw-iops": "3000"},
			want:        "3000",
		},
		{
			name: "all three, newest first",
			annotations: map[string]string{
				kube.AnnoMaxIOPS:             "1000",
				"simplyblock.io/qos-rw-iops": "2000",
				"simplybk/qos-rw-iops":       "3000",
			},
			want: "1000",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := kube.QoSAnnotation(tc.annotations, kube.CeilingIOPS); got != tc.want {
				t.Errorf("QoSAnnotation = %q, want %q", got, tc.want)
			}
		})
	}
}

// A class stating one ceiling under two spellings is a conflict rather than a
// merge: they are separate keys stating separate numbers, and quietly preferring
// one is how a volume ends up throttled at a value nobody chose.
func TestQoSParamConflictsFindsBothSpellings(t *testing.T) {
	params := map[string]string{
		kube.ParamMaxIOPS:         "1000",
		kube.ParamQoSRWIOPS:       "5000",
		kube.ParamMaxMBytesPerSec: "500",
	}

	conflicts := kube.QoSParamConflicts(params)

	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %v, want exactly the IOPS ceiling", conflicts)
	}
	if len(conflicts[0]) != 2 || conflicts[0][0] != kube.ParamMaxIOPS {
		t.Errorf("conflicts[0] = %v, want the current spelling first", conflicts[0])
	}
}

// A class that states each ceiling once is not a conflict, whichever generation
// it used.
func TestQoSParamConflictsIgnoresASingleSpelling(t *testing.T) {
	for _, params := range []map[string]string{
		{kube.ParamMaxIOPS: "1000", kube.ParamMaxMBytesPerSec: "500"},
		{kube.ParamQoSRWIOPS: "5000", kube.ParamQoSRWMBytes: "500"},
		{},
	} {
		if conflicts := kube.QoSParamConflicts(params); len(conflicts) != 0 {
			t.Errorf("QoSParamConflicts(%v) = %v, want none", params, conflicts)
		}
	}
}

// The key lists are handed out as copies, so a caller that sorts or appends to
// one does not reorder the precedence rule for everybody else.
func TestQoSKeyListsAreCopies(t *testing.T) {
	first := kube.QoSParamKeys(kube.CeilingIOPS)
	first[0] = "tampered"

	if second := kube.QoSParamKeys(kube.CeilingIOPS); second[0] != kube.ParamMaxIOPS {
		t.Errorf("the key list was mutated through a caller's copy: %v", second)
	}
}

// Properties parses either generation the same way, which is what lets a
// consumer read a class without knowing which operator generated it.
func TestPropertiesReadEitherQoSGeneration(t *testing.T) {
	current := map[string]string{
		kube.ParamPool:                 "tenant-a",
		kube.ParamMaxIOPS:              "20000",
		kube.ParamMaxMBytesPerSec:      "512",
		kube.ParamMaxReadMBytesPerSec:  "300",
		kube.ParamMaxWriteMBytesPerSec: "200",
	}
	older := map[string]string{
		kube.ParamPool:        "tenant-a",
		kube.ParamQoSRWIOPS:   "20000",
		kube.ParamQoSRWMBytes: "512",
		kube.ParamQoSRMBytes:  "300",
		kube.ParamQoSWMBytes:  "200",
	}

	want := kube.QoSLimits{RWIOPS: 20000, RWMBytes: 512, RMBytes: 300, WMBytes: 200}
	for name, params := range map[string]map[string]string{
		"the current spelling": current,
		"the older spelling":   older,
	} {
		t.Run(name, func(t *testing.T) {
			props, err := kube.PropertiesFromStorageClass(storageClassWith(params))
			if err != nil {
				t.Fatalf("PropertiesFromStorageClass: %v", err)
			}
			if props.QoS != want {
				t.Errorf("QoS = %+v, want %+v", props.QoS, want)
			}
		})
	}
}

// A ceiling that is present and not a number is an error naming the key it was
// actually written under, so a reader is sent to a key their class has.
func TestAnUnparsableCeilingNamesTheKeyItCameFrom(t *testing.T) {
	params := map[string]string{kube.ParamPool: "tenant-a", kube.ParamQoSRWIOPS: "lots"}

	_, err := kube.PropertiesFromStorageClass(storageClassWith(params))
	if err == nil {
		t.Fatal("an unparsable ceiling was accepted")
	}
	if !contains(err.Error(), kube.ParamQoSRWIOPS) {
		t.Errorf("the error %q does not name %q", err, kube.ParamQoSRWIOPS)
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
