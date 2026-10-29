{{/* 应用名：固定为 chart 名 newtg。 */}}
{{- define "newtg.name" -}}
{{- .Chart.Name -}}
{{- end -}}

{{/* 命名空间：由 values.namespace 决定，所有资源都显式写 metadata.namespace。 */}}
{{- define "newtg.namespace" -}}
{{- .Values.namespace -}}
{{- end -}}

{{/* 镜像引用：dict "root" $ "repo" "newtg/gateway"；registry 为空时回退到裸 repo。 */}}
{{- define "newtg.image" -}}
{{- $reg := .root.Values.image.registry -}}
{{- $tag := .root.Values.image.tag -}}
{{- if $reg -}}
{{- printf "%s/%s:%s" $reg .repo $tag -}}
{{- else -}}
{{- printf "%s:%s" .repo $tag -}}
{{- end -}}
{{- end -}}

{{/* 工作负载标签：app.kubernetes.io/name 必须等于角色名，NetworkPolicy 按它选 Pod。 */}}
{{- define "newtg.labels" -}}
app.kubernetes.io/name: {{ .role }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/part-of: {{ include "newtg.name" .root }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
{{- end -}}
