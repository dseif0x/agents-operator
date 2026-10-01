{{/*
Expand the name of the chart.
*/}}
{{- define "agents-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "agents-operator.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "agents-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "agents-operator.labels" -}}
helm.sh/chart: {{ include "agents-operator.chart" . }}
{{ include "agents-operator.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "agents-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agents-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: hub
{{- end }}

{{- define "agents-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "agents-operator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* The read-only ServiceAccount session pods may opt into. */}}
{{- define "agents-operator.runnerServiceAccountName" -}}
{{- default (printf "%s-runner" (include "agents-operator.fullname" .)) .Values.runner.serviceAccount.name }}
{{- end }}

{{/*
CIDRs (with ports) session pods need for the Kubernetes API: the values
override, else the cluster's own `kubernetes` Endpoints and Service, which
`lookup` can read at install or upgrade time but not under `helm template`.
Renders YAML list items of {cidr, port}.
*/}}
{{- define "agents-operator.apiServerTargets" -}}
{{- $port := .Values.runner.networkPolicy.apiServerPort -}}
{{- if .Values.runner.networkPolicy.apiServerCIDRs }}
{{- range .Values.runner.networkPolicy.apiServerCIDRs }}
- cidr: {{ . }}
  port: {{ $port }}
{{- end }}
{{- else }}
{{- $ep := lookup "v1" "Endpoints" "default" "kubernetes" }}
{{- range $ep.subsets }}
{{- $subset := . }}
{{- range .addresses }}
{{- $ip := .ip }}
{{- range $subset.ports }}
- cidr: {{ $ip }}/32
  port: {{ .port }}
{{- end }}
{{- end }}
{{- end }}
{{- $svc := lookup "v1" "Service" "default" "kubernetes" }}
{{- if $svc.spec }}
- cidr: {{ $svc.spec.clusterIP }}/32
  port: 443
{{- end }}
{{- end }}
{{- end }}

{{/* Name of the Secret holding cookieSecret and the admin password. */}}
{{- define "agents-operator.authSecretName" -}}
{{- if .Values.auth.existingSecret }}
{{- .Values.auth.existingSecret }}
{{- else }}
{{- printf "%s-auth" (include "agents-operator.fullname" .) }}
{{- end }}
{{- end }}

{{- define "agents-operator.publicHost" -}}
{{- .Values.publicUrl | trimPrefix "https://" | trimPrefix "http://" | trimSuffix "/" }}
{{- end }}

{{- define "agents-operator.postgresqlHost" -}}
{{- printf "%s-postgresql" .Release.Name }}
{{- end }}

{{- define "agents-operator.tlsSecretName" -}}
{{- default (printf "%s-tls" (include "agents-operator.fullname" .)) .Values.ingress.tlsSecretName }}
{{- end }}
