{{/* ============================================================ *
 * Naming
 * ============================================================ */}}

{{- define "durupages.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "durupages.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "durupages.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Per-component resource names. */}}
{{- define "durupages.controller.name" -}}{{ include "durupages.fullname" . }}-controller{{- end -}}
{{- define "durupages.router.name" -}}{{ include "durupages.fullname" . }}-router{{- end -}}
{{- define "durupages.hub.name" -}}{{ include "durupages.fullname" . }}-hub{{- end -}}

{{/* ============================================================ *
 * Labels
 * ============================================================ */}}

{{- define "durupages.labels" -}}
helm.sh/chart: {{ include "durupages.chart" . }}
{{ include "durupages.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "durupages.selectorLabels" -}}
app.kubernetes.io/name: {{ include "durupages.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Component labels: $ctx is a dict {root, component}. */}}
{{- define "durupages.componentLabels" -}}
{{ include "durupages.labels" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "durupages.componentSelectorLabels" -}}
{{ include "durupages.selectorLabels" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/* ============================================================ *
 * Advertised addresses
 * ============================================================ *
 * These are the addresses handed to *other* processes -- the router, and the
 * worker pods the controller creates at runtime. They default to the in-cluster
 * Service FQDN, which is what a normal cluster wants.
 *
 * Every one of them can be overridden (controller.advertiseAddr,
 * hub.advertiseAddr, hub.logAdvertiseAddr) because the Service FQDN is not
 * always reachable: an isolated worker network may only route to an ingress or
 * a fixed VIP, and a certificate SAN may name a domain that has nothing to do
 * with the Service name. */}}

{{/* Host part of an address: scheme, port and path stripped. Used to add the
     advertised hostname to a certificate's SANs. */}}
{{- define "durupages.hostOf" -}}
{{- $s := . -}}
{{- if contains "://" $s -}}{{- $s = (splitList "://" $s | last) -}}{{- end -}}
{{- $s = (splitList "/" $s | first) -}}
{{- regexReplaceAll ":[0-9]+$" $s "" -}}
{{- end -}}

{{/* gRPC dial target: bare host:port, never a URL. A scheme here is rejected
     rather than stripped, because silently accepting one would hide the real
     mistake until a worker failed to dial. */}}
{{- define "durupages.controllerAddr" -}}
{{- $v := .Values.controller.advertiseAddr -}}
{{- if $v -}}
{{- if contains "://" $v -}}
{{- fail (printf "\n\ndurupages: controller.advertiseAddr must be a bare host:port, not a URL.\nIt is a gRPC dial target, so a scheme makes the dial fail.\n  got:      %s\n  expected: controller.example.com:%v\n" $v .Values.controller.service.port) -}}
{{- end -}}
{{- $v -}}
{{- else -}}
{{ include "durupages.controller.name" . }}.{{ .Release.Namespace }}.svc.{{ .Values.clusterDomain }}:{{ .Values.controller.service.port }}
{{- end -}}
{{- end -}}

{{/* The bundle address is used as an HTTP URL prefix by the worker shim, so it
     MUST carry a scheme. Without one, net/url parses the hostname itself as the
     scheme and every bundle download fails with "unsupported protocol scheme"
     -- an outage this chart has already caused once, hence the hard failure on
     a scheme-less override.
     The generated address follows tls.enabled: https when the hub serves TLS. */}}
{{- define "durupages.hubBundleAddr" -}}
{{- $v := .Values.hub.advertiseAddr -}}
{{- if $v -}}
{{- if not (or (hasPrefix "http://" $v) (hasPrefix "https://" $v)) -}}
{{- fail (printf "\n\ndurupages: hub.advertiseAddr MUST include a URL scheme.\nThe worker shim uses it as an HTTP URL prefix; without a scheme every bundle\ndownload fails with \"unsupported protocol scheme\".\n  got:      %s\n  expected: https://%s  (or http:// when the hub serves plaintext)\n" $v $v) -}}
{{- end -}}
{{- if and .Values.tls.enabled (hasPrefix "http://" $v) -}}
{{- fail (printf "\n\ndurupages: tls.enabled=true but hub.advertiseAddr is http://.\nThe hub serves TLS, so workers dialling it in plaintext would fail. Use\nhttps:// (or turn tls.enabled off).\n  got: %s\n" $v) -}}
{{- end -}}
{{- $v -}}
{{- else -}}
{{ ternary "https" "http" .Values.tls.enabled }}://{{ include "durupages.hub.name" . }}.{{ .Release.Namespace }}.svc.{{ .Values.clusterDomain }}:{{ .Values.hub.service.httpPort }}
{{- end -}}
{{- end -}}

{{- define "durupages.hubLogAddr" -}}
{{- $v := .Values.hub.logAdvertiseAddr -}}
{{- if $v -}}
{{- if contains "://" $v -}}
{{- fail (printf "\n\ndurupages: hub.logAdvertiseAddr must be a bare host:port, not a URL.\nIt is a gRPC dial target, so a scheme makes the dial fail.\n  got:      %s\n  expected: hub-logs.example.com:%v\n" $v .Values.hub.service.grpcPort) -}}
{{- end -}}
{{- $v -}}
{{- else -}}
{{ include "durupages.hub.name" . }}.{{ .Release.Namespace }}.svc.{{ .Values.clusterDomain }}:{{ .Values.hub.service.grpcPort }}
{{- end -}}
{{- end -}}

{{/* ============================================================ *
 * Worker pod overrides
 * ============================================================ *
 * The chart cannot configure worker pods directly -- the controller creates
 * them at runtime -- so worker.podOverrides is carried in a ConfigMap the
 * controller reads at startup instead. Name, key, mount path and content live
 * here so the ConfigMap and the controller Deployment that mounts and
 * checksums it cannot disagree.
 *
 * The payload is worker.podOverrides dumped as-is: its shape is defined and
 * validated on the Go side (controller.WorkerPodOverrides), not here, so this
 * chart does not need to track that allowlist. */}}

{{- define "durupages.workerPodOverridesName" -}}
{{ include "durupages.fullname" . }}-worker-pod-overrides
{{- end -}}

{{- define "durupages.workerPodOverridesKey" -}}
pod-overrides.yaml
{{- end -}}

{{- define "durupages.workerPodOverridesDir" -}}
/etc/durupages/worker-pod-overrides
{{- end -}}

{{- define "durupages.workerPodOverridesFile" -}}
{{ include "durupages.workerPodOverridesDir" . }}/{{ include "durupages.workerPodOverridesKey" . }}
{{- end -}}

{{/* The ConfigMap payload. Also hashed into the controller's pod template: the
     checksum has to cover the CONTENT, since the ConfigMap's name never
     changes and hashing that would never trigger a restart. */}}
{{- define "durupages.workerPodOverridesYAML" -}}
{{ toYaml .Values.worker.podOverrides }}
{{- end -}}

{{/* ============================================================ *
 * Services
 * ============================================================ */}}

{{/* Renders the Service spec fields that are the same for every component --
     everything except `ports` and `selector`, which each component supplies
     itself. Context is a dict {service}.

     Fields are emitted only when set, so the rendered Service stays minimal and
     the API server's own defaults apply. Booleans and numbers are tested for
     nil rather than truthiness: `allocateLoadBalancerNodePorts: false` and
     `publishNotReadyAddresses: false` are meaningful settings, and `with` would
     drop them.

     `extraSpec` is the escape hatch for anything Kubernetes adds later or this
     chart does not model; it is merged last and wins. */}}
{{- define "durupages.service.spec" -}}
{{- $s := .service -}}
type: {{ $s.type | default "ClusterIP" }}
{{- with $s.clusterIP }}
clusterIP: {{ . | quote }}
{{- end }}
{{- with $s.externalIPs }}
externalIPs:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $s.externalName }}
externalName: {{ . | quote }}
{{- end }}
{{- with $s.loadBalancerIP }}
loadBalancerIP: {{ . | quote }}
{{- end }}
{{- with $s.loadBalancerClass }}
loadBalancerClass: {{ . | quote }}
{{- end }}
{{- with $s.loadBalancerSourceRanges }}
loadBalancerSourceRanges:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- if not (kindIs "invalid" $s.allocateLoadBalancerNodePorts) }}
allocateLoadBalancerNodePorts: {{ $s.allocateLoadBalancerNodePorts }}
{{- end }}
{{- with $s.externalTrafficPolicy }}
externalTrafficPolicy: {{ . | quote }}
{{- end }}
{{- with $s.internalTrafficPolicy }}
internalTrafficPolicy: {{ . | quote }}
{{- end }}
{{- if not (kindIs "invalid" $s.healthCheckNodePort) }}
healthCheckNodePort: {{ $s.healthCheckNodePort }}
{{- end }}
{{- with $s.sessionAffinity }}
sessionAffinity: {{ . | quote }}
{{- end }}
{{- with $s.sessionAffinityConfig }}
sessionAffinityConfig:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $s.ipFamilies }}
ipFamilies:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $s.ipFamilyPolicy }}
ipFamilyPolicy: {{ . | quote }}
{{- end }}
{{- if not (kindIs "invalid" $s.publishNotReadyAddresses) }}
publishNotReadyAddresses: {{ $s.publishNotReadyAddresses }}
{{- end }}
{{- with $s.trafficDistribution }}
trafficDistribution: {{ . | quote }}
{{- end }}
{{- with $s.extraSpec }}
{{- toYaml . | nindent 0 }}
{{- end }}
{{- end -}}

