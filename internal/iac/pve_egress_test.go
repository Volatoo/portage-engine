package iac

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/catalog"
)

func TestApplyPVEEgressPolicyReplacesOutboundRulesAndVerifiesReadback(t *testing.T) {
	var mu sync.Mutex
	rules := []pveFirewallRule{
		{Pos: 0, Type: "in", Action: "ACCEPT", Proto: "tcp", DPort: "22", Enable: float64(1)},
		{Pos: 1, Type: "out", Action: "ACCEPT", Dest: "0.0.0.0/0", Proto: "tcp", DPort: "1:65535", Enable: float64(1)},
	}
	var optionsEnabled atomic.Bool
	var started atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "PVEAPIToken=test@pve!builder=secret" {
			http.Error(writer, "missing token", http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api2/json/cluster/firewall/options":
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{"enable": 1}})
			return
		case request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/firewall/options"):
			_ = request.ParseForm()
			optionsEnabled.Store(request.Form.Get("enable") == "1" &&
				request.Form.Get("policy_in") == "ACCEPT" &&
				request.Form.Get("policy_out") == "DROP")
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/firewall/options"):
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{
				"enable": 1, "policy_in": "ACCEPT", "policy_out": "DROP",
			}})
			return
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/firewall/rules"):
			mu.Lock()
			current := append([]pveFirewallRule(nil), rules...)
			mu.Unlock()
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": current})
			return
		case request.Method == http.MethodDelete && strings.Contains(request.URL.Path, "/firewall/rules/"):
			position, _ := strconv.Atoi(request.URL.Path[strings.LastIndex(request.URL.Path, "/")+1:])
			mu.Lock()
			next := rules[:0]
			for _, rule := range rules {
				if rule.Pos != position {
					next = append(next, rule)
				}
			}
			rules = append([]pveFirewallRule(nil), next...)
			mu.Unlock()
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/firewall/rules"):
			_ = request.ParseForm()
			mu.Lock()
			rules = append(rules, pveFirewallRule{
				Pos: len(rules), Type: request.Form.Get("type"), Action: request.Form.Get("action"),
				Dest: request.Form.Get("dest"), Proto: request.Form.Get("proto"), DPort: request.Form.Get("dport"),
				Comment: request.Form.Get("comment"), Enable: float64(1),
			})
			mu.Unlock()
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/status/start"):
			started.Store(true)
		default:
			http.Error(writer, "unexpected request", http.StatusNotFound)
			return
		}
		_, _ = writer.Write([]byte(`{"data":null}`))
	}))
	defer server.Close()

	policy := &catalog.EgressPolicy{
		ID: "egress/test", Mode: catalog.EgressModeEnforce, Channel: "stable",
		DNSResolvers: []string{"10.31.0.1"},
		Rules: []catalog.EgressRule{{
			ID: "mirror", Hosts: []string{"nas.internal"}, CIDRs: []string{"10.31.0.2/32"}, Protocol: "tcp", Ports: []int{80, 443},
		}},
	}
	auth := PVEAuth{TokenID: "test@pve!builder", TokenSecret: "secret"}
	evidence, err := ApplyPVEEgressPolicy(context.Background(), server.URL, auth, "pve01", "9100", policy, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !optionsEnabled.Load() || evidence.PolicyIn != "ACCEPT" || evidence.PolicyOut != "DROP" ||
		evidence.RuleCount != 3 || evidence.ReadbackDigest == "" {
		t.Fatalf("unexpected enforcement evidence: options=%t evidence=%+v", optionsEnabled.Load(), evidence)
	}
	mu.Lock()
	if len(rules) != 4 || rules[0].Type != "in" {
		mu.Unlock()
		t.Fatalf("inbound rule was lost or outbound rules are wrong: %+v", rules)
	}
	for _, rule := range rules[1:] {
		if rule.Dest == "0.0.0.0/0" || rule.Type != "out" || rule.Action != "ACCEPT" {
			mu.Unlock()
			t.Fatalf("permissive or malformed outbound rule survived: %+v", rule)
		}
	}
	mu.Unlock()
	if err := StartPVEVM(context.Background(), server.URL, auth, "pve01", "9100"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if !started.Load() {
		t.Fatal("stopped VM was not started after enforcement")
	}
}

func TestApplyPVEEgressPolicyRejectsReadbackDrift(t *testing.T) {
	getCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet && request.URL.Path == "/api2/json/cluster/firewall/options" {
			_, _ = writer.Write([]byte(`{"data":{"enable":1}}`))
			return
		}
		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/firewall/options") {
			_, _ = writer.Write([]byte(`{"data":{"enable":1,"policy_in":"ACCEPT","policy_out":"DROP"}}`))
			return
		}
		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/firewall/rules") {
			getCount++
			if getCount == 1 {
				_, _ = writer.Write([]byte(`{"data":[]}`))
			} else {
				_, _ = writer.Write([]byte(`{"data":[{"pos":0,"type":"out","action":"ACCEPT","dest":"0.0.0.0/0","proto":"tcp","dport":"443","enable":1,"comment":"drift"}]}`))
			}
			return
		}
		_, _ = writer.Write([]byte(`{"data":null}`))
	}))
	defer server.Close()
	policy := &catalog.EgressPolicy{
		ID: "egress/test", Mode: catalog.EgressModeEnforce, Channel: "stable",
		Rules: []catalog.EgressRule{{ID: "mirror", Hosts: []string{"nas.internal"}, CIDRs: []string{"10.31.0.2/32"}, Protocol: "tcp", Ports: []int{443}}},
	}
	_, err := ApplyPVEEgressPolicy(context.Background(), server.URL, PVEAuth{TokenID: "id", TokenSecret: "secret"}, "pve", "9000", policy, time.Now())
	if err == nil || !strings.Contains(err.Error(), "readback differs") {
		t.Fatalf("readback drift was accepted: %v", err)
	}
}

