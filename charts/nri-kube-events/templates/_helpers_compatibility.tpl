{{/*
Returns a dictionary with legacy runAsUser config.
We know that it only has "one line" but it is separated from the rest of the helpers because it is a temporary things
that we should EOL. The EOL time of this will be marked when we GA the deprecation of Helm v2.
*/}}
{{- define "nri-kube-events.compatibility.securityContext.pod" -}}
{{- if .Values.runAsUser -}}
runAsUser: {{ .Values.runAsUser }}
{{- end -}}
{{- end -}}




{{- /* Functions to fetch integration and agent image configurations from the old .Values.image */ -}}
{{- define "nri-kube-events.compatibility.old.integration.registry" -}}
    {{- if .Values.image -}}
        {{- if .Values.image.kubeEvents -}}
            {{- if .Values.image.kubeEvents.registry -}}
                {{ .Values.image.kubeEvents.registry }}
            {{- end -}}
        {{- end -}}
    {{- end -}}
{{- end -}}

{{- define "nri-kube-events.compatibility.old.integration.repository" -}}
    {{- if .Values.image -}}
        {{- if .Values.image.kubeEvents -}}
            {{- if .Values.image.kubeEvents.repository -}}
                {{ .Values.image.kubeEvents.repository }}
            {{- end -}}
        {{- end -}}
    {{- end -}}
{{- end -}}

{{- define "nri-kube-events.compatibility.old.integration.tag" -}}
    {{- if .Values.image -}}
        {{- if .Values.image.kubeEvents -}}
            {{- if .Values.image.kubeEvents.tag -}}
                {{ .Values.image.kubeEvents.tag }}
            {{- end -}}
        {{- end -}}
    {{- end -}}
{{- end -}}

{{- define "nri-kube-events.compatibility.old.integration.pullPolicy" -}}
    {{- if .Values.image -}}
        {{- if .Values.image.kubeEvents -}}
            {{- if .Values.image.kubeEvents.pullPolicy -}}
                {{ .Values.image.kubeEvents.pullPolicy }}
            {{- end -}}
        {{- end -}}
    {{- end -}}
{{- end -}}

{{- define "nri-kube-events.compatibility.old.agent.registry" -}}
    {{- if .Values.image -}}
        {{- if .Values.image.infraAgent -}}
            {{- if .Values.image.infraAgent.registry -}}
                {{ .Values.image.infraAgent.registry }}
            {{- end -}}
        {{- end -}}
    {{- end -}}
{{- end -}}

{{- define "nri-kube-events.compatibility.old.agent.repository" -}}
    {{- if .Values.image -}}
        {{- if .Values.image.infraAgent -}}
            {{- if .Values.image.infraAgent.repository -}}
                {{ .Values.image.infraAgent.repository }}
            {{- end -}}
        {{- end -}}
    {{- end -}}
{{- end -}}

{{- define "nri-kube-events.compatibility.old.agent.tag" -}}
    {{- if .Values.image -}}
        {{- if .Values.image.infraAgent -}}
            {{- if .Values.image.infraAgent.tag -}}
                {{ .Values.image.infraAgent.tag }}
            {{- end -}}
        {{- end -}}
    {{- end -}}
{{- end -}}

{{- define "nri-kube-events.compatibility.old.agent.pullPolicy" -}}
    {{- if .Values.image -}}
        {{- if .Values.image.infraAgent -}}
            {{- if .Values.image.infraAgent.pullPolicy -}}
                {{ .Values.image.infraAgent.pullPolicy }}
            {{- end -}}
        {{- end -}}
    {{- end -}}
{{- end -}}

{{- /* Functions to fetch integration and agent image configurations from the new .Values.images */ -}}
{{- define "nri-kube-events.compatibility.new.integration.registry" -}}
    {{- if .Values.images -}}
        {{- if .Values.images.integration -}}
            {{- if .Values.images.integration.registry -}}
                {{ .Values.images.integration.registry }}
            {{- end -}}
        {{- end -}}
    {{- end -}}
{{- end -}}

{{- define "nri-kube-events.compatibility.new.integration.repository" -}}
    {{- if .Values.images -}}
        {{- if .Values.images.integration -}}
            {{- if .Values.images.integration.repository -}}
                {{ .Values.images.integration.repository }}
            {{- end -}}
        {{- end -}}
    {{- end -}}
{{- end -}}

{{- define "nri-kube-events.compatibility.new.integration.tag" -}}
    {{- if .Values.images -}}
        {{- if .Values.images.integration -}}
            {{- if .Values.images.integration.tag -}}
                {{ .Values.images.integration.tag }}
            {{- end -}}
        {{- end -}}
    {{- end -}}
{{- end -}}

