# New Relic Kube Events

This repository contains a simple Event Router for the kubernetes project. 
The event router serves as an active watcher of event resource in the kubernetes system, 
which takes those events and pushes them to a list of registered sinks. 

**Table of contents**
- [Things to impove](#things-to-impove)
- [Running](#running)
- [Available Sinks](#available-sinks)
  - [stdout](#stdout)
  - [newRelicInfra](#newrelicinfra)
 
## Things to impove

Some things we could add/improve

 - add more tests
 - retry policy / circuit breaking when sending events to the agent
 - add Prometheus metrics
 
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
➜  ~/nr-kube-events: minikube status
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
➜  ~/nr-kube-events: skaffold dev
Generating tags...
 - quay.io/newrelic/nr-kube-events -> quay.io/newrelic/nr-kube-events:latest
Tags generated in 684.354µs
Checking cache...
 - quay.io/newrelic/nr-kube-events: Not found. Building
Cache check complete in 39.444528ms
... more
```

This might take up to a minute to start, but this should start the application in your Minikube cluster with 2 sinks enabled!

## Configuration

nr-kube-events uses a yaml file to configure the application. The structure is as follows. See [Available Sinks](#available-sinks) for a list of sinks

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
