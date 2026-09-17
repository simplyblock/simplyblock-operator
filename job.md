Kubernetes Operator & CSI Engineer (m/f/d)
simplyblock · Remote (EU time zones) or Berlin · Full-time · Junior to Intermediate

## Why this role exists

At simplyblock, we build Kubernetes-native, software-defined NVMe-over-Fabrics block storage. Customers run it
underneath databases and stateful workloads where downtime is measured in money and latency is measured in microseconds.

Our operator and CSI driver are the part of that product customers actually talk to, and user experience is our key
metric. The operator turns roughly twenty custom resources into a running, self-healing storage cluster, managing
everything nodes, devices, pools, snapshots, backups, replication, migrations, and upgrades. The CSI driver turns
customer PVC requests into NVMe-oF volumes and keeps them attached throughout failovers, reboots, and path changes.

Both are growing fast. We are looking for someone early in their career but already well-versed in Go, who wants to
spend the next few years becoming genuinely excellent at Kubernetes controllers and storage.

We are a small company, which means the same person designs a CRD in the morning and explains to a customer's platform
team in the afternoon why their cluster did what it did. Both halves belong to the role, and the second one is not a
tax on the first: the questions customers ask are where the next fix usually comes from.

## What you'll do

- Write and extend our controllers: reconcilers that never block, complex processes modeled as multistep operations in
  persisted phases. You will start on well-scoped controllers and grow into the ones that move data.
- Work on the CSI driver, from provisioning and attachment down to the NVMe-oF paths, multipath states, and mounts.
  That includes the unglamorous half that matters most: self-healing after something crashed halfway through.
- Design and evolve the API. CRDs are user experience: validation and immutability where they belong, typed phases and
  conditions, and conversions that keep an already-shipped field from breaking a customer.
- Write the failing test first. Every fix and every behavior change starts with a test describing the wanted behavior,
  at the level that proves it: fake clients, a fake simplyblock, or a live cluster with real NVMe devices.
- Reproduce and fix real cluster behavior. A drain that stalls, a migration that loses writes, a node that never gets
  re-probed. You read logs, build the reproducer, and write the regression test that proves the problem. This part of
  the job is hard, and it is also the most fun.
- Work directly with customers. You will join a support conversation with a customer's engineers, read their logs with
  them, explain what their cluster is doing in terms a DBA or a platform team can act on, agree on the next step, and
  carry the outcome back into an issue, a regression test, or a fix.

## What you bring

- Go, in production. One to three years is the shape we have in mind: interfaces, contexts, goroutines, and errors
  hold no surprises, and you have shipped code that survived contact with users.
- Kubernetes fluency as a user, and curiosity as a developer. PVCs, StorageClasses, DaemonSets, RBAC, and lifecycle
  should be familiar. Having written a Kubernetes controller before is a strong plus. Wanting to is the minimum.
- Linux fundamentals. Block devices, filesystems, mounts, `/sys` and `/dev`, and enough of a systems instinct to be
  suspicious of the right things.
- Composure in front of a customer. Under pressure, you hold a professional technical conversation with their engineers:
  listen before diagnosing, separate what is known from what is still a guess, say when you have no answer yet rather
  than promising a date. Underpromise, overdeliver.
- Testing as a habit, not a chore. You would rather spend an hour making a failure reproducible than an afternoon
  guessing.
- Research and prototyping as part of your workflow. You read code to find out how things really work, and you throw
  away the branch that led nowhere without mourning it.
- AI-assisted engineering, with judgment. We use Claude daily for code and log analysis, reproductions, refactors, and
  documentation. Use it to make yourself faster, and learn where it (quietly) does not help.
- Communication that does not wait to be asked. Design documents, commit messages, issue comments, and standups are
  how a distributed team thinks together, and asking early beats guessing quietly.
- The temperament for distributed systems. Comfortable with not knowing yet, the patience to keep digging, and the
  honesty to say "this test passes, but I don't understand why."

## Preferred

- The CSI specification, or any prior work on a storage or networking plugin
- NVMe, NVMe-oF, iSCSI, SPDK, or other userspace / kernel-bypass storage stacks
- kubebuilder, envtest, Ginkgo, or client-go beyond the basics
- Helm chart authoring, OLM bundles, or OpenShift
- gRPC, and reading an OpenAPI-generated client without flinching
- One or more of Python, JavaScript/TypeScript, C/C++, Bash
- Fluent English required with German or other European languages a plus

## What you don't need

- Storage expertise. We will teach you NVMe-oF, and nobody here was born knowing what an ANA group is.
- A CV full of infrastructure companies. Two good years and clear momentum beat five unremarkable ones.
- Certainty. You will be reviewed, mentored, and occasionally told to throw a branch away. That is the job working
  correctly.

## Your first six months

- Month 1: ship small, real changes across the operator and the CSI driver. Validation markers, a webhook check, a
  status condition. Analyzing the first logs to make yourself familiar with the system components.
- Month 2–3: own features end to end. From design document to merged tests. Take your first on-cluster bug from a
  vague symptom to a regression test and successful fix. Sit in on customer calls, first listening, then answering the
  parts you know.
- Month 4–6: you carry weight. The whole system is your territory: you follow a symptom across components to the
  mechanism, and you know where the fix belongs. Your features are loved because the CRD, the events, and the failure
  messages carry the user experience. You take an escalation alone, and we hear about it from the satisfied customer.

## What we offer

- A technically challenging product where your commits reach production clusters, not a sandbox
- Review and mentoring from the engineers who wrote the operator, the CSI driver, and the storage engine underneath
- A codebase with strong and explicit conventions, being built with user experience in mind
- [Compensation range]
- [Equity / VSOP]
- [Remote setup, hardware budget, learning budget, conference attendance]
- [Holiday allowance, other benefits]
