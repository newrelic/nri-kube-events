{{/*
Returns a dictionary with legacy runAsUser config
*/}}
{{- define "newrelic.compatibility.securityContext" -}}
{{- if  .Values.runAsUser -}}
{{ dict "runAsUser" .Values.runAsUser | toYaml }}
{{- end -}}
{{- end -}}
