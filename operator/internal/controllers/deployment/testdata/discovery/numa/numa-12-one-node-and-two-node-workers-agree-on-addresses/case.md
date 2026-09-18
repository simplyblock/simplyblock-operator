# NUMA-12

**Mutation.** A 1-node worker and a 2-node worker whose chosen addresses coincide

**Expected.** One group: grouping reads addresses and the interface, never the topology

**Harness.** `CM`

**Note.** Both workers hand over the same two addresses, and the grouper reads the addresses rather than the topology, so they share a group.
