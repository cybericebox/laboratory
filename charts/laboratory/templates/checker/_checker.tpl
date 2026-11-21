{{- /*
The pieces of the checker DaemonSet. The DaemonSet (daemonset.yaml) is only a frame: a pod is the list of containers and the list of
volumes, and each is one named template, so that a second container (the terminal part, built later) is added by one `include` in each
list and one template of its own, without rewriting this one. The pod-level settings (account, placement, security) are shared.
*/ -}}

{{- /* The checker container. */ -}}
{{- define "laboratory.checker.container" -}}
- name: checker
  image: {{ include "laboratory.imageRef" (dict "image" .Values.nodeAgent.image "root" $) | quote }}
  imagePullPolicy: {{ include "laboratory.pullPolicy" (dict "policy" .Values.nodeAgent.image.pullPolicy "tag" (include "laboratory.tag" (dict "image" .Values.nodeAgent.image "root" $))) }}
  command: [ "/node", "checker" ]
  env:
    - name: NODE_NAME
      valueFrom:
        fieldRef:
          fieldPath: spec.nodeName
    - name: POD_NAME
      valueFrom:
        fieldRef:
          fieldPath: metadata.name
    - name: POD_NAMESPACE
      valueFrom:
        fieldRef:
          fieldPath: metadata.namespace
    - name: CHECKER_VERSION
      value: {{ include "laboratory.tag" (dict "image" .Values.nodeAgent.image "root" $) | quote }}
    # The one way to a device: the checker socket of the device access service of this node.
    - name: DEVICE_ACCESS_SOCKET
      value: {{ printf "%s/checker.sock" (trimSuffix "/" .Values.checks.socketDir) | quote }}
    - name: CHECKER_MAX_CONCURRENT
      value: {{ .Values.checks.maxConcurrent | quote }}
    - name: CHECKER_MAX_TIME_LIMIT_SECONDS
      value: {{ .Values.checks.maxTimeLimitSeconds | quote }}
    - name: CHECKER_RUN_DIR
      value: /run/checker
  securityContext:
    # A plain user with nothing: not privileged, every capability dropped, no escalation, nothing writable, no host namespace. The checker
    # reads no file of a container and holds no runtime socket (the device access service does the exec for it), so it needs neither a
    # root user nor a capability.
    privileged: false
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: true
    runAsNonRoot: true
    runAsUser: {{ .Values.checks.socketUid }}
    runAsGroup: {{ .Values.checks.socketGid }}
    capabilities:
      drop: [ ALL ]
  # No port, no probe endpoint: the kubelet runs a command that looks at two files the checker keeps.
  startupProbe:
    exec:
      command: [ "/node", "checker-health", "ready" ]
    periodSeconds: 3
    failureThreshold: 60
  readinessProbe:
    exec:
      command: [ "/node", "checker-health", "ready" ]
    periodSeconds: 15
  livenessProbe:
    exec:
      command: [ "/node", "checker-health", "live" ]
    periodSeconds: 30
    failureThreshold: 3
    timeoutSeconds: 5
  volumeMounts:
    # The directory of the checker socket, read-only: connecting to a socket needs no write on the mount, and the checker can make nothing in it.
    - name: device-socket
      mountPath: {{ .Values.checks.socketDir | quote }}
      readOnly: true
    - name: run
      mountPath: /run/checker
    - name: tmp
      mountPath: /tmp
  {{- with .Values.checks.resources }}
  resources:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end }}

{{- /* Containers after the checker: none today. The terminal part is added here. */ -}}
{{- define "laboratory.checker.extraContainers" -}}{{- end }}

{{- /* The volumes of the checker container. */ -}}
{{- define "laboratory.checker.volumes" -}}
- name: device-socket
  hostPath:
    path: {{ .Values.checks.socketDir | quote }}
    # Made by the kubelet when the checker starts first; the device access service puts its socket in it.
    type: DirectoryOrCreate
- name: run
  emptyDir:
    medium: Memory
    sizeLimit: 1Mi
- name: tmp
  emptyDir:
    sizeLimit: 8Mi
{{- end }}

{{- /* Volumes of the containers after the checker: none today. */ -}}
{{- define "laboratory.checker.extraVolumes" -}}{{- end }}

{{- define "laboratory.checker.selectorLabels" -}}
app: checker
{{- end }}
