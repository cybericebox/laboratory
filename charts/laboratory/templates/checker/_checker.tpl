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
    - name: CRI_SOCK
      value: {{ .Values.nodeAgent.criSocket | quote }}
    - name: CONTAINERD_STATE
      value: {{ .Values.checks.containerdState | quote }}
    - name: CONTAINERD_NAMESPACE
      value: {{ .Values.checks.containerdNamespace | quote }}
    - name: CHECKER_MAX_CONCURRENT
      value: {{ .Values.checks.maxConcurrent | quote }}
    - name: CHECKER_RUN_DIR
      value: /run/checker
  securityContext:
    # Not privileged, no host namespace, nothing writable. The one capability reads files the participant made unreadable (a
    # 0600 file of a user the container maps to another host user); it grants no write, no mount, no ptrace, no network.
    privileged: false
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: true
    runAsUser: 0
    capabilities:
      drop: [ ALL ]
      add: [ DAC_READ_SEARCH ]
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
    - name: cri-sock
      mountPath: {{ .Values.nodeAgent.criSocket | quote }}
    # The containerd state directory at the same path as on the host, read-only. HostToContainer: the root file system of a container is
    # mounted by the runtime on the host after this pod started, and must show up here.
    - name: containerd-state
      mountPath: {{ .Values.checks.containerdState | quote }}
      readOnly: true
      mountPropagation: HostToContainer
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
- name: cri-sock
  hostPath:
    path: {{ .Values.nodeAgent.criSocket | quote }}
    type: Socket
- name: containerd-state
  hostPath:
    path: {{ .Values.checks.containerdState | quote }}
    # No type check on purpose: a wrong path must reach the checker's own start-up check (which says so on its Lease) and not leave
    # the pod waiting for a volume.
    type: ""
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
