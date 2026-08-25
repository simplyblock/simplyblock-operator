# The refactoring catalog, filtered for Go and for this repository

Fowler's catalog, as published at <https://refactoring.guru/refactoring/catalog>,
names 66 refactorings and 24 code smells. The names are worth having: they are
shared vocabulary, so a commit message reading "Introduce Parameter Object on
`matchVolumesToPVs`" says more than "tidied up the signature," and a reviewer who
knows the name knows what to check.

Roughly a third of the catalog does not apply here. **Go has no inheritance**, so
every technique whose mechanism is a class hierarchy is not a thing to translate,
it is a thing to leave out, and importing it wholesale is how a cleanup skill
starts fighting the language. The exclusions are listed at the end with their
reasons, so that this page can be trusted as complete rather than merely partial.

Two entry points:

- **From a measurement.** `scripts/measure.sh` reports a number, `SKILL.md` §4
  names the pass, and this page names the technique.
- **From a smell.** Reading code and noticing something is wrong is the other
  half, and the catalog's real structure is smell to technique. The index below
  is that mapping.

## The smell index

Each row: what the smell looks like in this repository, the pass that owns it, and
the named techniques that resolve it.

| Smell                                             | Here                                                                                                                      | Pass | Techniques                                                                            |
|---------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------|------|---------------------------------------------------------------------------------------|
| **Long Method**                                   | 22 functions over 100 lines in the controllers, longest 183                                                               | 6    | Extract Method, Decompose Conditional, Replace Temp with Query                        |
| **Large Class**                                   | 14 controller files over 600 lines; `storagenodeops_controller.go` at 1,696                                               | 7    | Extract Class, Move Method                                                            |
| **Primitive Obsession**                           | 7 `Phase string` fields that should be `<Kind>Phase`, and three functions that split a volume handle out of a bare string | 8    | Replace Data Value with Object, Replace Magic Number with Symbolic Constant           |
| **Long Parameter List**                           | `StartMigration` takes 8 parameters, `onAllSocketNodesOnline` 7, and four more take 6                                     | 5    | Introduce Parameter Object, Preserve Whole Object, Replace Parameter with Method Call |
| **Data Clumps**                                   | `ctx, apiClient, clusterUUID, snCR` travel together; `apiClient` is a parameter in 17 files                               | 5, 8 | Introduce Parameter Object, Extract Class                                             |
| **Switch Statements**                             | 12 hand-rolled switches on a phase across 7 controllers                                                                   | 3, 8 | Replace Conditional with a declared state graph (`atlas-lib/statemachine`)            |
| **Temporary Field**                               | a struct field set only during one operation                                                                              | 8    | Extract Class, or move it to CR status where a restart can read it                    |
| **Divergent Change**                              | one file changing for unrelated reasons: reconciler, state machine, and topology in one                                   | 7    | Extract Class, Move Method                                                            |
| **Shotgun Surgery**                               | `apiClient()` in 5 controllers; the storage-node ClusterRole in 3 places                                                  | 4    | Move Method, Extract Class, `extract-to-atlas-lib`                                    |
| **Duplicate Code**                                | `find-twins.sh` reports it exactly                                                                                        | 4    | Extract Method, Move Method, Substitute Algorithm                                     |
| **Dead Code**                                     | `waitForNodeInfoReachable`, reached only from tests                                                                       | 1    | delete it                                                                             |
| **Comments**                                      | prose restating the statement below it                                                                                    | 2    | Extract Method with a name that says it, then delete the comment                      |
| **Lazy Class**                                    | a package or type that earns less than its file costs                                                                     | 7    | Inline Class                                                                          |
| **Speculative Generality**                        | an interface with one implementation and no test using it                                                                 | 7    | Collapse it. **See the caveat below**                                                 |
| **Feature Envy**                                  | a controller function using another package's data more than its own                                                      | 7    | Move Method                                                                           |
| **Inappropriate Intimacy**                        | reaching past a package's public surface                                                                                  | 7    | Move Method, Hide Method (unexport)                                                   |
| **Message Chains**                                | `a.B().C().D()`, each link a nil panic waiting                                                                            | 5    | Hide Delegate, Extract Method                                                         |
| **Middle Man**                                    | `operator/internal/webapi` wrapping what `atlas-lib/controlplane` already does                                            | 3    | Remove Middle Man                                                                     |
| **Alternative Classes with Different Interfaces** | two NVMe-oF connect implementations; two control-plane clients                                                            | 3, 4 | Substitute Algorithm, then delete one                                                 |
| **Incomplete Library Class**                      | an `atlas-lib` helper that is unexported, so a consumer copied it                                                         | 3    | export it in place. See `extract-to-atlas-lib`                                        |

