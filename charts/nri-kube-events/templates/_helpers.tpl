{{/* vim: set filetype=mustache: */}}

{{/*
Returns if the template should render, it checks if the required values
licenseKey and cluster are set.
*/}}
{{- define "nri-kube-events.areValuesValid" -}}
{{- $cluster := include "newrelic.common.cluster" . -}}
{{- $licenseKey := include "newrelic.common.license._licenseKey" . -}}
{{- $customSecretName := include "newrelic.common.license._customSecretName" . -}}
{{- $customSecretLicenseKey := include "newrelic.common.license._customSecretKey" . -}}
{{- and (or $licenseKey (and $customSecretName $customSecretLicenseKey)) $cluster}}
{{- end -}}

