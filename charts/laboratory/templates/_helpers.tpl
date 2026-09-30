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
{{- define "laboratory.snapshotRegistryAddr" -}}
{{- printf "laboratory-snapshots.%s.svc:5000" .Release.Namespace -}}
{{- end }}

{{/*
Labels that select the snapshot registry pod.
*/}}
{{- define "laboratory.snapshotRegistrySelector" -}}
app: laboratory-snapshots
{{- end }}
