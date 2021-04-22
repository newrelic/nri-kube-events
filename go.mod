module github.com/newrelic/nri-kube-events

go 1.12

require (
	github.com/google/go-cmp v0.5.4
	github.com/imdario/mergo v0.3.11 // indirect
	github.com/newrelic/infra-integrations-sdk v3.6.5+incompatible
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.9.0
	github.com/prometheus/client_model v0.2.0
	github.com/sethgrid/pester v1.1.0
	github.com/sirupsen/logrus v1.7.0
	github.com/stretchr/testify v1.7.0
	gopkg.in/yaml.v3 v3.0.0-20210107192922-496545a6307b
	k8s.io/api v0.21.0
	k8s.io/apimachinery v0.21.0
	k8s.io/client-go v0.20.2
)
