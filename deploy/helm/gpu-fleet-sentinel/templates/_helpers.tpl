{{- define "sentinel.name" -}}gpu-fleet-sentinel{{- end -}}

{{- define "sentinel.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "sentinel.labels" -}}
app.kubernetes.io/name: {{ include "sentinel.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "sentinel.selector" -}}
{{ .Values.nodeSelectorLabel.key }}={{ .Values.nodeSelectorLabel.value }}
{{- end -}}
