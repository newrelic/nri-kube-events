module github.com/newrelic/nri-kube-events

go 1.16

require (
	github.com/golangci/golangci-lint v1.40.1
	github.com/google/go-cmp v0.5.6
	github.com/newrelic/infra-integrations-sdk v3.7.0+incompatible
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.11.0
	github.com/prometheus/client_model v0.2.0
	github.com/sethgrid/pester v1.1.0
	github.com/sirupsen/logrus v1.8.1
	github.com/stretchr/testify v1.7.0
	gopkg.in/yaml.v3 v3.0.0-20210107192922-496545a6307b
	k8s.io/api v0.21.3
	k8s.io/apimachinery v0.21.3
	k8s.io/client-go v0.21.0
)

replace (
	// To avoid CVE-2020-13949 triggering a security scan.
	github.com/apache/thrift => github.com/apache/thrift v0.14.0
	// To avoid CVE-2018-16886 triggering a security scan.
	go.etcd.io/etcd => go.etcd.io/etcd v0.5.0-alpha.5.0.20190108173120-83c051b701d3
)
