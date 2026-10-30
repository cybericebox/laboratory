{{- define "laboratory.proxyPodTemplate" -}}
metadata:
  labels:
    app: laboratory-proxy-l7
    {{- include "laboratory.selectorLabels" . | nindent 4 }}
spec:
  serviceAccountName: laboratory-proxy
  priorityClassName: {{ include "laboratory.priorityClass.platform" . }}
  # After SIGTERM the proxy stops admission, closes tracked HTTP/WebSocket work and saves the settled counters; the preStop sleep lets
  # the Gateway and the Service stop sending connections to the pod first.
  terminationGracePeriodSeconds: {{ .Values.proxy.terminationGracePeriodSeconds }}
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  {{- if eq .Values.proxy.mode "replicas" }}
  affinity:
    podAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
      - labelSelector:
          matchLabels:
            app: laboratory-proxy-l7
        topologyKey: kubernetes.io/hostname
  {{- end }}
  {{- with include "laboratory.placement" (dict "own" .Values.proxy "root" $ "daemon" (eq .Values.proxy.mode "daemonset")) }}
  {{- . | nindent 2 }}
  {{- end }}
  {{- with .Values.imagePullSecrets }}
  imagePullSecrets:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  containers:
  {{- if .Values.proxy.l7.enabled }}
  - name: l7
    image: {{ include "laboratory.imageRef" (dict "image" .Values.proxy.image "root" $) | quote }}
    imagePullPolicy: {{ include "laboratory.pullPolicy" (dict "policy" .Values.proxy.image.pullPolicy "tag" (include "laboratory.tag" (dict "image" .Values.proxy.image "root" $))) }}
    command: ["/proxy", "proxy-l7"]
    securityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities:
        drop: [ ALL ]
    env:
    {{- /* The listen address (:8443), the probe port (:8081) and the TLS files (/etc/proxy/tls) are baked into the image. */}}
    - name: BASE_DOMAIN
      value: {{ required "operator.baseDomain is required" .Values.operator.baseDomain | quote }}
  {{- with .Values.proxy.l7.proxyProtocol.trustedFrom }}
    # The peers (the edge nodes) whose connections may begin with a PROXY protocol header; any other peer's header is ignored.
    - name: L7_PROXY_PROTOCOL_FROM
      value: {{ join "," . | quote }}
  {{- end }}
    - name: SESSION_COOKIE_NAME
      value: {{ .Values.proxy.l7.sessionCookieName | quote }}
    - name: SESSION_SECRET
      valueFrom:
        secretKeyRef:
          name: {{ .Values.proxy.l7.sessionSecret.name | quote }}
          key: {{ .Values.proxy.l7.sessionSecret.key | quote }}
    - name: ACCESS_TOKEN_MAX_TTL
      value: {{ .Values.proxy.l7.accessTokenMaxTTL | quote }}
    - name: SESSION_IDLE_TTL
      value: {{ .Values.proxy.l7.sessionIdleTTL | quote }}
    - name: SESSION_RENEW_BEFORE
      value: {{ .Values.proxy.l7.sessionRenewBefore | quote }}
    - name: SESSION_MAX_TTL
      value: {{ .Values.proxy.l7.sessionMaxTTL | quote }}
    - name: LIVE_MAX_LIFETIME
      value: {{ .Values.proxy.l7.liveMaxLifetime | quote }}
    - name: LIVE_CHECK_INTERVAL
      value: {{ .Values.proxy.l7.liveCheckInterval | quote }}
    - name: ERROR_JOURNAL_NAMESPACE
      value: {{ .Release.Namespace | quote }}
    - name: READ_HEADER_TIMEOUT
      value: {{ .Values.proxy.l7.readHeaderTimeout | quote }}
    - name: READ_TIMEOUT
      value: {{ .Values.proxy.l7.readTimeout | quote }}
    - name: IDLE_TIMEOUT
      value: {{ .Values.proxy.l7.idleTimeout | quote }}
    - name: MAX_HEADER_BYTES
      value: {{ .Values.proxy.l7.maxHeaderBytes | quote }}
    - name: REPORT_INTERVAL
      value: {{ .Values.proxy.l7.reportInterval | quote }}
    - name: MAX_CONNECTIONS
      value: {{ .Values.proxy.l7.maxConnections | quote }}
    - name: LIVE_PER_CLIENT
      value: {{ .Values.proxy.l7.livePerClient | quote }}
    - name: LIVE_PER_GROUP
      value: {{ .Values.proxy.l7.livePerGroup | quote }}
    - name: LIVE_TOTAL
      value: {{ .Values.proxy.l7.liveTotal | quote }}
    - name: AUTH_RATE
      value: {{ .Values.proxy.l7.authRate | quote }}
    - name: AUTH_BURST
      value: {{ .Values.proxy.l7.authBurst | quote }}
    - name: POD_NAME
      valueFrom:
        fieldRef:
          fieldPath: metadata.name
    {{- if .Values.bandwidth.enabled }}
    - name: NODE_NAME
      valueFrom:
        fieldRef:
          fieldPath: spec.nodeName
    {{- end }}
    {{- if .Values.proxy.kindMode }}
    - name: KUBERNETES_SERVICE_HOST
      value: "127.0.0.1"
    - name: KUBERNETES_SERVICE_PORT
      value: "6443"
    {{- end }}
    ports:
    - name: https
      containerPort: {{ include "laboratory.l7Port" . }}
    - name: health
      containerPort: {{ include "laboratory.l7HealthPort" . }}
    # Ready means the required caches have synced, prior counters are restored and the HTTPS listener is bound.
    startupProbe:
      httpGet:
        path: /readyz
        port: health
      periodSeconds: 2
      failureThreshold: 60
    readinessProbe:
      httpGet:
        path: /readyz
        port: health
      periodSeconds: 5
      failureThreshold: 2
      timeoutSeconds: 3
    livenessProbe:
      httpGet:
        path: /healthz
        port: health
      periodSeconds: 20
      failureThreshold: 3
      timeoutSeconds: 5
    lifecycle:
      preStop:
        sleep:
          seconds: {{ .Values.proxy.preStopSleepSeconds }}
    volumeMounts:
    - name: tls
      mountPath: /etc/proxy/tls
      readOnly: true
    {{- if .Values.bandwidth.enabled }}
    - name: bandwidth
      mountPath: /etc/bandwidth
      readOnly: true
    {{- end }}
    resources:
      {{- toYaml .Values.proxy.l7.resources | nindent 6 }}
  {{- end }}
  {{- if .Values.proxy.l7.enabled }}
  volumes:
  - name: tls
    secret:
      secretName: proxy-tls
  {{- if .Values.bandwidth.enabled }}
  # The bandwidth limits of every node, written by the operator; the proxy reads the key of its own node. Optional: the pod starts
  # before the operator has made the ConfigMap, and has no limit until it exists.
  - name: bandwidth
    configMap:
      name: laboratory-bandwidth
      optional: true
  {{- end }}
  {{- end }}
{{- end }}
