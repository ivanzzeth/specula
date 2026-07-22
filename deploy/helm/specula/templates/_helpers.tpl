{{/*
Expand the name of the chart.
*/}}
{{- define "specula.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "specula.fullname" -}}
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

{{- define "specula.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end }}

{{- define "specula.labels" -}}
helm.sh/chart: {{ include "specula.chart" . }}
{{ include "specula.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "specula.selectorLabels" -}}
app.kubernetes.io/name: {{ include "specula.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/* Bitnami subchart service names (release-name + dependency chart name). */}}
{{- define "specula.postgresql.fullname" -}}
{{- printf "%s-postgresql" .Release.Name }}
{{- end }}

{{- define "specula.postgresql.host" -}}
{{- include "specula.postgresql.fullname" . }}
{{- end }}

{{- define "specula.postgresql.secretName" -}}
{{- include "specula.postgresql.fullname" . }}
{{- end }}

{{- define "specula.redis.fullname" -}}
{{- printf "%s-redis" .Release.Name }}
{{- end }}

{{- define "specula.redis.host" -}}
{{- printf "%s-master" (include "specula.redis.fullname" .) }}
{{- end }}

{{- define "specula.redis.secretName" -}}
{{- include "specula.redis.fullname" . }}
{{- end }}

{{- define "specula.minio.fullname" -}}
{{- printf "%s-minio" .Release.Name }}
{{- end }}

{{- define "specula.minio.host" -}}
{{- include "specula.minio.fullname" . }}
{{- end }}

{{- define "specula.minio.secretName" -}}
{{- include "specula.minio.fullname" . }}
{{- end }}

{{- define "specula.secretName" -}}
{{- include "specula.fullname" . }}
{{- end }}

{{- define "specula.configMapName" -}}
{{- include "specula.fullname" . }}
{{- end }}

{{/* Resolve S3 endpoint: explicit value, or in-cluster MinIO. */}}
{{- define "specula.blob.s3.endpoint" -}}
{{- if .Values.blob.s3.endpoint }}
{{- .Values.blob.s3.endpoint }}
{{- else if .Values.minio.enabled }}
{{- printf "http://%s:9000" (include "specula.minio.host" .) }}
{{- else }}
{{- fail "blob.s3.endpoint is required when minio.enabled is false and blob.driver is s3" }}
{{- end }}
{{- end }}

{{/* Shared blob PVC claim name. */}}
{{- define "specula.blob.pvcName" -}}
{{- if .Values.blob.local.existingClaim }}
{{- .Values.blob.local.existingClaim }}
{{- else }}
{{- printf "%s-blobs" (include "specula.fullname" .) }}
{{- end }}
{{- end }}

{{/* Validate HA blob backend at template time. */}}
{{- define "specula.validate.ha.blob" -}}
{{- if eq .Values.blob.driver "local" }}
{{- if not .Values.blob.local.shared }}
{{- fail "HA requires blob.local.shared=true when blob.driver=local" }}
{{- end }}
{{- else if ne .Values.blob.driver "s3" }}
{{- fail (printf "blob.driver must be \"s3\" or \"local\", got %q" .Values.blob.driver) }}
{{- end }}
{{- end }}
