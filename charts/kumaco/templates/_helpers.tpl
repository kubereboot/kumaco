{{/* The name of the objects of the chart. */}}
{{- define "kumaco.name" -}}
{{- .Values.nameOverride | default .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* The name of the controller: its Lease, and the source of its events. */}}
{{- define "kumaco.controllerName" -}}
{{- .Values.controller.name | default (include "kumaco.name" .) -}}
{{- end -}}

{{- define "kumaco.selectorLabels" -}}
app.kubernetes.io/name: kumaco
app.kubernetes.io/instance: {{ include "kumaco.name" . }}
{{- end -}}

{{- define "kumaco.labels" -}}
{{ include "kumaco.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}
