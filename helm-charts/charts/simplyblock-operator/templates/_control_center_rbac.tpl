{{/*
  The Control Center's ClusterRole rules, shared by the console's own role
  (control-center-rbac.yaml) and the remote-access role
  (control-center-remote-access.yaml): a hub's console acts on a remote
  cluster with exactly the authority it has at home, and keeping the two in
  one define is what guarantees that.

  In serviceaccount mode this role IS the console's authority, for anyone who
  can reach the Service. Kept in step with control-center/deploy/k8s/rbac.yaml,
  the plain-manifest equivalent. Caller pipes through `nindent`.
*/}}
{{- define "sbcc.clusterRoleRules" -}}
# ---- simplyblock entities: read ----
- apiGroups: ["storage.simplyblock.io"]
  resources:
    - storageclusters
    - storagenodes
    - storagenodesets
    - storagedevices
    - storagepools
    - storagebackups
    - backuppolicies
    - backuprestores
    - backupimports
    - controlplanes
    - replicationpairs
    - replicationpolicies
    - replicationslots
    - volumemigrations
    - tasks
    - simplyblockdrivers
    - clusterdeploymentconfigs
  verbs: ["get", "list", "watch"]
- apiGroups: ["storage.simplyblock.io"]
  resources:
    - storageclusters/status
    - storagenodes/status
    - storagepools/status
    - replicationpolicies/status
    - replicationslots/status
  verbs: ["get"]

# ---- day-2 operations ----
# An action is not a verb against an entity: it is an Ops object the operator
# reconciles. The console creates one, reads its phase, patches spec.abort to
# unwind it, and deletes it only to abort.
- apiGroups: ["storage.simplyblock.io"]
  resources:
    - storageclusterops
    - storagenodeops
    - storagedeviceops
    - storagepoolops
    - storagebackupops
    - controlplaneops
    - persistentvolumeops
    - operatorops
    - replicationops
  verbs: ["get", "list", "watch", "create", "patch", "delete"]

# ---- resources the console authors ----
- apiGroups: ["storage.simplyblock.io"]
  resources: ["replicationpairs", "replicationpolicies"]
  verbs: ["create", "update", "patch", "delete"]
- apiGroups: ["storage.simplyblock.io"]
  resources: ["storagebackups", "backuppolicies", "backuprestores", "backupimports", "volumemigrations"]
  verbs: ["create", "update", "patch", "delete"]
- apiGroups: ["storage.simplyblock.io"]
  resources: ["storageclusters", "storagepools", "storagenodesets"]
  verbs: ["update", "patch"]

# ---- the API surface itself ----
# The console lists CustomResourceDefinitions to learn which optional kinds a
# cluster has (the DR hub, Ramen, the site mapper) before rendering their
# sections, and which versions the storage kinds serve.
- apiGroups: ["apiextensions.k8s.io"]
  resources: ["customresourcedefinitions"]
  verbs: ["get", "list", "watch"]

# ---- core Kubernetes ----
# No services/proxy: the kube-apiserver strips the Authorization header from a
# proxied request, so a control plane behind it could never see the bearer
# token the v2 API requires. A context names its control plane URL directly.
- apiGroups: [""]
  resources: ["nodes", "persistentvolumes", "persistentvolumeclaims", "pods", "events", "namespaces"]
  verbs: ["get", "list", "watch"]
- apiGroups: [""]
  resources: ["pods/log"]
  verbs: ["get"]
# A PVC annotation is the whole membership model for replication and for
# backup policies, so patching one is how a volume is attached.
- apiGroups: [""]
  resources: ["persistentvolumeclaims"]
  verbs: ["patch", "update"]
- apiGroups: ["storage.k8s.io"]
  resources: ["storageclasses"]
  verbs: ["get", "list", "watch", "patch"]
- apiGroups: ["snapshot.storage.k8s.io"]
  resources: ["volumesnapshots", "volumesnapshotcontents", "volumesnapshotclasses"]
  verbs: ["get", "list", "watch"]

# ---- workloads, read only, for the Ramen recipe editor ----
- apiGroups: ["apps"]
  resources: ["deployments", "statefulsets", "daemonsets", "replicasets"]
  verbs: ["get", "list"]
- apiGroups: [""]
  resources: ["services", "configmaps"]
  verbs: ["get", "list"]
- apiGroups: ["networking.k8s.io"]
  resources: ["ingresses"]
  verbs: ["get", "list"]
- apiGroups: ["kubevirt.io"]
  resources: ["virtualmachines", "virtualmachineinstances"]
  verbs: ["get", "list"]

# ---- Ramen: instances only, never the CRDs ----
- apiGroups: ["ramendr.openshift.io"]
  resources: ["drpolicies", "drclusters", "drclusterconfigs", "volumereplicationgroups"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["ramendr.openshift.io"]
  resources: ["drplacementcontrols"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["ramendr.openshift.io"]
  resources: ["recipes"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["cluster.open-cluster-management.io"]
  resources: ["managedclusters"]
  verbs: ["get", "list"]

# ---- DR hub (dr-simplyblock): every DR screen is a CR ----
# In serviceaccount mode this is the console's authority for DR: reads on
# everything, the dr-operator writes, the dr-admin writes on plans/paths/
# restores, and the override verb the hub's webhook checks before admitting a
# readiness override. Absent CRDs cost nothing — the DR section then reports
# that the hub is not installed. Trim to the site's policy, or use passthrough.
- apiGroups: ["dr.simplyblock.io"]
  resources: ["protectionplans", "drpaths", "protectedapplications", "recoveryplans", "recoveryactions", "testbubbles", "testschedules", "restoreactions", "drconfigs"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["dr.simplyblock.io"]
  resources: ["protectionplans", "drpaths", "protectedapplications", "recoveryplans", "recoveryactions", "testbubbles", "testschedules", "restoreactions"]
  verbs: ["create", "update", "patch", "delete"]
- apiGroups: ["dr.simplyblock.io"]
  resources: ["recoveryactions"]
  verbs: ["override"]
- apiGroups: ["sitemap.simplyblock.io"]
  resources: ["siteprofiles", "dhcpservers"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["sitemap.simplyblock.io"]
  resources: ["dhcpservers"]
  verbs: ["create", "update", "patch", "delete"]
# the console asks the API server what its identity may do, to disable controls
- apiGroups: ["authorization.k8s.io"]
  resources: ["selfsubjectaccessreviews", "selfsubjectrulesreviews"]
  verbs: ["create"]
- apiGroups: ["authentication.k8s.io"]
  resources: ["selfsubjectreviews"]
  verbs: ["create"]

# ---- Helm release view ----
# Helm stores releases as Secrets. This is the only reason the console reads a
# Secret, and a backup credentialsSecretRef is only ever shown by name.
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list"]
{{- end -}}
