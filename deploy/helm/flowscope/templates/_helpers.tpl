{{/*
Expand the name of the chart.
*/}}
{{- define "flowscope.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully-qualified app name. <release>-<chart-name>, truncated to 63
chars to satisfy DNS-1123 label constraints.
*/}}
{{- define "flowscope.fullname" -}}
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
Common labels stamped on every resource the chart emits.
*/}}
{{- define "flowscope.labels" -}}
app.kubernetes.io/name: {{ include "flowscope.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Per-component selector labels — keep stable across upgrades, never
include version/chart since those rotate.
*/}}
{{- define "flowscope.selectorLabels" -}}
app.kubernetes.io/name: {{ include "flowscope.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Resolve the init image. Tag falls back to the global image.tag when
not explicitly set.
*/}}
{{- define "flowscope.initImage" -}}
{{- $repo := .Values.init.image.repository -}}
{{- $tag := default .Values.image.tag .Values.init.image.tag -}}
{{- if .Values.global.imageRegistry -}}
{{ printf "%s/%s:%s" .Values.global.imageRegistry $repo $tag }}
{{- else -}}
{{ printf "%s:%s" $repo $tag }}
{{- end -}}
{{- end }}

{{/*
The init container spec. Reused by api and ingest deployments so they
both block on migrations + retention TTL before serving traffic.
*/}}
{{- define "flowscope.initContainer" -}}
- name: init
  image: {{ include "flowscope.initImage" . }}
  imagePullPolicy: {{ .Values.global.imagePullPolicy | default "IfNotPresent" }}
  env:
    - name: FLOWSCOPE_CLICKHOUSE_DSN
      value: {{ .Values.global.clickhouseDSN | quote }}
    - name: FLOWSCOPE_LOG_LEVEL
      value: {{ .Values.global.logLevel | default "info" | quote }}
  resources:
    {{- toYaml .Values.init.resources | nindent 4 }}
{{- end }}

{{/*
serviceAccountName picks the SA the consumer pods use. When
secrets.backend=kv we always need a dedicated SA (the federated token
projection is annotated on it). Otherwise we honour the explicit
serviceAccount.create / serviceAccount.name knob, and fall back to
the cluster default.
*/}}
{{- define "flowscope.serviceAccountName" -}}
{{- if or (eq .Values.secrets.backend "kv") .Values.serviceAccount.create -}}
{{ default (printf "%s-%s" (include "flowscope.fullname" .) "snmp-consumer") .Values.serviceAccount.name }}
{{- else -}}
default
{{- end -}}
{{- end }}

{{/*
Env entries for FLOWSCOPE_SNMP_KEY_REF on consumer pods (snmp, alert,
api). The ref string is identical across backends; only the
underlying delivery mechanism differs. For backend=env we also wire
the legacy FLOWSCOPE_SNMP_KEY var from a Secret so the env: ref has
something to point at.
*/}}
{{- define "flowscope.snmpKeyEnv" -}}
- name: FLOWSCOPE_SNMP_KEY_REF
  value: {{ .Values.secrets.snmpMasterRef | quote }}
{{- if eq .Values.secrets.backend "env" }}
- name: FLOWSCOPE_SNMP_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.env.secretName | quote }}
      key: {{ .Values.secrets.env.secretKey | quote }}
{{- end }}
{{- end }}

{{/*
Optional volume + mount for backend=file. The Secret is projected
read-only into the consumer pod at .secrets.file.path. The Deployment
points FLOWSCOPE_SNMP_KEY_REF at the same path.
*/}}
{{- define "flowscope.snmpKeyVolume" -}}
{{- if eq .Values.secrets.backend "file" }}
- name: snmp-master
  secret:
    secretName: {{ .Values.secrets.file.secretName | quote }}
    items:
      - key: {{ .Values.secrets.file.secretKey | quote }}
        path: snmp_master
    defaultMode: 0400
{{- end }}
{{- end }}

{{- define "flowscope.snmpKeyVolumeMount" -}}
{{- if eq .Values.secrets.backend "file" }}
- name: snmp-master
  mountPath: {{ dir .Values.secrets.file.path | quote }}
  readOnly: true
{{- end }}
{{- end }}
