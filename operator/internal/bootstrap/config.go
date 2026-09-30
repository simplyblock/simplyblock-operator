// What an installation states about the one discovery run a fresh install
// performs by itself.
//
// The chart writes it as a ConfigMap and this package reads it. It is a
// ConfigMap rather than fields on OperatorOps because the run it parameterizes
// does not exist yet when the answers are known: the chart knows them at
// install time, and the object is created later by the operator. It is not the
// operator's flags either, because the draft half of it is spent minutes after
// startup, by a reconciler, rather than by the process that read the flags.
//
// Every field is optional and every absent field means what the operator did
// before this file existed. An installation that never renders the ConfigMap,
// renders it empty, or renders something that cannot be parsed gets exactly
// that behavior, which is what carries an install predating the chart that
// writes it.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// ConfigMapName is what the chart calls the object, in the operator's own
	// namespace. It is fixed rather than configurable because something has to
	// be: a name the operator had to be told would need a place to be told it.
	ConfigMapName = "simplyblock-bootstrap"

	// ConfigKey is the one key in it. The document under it is the chart's
	// `discovery` values verbatim, so what an administrator writes in values.yaml
	// and what the operator parses are the same document.
	ConfigKey = "bootstrap.yaml"
)

// Config is an installation's statement about its initial discovery run.
//
// The document is rooted at the run rather than wrapping it, because the run is
// the whole of what is bootstrapped. Anything else that needs bootstrapping
// later gets its own key in the ConfigMap rather than a sibling field here.
type Config struct {
	// Enabled is false on an installation that raises no run at all, which is
	// what the managed profile renders: the cluster that manages this one
	// raises the run, in the namespace it chooses. Absent means enabled, so
	// that a ConfigMap stating only a device filter still gets its run.
	Enabled *bool `json:"enabled,omitempty"`

	// Name is what the run is called. Empty keeps the operator's own constant.
	Name string `json:"name,omitempty"`

	// Namespace is where the run, its probe Jobs, its reports, and the draft go.
	// Empty keeps the operator's own namespace.
	//
	// It places those four things and nothing else. What follows from it is the
	// ClusterDeploymentConfig reconciler's own behavior rather than this field's:
	// a draft expands into a StorageCluster and its StorageNodes in the draft's
	// namespace, so approving a draft that landed here builds the cluster here.
	Namespace string `json:"namespace,omitempty"`

	// NodeSelector restricts which workers the run inspects, and with it the
	// question of whether there is a worker worth inspecting at all.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations are what the run's probe pods tolerate, and what the draft
	// states for the storage nodes it proposes.
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// EnableControlPlaneNodes lets the run consider the machines that run the
	// API server and etcd, which is what a single-node or combined three-node
	// deployment is.
	EnableControlPlaneNodes *bool `json:"enableControlPlaneNodes,omitempty"`

	// ForceJournalDevice dedicates a journal device on a fleet whose disks do
	// not say which one, by taking one of the equal-smallest disks.
	//
	// It is here because an unattended install is exactly where the refusal it
	// overrides is most expensive: a logical block-device run on a fleet of
	// equal disks fails rather than drafting a document that cannot deploy, and
	// nobody is watching to correct the draft by hand.
	ForceJournalDevice *bool `json:"forceJournalDevice,omitempty"`

	// DeviceFilter narrows which of an inspected worker's devices reach the
	// draft. It is the API's own filter rather than a subset of it: the chart's
	// schema is what states which members an installation may set, and a type
	// here naming fewer would be a second answer to that question that could
	// drift from the first.
	DeviceFilter *simplyblockv1alpha2.DeviceFilter `json:"deviceFilter,omitempty"`

	// Draft is what the run's document proposes.
	Draft DraftConfig `json:"draft,omitempty"`
}

// DraftConfig seeds the ClusterDeploymentConfig the initial run writes.
//
// It carries the fields a probe cannot read and discovery therefore guesses. It
// does not carry the fields a probe reads better than an installer does, and it
// does not carry spec.approved: approving a document is an operation somebody
// performs against a draft they have read, and `helm install` is not where a
// running cluster comes from.
type DraftConfig struct {
	// Name is what the document is called. Empty keeps the derived name.
	Name string `json:"name,omitempty"`

	// EdgeCluster states that this is an edge deployment. Discovery never sets
	// it, and nothing about a worker says it.
	EdgeCluster *bool `json:"edgeCluster,omitempty"`

	// Images are the two the draft would otherwise leave to their defaults.
	Images ImagesConfig `json:"images,omitempty"`

	// Cluster is the layout the draft proposes.
	Cluster ClusterConfig `json:"cluster,omitempty"`
}

// ImagesConfig pins the images that run on a storage node and that nothing else
// in the chart names. The node agent is not here: a draft that states none takes
// the ControlPlane singleton's, which this chart already writes.
type ImagesConfig struct {
	// SPDK is the SPDK image, as a full repository:tag reference.
	SPDK string `json:"spdk,omitempty"`

	// SPDKProxy is the SPDK proxy image, as a full repository:tag reference.
	SPDKProxy string `json:"spdkProxy,omitempty"`
}

