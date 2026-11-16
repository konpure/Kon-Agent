{{/* 常规命名 helpers（helm 标准模式） */}}

{{- define "kon-agent-stack.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kon-agent-stack.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/* 选择器标签（Deployment/StatefulSet selector 必须逐字匹配，不可含版本等易变字段） */}}
{{- define "kon-agent-stack.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kon-agent-stack.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: kon-agent-stack
{{- end -}}

{{/* 全量标签 */}}
{{- define "kon-agent-stack.labels" -}}
{{ include "kon-agent-stack.selectorLabels" . }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/* 组件名：fullname-<component>，用法 (dict "context" $ "component" "victoria-metrics") */}}
{{- define "kon-agent-stack.componentName" -}}
{{- include "kon-agent-stack.fullname" .context -}}-{{ .component }}
{{- end -}}

{{/* 组件级标签，用法同上 */}}
{{- define "kon-agent-stack.componentLabels" -}}
{{ include "kon-agent-stack.selectorLabels" .context }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/*
镜像拼接：global.imageRegistry 存在时输出 <registry>/<repo>:<tag>，否则 <repo>:<tag>。
用法：include "kon-agent-stack.image" (dict "values" .Values.server "context" $)
*/}}
{{- define "kon-agent-stack.image" -}}
{{- $v := .values -}}
{{- $tag := $v.image.tag | default .context.Chart.AppVersion -}}
{{- if .context.Values.global.imageRegistry -}}
{{- printf "%s/%s:%s" .context.Values.global.imageRegistry $v.image.repository $tag -}}
{{- else -}}
{{- printf "%s:%s" $v.image.repository $tag -}}
{{- end -}}
{{- end -}}

{{/* imagePullSecrets（global.imagePullSecrets 为空则不输出） */}}
{{- define "kon-agent-stack.pullSecrets" -}}
{{- with .context.Values.global.imagePullSecrets }}
imagePullSecrets:
{{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}

{{/*
VictoriaMetrics 集群内地址，server(-vm)/vmalert(datasource/remoteWrite)/grafana(数据源) 共用，
保证换 release 名不破。
*/}}
{{- define "kon-agent-stack.vmUrl" -}}
{{- printf "http://%s-victoria-metrics:%v" (include "kon-agent-stack.fullname" .context) .context.Values.victoriaMetrics.service.port -}}
{{- end -}}

{{/* Grafana 管理员凭证来源：existingSecret 优先，否则 chart 内建 Secret */}}
{{- define "kon-agent-stack.grafanaAdminSecretName" -}}
{{- if .Values.grafana.existingSecret -}}
{{- .Values.grafana.existingSecret -}}
{{- else -}}
{{- include "kon-agent-stack.fullname" . -}}-grafana
{{- end -}}
{{- end -}}
