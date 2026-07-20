package nvme

import (
	"errors"
	"testing"

	"github.com/simplyblock/atlas/errs"
)

// stubIdentifyNN replaces the Identify ioctl for the duration of a test,
// recording whether it was called and returning a fixed NN.
func stubIdentifyNN(t *testing.T, nn uint32, err error) *bool {
	t.Helper()
	called := false
	prev := identifyNN
	identifyNN = func(devicePath string) (uint32, error) {
		called = true
		return nn, err
	}
	t.Cleanup(func() { identifyNN = prev })
	return &called
}

func liveCtrl() Controller {
	return Controller{ID: "nvme0", DevicePath: "/dev/nvme0", State: controllerStateLive}
}

func TestSubsystemIsMultiNamespace_ConclusiveFromSysfs(t *testing.T) {
	// More than one namespace, or any NSID > 1, is answered without any
	// Identify ioctl.
	cases := map[string]Subsystem{
		"two namespaces": {
			ID:          "nvme-subsys0",
			Controllers: []Controller{liveCtrl()},
			Namespaces:  []Namespace{{ID: 1}, {ID: 2}},
		},
		"single namespace nsid>1": {
			ID:          "nvme-subsys0",
			Controllers: []Controller{liveCtrl()},
			Namespaces:  []Namespace{{ID: 2}},
		},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			called := stubIdentifyNN(t, 1, nil) // NN=1 would say "false" if consulted
			got, err := s.IsMultiNamespace()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !got {
				t.Errorf("IsMultiNamespace = false, want true")
			}
			if *called {
				t.Errorf("Identify ioctl was issued for a conclusive sysfs case")
			}
		})
	}
}

func TestSubsystemIsMultiNamespace_IdentifyFallback(t *testing.T) {
	// The ambiguous case: one namespace at NSID 1 — must consult NN.
	s := Subsystem{
		ID:          "nvme-subsys0",
		Controllers: []Controller{liveCtrl()},
		Namespaces:  []Namespace{{ID: 1}},
	}

	t.Run("NN>1 is multi-namespace", func(t *testing.T) {
		called := stubIdentifyNN(t, 32, nil)
		got, err := s.IsMultiNamespace()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got {
			t.Errorf("IsMultiNamespace = false, want true (NN=32)")
		}
		if !*called {
			t.Errorf("Identify ioctl was not issued for the ambiguous case")
		}
	})

	t.Run("NN==1 is single-namespace", func(t *testing.T) {
		stubIdentifyNN(t, 1, nil)
		got, err := s.IsMultiNamespace()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got {
			t.Errorf("IsMultiNamespace = true, want false (NN=1)")
		}
	})
}

func TestSubsystemIsMultiNamespace_NoLiveController(t *testing.T) {
	s := Subsystem{
		ID:          "nvme-subsys0",
		Controllers: []Controller{{ID: "nvme0", DevicePath: "/dev/nvme0", State: "connecting"}},
		Namespaces:  []Namespace{{ID: 1}},
	}
	called := stubIdentifyNN(t, 32, nil)
	_, err := s.IsMultiNamespace()
	if !errors.Is(err, errs.ErrNotConnected) {
		t.Errorf("err = %v, want ErrNotConnected", err)
	}
	if *called {
		t.Errorf("Identify ioctl was issued despite no live controller")
	}
}

func TestControllerMaxNamespaces_RequiresLive(t *testing.T) {
	stubIdentifyNN(t, 32, nil)
	c := Controller{ID: "nvme0", DevicePath: "/dev/nvme0", State: "resetting"}
	if _, err := c.MaxNamespaces(); !errors.Is(err, errs.ErrNotConnected) {
		t.Errorf("err = %v, want ErrNotConnected for non-live controller", err)
	}
}

func TestDeviceIsMultiNamespace_NSIDFastPath(t *testing.T) {
	// NSID > 1 on the device's own namespace short-circuits before any
	// subsystem inspection or ioctl.
	called := stubIdentifyNN(t, 1, nil)
	d := Device{Namespace: Namespace{ID: 5}}
	got, err := d.IsMultiNamespace()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Errorf("IsMultiNamespace = false, want true (nsid=5)")
	}
	if *called {
		t.Errorf("Identify ioctl was issued for an NSID>1 device")
	}
}
