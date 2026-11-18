# TMPL-09

**Mutation.** Workers from three providers under a GKE control plane

**Expected.** `spec.environment: GKE`, and the EKS and OKE workers each in a group stating its own `environment`

**Harness.** `CM`
