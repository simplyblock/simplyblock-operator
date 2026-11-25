// Chapter: a management cluster for multi-edge-site deployments. The central
// side of chapter 8: one standalone install whose control plane serves every
// edge, with the static admin token the edges authenticate with, the
// management API exposed to them, the TLS decisions that exposure forces, the
// network in both directions, the rule that every edge's StorageCluster name is
// unique, and what the center can see of the fleet. The optional hub path,
// StorageSiteDeployment over Open Cluster Management, closes the lesson, and
// one short section sets superclusters, stretched clusters, and the fleet/
// component apart from this deployment.
//
// The commissioner's framing is adjusted in the lesson where the code differs:
// there is no hub profile and no per-edge registration object, and fleet/ is
// not part of the deployment.
TRAINING.register({
  id: 'edge-management-cluster',
  title: 'A management cluster for multi-edge-site deployments',
  summary: 'One central Kubernetes cluster runs the control plane for many edge sites: the admin token, the exposed management API and its TLS, the network both ways, unique cluster names, monitoring from the center, and the optional StorageSiteDeployment path over Open Cluster Management.',
  goal: 'the reader can set up a management cluster that several edge clusters register with, make the TLS and network decisions that remote edges require, keep their StorageCluster names from colliding, watch every site from the center, and request a site\'s storage from the hub with a StorageSiteDeployment.',

  sections: [
    {
      heading: 'What a management cluster is in simplyblock',
      body: [
        'A fleet of edge sites, each a small Kubernetes cluster with its own NVMe devices, is administered from one place: a **management cluster** that runs the simplyblock control plane, that is FoundationDB and the management API, for every site at once. Each edge runs the operator, the CSI driver, and its storage nodes, and no control plane. Its StorageCluster is created as one backend cluster in the management cluster\'s FoundationDB, and every later call about that cluster and its volumes goes to the management API on the central site.',
        { table: { head: ['Cluster', 'Helm profile', 'ControlPlane source', 'What runs there'], rows: [
          ['the management cluster', '`standalone`', '`local`, with `adminTokenSecretRef`', 'FoundationDB, the management API, the object store, Prometheus, and optionally Grafana and Graylog. No storage nodes.'],
          ['each edge', '`managed`', '`managed`, naming the management API', 'the operator, the CSI driver, the storage nodes, and its own StorageCluster'],
        ] } },
        'The design states the idea in two sentences: several backend clusters do not need several control planes (`design-controlplane.md` §3.1), and a managed control plane may be shared, so the operator on an edge must assume another cluster writes to the same FoundationDB (§5.2). Chapter [Two-node edge clusters with an arbiter](chapter.html?c=two-node-edge) teaches one edge pointing at a central control plane. This chapter is the central side, and the view across many edges.',
        'Three things in the framing are adjusted to the code. First, the Helm chart has no hub or management profile, and nothing on the central side registers an edge: a management cluster is a `standalone` install with an admin token and an exposed API, and an edge registers by creating its StorageCluster. Second, deploying storage on a site from the center is a separate and optional path, the StorageSiteDeployment kind, and it needs Open Cluster Management (OCM). Third, the `fleet/` component of the repository, the hub of the management-hub design, is not part of this deployment: it registers no controller for its kinds and is not in the chart.',
        { callout: 'Some of what follows is decided inside the control plane, which is not part of this repository: whether `SB_ADMIN_TOKENS` accepts more than one token, what the list call returns to a holder of the admin token, how the metrics endpoint labels each cluster, and which edge ports the control plane dials beyond the node agent. The chapter says so where each matters.', kind: 'info' },
      ],
    },
    {
      heading: 'The management cluster: the token, the values, and the ControlPlane',
      body: [
        { steps: [
          'Create the namespace and the Secret holding the admin token under the key `token`. The chart does not create it.',
          'Install the chart with `deployment.profile: standalone`, `controlplane.local.adminTokenSecretRef` naming the Secret, the TLS values of the next section, monitoring turned on, and `discovery.enabled: false`, since the cluster hosts no storage.',
          'Wait for the ControlPlane `simplyblock` to report `Available`.',
          'Expose the management API, as the next section shows, and hand its address and the token to the edges.',
        ] },
        'The token reaches the management API as an environment variable. The operator puts `SB_ADMIN_TOKENS` on the management API container with a `secretKeyRef` to the key `token`, beside `SB_K8S_ADMIN_SERVICE_ACCOUNTS`, which names the operator\'s own service account. It never reads the value, and it does not watch the Secret. Kubernetes resolves such a variable when the container starts, so a new value reaches the API only when its pods restart, and a missing Secret keeps them from starting at all.',
        'A static token exists because a control plane accepts its own cluster\'s Kubernetes identities through a TokenReview, and a token issued by an edge\'s API server cannot be reviewed on the management cluster. The edge operator presents the static token for two calls only, creating its cluster and listing clusters by name, both made before its cluster has a secret of its own.',
        'Monitoring is provisioned once, when the first storage cluster is created, and on a management cluster that is the first edge to register. The chart value `controlplane.observability.enabled: true` renders `observability.enableMonitoring` with the Secret `simplyblock-grafana-secrets`. Turned on after the first edge registered, it sets the environment and provisions nothing.',
        { code: 'kubectl -n simplyblock get controlplane simplyblock\nkubectl -n simplyblock get deployment simplyblock-webappapi \\\n  -o jsonpath=\'{.spec.template.spec.containers[0].env[?(@.name=="SB_ADMIN_TOKENS")]}\'', title: 'Checking the control plane and the token projection' },
      ],
      examples: [
        { title: 'The admin token Secret', file: 'examples/edge-management-cluster/01-admin-token-secret.yaml', notes: [
          { at: 'name: edge-admin-token', text: 'Any name. The ControlPlane refers to it by name, in its own namespace.' },
          { at: 'token: replace', text: 'The only key the management API reads. The edges hold the same value, under `token` or `secret` on their side.' },
        ] },
        { title: 'Helm values for the management cluster', file: 'examples/edge-management-cluster/02-helm-values.yaml', notes: [
          { at: 'profile: standalone', text: 'There is no hub profile. A management cluster is a standalone install.' },
          { at: 'adminTokenSecretRef: edge-admin-token', text: 'Rendered as `spec.source.local.adminTokenSecretRef`. Empty grants no credential beyond the operator\'s own service account, and no edge can create a cluster.' },
          { at: 'mutual_enabled: false', text: 'An edge cannot present a client certificate this cluster issued, so the control plane must not demand one. Immutable once the ControlPlane exists.' },
          { at: 'discovery:', text: 'The cluster hosts no storage, so the chart\'s discovery run, which would inspect its workers and write a draft, is turned off.' },
        ] },
        { title: 'The ControlPlane the chart renders', file: 'examples/edge-management-cluster/03-controlplane.yaml', notes: [
          { at: 'enableMutualTLS: false', text: 'The `tls` block is immutable. Changing it means recreating the control plane, which deletes the FoundationDB and every edge\'s cluster in it.' },
          { at: 'adminTokenSecretRef:', text: 'A LocalObjectReference. Projected into the management API as `SB_ADMIN_TOKENS` from the key `token`.' },
          { at: 'enableMonitoring: true', text: 'Requires `secretRef`, which a CEL rule on the block checks. Takes effect at the first cluster creation only.' },
        ] },
      ],
    },
    {
      heading: 'Exposing the management API: TLS and the network both ways',
      body: [
        'The operator creates the Service `simplyblock-webappapi` as a ClusterIP on port 5000, and nothing that reaches it from outside the cluster. Exposing it is the administrator\'s job, and three facts in the code, each read for this chapter, decide how.',
        { list: [
          '**The serving certificate names in-cluster names only.** With cert-manager, the operator\'s Certificate lists `simplyblock-webappapi`, `simplyblock-webappapi.simplyblock`, the `.svc` name, the fully qualified name, and a wildcard under each of the last two. `spec.source.local.tls` has three fields, `enableTLS`, `enableMutualTLS`, and `provider`, and none adds a name. A load balancer that passes this certificate through to an edge dialing `https://control.central.example.com` fails hostname verification.',
          '**An edge cannot present a client certificate the management cluster issued.** `spec.source.managed` has no client-certificate field. The edge operator presents only what its own release mounts under `/etc/simplyblock/tls`, which, if anything, comes from the edge\'s own cert-manager. So the management cluster is installed with mutual TLS off.',
          '**`caBundleSecretRef` is read by the readiness probe alone.** The probe that sets an edge ControlPlane\'s phase verifies the endpoint against it. The calls that create and manage the cluster go through another client, which trusts the system trust store and the CA bundle the edge release mounts. The CSI plugins, with TLS on, trust only that mounted bundle. A private CA named in `caBundleSecretRef` lets the edge ControlPlane report `Available` while cluster creation fails certificate verification.',
        ] },
        'The configuration this chapter recommends follows from the three:',
        { table: { head: ['Where', 'Decision', 'Why'], rows: [
          ['the management cluster', '`tls.enabled: true`, `tls.mutual_enabled: false`, provider `cert-manager`', 'Callers inside the cluster keep TLS. Edges cannot present a client certificate.'],
          ['in front of `simplyblock-webappapi`', 'an Ingress that terminates TLS with a certificate for the external name, from a CA in the system trust store', 'The serving certificate cannot name the external host.'],
          ['each edge', '`tls.enabled: false`, an `https://` endpoint, no `caBundleSecretRef`', 'The operator and the CSI plugins then verify the endpoint against the system trust store, which holds the Ingress certificate\'s CA.'],
        ] } },
        'Mutual TLS off has one consequence inside the management cluster: FoundationDB\'s peer TLS follows the client-certificate decision, not the serving one, so the database processes talk to each other unencrypted while the management API still serves TLS. On OpenShift the provider is the service CA, which issues serving certificates only, so mutual TLS is off there anyway. A Route can play the Ingress\'s part on OpenShift, and its fields are not documented in this repository.',
        { callout: 'The Ingress connects to a backend that serves HTTPS with a certificate from the management cluster\'s own CA, which is in the Secret `simplyblock-ca-bundle-tls` under `ca.crt`. How a controller is told to re-encrypt to its backend and to trust that CA is the controller\'s own configuration, not documented in this repository, so the example leaves it out. Without it the controller speaks plain HTTP to a TLS port, and the edges see an error from the Ingress, not from simplyblock.', kind: 'warn' },
        'A management API that serves plaintext, `tls.enabled: false` on the management cluster, behind a TLS-terminating Ingress also works and needs no backend configuration, at the cost of plaintext inside the management cluster, the database included. The chart treats plaintext as a decision to state, not a default.',
        'Traffic then runs in both directions, and the edge firewall has to admit the inbound half:',
        { table: { head: ['From', 'To', 'Port', 'What'], rows: [
          ['the edge operator and CSI plugins', 'the management API, through the Ingress', '443 at the Ingress, 5000 behind it', 'cluster creation, and every later call about the cluster and its volumes'],
          ['the management cluster\'s control plane', 'each edge storage worker\'s `InternalIP`', '5000', 'the storage-node agent, published with `hostPort: 5000`. A managed control plane is given `<InternalIP>:5000` as the node address, because it cannot resolve the edge\'s Service DNS.'],
          ['the management cluster\'s control plane', 'each edge storage worker', 'the StorageCluster\'s `rpcBasePort` range (default 8080) and `snodeApiPort` (default 50001)', 'the storage node\'s own APIs. Which of these the control plane dials directly is decided there and not verified here.'],
          ['hosts on the edge', 'the edge storage nodes', '`nvmfBasePort` and up (default 4420)', 'NVMe-oF data, which stays on the site'],
        ] } },
        'The management API\'s Deployment sets no host network, so its pods run on the management cluster\'s pod network and reach an edge from that cluster\'s egress addresses, which depend on its network plugin. The edge firewall admits those addresses to every storage worker. For a two-node edge the management cluster is also the witness that decides which node continues after a partition, so the link to the center carries that decision too.',
      ],
      examples: [
        { title: 'The Ingress in front of the management API', file: 'examples/edge-management-cluster/04-ingress.yaml', notes: [
          { at: 'host: control.central.example.com', text: 'The external name the edges dial. It is in the Ingress certificate and in no certificate the operator issues.' },
          { at: 'secretName: control-central-example-com-tls', text: 'A TLS Secret for the external name, from a CA the edges\' system trust store holds. A private CA here fails on the edges, whatever their `caBundleSecretRef` says.' },
          { at: 'name: simplyblock-webappapi', text: 'The Service the operator creates for the management API, a ClusterIP.' },
          { at: 'name: http', text: 'The Service\'s port 5000 is named `http`, and it serves TLS when `enableTLS` is on.' },
        ] },
      ],
    },
    {
      heading: 'Registering edges: one StorageCluster each, unique names',
      body: [
        'There is no per-edge object on the management cluster. An edge registers by doing what any cluster does: its operator, installed with the `managed` profile, creates its StorageCluster\'s backend cluster through the management API. Per edge:',
        { steps: [
          'Create the Secret holding the admin token under `token` in the operator\'s namespace on the edge.',
          'Install the chart with `deployment.profile: managed`, the endpoint, the Secret, the storage-node image, and `tls.enabled: false`.',
          'Check that the edge\'s ControlPlane reports `Available`.',
          'Run discovery and approve a draft as chapter [Two-node edge clusters with an arbiter](chapter.html?c=two-node-edge) shows, since the `managed` profile renders no discovery run, or request it from the hub with a StorageSiteDeployment.',
          'Make sure the StorageCluster\'s name is used by no other edge.',
        ] },
        'At its creation step the edge operator calls CreateCluster with the admin token. The answer carries the backend cluster\'s UUID and its own secret, which the operator records in `status.uuid` and in the Secret `simplyblock-cluster-<name>` beside the StorageCluster, under the keys `uuid` and `secret`. From then on the operator reads and changes the cluster as the cluster itself, with that secret. The admin token stays on the edge and is used again only to list clusters by name.',
        '**Names must be unique across every edge.** When CreateCluster fails, for whatever reason, the operator looks the cluster up by name and, if the control plane has one, adopts it: it records that cluster\'s UUID and secret as its own. The fallback exists to resume a retried create of the same cluster, and it cannot tell that case from another edge\'s cluster of the same name. Two edges with a StorageCluster called `edge` end up driving one backend cluster. Whether the control plane refuses a duplicate name on create is decided there, and nothing on the management cluster stops the collision.',
        'A discovery run guards against the common case. The cluster name it proposes is `<draft>-<id>-cluster`, where `<id>` is the first eight characters of the edge\'s `kube-system` namespace UID, so two edges whose drafts share a name still propose different cluster names. The guard is gone the moment a person names the cluster: `cluster.name` on a draft, `sizing.name` on a StorageSiteDeployment, or a StorageCluster written by hand. Name such clusters after their site.',
        'One token serves every edge, and the operator expects the list call it authorizes to return each cluster\'s secret, since adoption by name takes the secret from that list. Treat the token as a credential to the whole fleet. Per-edge tokens are not built: there is one `adminTokenSecretRef` with one key.',
        'Deleting an edge\'s StorageCluster deletes its backend cluster on the management cluster, with force, authenticated with the cluster\'s own secret. While the control plane is unreachable the deletion waits rather than orphaning the cluster. Taking an edge out of the fleet is done on the edge, and the management cluster has nothing left to clean up.',
        { code: 'kubectl -n simplyblock get controlplane simplyblock\nkubectl -n simplyblock get storagecluster\nkubectl -n simplyblock get events --field-selector reason=ClusterCreationFailed', title: 'On the edge, after the install' },
      ],
      examples: [
        { title: 'Helm values for one edge', file: 'examples/edge-management-cluster/05-edge-helm-values.yaml', notes: [
          { at: 'profile: managed', text: 'Operator, CSI driver, and storage nodes here, the control plane on the management cluster.' },
          { at: 'endpoint: https://control.central.example.com', text: 'The Ingress\'s external name. The operator refuses a loopback or link-local address.' },
          { at: 'credentialsSecretRef: central-control-plane', text: 'The edge\'s Secret holding the same token as the management cluster\'s `edge-admin-token`.' },
          { at: 'enabled: false', text: 'With TLS on, the edge\'s CSI plugins would trust only the edge\'s own CA bundle, which did not sign the Ingress certificate.' },
        ] },
        { title: 'The edge ControlPlane', file: 'examples/edge-management-cluster/06-edge-controlplane.yaml', notes: [
          { at: 'managed:', text: 'Installs nothing. The operator probes the endpoint and reports `Available` or `Unavailable`.' },
          { at: 'name: central-control-plane', text: 'Read under `token`, then `secret`. A missing Secret is a `CredentialsError` event.' },
          { at: 'storageNodeImage:', text: 'A managed control plane carries no image for the edge\'s storage nodes to fall back to.' },
        ] },
      ],
    },
    {
      heading: 'Watching every site from the center',
      body: [
        'Every edge\'s backend cluster lives in the management cluster\'s control plane, so the center is where the fleet is visible as one. Three things show it.',
        { list: [
          '**Prometheus**, installed by the ControlPlane controller on the management cluster, scrapes the management API at `/api/v2/metrics`, the FoundationDB exporter, and the pods annotated for scraping in the operator\'s namespace there. The metrics endpoint answers for every cluster the control plane holds, and how it labels each one is decided in the control plane. Pods on the edges are not scraped from the center, and an edge with the `managed` profile runs no control-plane Prometheus.',
          '**Grafana and Graylog**, the control plane\'s monitoring stack, when monitoring was on before the first edge registered.',
          '**The Control Center**, installed on the management cluster with `controlCenter.enabled`. With `controlCenter.controlPlane.enabled`, the default, it reads the control plane API through its own proxy, and with the `standalone` profile it finds this release\'s management API by itself. It lists every cluster, node, pool, and volume the control plane knows, read-only, which on a management cluster is every edge. `controlCenter.controlPlane.tokenSecret` can name the admin token Secret instead of trusting the console\'s service account.',
        ] },
        'The Kubernetes objects of an edge, its StorageCluster, its StorageNodes, their phases, and their events, stay on the edge. Nothing on the management cluster mirrors them, except for an edge deployed from the hub, whose StorageSiteDeployment status carries the draft, the StorageCluster\'s name, UUID, phase, and pool, and each node\'s phase.',
        'A roll-up of every site\'s health in one object or one metric, the FleetMember status and the `simplyblock_fleet_*` metrics of the management-hub design, is designed and not built.',
        { code: 'kubectl get storagesitedeployments -A\nkubectl -n simplyblock get sbsd edge-north -o jsonpath=\'{.status.storageCluster}\'', title: 'The hub-deployed sites, from the management cluster' },
      ],
    },
    {
      heading: 'Deploying a site from the hub: StorageSiteDeployment',
      body: [
        'Writing a discovery run and approving a draft on each edge by hand does not scale to many sites. When the management cluster is also an OCM hub, the operator there can do it: a **StorageSiteDeployment**, short name `sbsd`, requests one site\'s storage from the hub, and the operator carries the request to the site and reports back. The path is optional and needs OCM, none of which the chart installs:',
        { list: [
          'the OCM cluster manager on the management cluster, which serves `work.open-cluster-management.io/v1` ManifestWorks,',
          'a klusterlet on every edge, which makes it a ManagedCluster with a namespace of its own on the hub,',
          'ManagedClusterViews (`view.open-cluster-management.io/v1beta1`), which come from `multicloud-operators-foundation`,',
          'the operator on every edge with the `managed` profile pointed at the management API, as above.',
        ] },
        'The controller starts by itself. At start the operator checks whether ManifestWorks are served. If they are not, it looks again after 60 seconds, doubling the wait up to 10 minutes, and registers the hub-only controllers once they are, with no restart.',
        { steps: [
          'Write the request on the hub: the ManagedCluster in `cluster`, the site\'s operator namespace in `siteNamespace` (default `simplyblock`), the draft\'s name in `draftName` (default `site-draft`), the nodes to inspect in `discover`, and the sizing in `sizing`, with `approved: false`. `cluster` and `draftName` are immutable.',
          'The operator creates the ManifestWork `sbsd-<hash>` in the site\'s hub namespace. It carries an OperatorOps `hub-discover-<draft>-<hash>` with the action `Discover` and the draft\'s name as `configName`. The phase is `Discovering` until the draft names nodes.',
          'Once the draft names nodes, the work also carries a server-side apply of the sizing onto the draft\'s `spec.cluster`, with field manager `work-agent-hub-deploy`. Only the stated sizing fields are applied, so the rest of what discovery wrote stays. The phase is `Drafted`.',
          'The reviewer reads `status.draft`: the nodes and devices under `nodeSets`, the sized template under `cluster`, and the site\'s validation findings under `message`. Then the reviewer sets `approved: true` on the request.',
          'The work applies `approved: true` to the draft, and the site expands it as it expands any approved draft. The phase is `Deploying`, with the expansion\'s step in the message.',
          'The phase is `Online` once the StorageCluster is `Online` with a UUID. `status.storageCluster` carries its name, UUID, phase, pool, and nodes.',
        ] },
        { table: { head: ['Condition', 'True when'], rows: [
          ['`Delivered`', 'the ManifestWork is applied on the site'],
          ['`Discovered`', 'the draft names nodes'],
          ['`Approved`', 'the site\'s draft is approved'],
          ['`Ready`', 'the StorageCluster is `Online`'],
        ] } },
        'The controller does not watch the site. It reads the views again every 15 seconds while the request is in progress and every 2 minutes once it is `Online` or `Failed`. `Pending` is in the phase enum and the controller never sets it today: the first reconcile reports `Discovering`. `Failed` is reported when the site refuses a manifest, when the draft fails, or when the StorageCluster is `Unavailable` after the expansion. The controller keeps reading, so a cause fixed on the site moves the phase on. Deleting the request releases the work and the views with their resources orphaned: the discovery, the draft, and the StorageCluster stay on the site.',
        'The Control Center on the hub offers the same as a dialog, **Deploy storage on a managed site**, which lists the ManagedClusters without a deployment and creates the request.',
        { callout: 'Three things are missing. There is no design document for this kind in this repository, since the types cite one in the external simplyblock-dr repository, and no sample manifest besides this chapter\'s. `sizing` has no `clusterMode` and no arbitration fields, so a two-node edge with its witness cannot be fully requested from the hub. And OCM\'s work agent applies the OperatorOps and the draft on the site with permissions OCM defines: the chart\'s aggregated roles grant StorageClusters and StoragePools only, and nothing in the chart grants the work agent OperatorOps or ClusterDeploymentConfigs. If the site refuses them, the request reports `Failed` with `the site did not apply` followed by the kind, the name, and the refusal.', kind: 'warn' },
      ],
      examples: [
        { title: 'The request for one site', file: 'examples/edge-management-cluster/07-storagesitedeployment.yaml', notes: [
          { at: 'cluster: edge-north', text: 'The OCM ManagedCluster, and the hub namespace the work and the views are created in. Immutable.' },
          { at: 'draftName: site-draft', text: 'The ClusterDeploymentConfig the site\'s discovery writes. Immutable. The proposed cluster name carries the site\'s own id, and `sizing.name` replaces it.' },
          { at: 'workers:', text: 'The discovery inspects these nodes and no others. Three, since a `1+1` stripe needs three storage nodes.' },
          { at: 'name: edge-north', text: '`sizing.name`, the StorageCluster\'s name on the site. Stated by a person, so it has to be unique across the fleet.' },
          { at: 'approved: false', text: 'Nothing is deployed. The sizing is applied once the draft names nodes, and the phase reads `Drafted`.' },
        ] },
        { title: 'The same request, approved', file: 'examples/edge-management-cluster/08-storagesitedeployment-approved.yaml', notes: [
          { at: 'approved: true', text: 'One-way: a CEL rule refuses setting it back to false. Applied onto the draft on the site, which then expands.' },
        ] },
      ],
    },
    {
      heading: 'Not the same thing: superclusters, stretched clusters, and fleet',
      body: [
        { table: { head: ['Shape', 'Kubernetes clusters', 'Backend clusters', 'Control plane'], rows: [
          ['a management cluster with edges', 'one central and one per edge', 'one per edge StorageCluster', 'one, on the management cluster, shared by every edge'],
          ['a [supercluster](chapter.html?c=openshift-placement-groups)', 'one', 'one per member, under a supercluster StorageCluster that names no backend cluster itself', 'the site\'s own'],
          ['a StretchedCluster', 'two sites, requested from an OCM hub', 'one, synchronously replicated across both sites: the leading site creates it and the following site adopts it', 'on the hub'],
        ] } },
        'The `fleet/` component is the hub of `design-management-hub.md`, a draft whose admission guard is implemented. It defines StorageFleet, FleetMember, ClusterDeployment, DriverDeployment, StorageClassDeployment, and FleetOperation under `fleet.simplyblock.io/v1alpha1`. Its fleet manager registers no controller for any of them, and what it runs is the site-link relay that stretched clusters use. It is not in the Helm chart, and a multi-edge deployment today uses none of it. The design still names a member\'s ControlPlane source `external`, where the API now has `managed`.',
      ],
    },
    {
      heading: 'What goes wrong',
      body: [
        { pitfalls: [
          { symptom: 'The management API pods stay in `CreateContainerConfigError`, and the ControlPlane does not leave `Installing`.', cause: 'The Secret `adminTokenSecretRef` names does not exist, or carries no `token` key. The environment variable is not optional.', fix: 'Create the Secret with the key `token` in the operator\'s namespace. The pods start once it exists.' },
          { symptom: 'The edge ControlPlane is `Available`, and the StorageCluster stays in creation with `ClusterCreationFailed` events about a certificate signed by an unknown authority.', cause: 'The external certificate comes from a private CA named in `caBundleSecretRef`, which only the readiness probe reads.', fix: 'Serve the external name with a certificate from a CA in the system trust store, and drop `caBundleSecretRef`.' },
          { symptom: 'The edge ControlPlane is `Unavailable` with `EndpointUnreachable`, and the error names a certificate that is not valid for the external host.', cause: 'A load balancer passes the management API\'s own certificate through, and that certificate names in-cluster names only.', fix: 'Terminate TLS in front of the API with a certificate for the external name, as the Ingress example does.' },
          { symptom: 'The handshake with the management API fails because no client certificate was presented.', cause: 'The management cluster was installed with the chart\'s default, mutual TLS on, and an edge cannot present a certificate it issued.', fix: '`tls` is immutable, and recreating the control plane deletes its FoundationDB. Decide `tls.mutual_enabled: false` before the first edge registers.' },
          { symptom: 'The cluster is created, and volumes on the edge fail to provision with a certificate error from the CSI plugins.', cause: 'The edge was installed with `tls.enabled: true`, so the plugins trust only the edge\'s own CA bundle.', fix: 'Set `tls.enabled: false` in the edge\'s values and `spec.tls.enableTLS: false` on its SimplyblockDriver.' },
          { symptom: 'An edge\'s operator logs `the cluster already exists in the control plane; adopting it` for a cluster it never created, and its StorageCluster carries the UUID of a cluster another edge created.', cause: 'Two edges gave their StorageClusters the same name, and the create-conflict fallback adopted the other edge\'s backend cluster.', fix: 'Name every cluster after its site before approval. No recovery for two edges sharing one backend cluster is documented in this repository.' },
          { symptom: 'A StorageSiteDeployment gets no status at all.', cause: 'ManifestWorks are not served on the management cluster, so the hub-only controllers are not registered yet, or the operator is still in its back-off of up to 10 minutes.', fix: 'Install the OCM cluster manager. The operator registers the controllers on its next check, with no restart.' },
          { symptom: 'The request stays `Discovering` with `Delivered` false.', cause: 'The site has no klusterlet, so nothing applies the work, or ManagedClusterViews are not served, so no draft is ever projected back.', fix: 'Join the edge to the hub and install `multicloud-operators-foundation`.' },
          { symptom: 'The request is `Failed` with `the site did not apply OperatorOps hub-discover-...` and a refusal from the site\'s API server.', cause: 'OCM\'s work agent on the site lacks permission for OperatorOps or ClusterDeploymentConfigs, and the chart grants it none.', fix: 'Grant the work agent those resources on the site with a ClusterRole of your own. The request moves on at its next read.' },
          { symptom: 'Grafana shows no edge, and the monitoring stack was never set up.', cause: 'Monitoring was turned on after the first edge registered, and the control plane provisions it at the first cluster creation only.', fix: 'Turn monitoring on in the first install. No way to provision it later is documented in this repository.' },
        ] },
      ],
    },
  ],

  questions: [
    { topic: 'Management cluster', cat: 'concept', text: 'What makes a `standalone` install a management cluster that edges can register with?', options: ['`adminTokenSecretRef` naming a Secret with the admin token', 'A management API the edges can reach', 'The chart\'s hub profile', 'An object per edge on the management cluster', 'A TLS configuration that does not demand client certificates'], correct: [0, 1, 4], explain: 'The token, the exposure, and the TLS decision. The chart has no hub profile, and nothing on the management cluster registers an edge: an edge registers by creating its cluster.' },
    { topic: 'Management cluster', cat: 'field', text: 'Under which key does the management API read the admin token from the Secret `adminTokenSecretRef` names?', options: ['`token`', '`secret`', '`admin-token`', '`ca.crt`', '`MONITORING_SECRET`'], correct: [0], explain: 'Only `token`, through a `secretKeyRef`. The edge side, `credentialsSecretRef`, accepts `token` or `secret`.' },
    { topic: 'Management cluster', cat: 'concept', text: 'The value in the admin token Secret is changed. Which of these hold?', options: ['The running management API pods keep the old value until they restart', 'The operator rolls the management API at once', 'Every edge is updated with the new value', 'Every edge\'s cluster secret changes', 'Edges presenting the old token keep working until the pods restart'], correct: [0, 4], explain: 'The token is an environment variable from a `secretKeyRef`, which Kubernetes resolves at container start, and the operator does not watch the Secret. Edges hold their own copy, and cluster secrets are unrelated to the admin token.' },
    { topic: 'Management cluster', cat: 'field', text: 'Which values belong in the Helm values of the management cluster this chapter sets up?', options: ['`deployment.profile: standalone`', '`controlplane.local.adminTokenSecretRef`', '`discovery.enabled: false`', '`controlplane.managed.endpoint`', '`tls.mutual_enabled: false`'], correct: [0, 1, 2, 4], explain: 'A standalone install with the token, no discovery since it hosts no storage, and mutual TLS off. `controlplane.managed.*` is the edges\' side.' },
    { topic: 'Management cluster', cat: 'concept', text: 'Which statements hold for a management cluster with several edges?', options: ['One control plane is shared by every edge', 'Each edge\'s StorageCluster is a backend cluster of its own', 'The CSI driver on each edge dials the management API', 'The operator on an edge assumes another cluster writes to the same FoundationDB', 'Every edge presents the same admin token'], correct: [0, 1, 2, 3, 4], explain: 'All five. Several backend clusters share one control plane, the design says a managed control plane may be shared, and there is one admin token for all edges.' },
    { topic: 'Exposure and TLS', cat: 'concept', text: 'With cert-manager, which names does the management API\'s serving certificate carry?', options: ['`simplyblock-webappapi`', '`simplyblock-webappapi.simplyblock.svc`', 'The external host name the edges dial', 'The address of a load balancer in front of it', '`*.simplyblock-webappapi.simplyblock.svc`'], correct: [0, 1, 4], explain: 'The Service\'s in-cluster names and wildcards under the `.svc` and fully qualified names. No ControlPlane field adds another name, which is why TLS for the external name terminates in front of the API.' },
    { topic: 'Exposure and TLS', cat: 'concept', text: 'Why is the management cluster installed with mutual TLS off?', options: ['`spec.source.managed` has no field for a client certificate', 'An edge mounts only certificates from its own cert-manager', 'Mutual TLS is not supported with cert-manager', 'FoundationDB refuses mutual TLS with remote clients', 'The Ingress requires it'], correct: [0, 1], explain: 'An edge has no way to present a certificate the management cluster issued. cert-manager is the one provider that supports mutual TLS, and FoundationDB never talks to the edges.' },
    { topic: 'Exposure and TLS', cat: 'concept', text: 'What does the edge operator read `caBundleSecretRef` for?', options: ['The readiness probe that sets the ControlPlane\'s phase', 'The CreateCluster call', 'The CSI plugins\' calls', 'The Prometheus scrape', 'The Ingress backend'], correct: [0], explain: 'Only the probe. The cluster calls go through a client that trusts the system trust store and the bundle the edge release mounts, and the CSI plugins trust the mounted bundle or the system store.' },
    { topic: 'Exposure and TLS', cat: 'action', text: 'An edge ControlPlane reports `Available`, and cluster creation fails with a certificate signed by an unknown authority. What fixes it?', options: ['Serve the external name with a certificate from a CA in the system trust store', 'Put the private CA into `caBundleSecretRef` a second time', 'Turn mutual TLS on', 'Rename the StorageCluster', 'Restart the edge operator'], correct: [0], explain: 'The probe trusts `caBundleSecretRef` and the cluster calls do not, so the endpoint has to present a certificate the system store verifies.' },
    { topic: 'Exposure and TLS', cat: 'concept', text: 'What follows from `enableMutualTLS: false` on the management cluster\'s ControlPlane?', options: ['FoundationDB\'s peer TLS is off', 'The management API still serves TLS', 'Prometheus scrapes the API without a client certificate', 'Edges must present a client certificate', 'It can be turned back on by editing the ControlPlane'], correct: [0, 1, 2], explain: 'Peer TLS follows the client-certificate decision, serving TLS stays on, and the scrape presents no certificate. The `tls` block is immutable.' },
    { topic: 'Exposure and TLS', cat: 'field', text: 'Which of these can be changed by editing the management cluster\'s ControlPlane once it exists?', options: ['`tls.enableMutualTLS`', '`tls.enableTLS`', '`tls.provider`', 'The source, from `local` to `managed`', '`metadata.name`'], correct: [], explain: 'None. The whole `tls` block is immutable, the source member cannot switch, and an object\'s name never changes. Each change means recreating the control plane and the FoundationDB behind it.' },
    { topic: 'Exposure and TLS', cat: 'field', text: 'Why is `tls.enabled: false` set in an edge\'s values?', options: ['With TLS on, the edge\'s CSI plugins trust only the edge\'s own CA bundle', 'With it off, the operator and the CSI plugins verify the endpoint against the system trust store', 'It turns off TLS on the management cluster', 'It turns off the Ingress', 'The `managed` profile refuses to render with it on'], correct: [0, 1], explain: 'The value is about how the edge\'s own clients verify a control plane. It does not reach the management cluster, and the managed profile renders with either value.' },
    { topic: 'Network', cat: 'concept', text: 'Which inbound traffic does an edge firewall admit from the management cluster?', options: ['Port 5000 on every storage worker', 'Port 443 on the edge', 'The NVMe-oF ports, for the center to read data', 'The edge\'s Kubernetes API server', 'None, since the center never dials an edge'], correct: [0], explain: 'The control plane dials the node agent on `<InternalIP>:5000`, and the NVMe-oF data stays on the site. Port 443 is the Ingress\'s, on the management cluster.' },
    { topic: 'Network', cat: 'concept', text: 'Which node address is a managed control plane given for an edge storage node?', options: ['`<InternalIP>:5000` of the worker', 'The per-pod DNS name of the storage-node Service', 'The worker\'s ExternalIP', 'The address of the Ingress', 'The Service ClusterIP'], correct: [0], explain: 'A control plane on another cluster cannot resolve the edge\'s Service DNS, so the operator hands it the worker\'s `InternalIP` on the agent\'s host port.' },
    { topic: 'Network', cat: 'concept', text: 'Which statements about the traffic of this deployment are true?', options: ['The edge operator dials the management API', 'The edge CSI plugins dial the management API', 'The management cluster\'s control plane dials the edge workers', 'Volume data from the edge hosts crosses to the center', 'The management API pods run on the host network'], correct: [0, 1, 2], explain: 'Calls go out from the edges and come back from the center. NVMe-oF data stays on the site, and the management API\'s Deployment sets no host network.' },
    { topic: 'Registering edges', cat: 'concept', text: 'Which object on the management cluster registers an edge?', options: ['A FleetMember', 'A StorageCluster on the management cluster', 'A ControlPlane per edge', 'A Secret per edge', 'A ManagedControlPlane'], correct: [], explain: 'None. There is no per-edge object on the central side. An edge registers by creating its StorageCluster\'s backend cluster through the management API.' },
    { topic: 'Registering edges', cat: 'concept', text: 'Where does an edge record its backend cluster\'s identity after CreateCluster?', options: ['The Secret `simplyblock-cluster-<name>` on the edge, under `uuid` and `secret`', '`status.uuid` on the StorageCluster', 'A Secret on the management cluster', 'The admin token Secret', 'An annotation on the ControlPlane'], correct: [0, 1], explain: 'Both on the edge, beside the StorageCluster. Nothing is written on the management cluster\'s Kubernetes API.' },
    { topic: 'Registering edges', cat: 'concept', text: 'Which calls does the edge operator authenticate with the admin token?', options: ['CreateCluster', 'Listing clusters to find one by name', 'Reading the cluster\'s nodes', 'Deleting the cluster', 'Activating the cluster'], correct: [0, 1], explain: 'Both happen before the cluster has a secret. Every later call authenticates as the cluster with its own secret, deletion included.' },
    { topic: 'Registering edges', cat: 'concept', text: 'Two edges both name their StorageCluster `edge`. What can happen?', options: ['The second edge adopts the first edge\'s backend cluster after its create fails', 'Both edges end up driving one backend cluster', 'The management cluster refuses the second edge\'s ControlPlane', 'Admission on the edge refuses the name', 'The operator renames the second cluster'], correct: [0, 1], explain: 'On a failed create the operator adopts a cluster of the same name, and it cannot tell its own retry from another edge\'s cluster. Nothing checks names across clusters.' },
    { topic: 'Registering edges', cat: 'field', text: 'A discovery run with `configName: site-draft` proposes a cluster name. Which one?', options: ['`site-draft-<first eight characters of the kube-system UID>-cluster`', '`site-draft-cluster` on every edge', '`site-draft`', '`discovered-site-draft`', 'The ManagedCluster\'s name'], correct: [0], explain: 'The UID of `kube-system` is fixed per Kubernetes cluster, so two edges with the same draft name still propose different cluster names. Only when that namespace cannot be read does the run fall back to the name without it.' },
    { topic: 'Registering edges', cat: 'action', text: 'Which of these bypass the discovery run\'s guard against colliding names?', options: ['Setting `cluster.name` on a draft', 'Setting `sizing.name` on a StorageSiteDeployment', 'Writing a StorageCluster by hand', 'Accepting the name the run proposed', 'Setting `draftName` on a StorageSiteDeployment'], correct: [0, 1, 2], explain: 'A name a person states replaces the proposed one. `draftName` names the draft, and the proposed cluster name still carries the site\'s id.' },
    { topic: 'Registering edges', cat: 'concept', text: 'What happens when an edge\'s StorageCluster is deleted?', options: ['Its backend cluster is deleted on the management cluster, with force', 'The delete authenticates with the cluster\'s own secret', 'While the control plane is unreachable the deletion waits', 'The backend cluster stays for the management cluster to clean up', 'The admin token is revoked'], correct: [0, 1, 2], explain: 'The finalizer deletes the backend cluster and retries rather than orphaning it. Nothing is left on the center, and the shared token is untouched.' },
    { topic: 'Monitoring', cat: 'concept', text: 'What does the Prometheus on the management cluster scrape?', options: ['The management API at `/api/v2/metrics`', 'The FoundationDB exporter', 'Pods annotated for scraping in the operator\'s namespace there', 'The storage-node pods on the edges', 'A Prometheus on each edge'], correct: [0, 1, 2], explain: 'The metrics endpoint answers for every cluster the control plane holds. Edge pods are not scraped from the center, and a managed edge runs no control-plane Prometheus.' },
    { topic: 'Monitoring', cat: 'concept', text: 'Monitoring is turned on after the first edge registered. What happens?', options: ['The environment is set and nothing is provisioned', 'Grafana is provisioned when the next edge registers', 'The operator provisions it on its next reconcile', 'Every edge is re-registered', 'The ControlPlane refuses the change'], correct: [0], explain: 'The control plane provisions the monitoring stack once, at the first cluster creation, and records the decision then.' },
    { topic: 'Monitoring', cat: 'field', text: 'Which statements about the Control Center on a management cluster are true?', options: ['It is installed with `controlCenter.enabled`', 'It reads the control plane API when `controlCenter.controlPlane.enabled` is on', '`controlCenter.controlPlane.tokenSecret` can name the admin token Secret', '`controlCenter.controlPlane.url` must be set with the `standalone` profile', 'It writes changes through the control plane API'], correct: [0, 1, 2], explain: 'With the standalone profile an empty URL means this release\'s management API. The console reads the control plane read-only, and actions stay Ops objects on the Kubernetes API.' },
    { topic: 'Hub deployment', cat: 'concept', text: 'Which of these does a StorageSiteDeployment need?', options: ['The OCM cluster manager on the management cluster', 'A klusterlet on every edge', 'ManagedClusterViews from `multicloud-operators-foundation`', 'The operator on every edge, with the `managed` profile', 'The fleet manager from `fleet/`'], correct: [0, 1, 2, 3], explain: 'ManifestWorks carry the request and views carry the answer, and the edge\'s StorageCluster registers with the management cluster through its managed ControlPlane. The fleet manager plays no part.' },
    { topic: 'Hub deployment', cat: 'concept', text: 'Which phase does a request report while the site\'s draft names nodes and is not approved?', options: ['`Drafted`', '`Pending`', '`Discovering`', '`Deploying`', '`Online`'], correct: [0], explain: '`Discovering` lasts until the draft names nodes, and `Deploying` starts with the approval. `Pending` is in the enum and never set today.' },
    { topic: 'Hub deployment', cat: 'field', text: 'Which changes to a StorageSiteDeployment does admission refuse?', options: ['Changing `cluster`', 'Changing `draftName`', 'Setting `approved` back to false', 'Changing `sizing`', 'Changing `discover`'], correct: [0, 1, 2], explain: 'Three CEL rules. `sizing` and `discover` stay editable, and a changed discovery is a new OperatorOps, since its name carries a hash of the parameters.' },
    { topic: 'Hub deployment', cat: 'concept', text: 'What happens when a StorageSiteDeployment is deleted?', options: ['Its ManifestWork and views are released', 'The discovery, the draft, and the StorageCluster stay on the site', 'The site\'s StorageCluster is deleted', 'The backend cluster is deleted on the management cluster', 'Deletion is refused while the request is `Online`'], correct: [0, 1], explain: 'The work is deleted with its resources orphaned. A storage cluster is never torn down by deleting the request for it.' },
    { topic: 'Hub deployment', cat: 'field', text: 'Which conditions does a StorageSiteDeployment report?', options: ['`Delivered`', '`Discovered`', '`Approved`', '`Ready`', '`Synced`'], correct: [0, 1, 2, 3], explain: 'The work applied, the draft naming nodes, the site\'s draft approved, and the StorageCluster `Online`.' },
    { topic: 'Hub deployment', cat: 'action', text: 'A request is `Failed` with `the site did not apply OperatorOps hub-discover-...` and a forbidden error. What fixes it?', options: ['Grant OCM\'s work agent on the site permission for OperatorOps and ClusterDeploymentConfigs', 'Set `approved: true`', 'Install the operator on the hub again', 'Change `draftName`', 'Remove the ManagedCluster'], correct: [0], explain: 'The chart grants the work agent nothing for these kinds. `draftName` is immutable, and approval is applied only once a draft with nodes exists.' },
    { topic: 'Hub deployment', cat: 'concept', text: 'Which statements about the controller\'s timing are true?', options: ['It reads the site every 15 seconds while the request is in progress', 'It reads every 2 minutes once the request is `Online` or `Failed`', 'It watches the site\'s objects directly', 'It starts once ManifestWorks are served, checking after 60 seconds and doubling up to 10 minutes', 'The operator has to be restarted after OCM is installed'], correct: [0, 1, 3], explain: 'The site is read through views on a requeue cadence, never watched, and the hub-only controllers are registered into the running manager.' },
    { topic: 'Hub deployment', cat: 'field', text: 'Which of these can `sizing` on a StorageSiteDeployment set?', options: ['`name`', '`vcpuCount`', '`stripe`', '`clusterMode`', '`enableArbitration`'], correct: [0, 1, 2], explain: '`sizing` carries name, vCPUs, huge pages, subsystems, drive format, journal device, and stripe. A two-node edge\'s mode and arbitration cannot be requested from the hub.' },
    { topic: 'Hub deployment', cat: 'concept', text: 'Which are true of how the sizing reaches the site\'s draft?', options: ['It is a server-side apply', 'Its field manager is `work-agent-hub-deploy`', 'Only the fields the request states are applied', 'It is applied only once the draft names nodes', 'It is applied with force'], correct: [0, 1, 2, 3, 4], explain: 'All five. Applying before the draft exists would create a document with no nodes, which the site refuses, and the stated-fields-only rule leaves the discovery\'s own values on the draft.' },
    { topic: 'Neighbors', cat: 'concept', text: 'Which statements about the `fleet/` component are true today?', options: ['Its fleet manager registers no controller for the fleet kinds', 'It is not in the Helm chart', 'Its fleet manager runs the site-link relay for stretched clusters', 'A multi-edge deployment needs it', 'Its design is a draft with the admission guard implemented'], correct: [0, 1, 2, 4], explain: 'The kinds exist and nothing reconciles them, and the relay is the one thing it runs. A multi-edge deployment uses none of it.' },
    { topic: 'Neighbors', cat: 'concept', text: 'Which describe a StretchedCluster?', options: ['One backend cluster, synchronously replicated across two Kubernetes clusters', 'Requested from an OCM hub that also runs the control plane', 'The leading site creates the backend cluster and the following site adopts it', 'Each site has a backend cluster of its own', 'One Kubernetes cluster with several member clusters'], correct: [0, 1, 2], explain: 'A backend cluster per site is this chapter\'s shape, and several members in one Kubernetes cluster is a supercluster.' },
  ],

  // A task's manifest is a multi-line template literal under a computed key, and
  // a YAML key the house-style gate would take for a word is interpolated as a
  // code span: the gate reads a .js file as Markdown.
  tasks: [
    {
      title: 'The management cluster\'s ControlPlane',
      scenario: 'Complete the control plane of the management cluster: installed here, serving TLS, not demanding a client certificate from callers, and accepting the static admin token in the Secret `edge-admin-token`.',
      [`yaml`]: `apiVersion: storage.simplyblock.io/v1alpha2
kind: ControlPlane
metadata:
  name: simplyblock
  namespace: simplyblock
spec:
  source:
    {{source}}:
      image: quay.io/simplyblock-io/simplyblock:26.4.0-beta4
      ${`tls`}:
        enableTLS: {{serving}}
        enableMutualTLS: {{mutual}}
        provider: cert-manager
      {{tokenref}}:
        name: edge-admin-token
`,
      blanks: {
        source: { accept: ['local'], hint: 'the operator installs this control plane' },
        serving: { accept: ['true'], hint: 'the API serves TLS' },
        mutual: { accept: ['false'], hint: 'an edge cannot present a certificate this cluster issued' },
        tokenref: { accept: ['adminTokenSecretRef'], hint: 'the field naming the Secret with the static token' },
      },
      explain: '`local` is the control plane this cluster hosts, TLS stays on for callers inside the cluster, mutual TLS is off because edges have no client certificate to present, and `adminTokenSecretRef` projects the token into the management API.',
    },
    {
      title: 'The Helm values of an edge',
      scenario: 'Complete the values of an edge cluster that registers with the management API at `https://control.central.example.com`, behind an Ingress whose certificate comes from a CA in the system trust store.',
      [`yaml`]: `deployment:
  profile: {{profile}}

controlplane:
  managed:
    endpoint: {{endpoint}}
    credentialsSecretRef: central-control-plane
    storageNodeImage: quay.io/simplyblock-io/simplyblock:26.4.0-beta4

${`tls`}:
  enabled: {{edgetls}}
`,
      blanks: {
        profile: { accept: ['managed'], hint: 'operator and driver here, control plane elsewhere' },
        endpoint: { accept: ['/https:\\/\\/control\\.central\\.example\\.com(:443)?\\/?/'], hint: 'the Ingress\'s external name, over HTTPS' },
        edgetls: { accept: ['false'], hint: 'so the CSI plugins verify against the system trust store' },
      },
      explain: 'The `managed` profile points the edge at the management API, the endpoint is the Ingress\'s HTTPS name, and `tls.enabled: false` keeps the edge\'s CSI plugins from trusting only the edge\'s own CA bundle.',
    },
    {
      title: 'Requesting a second site from the hub',
      scenario: 'The site `edge-north` is deployed. Request the storage of the OCM ManagedCluster `edge-south` from the hub, approved after review, with a StorageCluster name no other site uses.',
      [`yaml`]: `apiVersion: storage.simplyblock.io/v1alpha2
kind: {{kind}}
metadata:
  name: edge-south
  namespace: simplyblock
spec:
  cluster: {{cluster}}
  discover:
    workers: [edge-south-0, edge-south-1, edge-south-2]
  sizing:
    name: {{name}}
    vcpuCount: 4
    maxSubsystemCount: 50
  approved: {{approved}}
`,
      blanks: {
        kind: { accept: ['StorageSiteDeployment'], hint: 'the hub-side request, short name `sbsd`' },
        cluster: { accept: ['edge-south'], hint: 'the ManagedCluster, and the hub namespace of the work' },
        name: { accept: ['edge-south'], hint: 'named after the site, so no other edge has it' },
        approved: { accept: ['true'], hint: 'the review is done' },
      },
      explain: 'A StorageSiteDeployment names the ManagedCluster in `cluster`, sets the StorageCluster\'s name in `sizing.name`, which a person states and so has to be unique across sites, and carries the approval to the site\'s draft once it names nodes.',
    },
  ],
});
