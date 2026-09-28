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

{{/*
  A random secret value that survives upgrades: reuse the key from the
  live Secret when it exists (lookup returns nothing under
  `helm template` / --dry-run, which then just renders a fresh value).
  Usage: include "kubehero.persistentRandom" (dict "root" . "secret" "name" "key" "k" "length" 32)
*/}}
{{- define "kubehero.persistentRandom" -}}
{{- $existing := lookup "v1" "Secret" .root.Release.Namespace .secret -}}
{{- if and $existing $existing.data (hasKey $existing.data .key) -}}
{{- index $existing.data .key | b64dec -}}
{{- else -}}
{{- randAlphaNum (int .length) -}}
{{- end -}}
{{- end -}}

{{/* Secret holding the control plane's generated credentials. */}}
{{- define "kubehero.controlPlaneSecret" -}}
{{- .Values.controlPlane.existingSecret | default (printf "%s-control-plane" (include "kubehero.fullname" .)) -}}
{{- end -}}

{{/* Secret + key the collector / operator authenticate with. */}}
{{- define "kubehero.clusterTokenSecret" -}}
{{- if .Values.cluster.tokenSecret -}}
{{- .Values.cluster.tokenSecret -}}
{{- else -}}
{{- include "kubehero.controlPlaneSecret" . -}}
{{- end -}}
{{- end -}}
{{- define "kubehero.clusterTokenKey" -}}
{{- if .Values.cluster.tokenSecret -}}token{{- else -}}cluster-token{{- end -}}
{{- end -}}

{{/* Base URL of the control plane every in-cluster client talks to. */}}
{{- define "kubehero.controlPlaneURL" -}}
{{- if .Values.controlPlane.url -}}
{{- .Values.controlPlane.url -}}
{{- else -}}
{{- printf "http://%s-control-plane.%s.svc:%v" (include "kubehero.fullname" .) .Release.Namespace .Values.controlPlane.service.port -}}
{{- end -}}
{{- end -}}

{{/* Postgres DSN source: (secret name, key). */}}
{{- define "kubehero.postgresSecret" -}}
{{- if .Values.postgresql.embedded -}}
{{- printf "%s-postgres" (include "kubehero.fullname" .) -}}
{{- else -}}
{{- .Values.postgresql.external.existingSecret -}}
{{- end -}}
{{- end -}}
{{- define "kubehero.postgresSecretKey" -}}
{{- if .Values.postgresql.embedded -}}url{{- else -}}{{ .Values.postgresql.external.existingSecretKey | default "url" }}{{- end -}}
{{- end -}}

{{/* ClickHouse DSN source: (secret name, key). */}}
{{- define "kubehero.clickhouseSecret" -}}
{{- if .Values.clickhouse.embedded -}}
{{- printf "%s-clickhouse" (include "kubehero.fullname" .) -}}
{{- else -}}
{{- .Values.clickhouse.external.existingSecret -}}
{{- end -}}
{{- end -}}
{{- define "kubehero.clickhouseSecretKey" -}}
{{- if .Values.clickhouse.embedded -}}url{{- else -}}{{ .Values.clickhouse.external.existingSecretKey | default "url" }}{{- end -}}
{{- end -}}

{{/* Restricted container securityContext shared by stateless components. */}}
{{- define "kubehero.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
runAsNonRoot: true
capabilities:
  drop: ["ALL"]
seccompProfile:
  type: RuntimeDefault
{{- end -}}
