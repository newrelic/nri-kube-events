<a href="https://opensource.newrelic.com/oss-category/#community-plus"><picture><source media="(prefers-color-scheme: dark)" srcset="https://github.com/newrelic/opensource-website/raw/main/src/images/categories/dark/Community_Plus.png"><source media="(prefers-color-scheme: light)" srcset="https://github.com/newrelic/opensource-website/raw/main/src/images/categories/Community_Plus.png"><img alt="New Relic Open Source community plus project banner." src="https://github.com/newrelic/opensource-website/raw/main/src/images/categories/Community_Plus.png"></picture></a>

# New Relic integration for Kubernetes events [![codecov](https://codecov.io/gh/newrelic/nri-kube-events/graph/badge.svg?token=NFUFLNJJPB)](https://codecov.io/gh/newrelic/nri-kube-events)

This repository contains a simple event router for the Kubernetes project.
The event router serves as an active watcher of event resource in the Kubernetes system,
which takes those events and pushes them to a list of configured sinks.

## Table of contents

- [Table of contents](#table-of-contents)
- [Installation](#installation)
- [Getting started](#getting-started)
- [Development flow](#development-flow)
  - [Running locally](#running-locally)
- [Configuration](#configuration)
- [Available sinks](#available-sinks)
  - [stdout](#stdout)
  - [newRelicInfra](#newrelicinfra)
- [Support](#support)
- [Contributing](#contributing)
- [License](#license)

## Installation

For installation instructions see our [docs](https://docs.newrelic.com/docs/integrations/kubernetes-integration/kubernetes-events/install-kubernetes-events-integration).

## Getting started

Once you've installed the integration, and you've configured the New Relic sink,
you can find your events in New Relic using this query:

```
FROM InfrastructureEvent
SELECT event.involvedObject.kind, event.involvedObject.name, event.type, event.message, event.reason
WHERE category = 'kubernetes' AND clusterName='YOUR_CLUSTER_NAME'
```

## Helm chart

You can install this chart using [`nri-bundle`](https://github.com/newrelic/helm-charts/tree/master/charts/nri-bundle) located in the
[helm-charts repository](https://github.com/newrelic/helm-charts) or directly from this repository by adding this Helm repository:

```shell
helm repo add nri-kube-events https://newrelic.github.io/nri-kube-events
helm upgrade --install nri-kube-events/nri-kube-events -f your-custom-values.yaml
```

For further information of the configuration needed for the chart just read the [chart's README](/charts/nri-kube-events/README.md).

## Development flow

This project uses a Makefile for the most common use cases:

Some available commands include:

```sh
make test # run the unit tests
make lint # lint the code using golangci-lint
make compile # compile the project into a single binary
```

### Running locally

The easiest way to get started is by using [Skaffold](https://skaffold.dev)
and [Minikube](https://kubernetes.io/docs/setup/learning-environment/minikube/).

Follow these steps to run this project:

 - Ensure Minikube is running
```sh
$ minikube status
host: Running
kubelet: Running
apiserver: Running
kubectl: Correctly Configured: pointing to minikube-vm at 192.168.x.x
```

 - Copy the example configuration and configure the placeholders marked with `<ADD ...>`
```sh
cp deploy/local.yaml.example deploy/local.yaml

# Command to see all placeholders that need to be configured:
grep -nrie '<ADD.*>' deploy/local.yaml
```

 - Start the project with the following command
 ```sh
$ skaffold dev
Generating tags...
 - quay.io/newrelic/nri-kube-events -> quay.io/newrelic/nri-kube-events:latest
Tags generated in 684.354µs
Checking cache...
 - quay.io/newrelic/nri-kube-events: Not found. Building
Cache check complete in 39.444528ms
... more
```

This might take up to a minute to start, but this should start the application
in your Minikube cluster with 2 sinks enabled!

### E2E Tests
See the [E2E README](./e2e/README.md) for more details regarding running E2E tests.

## Configuration

nri-kube-events uses a YAML file to configure the application. The structure is
as follows. See [Available Sinks](#available-sinks) for a list of sinks.

```yaml
sinks:
- name: sink1
  config:
    config_key_1: config_value_1
    config_key_2: config_value_2
- name: newRelicInfra
  config:
    agentEndpoint: http://infra-agent.default:8001/v1/data
    clusterName: minikube
```

## Available sinks

| Name                            | Description                                                 |
| ------------------------------- | ----------------------------------------------------------- |
| [stdout](#stdout)               | Logs all events to standard output                          |
| [newRelicInfra](#newRelicInfra) | Sends all events to a locally running New Relic infrastructure agent |


### stdout

The stdout sink has no configuration.

### newRelicInfra

| Key              | Type                                                   | Description                                               | Required | Default value (if any) |     |
| ---------------- | ------------------------------------------------------ | --------------------------------------------------------- | -------- | ---------------------- | --- |
| clusterName      | string                                                 | The name of your Kubernetes cluster                       | ✅        |                        |     |
| agentEndpoint    | string                                                 | URL of the locally running New Relic infrastructure Agent | ✅        |                        |     |
| agentHTTPTimeout | [duration](https://golang.org/pkg/time/#ParseDuration) | HTTP timeout for sending http request to the agent        |          | 10s                    |     |

## Support

New Relic hosts and moderates an online forum where customers can interact with
New Relic employees as well as other customers to get help and share best
practices. Like all official New Relic open source projects, there's a related
Community topic in the New Relic Explorers Hub. You can find this project's
topic/threads here:

https://forum.newrelic.com/t/new-relic-kube-events-integration/109094

## Contributing

Full details about how to contribute to Contributions to improve New Relic
integration for Kubernetes events are encouraged! Keep in mind when you submit
your pull request, you'll need to sign the CLA via the click-through using
CLA-Assistant. You only have to sign the CLA one time per project.  To execute
our corporate CLA, which is required if your contribution is on behalf of a
company, or if you have any questions, please drop us an email at
opensource@newrelic.com.

**A note about vulnerabilities**

As noted in our [security policy](../../security/policy), New Relic is committed
to the privacy and security of our customers and their data. We believe that
providing coordinated disclosure by security researchers and engaging with the
security community are important means to achieve our security goals.

If you believe you have found a security vulnerability in this project or any of
New Relic's products or websites, we welcome and greatly appreciate you reporting
it to New Relic through [HackerOne](https://hackerone.com/newrelic).

If you would like to contribute to this project, please review [these guidelines](./CONTRIBUTING.md).

To all contributors, we thank you!  Without your contribution, this project would
not be what it is today.

## License
The New Relic integration for Kubernetes events is licensed under the [Apache
2.0](http://apache.org/licenses/LICENSE-2.0.txt) License.

The New Relic integration for Kubernetes events also uses source code from
third party libraries. Full details on which libraries are used and the terms
under which they are licensed can be found in the third party notices document.
