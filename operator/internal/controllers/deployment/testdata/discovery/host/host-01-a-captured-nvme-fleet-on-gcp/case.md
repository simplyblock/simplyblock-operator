# HOST-01

**Mutation.** The six NVMe workers of e2e run 35988660084, replayed from sysfs

**Expected.** One group per worker. One PCI address each, the two namespaces counted once. The extra worker refused: its only disk is its boot disk

**Harness.** `CM`

**Note.** The six storage workers of e2e run 35988660084 and the extra worker beside them, replayed from sysfs. Each storage worker has one NVMe controller exporting two namespaces, a partitioned boot disk on the virtio-scsi bus, and sixteen nbd devices; the extra worker has only its own boot disk, which is an NVMe namespace. Memory is stated rather than captured; see statedMemory. Both captured cases record vcpuCount at the API's minimum of 4 on machines of 16 and 8 cores, because these hosts place no device on a memory node -- GCP exports no NUMA affinity for either bus -- and the sizing reads the cores of the node a device sits on. No synthetic case shows it, because every one of them states a memory node per device.
