{{/*
Chart name, truncated to 63 chars for Kubernetes name limits.
*/}}
{{- define "rhaii.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name, truncated to 63 chars.
*/}}
{{- define "rhaii.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "rhaii.labels" -}}
app.kubernetes.io/name: {{ include "rhaii.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
{{- end -}}

{{/*
Name of the controller ServiceAccount. Defaults to the fullname when
serviceAccount.name is empty.
*/}}
{{- define "rhaii.serviceAccountName" -}}
{{- default (include "rhaii.fullname" .) .Values.serviceAccount.name -}}
{{- end -}}

{{/*
Effective image-pull Secret name. If dockerConfigJson is provided the chart
creates the Secret (name defaults to "<fullname>-pull"); otherwise it returns
pullSecret.name (an existing Secret to reference), or "" if unset.
*/}}
{{- define "rhaii.pullSecretName" -}}
{{- if .Values.pullSecret.dockerConfigJson -}}
{{- default (printf "%s-pull" (include "rhaii.fullname" .)) .Values.pullSecret.name -}}
{{- else -}}
{{- .Values.pullSecret.name -}}
{{- end -}}
{{- end -}}
