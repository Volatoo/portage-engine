package iac

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/slchris/portage-engine/internal/catalog"
)

const egressEvidenceSchemaVersion = 2

// EgressEnforcementEvidence records the external packet-layer boundary that
// was read back from PVE before the build VM was allowed to boot.
type EgressEnforcementEvidence struct {
	SchemaVersion  int       `json:"schema_version"`
	PolicyID       string    `json:"policy_id"`
	PolicyDigest   string    `json:"policy_digest"`
	Provider       string    `json:"provider"`
	Node           string    `json:"node"`
	VMID           string    `json:"vmid"`
	AppliedAt      time.Time `json:"applied_at"`
	RuleCount      int       `json:"rule_count"`
	PolicyIn       string    `json:"policy_in"`
	PolicyOut      string    `json:"policy_out"`
	ReadbackDigest string    `json:"readback_digest"`
	ImplicitAllows []string  `json:"implicit_allows"`
}

type pveFirewallRule struct {
	Pos     int    `json:"pos"`
	Type    string `json:"type"`
	Action  string `json:"action"`
	Dest    string `json:"dest"`
	Proto   string `json:"proto"`
	DPort   string `json:"dport"`
	Comment string `json:"comment"`
	Enable  any    `json:"enable"`
}

type expectedPVEFirewallRule struct {
	Dest    string `json:"dest"`
	Proto   string `json:"proto"`
	DPort   string `json:"dport"`
	Comment string `json:"comment"`
}

// ApplyPVEEgressPolicy enables a default-deny outbound policy, replaces every
// VM-level outbound rule, and verifies the exact readback. Existing inbound
// rules are left alone and policy_in remains ACCEPT so enabling the VM
// firewall does not silently block the controller's SSH/bootstrap traffic.
func ApplyPVEEgressPolicy(
	ctx context.Context,
	endpoint string,
	auth PVEAuth,
	node string,
	vmid string,
	policy *catalog.EgressPolicy,
	now time.Time,
) (*EgressEnforcementEvidence, error) {
	if strings.TrimSpace(node) == "" || strings.TrimSpace(vmid) == "" {
		return nil, fmt.Errorf("PVE node and VMID are required for egress enforcement")
	}
	if policy == nil || policy.Mode != catalog.EgressModeEnforce {
		return nil, fmt.Errorf("PVE egress enforcement requires an enforced policy")
	}
	policyDigest, err := policy.Digest()
	if err != nil {
		return nil, fmt.Errorf("validate egress policy: %w", err)
	}
	expected, err := expectedPVERules(policy, policyDigest)
	if err != nil {
		return nil, err
	}
	client, err := newPVEAPIClient(ctx, endpoint, auth)
	if err != nil {
		return nil, err
	}
	if err := verifyPVEClusterFirewall(ctx, client); err != nil {
		return nil, err
	}
	base := "/api2/json/nodes/" + url.PathEscape(node) + "/qemu/" + url.PathEscape(vmid) + "/firewall"

	// Close the boundary before changing individual rules. Provision keeps an
	// enforced VM powered off until this whole function succeeds.
	options := url.Values{
		"enable":        {"1"},
		"policy_in":     {"ACCEPT"},
		"policy_out":    {"DROP"},
		"log_level_out": {"info"},
		"dhcp":          {"1"},
		"ndp":           {"1"},
	}
	if err := client.do(ctx, http.MethodPut, base+"/options", options, nil); err != nil {
		return nil, fmt.Errorf("enable PVE default-deny egress: %w", err)
	}

	rules, err := readPVEFirewallRules(ctx, client, base)
	if err != nil {
		return nil, err
	}
	outboundPositions := make([]int, 0)
	for _, rule := range rules {
		if strings.EqualFold(rule.Type, "out") {
			outboundPositions = append(outboundPositions, rule.Pos)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(outboundPositions)))
	for _, position := range outboundPositions {
		if err := client.do(ctx, http.MethodDelete, base+"/rules/"+strconv.Itoa(position), nil, nil); err != nil {
			return nil, fmt.Errorf("delete prior PVE outbound firewall rule %d: %w", position, err)
		}
	}
	for _, rule := range expected {
		form := url.Values{
			"type":    {"out"},
			"action":  {"ACCEPT"},
			"dest":    {rule.Dest},
			"proto":   {rule.Proto},
			"dport":   {rule.DPort},
			"enable":  {"1"},
			"comment": {rule.Comment},
		}
		if err := client.do(ctx, http.MethodPost, base+"/rules", form, nil); err != nil {
			return nil, fmt.Errorf("create PVE outbound firewall rule %q: %w", rule.Comment, err)
		}
	}

	if err := verifyPVEFirewallOptions(ctx, client, base); err != nil {
		return nil, err
	}
	readback, err := readPVEFirewallRules(ctx, client, base)
	if err != nil {
		return nil, err
	}
	actual := make([]expectedPVEFirewallRule, 0, len(expected))
	for _, rule := range readback {
		if !strings.EqualFold(rule.Type, "out") {
			continue
		}
		if !strings.EqualFold(rule.Action, "ACCEPT") || !pveFirewallValueEnabled(rule.Enable) {
			return nil, fmt.Errorf("PVE outbound firewall readback contains a non-ACCEPT or disabled rule at position %d", rule.Pos)
		}
		actual = append(actual, expectedPVEFirewallRule{
			Dest: rule.Dest, Proto: strings.ToLower(rule.Proto), DPort: rule.DPort, Comment: rule.Comment,
		})
	}
	sortExpectedPVERules(actual)
	if !equalExpectedPVERules(actual, expected) {
		return nil, fmt.Errorf("PVE outbound firewall readback differs from policy %q", policy.ID)
	}
	readbackDigest, err := digestPVERules(actual)
	if err != nil {
		return nil, err
	}
	return &EgressEnforcementEvidence{
		SchemaVersion: egressEvidenceSchemaVersion,
		PolicyID:      policy.ID, PolicyDigest: policyDigest, Provider: "pve",
		Node: node, VMID: vmid, AppliedAt: now.UTC(), RuleCount: len(actual),
		PolicyIn: "ACCEPT", PolicyOut: "DROP", ReadbackDigest: readbackDigest,
		ImplicitAllows: []string{"dhcp", "ndp"},
	}, nil
}

