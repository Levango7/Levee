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

{{/*
  是否要求 PostgreSQL 数据面：三个来源任一被填写即算（外部 Secret / 明文 dsn / 内嵌实例）。

  刻意**不看 mode**。#47 之后单节点 PostgreSQL 是合法形态：CLI 与 serve 经
  `database.driver` + `database.dsn` 取同一个库，而 cluster 模式仍走 `--pg-dsn`
  那条路（serve 在没有 --cluster 时拒绝 --pg-dsn）。两条门都在，Chart 只按"操作者
  有没有要求 PG"来决定要不要建 Secret、要不要注入环境变量。
*/}}
{{- define "levee.wantsPostgres" -}}
{{- if or .Values.postgres.enabled .Values.postgres.dsn .Values.postgres.externalSecretName -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{/*
  渲染后的 config.yaml 里 database.driver 的实际值，分两个 helper：

  `levee.dbDriverRaw` 区分"没写"和"写了 sqlite"——这是能不能替操作者补默认值的分界：
  显式写了 driver: sqlite 又启用 PostgreSQL 是一份自相矛盾，Chart 不许悄悄改它，
  交给二进制的配置校验大声拒绝（database.dsn is only used when driver = postgres）。

  `levee.dbDriver` 是最终生效值：没写驱动时，"要求 PostgreSQL"就意味着 postgres
  （#47 之后单节点 PG 合法：serve/CLI 按 database.driver + database.dsn 取同一个库），
  否则按二进制自己的默认值 sqlite。

  刻意从 values.config 读而不是另设一个 chart 私有旗标：多一个"数据库类型"的 value
  就多一份可能与 config.yaml 相反的说法。
*/}}
{{- define "levee.dbDriverRaw" -}}
{{- $cfg := .Values.config | default dict -}}
{{- if hasKey $cfg "database" -}}
{{- $db := get $cfg "database" | default dict -}}
{{- if hasKey $db "driver" -}}
{{- get $db "driver" | toString -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "levee.dbDriver" -}}
{{- $raw := include "levee.dbDriverRaw" . -}}
{{- if $raw -}}
{{- $raw -}}
{{- else if and (ne .Values.mode "cluster") (eq (include "levee.wantsPostgres" .) "true") -}}
{{- "postgres" -}}
{{- else -}}
{{- "sqlite" -}}
{{- end -}}
{{- end -}}

{{/*
  容器真正会读到的 config.yaml，一份来源。

  ConfigMap 的内容与 rollout 的 checksum/config 必须来自这里同一次计算：checksum 若仍按
  原始 values.config 求哈希，那么"Chart 补写了 database.driver"这件事不会改变哈希，
  已运行的 Pod 就不会因它而滚动重启——同一份配置在两个地方说法不同，正是本仓反复踩到的
  "同一个值多处判定"。
*/}}
{{- define "levee.effectiveConfig" -}}
{{- $cfg := deepCopy (.Values.config | default dict) -}}
{{- if and (not (include "levee.dbDriverRaw" .)) (eq (include "levee.dbDriver" .) "postgres") -}}
{{- $db := get $cfg "database" | default dict -}}
{{- $_ := set $cfg "database" (merge (dict "driver" "postgres") $db) -}}
{{- end -}}
{{- toYaml $cfg -}}
{{- end -}}
