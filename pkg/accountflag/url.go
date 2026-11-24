package accountflag

import (
	"fmt"
	"regexp"
	"strings"
)

// The command API base URLs and the region rules below are copied from the New Relic
// infrastructure agent at commit 3346a15: pkg/config/defaults.go:18-37 (URLs),
// pkg/config/config.go:2132-2165 (calculateCmdChannelURL and its helpers) and
// pkg/license/license.go (IsRegionEU, GetRegionForURL, GetRegionPrefix, GetRegion,
// IsFederalCompliance). Keep them in step with the agent so both poll the same endpoint.
const (
	cmdChannelURL              = "https://infrastructure-command-api.newrelic.com"
	cmdChannelStagingURL       = "https://staging-infrastructure-command-api.newrelic.com"
	cmdChannelURLEu            = "https://infrastructure-command-api.eu.newrelic.com"
	cmdChannelStagingURLEu     = "https://staging-infrastructure-command-api.eu.newrelic.com"
	cmdChannelFedRAMPURL       = "https://gov-infrastructure-command-api.newrelic.com"
	cmdChannelURLRegion        = "https://infrastructure-command-api.%s.nr-data.net"
	cmdChannelStagingURLRegion = "https://staging-infrastructure-command-api.%s.nr-data.net"

	// commandsPath is the agent's defaultCmdChannelEndpoint (pkg/config/defaults.go:67).
	commandsPath = "/agent_commands/v1/commands"
)

var (
	regionLicenseRegex = regexp.MustCompile(`^([a-z]{2,3})`)
	regionKeyRegex     = regexp.MustCompile(`^.+?x`)
)

// CommandAPIURL returns the infrastructure command API commands URL for licenseKey.
// Staging is checked before FedRAMP, as the infrastructure agent does.
func CommandAPIURL(licenseKey string, staging, fedramp bool) string {
	var base string
	switch {
	case staging:
		base = regionURL(licenseKey, cmdChannelStagingURLEu, cmdChannelStagingURL, cmdChannelStagingURLRegion)
	case fedramp:
		base = cmdChannelFedRAMPURL
	default:
		base = regionURL(licenseKey, cmdChannelURLEu, cmdChannelURL, cmdChannelURLRegion)
	}
	return base + commandsPath
}

func regionURL(licenseKey, eu, us, regionFormat string) string {
	if isRegionEU(licenseKey) {
		return eu
	}
	r := regionForURL(licenseKey)
	if r == "" {
		return us
	}
	return fmt.Sprintf(regionFormat, r)
}

func isRegionEU(licenseKey string) bool {
	r := region(licenseKey)
	return len(r) > 1 && r[:2] == "eu"
}

func isFederalCompliance(licenseKey string) bool {
	return region(licenseKey) == "gov"
}

// regionForURL returns the region of a region-aware key, or empty for a US or federal
// key. Only region-aware keys contain 'x', which is not a hex digit.
func regionForURL(licenseKey string) string {
	if !strings.ContainsRune(licenseKey, 'x') || isFederalCompliance(licenseKey) {
		return ""
	}
	return regionPrefix(licenseKey)
}

// regionPrefix returns the key up to its first 'x' with trailing 'x' characters removed,
// so "jp01xx..." gives "jp01".
func regionPrefix(licenseKey string) string {
	return strings.TrimRight(regionKeyRegex.FindString(licenseKey), "x")
}

func region(licenseKey string) string {
	if m := regionLicenseRegex.FindStringSubmatch(licenseKey); len(m) > 1 {
		return m[1]
	}
	return ""
}
