[![Community Plus header](https://github.com/newrelic/opensource-website/raw/master/src/images/categories/Community_Plus.png)](https://opensource.newrelic.com/oss-category/#community-plus)

# nri-kube-events

![Version: 2.2.2](https://img.shields.io/badge/Version-2.2.2-informational?style=flat-square) ![AppVersion: 1.8.0](https://img.shields.io/badge/AppVersion-1.8.0-informational?style=flat-square)

A Helm chart to deploy the New Relic Kube Events router

**Homepage:** <https://docs.newrelic.com/docs/integrations/kubernetes-integration/kubernetes-events/install-kubernetes-events-integration>

## Source Code

* <https://github.com/newrelic/nri-kube-events/>
* <https://github.com/newrelic/nri-kube-events/tree/master/charts/newrelic-infrastructure>
* <https://github.com/newrelic/infrastructure-agent/>

## Requirements

| Repository | Name | Version |
|------------|------|---------|
| https://helm-charts.newrelic.com | common-library | 1.0.2 |

## Values managed globally

This chart implements the [New Relic's common Helm library](https://github.com/newrelic/helm-charts/tree/master/library/common-library) which
means that is has a seamless UX between things that are configurable across different Helm charts. So there are behaviours that could be
changed globally if you install this chart from `nri-bundle` or your own umbrella chart.

A really broad list of global managed values are `affinity`, `nodeSelector`, `tolerations`, `proxy` and many more.

For more information go to the [user's guide of the common library](https://github.com/newrelic/helm-charts/blob/master/library/common-library/README.md)

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| agentHTTPTimeout | string | `"30s"` |  |
| deployment.annotations | object | `{}` | Annotations to add to the Deployment. |
| images | object | See `values.yaml` | Images used by the chart for the integration and agents |
| images.agent | object | See `values.yaml` | Image for the New Relic Infrastructure Agent sidecar |
| images.integration | object | See `values.yaml` | Image for the New Relic Kubernetes integration |
| podAnnotations | object | `{}` | Annotations to add to the pod. |
| rbac.create | bool | `true` | Specifies whether RBAC resources should be created |
| resources | object | `{}` | Resources available for this pod |
| serviceAccount | object | See `values.yaml` | Settings controlling ServiceAccount creation |
| serviceAccount.create | bool | `true` | Specifies whether a ServiceAccount should be created |
| sinks | object | See `values.yaml` | Configure where will the metrics be writen. Mostly for debugging purposes. |
| sinks.newRelicInfra | bool | `true` | The newRelicInfra sink sends all events to New relic. |
| sinks.stdout | bool | `false` | Enable the stdout sink to also see all events in the logs. |

## Maintainers

* [alvarocabanas](https://github.com/alvarocabanas)
* [carlossscastro](https://github.com/carlossscastro)
* [sigilioso](https://github.com/sigilioso)
* [gsanchezgavier](https://github.com/gsanchezgavier)
* [kang-makes](https://github.com/kang-makes)
* [marcsanmi](https://github.com/marcsanmi)
* [paologallinaharbur](https://github.com/paologallinaharbur)
* [roobre](https://github.com/roobre)