func StartPVEVM(ctx context.Context, endpoint string, auth PVEAuth, node, vmid string) error {
	client, err := newPVEAPIClient(ctx, endpoint, auth)
	if err != nil {
		return err
	}
	path := "/api2/json/nodes/" + url.PathEscape(node) + "/qemu/" + url.PathEscape(vmid) + "/status/start"
	if err := client.do(ctx, http.MethodPost, path, nil, nil); err != nil {
		return fmt.Errorf("start PVE VM: %w", err)
	}
	return nil
}

// ClosePVEVMInbound removes the transient SSH/bootstrap allowance after the
// worker has authenticated outbound. It changes only this VM's firewall
// options and removes every VM-level inbound rule so a template-carried ACCEPT
// rule cannot override policy_in=DROP. Cluster and node rules remain untouched.
func ClosePVEVMInbound(
	ctx context.Context,
	endpoint string,
	auth PVEAuth,
	node, vmid string,
) error {
	client, err := newPVEAPIClient(ctx, endpoint, auth)
	if err != nil {
		return err
	}
	base := "/api2/json/nodes/" + url.PathEscape(node) + "/qemu/" + url.PathEscape(vmid) + "/firewall"
	if err := client.do(ctx, http.MethodPut, base+"/options", url.Values{
		"enable":     {"1"},
		"policy_in":  {"DROP"},
		"policy_out": {"DROP"},
	}, nil); err != nil {
		return fmt.Errorf("close PVE VM inbound policy: %w", err)
	}
	rules, err := readPVEFirewallRules(ctx, client, base)
	if err != nil {
		return err
	}
	inboundPositions := make([]int, 0)
	for _, rule := range rules {
		if strings.EqualFold(rule.Type, "in") {
			inboundPositions = append(inboundPositions, rule.Pos)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(inboundPositions)))
	for _, position := range inboundPositions {
		if err := client.do(ctx, http.MethodDelete, base+"/rules/"+strconv.Itoa(position), nil, nil); err != nil {
			return fmt.Errorf("delete PVE inbound firewall rule %d: %w", position, err)
		}
	}
	var payload struct {
		Data map[string]any `json:"data"`
	}
	if err := client.do(ctx, http.MethodGet, base+"/options", nil, &payload); err != nil {
		return fmt.Errorf("read closed PVE VM firewall options: %w", err)
	}
	if !pveFirewallValueEnabled(payload.Data["enable"]) ||
		!strings.EqualFold(fmt.Sprint(payload.Data["policy_in"]), "DROP") ||
		!strings.EqualFold(fmt.Sprint(payload.Data["policy_out"]), "DROP") {
		return fmt.Errorf("PVE VM firewall readback is not enabled with policy_in=DROP and policy_out=DROP")
	}
	readback, err := readPVEFirewallRules(ctx, client, base)
	if err != nil {
		return err
	}
	for _, rule := range readback {
		if strings.EqualFold(rule.Type, "in") {
			return fmt.Errorf("PVE VM firewall readback still contains inbound rule at position %d", rule.Pos)
		}
	}
	return nil
}

