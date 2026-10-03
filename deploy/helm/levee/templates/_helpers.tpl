{{/* 公共名称与标签助手 */}}
{{- define "levee.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "levee.fullname" -}}
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

{{- define "levee.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "levee.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "levee.selectorLabels" -}}
app.kubernetes.io/name: {{ include "levee.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* 凭据 Secret 名称：existingSecret 优先 */}}
{{- define "levee.secretName" -}}
{{- if .Values.auth.existingSecret -}}
{{- .Values.auth.existingSecret -}}
{{- else -}}
{{- printf "%s-auth" (include "levee.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/* PG DSN Secret 名称：外部 existingSecret 优先，否则内嵌（外部 dsn 或无内嵌时由模板创建） */}}
{{- define "levee.pgSecretName" -}}
{{- if .Values.postgres.externalSecretName -}}
{{- .Values.postgres.externalSecretName -}}
{{- else -}}
{{- printf "%s-pg" (include "levee.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/* 内嵌 PG 的实际地址（cluster 且未给外部 dsn 且启用内嵌时） */}}
{{- define "levee.pgHost" -}}
{{- if .Values.postgres.enabled -}}
{{- printf "%s-postgres" (include "levee.fullname" .) -}}
{{- end -}}
{{- end -}}
