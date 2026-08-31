{{/*
Expand the name of the chart.
*/}}
{{- define "excalidraw-wopi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
Helm truncates the name at 63 characters, the DNS label limit.
*/}}
{{- define "excalidraw-wopi.fullname" -}}
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

{{/*
Common labels.
*/}}
{{- define "excalidraw-wopi.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "excalidraw-wopi.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "excalidraw-wopi.selectorLabels" -}}
app.kubernetes.io/name: {{ include "excalidraw-wopi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
The chart-managed Secret name. The helper truncates the name to 55
characters, so the "-secrets" suffix stays inside the 63-character
DNS label limit.
*/}}
{{- define "excalidraw-wopi.managedSecretName" -}}
{{- printf "%s-secrets" (include "excalidraw-wopi.fullname" . | trunc 55 | trimSuffix "-") }}
{{- end }}

{{/*
The Secret name that holds proof-key.pem and session-secret.
secret.create takes precedence: a chart-managed Secret always wins over
an existing one. Fails the render when neither is set, so a
misconfigured release does not deploy silently.
*/}}
{{- define "excalidraw-wopi.secretName" -}}
{{- if .Values.secret.create }}
{{- include "excalidraw-wopi.managedSecretName" . }}
{{- else if .Values.secret.existingSecret }}
{{- .Values.secret.existingSecret }}
{{- else }}
{{- fail "Set secret.existingSecret to the name of a Secret holding proof-key.pem and session-secret, or set secret.create to true." }}
{{- end }}
{{- end }}

{{/*
The headless Service name that carries the peer set through DNS.
The helper truncates the name to 57 characters, so the "-peers"
suffix stays inside the 63-character DNS label limit.
*/}}
{{- define "excalidraw-wopi.peersServiceName" -}}
{{- printf "%s-peers" (include "excalidraw-wopi.fullname" . | trunc 57 | trimSuffix "-") }}
{{- end }}
