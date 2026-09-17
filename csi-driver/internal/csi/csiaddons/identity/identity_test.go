package identity

import (
	"context"
	"testing"

	"github.com/csi-addons/spec/lib/go/identity"
)

func TestGetIdentity(t *testing.T) {
	s := New("test.csi.simplyblock.io", "v1.2.3")
	resp, err := s.GetIdentity(context.Background(), &identity.GetIdentityRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Name != "test.csi.simplyblock.io" || resp.VendorVersion != "v1.2.3" {
		t.Errorf("GetIdentity = %+v", resp)
	}
}

func TestGetCapabilitiesAdvertisesVolumeReplication(t *testing.T) {
	s := New("test.csi.simplyblock.io", "v1.2.3")
	resp, err := s.GetCapabilities(context.Background(), &identity.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var sawVolumeReplication, sawControllerService bool
	for _, c := range resp.Capabilities {
		if vr := c.GetVolumeReplication(); vr != nil && vr.Type == identity.Capability_VolumeReplication_VOLUME_REPLICATION {
			sawVolumeReplication = true
		}
		if svc := c.GetService(); svc != nil && svc.Type == identity.Capability_Service_CONTROLLER_SERVICE {
			sawControllerService = true
		}
	}
	if !sawVolumeReplication {
		t.Error("capabilities do not advertise VOLUME_REPLICATION")
	}
	if !sawControllerService {
		t.Error("capabilities do not advertise CONTROLLER_SERVICE")
	}
}

func TestProbeReportsReady(t *testing.T) {
	s := New("test.csi.simplyblock.io", "v1.2.3")
	resp, err := s.Probe(context.Background(), &identity.ProbeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Ready left nil means "assume ready" per the spec; asserting nil pins
	// that choice rather than a stray true/false creeping in later.
	if resp.Ready != nil {
		t.Errorf("Ready = %v, want nil (assume ready)", resp.Ready)
	}
}
