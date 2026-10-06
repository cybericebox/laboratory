{{/*
All tenants: "default" (always) overlaid by .Values.tenants. Each value is the tenant's spec.
The default tenant may use device persistence unless the values say otherwise.
*/}}
{{- define "laboratory.tenants" -}}
{{- $all := dict "default" (dict "persistence" (dict "allowed" true)) -}}
{{- range $name, $spec := .Values.tenants -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $name) -}}
{{- fail (printf "tenants.%s: a tenant name is a DNS-1123 label (it is the client certificate CN)" $name) -}}
{{- end -}}
{{- if gt (len $name) 63 -}}
{{- fail (printf "tenants.%s: a tenant name is at most 63 characters" $name) -}}
{{- end -}}
{{- $_ := set $all $name (default (dict) $spec) -}}
{{- end -}}
{{- toYaml $all -}}
{{- end -}}