func TestApplyPVEEgressPolicyRejectsDisabledClusterFirewall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":{"enable":0}}`))
	}))
	defer server.Close()
	policy := &catalog.EgressPolicy{
		ID: "egress/test", Mode: catalog.EgressModeEnforce, Channel: "stable",
		Rules: []catalog.EgressRule{{ID: "mirror", Hosts: []string{"nas.internal"}, CIDRs: []string{"10.31.0.2/32"}, Protocol: "tcp", Ports: []int{443}}},
	}
	_, err := ApplyPVEEgressPolicy(context.Background(), server.URL, PVEAuth{TokenID: "id", TokenSecret: "secret"}, "pve", "9000", policy, time.Now())
	if err == nil || !strings.Contains(err.Error(), "cluster firewall is disabled") {
		t.Fatalf("disabled cluster firewall was accepted: %v", err)
	}
}

func TestVerifyPVEFirewallOptionsRejectsImplicitInputPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":{"enable":1,"policy_out":"DROP"}}`))
	}))
	defer server.Close()

	client, err := newPVEAPIClient(context.Background(), server.URL, PVEAuth{
		TokenID: "id", TokenSecret: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = verifyPVEFirewallOptions(context.Background(), client, "/api2/json/nodes/pve/qemu/9000/firewall")
	if err == nil || !strings.Contains(err.Error(), "policy_in=ACCEPT") {
		t.Fatalf("implicit input policy was accepted: %v", err)
	}
}

func TestClosePVEVMInboundScopesChangeToTargetVMAndVerifiesReadback(t *testing.T) {
	var updated atomic.Bool
	var mu sync.Mutex
	rules := []pveFirewallRule{
		{Pos: 0, Type: "in", Action: "ACCEPT", Proto: "tcp", DPort: "22", Enable: float64(1)},
		{Pos: 1, Type: "out", Action: "ACCEPT", Dest: "10.31.0.104/32", Proto: "tcp", DPort: "19444", Enable: float64(1)},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		base := "/api2/json/nodes/pve01/qemu/9100/firewall"
		if !strings.HasPrefix(request.URL.Path, base) {
			http.Error(writer, "wrong VM scope", http.StatusBadRequest)
			return
		}
		switch {
		case request.Method == http.MethodPut && request.URL.Path == base+"/options":
			_ = request.ParseForm()
			updated.Store(request.Form.Get("enable") == "1" &&
				request.Form.Get("policy_in") == "DROP" &&
				request.Form.Get("policy_out") == "DROP")
			_, _ = writer.Write([]byte(`{"data":null}`))
		case request.Method == http.MethodGet && request.URL.Path == base+"/options":
			_, _ = writer.Write([]byte(`{"data":{"enable":1,"policy_in":"DROP","policy_out":"DROP"}}`))
		case request.Method == http.MethodGet && request.URL.Path == base+"/rules":
			mu.Lock()
			current := append([]pveFirewallRule(nil), rules...)
			mu.Unlock()
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": current})
		case request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, base+"/rules/"):
			position, _ := strconv.Atoi(request.URL.Path[strings.LastIndex(request.URL.Path, "/")+1:])
			mu.Lock()
			next := rules[:0]
			for _, rule := range rules {
				if rule.Pos != position {
					next = append(next, rule)
				}
			}
			rules = append([]pveFirewallRule(nil), next...)
			mu.Unlock()
			_, _ = writer.Write([]byte(`{"data":null}`))
		default:
			http.Error(writer, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	err := ClosePVEVMInbound(context.Background(), server.URL,
		PVEAuth{TokenID: "id", TokenSecret: "secret"}, "pve01", "9100")
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Load() {
		t.Fatal("target VM was not switched to policy_in=DROP")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(rules) != 1 || rules[0].Type != "out" {
		t.Fatalf("VM-level inbound rules survived the close operation: %+v", rules)
	}
}

func TestExpectedPVERulesUsesCanonicalPorts(t *testing.T) {
	policy := &catalog.EgressPolicy{
		ID: "egress/test", Mode: catalog.EgressModeEnforce, Channel: "stable",
		Rules: []catalog.EgressRule{{ID: "mirror", Hosts: []string{"nas.internal"}, CIDRs: []string{"10.31.0.2/32"}, Protocol: "tcp", Ports: []int{443, 80}}},
	}
	digest, err := policy.Digest()
	if err != nil {
		t.Fatal(err)
	}
	rules, err := expectedPVERules(policy, digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].DPort != "80,443" {
		t.Fatalf("unexpected rules: %+v", rules)
	}
}
