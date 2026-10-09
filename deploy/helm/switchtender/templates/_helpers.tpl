{{/* Expand the chart name. */}}
{{- define "switchtender.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified app name, or fullnameOverride when it is set. */}}
{{- define "switchtender.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "switchtender.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
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

{{/*
The image reference. A digest pins the exact image the release signed and renders as
repository@sha256:..., with the tag left out. A value that is not a digest is refused outright: put
in the tag's place it rendered a reference Kubernetes rejects at pull time, long after helm said
the install succeeded. Without a digest the tag applies, defaulting to the chart appVersion.
*/}}
{{- define "switchtender.image" -}}
{{- $digest := .Values.image.digest | default "" | toString -}}
{{- if $digest -}}
{{- if not (regexMatch "^sha256:[0-9a-f]{64}$" $digest) -}}
{{- fail (printf "image.digest %q is not an image digest. It must read sha256: followed by 64 hex characters, as cosign verify and the registry print it." $digest) -}}
{{- end -}}
{{- printf "%s@%s" .Values.image.repository $digest -}}
{{- else -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
{{- end -}}

{{/* The Secret name holding the encryption key: an existing one or the chart's own. */}}
{{- define "switchtender.secretName" -}}
{{- if .Values.existingSecret -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- printf "%s-secret" (include "switchtender.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
switchtender.requireDatabaseAwareImage refuses a PostgreSQL install running an image older than
1.101.0, the first release that reads its database from SWITCHTENDER_DB. An older binary ignores the
variable and opens a SQLite file of its own, so every pod would run on a private, throwaway database
while its health checks passed. A tag that is not a plain version, such as a release candidate,
cannot be compared and is left to the operator, and so is an image pinned by digest: whoever
pinned it chose the release.
*/}}
{{- define "switchtender.requireDatabaseAwareImage" -}}
{{- if and .Values.database.dsn (not .Values.image.digest) -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion | toString | trimPrefix "v" -}}
{{- if and (regexMatch "^[0-9]+[.][0-9]+[.][0-9]+$" $tag) (semverCompare "<1.101.0" $tag) -}}
{{- fail (printf "image.tag %s predates 1.101.0, the first release that reads its database from SWITCHTENDER_DB. Against PostgreSQL it would ignore database.dsn and run each pod on a SQLite file of its own. Use 1.101.0 or later, or leave image.tag empty to run the chart's own version." $tag) -}}
{{- end -}}
{{- end -}}
{{- end -}}
