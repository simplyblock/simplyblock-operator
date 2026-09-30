// What changes about a probe Job that is asked to reclaim controllers.
//
// Two things, and they go together: the flag that tells the probe to do it, and
// the writable sysfs it needs to. A Job carrying one without the other fails on
// the machine rather than here — the flag with a read-only /sys gets a
// permission error per controller, and the writable mount without the flag
// hands a read-only process a writable host tree for nothing.

package nodeprobe

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// reclaimingJob builds the Job a run that reclaims would create.
func reclaimingJob(t *testing.T) *batchv1.Job {
	t.Helper()
	job, err := Job(JobOptions{
		Namespace:               "simplyblock",
		Run:                     testRun,
		Node:                    testNode,
		Image:                   "quay.io/simplyblock-io/simplyblock-operator:26.4.0",
		ServiceAccountName:      testAccount,
		ReclaimUserspaceDevices: true,
	})
	if err != nil {
		t.Fatalf("build the probe Job: %v", err)
	}
	return job
}

// mountAt finds one of the container's mounts by path.
func mountAt(t *testing.T, c corev1.Container, path string) corev1.VolumeMount {
	t.Helper()
	for _, mount := range c.VolumeMounts {
		if mount.MountPath == path {
			return mount
		}
	}
	t.Fatalf("nothing is mounted at %s", path)
	return corev1.VolumeMount{}
}

func TestAReclaimingJobAsksTheProbeToReclaim(t *testing.T) {
	c := container(t, reclaimingJob(t))

	if !arg(c, "--reclaim-userspace-devices") {
		t.Errorf("the probe is invoked as %v, and nothing there asks it to reclaim", c.Command)
	}
}

// The writable sysfs is the whole privilege difference, so it is asserted
// rather than assumed: a rebind writes driver_override, unbind, and
// drivers_probe, and every one of them is under /sys.
func TestAReclaimingJobMountsSysfsWritable(t *testing.T) {
	c := container(t, reclaimingJob(t))

	if mountAt(t, c, hostSysfsMount).ReadOnly {
		t.Error("the host's sysfs is mounted read-only, and a rebind writes to it")
	}
}

// A run that was not asked to reclaim keeps the read-only sysfs it has always
// had. The widening is the cost of the reclaim and is paid only by the runs
// that asked for it, so a fleet that never does is no more exposed than before.
func TestAnOrdinaryJobNeitherReclaimsNorGetsAWritableSysfs(t *testing.T) {
	c := container(t, probeJob(t))

	if arg(c, "--reclaim-userspace-devices") {
		t.Error("a run that did not ask to reclaim invokes the probe with the flag anyway")
	}
	if !mountAt(t, c, hostSysfsMount).ReadOnly {
		t.Error("the host's sysfs is mounted writable for a run that only reads it")
	}
}

// The other host trees are unchanged by the reclaim. Only the sysfs a rebind
// writes to is widened: the process table and the os-release files stay
// read-only, and a reclaim has no business in either.
func TestAReclaimingJobWidensNothingElse(t *testing.T) {
	c := container(t, reclaimingJob(t))

	for _, path := range []string{hostProcMount, HostRootMount + "/etc", HostRootMount + "/usr/lib"} {
		if !mountAt(t, c, path).ReadOnly {
			t.Errorf("%s is mounted writable for a reclaiming run, and nothing writes there", path)
		}
	}
}
