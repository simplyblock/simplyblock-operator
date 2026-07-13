// Package nvme discovers and looks up local NVMe controllers and
// namespaces (for simplyblock, typically NVMe-oF/TCP attachments).
//
// It is read-only: establishing and tearing down fabric connections is
// the job of package nvmeof. The actual enumeration is delegated to
// internal/sysfs so this package stays a stable, testable API surface.
package nvme
