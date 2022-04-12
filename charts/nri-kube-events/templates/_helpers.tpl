{{/* vim: set filetype=mustache: */}}

{{/*
Return the licenseKey
*/}}
{{- define "nri-kube-events.licenseKey" -}}
{{- if .Values.global}}
  {{- if .Values.global.licenseKey }}
      {{- .Values.global.licenseKey -}}
  {{- else -}}
      {{- .Values.licenseKey | default "" -}}
  {{- end -}}
{{- else -}}
    {{- .Values.licenseKey | default "" -}}
{{- end -}}
{{- end -}}

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
Return the customSecretLicenseKey
*/}}
{{- define "nri-kube-events.customSecretLicenseKey" -}}
{{- if .Values.global }}
  {{- if .Values.global.customSecretLicenseKey }}
      {{- .Values.global.customSecretLicenseKey -}}
  {{- else -}}
      {{- .Values.customSecretLicenseKey | default "" -}}
  {{- end -}}
{{- else -}}
    {{- .Values.customSecretLicenseKey | default "" -}}
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
{{- $licenseKey := include "nri-kube-events.licenseKey" . -}}
{{- $customSecretName := include "nri-kube-events.customSecretName" . -}}
{{- $customSecretLicenseKey := include "nri-kube-events.customSecretLicenseKey" . -}}
{{- and (or $licenseKey (and $customSecretName $customSecretLicenseKey)) $cluster}}
{{- end -}}

{{/*
Returns Custom Attributes even if formatted as a json string
*/}}
{{- define "newrelic.customAttributes" -}}
{{- if kindOf .Values.customAttributes | eq "string" -}}
{{  .Values.customAttributes }}
{{- else -}}
{{ .Values.customAttributes | toJson }}
{{- end -}}
{{- end -}}
