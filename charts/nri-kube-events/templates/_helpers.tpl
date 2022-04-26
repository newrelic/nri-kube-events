{{/* vim: set filetype=mustache: */}}

{{- define "nri-kube-events.securityContext.pod" -}}
{{- $defaults := fromYaml ( include "nriKubernetes.securityContext.podDefaults" . ) -}}
{{- $compatibilityLayer := include "newrelic.compatibility.securityContext" . | fromYaml -}}
{{- $commonLibrary := fromYaml ( include "newrelic.common.securityContext.pod" . ) -}}

{{- $finalSecurityContext := dict -}}
{{- if $commonLibrary -}}
    {{- $finalSecurityContext = mustMergeOverwrite $commonLibrary $compatibilityLayer  -}}
{{- else -}}
    {{- $finalSecurityContext = mustMergeOverwrite $defaults $compatibilityLayer  -}}
{{- end -}}
{{- toYaml $finalSecurityContext -}}
{{- end -}}

{{- define "nriKubernetes.securityContext.podDefaults" -}}
runAsUser: 1000
runAsNonRoot: true
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
