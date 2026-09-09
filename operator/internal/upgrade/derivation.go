// The naming extension point. §19 of the design is a list of formulas this
// product uses to build a Kubernetes identifier out of a name somebody chose,
// each with a limit that binds it and an input length that overflows it.
// Declaring each formula as a value rather than burying it in the code that
// writes the object is what lets one check cover all of them, and what lets a
// formula added by a later release be checked without a new check being
// written.

package upgrade

import (
	"context"

	"github.com/simplyblock/atlas/kube"
)

// Model says which resource model a formula belongs to. The distinction is what
// lets a report tell a violation that is breaking a cluster today apart from
// one the migration is about to introduce, and the two are not the same news.
type Model string

const (
	// ModelCurrent is a formula the operator runs today. A violation of one is
	// already a reconcile that retries forever, and it is news whether or not
	// the user upgrades.
	ModelCurrent Model = "current"

	// ModelTarget is a formula the target model introduces, or an existing one
	// whose inputs change. §19.8 has the four routes that take two resources to
	// one derived name, and three of them are this: the DaemonSet, the per-node
	// ConfigMap, and the EndpointSlice are named per StorageNodeSet today
	// precisely so several sets can coexist, and the retirement of §16.1 makes
	// the cluster their parent.
	ModelTarget Model = "target"
)

// Fix is which of §19.5's three resolutions applies to a row. Which one it is
// follows from who owns the name rather than from how long it is, and the three
// do not substitute for one another: bounding an input a cloud owns breaks
// enrollment on a legal node name, and truncating a value the control plane
// correlates on breaks the correlation.
type Fix string

const (
	// FixUseUUID replaces the derived value with an identifier already at
	// hand. It applies when nothing reads the current value and the label
	// exists to be selected on rather than read.
	FixUseUUID Fix = "use a UUID rather than the name"

	// FixBoundInput refuses the long name at creation time. It applies when
	// the name is this API's to refuse, since a field somebody types has no
	// business being 200 characters.
	FixBoundInput Fix = "bound the input with a MaxLength marker on the field"

	// FixTruncateAndHash keeps the value working by shortening it and
	// appending a digest of the whole input. It applies when the name belongs
	// to somebody else, such as a worker a cloud named, or when the old value
	// has to keep working because it already sits inside live PersistentVolume
	// objects or names an object that cannot be renamed.
	FixTruncateAndHash Fix = "truncate the derived value and append a digest of the whole input"

	// FixNone marks a row that cannot realistically overflow. It is declared
	// rather than omitted so the audit is complete, and so a later release
	// widening the formula is caught rather than assumed safe.
	FixNone Fix = "none needed"
)

// Space is whether a derived identifier has to be unique, and if so where,
// which is what decides whether two objects deriving one string is a collision
// or the intended outcome.
//
// Most derived labels are selectors, and repetition is the point of them: every
// worker of a set carries the same set label, and every pod on a draining node
// carries the same drain label. Treating "derived" as "unique" reports those as
// collisions, which is a finding about the checker rather than about the
// cluster.
//
// Where uniqueness is real, the namespace decides its extent. A ConfigMap name
// is unique per namespace, so two StorageNodeSets of one name in two namespaces
// derive one string and collide with nothing. A cluster-scoped object's name,
// and a value that stakes an exclusive claim on a cluster-scoped object, have no
// namespace to be kept apart by.
//
// Every row declares one. There is no default, because the wrong default is how
// a check comes to report three storage nodes on three separate workers as
// three objects fighting over one label.
type Space string

const (
	// SpaceShared is an identifier several objects are expected to derive
	// identically, which is every label that exists to be selected on. The
	// uniqueness check skips these rows, and the length checks do not.
	SpaceShared Space = "no one place"

	// SpaceCluster is an identifier that must be unique across the cluster: a
	// cluster-scoped object's name, or a value that claims a cluster-scoped
	// object exclusively, such as the label a StorageNodeSet claims its
	// workers with.
	SpaceCluster Space = "the cluster"

	// SpaceNamespace is an identifier unique within one namespace, which is
	// every namespaced object's name.
	SpaceNamespace Space = "one namespace"
)

// Derivation is one formula, together with every set of inputs the cluster
// under examination would hand it.
//
// The split between [Derivation.Formula] and [Derivation.Inputs] is what makes
// the two questions §19 asks answerable by one implementation: whether a value
// fits its limit is a property of the formula and one input, and whether two
// values collide is a property of the formula and all of them.
type Derivation interface {
	Rule

	// Formula is how the identifier is built. [kube.Formula] carries the limit
	// that binds it, which is not always the limit of the object the value is
	// written on: a name copied into a label is held to the label's 63 bytes.
	Formula() kube.Formula

	// Written says where the derived value ends up, as a bare noun phrase with
	// no leading article, such as Node label io.simplyblock.node-type, or
	// StorageClass name. A report supplies whatever article its sentence needs,
	// and one carried here reads wrong in the sentence a collision finding
	// builds, which is that two objects derive one StorageClass name.
	Written() string

	// Model says whether the formula is the one running today or the one the
	// target model introduces.
	Model() Model

	// Fix is what a report tells the user to do about a violation of this row.
	Fix() Fix

	// Space is whether the derived identifier has to be unique, and where.
	Space() Space

	// Inputs enumerates what this cluster would hand the formula. Each input
	// names the object the parts came from, so a violation can be reported
	// against something a user can edit.
	Inputs(ctx context.Context, s *Scope) ([]Input, error)
}

// Input is one set of parts a formula would be given, and where they came from.
type Input struct {
	// Source is the object whose name, or names, the parts are.
	Source ObjectRef

	// Parts are the formula's arguments, in order.
	Parts []string
}

// Derive runs the formula over the input.
func (d Input) Derive(formula kube.Formula) kube.Derived {
	return formula.Derive(d.Parts...)
}
