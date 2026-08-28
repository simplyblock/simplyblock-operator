// Package nvmeof manages NVMe-oF fabric connections: connecting to and
// disconnecting from remote subsystems (TCP transport for simplyblock).
//
// It is the write-side counterpart to package nvme, which only reads.
// FabricsConnector talks to the kernel directly — it writes a connect
// options line to /dev/nvme-fabrics and tears a controller down through its
// delete_controller sysfs attribute — so no nvme-cli binary is required. It
// reads controller state back through a nvme.SubsystemResolver. The Connector
// interface keeps these mechanics out of callers and out of tests.
//
// Path order is part of the contract. Except on a single-node installation,
// the control plane answers a volume's connect request with several paths in
// descending priority — primary, secondary, tertiary — and both directions
// respect an order:
//
//   - Attaching (ConnectPaths) walks the paths in the order given, one at a
//     time, so the highest-priority reachable path is the one that carries
//     I/O first. A path that cannot be established is skipped, not
//     reordered: if the primary node is restarting, the secondary — the
//     current leader — is still attached before the tertiary.
//   - Detaching (Disconnect) works back down: paths that cannot serve I/O
//     (inaccessible, persistent-loss) and non-optimized paths are released
//     first, the optimized path last, so I/O in flight is never pushed onto
//     a path that is about to disappear too.
//
// Connecting is only half of an attach: the namespace block device surfaces a
// moment after the controller goes live. WaitForDevice (and ConnectDevice,
// which connects and then waits) bridges that gap and refuses to guess when
// several namespaces match — see wait.go.
// # Two mechanisms, one set of rules
//
// Attaching can be done two ways, and which one a caller needs is a property of
// the caller rather than of the fabric: FabricsConnector writes the kernel's
// fabrics device directly, CLIConnector shells out to nvme-cli. Only those two
// operations differ — establishing a path, and tearing a controller down — and
// everything above them, the path order above included, is shared. A caller that
// has to speak nvme-cli today therefore gets the same behavior as one that does
// not, and can change mechanism later without changing what it asks for.
//
// hack/nvmet holds the tooling for exercising either against a real fabric. The
// kernel's own NVMe-oF target, driven through configfs, needs no storage backend
// and no control plane, which makes it the cheapest honest test of a connector.
// # Who is connecting
//
// An access-controlled volume — one in a pool with an allowed-hosts list, with
// or without DHCHAP — is reachable only by a named host, so the identity a
// connect presents stops being a detail and becomes part of whether it works at
// all. Three things have to name the same host, and none can be derived from
// the others after the fact:
//
//   - The control plane is asked for the connection on that host's behalf
//     (lvol.ForHost). It authorizes the NQN against the subsystem's allowed
//     hosts and answers with the DHCHAP secret it issued to that host —
//     material it publishes through no other field, and which is therefore
//     carried on the Endpoint and then the Target.
//   - The connect presents that same NQN (WithHostNQN). A connection resolved
//     for one identity and attached under another authenticates with the wrong
//     secret.
//   - The host id goes with the NQN, always, because the kernel keeps the two
//     strictly 1:1 and refuses a hostid it has already seen paired with a
//     different hostnqn. hostIdentity derives it from the NQN rather than
//     letting it fall back to /etc/nvme/hostid independently, which is the
//     pairing that fails — and fails order-dependently, on whichever volume
//     happens to be attached second.
//
// A volume in an open pool needs none of this and is unaffected: with no host
// NQN named, the node's /etc/nvme identity applies as it always did.
// # A connect that succeeds is not a fabric that works
//
// The states that cause outages are the ones where a connect is satisfied at one
// layer of the NVMe object tree while the layer below it is unusable — and the
// check that gates the retry sits at the higher layer, so nothing looks missing
// and the retry spins. A subsystem whose controllers are live but which exports
// no namespace; a live controller at a published endpoint that serves no path to
// the namespace, so the volume runs below its published redundancy while every
// connect answers "already connected." Neither is visible to Connect,
// IsConnected or a wait for a device.
//
// Three pieces address that, layered so the judgment is separable from the
// mechanism:
//
//   - Inspect (inspect.go) diagnoses. It reads only, names each defect
//     positively from state the kernel publishes, and reports how much would
//     have to be torn down to fix it and whose volumes that would disturb.
//   - Repair (repair.go) acts, on one diagnosed defect, with no policy of its
//     own — for callers that make the decision themselves. It takes a
//     ControllerDetacher, so a caller whose connect path lives elsewhere need
//     not supply a whole Connector.
//   - Repairer.Attach (repair.go) is the two together under a policy: connect
//     every path, diagnose, and repair the narrowest defect it is permitted to,
//     never at the cost of another volume's block device and never in a loop.
//     It is what a CSI NodeStage and a node-side guardian both want.
//
// Teardown has its own asymmetry, for the same reason: DetachDevice (detach.go)
// will not disconnect a subsystem that can be shared, because doing so on one
// volume's behalf destroys data that is not the caller's.
package nvmeof
