# NET-73

**Mutation.** A run naming `storage-mgmt` on a cluster that does not serve NetworkAttachmentDefinitions

**Expected.** Host networking, as without the field, and a `NetworkAttachmentsUnavailable` Warning saying the run asked for an attachment the cluster cannot provide

**Harness.** `CM`
