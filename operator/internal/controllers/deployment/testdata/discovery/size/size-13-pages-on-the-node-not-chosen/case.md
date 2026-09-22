# SIZE-13

**Mutation.** Pages pre-allocated on node 1 while the placement chooses node 0

**Expected.** Unset, the note naming the worker. The placement does not read huge pages, and the pool on the other node is neither counted nor reported

**Harness.** `CM`
