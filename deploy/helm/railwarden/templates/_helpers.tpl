{{/* Expand the chart name. */}}
{{- define "railwarden.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified app name. */}}
{{- define "railwarden.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "railwarden.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Common labels. */}}
{{- define "railwarden.labels" -}}
app.kubernetes.io/name: {{ include "railwarden.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/* Selector labels. */}}
{{- define "railwarden.selectorLabels" -}}
app.kubernetes.io/name: {{ include "railwarden.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* The image reference, defaulting the tag to the chart appVersion. */}}
{{- define "railwarden.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/* The Secret name holding the encryption key: an existing one or the chart's own. */}}
{{- define "railwarden.secretName" -}}
{{- if .Values.existingSecret -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- printf "%s-secret" (include "railwarden.fullname" .) -}}
{{- end -}}
{{- end -}}
