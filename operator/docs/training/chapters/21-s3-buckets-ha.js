// Chapter 21: S3 buckets that survive the loss of a node, from setup to operation.
// A bucket here is served by three layers, each with its own way of surviving
// a loss: the gateway pods of an ObjectStore, the pNFS metadata server with its
// warm standby, and the erasure-coded volume under each bucket. The chapter
// builds them from the bottom up, then follows what happens to each when a node
// goes, and ends with the day-to-day operations and what is not there.
TRAINING.register({
  id: 's3-buckets-ha',
  title: 'Highly available S3 buckets: setting up and operating',
  summary: 'An S3 gateway with two replicas over pNFS bucket volumes, with a warm-standby metadata server underneath: what is written, what the operator builds, how each layer survives a lost node, and how buckets are grown, watched, and deleted.',
  goal: 'the learner can set up an ObjectStore whose buckets survive the loss of a gateway node and of the metadata server\'s node, say what each layer does during the loss and what it does not cover, and grow, monitor, and delete buckets safely.',

  sections: [
    {
      heading: 'What a highly available bucket is made of',
      body: [
        'No field turns high availability on for a bucket. An S3 request passes three layers, and each one survives a loss in its own way. A bucket is as available as the weakest of the three, so all three are set up in this chapter.',
        { table: { head: ['Layer', 'What is written', 'What the operator runs', 'What it survives'], rows: [
          ['S3 gateway', 'An ObjectStore with `replicas: 2` or more', 'A Deployment with one pod per node and a ClusterIP Service', 'The loss of a gateway pod or its node'],
          ['pNFS metadata server', '`spec.pnfs.mds.standbys` on the SimplyblockDriver, 1 by default', 'A StatefulSet of QEMU guests per storage cluster: one active, the rest standbys', 'The loss of the active guest or its node, through fencing and takeover'],
          ['Bucket volume', 'Nothing per bucket. The cluster\'s `spec.stripe`', 'One erasure-coded logical volume per bucket, formatted XFS', 'As many storage-node losses as the stripe has parity chunks'],
        ] } },
        'Each bucket is its own simplyblock volume, exported over pNFS and mounted on every node that runs a gateway pod. A gateway pod reads and writes the bucket through that mount: bulk data goes straight to the volume over NVMe-oF, and file metadata goes through the metadata server.',
        'The framing "highly available S3 buckets" was adjusted to the code in four places:',
        { list: [
          'Availability is not a property of a bucket. It comes from the ObjectStore\'s replica count, which defaults to 1, the metadata server\'s standby count, and the cluster\'s stripe. A bucket inherits all three.',
          'The metadata server\'s failover is implemented and covered by unit and operator tests against fakes. No end-to-end, guest-integration, or failure-injection test drives a pair of guests through a takeover, and the takeover depends on a guest kernel patch that is pending review and on SPDK reservation fixes on an unmerged fork branch.',
          'The S3 endpoint the operator builds is a ClusterIP Service. The operator creates no Ingress, Route, LoadBalancer, PodDisruptionBudget, or HorizontalPodAutoscaler, so this chapter stops at clients inside the cluster.',
          'The three designs behind the stack (`design-objectstore.md`, `design-bucket-provisioning.md`, and `design-pnfs-mds-standby.md`) are all in Draft.',
        ] },
        { callout: 'Treat the metadata server\'s takeover as implemented but not validated end to end. Its timings below are the code\'s windows, not measured takeover times.', kind: 'warn' },
      ],
    },
    {
      heading: 'The bottom layer: the metadata server and its standby',
      body: [
        'A pNFS volume needs a metadata server, and the SimplyblockDriver runs it once `spec.pnfs.mds` is set. Unset, the CSI driver refuses every pNFS claim, bucket volumes included. The Helm chart sets it by default (`driver.pnfs.mds.enabled: true`), so on a Helm install the question is how many standbys run and where.',
        'Per storage cluster the operator runs a StatefulSet named `simplyblock-pnfs-mds-` followed by the cluster id, cut to fit and with a digest appended, in the operator\'s namespace. It has `1 + standbys` pods. Each pod runs the NFS server inside a QEMU guest with its own kernel, so no node runs nfsd or needs nfs-utils. The pods carry a required anti-affinity on `kubernetes.io/hostname`, and they schedule only on nodes labeled `storage.simplyblock.io/kvm-capable=true`, merged with `spec.pnfs.mds.nodeSelector`.',
        'The CSI node plugin sets that label. At start it opens `/dev/kvm` for reading and writing and labels its node with the result. A label set by hand, without the plugin\'s `kvm-capable-managed-by: auto-detect` annotation, is left alone. A node therefore needs `/dev/kvm`, which means bare metal or nested virtualization, and a CSI node plugin.',
        { code: 'kubectl get nodes -l storage.simplyblock.io/kvm-capable=true\nkubectl get statefulsets -n simplyblock -o custom-columns=NAME:.metadata.name,ACTIVE:.metadata.annotations.storage\\.simplyblock\\.io/mds-active,EPOCH:.metadata.annotations.storage\\.simplyblock\\.io/mds-epoch', title: 'KVM nodes, and which guest is active' },
        'A standby is a booted guest whose agent answers. It holds no NVMe connection, no mount, and no running nfsd, so a standby cannot write to anything. The active guest\'s state, the NFS client-recovery database, lives on a 1 GiB simplyblock volume named after the StatefulSet with `-state` appended. The guest connects that volume over NVMe-oF itself, so no Kubernetes volume attachment is involved and a pod stuck `Terminating` on a dead node holds nothing a standby needs. The StatefulSet\'s annotations `mds-active`, `mds-active-instance`, `mds-epoch`, and `mds-state-volume` record which guest is active.',
        { list: [
          '`standbys`: 0 to 4, default 1. Each standby needs another KVM node. A standby without one stays `Pending` while the active serves. No event reports that while the active is healthy: `MDSStandbyMissing` appears only once the active looks lost and no standby can take over, so check that every metadata server pod is running. At 0, exports stay unserved after the loss of the active\'s node until the lost pod is deleted and started again elsewhere.',
          '`resources`: per pod, so every standby adds one pod\'s worth. The guest gets the whole cores of the CPU limit, at least one, and the memory limit less 256Mi. The memory limit has to be at least 512Mi. Unset limits are 2 CPUs and 2Gi, and an unset memory request is 512Mi.',
          '`dataNetwork`: attaches each guest to the storage data network through a Multus macvlan on the host interface carrying `subnet`. It needs Multus and the whereabouts IPAM plugin, and a range of at least one address per pod. The CRD refuses a subnet that is not an IPv4 CIDR and a `rangeStart` or `rangeEnd` outside it. Unset, the guests reach the storage nodes through the pod network.',
          '`unhealthyDeadline`: how long a booted guest may stay unhealthy before its pod restarts, 90s when unset, never at `0s`. A restart pauses every pNFS volume of the cluster.',
        ] },
      ],
      examples: [
        { title: 'Helm values for the standby', file: 'examples/s3-buckets-ha/01-helm-values-mds.yaml', notes: [
          { at: 'enabled: true', text: 'The chart default. With `false` the driver carries no `spec.pnfs.mds`, and every pNFS claim, bucket volumes included, is refused.' },
          { at: 'standbys: 1', text: 'Commented out in the chart\'s values, so it takes the driver\'s default of 1. Written here so the standby is a decision that shows up in review.' },
          { at: 'memory: 2Gi', text: 'The guest gets this less 256Mi. Below 512Mi the guest does not boot.' },
          { at: 'subnet: ""', text: 'Empty keeps the guests on the pod network. Setting any of the three data network values makes the chart require all three.' },
        ] },
        { title: 'The SimplyblockDriver with the metadata server written out', file: 'examples/s3-buckets-ha/02-simplyblockdriver-pnfs.yaml', notes: [
          { at: 'name: simplyblock', text: 'A cluster holds one SimplyblockDriver. The chart names it `simplyblock`, and the StatefulSet name starts with this name.' },
          { at: 'standbys: 1', text: 'Two pods in all, on two KVM-capable nodes. A third KVM node is what lets a replacement start while one of the two is down.' },
          { at: 'subnet: 10.10.10.0/24', text: 'A node whose interface holds an address in this subnet can run a guest. The pods prefer nodes without a storage node.' },
          { at: 'rangeEnd: 10.10.10.243', text: 'Four addresses for two pods. A restarted pod can take a different address, so the range holds a little more than one per pod.' },
          { at: 'unhealthyDeadline: 90s', text: 'The runner\'s default, written out. A shorter value restarts a struggling guest sooner, and every restart pauses all pNFS volumes.' },
        ] },
      ],
    },
    {
      heading: 'The pNFS class and the gateway\'s credentials',
      body: [
        'Every bucket volume is a claim against a StorageClass whose `csi.storage.k8s.io/fstype` is `pnfs`. For such a claim the CSI controller creates the volume and writes an NFSExport in the claim\'s namespace, the operator assembles the export on the active metadata server, and the claim binds once the export is `Ready`. Nobody writes NFSExports by hand. The driver refuses a pNFS claim while no SimplyblockDriver configures a metadata server, and a ReadWriteMany claim against a class without the `pnfs` fstype. A refused claim stays unbound, and its bucket stays `Pending`.',
        'The class is also where everything per bucket that is not a size lives. The pool comes from `pool_name`, and QoS and encryption from the `qos_rw_iops`, `qos_rw_mbytes`, `qos_r_mbytes`, `qos_w_mbytes`, and `encryption` parameters. S3Bucket has no pool, QoS, quota, key, or policy field. Two classes are how two kinds of bucket are told apart.',
        'The gateway takes its whole configuration from its environment, and `spec.envFrom` passes Secrets and ConfigMaps into it without the operator reading them. The gateway exits at start without `ROOT_ACCESS_KEY_ID` and `ROOT_SECRET_ACCESS_KEY`. With only those set it serves the root account alone. Further accounts come from a Vault or OpenBao IAM backend, turned on by `VGW_IAM_VAULT_ENDPOINT_URL` and the other `VGW_IAM_VAULT_*` variables in the same Secret, and managed with `versitygw admin ... create-user` against the S3 port.',
        { list: [
          'Each gateway replica caches accounts for `VGW_IAM_CACHE_TTL` seconds, 120 by default. A changed or deleted account takes effect on the other replicas only after that, so a revoked key keeps working for up to two minutes.',
          'There is no key rotation. No design covers it, and the Deployment carries no hash of the Secret, so a changed Secret reaches the gateway only when its pods restart: `kubectl rollout restart deployment/s3-ha -n simplyblock`.',
          'Bucket ACLs, ownership, and bucket policies are set through the S3 API and stored as extended attributes on each bucket\'s volume, not in Vault. Vault sits outside the bucket volumes\' consistency group, and whether it should join one is an open question in the gateway\'s README.',
        ] },
      ],
      examples: [
        { title: 'The pNFS StorageClass', file: 'examples/s3-buckets-ha/03-storageclass-pnfs.yaml', notes: [
          { at: 'fstype: pnfs', text: 'Names a request, not an on-disk format: the volume is formatted XFS on the metadata server, and the gateway nodes mount it over NFS.' },
          { at: 'pool_name: prod-default', text: 'Every bucket from this class lands in this pool, with the pool\'s capacity and QoS.' },
          { at: 'reclaimPolicy: Delete', text: 'Deleting a bucket deletes its claim, and with `Delete` the volume and every object in it go too.' },
          { at: 'allowVolumeExpansion: true', text: 'Without it, raising a bucket\'s `capacityBytes` is refused with the event `CapacityExpansionRefused`.' },
        ] },
        { title: 'The gateway\'s credentials', file: 'examples/s3-buckets-ha/04-credentials-secret.yaml', notes: [
          { at: 'ROOT_ACCESS_KEY_ID', text: 'Required with its secret. The gateway exits at start with `missing required environment variable(s)` when either is absent, and the Deployment never becomes Available.' },
          { at: 'VGW_PORT', text: 'The gateway listens on `:9000` unless this says otherwise. The operator sets the container port and the Service to `spec.port` and does not pass it to the gateway, so the two have to agree.' },
          { at: 'VGW_IAM_CACHE_TTL', text: 'Seconds a replica caches an account. Lower means a revoked key stops working sooner, and Vault is asked more often.' },
        ] },
      ],
    },
    {
      heading: 'The ObjectStore',
      body: [
        'The ObjectStore is the gateway. Its `image` is required and has no default, because the gateway is not released with the operator: it has no tagged releases yet and is published from `main` as `simplyblock/gateway:main` and `:latest`. `port` is required. `region` defaults to `us-east-1`, and every client has to sign for exactly that region. `replicas` defaults to 1, so a highly available gateway states 2 or more.',
        'From the ObjectStore the operator writes, all named after it and owned by it:',
        { list: [
          'A ServiceAccount, a Role, and a RoleBinding that let the gateway get, create, and delete S3Buckets in its namespace and nothing else.',
          'A Deployment whose pods carry `storage.simplyblock.io/objectstore=<name>` and a required anti-affinity on `kubernetes.io/hostname`, so no two land on one node. Each pod has a readiness probe on `:8081/readyz` every 10 seconds, which fails while any bucket directory is not mounted, and a hostPath under `/var/lib/kubelet/plugins/csi.simplyblock.io/s3-gateway-mounts/<namespace>/<name>`, mounted with `HostToContainer` propagation, where the CSI node plugin puts the bucket mounts.',
          'A ClusterIP Service with one port named `s3`, at `spec.port`.',
          'A serving certificate. With `tlsProvider: cert-manager` it is a Certificate against the ClusterIssuer `simplyblock-certificate-authority-issuer`, which has to exist. With `OpenShift`, the default, it is an annotation on the Service that the OpenShift service CA acts on. The gateway serves TLS only and re-reads the certificate every 30 seconds.',
        ] },
        'The ObjectStore moves `Pending`, `Provisioning`, `Ready`. In `Provisioning` it waits up to 5 minutes for the Deployment to report Available and the certificate Ready, and degrades with `DeploymentNotAvailable` or `CertificateNotReady` when either does not come. A `Degraded` ObjectStore returns to `Provisioning` when a Secret or ConfigMap named in `envFrom` is created or written after it degraded, or, when it degraded on the certificate, once the certificate turns Ready, which it checks every minute. `Ready` has no way out: a spec change is applied in place, and a lost gateway pod lowers `status.readyReplicas` while the phase stays `Ready`.',
        'The gateway pods have to run where the CSI node plugin runs, because a bucket reaches a pod through the node plugin on that pod\'s node. A gateway pod on a node without one never receives its buckets.',
        { code: 'kubectl get s3store -n simplyblock\nkubectl get deployment,service,certificate s3-ha -n simplyblock\nkubectl get pods -n simplyblock -l storage.simplyblock.io/objectstore=s3-ha -o wide', title: 'The gateway and what the operator wrote for it' },
      ],
      examples: [
        { title: 'The ObjectStore', file: 'examples/s3-buckets-ha/05-objectstore.yaml', notes: [
          { at: 'image:', text: 'Required. The CRD\'s image pattern needs a tag or a digest. The default pull policy is `Always`, because `main` is a moving tag.' },
          { at: 'port: 9000', text: 'The container port and the Service port. The gateway itself listens on `VGW_PORT`, `:9000` by default, so a different port here needs `VGW_PORT` in the Secret.' },
          { at: 'region: us-east-1', text: 'Clients sign for this region. A request signed for any other is refused with `AuthorizationHeaderMalformed`.' },
          { at: 'replicas: 2', text: 'Two pods on two nodes. A third eligible node is what a replacement or a rolling update needs to start while both are up or one node is down.' },
          { at: 'tlsProvider: cert-manager', text: 'Off OpenShift this has to be stated, because the default `OpenShift` writes an annotation nothing else acts on.' },
          { at: 'name: s3-ha-credentials', text: 'The Secret of the previous file. The operator watches it, so creating it after the ObjectStore degraded starts provisioning again.' },
          { at: 'capacityBytes: 107374182400', text: '100 GiB for every bucket created through S3 CreateBucket. Per bucket, not a total. Changing it affects later buckets only, and it changes the pod template, so every gateway pod restarts.' },
        ] },
      ],
    },
    {
      heading: 'Creating buckets and reaching them',
      body: [
        'Buckets are created through the S3 API. A CreateBucket request reaches one gateway pod, and the gateway does the Kubernetes side itself:',
        { steps: [
          'It refuses a name that is not also a valid Kubernetes object name with `InvalidBucketName`.',
          'It creates an S3Bucket named after the bucket, with `spec` filled from the ObjectStore\'s `spec.buckets` and the annotation `storage.simplyblock.io/s3-owner` set to the caller\'s access key.',
          'It waits up to `GATEWAY_BUCKET_TIMEOUT`, 5 minutes by default, for the S3Bucket to report `Ready`.',
          'It writes the request\'s ACL, ownership controls, tags, and object-lock setting onto the mounted volume, and answers.',
        ] },
        'If the S3Bucket degrades, the gateway answers `InternalError`. If the wait runs out, it answers 503 `ServiceUnavailable`. Either way it deletes the S3Bucket again. A repeated CreateBucket answers `BucketAlreadyOwnedByYou` to the account that created the bucket and `BucketAlreadyExists` to any other.',
        'Behind the S3Bucket the operator creates a ReadWriteMany claim named `s3bucket-` followed by the bucket name, owned by the S3Bucket. Provisioning that claim writes the NFSExport, and the claim binds once the export is assembled on the active metadata server. The operator then asks the CSI node plugin on every node that runs a gateway pod to mount the bucket into the gateway\'s hostPath.',
        { table: { head: ['S3Bucket phase', 'Waits for', 'Bound'], rows: [
          ['`Pending`', 'The claim to bind, checked every 10 seconds, with the event `PersistentVolumeClaimNotBound`', 'No deadline'],
          ['`Provisioning`', 'The ObjectStore to exist, the NFSExport to be `Ready`, and the mount on every node with a gateway pod', '5 minutes'],
          ['`Ready`', 'Nothing. Mounts are reasserted every 5 minutes and when a gateway pod moves', 'None'],
          ['`Degraded`', '`ObjectStoreNotFound`, `NoMatchingExport`, `ExportDegraded`, or `MountDeliveryFailed`', 'Terminal: only deleting the S3Bucket leaves it'],
        ] } },
        { code: 'kubectl get s3bkt,pvc,nfsexp -n simplyblock\nkubectl describe s3bkt team-a-reports -n simplyblock', title: 'The objects that appear for one bucket' },
        'An S3Bucket can also be written directly, for a bucket that needs another size or another class than `spec.buckets` gives. Such a bucket has no owner annotation, and no ACL, ownership, or tags are written for it. How the gateway treats a bucket without ACL metadata is not documented in the repository. Keep its object name equal to `spec.bucketName`: S3 DeleteBucket looks the S3Bucket up by the bucket name.',
        'Clients reach the gateway at `https://s3-ha.simplyblock.svc:9000`, sign for the ObjectStore\'s region, and use path-style addressing. With cert-manager, the CA is the `ca.crt` key of the Secret named after the ObjectStore, which is how the e2e suite trusts the gateway. The repository documents no AWS CLI recipe, and the e2e suite runs awscli, mc, mint, and warp from pods inside the cluster. A client pod is a sketch along these lines:',
        { code: 'kubectl get secret s3-ha -n simplyblock -o jsonpath=\'{.data.ca\\.crt}\' | base64 -d > ca.crt\n\n# inside a pod that has ca.crt and the access key in its environment\naws configure set default.s3.addressing_style path\naws --endpoint-url https://s3-ha.simplyblock.svc:9000 --region us-east-1 --ca-bundle ca.crt s3api create-bucket --bucket team-b-logs\naws --endpoint-url https://s3-ha.simplyblock.svc:9000 --region us-east-1 --ca-bundle ca.crt s3api put-object --bucket team-b-logs --key 2026/report.csv --body report.csv', title: 'A client inside the cluster' },
      ],
      examples: [
        { title: 'An S3Bucket written directly', file: 'examples/s3-buckets-ha/06-s3bucket.yaml', notes: [
          { at: 'name: team-a-reports', text: 'Equal to `bucketName`, so that S3 DeleteBucket finds it. The claim is named from `bucketName` whatever this is: `s3bucket-team-a-reports`.' },
          { at: 'objectStoreRef: s3-ha', text: 'Immutable. A bucket does not move between gateways. An ObjectStore that does not exist, or is being deleted, degrades the bucket with `ObjectStoreNotFound`.' },
          { at: 'bucketName: team-a-reports', text: 'Immutable, 3 to 63 characters of lowercase letters, digits, dots, and hyphens.' },
          { at: 'capacityBytes: 214748364800', text: '200 GiB, the bucket\'s quota. There is no other quota: a full volume is a full bucket.' },
          { at: 'storageClassName: simplyblock-pnfs', text: 'Immutable, and it has to be a `pnfs` class. Against any other class the driver refuses the ReadWriteMany claim, and the bucket stays `Pending`.' },
        ] },
      ],
    },
    {
      heading: 'How each layer survives a loss',
      body: [
        '**The gateway.** The anti-affinity keeps the replicas on different nodes, and the Service sends requests only to Ready pods. When a node goes, the Service stops routing to its pod once the pod reads not Ready. Kubernetes replaces the pod after it evicts it from the lost node, and the replacement needs another eligible node without a gateway pod: more replicas than eligible nodes leaves the extra pods `Pending`. The new pod\'s node gets every bucket mounted when the operator sees the pod, and the 5-minute resync catches what that misses. The ObjectStore stays `Ready` throughout, so watch `status.readyReplicas`.',
        'A node that reboots under a running gateway pod loses its bucket mounts. The pod\'s readiness probe fails, so it is taken out of the Service rather than serving from the node\'s local disk, and it comes back when the next resync mounts the buckets again, within 5 minutes.',
        '**The metadata server.** The operator decides that the active guest is lost in one of two ways:',
        { table: { head: ['Signal', 'What the code does'], rows: [
          ['The active pod has a deletion timestamp, as in a drain', 'Takes over at once'],
          ['The active guest restarted and reports the standby role', 'Takes over at once'],
          ['No csi-link session with the active, or no answer to `Describe`, and its pod or node is not Ready, and a standby with a session reports the standby role', 'Takes over once all of this has held continuously for more than 20 seconds. Any blip, a failed API read, or a change of operator leader restarts the window'],
        ] } },
        'For a lost node the third row starts only when the node controller marks the node NotReady, after the kube-controller-manager\'s `--node-monitor-grace-period`, which the design takes as 40 seconds. The design and a code comment put the takeover about 45 seconds after the node\'s last heartbeat. The grace period plus the 20-second window adds up to 60 seconds, plus up to the 5 seconds between passes while the cluster is settled. No test has measured either figure on a cluster.',
        'A takeover then runs these steps, each pass resuming from the StatefulSet\'s annotations:',
        { steps: [
          'Write the new active and the epoch, increased by one, to the StatefulSet\'s annotations, with optimistic concurrency so two writers cannot both win.',
          'Withdraw the metadata server\'s address, so no client reaches the old guest.',
          'Ask the chosen standby to promote itself. It fences every device first, the state volume and every export, by taking the NVMe persistent reservation from every earlier metadata server key with Preempt and Abort, and mounts nothing if any device fails to fence. Only then does it mount the state volume and start nfsd. The call is bounded at 5 minutes.',
          'Point the address at the new active and hand it the exports, eight at a time. The event `MDSPromoted` names the cause and how long the takeover took.',
        ] },
        'A failed fence is the event `MDSFenceFailed` and a retry at the same epoch, 5 seconds later and doubling to 2 minutes. An old active that comes back reporting the active role is demoted by deleting its pod (`MDSDemoted`), and its writes are already refused by the target because its key was preempted.',
        'To the gateway nodes, which are the pNFS clients, a takeover looks like an NFS server restart under the same identity. Clients reclaim their state in the grace period from the database on the state volume. An export that is not reassembled yet answers `NFS4ERR_DELAY` for up to 60 seconds after nfsd starts. Data on layouts a client already holds goes to the volume over NVMe-oF, so the design expects a takeover to stall metadata operations and lose no acknowledged data. For S3 that means requests stall during the takeover. `design-objectstore.md` adds that objects assembled from a multipart upload are reflinked XFS files, which the kernel serves through the metadata server rather than over a block layout, so I/O to those stalls entirely.',
        { callout: 'The takeover\'s safety rests on prerequisites outside this repository: the guest kernel\'s per-instance reservation key (patch pending review), SPDK reservation fixes and the 64-registrant limit (on the fork branch `fix/spdk-nvmf-reservations`, not merged), and a reservation that survives a change of the volume\'s primary node (not tested). None of it can be verified from the operator\'s code.', kind: 'warn' },
        '**The bucket volume.** Each bucket is one logical volume whose chunks are spread according to the cluster\'s `StorageCluster.spec.stripe`. It survives as many storage-node losses as the stripe has parity chunks: a `1+1` stripe survives one loss, and a `4+2` stripe survives two. The stripe is immutable and cluster-wide, so no bucket can ask for more than the cluster has. The [failure domains chapter](chapter.html?c=failure-domains) covers placing those chunks.',
      ],
    },
    {
      heading: 'Operating the buckets',
      body: [
        '**Scaling the gateway.** Edit `spec.replicas`. The ObjectStore stays `Ready`, and the operator mounts every bucket on each new pod\'s node and withdraws the mounts from a node left without a gateway pod. A scale-down is the Deployment\'s own: no drain, no PodDisruptionBudget. The Deployment carries no strategy of its own, so Kubernetes\' default rolling update applies, and its surge pod needs a node free of a gateway pod. Any change to the pod template rolls every gateway pod: the image, `resources`, placement, `region`, and `spec.buckets`, whose values are environment variables.',
        '**Growing a bucket.** Raise `spec.capacityBytes` on the S3Bucket. While the bucket is `Ready` the operator raises the claim\'s request (`CapacityExpanding`), the CSI resizer grows the volume and records the size on the NFSExport, and the metadata server runs `xfs_growfs`. The bucket stays `Ready`. A class without `allowVolumeExpansion` gives `CapacityExpansionRefused`, and the CRD refuses a decrease. Raising the ObjectStore\'s `spec.buckets.capacityBytes` grows no existing bucket.',
        '**A full bucket.** A write that does not fit answers 507 `InsufficientStorage`. The gateway counts a volume as full when less than the object\'s size is free, or, for a write of unknown size, less than 1 percent of its capacity. Growing the bucket is the remedy.',
        '**Metrics.** The operator\'s metrics are off by default. With `operatorMetrics.enabled: true` in the chart they are served on port 8443 over HTTPS to a scraper whose token is bound to the ClusterRole `simplyblock-operator-metrics-reader`. The metadata server\'s standby adds no metric.',
        { table: { head: ['Metric', 'What it says'], rows: [
          ['`simplyblock_objectstore_phase_state`', '1 for the phase an ObjectStore is in'],
          ['`simplyblock_objectstore_ready_replicas_count`', 'Its `status.readyReplicas`, the number to alert on'],
          ['`simplyblock_objectstore_bucket_phase_state`', '1 for the phase a bucket is in'],
          ['`simplyblock_objectstore_bucket_provisioning_seconds`', 'Time from a bucket\'s creation to `Ready`'],
          ['`simplyblock_objectstore_mount_delivery_calls_total`', 'Mount and withdrawal calls by node, operation, and outcome'],
        ] } },
        '**Events.** The ObjectStore reports `ObjectStoreReady`, `DeploymentNotAvailable`, `CertificateNotReady`, `Recovering`, and `BucketsRemaining`. An S3Bucket reports `BucketReady`, `PersistentVolumeClaimNotBound`, `ObjectStoreNotFound`, `NoMatchingExport`, `ExportDegraded`, `MountDeliveryFailed`, `CapacityExpanding`, and `CapacityExpansionRefused`. The metadata server reports on the SimplyblockDriver: `MDSPromoted`, `MDSFenced`, `MDSFenceFailed`, `MDSFailoverHeld`, `MDSStandbyMissing`, `MDSDemoted`, `MDSScaleHeld`, and `MDSHostNotAllowed`. `MDSPromoted` and `MDSFenceFailed` are also posted on every NFSExport of the cluster. The NFSExport\'s condition `MDSReady` turns False while its metadata server cannot serve it, with the phase left at `Ready`.',
        '**Deleting a bucket.** Empty it, then delete it with S3 DeleteBucket, which refuses a bucket that still holds objects with `BucketNotEmpty`, answers `OperationAborted` for one still being created, and answers only once the S3Bucket is gone. `kubectl delete s3bkt` does not check for objects: the operator withdraws the mounts and deletes the claim, and with `reclaimPolicy: Delete` the data is gone.',
        '**Deleting the ObjectStore.** Its finalizer `storage.simplyblock.io/objectstore-buckets` holds the delete while any S3Bucket names it, with the condition `BucketsRemaining` listing them, and the gateway keeps serving them meanwhile. Delete the buckets first.',
        'Not there today, and not to be planned on:',
        { list: [
          'Per-bucket quota, access key, or policy fields. The quota is the volume size, and keys and policies go through the S3 API.',
          'Key rotation, and an IAM change that takes effect on every replica at once.',
          'Bucket backup. Nothing is bucket-specific, a StorageBackup of a bucket\'s claim is neither designed nor tested, and a pNFS snapshot is taken without freezing the filesystem. The [backup chapter](chapter.html?c=backup-restore) does not apply to buckets.',
          'Object versioning. It is deferred by design, and the gateway answers `VersioningNotConfigured`.',
        ] },
      ],
      examples: [
        { title: 'The bucket grown to 500 GiB', file: 'examples/s3-buckets-ha/07-s3bucket-grown.yaml', notes: [
          { at: 'capacityBytes: 536870912000', text: 'Raised from 200 GiB. Applied while the bucket is `Ready`. A lower value than the stored one is refused with `capacityBytes cannot decrease`.' },
          { at: 'storageClassName: simplyblock-pnfs', text: 'Unchanged and unchangeable. A bucket that needs another class is a new bucket.' },
        ] },
      ],
    },
    {
      heading: 'What goes wrong',
      body: [
        { pitfalls: [
          { symptom: 'The ObjectStore degrades with `DeploymentNotAvailable` and the gateway pods restart in a loop.', cause: 'The Secret in `envFrom` is missing or lacks `ROOT_ACCESS_KEY_ID` or `ROOT_SECRET_ACCESS_KEY`, and the gateway exits at start.', fix: 'Create or correct the Secret. The operator notices the write and provisions again.' },
          { symptom: 'Off OpenShift, the gateway pods stay in `ContainerCreating` and the ObjectStore degrades with `DeploymentNotAvailable`.', cause: '`tlsProvider` was left at its default `OpenShift`, so nothing issues the TLS Secret the pods mount.', fix: 'Set `tlsProvider: cert-manager`, with the ClusterIssuer `simplyblock-certificate-authority-issuer` in place.' },
          { symptom: 'The ObjectStore degrades with `CertificateNotReady`.', cause: 'The ClusterIssuer `simplyblock-certificate-authority-issuer` does not exist or cannot issue.', fix: 'Fix the issuer. The ObjectStore checks the certificate every minute while degraded and provisions again once it is Ready.' },
          { symptom: 'The ObjectStore is `Ready`, and every request to the Service times out or is refused.', cause: '`spec.port` is not 9000 and the Secret has no `VGW_PORT`, so the gateway listens on 9000 while the Service sends to `spec.port`.', fix: 'Set `VGW_PORT` to the same port in the Secret and restart the gateway pods, or use port 9000.' },
          { symptom: 'One gateway pod stays `Pending`, and a rolling update of the image never finishes.', cause: 'The required anti-affinity allows one gateway pod per node, and there are no more eligible nodes than replicas.', fix: 'Add an eligible node or widen `nodeSelector`. Keep one spare eligible node for replacements and rollouts.' },
          { symptom: 'CreateBucket answers 503 or `InternalError` after minutes, and no bucket exists afterward.', cause: 'The S3Bucket did not reach `Ready` within 5 minutes: the class is not `pnfs`, no metadata server serves the export, or a node with a gateway pod has no CSI node plugin session. The gateway deleted the S3Bucket again.', fix: 'Write the same bucket as an S3Bucket directly. It stays in the phase where it stopped, with the event that says why. Fix the cause, delete it, and create the bucket again.' },
          { symptom: 'Every new bucket stays in `Pending` with the event `PersistentVolumeClaimNotBound`.', cause: 'The claim cannot bind: `spec.pnfs.mds` is unset, so the driver refuses the claim, or no node carries `storage.simplyblock.io/kvm-capable=true`, so the NFSExport stays `Pending`.', fix: 'Turn the metadata server on and make sure at least `1 + standbys` nodes expose `/dev/kvm` and run the CSI node plugin. The claim binds on the provisioner\'s next retry.' },
          { symptom: 'A metadata server pod stays `Pending`, and after a loss of the active the event `MDSStandbyMissing` appears instead of a takeover.', cause: 'Fewer KVM-capable nodes than `1 + standbys`, or, with `dataNetwork`, no free address or no further node on the subnet.', fix: 'Add a KVM node or widen the address range. The active serves meanwhile, with no standby to take over, and nothing but the `Pending` pod shows it.' },
          { symptom: 'A revoked access key still works for a short while.', cause: 'Each gateway replica caches accounts for `VGW_IAM_CACHE_TTL`, 120 seconds by default.', fix: 'Expected. Lower the TTL in the Secret and restart the gateway pods if two minutes is too long.' },
          { symptom: 'S3 DeleteBucket answers `NoSuchBucket` for a bucket that clients use.', cause: 'The bucket was written as an S3Bucket whose object name differs from `spec.bucketName`, and the gateway looks the S3Bucket up by the bucket name.', fix: 'Empty it and delete the S3Bucket with kubectl. Name direct S3Buckets after their bucket.' },
          { symptom: 'Deleting the ObjectStore hangs.', cause: 'Its finalizer waits for every S3Bucket that names it, as `BucketsRemaining` says.', fix: 'Empty and delete those buckets. The delete completes when the last one is gone.' },
        ] },
      ],
    },
  ],

  questions: [
    { topic: 'Layers and limits', cat: 'concept', text: 'Which objects does the operator create from an ObjectStore?', options: ['A Deployment of gateway pods', 'A ClusterIP Service', 'A PodDisruptionBudget for the gateway pods', 'An Ingress for the S3 endpoint', 'A Role that lets the gateway create and delete S3Buckets'], correct: [0, 1, 4], explain: 'The operator writes a ServiceAccount, a Role and its binding, the Deployment, the ClusterIP Service, and with cert-manager a Certificate. A PodDisruptionBudget and any external exposure are left to the deployer.' },
    { topic: 'Layers and limits', cat: 'concept', text: 'Which of these does the operator provide for reaching the gateway from outside the cluster?', options: ['An Ingress', 'An OpenShift Route', 'A LoadBalancer Service', 'A NodePort Service', 'A Gateway API route'], correct: [], explain: 'None. The Service is ClusterIP, and the design lists external exposure as a non-goal that the deployer handles like for any other in-cluster Service.' },
    { topic: 'Layers and limits', cat: 'concept', text: 'Which settings decide whether a bucket survives the loss of a node?', options: ['`replicas` on the ObjectStore, 2 or more', '`standbys` on the SimplyblockDriver\'s metadata server, 1 or more', 'The cluster\'s `spec.stripe` having parity chunks', 'A `highAvailability` field on the S3Bucket', 'A replica count on the S3Bucket'], correct: [0, 1, 2], explain: 'Availability comes from the three layers: gateway replicas, the metadata server\'s standby, and the erasure-coded volume. S3Bucket has only `objectStoreRef`, `bucketName`, `capacityBytes`, and `storageClassName`.' },
    { topic: 'Layers and limits', cat: 'concept', text: 'Which statements about the validation of the metadata server\'s failover are true?', options: ['Unit and operator tests cover its decisions against fakes', 'An end-to-end test drives a pair of guests through a takeover', 'The guest kernel patch it depends on is merged', 'The SPDK reservation fixes it depends on are on an unmerged fork branch', 'Failure-injection tests exist for it'], correct: [0, 3], explain: 'The test plan covers the unit and operator rows only, and says no end-to-end, guest-integration, or failure-injection row has a test. The kernel patch is pending review and the SPDK fixes are on `fix/spdk-nvmf-reservations`.' },
    { topic: 'Layers and limits', cat: 'field', text: 'Which fields does an S3Bucket\'s spec have?', options: ['`objectStoreRef`', '`bucketName`', '`capacityBytes`', '`storageClassName`', '`quotaBytes`'], correct: [0, 1, 2, 3], explain: 'Four fields, three of them immutable. The quota is the volume size, and pool, QoS, and encryption come from the StorageClass.' },

    { topic: 'Metadata server', cat: 'concept', text: 'Where do the metadata server\'s pods run?', options: ['On nodes labeled `storage.simplyblock.io/kvm-capable=true`', 'No two on one node', 'On every storage node, as a DaemonSet', 'Inside the gateway pods', 'Only on control-plane nodes'], correct: [0, 1], explain: 'One StatefulSet per storage cluster, with the KVM label merged into its node selector and a required anti-affinity on the hostname. Two guests on one node would be lost together.' },
    { topic: 'Metadata server', cat: 'concept', text: 'How does the `kvm-capable` label get onto a node?', options: ['The CSI node plugin opens `/dev/kvm` at start and labels its node', 'The operator sets it after a discovery run', 'An administrator always has to set it', 'A label set by hand, without the plugin\'s managed-by annotation, is left alone', 'The metadata server StatefulSet sets it when it schedules'], correct: [0, 3], explain: 'The plugin probes `/dev/kvm` for reading and writing once at start and publishes the answer. A hand-set label without its annotation is left as it is, so a golden-image node can assert the capability.' },
    { topic: 'Metadata server', cat: 'concept', text: 'What is true of a standby guest?', options: ['It has booted and its agent answers', 'It holds no NVMe connection', 'It has nothing mounted', 'It runs nfsd read-only', 'It holds the state volume mounted read-only'], correct: [0, 1, 2], explain: 'A standby is ready to be promoted and can write to nothing. It connects and mounts the state volume and the exports only when promoted, after fencing every device.' },
    { topic: 'Metadata server', cat: 'field', text: 'With `standbys: 0`, what happens when the active guest\'s node is lost?', options: ['Exports stay unserved until the lost pod is deleted and started again elsewhere', 'A standby is started on demand within 20 seconds', 'The CRD refuses 0', 'The gateway serves the buckets from local disk', 'The operator raises the StatefulSet\'s replicas on its own'], correct: [0], explain: '0 is allowed. Without a standby there is nobody to take over, and the field\'s documentation says the exports stay unserved until the lost pod restarts elsewhere.' },
    { topic: 'Metadata server', cat: 'field', text: 'Which statements about `spec.pnfs.mds.dataNetwork` are true?', options: ['It needs Multus and the whereabouts IPAM plugin', 'Its range needs at least one address per pod', 'The CRD refuses a subnet that is not an IPv4 CIDR', 'The CRD refuses a `rangeStart` outside the subnet', 'Leaving it unset turns pNFS off'], correct: [0, 1, 2, 3], explain: 'The data network is optional. Unset, the guests reach the storage nodes through the pod network. Set, CEL rules check the subnet and that both ends of the range are in it.' },
    { topic: 'Metadata server', cat: 'field', text: 'The metadata server\'s memory limit is set to 1Gi. How much memory does each guest get?', options: ['768Mi', '1Gi', '512Mi', '256Mi', '896Mi'], correct: [0], explain: 'The guest gets the memory limit less 256Mi for QEMU, so 1Gi gives 768Mi. The limit has to be at least 512Mi.' },

    { topic: 'Gateway and credentials', cat: 'field', text: 'Which variables does the Secret in `envFrom` have to provide for the gateway to start?', options: ['`ROOT_ACCESS_KEY_ID`', '`ROOT_SECRET_ACCESS_KEY`', '`VGW_IAM_VAULT_ENDPOINT_URL`', '`GATEWAY_ROOT_DIR`', '`GATEWAY_REGION`'], correct: [0, 1], explain: 'The root key pair is required and nobody else supplies it. The operator sets `GATEWAY_ROOT_DIR` and `GATEWAY_REGION` itself, and Vault is optional.' },
    { topic: 'Gateway and credentials', cat: 'concept', text: 'An ObjectStore has `port: 8443` and its Secret has no `VGW_PORT`. What happens?', options: ['The gateway listens on 9000 while the Service sends to 8443, so no request arrives', 'The operator sets `VGW_PORT` from `spec.port`', 'The CRD refuses 8443', 'The gateway reads `spec.port` from the API', 'The Service maps 8443 to 9000'], correct: [0], explain: 'The operator uses `spec.port` for the container port and the Service, and does not pass it to the gateway, which listens on `VGW_PORT`, `:9000` by default.' },
    { topic: 'Gateway and credentials', cat: 'concept', text: 'On a cluster without OpenShift, `tlsProvider` is left unset. Which of these follow?', options: ['The provider is `OpenShift`, the default', 'The Service gets an annotation nothing acts on, so the TLS Secret never appears', 'The gateway serves plain HTTP instead', 'After 5 minutes the ObjectStore degrades with `DeploymentNotAvailable`', 'The operator creates a cert-manager Certificate anyway'], correct: [0, 1, 3], explain: 'The pods mount a Secret that is never issued and do not start. The operator treats the OpenShift certificate as ready, so the Deployment is what fails the 5-minute deadline. The gateway never serves plain HTTP when the certificate paths are set.' },
    { topic: 'Gateway and credentials', cat: 'concept', text: 'An IAM user is deleted from Vault. Which statements hold?', options: ['Its key keeps working on a replica that cached it, for up to `VGW_IAM_CACHE_TTL`', 'The default TTL is 120 seconds', 'Deleting its S3Bucket revokes it', 'The root account is stored in Vault too', 'Each gateway replica caches accounts independently'], correct: [0, 1, 4], explain: 'Each replica keeps its own cache, so a change reaches every replica within the TTL, 120 seconds by default. The root account stays in the Secret.' },
    { topic: 'Gateway and credentials', cat: 'action', text: 'How is the root key pair rotated today?', options: ['Change the Secret, then restart the gateway pods', 'Change the Secret, and the operator rolls the pods', 'Set a key rotation field on the ObjectStore', 'Change the Secret, and the gateway re-reads its environment every 30 seconds', 'Follow the rotation design'], correct: [0], explain: 'No design covers rotation and the Deployment carries no hash of the Secret. Environment variables are read at start, so the pods have to restart. The 30-second re-read is the TLS certificate\'s.' },
    { topic: 'Gateway and credentials', cat: 'concept', text: 'What returns a `Degraded` ObjectStore to `Provisioning`?', options: ['A Secret named in `envFrom` created or written after it degraded', 'Its certificate turning Ready, when it degraded on the certificate', 'A timer every 5 minutes', 'Editing `spec.replicas`', 'Deleting one gateway pod'], correct: [0, 1], explain: 'Degraded is left only when what it waited on may have changed: an `envFrom` reference written later, or the certificate, which is checked every minute.' },
    { topic: 'Gateway and credentials', cat: 'concept', text: 'Which of these variables does the operator itself read from the `envFrom` Secret?', options: ['`ROOT_ACCESS_KEY_ID`', '`VGW_PORT`', '`VGW_IAM_CACHE_TTL`', '`VGW_IAM_VAULT_ENDPOINT_URL`', '`GATEWAY_BUCKET_TIMEOUT`'], correct: [], explain: 'None. `envFrom` is passed into the gateway container unread. The operator only watches the named Secrets and ConfigMaps so that a Degraded ObjectStore notices a change.' },

    { topic: 'Buckets', cat: 'action', text: 'Which of these does the gateway do for an S3 CreateBucket request?', options: ['Creates an S3Bucket named after the bucket', 'Waits up to 5 minutes for it to be `Ready`', 'Writes the ACL, ownership, and tags onto the mounted volume', 'Creates the bucket\'s PersistentVolumeClaim itself', 'Records the caller\'s access key in an annotation'], correct: [0, 1, 2, 4], explain: 'The gateway creates the S3Bucket with the `s3-owner` annotation, waits for `Ready`, and writes the metadata. The claim is the operator\'s work, from the S3Bucket.' },
    { topic: 'Buckets', cat: 'concept', text: 'A bucket `team-a-reports` is created through CreateBucket on the ObjectStore `s3-ha`. Which of these exist once it is `Ready`?', options: ['An S3Bucket `team-a-reports`', 'A PersistentVolumeClaim `s3bucket-team-a-reports`', 'An NFSExport for the claim\'s volume', 'A PersistentVolume bound to the claim', 'A mount of the bucket on every node that runs a gateway pod'], correct: [0, 1, 2, 3, 4], explain: 'All of them. `Ready` is reached only once the claim is bound, the NFSExport the CSI controller wrote is `Ready`, and every node with a gateway pod confirmed the mount.' },
    { topic: 'Buckets', cat: 'concept', text: 'Which statements about a `Degraded` S3Bucket are true?', options: ['It is terminal, and only deleting it leaves it', 'It is retried every 10 seconds', 'It returns to `Provisioning` when the NFSExport turns `Ready`', 'A CreateBucket waiting on it answers `InternalError`', 'The operator deletes it on its own'], correct: [0, 3], explain: 'Degraded is reached by declining to act and has no outgoing edge. The gateway sees it while waiting, answers `InternalError`, and deletes the S3Bucket itself. A directly created one stays until somebody deletes it.' },
    { topic: 'Buckets', cat: 'concept', text: 'An S3Bucket is written directly with `metadata.name: reports` and `bucketName: team-a-reports`. Which statements hold?', options: ['S3 DeleteBucket for `team-a-reports` answers `NoSuchBucket`', 'Its claim is named `s3bucket-team-a-reports`', 'The CRD refuses the mismatch', 'The operator renames the object', 'It can still be deleted with kubectl'], correct: [0, 1, 4], explain: 'The gateway looks the S3Bucket up by the bucket name, and the claim is named from `bucketName`. Nothing refuses the mismatch, which is why direct S3Buckets are named after their bucket.' },
    { topic: 'Buckets', cat: 'concept', text: 'Which bucket names does CreateBucket refuse?', options: ['`team_a`', '`a.-b`', '`ab`', '`reports-2026`', '`logs.archive`'], correct: [0, 1, 2], explain: 'A bucket name has to be 3 to 63 characters and also a valid Kubernetes object name. An underscore fails both, a period next to a hyphen fails the object name, and two characters is too short.' },

    { topic: 'Surviving a loss', cat: 'concept', text: 'One of two gateway nodes is lost. Which statements hold?', options: ['The ObjectStore\'s phase stays `Ready`', 'Its `status.readyReplicas` drops', 'The replacement pod needs another eligible node without a gateway pod', 'The operator creates a PodDisruptionBudget to protect the other pod', 'Every bucket turns `Degraded`'], correct: [0, 1, 2], explain: '`Ready` has no outgoing edge, so the replica count is what shows the loss. The anti-affinity needs a free node for the replacement. Buckets that are `Ready` stay `Ready`.' },
    { topic: 'Surviving a loss', cat: 'concept', text: 'For a loss that is not a pod deletion, which conditions have to hold together for more than 20 seconds before a takeover?', options: ['No csi-link session with the active, or no answer to `Describe`', 'The active\'s pod or node is not Ready', 'A standby with a session reports the standby role', 'Every NFSExport\'s `MDSReady` is False', 'The operator has just acquired leadership'], correct: [0, 1, 2], explain: 'Requiring a link signal and a Kubernetes signal together keeps an API server blip or a broken link alone from starting a takeover, and without a standby there is nobody to promote. A new leader restarts the window.' },
    { topic: 'Surviving a loss', cat: 'concept', text: 'The active guest\'s pod is deleted during a node drain. When does the takeover start?', options: ['At once, without the detection window', 'After the 20-second window', 'After the node controller\'s grace period', 'Never, because a drain is held with `MDSFailoverHeld`', 'Only once the pod is gone'], correct: [0], explain: 'A pod with a deletion timestamp is the planned case, and the detector acts on it immediately.' },
    { topic: 'Surviving a loss', cat: 'action', text: 'Which steps does a takeover perform?', options: ['Writes the new active and a higher epoch to the StatefulSet\'s annotations with optimistic concurrency', 'Withdraws the metadata server\'s address', 'Has the target fence every device before it mounts anything', 'Points the address at the new active', 'Hands the exports to it eight at a time'], correct: [0, 1, 2, 3, 4], explain: 'Record, withdraw, promote with fencing first, serve, and reassemble in batches of eight, so every export is back inside the 60-second window in which clients are told to wait.' },
    { topic: 'Surviving a loss', cat: 'concept', text: 'What does a takeover look like to the gateway nodes and their S3 clients?', options: ['An NFS server restart under the same identity', 'Clients reclaim their state in the grace period', 'An export not reassembled yet answers `NFS4ERR_DELAY` for up to 60 seconds', 'S3 requests stall while it runs', 'Acknowledged objects written before it are lost'], correct: [0, 1, 2, 3], explain: 'The gateway nodes are the pNFS clients, so metadata operations stall and S3 requests with them. Data on held layouts goes to the volume directly, so the design expects no acknowledged data to be lost.' },
    { topic: 'Surviving a loss', cat: 'concept', text: 'Which statements about the time to take over after a lost node are true?', options: ['A loss is acted on only after the conditions held for more than 20 seconds', 'For a lost node those conditions start only when the node controller marks the node NotReady', 'The design\'s "about 45 seconds" does not match the 40-second grace period plus the 20-second window', 'An end-to-end test measured the takeover at 45 seconds', 'A failed API read restarts the window'], correct: [0, 1, 2, 4], explain: 'The code\'s windows add up to about 60 seconds after the last heartbeat, plus the time between passes. No test measured either figure.' },

    { topic: 'Operations', cat: 'concept', text: 'Which statements about growing a bucket are true?', options: ['Raising the S3Bucket\'s `capacityBytes` expands its claim', 'The bucket leaves `Ready` while it grows', 'A class without `allowVolumeExpansion` gives `CapacityExpansionRefused`', 'Raising the ObjectStore\'s `spec.buckets.capacityBytes` grows every existing bucket', 'The metadata server runs `xfs_growfs` on the export'], correct: [0, 2, 4], explain: 'The operator patches the claim while the bucket stays `Ready`, and the export\'s new size makes the metadata server grow the filesystem. `spec.buckets` applies to buckets created later.' },
    { topic: 'Operations', cat: 'concept', text: 'What does a client get for a write to a full bucket?', options: ['507 `InsufficientStorage`', '409 `Conflict`', '403 `QuotaExceeded`', '500 `InternalError`', '413 `EntityTooLarge`'], correct: [0], explain: 'The gateway recognizes a full volume, less than the object\'s size or less than 1 percent of capacity free, and answers 507.' },
    { topic: 'Operations', cat: 'action', text: 'What is needed to scrape `simplyblock_objectstore_ready_replicas_count`?', options: ['`operatorMetrics.enabled: true` in the chart', 'A scraper token bound to `simplyblock-operator-metrics-reader`', 'At least one metadata server standby', 'A ServiceMonitor the operator creates', 'The gateway\'s own metrics endpoint'], correct: [0, 1], explain: 'The ObjectStore families are operator metrics, off by default and served on port 8443 behind Kubernetes authorization. The operator creates no ServiceMonitor.' },
    { topic: 'Operations', cat: 'action', text: 'A client sends S3 DeleteBucket for a bucket that still holds objects. What does it get?', options: ['`BucketNotEmpty`', '`NoSuchBucket`', '`OperationAborted`', '`AccessDenied`', '`InsufficientStorage`'], correct: [0], explain: 'The gateway checks emptiness before deleting the S3Bucket. `OperationAborted` is for a bucket still being created. `kubectl delete` makes no such check.' },
    { topic: 'Operations', cat: 'concept', text: 'The ObjectStore is deleted while two S3Buckets still name it. Which of these happen?', options: ['Its finalizer holds the delete', 'The condition `BucketsRemaining` lists the two buckets', 'The gateway keeps serving them', 'The buckets are deleted by cascade', 'Their claims are released at once'], correct: [0, 1, 2], explain: 'The finalizer `storage.simplyblock.io/objectstore-buckets` waits until no S3Bucket references the ObjectStore. Nothing deletes the buckets for you.' },
    { topic: 'Operations', cat: 'concept', text: 'Which of these does the stack support today?', options: ['Object versioning', 'Backing up a bucket with StorageBackup', 'A per-bucket access key field', 'Automatic key rotation', 'An autoscaler the operator creates for the gateway'], correct: [], explain: 'None. Versioning is deferred and answers `VersioningNotConfigured`, bucket backup is neither designed nor tested, keys go through the S3 IAM, rotation has no design, and autoscaling is a non-goal.' },
  ],

  tasks: [
    {
      title: 'A gateway that survives a lost node',
      scenario: 'Write the ObjectStore `s3-ha` for a Kubernetes cluster without OpenShift: two gateway pods, a serving certificate from cert-manager, the root keys in the Secret `s3-ha-credentials`, and buckets on the class `simplyblock-pnfs`.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: {{kind}}\nmetadata:\n  name: s3-ha\n  namespace: simplyblock\nspec:\n  image: docker.io/simplyblock/gateway:main\n  port: 9000\n  replicas: {{replicas}}\n  tlsProvider: {{provider}}\n  envFrom:\n    - secretRef:\n        name: {{secret}}\n  buckets:\n    storageClassName: {{class}}\n    capacityBytes: 107374182400\n',
      blanks: {
        kind: { accept: ['ObjectStore'], hint: 'short name `s3store`' },
        replicas: { accept: ['/([2-9]|[1-9][0-9]+)/'], hint: 'the default of 1 survives nothing' },
        provider: { accept: ['cert-manager'], hint: 'the default only works on OpenShift' },
        secret: { accept: ['s3-ha-credentials'], hint: 'where the root key pair lives' },
        class: { accept: ['simplyblock-pnfs'], hint: 'a class with the `pnfs` fstype' },
      },
      explain: 'Two or more replicas with the required anti-affinity put the gateway on two nodes. Off OpenShift the provider has to be `cert-manager`. The Secret carries the root keys the gateway needs to start, and the bucket class has to be a `pnfs` class.',
    },
    {
      title: 'A metadata server with a standby on the data network',
      scenario: 'Write the metadata server block of the SimplyblockDriver: one standby beside the active guest, attached to the data network `10.10.10.0/24` with addresses from `10.10.10.240`, enough for both pods and at most up to `10.10.10.254`.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: SimplyblockDriver\nmetadata:\n  name: simplyblock\n  namespace: simplyblock\nspec:\n  driverName: csi.simplyblock.io\n  {{pnfs}}:\n    {{mds}}:\n      standbys: {{standbys}}\n      dataNetwork:\n        subnet: 10.10.10.0/24\n        rangeStart: 10.10.10.240\n        rangeEnd: {{rangeEnd}}\n',
      blanks: {
        pnfs: { accept: ['pnfs'], hint: 'the section for ReadWriteMany volumes' },
        mds: { accept: ['mds'], hint: 'the metadata server' },
        standbys: { accept: ['1'], hint: 'one beside the active' },
        rangeEnd: { accept: ['/10\\.10\\.10\\.(24[1-9]|25[0-4])/'], hint: 'at least one address per pod, inside the subnet' },
      },
      explain: '`spec.pnfs.mds` runs the metadata server, and `standbys: 1` gives two pods on two KVM nodes. Two pods need at least two addresses, so the range ends at `10.10.10.241` or later, and the CRD refuses an end outside the subnet.',
    },
    {
      title: 'Growing a bucket',
      scenario: 'The bucket `team-a-reports` was written as an S3Bucket of 200 GiB on the ObjectStore `s3-ha` and the class `simplyblock-pnfs`, named so that S3 DeleteBucket finds it. Grow it to 500 GiB.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: {{kind}}\nmetadata:\n  name: {{name}}\n  namespace: simplyblock\nspec:\n  objectStoreRef: s3-ha\n  bucketName: team-a-reports\n  capacityBytes: {{capacity}}\n  storageClassName: {{class}}\n',
      blanks: {
        kind: { accept: ['S3Bucket'], hint: 'short name `s3bkt`' },
        name: { accept: ['team-a-reports'], hint: 'what the gateway looks a bucket up by' },
        capacity: { accept: ['536870912000'], hint: '500 GiB in bytes' },
        class: { accept: ['simplyblock-pnfs'], hint: 'immutable' },
      },
      explain: 'Growing a bucket is raising `capacityBytes` on its S3Bucket, here to 500 times 1073741824 bytes. The object name equals the bucket name for DeleteBucket, and the class cannot change.',
    },
  ],
});
