{{/* Full resource name. */}}
{{- define "kubehero.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (.Values.nameOverride | default .Chart.Name) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "kubehero.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{- define "kubehero.image" -}}
{{- $reg := default .root.Values.image.registry .image.registry -}}
{{- $repo := default .root.Values.image.repository .image.repository -}}
{{- $tag := .image.tag | default .root.Values.image.tag | default .root.Chart.AppVersion -}}
{{- if $reg -}}{{ $reg }}/{{ end }}{{ $repo }}/{{ .image.name }}:{{ $tag }}
{{- end -}}

{{/*
  Resolve the priorityClassName a pod should use.

    1. .Values.priorityClassName explicit override wins.
    2. Otherwise, if priorityClass.create is true, use the chart-created PC.
    3. Otherwise, empty string → omit priorityClassName from the pod spec.

  Usage: {{ include "kubehero.priorityClassName" . }}
*/}}
{{- define "kubehero.priorityClassName" -}}
{{- if .Values.priorityClassName -}}
{{- .Values.priorityClassName -}}
{{- else if .Values.priorityClass.create -}}
{{- .Values.priorityClass.name | default (printf "%s-control-plane" (include "kubehero.fullname" .)) -}}
{{- end -}}
{{- end -}}

{{/* Prefer-not-co-located soft anti-affinity for HA components */}}
{{- define "kubehero.antiAffinity" -}}
podAntiAffinity:
  preferredDuringSchedulingIgnoredDuringExecution:
    - weight: 100
      podAffinityTerm:
        labelSelector:
          matchExpressions:
            - key: app.kubernetes.io/component
              operator: In
              values:
                - {{ .component }}
        topologyKey: kubernetes.io/hostname
    - weight: 50
      podAffinityTerm:
        labelSelector:
          matchExpressions:
            - key: app.kubernetes.io/component
              operator: In
              values:
                - {{ .component }}
        topologyKey: topology.kubernetes.io/zone
{{- end -}}