{{- define "nri-kube-events.compatibility.new.integration.pullPolicy" -}}
    {{- if .Values.images -}}
        {{- if .Values.images.integration -}}
            {{- if .Values.images.integration.pullPolicy -}}
                {{ .Values.images.integration.pullPolicy }}
            {{- end -}}
        {{- end -}}
    {{- end -}}
{{- end -}}

{{- define "nri-kube-events.compatibility.new.agent.registry" -}}
    {{- if .Values.images -}}
        {{- if .Values.images.agent -}}
            {{- if .Values.images.agent.registry -}}
                {{ .Values.images.agent.registry }}
            {{- end -}}
        {{- end -}}
    {{- end -}}
{{- end -}}

{{- define "nri-kube-events.compatibility.new.agent.repository" -}}
    {{- if .Values.images -}}
        {{- if .Values.images.agent -}}
            {{- if .Values.images.agent.repository -}}
                {{ .Values.images.agent.repository }}
            {{- end -}}
        {{- end -}}
    {{- end -}}
{{- end -}}

{{- define "nri-kube-events.compatibility.new.agent.tag" -}}
    {{- if .Values.images -}}
        {{- if .Values.images.agent -}}
            {{- if .Values.images.agent.tag -}}
                {{ .Values.images.agent.tag }}
            {{- end -}}
        {{- end -}}
    {{- end -}}
{{- end -}}

{{- define "nri-kube-events.compatibility.new.agent.pullPolicy" -}}
    {{- if .Values.images -}}
        {{- if .Values.images.agent -}}
            {{- if .Values.images.agent.pullPolicy -}}
                {{ .Values.images.agent.pullPolicy }}
            {{- end -}}
        {{- end -}}
    {{- end -}}
{{- end -}}

{{- /* Functions to fetch image configurations from globals */ -}}
{{- define "nri-kube-events.compatibility.global.registry" -}}
    {{- if .Values.global -}}
        {{- if .Values.global.images -}}
            {{- if .Values.global.images.registry -}}
                {{ .Values.global.images.registry }}
            {{- end -}}
        {{- end -}}
    {{- end -}}
{{- end -}}



{{/*
Creates the image string needed to pull the integration image respecting the breaking change we made in the values file
*/}}
{{- define "nri-kube-events.compatibility.images.integration" -}}
{{- $globalRegistry := include "nri-kube-events.compatibility.global.registry" . -}}
{{- $oldRegistry := include "nri-kube-events.compatibility.old.integration.registry" . -}}
{{- $newRegistry := include "nri-kube-events.compatibility.new.integration.registry" . -}}
{{- $registry := $oldRegistry | default $newRegistry | default $globalRegistry -}}

{{- $oldRepository := include "nri-kube-events.compatibility.old.integration.repository" . -}}
{{- $newRepository := include "nri-kube-events.compatibility.new.integration.repository" . -}}
{{- $repository := $oldRepository | default $newRepository }}

{{- $oldTag := include "nri-kube-events.compatibility.old.integration.tag" . -}}
{{- $newTag := include "nri-kube-events.compatibility.new.integration.tag" . -}}
{{- $tag := $oldTag | default $newTag -}}

{{- if $registry -}}
    {{- printf "%s/%s:%s" $registry $repository $tag -}}
{{- else -}}
    {{- printf "%s:%s" $repository $tag -}}
{{- end -}}
{{- end -}}



{{/*
Creates the image string needed to pull the agent's image respecting the breaking change we made in the values file
*/}}
{{- define "nri-kube-events.compatibility.images.agent" -}}
{{- $globalRegistry := include "nri-kube-events.compatibility.global.registry" . -}}
{{- $oldRegistry := include "nri-kube-events.compatibility.old.agent.registry" . -}}
{{- $newRegistry := include "nri-kube-events.compatibility.new.agent.registry" . -}}
{{- $registry := $oldRegistry | default $newRegistry | default $globalRegistry -}}

{{- $oldRepository := include "nri-kube-events.compatibility.old.agent.repository" . -}}
{{- $newRepository := include "nri-kube-events.compatibility.new.agent.repository" . -}}
{{- $repository := $oldRepository | default $newRepository }}

{{- $oldTag := include "nri-kube-events.compatibility.old.agent.tag" . -}}
{{- $newTag := include "nri-kube-events.compatibility.new.agent.tag" . -}}
{{- $tag := $oldTag | default $newTag -}}

{{- if $registry -}}
    {{- printf "%s/%s:%s" $registry $repository $tag -}}
{{- else -}}
    {{- printf "%s:%s" $repository $tag -}}
{{- end -}}
{{- end -}}



