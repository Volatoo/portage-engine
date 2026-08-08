// Package distcc defines the narrow Distributed Build Alpha contract shared by
// the scheduler, disposable builders, and compile-worker authorization plane.
// It deliberately contains no Portage repository, publication, signing, or
// infrastructure-provider credentials.
package distcc

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	FallbackLocal     = "local"
	FallbackBlocked   = "blocked"
	MetadataReportKey = "distcc_report"

	OutcomeLocal    = "local"
	OutcomeRemote   = "remote"
	OutcomeHit      = "hit"
	OutcomeFallback = "fallback"

	maxCPUFeatures    = 64
	poolHashBytes     = 16
	maxEligibleAtoms  = 512
	atomOperatorChars = "!=<>~"
)

var (
	dimensionPattern      = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9+._/@:-]{0,255}$`)
	atomPattern           = regexp.MustCompile(`^[a-zA-Z0-9+_.-]+/[a-zA-Z0-9+_.-]+$`)
	atomVersionPattern    = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*[a-z]?(_(alpha|beta|pre|rc|p)[0-9]*)*(-r[0-9]+)?$`)
	compilerDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	failureReasons        = map[string]struct{}{
		"": {}, "capacity": {}, "connect": {}, "lease-expired": {},
		"lease-fenced": {}, "pool-mismatch": {}, "remote-compile": {},
		"worker-stale": {}, "policy": {}, "unknown": {},
	}
)

// Pool is a hard compatibility/routing key. Project fairness and quota are
// intentionally absent: they are decided before a compile slot is requested.
type Pool struct {
	Architecture             string   `json:"architecture"`
	CHOST                    string   `json:"chost"`
	CompilerDigest           string   `json:"compiler_digest"`
	ToolchainImageGeneration string   `json:"toolchain_image_generation"`
	CPUFeatures              []string `json:"cpu_features"`
	NetworkZone              string   `json:"network_zone"`
	ProjectTrustDomain       string   `json:"project_trust_domain"`
}

// Normalize validates a pool and returns a stable copy. CPU features are an
// exact set, not a minimum-capability expression.
func (p Pool) Normalize() (Pool, error) {
	values := []struct {
		name  string
		value string
	}{
		{"architecture", p.Architecture}, {"CHOST", p.CHOST},
		{"compiler digest", p.CompilerDigest},
		{"toolchain image generation", p.ToolchainImageGeneration},
		{"network zone", p.NetworkZone},
		{"project trust domain", p.ProjectTrustDomain},
	}
	for _, item := range values {
		item.value = strings.TrimSpace(item.value)
		if !dimensionPattern.MatchString(item.value) {
			return Pool{}, fmt.Errorf("invalid distcc %s %q", item.name, item.value)
		}
	}
	if !compilerDigestPattern.MatchString(strings.TrimSpace(p.CompilerDigest)) {
		return Pool{}, fmt.Errorf("distcc compiler digest must be sha256:<64 lowercase hex>")
	}
	if len(p.CPUFeatures) > maxCPUFeatures {
		return Pool{}, fmt.Errorf("distcc CPU feature count exceeds %d", maxCPUFeatures)
	}
	features := make([]string, 0, len(p.CPUFeatures))
	seen := make(map[string]struct{}, len(p.CPUFeatures))
	for _, feature := range p.CPUFeatures {
		feature = strings.TrimSpace(feature)
		if !dimensionPattern.MatchString(feature) {
			return Pool{}, fmt.Errorf("invalid distcc CPU feature %q", feature)
		}
		if _, exists := seen[feature]; exists {
			continue
		}
		seen[feature] = struct{}{}
		features = append(features, feature)
	}
	sort.Strings(features)
	return Pool{
		Architecture:             strings.TrimSpace(p.Architecture),
		CHOST:                    strings.TrimSpace(p.CHOST),
		CompilerDigest:           strings.TrimSpace(p.CompilerDigest),
		ToolchainImageGeneration: strings.TrimSpace(p.ToolchainImageGeneration),
		CPUFeatures:              features,
		NetworkZone:              strings.TrimSpace(p.NetworkZone),
		ProjectTrustDomain:       strings.TrimSpace(p.ProjectTrustDomain),
	}, nil
}

