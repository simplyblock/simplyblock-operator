// Package nvmeof manages NVMe-oF fabric connections: connecting to and
// disconnecting from remote subsystems (TCP transport for simplyblock).
//
// It is the write-side counterpart to package nvme, which only reads.
// Implementations shell out to nvme-cli or write the kernel fabrics
// sysfs interface via internal/exec; the Connector interface keeps that
// detail out of callers and out of tests.
package nvmeof
