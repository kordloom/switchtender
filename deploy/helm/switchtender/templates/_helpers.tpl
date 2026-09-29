{{/* Expand the chart name. */}}
{{- define "switchtender.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified app name. */}}
{{- define "switchtender.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "switchtender.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Common labels. */}}
{{- define "switchtender.labels" -}}
app.kubernetes.io/name: {{ include "switchtender.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/* Selector labels. */}}
{{- define "switchtender.selectorLabels" -}}
app.kubernetes.io/name: {{ include "switchtender.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* The image reference, defaulting the tag to the chart appVersion. */}}
{{- define "switchtender.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/* The Secret name holding the encryption key: an existing one or the chart's own. */}}
{{- define "switchtender.secretName" -}}
{{- if .Values.existingSecret -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- printf "%s-secret" (include "switchtender.fullname" .) -}}
{{- end -}}
{{- end -}}
