// Chapter 9: a management cluster for multi-edge-site deployments. The central
// side of chapter 8: one standalone install whose control plane serves every
// edge, the admin tokens the edges present, the management API exposed to
// them, the network in both directions, unique cluster names, and what the
// center sees of the fleet. The optional StorageSiteDeployment path over Open
// Cluster Management closes the lesson.
TRAINING.register({
  id: 'edge-management-cluster',
  title: 'A management cluster for multi-edge-site deployments',
  summary: 'One central Kubernetes cluster runs the control plane for many edge sites: admin tokens, the exposed management API, the network both ways, unique cluster names, monitoring from the center, and the optional StorageSiteDeployment path over Open Cluster Management.',
  goal: 'you can set up a management cluster that several edges register with, expose it to them safely, keep their cluster names apart, watch every site from the center, and request a site\'s storage from the hub with a StorageSiteDeployment.',

  sections: [
    {
      heading: 'What a management cluster is in simplyblock',
      body: [
        'A **management cluster** is one central Kubernetes cluster that runs the simplyblock control plane for many edge sites. The control plane is FoundationDB plus the management API.',
        'Each edge runs the operator, the CSI driver, and its storage nodes. It runs no control plane. Its StorageCluster becomes one backend cluster in the management cluster\'s FoundationDB.',
        { table: { head: ['Cluster', 'Helm profile', 'ControlPlane source', 'What runs there'], rows: [
          ['management cluster', '`standalone`', '`local`, with `adminTokenSecretRef`', 'FoundationDB, the management API, the object store, Prometheus, optionally Grafana and Graylog'],
          ['each edge', '`managed`', '`managed`, pointing at the management API', 'the operator, the CSI driver, the storage nodes'],
        ] } },
        'Worked example: 20 edge sites need 1 control plane and 20 operators. The management cluster\'s FoundationDB holds 20 backend clusters.',
        'There is no special hub profile. A management cluster is a `standalone` install with admin tokens and an exposed API. An edge registers by creating its StorageCluster. Nothing on the central side needs a per-edge object.',
        '[Two-node edge clusters with an arbiter](chapter.html?c=two-node-edge) shows one edge. This chapter shows the central side and many edges.',
        { callout: 'A management cluster with remote edges has not been tested end to end yet. Try it in a lab before you rely on it.', kind: 'warn' },
      ],
    },
    {
      heading: 'The management cluster: the token, the values, and the ControlPlane',
      body: [
        { steps: [
          'Create the namespace and the admin token Secret. The chart does not create it.',
          'Install the chart with the values of `02-helm-values.yaml`.',
          'Wait for the ControlPlane `simplyblock` to report `Available`.',
          'Expose the management API, as the next section shows.',
          'Give each edge the address and its own token.',
        ] },
        { code: '$ kubectl -n simplyblock get controlplane simplyblock\nNAME          PHASE       STEP        ENDPOINT                                            VERSION   AGE\nsimplyblock   Available   Installed   http://simplyblock-webappapi.simplyblock.svc:5000   26.4.0    6m', title: 'Expected result' },
        'The Secret key `token` holds a comma-separated list. Each value is a full admin token. Give each edge its own value, so you can remove one edge\'s token without touching the others.',
        'The operator passes the key to the management API as the variable `SB_ADMIN_TOKENS`. It never reads the value. A missing Secret keeps the API pods from starting.',
        { code: '$ kubectl -n simplyblock get deployment simplyblock-webappapi \\\n  -o jsonpath=\'{.spec.template.spec.containers[0].env[?(@.name=="SB_ADMIN_TOKENS")]}\'\n{"name":"SB_ADMIN_TOKENS","valueFrom":{"secretKeyRef":{"key":"token","name":"edge-admin-token"}}}', title: 'Check the token is wired in' },
        'Why a static token at all? The control plane checks Kubernetes tokens with its own API server. A token from an edge\'s API server cannot be checked there.',
        'Turn monitoring on in the first install. The control plane sets up Grafana and Graylog once, when the first cluster is created. On a management cluster that is the first edge to register.',
        'Install with TLS off. The next section explains why. TLS cannot be changed once the ControlPlane exists.',
      ],
      examples: [
        { title: 'The admin token Secret', file: 'examples/edge-management-cluster/01-admin-token-secret.yaml', notes: [
          { at: 'name: edge-admin-token', text: 'Any name. The ControlPlane refers to it.' },
          { at: 'token: replace', text: 'The only key read. Two tokens here, one per edge, separated by a comma.' },
        ] },
        { title: 'Helm values for the management cluster', file: 'examples/edge-management-cluster/02-helm-values.yaml', notes: [
          { at: 'profile: standalone', text: 'A management cluster is a standalone install.' },
          { at: 'adminTokenSecretRef: edge-admin-token', text: 'Empty means no edge can create a cluster.' },
          { at: 'enabled: false', text: 'TLS off, so the control plane can call the edges\' storage nodes.' },
          { at: 'discovery:', text: 'No storage here, so no discovery run.' },
        ] },
        { title: 'The ControlPlane the chart renders', file: 'examples/edge-management-cluster/03-controlplane.yaml', notes: [
          { at: 'enableTLS: false', text: 'The `tls` block cannot change. Changing it means recreating the control plane, which deletes every edge\'s cluster.' },
          { at: 'adminTokenSecretRef:', text: 'Passed to the management API as `SB_ADMIN_TOKENS`.' },
          { at: 'enableMonitoring: true', text: 'Needs `secretRef`. Takes effect at the first cluster creation only.' },
        ] },
      ],
    },
    {
      heading: 'Exposing the management API: TLS and the network both ways',
      body: [
        'The operator creates the Service `simplyblock-webappapi`, a ClusterIP on port 5000. Exposing it to the edges is your job.',
        'Traffic flows both ways. The edges call the management API. The control plane calls every storage node on every edge.',
        'The control plane uses one TLS setting for all of it:',
        { list: [
          'With TLS on, it calls each storage node by its in-cluster DNS name. That name exists only inside the edge\'s cluster, so the call fails.',
          'With TLS off, it calls each storage node at the worker\'s `InternalIP` over plain HTTP. This works across sites.',
        ] },
        'The edge side must match. An edge whose endpoint starts with `https://` runs its storage nodes with TLS. So each edge uses an `http://` endpoint and `tls.enabled: false`.',
        'That means the admin tokens, cluster secrets, and node traffic cross the network in clear text. Connect the sites with a VPN or private link, and expose the API only there.',
        { code: '$ kubectl apply -f 04-ingress.yaml\n$ curl -s -o /dev/null -w \'%{http_code}\\n\' -H \'Authorization: Bearer wrong\' \\\n    http://control.central.example.com/api/v2/clusters/\n401', title: 'From an edge: the API answers and refuses a wrong token' },
        { table: { head: ['From', 'To', 'Port', 'What'], rows: [
          ['edge operator and CSI driver', 'management API, through the Ingress', '80, then 5000', 'cluster creation and every call about its volumes'],
          ['management control plane', 'each edge storage worker\'s `InternalIP`', '5000', 'the storage-node agent'],
          ['management control plane', 'each edge storage worker', 'from `rpcBasePort` (default 8080)', 'the storage node\'s RPC proxy'],
          ['hosts on the edge', 'the edge storage nodes', 'from `nvmfBasePort` (default 4420)', 'NVMe-oF data, which stays on the site'],
        ] } },
        'The management API pods run on the pod network. They reach the edges from the management cluster\'s egress addresses. Allow those addresses into every edge storage worker.',
        'For a two-node edge, this link also carries the arbiter\'s decisions.',
      ],
      examples: [
        { title: 'The Ingress in front of the management API', file: 'examples/edge-management-cluster/04-ingress.yaml', notes: [
          { at: 'whitelist-source-range', text: 'An ingress-nginx annotation. Only the VPN range may call the API.' },
          { at: 'host: control.central.example.com', text: 'The name every edge uses in its endpoint.' },
          { at: 'name: simplyblock-webappapi', text: 'The Service the operator creates.' },
          { at: 'name: http', text: 'The Service\'s port 5000.' },
        ] },
      ],
    },
    {
      heading: 'Registering edges: one StorageCluster each, unique names',
      body: [
        'For each edge:',
        { steps: [
          'Create the edge\'s token Secret in its `simplyblock` namespace.',
          'Install the chart with `05-edge-helm-values.yaml`.',
          'Check that the edge\'s ControlPlane is `Available`.',
          'Run discovery and approve a draft, as in [Two-node edge clusters with an arbiter](chapter.html?c=two-node-edge), or request it from the hub with a StorageSiteDeployment.',
          'Give the StorageCluster a name no other edge uses.',
        ] },
        'The edge operator creates its backend cluster with the admin token. The answer holds the cluster\'s UUID and its own secret. The operator stores them on the edge:',
        { code: '$ kubectl -n simplyblock get storagecluster edge-north -o jsonpath=\'{.status.uuid}\'\n1102ec3c-6d1a-4b0e-9f4e-2a7c1d5e8b90\n$ kubectl -n simplyblock get secret simplyblock-cluster-edge-north -o jsonpath=\'{.data}\' | jq keys\n["secret","uuid"]', title: 'Where the edge keeps its cluster identity' },
        'After that, the operator uses the cluster\'s own secret. It uses the admin token again only to look clusters up by name.',
        '**Names must be unique across all edges.** The control plane refuses a second cluster with the same name. The operator then looks the name up and adopts the cluster it finds. It cannot tell a retry of its own create from another edge\'s cluster.',
        'Worked example: edge-north and edge-south both name their cluster `edge`. edge-north creates it. edge-south is refused, finds `edge` by name, and adopts it. Both edges now drive one backend cluster.',
        'Discovery protects you if you keep its proposed name. That name is `<draft>-<id>-cluster`, where `<id>` is the first eight characters of the edge\'s `kube-system` namespace UID. A name you set yourself has no such protection. Name those clusters after their site.',
        'Each admin token is a full admin credential. It can list every cluster, with each cluster\'s secret. Protect every edge\'s token as a key to the whole fleet.',
        'To remove one edge\'s token, delete it from the list and restart the management API:',
        { code: '$ kubectl -n simplyblock create secret generic edge-admin-token \\\n    --from-literal=token="$SOUTH_TOKEN" --dry-run=client -o yaml | kubectl apply -f -\n$ kubectl apply -f 10-controlplaneops-restart.yaml', title: 'Remove edge-north\'s token' },
        'Deleting an edge\'s StorageCluster deletes its backend cluster on the management cluster. While the central site is unreachable, the deletion waits. Nothing is left to clean up in the center.',
      ],
      examples: [
        { title: 'Helm values for one edge', file: 'examples/edge-management-cluster/05-edge-helm-values.yaml', notes: [
          { at: 'profile: managed', text: 'Operator, CSI driver, and storage nodes here.' },
          { at: 'endpoint: http://control.central.example.com', text: 'The Ingress name. Loopback and link-local addresses are refused.' },
          { at: 'credentialsSecretRef: central-control-plane', text: 'The Secret of `09-edge-credentials-secret.yaml`.' },
          { at: 'enabled: false', text: 'With TLS on, the CSI driver would switch the endpoint to `https://`.' },
        ] },
        { title: 'The edge ControlPlane', file: 'examples/edge-management-cluster/06-edge-controlplane.yaml', notes: [
          { at: 'managed:', text: 'Installs nothing. The operator probes the endpoint.' },
          { at: 'name: central-control-plane', text: 'Read under `token`, then `secret`.' },
          { at: 'storageNodeImage:', text: 'A managed control plane supplies no image.' },
        ] },
        { title: 'The edge\'s token Secret', file: 'examples/edge-management-cluster/09-edge-credentials-secret.yaml', notes: [
          { at: 'name: central-control-plane', text: 'The name the edge\'s values refer to.' },
          { at: 'token: replace', text: 'This edge\'s value only, not the whole list.' },
        ] },
        { title: 'Restart the management API', file: 'examples/edge-management-cluster/10-controlplaneops-restart.yaml', notes: [
          { at: 'action: Restart', text: 'Pods read the token list only when they start.' },
          { at: '- simplyblock-webappapi', text: 'Restarts only the management API.' },
        ] },
      ],
    },
    {
      heading: 'Watching every site from the center',
      body: [
        'Every edge\'s backend cluster lives in the management cluster\'s control plane. So the center sees the whole fleet.',
        { list: [
          '**Prometheus** on the management cluster scrapes the management API at `/api/v2/metrics`. Every series carries the labels `cluster` and `cluster_name`.',
          '**Grafana and Graylog** show every cluster, if monitoring was on before the first edge registered.',
          '**The Control Center** (`controlCenter.enabled`) lists every cluster, node, pool, and volume the control plane knows, read-only.',
        ] },
        { code: 'sum by (cluster_name) (simplyblock_snode_size_used)', title: 'Example query: used capacity of the storage nodes, per edge' },
        'Kubernetes objects stay on each edge: StorageClusters, StorageNodes, their phases, and their events. The center does not copy them. The exception is an edge deployed from the hub, whose StorageSiteDeployment shows the cluster\'s phase.',
        { code: '$ kubectl get storagesitedeployments -A\nNAMESPACE     NAME         CLUSTER      APPROVED   PHASE    DRAFT      STORAGE   AGE\nsimplyblock   edge-north   edge-north   true       Online   Expanded   Online    2d', title: 'Hub-deployed sites, from the management cluster' },
      ],
    },
    {
      heading: 'Deploying a site from the hub: StorageSiteDeployment',
      body: [
        'Writing a discovery run on every edge does not scale. If the management cluster is also an Open Cluster Management (OCM) hub, write a **StorageSiteDeployment** (short name `sbsd`) on the hub instead. The operator carries it to the edge and reports back.',
        'You need, none of which the chart installs:',
        { list: [
          'the OCM cluster manager on the management cluster,',
          'a klusterlet on every edge, which makes it a ManagedCluster,',
          'ManagedClusterViews, from `multicloud-operators-foundation`,',
          'the operator on every edge with the `managed` profile.',
        ] },
        'The operator finds OCM by itself. It checks after 60 seconds, doubling the wait up to 10 minutes. No restart is needed.',
        { steps: [
          'Write the request with `approved: false`. `cluster` and `draftName` cannot change later.',
          'The operator sends a discovery run to the edge. Phase `Discovering`.',
          'Once the draft lists nodes, the operator applies your `sizing` onto it. Phase `Drafted`.',
          'Read `status.draft`, then set `approved: true`.',
          'The edge expands the draft. Phase `Deploying`.',
          'The StorageCluster is `Online`. Phase `Online`.',
        ] },
        { code: '$ kubectl apply -f 07-storagesitedeployment.yaml\n$ kubectl -n simplyblock get sbsd edge-north -w\nNAME         CLUSTER      APPROVED   PHASE         DRAFT   STORAGE   AGE\nedge-north   edge-north   false      Discovering                     10s\nedge-north   edge-north   false      Drafted       Draft             2m\n$ kubectl apply -f 08-storagesitedeployment-approved.yaml\nedge-north   edge-north   true       Deploying     Expanding         3m\nedge-north   edge-north   true       Online        Expanded   Online   14m', title: 'Request, review, approve' },
        { table: { head: ['Condition', 'True when'], rows: [
          ['`Delivered`', 'the edge applied the request'],
          ['`Discovered`', 'the draft lists nodes'],
          ['`Approved`', 'the edge\'s draft is approved'],
          ['`Ready`', 'the StorageCluster is `Online`'],
        ] } },
        'The operator reads the edge every 15 seconds while work is in progress, and every 2 minutes once it is `Online` or `Failed`. `Failed` clears by itself when you fix the cause on the edge.',
        'Deleting the request leaves the discovery run, the draft, and the StorageCluster on the edge.',
        { callout: 'Three limits. `sizing` has no `clusterMode` and no arbitration fields, so you cannot request a two-node edge from the hub. The chart does not grant OCM\'s work agent access to OperatorOps or ClusterDeploymentConfigs, so you may need to grant it yourself. And the Control Center offers the same request as the dialog **Deploy storage on a managed site**.', kind: 'warn' },
      ],
      examples: [
        { title: 'The request for one site', file: 'examples/edge-management-cluster/07-storagesitedeployment.yaml', notes: [
          { at: 'cluster: edge-north', text: 'The ManagedCluster. Cannot change.' },
          { at: 'draftName: site-draft', text: 'The draft on the edge. Cannot change.' },
          { at: 'workers:', text: 'Three nodes, because `1+1` needs three outside `Dual` mode.' },
          { at: 'name: edge-north', text: 'You set this name, so it must be unique across the fleet.' },
          { at: 'approved: false', text: 'Nothing is deployed yet.' },
        ] },
        { title: 'The same request, approved', file: 'examples/edge-management-cluster/08-storagesitedeployment-approved.yaml', notes: [
          { at: 'approved: true', text: 'Cannot be set back to false.' },
        ] },
      ],
    },
    {
      heading: 'Not the same thing: superclusters, stretched clusters, and fleet',
      body: [
        { table: { head: ['Shape', 'Kubernetes clusters', 'Backend clusters', 'Control plane'], rows: [
          ['management cluster with edges', 'one central, one per edge', 'one per edge', 'one, shared by all edges'],
          ['[supercluster](chapter.html?c=openshift-placement-groups)', 'one', 'one per member', 'the site\'s own'],
          ['StretchedCluster', 'two sites, from an OCM hub', 'one, replicated across both sites', 'on the hub'],
        ] } },
        'The `fleet.simplyblock.io` kinds, such as StorageFleet and FleetMember, are not part of this deployment. The chart does not install them, and nothing acts on them.',
      ],
    },
    {
      heading: 'What goes wrong',
      body: [
        { pitfalls: [
          { symptom: 'The management API pods stay in `CreateContainerConfigError`.', cause: 'The admin token Secret is missing or has no `token` key.', fix: 'Create the Secret. The pods start once it exists.' },
          { symptom: 'Edge storage nodes never come online on the management cluster.', cause: 'The management cluster has TLS on, so the control plane calls in-cluster names that do not exist there.', fix: 'TLS cannot change. Reinstall the management cluster with `tls.enabled: false` before any edge registers.' },
          { symptom: 'Edge storage nodes never come online, and their pods wait for a TLS Secret.', cause: 'The edge\'s endpoint starts with `https://`.', fix: 'Use an `http://` endpoint over the private link, and `tls.enabled: false` on the edge.' },
          { symptom: 'Storage nodes stay offline, and the agent calls time out.', cause: 'The edge firewall blocks the control plane.', fix: 'Allow port 5000 and the RPC ports from the management cluster\'s egress addresses.' },
          { symptom: 'An edge logs `the cluster already exists in the control plane; adopting it` for a cluster it never created.', cause: 'Two edges use the same StorageCluster name.', fix: 'Name every cluster after its site before approval.' },
          { symptom: 'An edge gets `401` after you edited the token list.', cause: 'Its token was removed, or the API pods were restarted with a list that misses it.', fix: 'Put the edge\'s token back in the list and restart the management API.' },
          { symptom: 'A new token is refused, though it is in the Secret.', cause: 'The API pods read the list only at start.', fix: 'Restart the management API with a ControlPlaneOps `Restart`.' },
          { symptom: 'A StorageSiteDeployment has no status at all.', cause: 'OCM is not installed, or the operator is still waiting up to 10 minutes.', fix: 'Install the OCM cluster manager. No restart is needed.' },
          { symptom: 'The request stays `Discovering` with `Delivered` false.', cause: 'The edge has no klusterlet, or ManagedClusterViews are missing.', fix: 'Join the edge to the hub and install `multicloud-operators-foundation`.' },
          { symptom: 'The request is `Failed` with `the site did not apply OperatorOps hub-discover-...`.', cause: 'OCM\'s work agent on the edge may not create OperatorOps or ClusterDeploymentConfigs.', fix: 'Grant it those resources with a ClusterRole.' },
          { symptom: 'Grafana shows no edges.', cause: 'Monitoring was turned on after the first edge registered.', fix: 'Turn monitoring on in the first install.' },
        ] },
      ],
    },
  ],

  questions: [
    { topic: 'Management cluster', cat: 'concept', text: 'What makes a `standalone` install a management cluster that edges can register with?', options: ['`adminTokenSecretRef` naming a Secret with admin tokens', 'A management API the edges can reach', 'The chart\'s hub profile', 'One object per edge on the management cluster', 'TLS turned off'], correct: [0, 1, 4], explain: 'Tokens, exposure, and TLS off. There is no hub profile, and an edge registers by creating its cluster.' },
    { topic: 'Management cluster', cat: 'field', text: 'How does the management API read the admin tokens from the Secret?', options: ['From the key `token`', 'As a comma-separated list', 'From the key `secret`', 'From every key of the Secret', 'From a file the operator writes'], correct: [0, 1], explain: 'The key `token` becomes `SB_ADMIN_TOKENS`, a comma-separated list. The edge side reads `token` or `secret`.' },
    { topic: 'Management cluster', cat: 'concept', text: 'You add a token to the admin token Secret. When does the management API accept it?', options: ['After the management API pods restart', 'At once', 'After the next edge registers', 'After the operator\'s next pass', 'Never, tokens are fixed at install'], correct: [0], explain: 'The token list is an environment variable, read when the container starts. Restart the API with a ControlPlaneOps `Restart`.' },
    { topic: 'Management cluster', cat: 'field', text: 'Which values belong in the management cluster\'s Helm values?', options: ['`deployment.profile: standalone`', '`controlplane.local.adminTokenSecretRef`', '`discovery.enabled: false`', '`controlplane.managed.endpoint`', '`tls.enabled: false`'], correct: [0, 1, 2, 4], explain: '`controlplane.managed.*` belongs on the edges.' },
    { topic: 'Management cluster', cat: 'concept', text: 'Which statements hold for a management cluster with several edges?', options: ['One control plane serves every edge', 'Each edge\'s StorageCluster is its own backend cluster', 'Each edge\'s CSI driver calls the management API', 'Each edge runs its own FoundationDB', 'Each edge can have its own admin token'], correct: [0, 1, 2, 4], explain: 'Edges run no control plane, so no FoundationDB. The token list allows one token per edge.' },
    { topic: 'Management cluster', cat: 'concept', text: 'Monitoring is turned on after the first edge registered. What happens?', options: ['Nothing is set up', 'Grafana is set up when the next edge registers', 'The operator sets it up on its next pass', 'Every edge registers again', 'The ControlPlane refuses the change'], correct: [0], explain: 'The control plane decides about monitoring once, when the first cluster is created.' },
    { topic: 'TLS and network', cat: 'concept', text: 'Why does the management cluster run with TLS off?', options: ['With TLS on, the control plane calls storage nodes by in-cluster names that exist only on each edge', 'With TLS off, it calls storage nodes at the worker\'s `InternalIP`', 'TLS is not supported with cert-manager', 'FoundationDB refuses TLS', 'The Ingress requires it'], correct: [0, 1], explain: 'The control plane uses one TLS setting for all its calls. Only plain HTTP to the worker address reaches a remote edge.' },
    { topic: 'TLS and network', cat: 'concept', text: 'Why does each edge use an `http://` endpoint?', options: ['An `https://` endpoint makes the edge\'s storage nodes serve TLS', 'The management control plane calls them over plain HTTP', 'The endpoint field admits only HTTP', 'The CSI driver cannot use HTTPS', 'The admin token must not be encrypted'], correct: [0, 1], explain: 'Both sides must agree on plain HTTP. HTTPS is admitted, but it breaks the calls to the edge\'s storage nodes.' },
    { topic: 'TLS and network', cat: 'action', text: 'Tokens and cluster secrets cross the network in clear text. What do you do?', options: ['Connect the sites with a VPN or private link', 'Expose the API only on that link', 'Turn on TLS on the management cluster', 'Put a private CA in `caBundleSecretRef`', 'Use a public LoadBalancer'], correct: [0, 1], explain: 'TLS on the management cluster breaks the calls to edge storage nodes. Protect the traffic at the network level.' },
    { topic: 'TLS and network', cat: 'field', text: 'Which of these can you change on the management cluster\'s ControlPlane after it exists?', options: ['`tls.enableMutualTLS`', '`tls.enableTLS`', '`tls.provider`', 'The source, from `local` to `managed`', '`metadata.name`'], correct: [], explain: 'None. The `tls` block and the source are fixed, and names never change. Decide TLS before the first edge registers.' },
    { topic: 'TLS and network', cat: 'concept', text: 'Which inbound traffic must an edge firewall allow from the management cluster?', options: ['Port 5000 on every storage worker', 'The RPC ports from `rpcBasePort` on every storage worker', 'The NVMe-oF ports', 'The edge\'s Kubernetes API server', 'None'], correct: [0, 1], explain: 'The control plane calls the storage-node agent and the RPC proxy. NVMe-oF data stays on the site.' },
    { topic: 'TLS and network', cat: 'concept', text: 'Which address does the operator give the control plane for an edge storage node?', options: ['The worker\'s `InternalIP` with port 5000', 'The storage node Service\'s DNS name', 'The worker\'s ExternalIP', 'The Ingress address', 'The Service ClusterIP'], correct: [0], explain: 'A control plane on another cluster cannot resolve the edge\'s DNS names.' },
    { topic: 'TLS and network', cat: 'concept', text: 'Which statements about traffic are true?', options: ['The edge operator calls the management API', 'The edge CSI driver calls the management API', 'The management control plane calls the edge workers', 'Volume data from edge hosts crosses to the center', 'The management API pods use the host network'], correct: [0, 1, 2], explain: 'NVMe-oF data stays on the site. The API pods run on the pod network.' },
    { topic: 'TLS and network', cat: 'action', text: 'From an edge, `curl` with a wrong bearer token to `http://control.central.example.com/api/v2/clusters/` returns `401`. What does that tell you?', options: ['The network path and the Ingress work', 'The management API refused the token', 'The control plane is down', 'TLS is required', 'The edge firewall blocks port 5000'], correct: [0, 1], explain: 'A 401 comes from the management API itself. The path is open, and the call needs a token.' },
    { topic: 'Registering edges', cat: 'concept', text: 'Which object on the management cluster registers an edge?', options: ['A FleetMember', 'A StorageCluster on the management cluster', 'A ControlPlane per edge', 'A Secret per edge', 'A ManagedControlPlane'], correct: [], explain: 'None. An edge registers by creating its backend cluster through the management API.' },
    { topic: 'Registering edges', cat: 'concept', text: 'Where does an edge keep its backend cluster\'s identity?', options: ['The Secret `simplyblock-cluster-<name>` on the edge', '`status.uuid` on the StorageCluster', 'A Secret on the management cluster', 'The admin token Secret', 'An annotation on the ControlPlane'], correct: [0, 1], explain: 'Both on the edge. Nothing is written to the management cluster\'s Kubernetes API.' },
    { topic: 'Registering edges', cat: 'concept', text: 'Which calls does the edge operator make with the admin token?', options: ['Creating its cluster', 'Looking clusters up by name', 'Reading its nodes', 'Deleting its cluster', 'Activating its cluster'], correct: [0, 1], explain: 'Both happen before the cluster has its own secret. Every later call uses the cluster\'s secret.' },
    { topic: 'Registering edges', cat: 'concept', text: 'Two edges both name their StorageCluster `edge`. What happens?', options: ['The control plane refuses the second create', 'The second edge adopts the first edge\'s backend cluster', 'Both edges drive one backend cluster', 'Admission on the edge refuses the name', 'The operator renames the second cluster'], correct: [0, 1, 2], explain: 'After the refusal, the operator finds the name and adopts that cluster. It cannot tell its own retry from another edge\'s cluster.' },
    { topic: 'Registering edges', cat: 'field', text: 'A discovery run with `configName: site-draft` proposes a cluster name. Which one?', options: ['`site-draft-<first eight characters of the kube-system UID>-cluster`', '`site-draft-cluster`', '`site-draft`', '`discovered-site-draft`', 'The ManagedCluster\'s name'], correct: [0], explain: 'The `kube-system` UID differs per Kubernetes cluster, so two edges get different names.' },
    { topic: 'Registering edges', cat: 'action', text: 'Which of these lose the protection of the proposed cluster name?', options: ['Setting `cluster.name` on a draft', 'Setting `sizing.name` on a StorageSiteDeployment', 'Writing a StorageCluster by hand', 'Keeping the proposed name', 'Setting `draftName` on a StorageSiteDeployment'], correct: [0, 1, 2], explain: 'A name you set replaces the proposed one. `draftName` names only the draft.' },
    { topic: 'Registering edges', cat: 'concept', text: 'What can a holder of any admin token do?', options: ['List every cluster', 'Read every cluster\'s secret from that list', 'Create clusters', 'Only reach the cluster of its own edge', 'Nothing until an edge approves it'], correct: [0, 1, 2], explain: 'Each admin token is a full admin credential. Protect every edge\'s token as a key to the fleet.' },
    { topic: 'Registering edges', cat: 'concept', text: 'What happens when you delete an edge\'s StorageCluster?', options: ['Its backend cluster is deleted on the management cluster', 'The delete uses the cluster\'s own secret', 'While the central site is unreachable, the delete waits', 'The backend cluster stays for you to clean up', 'The admin token is revoked'], correct: [0, 1, 2], explain: 'Nothing is left in the center, and the tokens are untouched.' },
    { topic: 'Monitoring', cat: 'concept', text: 'What does Prometheus on the management cluster scrape?', options: ['The management API at `/api/v2/metrics`', 'The FoundationDB exporter', 'Annotated pods in the operator\'s namespace there', 'Storage-node pods on the edges', 'A Prometheus on each edge'], correct: [0, 1, 2], explain: 'The management API reports every cluster, labeled `cluster` and `cluster_name`. Edge pods are not scraped from the center.' },
    { topic: 'Monitoring', cat: 'field', text: 'Which statements about the Control Center on a management cluster are true?', options: ['You install it with `controlCenter.enabled`', 'It reads the control plane API when `controlCenter.controlPlane.enabled` is on', '`controlCenter.controlPlane.tokenSecret` can name a Secret with an admin token', 'You must set `controlCenter.controlPlane.url` with the `standalone` profile', 'It changes clusters through the control plane API'], correct: [0, 1, 2], explain: 'With `standalone`, an empty URL means this release\'s management API. The console only reads.' },
    { topic: 'Hub deployment', cat: 'concept', text: 'What does a StorageSiteDeployment need?', options: ['The OCM cluster manager on the management cluster', 'A klusterlet on every edge', 'ManagedClusterViews from `multicloud-operators-foundation`', 'The operator on every edge with the `managed` profile', 'The `fleet.simplyblock.io` kinds'], correct: [0, 1, 2, 3], explain: 'OCM carries the request and the answer. The fleet kinds play no part.' },
    { topic: 'Hub deployment', cat: 'concept', text: 'The edge\'s draft lists nodes and is not approved. Which phase does the request show?', options: ['`Drafted`', '`Pending`', '`Discovering`', '`Deploying`', '`Online`'], correct: [0], explain: '`Discovering` lasts until the draft lists nodes. `Deploying` starts with approval.' },
    { topic: 'Hub deployment', cat: 'field', text: 'Which changes to a StorageSiteDeployment are refused?', options: ['Changing `cluster`', 'Changing `draftName`', 'Setting `approved` back to false', 'Changing `sizing`', 'Changing `discover`'], correct: [0, 1, 2], explain: '`sizing` and `discover` stay editable.' },
    { topic: 'Hub deployment', cat: 'concept', text: 'What happens when you delete a StorageSiteDeployment?', options: ['The request\'s OCM objects are removed', 'The discovery run, the draft, and the StorageCluster stay on the edge', 'The edge\'s StorageCluster is deleted', 'The backend cluster is deleted', 'Deletion is refused while `Online`'], correct: [0, 1], explain: 'Deleting the request never removes storage.' },
    { topic: 'Hub deployment', cat: 'action', text: 'A request is `Failed` with `the site did not apply OperatorOps hub-discover-...` and a forbidden error. What fixes it?', options: ['Grant OCM\'s work agent access to OperatorOps and ClusterDeploymentConfigs on the edge', 'Set `approved: true`', 'Reinstall the operator on the hub', 'Change `draftName`', 'Remove the ManagedCluster'], correct: [0], explain: 'The chart grants the work agent nothing for these kinds. The request moves on at its next read.' },
    { topic: 'Hub deployment', cat: 'field', text: 'Which fields can `sizing` set?', options: ['`name`', '`vcpuCount`', '`stripe`', '`clusterMode`', '`enableArbitration`'], correct: [0, 1, 2], explain: 'There is no mode and no arbitration, so a two-node edge cannot be requested from the hub.' },
    { topic: 'Hub deployment', cat: 'concept', text: 'Which statements about the request\'s timing are true?', options: ['The edge is read every 15 seconds while work is in progress', 'It is read every 2 minutes once `Online` or `Failed`', 'The operator watches the edge\'s objects directly', 'The operator finds OCM by itself, checking after 60 seconds and up to every 10 minutes', 'The operator must restart after OCM is installed'], correct: [0, 1, 3], explain: 'The edge is read on a timer, never watched, and no restart is needed.' },
  ],

  // A task's manifest is a multi-line template literal under a computed key, and
  // a YAML key the house-style gate would take for a word is interpolated as a
  // code span: the gate reads a .js file as Markdown.
  tasks: [
    {
      title: 'The management cluster\'s ControlPlane',
      scenario: 'Complete the management cluster\'s ControlPlane: installed here, with TLS off so it can call remote storage nodes, and accepting the admin tokens in the Secret `edge-admin-token`.',
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
        serving: { accept: ['false'], hint: 'remote storage nodes are called over plain HTTP' },
        mutual: { accept: ['false'], hint: 'no TLS, so no client certificates' },
        tokenref: { accept: ['adminTokenSecretRef'], hint: 'the field naming the Secret with the tokens' },
      },
      explain: '`local` is the control plane this cluster runs. TLS is off because the control plane must call edge storage nodes at their worker address. `adminTokenSecretRef` passes the token list to the management API.',
    },
    {
      title: 'The Helm values of an edge',
      scenario: 'Complete the values of an edge that registers with the management API through the Ingress at `control.central.example.com`, over the private link.',
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
        endpoint: { accept: ['/http:\\/\\/control\\.central\\.example\\.com(:80)?\\/?/'], hint: 'the Ingress name, over plain HTTP' },
        edgetls: { accept: ['false'], hint: 'keeps the CSI driver on the http:// endpoint' },
      },
      explain: 'The `managed` profile points the edge at the management API. The endpoint is plain HTTP, so the edge\'s storage nodes serve plain HTTP too. `tls.enabled: false` keeps the CSI driver from switching to HTTPS.',
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
        cluster: { accept: ['edge-south'], hint: 'the ManagedCluster' },
        name: { accept: ['edge-south'], hint: 'named after the site, so no other edge has it' },
        approved: { accept: ['true'], hint: 'the review is done' },
      },
      explain: 'The request names the ManagedCluster in `cluster` and the StorageCluster in `sizing.name`. You set that name, so it must be unique across sites. The approval reaches the edge\'s draft once it lists nodes.',
    },
  ],
});
