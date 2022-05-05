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



{{/*
Creates the image string needed to pull the integration image respecting the breaking change we made in the values file
*/}}
{{- define "nri-kube-events.compatibility.images.integration" -}}
{{- /* workaround: https://github.com/helm/helm/issues/9266 */ -}}
{{- $values := (.Values | merge (dict)) -}}

{{- $oldRegistry := dig "image" "kubeEvents" "registry" "" $values }}
{{- $newRegistry := dig "images" "integration" "registry" "" $values }}
{{- $registry := $oldRegistry | default $newRegistry }}

{{- $oldRepository := dig "image" "kubeEvents" "repository" "" $values }}
{{- $newRepository := dig "images" "integration" "repository" "" $values }}
{{- $repository := $oldRepository | default $newRepository }}

{{- $oldTag := dig "image" "kubeEvents" "tag" "" $values }}
{{- $newTag := dig "images" "integration" "tag" "" $values }}
{{- $tag := $oldTag | default $newTag }}

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
{{- /* workaround: https://github.com/helm/helm/issues/9266 */ -}}
{{- $values := (.Values | merge (dict)) -}}

{{- $oldRegistry := dig "image" "infraAgent" "registry" "" $values -}}
{{- $newRegistry := dig "images" "agent" "registry" "" $values -}}
{{- $registry := $oldRegistry | default $newRegistry -}}

{{- $oldRepository := dig "image" "infraAgent" "repository" "" $values -}}
{{- $newRepository := dig "images" "agent" "repository" "" $values -}}
{{- $repository := $oldRepository | default $newRepository -}}

{{- $oldTag := dig "image" "infraAgent" "tag" "" $values -}}
{{- $newTag := dig "images" "agent" "tag" "" $values -}}
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
{{- /* workaround: https://github.com/helm/helm/issues/9266 */ -}}
{{- $values := (.Values | merge (dict)) -}}

{{- $oldPullPolicy := dig "image" "kubeEvents" "pullPolicy" "" $values -}}
{{- $newPullPolicy := dig "images" "integration" "pullPolicy" "" $values -}}

{{- $oldPullPolicy | default $newPullPolicy -}}
{{- end -}}



{{/*
Returns the pull policy for the agent image taking into account that we made a breaking change on the values path.
*/}}
{{- define "nri-kube-events.compatibility.images.pullPolicy.agent" -}}
{{- /* workaround: https://github.com/helm/helm/issues/9266 */ -}}
{{- $values := (.Values | merge (dict)) -}}

{{- $oldPullPolicy := dig "image" "infraAgent" "pullPolicy" "" $values -}}
{{- $newPullPolicy := dig "images" "agent" "pullPolicy" "" $values -}}

{{- $oldPullPolicy | default $newPullPolicy -}}
{{- end -}}



{{- /* Messege to show to the user saying that image value is not supported anymore */ -}}
{{- define "nri-kube-events.compatibility.message.images" -}}
{{- /* workaround: https://github.com/helm/helm/issues/9266 */ -}}
{{- $values := (.Values | merge (dict)) -}}

{{- $oldIntegrationRegistry := dig "image" "kubeEvents" "registry" "" $values }}
{{- $oldIntegrationRepository := dig "image" "kubeEvents" "repository" "" $values }}
{{- $oldIntegrationTag := dig "image" "kubeEvents" "tag" "" $values }}
{{- $oldIntegrationPullPolicy := dig "image" "kubeEvents" "pullPolicy" "" $values -}}
{{- $oldAgentRegistry := dig "image" "infraAgent" "registry" "" $values -}}
{{- $oldAgentRepository := dig "image" "infraAgent" "repository" "" $values -}}
{{- $oldAgentTag := dig "image" "infraAgent" "tag" "" $values -}}
{{- $oldAgentPullPolicy := dig "image" "infraAgent" "pullPolicy" "" $values -}}

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