**Why Long Parameter List is a correctness smell here, not a style one.**
`StartMigration` takes `volumeUUID, targetNodeUUID, name, namespace string`: four
adjacent parameters of the same type, which the compiler will happily accept in
any order. A parameter object makes each one named at the call site, so a
transposition becomes a compile error instead of a migration pointed at the wrong
node.

**The clearest example in the repository.** Three functions in three controllers
each take a colon-separated string and return four values:

- `persistentvolumeclaim_controller.go:309` — `splitCSIVolumeHandle`, returning `ok bool`
- `replicationslot_controller.go:410` — `splitVolumeHandle`, returning `ok bool`
- `storagebackup_controller.go:859` — `parseSimplyblockVolumeHandle`, returning `error`

That is four smells in one place. Primitive Obsession, because the handle is a
string rather than a type. Duplicate Code, because there are three of them.
Incomplete Library Class, because `lvol.VolumeHandle.Split` already does this and
has no importers. And two of the three signal failure with a boolean while the
third returns an error, so a caller cannot handle them the same way. Passes 3 and
4 own it, and the fix is to adopt the primitive rather than to tidy any of the
three.

**The Speculative Generality caveat.** `atlas-lib` is deliberately interface-first
so that a consumer test needs no kernel, cluster, or control plane. `nvmeof.Connector`
having one production implementation is a seam, not speculation, and collapsing
it would break every fake in both consumers. The smell applies to an abstraction
with no second implementation **and** no test standing in for one.

## The techniques that apply

Grouped as the catalog groups them, in their Go form.

### Composing methods

| Technique                         | In Go                                                                                            |
|-----------------------------------|--------------------------------------------------------------------------------------------------|
| Extract Method                    | a named function or method. The name is the deliverable, not the line count                      |
| Inline Method                     | delete a function whose body is as clear as its name, and whose name adds a hop                  |
| Extract Variable                  | a named local for a subexpression, which is often the cheapest way to explain a condition        |
| Inline Temp                       | delete a local assigned once and used once                                                       |
| Replace Temp with Query           | a small method instead of a field or local holding a derived value, so it cannot go stale        |
| Split Temporary Variable          | one variable per meaning. A reused `err` is fine; a reused `count` is two counts                 |
| Remove Assignments to Parameters  | Go passes by value, so this bites on slices, maps, and pointers, where the caller sees the write |
| Replace Method with Method Object | move a long function's parameters and locals onto a struct, then make the steps its methods      |
| Substitute Algorithm              | replace the body wholesale with a clearer one. The pass that most needs the behavior gate        |

### Moving features between packages

| Technique                | In Go                                                                                                                                         |
|--------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------|
| Move Method / Move Field | move a function or field to the package or type that owns the concern. Across the module boundary this is `extract-to-atlas-lib`              |
| Extract Class            | split one struct into two when its fields fall into two groups changing for different reasons                                                 |
| Inline Class             | fold a type that does too little back into its only user                                                                                      |
| Hide Delegate            | add a method so callers stop chaining through an intermediate                                                                                 |
| Remove Middle Man        | delete a type whose methods only forward. The webapi client is the live example                                                               |
| Introduce Foreign Method | Go forbids adding a method to another package's type, so this is a function in your own package taking that type. Prefer it to a wrapper type |

### Organizing data

| Technique                                   | In Go                                                                                                                                                                                              |
|---------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Replace Magic Number with Symbolic Constant | a named constant next to what it configures. Pass 9                                                                                                                                                |
| Replace Data Value with Object              | a named type instead of a bare `string` or `int`. `type StorageNodeOpsPhase string` with its constants, and `lvol.VolumeHandle` instead of a colon-separated string                                |
| Replace Array with Object                   | a struct instead of a positional slice, or instead of a multi-return whose order has to be memorized. `matchVolumesToPVs` returns six values, one a `bool` failure flag standing beside an `error` |
| Encapsulate Collection                      | return a copy, or an iterator, rather than the internal slice a caller can then append to                                                                                                          |

### Simplifying conditionals

| Technique                                     | In Go                                                                              |
|-----------------------------------------------|------------------------------------------------------------------------------------|
| Replace Nested Conditional with Guard Clauses | the first move of pass 5, and the one that pays most in a reconciler               |
| Decompose Conditional                         | name the condition and name each branch's body                                     |
| Consolidate Conditional Expression            | merge branches with the same body into one named predicate                         |
| Consolidate Duplicate Conditional Fragments   | lift the statement that both branches end with out of both                         |
| Remove Control Flag                           | `break`, `continue`, or `return` instead of a `done` boolean steering a loop       |
| Introduce Assertion                           | Go has no assert, so this is an early return with an error naming the precondition |
| Introduce Null Object                         | a zero value that is usable, or a no-op implementation of an interface             |

