{{- define "laboratory.deviceAccess.selectorLabels" -}}
app: laboratory-device-access
{{- end }}

{{- /* The runtime socket the service mounts: terminal.containerdSocket, else the one of the node-agent. */ -}}
{{- define "laboratory.deviceAccess.socket" -}}
{{- .Values.terminal.containerdSocket | default .Values.nodeAgent.criSocket -}}
{{- end }}

{{- /* The namespace of the service: its own, so that the one Role the proxy gets (EndpointSlices) covers nothing else. */ -}}
{{- define "laboratory.deviceAccess.namespace" -}}{{ .Values.terminal.namespace }}{{- end }}

{{- /* The service runs when either of its two users is on: the console (terminal.enabled) or the lab checker (checks.enabled). */ -}}
{{- define "laboratory.deviceAccess.enabled" -}}
{{- if or .Values.terminal.enabled .Values.checks.enabled -}}true{{- end -}}
{{- end }}