// ClusterConfig is the part of a draft's cluster an installation states rather
// than discovery deriving it.
//
// What decides membership is that a wrong guess is expensive to undo, and the
// members are here for two different reasons.
//
// Most are immutable on the StorageCluster the draft expands into: the name, the
// stripe, and the two checksum settings cannot be corrected once the cluster
// exists, and the backend bakes the checksum method into each device at create
// and never re-applies it. A wrong guess there is permanent.
//
// The other two are not immutable and are here anyway. MaxSubsystemCount can be
// changed on the cluster afterward, and is stated because the number discovery
// proposes is the middle of the API's range rather than a reading of anything.
// EnableDriveFormat is spent during provisioning rather than held as cluster
// state, and a wrong guess is undone by restoring a backup, which is worse than
// permanent for the data it formatted.
type ClusterConfig struct {
	// Name is the StorageCluster's name, held to the 63 a cluster name may be
	// and, in practice, to the shorter budget the control plane's derived names
	// impose.
	Name string `json:"name,omitempty"`

	// Stripe is the erasure-coding layout. Discovery derives one from the number
	// of nodes it found, which is a fact about the fleet rather than about how
	// much redundancy the deployment wants.
	Stripe *simplyblockv1alpha2.StripeSpec `json:"stripe,omitempty"`

	// MaxSubsystemCount is how many NVMe-oF subsystems each node serves.
	// Discovery proposes the middle of the API's range and says in the draft that
	// the number is not a reading.
	MaxSubsystemCount *int32 `json:"maxSubsystemCount,omitempty"`

	// EnableDriveFormat formats every device the draft names. Discovery proposes
	// it set, because a drive carrying anything is not usable otherwise.
	EnableDriveFormat *bool `json:"enableDriveFormat,omitempty"`

	// EnableChecksumValidation turns on inline CRC validation of every I/O. The
	// backend bakes the method into each device at cluster create and never
	// re-applies it, so a cluster created without this is one nobody can turn it
	// on for.
	EnableChecksumValidation *bool `json:"enableChecksumValidation,omitempty"`

	// EnableAtomicity4K enforces 4K write atomicity, which is what lets checksum
	// validation run on a device whose logical block size is under the data
	// plane's 4K minimum and that cannot be reformatted.
	//
	// It is an enforcement rather than a reading, and enforcing a guarantee the
	// hardware does not keep is how a torn write becomes a checksum that silently
	// disagrees with it.
	EnableAtomicity4K *bool `json:"enableAtomicity4K,omitempty"`
}

// ErrUnreadable reports that the ConfigMap could not be read at all, as opposed
// to being absent, carrying no key, or carrying a document that will not parse.
//
// The three are not one failure, and the difference decides whether a caller may
// act. A configuration that is absent or unusable was read: what the
// installation states is known, and it is nothing, so the operator's own
// behavior is the right answer. A configuration that could not be read states
// whatever it states, and the caller does not know what — including, on a
// managed installation, that it wants no discovery run at all. Proceeding on the
// defaults there raises the run the installation wrote `enabled: false` to
// suppress, and the run creates objects on every worker in the fleet.
//
// So it is a sentinel and not a message. A caller has to branch on it, and prose
// is not something a caller can branch on.
var ErrUnreadable = errors.New("the installation's bootstrap configuration could not be read")

// Load reads the installation's configuration from its ConfigMap.
//
// It never returns a nil Config. An absent ConfigMap, an absent key, and content
// that cannot be parsed all yield the zero value, which is the behavior the
// operator had before this file existed; the parse failure is returned alongside
// it so a caller can say so once rather than deciding what to do about it.
//
// A failure to read the object at all is different, and is wrapped in
// [ErrUnreadable]: the zero Config comes back with it, and a caller that acts on
// that value is acting on an answer nobody gave.
func Load(ctx context.Context, reader client.Reader, namespace string) (*Config, error) {
	key := client.ObjectKey{Namespace: namespace, Name: ConfigMapName}

	var held corev1.ConfigMap
	switch err := reader.Get(ctx, key, &held); {
	case apierrors.IsNotFound(err):
		return &Config{}, nil
	case err != nil:
		return &Config{}, fmt.Errorf("%w: reading %s: %w", ErrUnreadable, ConfigMapName, err)
	}

	document := strings.TrimSpace(held.Data[ConfigKey])
	if document == "" {
		return &Config{}, nil
	}

	// Not strict. A key this operator does not know is a chart from a later
	// release, and dropping the whole document over one would turn a forward
	// installation into a silently unconfigured one. What catches a typo is the
	// chart's values schema, which refuses it before the ConfigMap is rendered.
	config := &Config{}
	if err := yaml.Unmarshal([]byte(document), config); err != nil {
		return &Config{}, fmt.Errorf("parsing %s of %s: %w", ConfigKey, ConfigMapName, err)
	}
	return config, nil
}