### Simplifying calls

| Technique                               | In Go                                                                                                                                                     |
|-----------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------|
| Rename Method                           | the highest value-to-risk refactoring there is, and free inside a package                                                                                 |
| Add Parameter / Remove Parameter        | remove is the one to look for. An unused parameter is `unparam`'s finding and lint catches it                                                             |
| Separate Query from Modifier            | a function that both reads state and writes it cannot be called twice safely. In a reconciler this is the difference between an idempotent step and a bug |
| Introduce Parameter Object              | a struct for a parameter group that travels together. `StartMigration`'s eight, and the `apiClient` clump                                                 |
| Preserve Whole Object                   | pass the CR, not six fields read off it                                                                                                                   |
| Parameterize Method                     | one function with a parameter instead of near-identical siblings                                                                                          |
| Replace Parameter with Explicit Methods | the inverse, when the parameter is a flag selecting unrelated behavior                                                                                    |
| Hide Method                             | unexport. A smaller public surface is a smaller thing to keep compatible                                                                                  |

### Generalization

Only one of the twelve survives the translation:

| Technique         | In Go                                                                                                                                                           |
|-------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Extract Interface | define the interface where it is consumed, naming only the methods that consumer needs. This is how every `atlas-lib` seam is built and why the fakes are small |

## What the catalog has that Go does not

Left out deliberately. If one of these looks applicable, the mechanism being
reached for is probably a hierarchy that does not exist.

| Excluded                                                                                                                                                                                                      | Why                                                                                                    |
|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------|
| Pull Up / Push Down Field, Method, Constructor Body; Extract Subclass, Extract Superclass; Collapse Hierarchy; Form Template Method; Replace Inheritance with Delegation; Replace Delegation with Inheritance | Go has no inheritance. Embedding is composition and does not behave like a base class                  |
| Replace Conditional with Polymorphism; Replace Type Code with Subclasses, with State/Strategy; Replace Subclass with Fields                                                                                   | the same. A dispatch table, a map of funcs, or `atlas-lib/statemachine` is the Go answer               |
| Refused Bequest, Parallel Inheritance Hierarchies                                                                                                                                                             | smells of hierarchies                                                                                  |
| Replace Error Code with Exception; Replace Exception with Test                                                                                                                                                | Go has no exceptions. The useful inversion is below                                                    |
| Self Encapsulate Field, Encapsulate Field, Remove Setting Method                                                                                                                                              | getters and setters over every field are not idiomatic Go                                              |
| Replace Constructor with Factory Method                                                                                                                                                                       | already the norm: `New…` returning an interface or a configured struct                                 |
| Change Value to Reference, Change Reference to Value, Duplicate Observed Data, Change Unidirectional Association to Bidirectional and back                                                                    | object-graph and ORM shaped. Pointer versus value in Go is a mutability and copying decision, not this |
| Data Class (smell)                                                                                                                                                                                            | a struct that is only data is idiomatic here. `nvme.Device` is one on purpose                          |

## What Go needs that the catalog does not name

Three moves come up constantly in this repository and have no entry in the
catalog, because they are consequences of Go's error and interface model.

- **Replace Boolean Result with Error.** The inverse of Replace Error Code with
  Exception. A function returning `(ctrl.Result, bool)` tells the caller that
  something failed but not what, and it cannot be wrapped or matched with
  `errors.Is`. Several reconciler helpers do this today. Return an error, and use
  `atlas-lib/errs` sentinels so the caller can decide.
- **Replace Sentinel String with a Typed Error.** `strings.Contains(err.Error(), …)`
  is a comparison against a message that is free to change. `errs` and
  `errs/class` exist for this, and pass 3 covers it.
- **Narrow the Interface at the Consumer.** Not Extract Interface's usual form:
  when a consumer takes a wide interface and calls two of its methods, declaring a
  two-method interface in the consumer shrinks every fake in every test that
  touches it. This is the Go idiom that makes `atlas-lib`'s seams cheap.

## Using the names

Name the technique in the commit message and in the report. It tells a reviewer
what invariant to check: Extract Method should not change behavior at all, whereas
Substitute Algorithm changes everything except behavior and needs the strongest
evidence on the page. `SKILL.md` §5's two gates apply either way, and the name is
what tells a reader which gate mattered.