// ID returns a stable, non-secret identity for an exact pool.
func (p Pool) ID() (string, error) {
	normalized, err := p.Normalize()
	if err != nil {
		return "", err
	}
	parts := []string{
		normalized.Architecture, normalized.CHOST,
		normalized.CompilerDigest, normalized.ToolchainImageGeneration,
		strings.Join(normalized.CPUFeatures, ","), normalized.NetworkZone,
		normalized.ProjectTrustDomain,
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "distcc-" + hex.EncodeToString(digest[:poolHashBytes]), nil
}

// WorkerHeartbeat is inventory reported by a credential-minimal compile
// worker. The type has no place to carry repository, release, signing, PVE, or
// PBS credentials.
type WorkerHeartbeat struct {
	StableName string `json:"stable_name"`
	Endpoint   string `json:"endpoint"`
	MaxSlots   int    `json:"max_slots"`
	Pool       Pool   `json:"pool"`
}

// Validate rejects mutable or routable inventory shapes before persistence.
func (h WorkerHeartbeat) Validate() error {
	if !dimensionPattern.MatchString(strings.TrimSpace(h.StableName)) {
		return fmt.Errorf("invalid distcc worker stable name %q", h.StableName)
	}
	if h.MaxSlots < 1 || h.MaxSlots > 256 {
		return fmt.Errorf("distcc worker max slots must be between 1 and 256")
	}
	if _, err := h.Pool.Normalize(); err != nil {
		return err
	}
	return ValidateEndpoint(h.Endpoint, nil)
}

// Worker is the durable inventory projection returned by persistence.
type Worker struct {
	ID            string    `json:"id"`
	StableName    string    `json:"stable_name"`
	Endpoint      string    `json:"endpoint"`
	MaxSlots      int       `json:"max_slots"`
	PoolID        string    `json:"pool_id"`
	Pool          Pool      `json:"pool"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	NextFence     int64     `json:"next_fence"`
}

// ReservationRequest is made only after the normal project-fair scheduler has
// claimed the build. Compile-pool selection therefore cannot consume or reset
// project fairness credit.
type ReservationRequest struct {
	ProjectID              string        `json:"project_id"`
	JobID                  string        `json:"job_id"`
	AttemptID              string        `json:"attempt_id"`
	AttemptFence           int64         `json:"attempt_fence"`
	BuilderID              string        `json:"builder_id"`
	BuilderNetworkIdentity string        `json:"builder_network_identity"`
	Slots                  int           `json:"slots"`
	LeaseDuration          time.Duration `json:"-"`
	WorkerFreshness        time.Duration `json:"-"`
	Pool                   Pool          `json:"pool"`
	FallbackPolicy         string        `json:"fallback_policy"`
}

// Lease is both the scheduler result and the builder's non-secret execution
// contract. Network authorization must bind BuilderNetworkIdentity to this
// live ID/fence; distccd itself is never exposed outside the isolated network.
type Lease struct {
	ID                     string        `json:"id"`
	WorkerID               string        `json:"worker_id"`
	WorkerEndpoint         string        `json:"worker_endpoint"`
	BuilderID              string        `json:"builder_id"`
	BuilderNetworkIdentity string        `json:"builder_network_identity"`
	AttemptID              string        `json:"attempt_id"`
	AttemptFence           int64         `json:"attempt_fence"`
	Fence                  int64         `json:"fence"`
	Slots                  int           `json:"slots"`
	PoolID                 string        `json:"pool_id"`
	Pool                   Pool          `json:"pool"`
	FallbackPolicy         string        `json:"fallback_policy"`
	Pump                   bool          `json:"pump"`
	ExpiresAt              time.Time     `json:"expires_at"`
	QueueTime              time.Duration `json:"-"`
	// EligibleAtoms is the reduced category/name set the control plane decided
	// this reservation covers. Eligibility is decided once, from the request's
	// own package set, because the control plane reserves the slot while the
	// builder writes the package.env mapping: if the two re-derive it
	// independently they can disagree on atom form and silently waste the slot.
	EligibleAtoms []string `json:"eligible_atoms,omitempty"`
}

// ErrLeaseExpired is returned when a lease is well formed but its slot
// reservation has run out. It is separate from the shape errors because it is
// the one refusal a caller may be able to absorb: a reservation that ran out
// while the build waited is a scheduling delay, not a malformed request, and a
// build whose fallback policy is local can still compile.
var ErrLeaseExpired = errors.New("distcc lease is expired")

// Expired reports whether the reservation this lease describes has run out.
func (l Lease) Expired(now time.Time) bool { return !l.ExpiresAt.After(now) }

// Validate checks the complete builder-visible lease shape and that the lease
// is still live.
func (l Lease) Validate(now time.Time) error {
	if err := l.ValidateShape(); err != nil {
		return err
	}
	if l.Expired(now) {
		return ErrLeaseExpired
	}
	return nil
}

// ValidateShape checks everything about a lease except whether it is still
// live: identity, mode, fallback policy, pool binding, eligible atoms and
// endpoint. Every one of these is wrong no matter when it is read.
func (l Lease) ValidateShape() error {
	if l.ID == "" || l.WorkerID == "" || l.BuilderID == "" ||
		l.BuilderNetworkIdentity == "" || l.AttemptID == "" ||
		l.AttemptFence <= 0 || l.Fence <= 0 || l.Slots <= 0 {
		return fmt.Errorf("distcc lease identity is incomplete")
	}
	if l.Pump {
		return fmt.Errorf("distcc pump mode is forbidden")
	}
	if l.FallbackPolicy != FallbackLocal && l.FallbackPolicy != FallbackBlocked {
		return fmt.Errorf("invalid distcc fallback policy %q", l.FallbackPolicy)
	}
	poolID, err := l.Pool.ID()
	if err != nil {
		return err
	}
	if l.PoolID != poolID {
		return fmt.Errorf("distcc lease pool identity mismatch")
	}
	if len(l.EligibleAtoms) > maxEligibleAtoms {
		return fmt.Errorf("distcc lease eligible atom count exceeds %d", maxEligibleAtoms)
	}
	for _, atom := range l.EligibleAtoms {
		if !ValidAtom(atom) {
			return fmt.Errorf("invalid distcc lease eligible atom %q", atom)
		}
	}
	if err := ValidateEndpoint(l.WorkerEndpoint, nil); err != nil {
		return err
	}
	return nil
}

// Observation contains only bounded, low-cardinality dimensions. IDs are
// persisted for fencing/audit but are never metric labels.
type Observation struct {
	Outcome       string `json:"outcome"`
	FailureReason string `json:"failure_reason,omitempty"`
	NetworkBytes  int64  `json:"network_bytes"`
	QueueMillis   int64  `json:"queue_millis"`
	Count         int64  `json:"count"`
}

// Report is the bounded builder-to-scheduler telemetry envelope. The builder
// aggregates compiler-wrapper events before returning it, so no source path,
// package, worker, project, or arbitrary error text becomes a metric label.
type Report struct {
	Observations []Observation `json:"observations"`
}

// Validate rejects unbounded or malformed builder telemetry.
func (r Report) Validate() error {
	if len(r.Observations) > len(failureReasons)*4 {
		return fmt.Errorf("distcc report has too many observation groups")
	}
	for _, observation := range r.Observations {
		if err := observation.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// MetricsSnapshot is a scrape-time, low-cardinality aggregate. Pool, worker,
// project, job, package, and endpoint identities are intentionally absent.
type MetricsSnapshot struct {
	WorkersFresh     int64
	SlotsTotal       int64
	SlotsLeased      int64
	LocalCompiles    int64
	RemoteCompiles   int64
	Hits             int64
	Fallbacks        int64
	NetworkBytes     int64
	QueueMillis      int64
	FailuresByReason map[string]int64
}

func (o Observation) Validate() error {
	switch o.Outcome {
	case OutcomeLocal, OutcomeRemote, OutcomeHit, OutcomeFallback:
	default:
		return fmt.Errorf("invalid distcc observation outcome %q", o.Outcome)
	}
	if _, ok := failureReasons[o.FailureReason]; !ok {
		return fmt.Errorf("invalid distcc failure reason %q", o.FailureReason)
	}
	if o.NetworkBytes < 0 || o.QueueMillis < 0 || o.Count <= 0 {
		return fmt.Errorf("invalid distcc observation counters")
	}
	return nil
}

// ValidateEndpoint accepts only a literal IP and port. If isolatedNetworks is
// non-empty, the IP must belong to one of those operator-approved CIDRs.
func ValidateEndpoint(endpoint string, isolatedNetworks []*net.IPNet) error {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || port == "" {
		return fmt.Errorf("distcc endpoint must be an IP:port pair")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("distcc endpoint host must be a literal isolated-network IP")
	}
	if len(isolatedNetworks) == 0 {
		return nil
	}
	for _, network := range isolatedNetworks {
		if network != nil && network.Contains(ip) {
			return nil
		}
	}
	return fmt.Errorf("distcc endpoint is outside the configured isolated build networks")
}

// ValidAtom reports whether atom is a package-level category/name atom.
func ValidAtom(atom string) bool { return atomPattern.MatchString(atom) }

// NormalizeAtom reduces any dependency-atom form a client's package set may
// carry — blockers, version operators, versions, slots, repository and USE
// qualifiers — to the category/name identity distcc eligibility is decided on.
// The reviewed allowlist is package-level, so "=dev-qt/qtwebengine-6.7.2" and
// "dev-lang/python:3.11" must reduce to the same key the allowlist holds.
func NormalizeAtom(atom string) (string, bool) {
	atom = strings.TrimLeft(strings.TrimSpace(atom), atomOperatorChars)
	if index := strings.Index(atom, "["); index >= 0 {
		atom = atom[:index]
	}
	if index := strings.Index(atom, ":"); index >= 0 {
		atom = atom[:index]
	}
	atom = strings.TrimSuffix(atom, "*")
	slash := strings.Index(atom, "/")
	if slash < 0 {
		return "", false
	}
	category, name := atom[:slash], atom[slash+1:]
	// A Gentoo package name ends at the last "-" that begins a complete
	// version, so "docbook-xml-dtd-4.5" keeps its dashed name while
	// "qtwebengine-6.7.2" loses only the version.
	for index := strings.LastIndex(name, "-"); index > 0; index = strings.LastIndex(name[:index], "-") {
		if atomVersionPattern.MatchString(name[index+1:]) {
			name = name[:index]
			break
		}
	}
	reduced := category + "/" + name
	if !ValidAtom(reduced) {
		return "", false
	}
	return reduced, true
}
