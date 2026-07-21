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
 * In-cluster addresses
 * ============================================================ */}}

{{- define "durupages.controllerAddr" -}}
{{ include "durupages.controller.name" . }}.{{ .Release.Namespace }}.svc.{{ .Values.clusterDomain }}:{{ .Values.controller.service.port }}
{{- end -}}

{{- define "durupages.hubBundleAddr" -}}
{{ include "durupages.hub.name" . }}.{{ .Release.Namespace }}.svc.{{ .Values.clusterDomain }}:{{ .Values.hub.service.httpPort }}
{{- end -}}

{{- define "durupages.hubLogAddr" -}}
{{ include "durupages.hub.name" . }}.{{ .Release.Namespace }}.svc.{{ .Values.clusterDomain }}:{{ .Values.hub.service.grpcPort }}
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
