{{- define "laboratory.deviceAccess.selectorLabels" -}}
app: laboratory-device-access
{{- end }}

{{- /* The runtime socket the service mounts: terminal.containerdSocket, else the one of the node-agent. */ -}}
{{- define "laboratory.deviceAccess.socket" -}}
{{- .Values.terminal.containerdSocket | default .Values.nodeAgent.criSocket -}}
{{- end }}

{{- /* The namespace of the service: its own, so that the one Role the proxy gets (EndpointSlices) covers nothing else. */ -}}
{{- define "laboratory.deviceAccess.namespace" -}}{{ .Values.terminal.namespace }}{{- end }}

{{- /* The HMAC key of the cable stream between the device access pods (consoleCables): made once per render and cached in
.Values._consoleCableKey, generated once and kept across upgrades (read back with lookup). Only the device access pods mount it. */ -}}
{{- define "laboratory.consoleCableKey" -}}
{{- if not (hasKey .Values "_consoleCableKey") -}}
{{- $existing := lookup "v1" "Secret" (include "laboratory.deviceAccess.namespace" .) "laboratory-console-cable-key" -}}
{{- $key := randAlphaNum 64 -}}
{{- if and $existing $existing.data (hasKey $existing.data "key") -}}
{{- $key = index $existing.data "key" | b64dec -}}
{{- end -}}
{{- $_ := set .Values "_consoleCableKey" $key -}}
{{- end -}}
{{- end }}

{{- /* The service runs when any of its users is on: the console (terminal.enabled), the lab checker (checks.enabled) or the console lines
(consoleCables.enabled). */ -}}
{{- define "laboratory.deviceAccess.enabled" -}}
{{- if or .Values.terminal.enabled .Values.checks.enabled .Values.consoleCables.enabled -}}true{{- end -}}
{{- end }}
