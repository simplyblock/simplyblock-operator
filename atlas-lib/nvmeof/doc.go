// Package nvmeof manages NVMe-oF fabric connections: connecting to and
// disconnecting from remote subsystems (TCP transport for simplyblock).
//
// It is the write-side counterpart to package nvme, which only reads.
// FabricsConnector talks to the kernel directly — it writes a connect
// options line to /dev/nvme-fabrics and tears a controller down through its
// delete_controller sysfs attribute — so no nvme-cli binary is required. It
// reads controller state back through a nvme.SubsystemResolver. The Connector
// interface keeps these mechanics out of callers and out of tests.
package nvmeof
