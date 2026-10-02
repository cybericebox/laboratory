{{- define "laboratory.proxyPodTemplate" -}}
metadata:
  labels:
    app: laboratory-proxy-l7
    {{- include "laboratory.selectorLabels" . | nindent 4 }}
spec:
  serviceAccountName: laboratory-proxy
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
    imagePullPolicy: {{ .Values.proxy.image.pullPolicy }}
    command: ["/proxy", "proxy-l7"]
    env:
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
    - name: REPORT_INTERVAL
      value: {{ .Values.proxy.l7.reportInterval | quote }}
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
    imagePullPolicy: {{ .Values.proxy.image.pullPolicy }}
    command: ["/proxy", "proxy-wg"]
    env:
    - name: UDP_LISTEN_ADDR
      value: {{ printf ":%d" (.Values.proxy.wg.listenPort | int) | quote }}
    - name: VPN_SERVICE_PORT
      value: {{ .Values.operator.vpnServicePort | quote }}
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
