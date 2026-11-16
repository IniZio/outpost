{{- define "outpost.labels" -}}
app.kubernetes.io/name: outpost
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end -}}

{{- define "outpost.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "outpost.secretName" -}}
{{ .Values.secrets.existingSecret | default (printf "%s-secrets" .Release.Name) }}
{{- end -}}

{{/* Settings shared by the migrate init container and the server. */}}
{{- define "outpost.env" -}}
- { name: OUTPOST_LISTEN, value: {{ printf ":%d" (int .Values.service.port) | quote }} }
- { name: OUTPOST_PUBLIC_URL, value: {{ printf "https://%s" (required "host is required" .Values.host) | quote }} }
- { name: OUTPOST_AGENT_URL, value: {{ printf "wss://%s/machines/link" .Values.host | quote }} }
- { name: OUTPOST_BASE_IMAGE, value: {{ required "baseImage is required" .Values.baseImage | quote }} }
- { name: OUTPOST_ALLOWED_LOGINS, value: {{ .Values.allowedLogins | quote }} }
- { name: OUTPOST_MAX_RUNNING, value: {{ .Values.maxRunning | quote }} }
- { name: OUTPOST_LOG, value: {{ .Values.log | quote }} }
- { name: OUTPOST_DRAIN, value: {{ printf "%ds" (int .Values.drainSeconds) | quote }} }
- { name: OUTPOST_CLAUDE_MODEL, value: {{ .Values.claude.model | quote }} }
- { name: OUTPOST_CLAUDE_BIN, value: {{ .Values.claude.bin | quote }} }
- { name: OUTPOST_CLAUDE_MARKETPLACES, value: {{ .Values.claude.marketplaces | quote }} }
- { name: OUTPOST_CLAUDE_PLUGINS, value: {{ .Values.claude.plugins | quote }} }
- { name: OUTPOST_PROXY_URL, value: {{ .Values.proxy.url | quote }} }
{{- if .Values.proxy.url }}
- { name: OUTPOST_PROXY_CA_FILE, value: /etc/outpost/proxy/ca.pem }
{{- end }}
{{- range $k, $v := .Values.extraEnv }}
- { name: {{ $k | quote }}, value: {{ $v | quote }} }
{{- end }}
{{- end -}}