{{/* ============================================================ *
 * Image references
 * ============================================================ */}}

{{/* Renders one image reference from an image block {repository, tag, digest}.
     Context is a dict {root, image}.

     A digest wins over a tag when both are set. A tag is a moving pointer --
     the registry can repoint it, and two nodes pulling "the same" tag can end
     up on different bytes -- while a digest names the exact image and cannot
     drift. Anyone who bothered to pin one meant it, so it is not treated as a
     fallback for a missing tag.

     Empty tag falls back to .Chart.AppVersion, as before. */}}
{{- define "durupages.image" -}}
{{- $image := .image -}}
{{- with $image.digest -}}
{{- if not (regexMatch "^[a-z0-9]+:[a-fA-F0-9]{32,}$" .) -}}
{{- fail (printf "\n\ndurupages: image digest %q is not a digest.\nExpected <algorithm>:<hex>, which is what `docker buildx imagetools inspect`\nand the registry report:\n  digest: sha256:9f5b1c...  (64 hex characters)\nTo pin by tag instead, leave digest empty and set tag.\n" .) -}}
{{- end -}}
{{- printf "%s@%s" $image.repository . -}}
{{- else -}}
{{- printf "%s:%s" $image.repository (default .root.Chart.AppVersion $image.tag) -}}
{{- end -}}
{{- end -}}

