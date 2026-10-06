{{/*
  Ramen helpers — namespaced under `sbramen.` so nothing collides.

  The manifests these helpers render are vendored from RamenDR/ramen
  v0.1.0-rc1 (config/manager, config/hub, config/dr-cluster). Upstream builds
  them with kustomize (namePrefix ramen-hub- / ramen-dr-cluster-, namespace
  ramen-system); the helpers reproduce exactly that shape, parameterized only
  for namespace, image, resources and scheduling.
*/}}

{{/* sbramen.labels: the labels upstream's LabelTransformer stamps.
     Takes (dict "app" <ramen-hub|ramen-dr-cluster>). */}}
{{- define "sbramen.labels" -}}
app: {{ .app }}
control-plane: controller-manager
{{- end -}}

{{/* sbramen.managerConfig: the RamenConfig document for one role.
     Takes (dict "root" $ "role" <hub|dr-cluster> "vals" .Values.ramen.<role>).
     Starts from the upstream ramen_manager_config.yaml defaults, appends the
     chart's shared s3StoreProfiles, then deep-merges the user's overrides on
     top. */}}
{{- define "sbramen.managerConfig" -}}
{{- $ramen := .root.Values.ramen -}}
{{- $controllerType := ternary "dr-hub" "dr-cluster" (eq .role "hub") -}}
{{- $base := fromYaml (printf `apiVersion: ramendr.openshift.io/v1alpha1
kind: RamenConfig
health:
  healthProbeBindAddress: :8081
metrics:
  bindAddress: :8443
webhook:
  port: 9443
leaderElection:
  leaderElect: true
  resourceName: %s.ramendr.openshift.io
ramenControllerType: %s
maxConcurrentReconciles: 50
volSync:
  destinationCopyMethod: Direct
volumeUnprotectionEnabled: true
ramenOpsNamespace: ramen-ops
multiNamespace:
  FeatureEnabled: true
  volsyncSupported: true
kubeObjectProtection:
  veleroNamespaceName: velero
` .role $controllerType) -}}
{{- if $ramen.s3StoreProfiles -}}
{{- $_ := set $base "s3StoreProfiles" $ramen.s3StoreProfiles -}}
{{- end -}}
{{- toYaml (mustMergeOverwrite $base (deepCopy (.vals.config | default dict))) -}}
{{- end -}}

{{/* sbramen.operator: ServiceAccount, operator ConfigMap and Deployment for
     one role — upstream config/manager/manager.yaml with the
     manager_config_patch applied, names and labels as kustomize produces
     them. Takes (dict "root" $ "role" <hub|dr-cluster> "vals" ...). */}}
{{- define "sbramen.operator" -}}
{{- $ramen := .root.Values.ramen -}}
{{- $prefix := printf "ramen-%s" .role -}}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ $prefix }}-operator
  namespace: {{ $ramen.namespace }}
  labels:
    {{- include "sbramen.labels" (dict "app" $prefix) | nindent 4 }}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ $prefix }}-operator-config
  namespace: {{ $ramen.namespace }}
  labels:
    {{- include "sbramen.labels" (dict "app" $prefix) | nindent 4 }}
    {{- if eq .role "hub" }}
    cluster.open-cluster-management.io/backup: resource
    {{- end }}
data:
  ramen_manager_config.yaml: |
    {{- include "sbramen.managerConfig" . | nindent 4 }}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ $prefix }}-operator
  namespace: {{ $ramen.namespace }}
  labels:
    {{- include "sbramen.labels" (dict "app" $prefix) | nindent 4 }}
spec:
  replicas: {{ .vals.replicas }}
  selector:
    matchLabels:
      {{- include "sbramen.labels" (dict "app" $prefix) | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "sbramen.labels" (dict "app" $prefix) | nindent 8 }}
      annotations:
        kubectl.kubernetes.io/default-container: manager
        # roll the operator when its RamenConfig changes
        checksum/config: {{ include "sbramen.managerConfig" . | sha256sum }}
    spec:
      serviceAccountName: {{ $prefix }}-operator
      terminationGracePeriodSeconds: 10
      securityContext:
        runAsNonRoot: true
      {{- with $ramen.nodeSelector }}
      nodeSelector: {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $ramen.tolerations }}
      tolerations: {{- toYaml . | nindent 8 }}
      {{- end }}
      containers:
        - name: manager
          command:
            - /manager
          args:
            - --config=/config/ramen_manager_config.yaml
          image: "{{ $ramen.image.repository }}:{{ $ramen.image.tag }}"
          imagePullPolicy: {{ $ramen.image.pullPolicy }}
          env:
            - name: POD_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
            - name: POD_NAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
          securityContext:
            allowPrivilegeEscalation: false
          livenessProbe:
            httpGet:
              path: /healthz
              port: 8081
            initialDelaySeconds: 15
            periodSeconds: 20
          readinessProbe:
            httpGet:
              path: /readyz
              port: 8081
            initialDelaySeconds: 5
            periodSeconds: 10
          resources: {{- toYaml .vals.resources | nindent 12 }}
          volumeMounts:
            - name: ramen-manager-config-vol
              mountPath: /config
              readOnly: true
      volumes:
        - name: ramen-manager-config-vol
          configMap:
            name: {{ $prefix }}-operator-config
{{- end -}}

{{/* sbramen.leaderElection: the namespaced leader-election Role and binding,
     from upstream config/rbac. Takes (dict "root" $ "role" ...). */}}
{{- define "sbramen.leaderElection" -}}
{{- $ramen := .root.Values.ramen -}}
{{- $prefix := printf "ramen-%s" .role -}}
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ $prefix }}-leader-election-role
  namespace: {{ $ramen.namespace }}
  labels:
    {{- include "sbramen.labels" (dict "app" $prefix) | nindent 4 }}
rules:
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ $prefix }}-leader-election-rolebinding
  namespace: {{ $ramen.namespace }}
  labels:
    {{- include "sbramen.labels" (dict "app" $prefix) | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {{ $prefix }}-leader-election-role
subjects:
  - kind: ServiceAccount
    name: {{ $prefix }}-operator
    namespace: {{ $ramen.namespace }}
{{- end -}}
