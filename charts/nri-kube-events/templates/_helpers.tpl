{{/* vim: set filetype=mustache: */}}

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

