{{/*
The one discovery run a fresh standalone install performs.

This chart is the only thing that creates it: the operator raises no run of its
own. A managed or empty install, and `discovery.enabled=false`, render none. The
`draft` values are accepted and unused.

Rendered into the simplyblock-bootstrap-objects ConfigMap rather than as a
release resource, for the reason _bootstrap_controlplane.tpl gives. The
operator applies it once and does not create it again while its key is
unchanged, so a finished run is not repeated on an upgrade. In the release
namespace it is owned by that ConfigMap, so `helm uninstall` still removes it;
one written to discovery.namespace is left for the cleanup script.

Authored at v1alpha2, the storage version, so nothing on a fresh install's path
calls the conversion webhook.
*/}}
{{- define "simplyblock.bootstrap.initialDiscovery" -}}
{{- $d := .Values.discovery }}
{{- if and (eq .Values.deployment.profile "standalone") $d.enabled }}
{{- $name := $d.name | default "initial-discovery" }}
{{- /* The draft is "discovered-<name>" and its cluster "<draft>-cluster", which the control plane accepts at 37 characters. */}}
{{- if gt (len (printf "discovered-%s-cluster" $name)) 37 }}
{{- fail (printf "discovery.name %q would make the draft's StorageCluster %d characters, and the control plane accepts at most 37. Use a name of at most 18 characters." $name (len (printf "discovered-%s-cluster" $name))) }}
{{- end }}
apiVersion: storage.simplyblock.io/v1alpha2
kind: OperatorOps
metadata:
  name: {{ $name }}
  namespace: {{ $d.namespace | default .Release.Namespace }}
spec:
  action: Discover
  discover:
    # The partition waiver is on unless stated, because a machine that held data
    # before carries a table on every disk. The rest are left out of the spec when
    # they are at their default, which keeps an empty PCI filter from reaching a
    # logical-block-device run that refuses one.
    enablePartitionedDevices: {{ if hasKey $d "enablePartitionedDevices" }}{{ $d.enablePartitionedDevices }}{{ else }}true{{ end }}
    {{- /* Tri-state: rendered only when stated, so an unset value leaves the run's own auto decision. */}}
    {{- if hasKey $d "enableFailureDomains" }}
    enableFailureDomains: {{ $d.enableFailureDomains }}
    {{- end }}
    {{- with $d.nodeSelector }}
    nodeSelector:
      {{- toYaml . | nindent 6 }}
    {{- end }}
    {{- with $d.tolerations }}
    tolerations:
      {{- toYaml . | nindent 6 }}
    {{- end }}
    {{- range $field := list "enableControlPlaneNodes" "enableLogicalBlockDevices" "forceJournalDevice" }}
    {{- if index $d $field }}
    {{ $field }}: true
    {{- end }}
    {{- end }}
    {{- with $d.network }}
    {{- if and .podNetwork .spdkOnHostNetwork }}
    {{- fail "discovery.network.podNetwork and discovery.network.spdkOnHostNetwork are exclusive; set one." }}
    {{- end }}
    {{- if or .podNetwork .spdkOnHostNetwork }}
    network:
      {{ if .podNetwork }}podNetwork{{ else }}spdkOnHostNetwork{{ end }}: true
    {{- end }}
    {{- end }}
    {{- /* Host networking unless management names an attachment; data alone describes nothing deployable. */}}
    {{- with $d.networkAttachment }}
    {{- if and .data (not .management) }}
    {{- fail (printf "discovery.networkAttachment.data is %q and discovery.networkAttachment.management is empty. Data flows through the management attachment when only that one is set, so set management as well." .data) }}
    {{- end }}
    {{- if .management }}
    networkAttachment:
      management: {{ .management | quote }}
      {{- with .data }}
      data: {{ . | quote }}
      {{- end }}
    {{- end }}
    {{- end }}
    {{- $filter := dict }}
    {{- range $key, $value := $d.deviceFilter }}
    {{- if $value }}{{ $_ := set $filter $key $value }}{{ end }}
    {{- end }}
    {{- with $filter }}
    deviceFilter:
      {{- toYaml . | nindent 6 }}
    {{- end }}
{{- end }}
{{- end -}}
