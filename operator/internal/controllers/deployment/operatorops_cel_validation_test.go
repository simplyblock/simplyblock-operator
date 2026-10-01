// Validation of where an OperatorOps Discover run states the device class it
// scans, run against a real apiserver.
//
// It lives beside cel_validation_test.go because envtest is the only place in
// the tree that starts an apiserver, and both the class rules and the pruning
// of a field the schema does not carry are enforced by the apiserver rather
// than by a webhook.
//
// The runs are unstructured on purpose. What is under test is the shape a
// document is written in, spec.discover.enableLogicalBlockDevices against
// spec.discover.deviceFilter.enableLogicalBlockDevices, and a typed object
// can only express the shape the Go types happen to have.

package deployment

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// aDiscoverRun is an OperatorOps Discover run carrying the given discover
// block, in the shape a manifest would state it.
func aDiscoverRun(discover map[string]any) *unstructured.Unstructured {
	run := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"generateName": "discover-", "namespace": "default"},
		"spec": map[string]any{
			"action":   string(simplyblockv1alpha2.OperatorOpsActionDiscover),
			"discover": discover,
		},
	}}
	run.SetGroupVersionKind(simplyblockv1alpha2.GroupVersion.WithKind("OperatorOps"))
	return run
}

// The class is a statement about the run, so it sits on spec.discover beside
// the run's other statements, and the block lists that need it are accepted
// with it there.
func TestABlockRunStatesItsClassOnTheDiscoverBlock(t *testing.T) {
	apiClient := apiServer(t)

	run := aDiscoverRun(map[string]any{
		"enableLogicalBlockDevices": true,
		"deviceFilter":              map[string]any{"blockAllowList": []any{"/dev/sdb"}},
	})
	if err := apiClient.Create(context.Background(), run, client.FieldValidation("Strict")); err != nil {
		t.Fatalf("a block run with an allow list was refused: %v", err)
	}
}

// The filter narrows devices and does not choose their class, so the class
// stated inside it is a field the schema does not carry.
func TestTheDeviceFilterNoLongerCarriesTheClass(t *testing.T) {
	apiClient := apiServer(t)

	run := aDiscoverRun(map[string]any{
		"deviceFilter": map[string]any{"enableLogicalBlockDevices": true},
	})
	err := apiClient.Create(context.Background(), run, client.FieldValidation("Strict"))
	if err == nil {
		t.Fatal("a run stating its class inside deviceFilter was stored")
	}
	if !strings.Contains(err.Error(), "enableLogicalBlockDevices") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

// The block lists still need the block class, and the PCI filters still refuse
// it, with the class read from spec.discover.
func TestTheFiltersOfTheClassNotScannedAreRefused(t *testing.T) {
	apiClient := apiServer(t)

	for _, testCase := range []struct {
		name     string
		discover map[string]any
		want     string
	}{
		{
			name:     "block lists on an NVMe run",
			discover: map[string]any{"deviceFilter": map[string]any{"blockAllowList": []any{"/dev/sdb"}}},
			want:     "require enableLogicalBlockDevices",
		},
		{
			name: "PCI filters on a block run",
			discover: map[string]any{
				"enableLogicalBlockDevices": true,
				"deviceFilter":              map[string]any{"pcieModel": "PM1733"},
			},
			want: "cannot be combined with enableLogicalBlockDevices",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := apiClient.Create(context.Background(), aDiscoverRun(testCase.discover))
			if err == nil {
				t.Fatal("the run was stored")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("refused for the wrong reason: %v", err)
			}
		})
	}
}
