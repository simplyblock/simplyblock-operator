# SIZE-08

**Mutation.** The same report with `WorkerRules: WorkerWasReadable`

**Expected.** The worker is refused, the message quoting the unreadable readings

**Harness.** `GO`

**Note.** Driven in the discovery package with Planner{WorkerRules: []WorkerRule{WorkerHasDevices{}, WorkerWasReadable{}}} over the SIZE-07 report, because the controller never substitutes a worker rule.
