# fleet

The hub half of a multi-cluster simplyblock installation. It runs beside one
shared control plane, and it drives the several Kubernetes clusters that control
plane fronts, each of which runs the operator exactly as a standalone
installation does.

## What it is for

One simplyblock control plane can serve more than one Kubernetes cluster. This
component is what makes that arrangement operable from one place: it holds the
desired shape of every enrolled cluster, ships it down through Open Cluster
Management, and reports back what the member did with it.

## What it is not

It is not a mode of the operator, and the operator is unaffected by its
existence. A standalone installation installs nothing from this component: no
`fleet.simplyblock.io` CustomResourceDefinition, no add-on, and no change to the
operator's RBAC or webhook configurations. That constraint is what keeps the
single-cluster product the thing it already is, and it is the reason this is a
separate binary in a separate module rather than a second mode selected at
startup.

It is also not a copy of what a member holds. No kind here exists in a member,
and no kind in a member exists here. The hub carries intent and names the member
it is for, and the member carries what its operator reconciles. Nothing is
synchronized in either direction, so a mirrored `metadata.generation`, a spec
pruned differently on each side, a UID that does not survive the trip, and a
refused create with nowhere to report itself are absent problems rather than
solved ones.

## The kinds

| Kind                     | Short | Holds                                                                   |
|--------------------------|-------|-------------------------------------------------------------------------|
| `StorageFleet`           | `sf`  | The one control plane, and the fleet's defaults. A singleton            |
| `FleetMember`            | `fm`  | One enrolled Kubernetes cluster, its link, its inventory, and a roll-up |
| `ClusterDeployment`      | `cd`  | The cluster document for one member, and its approval                   |
| `DriverDeployment`       | `dd`  | The CSI driver one member should run                                    |
| `StorageClassDeployment` | `scd` | One `StorageClass` drawing on a pool in one member                      |
| `FleetOperation`         | `fop` | One storage-group operation, shipped into one member                    |

`FleetMember` is the root of the ownership tree: every object that names a member
is owned by it, so detaching a member collects its deployments, its classes, and
its operations. `StorageFleet` owns nothing, because a member outlives an edit to
the fleet's defaults.

## The two binaries

| Binary          | Runs        | State     | Does                                                          |
|-----------------|-------------|-----------|---------------------------------------------------------------|
| `fleet-manager` | On the hub  | A shell   | Reconciles the kinds above into the payloads a member applies |
| `fleet-agent`   | In a member | Not built | Reports what only the member knows, and nothing else          |

`fleet-manager` starts, serves its health probes, and registers no controller
yet, so it reconciles nothing. `fleet-agent` has no `cmd` at all. What is built
is the API group and the admission guard, and the guard needs neither binary:
it is evaluated inside the member's own API server.

The two will share this module because they share a wire contract, and one
module is what keeps a single definition of it from skewing between them. They
will share one image for the reason the operator's image carries its node probe:
the manager renders the add-on template that deploys the agent, and names its own
image for it.

## Building

```bash
make -C fleet build     # generate, vet, and build
make -C fleet test      # the unit tests
make -C fleet lint      # golangci-lint
```

The module resolves `atlas-lib` and the operator through local `replace`
directives, so `make docker-build` builds from the repository root as its
context.
