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
host:port of the platform registry Service inside the cluster (the release namespace).
*/}}
{{- define "laboratory.registryAddr" -}}
{{- printf "laboratory-registry.%s.svc:5000" .Release.Namespace -}}
{{- end }}

{{/*
The release tag of an image: the image's own tag when set, else the global image.tag, else the chart appVersion. One release tag
drives every image; the per-image tag stays for pinning another build. Arguments: dict "image" <values .image> "root" $.
*/}}
{{- define "laboratory.tag" -}}
{{- toString (.image.tag | default .root.Values.image.tag | default .root.Chart.AppVersion) -}}
{{- end }}

{{/*
repository:tag of an image. Arguments: dict "image" <values .image> "root" $.
*/}}
{{- define "laboratory.imageRef" -}}
{{- printf "%s:%s" .image.repository (include "laboratory.tag" .) -}}
{{- end }}

{{/*
Constants of the platform that the images share (internal/names): the WireGuard port inside the VPN pods, the ports of the proxy
containers. Not values: they are baked into the images, so nothing has to pass them.
*/}}
{{- define "laboratory.wgPort" -}}51820{{- end }}

{{/*
The UDP port clients connect to, outside the cluster (the port of the LoadBalancer Service): proxy.wg.publicPort.
*/}}
{{- define "laboratory.wgPublicPort" -}}{{ .Values.proxy.wg.publicPort | int }}{{- end }}

{{/*
The WireGuard address advertised to clients (PUBLIC_VPN_ENDPOINT): operator.publicVPNEndpoint, with proxy.wg.publicPort appended when it
names no port.
*/}}
{{- define "laboratory.publicVPNEndpoint" -}}
{{- $endpoint := required "operator.publicVPNEndpoint is required" .Values.operator.publicVPNEndpoint -}}
{{- if regexMatch ":[0-9]+$" $endpoint -}}{{ $endpoint }}{{- else -}}{{ printf "%s:%s" $endpoint (include "laboratory.wgPublicPort" .) }}{{- end -}}
{{- end }}
{{- define "laboratory.l7Port" -}}8443{{- end }}
{{- define "laboratory.l7HealthPort" -}}8081{{- end }}
{{- define "laboratory.wgHealthPort" -}}8082{{- end }}
{{- define "laboratory.priorityClass.platform" -}}{{ .Values.priorityClasses.platform.name }}{{- end }}
{{- define "laboratory.priorityClass.group" -}}{{ .Values.priorityClasses.group.name }}{{- end }}
{{- define "laboratory.priorityClass.device" -}}{{ .Values.priorityClasses.device.name }}{{- end }}

{{/*
The public host of the management agent: agent.domain, else ctl.<operator.baseDomain>.
*/}}
{{- define "laboratory.agentDomain" -}}
{{- if .Values.agent.domain -}}{{ .Values.agent.domain }}{{- else -}}{{ printf "ctl.%s" (required "operator.baseDomain is required" .Values.operator.baseDomain) }}{{- end -}}
{{- end }}

