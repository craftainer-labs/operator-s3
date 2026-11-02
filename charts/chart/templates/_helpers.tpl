{{/*
Expand the name of the chart.
*/}}
{{- define "operator-s3.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "operator-s3.fullname" -}}
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
Namespace for generated references.
Always uses the Helm release namespace.
*/}}
{{- define "operator-s3.namespaceName" -}}
{{- .Release.Namespace }}
{{- end }}

{{/*
Resource name with proper truncation for Kubernetes 63-character limit.
Takes a dict with:
  - .suffix: Resource name suffix (e.g., "metrics", "webhook")
  - .context: Template context (root context with .Values, .Release, etc.)
Dynamically calculates safe truncation to ensure total name length <= 63 chars.
*/}}
{{- define "operator-s3.resourceName" -}}
{{- $fullname := include "operator-s3.fullname" .context }}
{{- $suffix := .suffix }}
{{- $maxLen := sub 62 (len $suffix) | int }}
{{- if gt (len $fullname) $maxLen }}
{{- printf "%s-%s" (trunc $maxLen $fullname | trimSuffix "-") $suffix | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" $fullname $suffix | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
ServiceAccount name to use.
When enabled, use the chart's ServiceAccount name.
When disabled, serviceAccount.name must be set; use "default" to pick the namespace default ServiceAccount.
*/}}
{{- define "operator-s3.serviceAccountName" -}}
{{- if .Values.serviceAccount.enabled }}
{{- include "operator-s3.resourceName" (dict "suffix" "controller-manager" "context" .) }}
{{- else }}
{{- required "serviceAccount.name is required when serviceAccount.enabled=false (set name: default explicitly to use the namespace default ServiceAccount)" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The manager image: <registry>/<repository>@<digest>, or :<tag> (default
Chart.appVersion) without a digest. global.imageRegistry overrides the registry,
for mirrors in air-gapped clusters.
*/}}
{{- define "operator-s3.image" -}}
{{- $image := .Values.manager.image -}}
{{- $registry := default $image.registry (default "" (.Values.global | default dict).imageRegistry) -}}
{{- $repository := printf "%s/%s" $registry $image.repository | trimPrefix "/" -}}
{{- if $image.digest -}}
{{- printf "%s@%s" $repository $image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repository (default .Chart.AppVersion $image.tag) -}}
{{- end -}}
{{- end }}
