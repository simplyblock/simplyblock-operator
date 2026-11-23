// The names of a metadata server's Service and EndpointSlice, which belong to
// the StatefulSet and so stay the same whichever ordinal is active (O-43).

package utils

import "testing"

func TestMDSServiceNameDerivesFromTheStatefulSet(t *testing.T) {
	for _, sts := range []string{"simplyblock-pnfs-mds-0f2ac1d3-d1167e95", "mds-0", "mds-1"} {
		if got, want := MDSServiceName(sts), sts+"-nfs"; got != want {
			t.Errorf("MDSServiceName(%q) = %q, want %q", sts, got, want)
		}
	}
}

func TestEveryOrdinalOfAStatefulSetNamesTheSameService(t *testing.T) {
	const sts = "simplyblock-pnfs-mds-0f2ac1d3-d1167e95"
	for i, pod := range []string{sts + "-0", sts + "-1", sts + "-4", sts + "-12"} {
		got, ordinal, ok := SplitMDSPodName(pod)
		if want := []int32{0, 1, 4, 12}[i]; !ok || got != sts || ordinal != want {
			t.Errorf("SplitMDSPodName(%q) = %q, %d, %v, want %q, %d", pod, got, ordinal, ok, sts, want)
		}
		if MDSServiceName(got) != sts+"-nfs" {
			t.Errorf("pod %s names Service %s", pod, MDSServiceName(got))
		}
	}
}

func TestSplitMDSPodNameRefusesWhatIsNotAPodName(t *testing.T) {
	for _, name := range []string{"", "mds", "mds-", "mds-x", "-1", "mds-+5", "mds-2147483648"} {
		if got, ordinal, ok := SplitMDSPodName(name); ok {
			t.Errorf("SplitMDSPodName(%q) = %q, %d, want a refusal", name, got, ordinal)
		}
	}
}
