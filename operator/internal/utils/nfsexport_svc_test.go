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
	for _, pod := range []string{sts + "-0", sts + "-1", sts + "-4", sts + "-12"} {
		got, ok := MDSStatefulSetNameOfPod(pod)
		if !ok || got != sts {
			t.Errorf("MDSStatefulSetNameOfPod(%q) = %q, %v, want %q", pod, got, ok, sts)
		}
		if MDSServiceName(got) != sts+"-nfs" {
			t.Errorf("pod %s names Service %s", pod, MDSServiceName(got))
		}
	}
}

func TestMDSStatefulSetNameOfPodRefusesWhatIsNotAPodName(t *testing.T) {
	for _, name := range []string{"", "mds", "mds-", "mds-x", "-1"} {
		if got, ok := MDSStatefulSetNameOfPod(name); ok {
			t.Errorf("MDSStatefulSetNameOfPod(%q) = %q, want a refusal", name, got)
		}
	}
}
