package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	EgressModeEnforce  = "enforce"
	EgressModeDisabled = "disabled"
)

// EgressPolicy is an operator-owned, fail-closed network contract. Hostnames
// make URL-to-policy checks explainable while destination CIDRs are the actual
// packet-layer boundary installed outside the guest.
type EgressPolicy struct {
	ID           string       `json:"id"`
	Mode         string       `json:"mode"`
	DNSResolvers []string     `json:"dns_resolvers,omitempty"`
	Rules        []EgressRule `json:"rules,omitempty"`
	Channel      string       `json:"channel"`
}

// EgressRule permits one protocol/port set to one or more immutable
// destination prefixes. Hosts are audit labels and must cover every URL that
// the resolved build will use; they never become firewall rules by themselves.
type EgressRule struct {
	ID       string   `json:"id"`
	Hosts    []string `json:"hosts"`
	CIDRs    []string `json:"cidrs"`
	Protocol string   `json:"protocol"`
	Ports    []int    `json:"ports"`
}

// Validate normalizes ordering and rejects policies that cannot be represented
// safely by the PVE firewall adapter.
func (p *EgressPolicy) Validate() error {
	if p == nil || !idPattern.MatchString(p.ID) || !validChannel(p.Channel) {
		return fmt.Errorf("invalid egress policy identity")
	}
	switch p.Mode {
	case EgressModeEnforce:
		if len(p.Rules) == 0 || len(p.Rules) > 64 {
			return fmt.Errorf("enforced egress policy requires 1..64 rules")
		}
	case EgressModeDisabled:
		if p.Channel != "compatibility" || len(p.Rules) != 0 || len(p.DNSResolvers) != 0 {
			return fmt.Errorf("disabled egress policy is allowed only for empty compatibility policy")
		}
		return nil
	default:
		return fmt.Errorf("unsupported egress policy mode %q", p.Mode)
	}

	if err := normalizeDNSResolvers(&p.DNSResolvers); err != nil {
		return err
	}
	seenIDs := make(map[string]struct{}, len(p.Rules))
	for index := range p.Rules {
		rule := &p.Rules[index]
		if err := normalizeEgressRule(rule); err != nil {
			return fmt.Errorf("egress rule %q: %w", rule.ID, err)
		}
		if _, duplicate := seenIDs[rule.ID]; duplicate {
			return fmt.Errorf("duplicate egress rule ID %q", rule.ID)
		}
		seenIDs[rule.ID] = struct{}{}
	}
	sort.Slice(p.Rules, func(i, j int) bool { return p.Rules[i].ID < p.Rules[j].ID })
	return nil
}

func normalizeDNSResolvers(values *[]string) error {
	seen := map[string]struct{}{}
	for index, raw := range *values {
		address, err := netip.ParseAddr(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("invalid DNS resolver %q", raw)
		}
		value := address.String()
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("duplicate DNS resolver %q", value)
		}
		seen[value] = struct{}{}
		(*values)[index] = value
	}
	sort.Strings(*values)
	return nil
}

func normalizeEgressRule(rule *EgressRule) error {
	if rule == nil || !idPattern.MatchString(rule.ID) {
		return fmt.Errorf("invalid rule ID")
	}
	if rule.Protocol != "tcp" && rule.Protocol != "udp" {
		return fmt.Errorf("protocol must be tcp or udp")
	}
	if len(rule.Hosts) == 0 || len(rule.Hosts) > 32 || len(rule.CIDRs) == 0 || len(rule.CIDRs) > 32 ||
		len(rule.Ports) == 0 || len(rule.Ports) > 32 {
		return fmt.Errorf("hosts, cidrs, and ports must each contain 1..32 values")
	}
	if err := normalizeRuleHosts(&rule.Hosts); err != nil {
		return err
	}
	if err := normalizeRuleCIDRs(&rule.CIDRs); err != nil {
		return err
	}
	sort.Ints(rule.Ports)
	for index, port := range rule.Ports {
		if port < 1 || port > 65535 || (index > 0 && rule.Ports[index-1] == port) {
			return fmt.Errorf("ports must be unique values in 1..65535")
		}
	}
	return nil
}

func normalizeRuleHosts(values *[]string) error {
	seen := map[string]struct{}{}
	for index, raw := range *values {
		host := normalizePolicyHost(raw)
		if host == "" {
			return fmt.Errorf("invalid host %q", raw)
		}
		if _, duplicate := seen[host]; duplicate {
			return fmt.Errorf("duplicate host %q", host)
		}
		seen[host] = struct{}{}
		(*values)[index] = host
	}
	sort.Strings(*values)
	return nil
}

func normalizeRuleCIDRs(values *[]string) error {
	seen := map[string]struct{}{}
	for index, raw := range *values {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil || prefix != prefix.Masked() {
			return fmt.Errorf("CIDR %q must be canonical", raw)
		}
		value := prefix.String()
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("duplicate CIDR %q", value)
		}
		seen[value] = struct{}{}
		(*values)[index] = value
	}
	sort.Strings(*values)
	return nil
}

func normalizePolicyHost(raw string) string {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if address := net.ParseIP(host); address != nil {
		return address.String()
	}
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "/\\:@?#") {
		return ""
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return ""
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return ""
			}
		}
	}
	return host
}

// CoversURL verifies that a runtime URL is represented by an enforced policy.
func (p *EgressPolicy) CoversURL(raw, purpose string) error {
	if p == nil || p.Mode != EgressModeEnforce {
		return fmt.Errorf("%s requires an enforced egress policy", purpose)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s is not a credential-free network URL", purpose)
	}
	protocol, port, err := egressURLProtocolPort(parsed)
	if err != nil {
		return fmt.Errorf("%s: %w", purpose, err)
	}
	host := normalizePolicyHost(parsed.Hostname())
	for _, rule := range p.Rules {
		if rule.Protocol == protocol && containsString(rule.Hosts, host) && containsPort(rule.Ports, port) {
			return nil
		}
	}
	return fmt.Errorf("%s host %q port %d/%s is absent from egress policy %q", purpose, host, port, protocol, p.ID)
}

func egressURLProtocolPort(parsed *url.URL) (string, int, error) {
	protocol := "tcp"
	defaultPort := 0
	switch parsed.Scheme {
	case "http":
		defaultPort = 80
	case "https":
		defaultPort = 443
	case "rsync":
		defaultPort = 873
	default:
		return "", 0, fmt.Errorf("unsupported URL scheme %q", parsed.Scheme)
	}
	if parsed.Port() == "" {
		return protocol, defaultPort, nil
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid URL port")
	}
	return protocol, port, nil
}

// Digest returns the canonical policy identity stored with a job and its
// infrastructure evidence.
func (p *EgressPolicy) Digest() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func containsString(values []string, wanted string) bool {
	index := sort.SearchStrings(values, wanted)
	return index < len(values) && values[index] == wanted
}

func containsPort(values []int, wanted int) bool {
	index := sort.SearchInts(values, wanted)
	return index < len(values) && values[index] == wanted
}
