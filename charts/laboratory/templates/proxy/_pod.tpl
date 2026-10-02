{{- define "laboratory.proxyPodTemplate" -}}
metadata:
  labels:
    app: laboratory-proxy-l7
    {{- include "laboratory.selectorLabels" . | nindent 4 }}
spec:
  serviceAccountName: laboratory-proxy
  priorityClassName: {{ .Values.priorityClasses.platform.name }}
  # After SIGTERM the HTTP server stops taking connections and lets requests in flight finish (up to this long); the preStop sleep lets
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
  {{- with .Values.proxy.nodeSelector }}
  nodeSelector:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.proxy.tolerations }}
  tolerations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.imagePullSecrets }}
  imagePullSecrets:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  containers:
  {{- if .Values.proxy.l7.enabled }}
  - name: l7
    image: "{{ .Values.proxy.image.repository }}:{{ .Values.proxy.image.tag | default .Chart.AppVersion }}"
    imagePullPolicy: {{ include "laboratory.pullPolicy" (dict "policy" .Values.proxy.image.pullPolicy "tag" (.Values.proxy.image.tag | default .Chart.AppVersion)) }}
    command: ["/proxy", "proxy-l7"]
    securityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities:
        drop: [ ALL ]
    env:
    - name: HEALTH_ADDR
      value: {{ printf ":%d" (.Values.proxy.l7.healthPort | int) | quote }}
    - name: BASE_DOMAIN
      value: {{ required "operator.baseDomain is required" .Values.operator.baseDomain | quote }}
    - name: LISTEN_HTTPS
      value: {{ .Values.proxy.l7.listen | quote }}
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
    - name: TLS_CERT_PATH
      value: /etc/proxy/tls/tls.crt
    - name: TLS_KEY_PATH
      value: /etc/proxy/tls/tls.key
    {{- if .Values.proxy.kindMode }}
    - name: KUBERNETES_SERVICE_HOST
      value: "127.0.0.1"
    - name: KUBERNETES_SERVICE_PORT
      value: "6443"
    {{- end }}
    ports:
    - name: https
      containerPort: {{ trimPrefix ":" .Values.proxy.l7.listen | int }}
    - name: health
      containerPort: {{ .Values.proxy.l7.healthPort }}
    # Ready means it serves: the HTTPS listener is bound and the informer caches have synced (see DEPLOY.md, "Probes").
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
    resources:
      {{- toYaml .Values.proxy.l7.resources | nindent 6 }}
  {{- end }}
  {{- if .Values.proxy.wg.enabled }}
  - name: wg-demux
    image: "{{ .Values.proxy.image.repository }}:{{ .Values.proxy.image.tag | default .Chart.AppVersion }}"
    imagePullPolicy: {{ include "laboratory.pullPolicy" (dict "policy" .Values.proxy.image.pullPolicy "tag" (.Values.proxy.image.tag | default .Chart.AppVersion)) }}
    command: ["/proxy", "proxy-wg"]
    securityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities:
        drop: [ ALL ]
    env:
    - name: HEALTH_ADDR
      value: {{ printf ":%d" (.Values.proxy.wg.healthPort | int) | quote }}
    - name: UDP_LISTEN_ADDR
      value: {{ printf ":%d" (.Values.proxy.wg.listenPort | int) | quote }}
    - name: VPN_SERVICE_PORT
      value: {{ .Values.operator.vpnServicePort | quote }}
    - name: ERROR_JOURNAL_NAMESPACE
      value: {{ .Release.Namespace | quote }}
    - name: DEMUX_MAX_ENTRIES
      value: {{ .Values.proxy.wg.limits.maxEntries | quote }}
    - name: DEMUX_PARTIAL_TTL
      value: {{ .Values.proxy.wg.limits.partialTTL | quote }}
    - name: DEMUX_MISS_RATE
      value: {{ .Values.proxy.wg.limits.missRate | quote }}
    - name: DEMUX_MISS_BURST
      value: {{ .Values.proxy.wg.limits.missBurst | quote }}
    - name: DEMUX_ROAM_INTERVAL
      value: {{ .Values.proxy.wg.limits.roamInterval | quote }}
    - name: DEMUX_GLOBAL_HANDSHAKE_RATE
      value: {{ .Values.proxy.wg.limits.globalHandshakeRate | quote }}
    - name: DEMUX_GLOBAL_HANDSHAKE_BURST
      value: {{ .Values.proxy.wg.limits.globalHandshakeBurst | quote }}
    - name: DEMUX_SESSION_RATE
      value: {{ .Values.proxy.wg.limits.sessionRate | quote }}
    - name: DEMUX_SESSION_BURST
      value: {{ .Values.proxy.wg.limits.sessionBurst | quote }}
    - name: DEMUX_READERS
      value: {{ .Values.proxy.wg.limits.readers | quote }}
    {{- if .Values.proxy.kindMode }}
    - name: KUBERNETES_SERVICE_HOST
      value: "127.0.0.1"
    - name: KUBERNETES_SERVICE_PORT
      value: "6443"
    {{- end }}
    ports:
    - name: wg
      containerPort: {{ .Values.proxy.wg.listenPort }}
      protocol: UDP
    - name: health
      containerPort: {{ .Values.proxy.wg.healthPort }}
    # Ready means it serves: the UDP socket is bound, the caches have synced and every group is in the demux table (see DEPLOY.md, "Probes").
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
    resources:
      {{- toYaml .Values.proxy.wg.resources | nindent 6 }}
  {{- end }}
  {{- if .Values.proxy.l7.enabled }}
  volumes:
  - name: tls
    secret:
      secretName: proxy-tls
  {{- end }}
{{- end }}
