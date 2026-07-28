package catalog

import (
	"strings"
	"testing"
)

func TestEgressPolicyNormalizesAndCoversURLs(t *testing.T) {
	policy := &EgressPolicy{
		ID: "egress/test", Mode: EgressModeEnforce, Channel: "stable",
		DNSResolvers: []string{"2001:db8::53", "10.31.0.1"},
		Rules: []EgressRule{{
			ID: "artifact", Hosts: []string{"NAS.INTERNAL.", "10.31.0.2"},
			CIDRs: []string{"2001:db8::2/128", "10.31.0.2/32"}, Protocol: "tcp", Ports: []int{443, 80},
		}},
	}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"http://10.31.0.2/binpkgs", "https://nas.internal/builder"} {
		if err := policy.CoversURL(endpoint, "test endpoint"); err != nil {
			t.Fatalf("CoversURL(%q): %v", endpoint, err)
		}
	}
	if err := policy.CoversURL("https://public.example/binpkgs", "public fallback"); err == nil ||
		!strings.Contains(err.Error(), "absent") {
		t.Fatalf("unexpected public fallback result: %v", err)
	}
	first, err := policy.Digest()
	if err != nil {
		t.Fatal(err)
	}
	second, err := policy.Digest()
	if err != nil || first != second {
		t.Fatalf("policy digest is not stable: first=%q second=%q err=%v", first, second, err)
	}
}

func TestEgressPolicyRejectsUnsafeContracts(t *testing.T) {
	tests := []struct {
		name   string
		policy EgressPolicy
	}{
		{
			name: "non-canonical CIDR",
			policy: EgressPolicy{ID: "egress/test", Mode: EgressModeEnforce, Channel: "stable",
				Rules: []EgressRule{{ID: "bad", Hosts: []string{"mirror.internal"}, CIDRs: []string{"10.31.0.2/24"}, Protocol: "tcp", Ports: []int{443}}}},
		},
		{
			name: "wildcard host",
			policy: EgressPolicy{ID: "egress/test", Mode: EgressModeEnforce, Channel: "stable",
				Rules: []EgressRule{{ID: "bad", Hosts: []string{"*.internal"}, CIDRs: []string{"10.31.0.2/32"}, Protocol: "tcp", Ports: []int{443}}}},
		},
		{
			name:   "disabled stable policy",
			policy: EgressPolicy{ID: "egress/test", Mode: EgressModeDisabled, Channel: "stable"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.policy.Validate(); err == nil {
				t.Fatal("unsafe egress policy was accepted")
			}
		})
	}
}
