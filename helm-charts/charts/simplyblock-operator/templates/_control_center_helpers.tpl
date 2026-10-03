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
{{/* The operator's HTTP API, when this release serves one. Unset, it is the
     simplyblock-operator Service if that exists in the release namespace, and
     empty otherwise: nginx refuses to start on an upstream host it cannot
     resolve ("host not found in upstream"), and an empty upstream makes the
     console answer 503 "not part of this deployment" on the operator-backed
     screens instead (2026-10-03, a release without the operator API). */}}
{{- define "sbcc.operatorUrl" -}}
{{- if .Values.controlCenter.operatorUrl -}}
{{- .Values.controlCenter.operatorUrl -}}
{{- else if (lookup "v1" "Service" .Release.Namespace "simplyblock-operator") -}}
http://simplyblock-operator:8080
{{- end -}}
{{- end -}}

{{- define "sbcc.prometheusUrl" -}}
{{- if .Values.controlCenter.prometheusUrl -}}
{{- .Values.controlCenter.prometheusUrl -}}
{{- else -}}
{{- $p := ((.Values.prometheus).simplyblock) | default dict -}}
{{- printf "http://%s:%v" ($p.prometheusURL | default "simplyblock-prometheus") ($p.prometheusPORT | default 9090) -}}
{{- end -}}
{{- end -}}