// DiscoveryEnabled reports whether this installation raises the run at all.
// Absent means yes: a ConfigMap stating only a device filter still gets its run.
func (c *Config) DiscoveryEnabled() bool {
	return c == nil || c.Enabled == nil || *c.Enabled
}

// RunName is what the run is called, falling back to the operator's constant.
func (c *Config) RunName(fallback string) string {
	if c == nil {
		return fallback
	}
	return stated(c.Name, fallback)
}

// RunNamespace is where the run and its draft go, falling back to the operator's
// own namespace.
func (c *Config) RunNamespace(fallback string) string {
	if c == nil {
		return fallback
	}
	return stated(c.Namespace, fallback)
}

// DiscoverSpec is the run this installation asks for.
//
// An installation that states no device filter gets the partition waiver, which
// is what the operator raised before this file existed: a machine that has held
// data before carries a table on every disk, so refusing them makes the run meant
// to show a fleet what it has report that it has nothing.
func (c *Config) DiscoverSpec() *simplyblockv1alpha2.DiscoverSpec {
	spec := &simplyblockv1alpha2.DiscoverSpec{
		DeviceFilter: &simplyblockv1alpha2.DeviceFilter{
			EnablePartitionedDevices: ptr.To(true),
		},
	}
	if c == nil {
		return spec
	}

	spec.ConfigName = strings.TrimSpace(c.Draft.Name)
	spec.EnableControlPlaneNodes = c.EnableControlPlaneNodes
	spec.ForceJournalDevice = c.ForceJournalDevice
	if len(c.NodeSelector) > 0 {
		spec.NodeSelector = maps.Clone(c.NodeSelector)
	}
	if len(c.Tolerations) > 0 {
		spec.Tolerations = slices.Clone(c.Tolerations)
	}
	// A stated filter is taken whole. The waiver above is what an installation
	// that decided nothing gets, not a floor under one that decided no: an
	// installation refusing partitioned devices has refused them.
	if c.DeviceFilter != nil {
		spec.DeviceFilter = c.DeviceFilter.DeepCopy()
	}
	return spec
}

// Seed is the cluster layout this installation states, as a partial template for
// discovery to apply over what it derived. It is nil when nothing is stated,
// which is what leaves every number to discovery.
func (c *Config) Seed() *simplyblockv1alpha2.ClusterTemplate {
	if c == nil {
		return nil
	}
	cluster := c.Draft.Cluster

	seed := &simplyblockv1alpha2.ClusterTemplate{
		Name:                     strings.TrimSpace(cluster.Name),
		MaxSubsystemCount:        cluster.MaxSubsystemCount,
		EnableDriveFormat:        cluster.EnableDriveFormat,
		EnableChecksumValidation: cluster.EnableChecksumValidation,
		EnableAtomicity4K:        cluster.EnableAtomicity4K,
	}
	// An empty block is what the chart renders for an installation that decided
	// nothing, so `stripe: {}` is not a statement and must not override the
	// layout discovery derived from the fleet it found.
	if cluster.Stripe != nil &&
		(cluster.Stripe.DataChunks != nil || cluster.Stripe.ParityChunks != nil) {
		seed.Stripe = cluster.Stripe.DeepCopy()
	}

	if seed.Name == "" &&
		seed.Stripe == nil &&
		seed.MaxSubsystemCount == nil &&
		seed.EnableDriveFormat == nil &&
		seed.EnableChecksumValidation == nil &&
		seed.EnableAtomicity4K == nil {
		return nil
	}
	return seed
}

// DraftImages are the images this installation pins, as the API's own block, or
// nil where it pinned none.
//
// The node agent is not among them. A draft that states none takes the
// ControlPlane singleton's image, which is the same artifact the control plane
// runs and the pairing a release was tested as, and the chart already writes that
// object.
func (c *Config) DraftImages() *simplyblockv1alpha2.DeploymentImages {
	if c == nil {
		return nil
	}

	images := &simplyblockv1alpha2.DeploymentImages{}
	if reference := strings.TrimSpace(c.Draft.Images.SPDK); reference != "" {
		images.SPDK = &simplyblockv1alpha2.ImageSpec{Image: reference}
	}
	if reference := strings.TrimSpace(c.Draft.Images.SPDKProxy); reference != "" {
		images.SPDKProxy = &simplyblockv1alpha2.ImageSpec{Image: reference}
	}

	if images.SPDK == nil && images.SPDKProxy == nil {
		return nil
	}
	return images
}

// stated reports a value an installation wrote, or the fallback where it wrote
// nothing. The chart renders every key it knows, so an empty string is the
// ordinary state of a field nobody decided rather than a deliberate blank.
func stated(value, fallback string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return fallback
}
