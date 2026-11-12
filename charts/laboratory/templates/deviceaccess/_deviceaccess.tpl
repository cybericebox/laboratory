{{- define "laboratory.deviceAccess.selectorLabels" -}}
app: laboratory-device-access
{{- end }}

{{- /* The runtime socket the service mounts: terminal.containerdSocket, else the one of the node-agent. */ -}}
{{- define "laboratory.deviceAccess.socket" -}}
{{- .Values.terminal.containerdSocket | default .Values.nodeAgent.criSocket -}}
{{- end }}