{{/* ============================================================ *
 * Secret names
 * ============================================================ */}}

{{- define "durupages.controllerServiceAccountName" -}}
{{ include "durupages.controller.name" . }}
{{- end -}}

{{- define "durupages.workerJwtSecretName" -}}
{{- if .Values.workerJwt.existingSecret -}}
{{ .Values.workerJwt.existingSecret }}
{{- else -}}
{{ include "durupages.fullname" . }}-worker-jwt
{{- end -}}
{{- end -}}

{{- define "durupages.postgresSecretName" -}}
{{- if .Values.postgres.existingSecret -}}
{{ .Values.postgres.existingSecret }}
{{- else -}}
{{ include "durupages.fullname" . }}-postgres
{{- end -}}
{{- end -}}

{{- define "durupages.postgresSecretKey" -}}
{{- if .Values.postgres.existingSecret -}}
{{ .Values.postgres.existingSecretKey }}
{{- else -}}
dsn
{{- end -}}
{{- end -}}

{{- define "durupages.s3SecretName" -}}
{{- if .Values.s3.existingSecret -}}
{{ .Values.s3.existingSecret }}
{{- else -}}
{{ include "durupages.fullname" . }}-s3
{{- end -}}
{{- end -}}

{{/* True when S3 credentials should be injected via secretKeyRef. */}}
{{- define "durupages.s3HasCreds" -}}
{{- if or .Values.s3.existingSecret (and .Values.s3.accessKey .Values.s3.secretKey) -}}true{{- end -}}
{{- end -}}

{{/* ============================================================ *
 * Shared env blocks
 * ============================================================ */}}

{{/* S3 storage env. Context is root (.). */}}
{{- define "durupages.s3Env" -}}
- name: DURUPAGES_S3_ENDPOINT
  value: {{ .Values.s3.endpoint | quote }}
- name: DURUPAGES_S3_REGION
  value: {{ .Values.s3.region | quote }}
- name: DURUPAGES_S3_BUCKET
  value: {{ .Values.s3.bucket | quote }}
- name: DURUPAGES_S3_PATH_STYLE
  value: {{ .Values.s3.pathStyle | quote }}
{{- if include "durupages.s3HasCreds" . }}
- name: DURUPAGES_S3_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "durupages.s3SecretName" . }}
      key: {{ .Values.s3.accessKeySecretKey }}
- name: DURUPAGES_S3_SECRET_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "durupages.s3SecretName" . }}
      key: {{ .Values.s3.secretKeySecretKey }}
{{- end }}
{{- end -}}

{{/* imagePullSecrets block. Context is root (.). */}}
{{- define "durupages.imagePullSecrets" -}}
{{- with .Values.imagePullSecrets }}
imagePullSecrets:
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* ============================================================ *
 * TLS
 * ============================================================ *
 * Transport security is opt-in. With tls.enabled=false nothing below renders
 * and every hop stays plaintext, which is the pre-TLS behaviour verbatim.
 *
 * Certificates reach a listener in one of two ways:
 *   existingSecret : a kubernetes.io/tls Secret the operator created.
 *   cert-manager   : a Certificate this chart creates, one per listener.
 * existingSecret always wins for its listener, so the two can be mixed. */}}

