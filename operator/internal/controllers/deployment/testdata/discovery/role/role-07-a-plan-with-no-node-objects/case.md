# ROLE-07

**Mutation.** No `nodes.yaml` at all

**Expected.** Every worker a `Worker`. The interface is chosen with no address hint

**Harness.** `GO`

**Note.** Driven in the discovery package with a Planner carrying no KubeNodes, because the controller always reads the node objects for the workers its run settled on.
