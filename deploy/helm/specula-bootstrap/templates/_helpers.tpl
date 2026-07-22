{{- define "specula-bootstrap.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "specula-bootstrap.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "specula-bootstrap.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "specula-bootstrap.labels" -}}
helm.sh/chart: {{ include "specula-bootstrap.chart" . }}
{{ include "specula-bootstrap.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: bootstrap
{{- end -}}

{{- define "specula-bootstrap.selectorLabels" -}}
app.kubernetes.io/name: {{ include "specula-bootstrap.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