{{/*
Returns the pull policy for the integration image taking into account that we made a breaking change on the values path.
*/}}
{{- define "nri-kube-events.compatibility.images.pullPolicy.integration" -}}
{{- $old := include "nri-kube-events.compatibility.old.integration.pullPolicy" . -}}
{{- $new := include "nri-kube-events.compatibility.new.integration.pullPolicy" . -}}

{{- $old | default $new -}}
{{- end -}}



{{/*
Returns the pull policy for the agent image taking into account that we made a breaking change on the values path.
*/}}
{{- define "nri-kube-events.compatibility.images.pullPolicy.agent" -}}
{{- $old := include "nri-kube-events.compatibility.old.agent.pullPolicy" . -}}
{{- $new := include "nri-kube-events.compatibility.new.agent.pullPolicy" . -}}

{{- $old | default $new -}}
{{- end -}}



{{/*
Returns a merged list of pull secrets ready to be used
*/}}
{{- define "nri-kube-events.compatibility.images.pullSecrets" -}}
{{- $flatlist := list }}

{{- $global := list -}}
{{- if .Values.global -}}
    {{- if .Values.global.images -}}
        {{- if .Values.global.images.pullSecrets -}}
            {{- $global = .Values.global.images.pullSecrets -}}
        {{- end -}}
    {{- end -}}
{{- end -}}

{{- $old := list -}}
{{- if .Values.image -}}
    {{- if .Values.image.pullSecrets -}}
        {{- $old = .Values.image.pullSecrets }}
    {{- end -}}
{{- end -}}

{{- $new := list -}}
{{- if .Values.images -}}
    {{- if .Values.images.pullSecrets -}}
        {{- $new = .Values.images.pullSecrets -}}
    {{- end -}}
{{- end -}}

{{- range $global -}}
    {{- $flatlist = append $flatlist . -}}
{{- end -}}
{{- range $old -}}
    {{- $flatlist = append $flatlist . -}}
{{- end -}}
{{- range $new -}}
    {{- $flatlist = append $flatlist . -}}
{{- end -}}

{{ toYaml $flatlist }}
{{- end -}}



{{- /* Messege to show to the user saying that image value is not supported anymore */ -}}
{{- define "nri-kube-events.compatibility.message.images" -}}
{{- /* workaround: https://github.com/helm/helm/issues/9266 */ -}}
{{- $values := (.Values | merge (dict)) -}}

{{- $oldIntegrationRegistry := include "nri-kube-events.compatibility.old.integration.registry" . -}}
{{- $oldIntegrationRepository := include "nri-kube-events.compatibility.old.integration.repository" . -}}
{{- $oldIntegrationTag := include "nri-kube-events.compatibility.old.integration.tag" . -}}
{{- $oldIntegrationPullPolicy := include "nri-kube-events.compatibility.old.integration.pullPolicy" . -}}
{{- $oldAgentRegistry := include "nri-kube-events.compatibility.old.agent.registry" . -}}
{{- $oldAgentRepository := include "nri-kube-events.compatibility.old.agent.repository" . -}}
{{- $oldAgentTag := include "nri-kube-events.compatibility.old.agent.tag" . -}}
{{- $oldAgentPullPolicy := include "nri-kube-events.compatibility.old.agent.pullPolicy" . -}}

{{- if or $oldIntegrationRegistry $oldIntegrationRepository $oldIntegrationTag $oldIntegrationPullPolicy
          $oldAgentRegistry $oldAgentRepository $oldAgentTag $oldAgentPullPolicy }}
Configuring image repository an tag under 'image' is no longer supported.
This is the list values that we no longer support:
 - image.kubeEvents.registry
 - image.kubeEvents.repository
 - image.kubeEvents.tag
 - image.kubeEvents.pullPolicy
 - image.infraAgent.registry
 - image.infraAgent.repository
 - image.infraAgent.tag
 - image.infraAgent.pullPolicy

Please set:
 - images.agent.* to configure the infrastructure-agent forwarder.
 - images.integration.* to configure the image in charge of scraping k8s data.

------
{{- end }}
{{- end -}}



{{- /* Messege to show to the user saying that image value is not supported anymore */ -}}
{{- define "nri-kube-events.compatibility.message.securityContext.runAsUser" -}}
{{- if .Values.runAsUser }}
WARNING: `runAsUser` is deprecated
==================================

We have automatically translated your `runAsUser` setting to the new format, but this shimming will be removed in the
future. Please migrate your configs to the new format in the `securityContext` key.
{{- end }}
{{- end -}}
