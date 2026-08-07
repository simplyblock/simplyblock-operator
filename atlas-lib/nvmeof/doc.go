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
package nvmeof
