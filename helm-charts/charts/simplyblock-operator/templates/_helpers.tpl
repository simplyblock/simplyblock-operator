{{/*
Whether this profile runs a CSI driver.

The profiles are listed rather than a negation of the ones that do not, so a
profile added later renders no driver until somebody decides it should. The
negation would do the opposite and give every future profile a CSI deployment
by default, which is the wrong way round: a driver registers a provisioner and
takes over the node plugin's socket on every worker, and a profile that wanted
neither would get both by saying nothing.

standalone and managed are here because both run workloads that mount
simplyblock volumes. What differs between them is where the control plane is,
and the driver reaches it through the credentials Secret either way
(design-simplyblockdriver.md §4.3). empty is not: its administrator writes the
SimplyblockDriver, and a chart-rendered one beside it would be a second CSI
deployment.
*/}}
{{- define "simplyblock.rendersCSIDriver" -}}
{{- if has .Values.deployment.profile (list "standalone" "managed") -}}
true
{{- end -}}
{{- end -}}

{{- define "simplyblock.controlPlaneAddr" -}}
{{- if .Values.csiConfig.simplybk.ip -}}
{{ .Values.csiConfig.simplybk.ip }}
{{- else -}}
http://simplyblock-webappapi.{{ .Release.Namespace }}.svc:5000
{{- end -}}
{{- end -}}

{{/*
Volume named "tls" holding only ca.crt, for client-only consumers without mTLS.
- openshift: cabundle ConfigMap (renamed key).
- cert-manager: project ca.crt out of simplyblock-ca-bundle-tls, a Certificate
  issued by the chart's ClusterIssuer into the release namespace. The root CA
  secret lives in the cert-manager namespace and cannot be mounted directly.
Caller pipes through `nindent N`.
*/}}
{{- define "simplyblock.caVolume" -}}
{{- if .Values.tls.enabled -}}
{{- if eq .Values.tls.provider "openshift" }}
- name: tls
  configMap:
    name: simplyblock-certificate-authority
    items:
    - key: service-ca.crt
      path: ca.crt
{{- else if eq .Values.tls.provider "cert-manager" }}
- name: tls
  secret:
    # simplyblock-ca-bundle-tls is issued in the release namespace by the chart's
    # ClusterIssuer, so pods can mount it. The CA secret itself lives in the
    # cert-manager namespace and is not directly mountable from here.
    secretName: simplyblock-ca-bundle-tls
    items:
    - key: ca.crt
      path: ca.crt
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Volume named "tls" for client pods, with optional mTLS.
Args: dict "ctx" $root "clientSecret" <client-cert-secret-name>
- mutual_enabled=false: delegate to simplyblock.caVolume (CA-only mount).
- mutual_enabled=true: project the client cert Secret alongside the
  per-provider CA bundle.
Caller pipes through `nindent N`.
*/}}
{{- define "simplyblock.clientTlsVolume" -}}
{{- $ctx := .ctx -}}
{{- $clientSecret := .clientSecret -}}
{{- if $ctx.Values.tls.enabled -}}
{{- if not $ctx.Values.tls.mutual_enabled -}}
{{- include "simplyblock.caVolume" $ctx }}
{{- else if eq $ctx.Values.tls.provider "openshift" }}
- name: tls
  projected:
    sources:
    - secret:
        name: {{ $clientSecret }}
    - configMap:
        name: simplyblock-certificate-authority
        items:
        - key: service-ca.crt
          path: ca.crt
{{- else if eq $ctx.Values.tls.provider "cert-manager" }}
- name: tls
  secret:
    # cert-manager populates the client cert secret with tls.crt, tls.key, and
    # ca.crt, so a projected volume is not needed.
    secretName: {{ $clientSecret }}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Shared TLS volumeMount entry for the "tls" volume, gated on tls.enabled.
Caller pipes through `nindent N` to land it inside a `volumeMounts:` list.
*/}}
{{- define "simplyblock.tlsVolumeMount" -}}
{{- if .Values.tls.enabled }}
- name: tls
  mountPath: /etc/simplyblock/tls
  readOnly: true
{{- end }}
{{- end -}}

{{/*
The operator's TLS environment.

It carries one thing: SB_TLS_PROVIDER, which selects where the admission
webhooks' serving certificate comes from. The installation's own TLS is stated by
the ControlPlane (spec.source.local.tls) and read from it, so nothing else about
TLS is set here. The webhook certificate has to exist before the manager starts,
which is before any ControlPlane can be read.
Caller pipes through `nindent N`.
*/}}
{{- define "simplyblock.tlsEnv" -}}
{{- if .Values.tls.enabled }}
- name: SB_TLS_PROVIDER
  value: {{ .Values.tls.provider | quote }}
{{- end }}
{{- end -}}

{{/*
How long Graylog writes into one index before it rotates to the next, as whole
hours. Graylog deletes the oldest index once there are more than maxNumberIndex
of them, so retentionPeriod is divided across that many: 7d over 3 indices is
56h each. The period is a count followed by h, d, or w.
*/}}
{{- define "simplyblock.graylogMaxTimePerIndex" -}}
{{- $graylog := .Values.controlplane.observability.graylog }}
{{- $period := toString $graylog.retentionPeriod }}
{{- if not (regexMatch "^[1-9][0-9]*[hdw]$" $period) }}
{{- fail (printf "controlplane.observability.graylog.retentionPeriod %q is not a count followed by h, d, or w, such as 7d" $period) }}
{{- end }}
{{- $unit := get (dict "h" 1 "d" 24 "w" 168) (regexFind "[hdw]$" $period) }}
{{- $hours := mul (atoi (regexFind "^[0-9]+" $period)) $unit }}
{{- $indices := atoi (toString $graylog.maxNumberIndex) }}
{{- if lt $indices 1 }}
{{- fail (printf "controlplane.observability.graylog.maxNumberIndex %q is not a positive count" (toString $graylog.maxNumberIndex)) }}
{{- end }}
{{- $perIndex := div $hours $indices }}
{{- if lt $perIndex 1 }}
{{- fail (printf "controlplane.observability.graylog.retentionPeriod %s over maxNumberIndex %d indices is under an hour each; lengthen the period or lower the count" $period $indices) }}
{{- end }}
{{- printf "%dh" $perIndex }}
{{- end -}}

{{/*
The Secret holding the CSI link's serving certificate, or nothing when the link
is served in plaintext.

csiLink.certSecret names one the deployer provides. Otherwise the link follows
tls.enabled like everything else in this chart: the certificate is issued into
simplyblock-csi-link-tls, by the chart's ClusterIssuer under cert-manager
(csi-link-svc.yaml) or by the service-CA operator on OpenShift (the Service's
serving-cert annotation).
*/}}
{{- define "simplyblock.csiLinkCertSecret" -}}
{{- if .Values.csiLink.certSecret -}}
{{ .Values.csiLink.certSecret }}
{{- else if .Values.tls.enabled -}}
simplyblock-csi-link-tls
{{- end -}}
{{- end -}}
