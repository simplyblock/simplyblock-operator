{{/*
Authored at v1alpha2, which is the storage version a fresh install gets.

That is what keeps the conversion webhook out of a new deployment: an object
written at the storage version needs no conversion, so nothing on this path
ever calls the webhook and it is not deployed. Writing this at v1alpha1 would
make the chart itself the one client that forces conversion, on a cluster where
nothing is serving it.

This object is the whole of what the chart renders for the control plane. Which
member of spec.source it names is what deployment.profile selects, and the
operator does the rest: it installs the control plane for the standalone
profile, and resolves and probes a remote one for the managed profile. The empty
profile renders none: its administrator writes the ControlPlane.

The object is not rendered as a release resource. It goes into the
simplyblock-bootstrap-objects ConfigMap (bootstrap-objects.yaml), and the
operator applies it from there: a release that renders an object of a CRD it
creates in the same install is refused while the API server has not yet listed
the new kind.
*/}}
{{- define "simplyblock.bootstrap.controlPlane" -}}
{{- if has .Values.deployment.profile (list "standalone" "managed") }}
apiVersion: storage.simplyblock.io/v1alpha2
kind: ControlPlane
metadata:
  name: simplyblock
  namespace: {{ .Release.Namespace }}
  annotations:
    # Helm must not delete this object, and the reason is the database behind it.
    # Deleting a ControlPlane whose source is local deletes the FoundationDB
    # holding every cluster definition, node registration, and lvol record, so it
    # is not something `helm uninstall` should do as a side effect. Removing a
    # control plane stays a deliberate `kubectl delete controlplane`, performed
    # while the operator is running so its finalizer can be honored.
    #
    # The operator applies this object, so it is not part of the release and
    # Helm deletes it neither on uninstall nor when a values change drops it.
    # The annotation is still what keeps it on an upgrade from a chart release
    # that rendered it directly: Helm prunes what the old manifest held and the
    # new one does not, unless the live object says keep.
    helm.sh/resource-policy: keep
    # Which release's chart wrote it. The operator applies it, so it carries no
    # Helm release annotation of its own, and validate-controlplane.yaml reads
    # this to refuse the switch to the empty profile while it exists.
    storage.simplyblock.io/chart-release: {{ .Release.Name | quote }}
spec:
  source:
{{- if eq .Values.deployment.profile "standalone" }}
    # This cluster hosts its own control plane, and the operator installs it.
    local:
      image: "{{ .Values.image.simplyblock.repository }}:{{ .Values.image.simplyblock.tag }}"
      imagePullPolicy: "{{ .Values.image.simplyblock.pullPolicy }}"
      foundationDB:
        # The chart states the redundancy mode and the operator states the
        # coordinator count, and the two have to agree: triple redundancy needs
        # five coordinators and double needs three.
        replicas: {{ if eq (.Values.controlplane.foundationdb.redundancyMode | default "double") "triple" }}5{{ else }}3{{ end }}
        {{- if .Values.controlplane.storageclass.name }}
        storageClassName: {{ .Values.controlplane.storageclass.name }}
        {{- end }}
        {{- with .Values.controlplane.foundationdb.nodeSelector }}
        # Where the database's processes go, which is stated apart from
        # controlplane.nodeSelector because it is the database alone that the
        # node-add cap serializes against. Unset leaves them with the rest of
        # the control plane.
        nodeSelector:
          {{- toYaml . | nindent 10 }}
        {{- end }}
        {{- with .Values.controlplane.foundationdb.tolerations }}
        # What the database's processes tolerate. Unset leaves them with the
        # control plane's tolerations below; stated, these replace them.
        tolerations:
          {{- toYaml . | nindent 10 }}
        {{- end }}
      # The chart states both halves explicitly rather than leaving the CR's
      # defaults to decide. The CR defaults to serving TLS, and the chart's
      # tls.enabled defaults to off, and a release that said nothing would get
      # the CR's answer instead of the one its values state -- which is a
      # deployment that comes up over TLS because a field was omitted.
      tls:
        enableTLS: {{ .Values.tls.enabled }}
        enableMutualTLS: {{ and .Values.tls.enabled .Values.tls.mutual_enabled }}
        # The chart spells the providers in lower case and this API spells
        # OpenShift as the product does, the same translation
        # _bootstrap_simplyblockdriver.tpl performs.
        provider: {{ if eq .Values.tls.provider "openshift" }}OpenShift{{ else }}cert-manager{{ end }}
      {{- if .Values.controlplane.observability.enabled }}
      # The monitoring stack this chart installs beside the control plane. The
      # control plane opens its Graylog input and creates its Grafana user when
      # the first cluster is created, with the password from the Secret
      # controlplane_secret.yaml renders.
      observability:
        enableMonitoring: true
        secretRef:
          name: simplyblock-grafana-secrets
      {{- end }}
      {{- if .Values.controlplane.nodeSelector.create }}
      nodeSelector:
        {{ .Values.controlplane.nodeSelector.key }}: {{ .Values.controlplane.nodeSelector.value | quote }}
      {{- end }}
      {{- if .Values.controlplane.tolerations.create }}
      tolerations:
      {{- range .Values.controlplane.tolerations.list }}
      - operator: {{ .operator | quote }}
        {{- if .effect }}
        effect: {{ .effect | quote }}
        {{- end }}
        {{- if .key }}
        key: {{ .key | quote }}
        {{- end }}
        {{- if .value }}
        value: {{ .value | quote }}
        {{- end }}
      {{- end }}
      {{- end }}
      {{- with .Values.controlplane.local.prerequisites }}
      # Which missing cluster add-ons the operator installs. Stated explicitly,
      # for the same reason as tls above: the release's values decide, not the
      # CR's defaults.
      prerequisites:
        installFoundationDBCRDs: {{ ne .installFoundationDBCRDs false }}
        installCertManager: {{ ne .installCertManager false }}
        installLocalPathProvisioner: {{ ne .installLocalPathProvisioner false }}
        installSnapshotController: {{ ne .installSnapshotController false }}
      {{- end }}
      {{- if .Values.controlplane.local.adminTokenSecretRef }}
      adminTokenSecretRef:
        name: {{ .Values.controlplane.local.adminTokenSecretRef | quote }}
      {{- end }}
      {{- if .Values.controlplane.local.eksRoleARN }}
      eksRoleARN: {{ .Values.controlplane.local.eksRoleARN | quote }}
      {{- end }}
{{- else }}
    # A control plane elsewhere manages this cluster's storage. The operator
    # installs nothing here: it resolves the endpoint, probes it, and reports.
    managed:
      endpoint: {{ required "controlplane.managed.endpoint is required for the managed profile: it is where the control plane that manages this cluster answers" .Values.controlplane.managed.endpoint | quote }}
      {{- if .Values.controlplane.managed.credentialsSecretRef }}
      credentialsSecretRef:
        name: {{ .Values.controlplane.managed.credentialsSecretRef | quote }}
      {{- end }}
      {{- if .Values.controlplane.managed.caBundleSecretRef }}
      caBundleSecretRef:
        name: {{ .Values.controlplane.managed.caBundleSecretRef | quote }}
      {{- end }}
      {{- if .Values.controlplane.managed.storageNodeImage }}
      storageNodeImage: {{ .Values.controlplane.managed.storageNodeImage | quote }}
      {{- end }}
{{- end }}
{{- end }}
{{- end -}}
