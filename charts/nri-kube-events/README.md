# nri-kube-events

![Version: 3.16.2](https://img.shields.io/badge/Version-3.16.2-informational?style=flat-square) ![AppVersion: 2.16.2](https://img.shields.io/badge/AppVersion-2.16.2-informational?style=flat-square)

A Helm chart to deploy the New Relic Kube Events router

**Homepage:** <https://docs.newrelic.com/docs/integrations/kubernetes-integration/kubernetes-events/install-kubernetes-events-integration>

# Helm installation

You can install this chart using [`nri-bundle`](https://github.com/newrelic/helm-charts/tree/master/charts/nri-bundle) located in the
[helm-charts repository](https://github.com/newrelic/helm-charts) or directly from this repository by adding this Helm repository:

```shell
helm repo add nri-kube-events https://newrelic.github.io/nri-kube-events
helm upgrade --install nri-kube-events/nri-kube-events -f your-custom-values.yaml
```

## Source Code

* <https://github.com/newrelic/nri-kube-events/>
* <https://github.com/newrelic/nri-kube-events/tree/main/charts/nri-kube-events>
* <https://github.com/newrelic/infrastructure-agent/>

## Values managed globally

This chart implements the [New Relic's common Helm library](https://github.com/newrelic/helm-charts/tree/master/library/common-library) which
means that it honors a wide range of defaults and globals common to most New Relic Helm charts.

Options that can be defined globally include `affinity`, `nodeSelector`, `tolerations`, `proxy` and others. The full list can be found at
[user's guide of the common library](https://github.com/newrelic/helm-charts/blob/master/library/common-library/README.md).

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| nameOverride | string | `""` | Override the name of the chart |
| fullnameOverride | string | `""` | Override the full name of the release |
| cluster | string | `""` | Name of the Kubernetes cluster monitored. Mandatory. Can be configured also with `global.cluster` |
| licenseKey | string | `""` | This set this license key to use. Can be configured also with `global.licenseKey` |
| customSecretName | string | `""` | In case you don't want to have the license key in you values, this allows you to point to a user created secret to get the key from there. Can be configured also with `global.customSecretName` |
| customSecretLicenseKey | string | `""` | In case you don't want to have the license key in you values, this allows you to point to which secret key is the license key located. Can be configured also with `global.customSecretLicenseKey` |
| images | object | See `values.yaml` | Images used by the chart for the integration and agents |
| images.integration | object | See `values.yaml` | Image for the New Relic Kubernetes integration |
| images.agent | object | See `values.yaml` | Image for the New Relic Infrastructure Agent sidecar |
| images.pullSecrets | list | `[]` | The secrets that are needed to pull images from a custom registry. |
| resources | object | `{}` (no limits set) | Resources for the integration container. |
| forwarder | object | `{}` (no limits set) | Resources for the forwarder sidecar container. |
| rbac.create | bool | `true` | Specifies whether RBAC resources should be created |
| serviceAccount | object | See `values.yaml` | Settings controlling ServiceAccount creation |
| serviceAccount.create | bool | `true` | Specifies whether a ServiceAccount should be created |
| podAnnotations | object | `{}` | Annotations to add to the pod. |
| deployment.annotations | object | `{}` | Annotations to add to the Deployment. |
| podLabels | object | `{}` | Additional labels for chart pods |
| labels | object | `{}` | Additional labels for chart objects |
| agentHTTPTimeout | string | `"30s"` | Amount of time to wait until timeout to send metrics to the metric forwarder |
| sinks | object | See `values.yaml` | Configure where will the metrics be written. Mostly for debugging purposes. |
| sinks.stdout | bool | `false` | Enable the stdout sink to also see all events in the logs. |
| sinks.newRelicInfra | bool | `true` | The newRelicInfra sink sends all events to New Relic. |
| scrapers | object | See `values.yaml` | Configure the various kinds of scrapers that should be run. |
| priorityClassName | string | `""` | Sets pod's priorityClassName. Can be configured also with `global.priorityClassName` |
| hostNetwork | bool | `false` | Sets pod's hostNetwork. Can be configured also with `global.hostNetwork` |
| dnsConfig | object | `{}` | Sets pod's dnsConfig. Can be configured also with `global.dnsConfig` |
| podSecurityContext | object | `{}` | Sets security context (at pod level). Can be configured also with `global.podSecurityContext` |
| containerSecurityContext | object | `{}` | Sets security context (at container level). Can be configured also with `global.containerSecurityContext` |
| affinity | object | `{}` | Sets pod/node affinities. Can be configured also with `global.affinity` |
| nodeSelector | object | `{}` | Sets pod's node selector. Can be configured also with `global.nodeSelector` |
| tolerations | list | `[]` | Sets pod's tolerations to node taints. Can be configured also with `global.tolerations` |
| customAttributes | object | `{}` | Adds extra attributes to the cluster and all the metrics emitted to the backend. Can be configured also with `global.customAttributes` |
| proxy | string | `""` | Configures the integration to send all HTTP/HTTPS request through the proxy in that URL. The URL should have a standard format like `https://user:password@hostname:port`. Can be configured also with `global.proxy` |
| nrStaging | bool | `false` | Send the metrics to the staging backend. Requires a valid staging license key. Can be configured also with `global.nrStaging` |
| fedramp.enabled | bool | `false` | Enables FedRAMP. Can be configured also with `global.fedramp.enabled` |
| verboseLog | bool | `false` | Sets the debug logs to this integration or all integrations if it is set globally. Can be configured also with `global.verboseLog` |

## Maintainers

* [danielstokes](https://github.com/danielstokes)
* [dbudziwojskiNR](https://github.com/dbudziwojskiNR)
* [kondracek-nr](https://github.com/kondracek-nr)
* [kpattaswamy](https://github.com/kpattaswamy)
* [Philip-R-Beckwith](https://github.com/Philip-R-Beckwith)
* [TmNguyen12](https://github.com/TmNguyen12)