{{/* Mount points. Certificates are always mounted under a stable filename
     (tls.crt / tls.key / ca.crt) via `items`, so a Secret using different keys
     needs no change anywhere but its own *Key value. */}}
{{- define "durupages.tls.certDir" -}}/etc/durupages/tls{{- end -}}
{{- define "durupages.tls.logCertDir" -}}/etc/durupages/tls-log{{- end -}}
{{- define "durupages.tls.adminCertDir" -}}/etc/durupages/tls-admin{{- end -}}
{{- define "durupages.tls.caDir" -}}/etc/durupages/ca{{- end -}}
{{- define "durupages.tls.caFile" -}}{{ include "durupages.tls.caDir" . }}/ca.crt{{- end -}}

{{- define "durupages.tls.controllerSecretName" -}}
{{- if .Values.tls.controller.existingSecret -}}
{{ .Values.tls.controller.existingSecret }}
{{- else -}}
{{ include "durupages.fullname" . }}-controller-tls
{{- end -}}
{{- end -}}

{{- define "durupages.tls.hubSecretName" -}}
{{- if .Values.tls.hub.existingSecret -}}
{{ .Values.tls.hub.existingSecret }}
{{- else -}}
{{ include "durupages.fullname" . }}-hub-tls
{{- end -}}
{{- end -}}

{{/* The hub log listener and the controller admin API reuse the sibling
     listener's certificate unless the operator asked for their own -- which is
     what giving them a Secret, a domain or a common name means. Nothing else
     distinguishes "wants a separate certificate" from "fine with the default",
     and an extra toggle would only be another thing to forget to set. */}}
{{- define "durupages.tls.hubLogSeparate" -}}
{{- $c := .Values.tls.hubLog -}}
{{- if or $c.existingSecret $c.dnsNames $c.ipAddresses $c.commonName -}}true{{- end -}}
{{- end -}}

{{- define "durupages.tls.hubLogSecretName" -}}
{{- if .Values.tls.hubLog.existingSecret -}}
{{ .Values.tls.hubLog.existingSecret }}
{{- else -}}
{{ include "durupages.fullname" . }}-hub-log-tls
{{- end -}}
{{- end -}}

{{- define "durupages.tls.adminSeparate" -}}
{{- $c := .Values.tls.admin -}}
{{- if or $c.existingSecret $c.dnsNames $c.ipAddresses $c.commonName -}}true{{- end -}}
{{- end -}}

{{- define "durupages.tls.adminSecretName" -}}
{{- if .Values.tls.admin.existingSecret -}}
{{ .Values.tls.admin.existingSecret }}
{{- else -}}
{{ include "durupages.fullname" . }}-admin-tls
{{- end -}}
{{- end -}}

{{/* CA bundle used by clients (router, and worker pods via the controller).
     An explicit caSecret wins; otherwise a cert-manager issued Secret carries
     the issuing CA in ca.crt, which is the common self-signed / private-PKI
     case. Public (ACME) certificates have no ca.crt -- for those set
     tls.caSecret.systemRoots=true and let clients use the system trust store. */}}
{{- define "durupages.tls.caEnabled" -}}
{{- if and .Values.tls.enabled (not .Values.tls.caSecret.systemRoots) -}}
{{- if or .Values.tls.caSecret.name .Values.tls.certManager.enabled -}}true{{- end -}}
{{- end -}}
{{- end -}}

{{- define "durupages.tls.caSecretName" -}}
{{- if .Values.tls.caSecret.name -}}
{{ .Values.tls.caSecret.name }}
{{- else -}}
{{ include "durupages.tls.controllerSecretName" . }}
{{- end -}}
{{- end -}}

{{- define "durupages.tls.caSecretKey" -}}
{{ .Values.tls.caSecret.key | default "ca.crt" }}
{{- end -}}

