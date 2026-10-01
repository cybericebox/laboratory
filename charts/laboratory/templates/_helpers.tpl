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
Non-empty when the platform registry (zot) is deployed: device state persistence
or the image cache is on.
*/}}
{{- define "laboratory.registryEnabled" -}}
{{- if or .Values.devices.statePersistence.enabled .Values.registry.cache.enabled -}}true{{- end -}}
{{- end }}

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