{{/*
The settings that more than one binary reads, as ONE map under ONE name each (env names of the operator): the operator gets it as its
ConfigMap, the management agent as its env. Each chart value is written here once, so the two can never disagree. A binary reads only the
names it knows. The map is YAML: consumers use `include ... | fromYaml`.
*/}}
{{- define "laboratory.sharedEnv" -}}
BASE_DOMAIN: {{ required "operator.baseDomain is required" .Values.operator.baseDomain | quote }}
PUBLIC_VPN_ENDPOINT: {{ include "laboratory.publicVPNEndpoint" . | quote }}
IMAGE_PULL_SECRETS: {{ include "laboratory.pullSecretNames" . | quote }}
LAB_NODE_SELECTOR: {{ include "laboratory.labNodeSelector" . | quote }}
LAB_TOLERATIONS: {{ .Values.labWorkloads.tolerations | toJson | quote }}
VPN_CPU: {{ .Values.vpn.resources.cpu | quote }}
VPN_MEMORY: {{ .Values.vpn.resources.memory | quote }}
GATEWAY_CPU: {{ .Values.inetGateway.resources.cpu | quote }}
GATEWAY_MEMORY: {{ .Values.inetGateway.resources.memory | quote }}
VPN_BASE_CPU: {{ .Values.vpn.sizing.baseCpu | quote }}
VPN_BASE_MEMORY: {{ .Values.vpn.sizing.baseMemory | quote }}
VPN_PER_USER_CPU: {{ .Values.vpn.sizing.perUserCpu | quote }}
VPN_PER_USER_MEMORY: {{ .Values.vpn.sizing.perUserMemory | quote }}
VPN_MAX_USERS: {{ .Values.vpn.sizing.maxUsers | quote }}
GATEWAY_BASE_CPU: {{ .Values.inetGateway.sizing.baseCpu | quote }}
GATEWAY_BASE_MEMORY: {{ .Values.inetGateway.sizing.baseMemory | quote }}
GATEWAY_PER_LAB_CPU: {{ .Values.inetGateway.sizing.perLabCpu | quote }}
GATEWAY_PER_LAB_MEMORY: {{ .Values.inetGateway.sizing.perLabMemory | quote }}
GATEWAY_MAX_LABS: {{ .Values.inetGateway.sizing.maxLabs | quote }}
SCHEDULER_ENABLED: {{ .Values.scheduler.enabled | quote }}
SCHEDULER_MAX_PODS: {{ .Values.scheduler.maxPods | quote }}
SCHEDULER_PLATFORM_RESERVE_PERCENT: {{ .Values.scheduler.platformReservePercent | quote }}
SCHEDULER_PLATFORM_RESERVE_CPU: {{ .Values.scheduler.platformReserveCpu | quote }}
SCHEDULER_PLATFORM_RESERVE_MEMORY: {{ .Values.scheduler.platformReserveMemory | quote }}
DEVICE_DEFAULT_CPU: {{ .Values.limits.device.defaultCpu | quote }}
DEVICE_DEFAULT_MEMORY: {{ .Values.limits.device.defaultMemory | quote }}
DEVICE_MAX_CPU: {{ .Values.limits.device.maxCpu | quote }}
DEVICE_MAX_MEMORY: {{ .Values.limits.device.maxMemory | quote }}
STATE_PERSISTENCE_ENABLED: {{ .Values.devices.statePersistence.enabled | quote }}
STATE_DEBOUNCE: {{ .Values.devices.statePersistence.debounce | quote }}
STATE_EXCLUDE_PATHS: {{ join "," .Values.devices.statePersistence.excludePaths | quote }}
STATE_WRITE_QUOTA: {{ .Values.devices.statePersistence.writeQuota | quote }}
STATE_MAX_FILE_SIZE: {{ .Values.devices.statePersistence.maxFileSize | quote }}
STATE_MAX_ENTRIES: {{ .Values.devices.statePersistence.maxEntries | quote }}
STATE_TENANT_QUOTA: {{ .Values.devices.statePersistence.tenantQuota | quote }}
IMAGE_CACHE_ENABLED: {{ .Values.registry.cache.enabled | quote }}
IMAGE_CACHE_PIN_TTL: {{ .Values.registry.cache.pinTTL | quote }}
IMAGE_CACHE_REGISTRIES: {{ include "laboratory.cacheRegistryNames" . | quote }}
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
The image pull policy of a container: the explicit one when set, else IfNotPresent (every image has an exact tag, see validate.yaml).
Arguments: dict "policy" <values pullPolicy> "tag" <effective tag>; the tag is not used, it stays so that the callers do not change.
*/}}
{{- define "laboratory.pullPolicy" -}}
{{- if .policy -}}{{ .policy }}{{- else -}}IfNotPresent{{- end -}}
{{- end }}

{{/*
Non-empty when the proxy Deployment may surge a pod during a rollout: proxy.surge (true/false) when set, else when a live install can count
the nodes and there are more of them than replicas (the pod anti-affinity is required, so a surge pod needs a node of its own).
*/}}
{{- define "laboratory.proxySurge" -}}
{{- if kindIs "bool" .Values.proxy.surge -}}
{{- if .Values.proxy.surge -}}true{{- end -}}
{{- else -}}
{{- $nodes := (lookup "v1" "Node" "" "").items -}}
{{- if and $nodes (gt (len $nodes) (int .Values.proxy.replicas)) -}}true{{- end -}}
{{- end -}}
{{- end }}
