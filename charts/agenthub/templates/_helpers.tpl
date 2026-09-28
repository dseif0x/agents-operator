{{/*
Expand the name of the chart.
*/}}
{{- define "agenthub.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "agenthub.fullname" -}}
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

{{- define "agenthub.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "agenthub.labels" -}}
helm.sh/chart: {{ include "agenthub.chart" . }}
{{ include "agenthub.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "agenthub.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agenthub.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: hub
{{- end }}

{{- define "agenthub.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "agenthub.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* Name of the Secret holding cookieSecret and the admin password. */}}
{{- define "agenthub.authSecretName" -}}
{{- if .Values.auth.existingSecret }}
{{- .Values.auth.existingSecret }}
{{- else }}
{{- printf "%s-auth" (include "agenthub.fullname" .) }}
{{- end }}
{{- end }}

{{- define "agenthub.publicHost" -}}
{{- .Values.publicUrl | trimPrefix "https://" | trimPrefix "http://" | trimSuffix "/" }}
{{- end }}

{{- define "agenthub.postgresqlHost" -}}
{{- printf "%s-postgresql" .Release.Name }}
{{- end }}

{{- define "agenthub.tlsSecretName" -}}
{{- default (printf "%s-tls" (include "agenthub.fullname" .)) .Values.ingress.tlsSecretName }}
{{- end }}
