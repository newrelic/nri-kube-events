package accountflag

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCommandAPIURL(t *testing.T) {
	us := strings.Repeat("0123456789", 4)
	eu := "eu01xx" + strings.Repeat("a", 34)
	jp := "jp01xx" + strings.Repeat("b", 34)
	gov := "gov01x" + strings.Repeat("c", 34)

	cases := []struct {
		name             string
		licenseKey       string
		want             string
		staging, fedramp bool
	}{
		{name: "us", licenseKey: us, want: "https://infrastructure-command-api.newrelic.com"},
		{name: "us staging", licenseKey: us, staging: true, want: "https://staging-infrastructure-command-api.newrelic.com"},
		{name: "eu", licenseKey: eu, want: "https://infrastructure-command-api.eu.newrelic.com"},
		{name: "eu staging", licenseKey: eu, staging: true, want: "https://staging-infrastructure-command-api.eu.newrelic.com"},
		{name: "jp", licenseKey: jp, want: "https://infrastructure-command-api.jp01.nr-data.net"},
		{name: "jp staging", licenseKey: jp, staging: true, want: "https://staging-infrastructure-command-api.jp01.nr-data.net"},
		{name: "fedramp", licenseKey: us, fedramp: true, want: "https://gov-infrastructure-command-api.newrelic.com"},
		{name: "staging wins over fedramp", licenseKey: us, staging: true, fedramp: true, want: "https://staging-infrastructure-command-api.newrelic.com"},
		{name: "gov key without fedramp", licenseKey: gov, want: "https://infrastructure-command-api.newrelic.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want+"/agent_commands/v1/commands", CommandAPIURL(c.licenseKey, c.staging, c.fedramp))
		})
	}
}