{{/* Fail early on the combinations that would install something broken:
     a listener with no certificate at all, cert-manager with no issuer, or
     clients with no way to verify anybody. Rendered from controller.yaml so it
     runs on every template/install. */}}
{{- define "durupages.tls.validate" -}}
{{- if and .Values.tls.certManager.enabled (not .Values.tls.enabled) -}}
{{- fail "\n\ndurupages: tls.certManager.enabled=true but tls.enabled=false.\nNothing would use the issued certificates. Set tls.enabled=true.\n" -}}
{{- end -}}
{{- if .Values.tls.enabled -}}
{{- if .Values.tls.certManager.enabled -}}
{{- if not .Values.tls.certManager.issuerRef.name -}}
{{- fail "\n\ndurupages: tls.certManager.enabled=true requires tls.certManager.issuerRef.name.\n\n  --set tls.certManager.issuerRef.name=my-issuer \\\n  --set tls.certManager.issuerRef.kind=ClusterIssuer\n" -}}
{{- end -}}
{{- else -}}
{{- $missing := list -}}
{{- if not .Values.tls.controller.existingSecret -}}{{- $missing = append $missing "tls.controller.existingSecret" -}}{{- end -}}
{{- if not .Values.tls.hub.existingSecret -}}{{- $missing = append $missing "tls.hub.existingSecret" -}}{{- end -}}
{{- if $missing -}}
{{- fail (printf "\n\ndurupages: tls.enabled=true but no certificate source for: %s\nEither point each listener at a kubernetes.io/tls Secret you created, or turn\non cert-manager issuance:\n\n  --set tls.certManager.enabled=true --set tls.certManager.issuerRef.name=my-issuer\n" (join ", " $missing)) -}}
{{- end -}}
{{- end -}}
{{/* The derived CA comes out of the controller's issued Secret, so it only
     exists when cert-manager wrote that Secret. Pointing the derivation at an
     operator-supplied Secret would mount a key that probably is not there and
     hang the pod on a missing secret key. */}}
{{- if and .Values.tls.certManager.enabled .Values.tls.controller.existingSecret (not .Values.tls.caSecret.name) (not .Values.tls.caSecret.systemRoots) -}}
{{- fail "\n\ndurupages: the CA bundle cannot be derived here.\ntls.controller.existingSecret is set, so the controller certificate is yours\nand the chart cannot assume it carries a ca.crt. Name the CA explicitly:\n\n  --set tls.caSecret.name=my-ca-secret --set tls.caSecret.key=ca.crt\n\nor use the system trust store with --set tls.caSecret.systemRoots=true\n" -}}
{{- end -}}
{{- if not (include "durupages.tls.caEnabled" .) -}}
{{- if not .Values.tls.caSecret.systemRoots -}}
{{- fail "\n\ndurupages: tls.enabled=true but no CA bundle is configured.\nThe router and the worker pods have to verify the servers they dial. Either\n\n  --set tls.caSecret.name=my-ca-secret   (key: tls.caSecret.key, default ca.crt)\n\nor, when the certificates are signed by a publicly trusted CA,\n\n  --set tls.caSecret.systemRoots=true    (verify against the system trust store)\n" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Volume carrying a server key pair, normalised to tls.crt / tls.key.
     Context: dict {name, secretName, certKey, keyKey}. */}}
{{- define "durupages.tls.certVolume" -}}
- name: {{ .name }}
  secret:
    secretName: {{ .secretName }}
    items:
      - key: {{ .certKey }}
        path: tls.crt
      - key: {{ .keyKey }}
        path: tls.key
{{- end -}}

{{/* Volume carrying the CA bundle, normalised to ca.crt. Context is root. */}}
{{- define "durupages.tls.caVolume" -}}
- name: ca
  secret:
    secretName: {{ include "durupages.tls.caSecretName" . }}
    items:
      - key: {{ include "durupages.tls.caSecretKey" . }}
        path: ca.crt
{{- end -}}

{{/* Client-side TLS env shared by the router and by the controller (which
     restates it to the worker pods it creates). Only "true" is emitted: an
     absent variable means plaintext, so a chart with TLS off adds nothing.
     Context is root. */}}
{{- define "durupages.tls.clientEnv" -}}
{{- if .Values.tls.enabled -}}
- name: DURUPAGES_CONTROLLER_TLS
  value: "true"
{{- if .Values.logIngest.enabled }}
- name: DURUPAGES_HUB_LOG_TLS
  value: "true"
{{- end }}
{{- with .Values.tls.controller.serverName }}
- name: DURUPAGES_CONTROLLER_SERVER_NAME
  value: {{ . | quote }}
{{- end }}
{{- with .Values.tls.hub.serverName }}
- name: DURUPAGES_HUB_SERVER_NAME
  value: {{ . | quote }}
{{- end }}
{{- if .Values.logIngest.enabled }}
{{- with .Values.tls.hubLog.serverName }}
- name: DURUPAGES_HUB_LOG_SERVER_NAME
  value: {{ . | quote }}
{{- end }}
{{- end }}
{{- end }}
{{- end -}}
