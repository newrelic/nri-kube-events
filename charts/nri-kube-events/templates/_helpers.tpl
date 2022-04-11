{{/* vim: set filetype=mustache: */}}

{{- define "nri-kube-events.securityContext.pod" -}}
{{- if include "newrelic.common.securityContext.pod" . -}}
{{- include "newrelic.common.securityContext.pod" . -}}
{{- else if include "newrelic.compatibility.securityContext" . -}}
{{- include "newrelic.compatibility.securityContext" . -}}
runAsNonRoot: true
{{- else -}}
runAsUser: 1000
runAsNonRoot: true
{{- end -}}
{{- end -}}

{{- define "nri-kube-events.securityContext.container" -}}
{{- if include "newrelic.common.securityContext.container" . -}}
{{- include "newrelic.common.securityContext.container" . -}}
{{- else -}}
privileged: false
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
{{- end -}}
{{- end -}}
