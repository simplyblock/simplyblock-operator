// What the operator does with each state the ConfigMap can be in.
//
// Three of the cases here are the same assertion written three ways, and that is
// the point of them: an absent ConfigMap, an absent key, and content that cannot
// be parsed all have to mean what the operator did before this package existed.
// An installation predating the chart that writes the object is indistinguishable
// from one whose profile renders nothing, and neither may be turned into a
// deployment that behaves differently from the one it was.

package bootstrap

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

const (
	theNamespace = "simplyblock"

	// theOperatorsOwnName is what InitialDiscovery calls the run when an
	// installation states no name of its own.
	theOperatorsOwnName = "initial-discovery"
)

// configMap is the object the chart renders, with whatever document a case wants
// under the one key.
func configMap(document string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: theNamespace},
		Data:       map[string]string{ConfigKey: document},
	}
}

func loadFrom(t *testing.T, objects ...client.Object) (*Config, error) {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return Load(context.Background(), c, theNamespace)
}

// An installation that never rendered the object is one that predates the chart
// writing it, and it keeps the behavior it was installed with.
func TestAnAbsentConfigMapKeepsTheOperatorsOwnBehavior(t *testing.T) {
	config, err := loadFrom(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config == nil {
		t.Fatal("Load returned no config; an absent ConfigMap is a state, not a failure")
	}
	if !config.DiscoveryEnabled() {
		t.Error("an installation that states nothing raises no run")
	}
	if got := config.RunName(theOperatorsOwnName); got != theOperatorsOwnName {
		t.Errorf("RunName = %q, want the operator's own constant", got)
	}
	if got := config.RunNamespace(theNamespace); got != theNamespace {
		t.Errorf("RunNamespace = %q, want the operator's own namespace", got)
	}
	if config.Seed() != nil {
		t.Error("an installation that states nothing seeded the draft")
	}

	// The partition waiver is what the operator raised before this package
	// existed, so an installation that states no filter still gets it.
	spec := config.DiscoverSpec()
	if spec == nil || spec.DeviceFilter == nil {
		t.Fatalf("the run carries no device filter: %+v", spec)
	}
	if !ptr.BoolFromOrFalse(spec.DeviceFilter.EnablePartitionedDevices) {
		t.Error("the partition waiver was dropped; a fleet that has held data reports nothing")
	}
}

// A key that is not there, and content that is not a document, are the same state
// as an absent object. The parse failure travels back so a caller can say so once.
func TestUnreadableContentKeepsTheOperatorsOwnBehavior(t *testing.T) {
	for _, tc := range []struct {
		name      string
		object    client.Object
		wantError bool
	}{
		{
			name: "the key is absent",
			object: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: theNamespace},
				Data:       map[string]string{"something-else": "enabled: false"},
			},
		},
		{
			name:   "the document is empty",
			object: configMap(""),
		},
		{
			name:      "the document cannot be parsed",
			object:    configMap("enabled: true\n  name: [unbalanced\n"),
			wantError: true,
		},
		{
			name:      "the document is a scalar",
			object:    configMap("just a string\n"),
			wantError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, err := loadFrom(t, tc.object)
			if tc.wantError && err == nil {
				t.Error("the content could not be read and nothing said so")
			}
			if !tc.wantError && err != nil {
				t.Errorf("Load: %v", err)
			}
			if config == nil {
				t.Fatal("Load returned no config; a caller has nothing to fall back to")
			}
			if !config.DiscoveryEnabled() {
				t.Error("unreadable content stopped the run rather than being ignored")
			}
			if config.Seed() != nil {
				t.Error("unreadable content seeded the draft")
			}
		})
	}
}

