{{/* vim: set filetype=mustache: */}}

{{/*
Return the customSecretName
*/}}
{{- define "nri-kube-events.customSecretName" -}}
{{- if .Values.global }}
  {{- if .Values.global.customSecretName }}
      {{- .Values.global.customSecretName -}}
  {{- else -}}
      {{- .Values.customSecretName | default "" -}}
  {{- end -}}
{{- else -}}
    {{- .Values.customSecretName | default "" -}}
{{- end -}}
{{- end -}}

{{/*
Returns nrStaging
*/}}
{{- define "newrelic.nrStaging" -}}
{{- if .Values.global }}
  {{- if .Values.global.nrStaging }}
    {{- .Values.global.nrStaging -}}
  {{- end -}}
{{- else if .Values.nrStaging }}
  {{- .Values.nrStaging -}}
{{- end -}}
{{- end -}}

{{/*
Returns if the template should render, it checks if the required values
licenseKey and cluster are set.
*/}}
{{- define "nri-kube-events.areValuesValid" -}}
{{- $cluster := include "common.cluster" . -}}
{{- $licenseKey := include "common.license._licenseKey" . -}}
{{- $customSecretName := include "common.license._customSecretName" . -}}
{{- $customSecretLicenseKey := include "common.license._customSecretKey" . -}}
{{- and (or $licenseKey (and $customSecretName $customSecretLicenseKey)) $cluster}}
{{- end -}}

