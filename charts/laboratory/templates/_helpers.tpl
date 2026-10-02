{{/*
Expand the name of the chart.
*/}}
{{- define "laboratory.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "laboratory.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Chart label.
*/}}
{{- define "laboratory.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "laboratory.labels" -}}
helm.sh/chart: {{ include "laboratory.chart" . }}
{{ include "laboratory.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "laboratory.selectorLabels" -}}
app.kubernetes.io/name: {{ include "laboratory.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Comma-separated names of .Values.imagePullSecrets (a list of `- name: x`): what the
operator copies into every lab group namespace and sets on lab pods.
*/}}
{{- define "laboratory.pullSecretNames" -}}
{{- $n := list -}}
{{- range .Values.imagePullSecrets -}}
{{- $n = append $n (required "imagePullSecrets entries need a name" .name) -}}
{{- end -}}
{{- join "," $n -}}
{{- end -}}

{{/*
host:port of the snapshot registry Service inside the cluster.
*/}}
{{- define "laboratory.registryAddr" -}}
{{- printf "laboratory-registry.%s.svc:5000" .Release.Namespace -}}
{{- end }}

{{/*
Labels that select the snapshot registry pod.
*/}}
{{- define "laboratory.registrySelector" -}}
app: laboratory-registry
{{- end }}

{{/*
Comma-separated names of the registries the image cache serves.
*/}}
{{- define "laboratory.cacheRegistryNames" -}}
{{- $names := list -}}
{{- range (include "laboratory.cacheUpstreams" . | fromJsonArray) }}{{ $names = append $names .name }}{{ end -}}
{{- join "," $names -}}
{{- end }}

{{/*
Non-empty when the platform registry (zot) is deployed. It always is: neither
devices.statePersistence nor registry.cache decides it, so flipping one of them never
removes the registry (and the snapshots in it).
*/}}
{{- define "laboratory.registryEnabled" -}}true{{- end }}

{{/*
Upstream registries of the image cache as a JSON list of {name, url}: the built-in
ones named in registry.cache.registries, plus registry.cache.extraRegistries.
*/}}
{{- define "laboratory.cacheUpstreams" -}}
{{- $known := dict "docker.io" "https://registry-1.docker.io" "ghcr.io" "https://ghcr.io" "quay.io" "https://quay.io" "registry.k8s.io" "https://registry.k8s.io" -}}
{{- $out := list -}}
{{- range .Values.registry.cache.registries -}}
{{- $url := index $known . -}}
{{- if not $url -}}{{- fail (printf "registry.cache.registries: %q is not built in; add it to registry.cache.extraRegistries with its url" .) -}}{{- end -}}
{{- $out = append $out (dict "name" . "url" $url) -}}
{{- end -}}
{{- range .Values.registry.cache.extraRegistries -}}
{{- $out = append $out (dict "name" (required "registry.cache.extraRegistries entries need a name" .name) "url" (required "registry.cache.extraRegistries entries need a url" .url)) -}}
{{- end -}}
{{- toJson $out -}}
{{- end }}

{{/*
The credentials of the platform registry (zot), made once per render and cached in .Values._registryCreds so that the
registry Secret and the copy for the agent (another namespace) agree. Generated once and kept across upgrades (read back with
lookup). Two accounts: writer (the operator and the node-agents write snapshots) and reader (it may read the snapshots of the
labs and the base repository; everything else in the registry is the public image cache, anonymous read). A registry made by an
earlier version has no reader yet: it is added, the writer stays.
*/}}
{{- define "laboratory.registryCreds" -}}
{{- if not (hasKey .Values "_registryCreds") -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace "laboratory-registry" -}}
{{- $writer := randAlphaNum 40 -}}
{{- $reader := randAlphaNum 40 -}}
{{- $htpasswd := "" -}}
{{- if and $existing $existing.data -}}
{{- $writer = index $existing.data "password" | b64dec -}}
{{- if hasKey $existing.data "readerPassword" -}}
{{- $reader = index $existing.data "readerPassword" | b64dec -}}
{{- $htpasswd = index $existing.data "htpasswd" | b64dec -}}
{{- else -}}
{{- $htpasswd = printf "%s\n%s" (index $existing.data "htpasswd" | b64dec | trim) (htpasswd "reader" $reader) -}}
{{- end -}}
{{- else -}}
{{- $htpasswd = printf "%s\n%s" (htpasswd "writer" $writer) (htpasswd "reader" $reader) -}}
{{- end -}}
{{- $_ := set .Values "_registryCreds" (dict "writer" $writer "reader" $reader "htpasswd" $htpasswd) -}}
{{- end -}}
{{- end -}}

{{/*
The capabilities the operator may add to a pod (a CEL list literal for the admission policy): the base set of every device, the
additions of the device profiles, and what the VPN and gateway pods need. A Go test keeps this equal to what the code can add.
*/}}
{{- define "laboratory.operatorCapabilities" -}}
['AUDIT_WRITE', 'CHOWN', 'DAC_OVERRIDE', 'FOWNER', 'FSETID', 'IPC_LOCK', 'KILL', 'LINUX_IMMUTABLE', 'NET_ADMIN', 'NET_BIND_SERVICE', 'NET_RAW', 'SETGID', 'SETPCAP', 'SETUID', 'SYS_CHROOT', 'SYS_PTRACE']
{{- end -}}

{{/*
The node selector of every lab pod (VPN, gateway, device): labWorkloads.nodeSelector plus the node-agent-ready label that the
node-agent sets on its node. Lab pods run only where a ready node-agent serves them (see DEPLOY.md, "Which nodes run labs").
*/}}
{{- define "laboratory.labNodeSelector" -}}
{{- merge (dict "laboratory.cybericebox.com/node-agent-ready" "true") (deepCopy .Values.labWorkloads.nodeSelector) | toJson -}}
{{- end }}

{{/*
The image pull policy of a container: the explicit one when set, else it follows the tag. A tag that moves (latest, or none) is pulled every
time; an exact version tag only when the node does not have it. Arguments: dict "policy" <values pullPolicy> "tag" <effective tag>.
*/}}
{{- define "laboratory.pullPolicy" -}}
{{- if .policy -}}{{ .policy }}{{- else if or (eq (toString .tag) "") (eq (toString .tag) "latest") -}}Always{{- else -}}IfNotPresent{{- end -}}
{{- end }}
