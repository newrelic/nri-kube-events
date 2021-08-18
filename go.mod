module github.com/newrelic/nri-kube-events

go 1.16

require (
	github.com/golangci/golangci-lint v1.40.1
	github.com/google/go-cmp v0.5.6
	github.com/newrelic/infra-integrations-sdk v3.6.7+incompatible
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.10.0
	github.com/prometheus/client_model v0.2.0
	github.com/sethgrid/pester v1.1.0
	github.com/sirupsen/logrus v1.8.1
	github.com/stretchr/testify v1.7.0
	gopkg.in/yaml.v3 v3.0.0-20210107192922-496545a6307b
	k8s.io/api v0.22.0
	k8s.io/apimachinery v0.22.0
	k8s.io/client-go v0.21.0
)