func expectedPVERules(policy *catalog.EgressPolicy, policyDigest string) ([]expectedPVEFirewallRule, error) {
	prefix := strings.TrimPrefix(policyDigest, "sha256:")
	if len(prefix) < 12 {
		return nil, fmt.Errorf("invalid egress policy digest")
	}
	result := make([]expectedPVEFirewallRule, 0)
	for _, rule := range policy.Rules {
		ports := joinPorts(rule.Ports)
		for _, destination := range rule.CIDRs {
			result = append(result, expectedPVEFirewallRule{
				Dest: destination, Proto: rule.Protocol, DPort: ports,
				Comment: "portage-engine:" + prefix[:12] + ":" + rule.ID,
			})
		}
	}
	for _, resolver := range policy.DNSResolvers {
		address, err := netip.ParseAddr(resolver)
		if err != nil {
			return nil, err
		}
		destination := netip.PrefixFrom(address, address.BitLen()).String()
		for _, protocol := range []string{"tcp", "udp"} {
			result = append(result, expectedPVEFirewallRule{
				Dest: destination, Proto: protocol, DPort: "53",
				Comment: "portage-engine:" + prefix[:12] + ":dns",
			})
		}
	}
	sortExpectedPVERules(result)
	return result, nil
}

func readPVEFirewallRules(ctx context.Context, client *pveAPIClient, base string) ([]pveFirewallRule, error) {
	var payload struct {
		Data []pveFirewallRule `json:"data"`
	}
	if err := client.do(ctx, http.MethodGet, base+"/rules", nil, &payload); err != nil {
		return nil, fmt.Errorf("read PVE firewall rules: %w", err)
	}
	return payload.Data, nil
}

func verifyPVEFirewallOptions(ctx context.Context, client *pveAPIClient, base string) error {
	var payload struct {
		Data map[string]any `json:"data"`
	}
	if err := client.do(ctx, http.MethodGet, base+"/options", nil, &payload); err != nil {
		return fmt.Errorf("read PVE firewall options: %w", err)
	}
	if !pveFirewallValueEnabled(payload.Data["enable"]) ||
		!strings.EqualFold(fmt.Sprint(payload.Data["policy_in"]), "ACCEPT") ||
		!strings.EqualFold(fmt.Sprint(payload.Data["policy_out"]), "DROP") {
		return fmt.Errorf("PVE firewall readback is not enabled with policy_in=ACCEPT and policy_out=DROP")
	}
	return nil
}

func verifyPVEClusterFirewall(ctx context.Context, client *pveAPIClient) error {
	var payload struct {
		Data map[string]any `json:"data"`
	}
	if err := client.do(ctx, http.MethodGet, "/api2/json/cluster/firewall/options", nil, &payload); err != nil {
		return fmt.Errorf("read PVE cluster firewall options: %w", err)
	}
	if !pveFirewallValueEnabled(payload.Data["enable"]) {
		return fmt.Errorf("PVE cluster firewall is disabled; VM rules would not be an enforcement boundary")
	}
	return nil
}

func pveFirewallValueEnabled(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case float64:
		return typed == 1
	case string:
		return typed == "1" || strings.EqualFold(typed, "true") || strings.EqualFold(typed, "yes")
	default:
		return false
	}
}

func joinPorts(ports []int) string {
	values := make([]string, len(ports))
	for index, port := range ports {
		values[index] = strconv.Itoa(port)
	}
	return strings.Join(values, ",")
}

func sortExpectedPVERules(rules []expectedPVEFirewallRule) {
	sort.Slice(rules, func(i, j int) bool {
		left := rules[i].Dest + "\x00" + rules[i].Proto + "\x00" + rules[i].DPort + "\x00" + rules[i].Comment
		right := rules[j].Dest + "\x00" + rules[j].Proto + "\x00" + rules[j].DPort + "\x00" + rules[j].Comment
		return left < right
	})
}

func equalExpectedPVERules(left, right []expectedPVEFirewallRule) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func digestPVERules(rules []expectedPVEFirewallRule) (string, error) {
	data, err := json.Marshal(rules)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
