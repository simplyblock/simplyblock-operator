{{/*
  Control Center helpers — self-contained on purpose.

  The console ships as a fragment (see control-center/ in the monorepo) and
  deliberately does NOT call this chart's other helpers: everything is
  namespaced under `sbcc.` so it cannot collide, and the fragment renders
  unchanged if it is ever lifted out of this chart again.
*/}}

{{- define "sbcc.name" -}}
{{- default "control-center" .Values.controlCenter.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "sbcc.fullname" -}}
{{- if .Values.controlCenter.fullnameOverride -}}
{{- .Values.controlCenter.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "sbcc.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "sbcc.selectorLabels" -}}
app.kubernetes.io/name: {{ include "sbcc.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "sbcc.labels" -}}
{{ include "sbcc.selectorLabels" . }}
app.kubernetes.io/component: ui
app.kubernetes.io/part-of: simplyblock
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Chart }}
helm.sh/chart: {{ printf "%s-%s" .Name .Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end -}}

{{/* Image reference. The registry falls back to the public one and the tag to
     the chart's appVersion — pin controlCenter.image.tag for production, this
     chart's appVersion is a floating tag. */}}
{{- define "sbcc.image" -}}
{{- $i := .Values.controlCenter.image -}}
{{- $reg := $i.registry | default "quay.io" -}}
{{- $tag := $i.tag | default (.Chart.AppVersion | default "latest") -}}
{{- printf "%s/%s:%s" $reg $i.repository $tag -}}
{{- end -}}

{{/* Default upstream URLs. This chart names its objects statically
     (simplyblock-operator, simplyblock-prometheus, …) rather than deriving
     them from the release, so the fallbacks here are static too. */}}
{{- define "sbcc.operatorUrl" -}}
{{- .Values.controlCenter.operatorUrl | default "http://simplyblock-operator:8080" -}}
{{- end -}}

{{- define "sbcc.prometheusUrl" -}}
{{- if .Values.controlCenter.prometheusUrl -}}
{{- .Values.controlCenter.prometheusUrl -}}
{{- else -}}
{{- $p := ((.Values.prometheus).simplyblock) | default dict -}}
{{- printf "http://%s:%v" ($p.prometheusURL | default "simplyblock-prometheus") ($p.prometheusPORT | default 9090) -}}
{{- end -}}
{{- end -}}

{{/* The sb-mock service, which impersonates every upstream when the mock is
     enabled. */}}
{{- define "sbcc.mockUrl" -}}
{{- printf "http://%s-mock:8080" (include "sbcc.fullname" .) -}}
{{- end -}}

{{/* The local control plane v2 API (/cp/local/). Mirrors how the operator
     picks its URL: the chart's simplyblock-webappapi service on 5000, HTTPS
     when the chart serves TLS. The mock takes over the whole thing when
     enabled. */}}
{{- define "sbcc.controlPlaneUrl" -}}
{{- $cc := .Values.controlCenter -}}
{{- if $cc.mock.enabled -}}
{{- printf "%s/cp/local" (include "sbcc.mockUrl" .) -}}
{{- else if $cc.controlPlaneUrl -}}
{{- $cc.controlPlaneUrl -}}
{{- else if (.Values.tls).enabled -}}
https://simplyblock-webappapi:5000
{{- else -}}
http://simplyblock-webappapi:5000
{{- end -}}
{{- end -}}

{{/* "true" when the local control plane is the chart's own service over TLS:
     the chart's CA bundle then has to be mounted so nginx can verify it. */}}
{{- define "sbcc.localCpTls" -}}
{{- $cc := .Values.controlCenter -}}
{{- if and (not $cc.mock.enabled) (not $cc.controlPlaneUrl) ((.Values.tls).enabled) -}}true{{- end -}}
{{- end -}}

{{/* The CA volume for sbcc.localCpTls, named cp-ca. The same projection the
     chart's own clients use (simplyblock.caVolume in _helpers.tpl), repeated
     here so the console templates stay self-contained: the OpenShift
     service CA ConfigMap, or the ca.crt of the cert-manager CA bundle
     Certificate issued into the release namespace. Caller pipes through
     nindent. */}}
{{- define "sbcc.cpCaVolume" -}}
{{- if eq ((.Values.tls).provider | default "cert-manager") "openshift" }}
- name: cp-ca
  configMap:
    name: simplyblock-certificate-authority
    items:
      - key: service-ca.crt
        path: ca.crt
{{- else }}
- name: cp-ca
  secret:
    secretName: simplyblock-ca-bundle-tls
    items:
      - key: ca.crt
        path: ca.crt
{{- end }}
{{- end -}}

{{/* controlCenter.localContext.labels as the entrypoint's SB_LOCAL_LABELS:
     key=value pairs joined by commas, empty values dropped. */}}
{{- define "sbcc.localLabels" -}}
{{- $parts := list -}}
{{- range $k, $v := (((.Values.controlCenter).localContext).labels | default dict) -}}
{{- if $v }}{{ $parts = append $parts (printf "%s=%s" $k $v) }}{{ end -}}
{{- end -}}
{{- join "," $parts -}}
{{- end -}}

{{/* controlCenter.contexts as the contexts.json the entrypoint reads: one
     {name, labels, namespaces, kubernetesApi, kubernetesApiHost, controlPlane}
     per context and nothing secret. With the mock enabled every context's
     Kubernetes API and control plane point at the mock's per-context routes,
     /k8s/<name> and /cp/<name>, so a demo install shows a fleet with one pod. */}}
{{- define "sbcc.contextsJson" -}}
{{- $cc := .Values.controlCenter -}}
{{- $mock := include "sbcc.mockUrl" . -}}
{{- $out := list -}}
{{- range $ctx := $cc.contexts -}}
{{- $name := required "controlCenter.contexts[].name is required" $ctx.name -}}
{{- $labels := dict -}}
{{- range $k, $v := ($ctx.labels | default dict) }}{{ if $v }}{{ $_ := set $labels $k $v }}{{ end }}{{ end -}}
{{- $c := dict "name" $name "labels" $labels "namespaces" ($ctx.namespaces | default list) -}}
{{- if $cc.mock.enabled -}}
{{- $_ := set $c "kubernetesApi" (printf "%s/k8s/%s" $mock $name) -}}
{{- $_ := set $c "controlPlane" (dict "url" (printf "%s/cp/%s" $mock $name)) -}}
{{- else -}}
{{- $_ := set $c "kubernetesApi" (required (printf "controlCenter.contexts[%s].kubernetesApi is required" $name) $ctx.kubernetesApi) -}}
{{- with $ctx.kubernetesApiHost }}{{ $_ := set $c "kubernetesApiHost" . }}{{ end -}}
{{- with $ctx.controlPlane -}}
{{- if .url }}{{ $_ := set $c "controlPlane" (dict "url" .url "host" (.host | default "")) }}{{ end -}}
{{- end -}}
{{- end -}}
{{- $out = append $out $c -}}
{{- end -}}
{{- $out | toJson -}}
{{- end -}}
