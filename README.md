[![Community Project header](https://github.com/newrelic/open-source-office/raw/master/examples/categories/images/Community_Project.png)](https://github.com/newrelic/open-source-office/blob/master/examples/categories/index.md#community-project)

# New Relic integration for Kubernetes Events

This repository contains a simple Event Router for the kubernetes project.
The event router serves as an active watcher of event resource in the kubernetes system,
which takes those events and pushes them to a list of registered sinks.

## Installation

Add your cluster name and New Relic licence key in the file
`deploy/nri-kube-events.yaml`. You can do a search on the file for `<ADD YOUR
CLUSTER NAME>` and `<ADD YOUR LICENSE KEY>` to find quickly where you need to
replace the value.

Apply the yaml to the cluster with:

```
kubectl apply -f deploy/nri-kube-events.yaml
```

For more in depth instructions check [the official nri-kube-events
docs](https://docs.newrelic.com/docs/integrations/kubernetes-integration/kubernetes-events/install-kubernetes-events-integration).

## Getting Started

Once you've intalled the integration, if you're using the New Relic sink you
can query your events with NRQL like:

```
FROM InfrastructureEvent
SELECT event.involvedObject.kind, event.involvedObject.name, event.type, event.message, event.reason
WHERE category = 'kubernetes' AND clusterName='YOUR_CLUSTER_NAME'
```

## Development flow

### Running the tests

`make test`

### Running the linters

`make lint`

### Building the binary

`make compile`

### Running locally

The easiest way to get started is by using [Skaffold](https://skaffold.dev) and [minikube](https://kubernetes.io/docs/setup/learning-environment/minikube/).
Follow these steps to run this project:

 - Ensure minikube is running
```shell script
➜  ~/nri-kube-events: minikube status
host: Running
kubelet: Running
apiserver: Running
kubectl: Correctly Configured: pointing to minikube-vm at 192.168.x.x
```
 - Create local config and configure the fields marked as `<ADD ...>`

```shell script
cp deploy/local.yaml.example deploy/local.yaml

# check which fields need to be filled:
grep -nrie '<ADD.*>' deploy/local.yaml
```

 - Start the project with the following command

 ```shell script
➜  ~/nri-kube-events: skaffold dev
Generating tags...
 - quay.io/newrelic/nri-kube-events -> quay.io/newrelic/nri-kube-events:latest
Tags generated in 684.354µs
Checking cache...
 - quay.io/newrelic/nri-kube-events: Not found. Building
Cache check complete in 39.444528ms
... more
```

This might take up to a minute to start, but this should start the application in your Minikube cluster with 2 sinks enabled!

## Configuration

nri-kube-events uses a yaml file to configure the application. The structure is as follows. See [Available Sinks](#available-sinks) for a list of sinks

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

## Available Sinks

| Name                            | Description                                                 |
| ------------------------------- | ----------------------------------------------------------- |
| [stdout](#stdout)               | Logs all events to standard output                          |
| [newRelicInfra](#newRelicInfra) | Sends all events to a locally running New Relic Infra Agent |


### stdout

The stdout sink has no configuration.

### newRelicInfra

| Key              | Type                                                   | Description                                               | Required | Default value (if any) |     |
| ---------------- | ------------------------------------------------------ | --------------------------------------------------------- | -------- | ---------------------- | --- |
| clusterName      | string                                                 | The name of your Kubernetes cluster                       | ✅        |                        |     |
| agentEndpoint    | string                                                 | URL of the locally running New Relic Infrastructure Agent | ✅        |                        |     |
| agentHTTPTimeout | [duration](https://golang.org/pkg/time/#ParseDuration) | HTTP timeout for sending http request to the agent        |          | 10s                    |     |

## Releasing

- Update the integration version in the variable `newRelicEventrouterVersion`
  in `pkg/sinks/new_relic_infra.go:38`.
- Make sure the CHANGELOG is up to date.
- Create a Github release like `vX.Y.Z` for both release and tag.
- Publish Docker image and manifest.

## Support

New Relic hosts and moderates an online forum where customers can interact with
New Relic employees as well as other customers to get help and share best
practices. Like all official New Relic open source projects, there's a related
Community topic in the New Relic Explorers Hub. You can find this project's
topic/threads here:

>Add the url for the support thread here

## Contributing
Full details about how to contribute to Contributions to improve New Relic
integration for Kubernetes Events are encouraged! Keep in mind when you submit
your pull request, you'll need to sign the CLA via the click-through using
CLA-Assistant. You only have to sign the CLA one time per project.  To execute
our corporate CLA, which is required if your contribution is on behalf of a
company, or if you have any questions, please drop us an email at
opensource@newrelic.com.

## License
The New Relic integration for Kubernetes Events is licensed under the [Apache
2.0](http://apache.org/licenses/LICENSE-2.0.txt) License.

The New Relic integration for Kubernetes Events also uses source code from
third party libraries. Full details on which libraries are used and the terms
under which they are licensed can be found in the third party notices document.
