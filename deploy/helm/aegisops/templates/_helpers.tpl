{{- define "aegisops.name" -}}
{{- printf "%s" .Release.Name | trunc 45 | trimSuffix "-" -}}
{{- end -}}
{{- define "aegisops.credentials" -}}
{{- default (printf "%s-credentials" (include "aegisops.name" .)) .Values.credentials.existingSecret -}}
{{- end -}}
{{- define "aegisops.image" -}}
{{- if .Values.image.digest -}}
{{ printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else -}}
{{ printf "%s:%s" .Values.image.repository (required "image.tag or image.digest is required" .Values.image.tag) }}
{{- end -}}
{{- end -}}
{{- define "aegisops.labels" -}}
app.kubernetes.io/name: aegisops
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: Helm
{{- end -}}
