package distcc

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// BuilderPolicy is the disposable builder's defense-in-depth policy. The
// scheduler allowlist alone cannot turn an arbitrary package or endpoint into
// an eligible remote compilation target.
type BuilderPolicy struct {
	Enabled          bool
	Allowlist        []string
	IsolatedNetworks []*net.IPNet
	Architecture     string
	CHOST            string
	CompilerDigest   string
	ImageGeneration  string
	CPUFeatures      []string
	NetworkZone      string
	allowed          map[string]struct{}
}

// Allowed reports whether atom is present in the normalized reviewed list.
func (p BuilderPolicy) Allowed(atom string) bool {
	_, ok := p.allowed[atom]
	return p.Enabled && ok
}

// NewBuilderPolicy validates and normalizes operator configuration.
//
// The allowlist is reduced with NormalizeAtom, the same reduction the control
// plane applies to the same operator list in distCCEligibleAtoms. It demanded
// ValidAtom instead — the bare category/name form only — so an operator writing
// a dependency atom they are entitled to write, `>=dev-qt/qtwebengine-6.7.2`,
// got a builder that logged one line and then ran with distcc off, while the
// control plane went on reserving compile slots for every build in the pool.
// Eligibility is decided at package level on both sides, so both sides have to
// reduce to it the same way; a form only one of them accepts is a disagreement
// rather than a policy.
func NewBuilderPolicy(policy BuilderPolicy) (BuilderPolicy, error) {
	allowed := make(map[string]struct{}, len(policy.Allowlist))
	allowlist := make([]string, 0, len(policy.Allowlist))
	for _, entry := range policy.Allowlist {
		atom, ok := NormalizeAtom(entry)
		if !ok {
			return BuilderPolicy{}, fmt.Errorf("invalid distcc allowlist atom %q", entry)
		}
		if _, exists := allowed[atom]; exists {
			continue
		}
		allowed[atom] = struct{}{}
		allowlist = append(allowlist, atom)
	}
	sort.Strings(allowlist)
	features := append([]string(nil), policy.CPUFeatures...)
	sort.Strings(features)
	policy.Allowlist = allowlist
	policy.CPUFeatures = features
	policy.allowed = allowed
	if policy.Enabled && (len(allowlist) == 0 || len(policy.IsolatedNetworks) == 0) {
		return BuilderPolicy{}, fmt.Errorf("enabled distcc requires a reviewed package allowlist and isolated network CIDR")
	}
	return policy, nil
}

// Authorize returns nil only for an exact, live, reviewed C/C++ package
// contract. Small packages and non-C/C++ ecosystems stay local by being absent
// from the reviewed allowlist.
func (p BuilderPolicy) Authorize(
	atom, projectTrustDomain, attemptID string,
	attemptFence int64,
	lease *Lease,
	now time.Time,
) error {
	if !p.Enabled {
		return fmt.Errorf("distcc is disabled on this builder")
	}
	if _, ok := p.allowed[atom]; !ok {
		return fmt.Errorf("package %q is not in the reviewed distcc C/C++ allowlist", atom)
	}
	if lease == nil {
		return fmt.Errorf("distcc lease is required")
	}
	if err := lease.Validate(now); err != nil {
		return err
	}
	pool, err := lease.Pool.Normalize()
	if err != nil {
		return err
	}
	features := append([]string(nil), p.CPUFeatures...)
	sort.Strings(features)
	if pool.Architecture != p.Architecture || pool.CHOST != p.CHOST ||
		pool.CompilerDigest != p.CompilerDigest ||
		pool.ToolchainImageGeneration != p.ImageGeneration ||
		pool.NetworkZone != p.NetworkZone ||
		strings.Join(pool.CPUFeatures, "\x00") != strings.Join(features, "\x00") {
		return fmt.Errorf("distcc lease does not exactly match the builder toolchain pool")
	}
	if projectTrustDomain == "" || pool.ProjectTrustDomain != projectTrustDomain {
		return fmt.Errorf("distcc lease crosses the claimed project trust domain")
	}
	if attemptID == "" || attemptFence <= 0 ||
		lease.AttemptID != attemptID || lease.AttemptFence != attemptFence {
		return fmt.Errorf("distcc lease crosses the claimed build attempt fence")
	}
	return ValidateEndpoint(lease.WorkerEndpoint, p.IsolatedNetworks)
}