// The managed profile renders one key. The cluster that manages this one raises
// the run, in the namespace it chooses, so this operator raises none.
func TestAManagedInstallationRaisesNoRun(t *testing.T) {
	config, err := loadFrom(t, configMap("enabled: false\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.DiscoveryEnabled() {
		t.Error("a managed installation raised its own discovery run")
	}
}

// The document the chart renders, read back whole. It is one case rather than one
// per field because the failure it guards against is the document not being read
// at all.
func TestTheStatedDocumentIsRead(t *testing.T) {
	config, err := loadFrom(t, configMap(`
enabled: true
name: fleet-discovery
namespace: storage
nodeSelector:
  node-role.kubernetes.io/simplyblock-storage: ""
tolerations:
  - key: storage
    operator: Equal
    value: dedicated
    effect: NoSchedule
enableControlPlaneNodes: true
deviceFilter:
  enableLogicalBlockDevices: true
  enablePartitionedDevices: false
  blockDenyList:
    - /dev/sda
  driveSizeRange: 1T-16T
draft:
  name: fleet-draft
  edgeCluster: true
  images:
    spdk: quay.io/simplyblock-io/spdk:v26.2.6
    spdkProxy: quay.io/simplyblock-io/spdk-proxy:v26.2.6
  cluster:
    name: fleet-cluster
    maxSubsystemCount: 50
    enableDriveFormat: false
    enableChecksumValidation: true
    enableAtomicity4K: true
    stripe:
      dataChunks: 4
      parityChunks: 2
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := config.RunName(theOperatorsOwnName); got != "fleet-discovery" {
		t.Errorf("RunName = %q, want the stated name", got)
	}
	if got := config.RunNamespace(theNamespace); got != "storage" {
		t.Errorf("RunNamespace = %q, want the stated namespace", got)
	}

	spec := config.DiscoverSpec()
	if spec == nil {
		t.Fatal("the run carries no discover block")
	}
	if spec.ConfigName != "fleet-draft" {
		t.Errorf("configName = %q, want the stated draft name", spec.ConfigName)
	}
	if spec.NodeSelector["node-role.kubernetes.io/simplyblock-storage"] != "" ||
		len(spec.NodeSelector) != 1 {
		t.Errorf("nodeSelector = %v, want the stated selector", spec.NodeSelector)
	}
	if len(spec.Tolerations) != 1 || spec.Tolerations[0].Key != "storage" {
		t.Errorf("tolerations = %+v, want the stated one", spec.Tolerations)
	}
	if !ptr.BoolFromOrFalse(spec.EnableControlPlaneNodes) {
		t.Error("enableControlPlaneNodes was dropped")
	}
	if spec.DeviceFilter == nil {
		t.Fatal("the stated device filter was dropped")
	}
	if !ptr.BoolFromOrFalse(spec.DeviceFilter.EnableLogicalBlockDevices) {
		t.Error("enableLogicalBlockDevices was dropped")
	}
	// Stated false, and false is what the run gets: the waiver is the default for
	// an installation that says nothing, not an override of one that says no.
	if ptr.BoolFromOrFalse(spec.DeviceFilter.EnablePartitionedDevices) {
		t.Error("the stated partition refusal was overridden by the default waiver")
	}
	if len(spec.DeviceFilter.BlockDenyList) != 1 || spec.DeviceFilter.BlockDenyList[0] != "/dev/sda" {
		t.Errorf("blockDenyList = %v, want the boot disk", spec.DeviceFilter.BlockDenyList)
	}
	if spec.DeviceFilter.DriveSizeRange != "1T-16T" {
		t.Errorf("driveSizeRange = %q, want the stated range", spec.DeviceFilter.DriveSizeRange)
	}

	seed := config.Seed()
	if seed == nil {
		t.Fatal("the stated cluster layout did not reach the draft")
	}
	if seed.Name != "fleet-cluster" {
		t.Errorf("cluster name = %q, want the stated one", seed.Name)
	}
	if ptr.IntFrom(seed.MaxSubsystemCount, 0) != 50 {
		t.Errorf("maxSubsystemCount = %v, want 50", seed.MaxSubsystemCount)
	}
	if ptr.BoolFromOrFalse(seed.EnableDriveFormat) {
		t.Error("the stated refusal to format drives was dropped")
	}
	if seed.EnableDriveFormat == nil {
		t.Error("enableDriveFormat is unstated, so discovery's own default would format the fleet")
	}
	if !ptr.BoolFromOrFalse(seed.EnableChecksumValidation) {
		t.Error("enableChecksumValidation was dropped; it cannot be turned on afterward")
	}
	if !ptr.BoolFromOrFalse(seed.EnableAtomicity4K) {
		t.Error("enableAtomicity4K was dropped")
	}
	if seed.Stripe == nil ||
		ptr.IntFrom(seed.Stripe.DataChunks, 0) != 4 ||
		ptr.IntFrom(seed.Stripe.ParityChunks, 0) != 2 {
		t.Errorf("stripe = %+v, want the stated 4+2", seed.Stripe)
	}

	if !ptr.BoolFromOrFalse(config.Draft.EdgeCluster) {
		t.Error("edgeCluster was dropped")
	}
	if config.Draft.Images.SPDK == "" || config.Draft.Images.SPDKProxy == "" {
		t.Errorf("images = %+v, want both stated references", config.Draft.Images)
	}
}

// The chart renders every key it knows, so an installation that decided nothing
// renders empty maps and empty blocks rather than omitting them. None of those is
// a statement, and a seed built out of them would override what discovery derived
// with nothing.
func TestAnEmptyBlockIsNotAStatement(t *testing.T) {
	config, err := loadFrom(t, configMap(`
enabled: true
name: ""
namespace: ""
nodeSelector: {}
tolerations: []
draft:
  name: ""
  cluster:
    name: ""
    stripe: {}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := config.RunName(theOperatorsOwnName); got != theOperatorsOwnName {
		t.Errorf("RunName = %q, want the operator's own constant", got)
	}
	if got := config.RunNamespace(theNamespace); got != theNamespace {
		t.Errorf("RunNamespace = %q, want the operator's own namespace", got)
	}
	if seed := config.Seed(); seed != nil {
		t.Errorf("an empty block seeded the draft with %+v", seed)
	}
}

// A stripe stating one half of itself is a statement, and an incomplete one: the
// cluster's layout is immutable, so the half that was left out cannot be supplied
// afterward. It reaches the draft as written rather than being completed here,
// because the draft is what a reviewer reads and a number nobody wrote is one
// nobody can account for.
func TestAHalfStatedStripeReachesTheDraftAsWritten(t *testing.T) {
	config, err := loadFrom(t, configMap("draft:\n  cluster:\n    stripe:\n      dataChunks: 4\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	seed := config.Seed()
	if seed == nil || seed.Stripe == nil {
		t.Fatalf("the stated stripe did not reach the draft: %+v", seed)
	}
	if ptr.IntFrom(seed.Stripe.DataChunks, 0) != 4 {
		t.Errorf("dataChunks = %v, want the stated 4", seed.Stripe.DataChunks)
	}
	if seed.Stripe.ParityChunks != nil {
		t.Errorf("parityChunks = %v, want the unstated half left unstated", seed.Stripe.ParityChunks)
	}
}
