package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/slchris/portage-engine/internal/catalog"
)

const (
	maxExecutorCapabilities = 4096
	defaultExecutionZone    = "default"
	capacityPoolHashBytes   = 12
)

var executorCapabilityPattern = regexp.MustCompile(
	`^[a-z][a-z0-9-]{0,31}:[a-zA-Z0-9][a-zA-Z0-9+._/@:-]{0,479}$`,
)

// SchedulerCapacityPoolDefinition is one immutable executor environment from
// the server-owned catalog. It identifies scheduling capacity, not a per-job
// disposable builder VM.
type SchedulerCapacityPoolDefinition struct {
	ID              string   `json:"id"`
	Provider        string   `json:"provider"`
	ExecutionZone   string   `json:"execution_zone"`
	Arch            string   `json:"arch"`
	BuildMode       string   `json:"build_mode"`
	ProfileID       string   `json:"profile_id"`
	ImageID         string   `json:"image_id"`
	ImageGeneration string   `json:"image_generation"`
	Selector        []string `json:"selector"`
}

// CapacityPoolID returns a short stable identity for the exact catalog
// environment. The readable dimensions remain in the durable pool record;
// hashing keeps the capability label bounded even when catalog IDs are long.
func CapacityPoolID(resolved *catalog.ResolvedBuildContext) (string, error) {
	if resolved == nil {
		return "", fmt.Errorf("resolved build context is required")
	}
	zone := strings.TrimSpace(resolved.ExecutionZone)
	if zone == "" {
		zone = defaultExecutionZone
	}
	parts := []string{
		strings.TrimSpace(resolved.Provider),
		zone,
		strings.TrimSpace(resolved.Arch),
		strings.TrimSpace(resolved.BuildMode),
		strings.TrimSpace(resolved.ProfileID),
		strings.TrimSpace(resolved.ImageID),
		strings.TrimSpace(resolved.ImageGeneration),
	}
	for _, part := range parts {
		if part == "" {
			return "", fmt.Errorf("capacity pool dimensions are incomplete")
		}
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return strings.Join(parts[:3], "-") + "-" +
		hex.EncodeToString(digest[:capacityPoolHashBytes]), nil
}

func capacityPoolDefinition(
	resolved *catalog.ResolvedBuildContext,
) (SchedulerCapacityPoolDefinition, error) {
	id, err := CapacityPoolID(resolved)
	if err != nil {
		return SchedulerCapacityPoolDefinition{}, err
	}
	zone := strings.TrimSpace(resolved.ExecutionZone)
	if zone == "" {
		zone = defaultExecutionZone
	}
	selector, err := NormalizeExecutorCapabilities([]string{
		"capacity-pool:" + id,
		"provider:" + resolved.Provider,
		"zone:" + zone,
		"arch:" + resolved.Arch,
		"build-mode:" + resolved.BuildMode,
		"profile:" + resolved.ProfileID,
		"image:" + resolved.ImageID + "@" + resolved.ImageGeneration,
	})
	if err != nil {
		return SchedulerCapacityPoolDefinition{}, err
	}
	return SchedulerCapacityPoolDefinition{
		ID: id, Provider: resolved.Provider, ExecutionZone: zone,
		Arch: resolved.Arch, BuildMode: resolved.BuildMode,
		ProfileID: resolved.ProfileID, ImageID: resolved.ImageID,
		ImageGeneration: resolved.ImageGeneration, Selector: selector,
	}, nil
}

// NormalizeExecutorCapabilities validates, deduplicates and sorts capability
// labels so their JSON representation and database comparison are stable.
func NormalizeExecutorCapabilities(labels []string) ([]string, error) {
	if len(labels) == 0 || len(labels) > maxExecutorCapabilities {
		return nil, fmt.Errorf("executor capabilities must contain 1..%d labels", maxExecutorCapabilities)
	}
	seen := make(map[string]struct{}, len(labels))
	normalized := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if !executorCapabilityPattern.MatchString(label) {
			return nil, fmt.Errorf("invalid executor capability %q", label)
		}
		if _, exists := seen[label]; exists {
			continue
		}
		seen[label] = struct{}{}
		normalized = append(normalized, label)
	}
	sort.Strings(normalized)
	return normalized, nil
}

// PhaseCapabilityRequirements binds one phase to the exact catalog-resolved
// environment. A claimant must advertise every returned label; there is no
// fallback to a similar architecture, profile, image, provider or zone.
func PhaseCapabilityRequirements(request *BuildRequest, phase string) ([]string, error) {
	switch phase {
	case "provision", "build", "verify", "publish":
	default:
		return nil, fmt.Errorf("unsupported executor phase %q", phase)
	}
	if request == nil {
		return nil, fmt.Errorf("build request is required for capability routing")
	}
	if request.ResolvedContext == nil {
		return NormalizeExecutorCapabilities([]string{
			"phase:" + phase,
			"context:legacy",
		})
	}
	resolved := request.ResolvedContext
	zone := strings.TrimSpace(resolved.ExecutionZone)
	if zone == "" {
		zone = defaultExecutionZone
	}
	poolID, err := CapacityPoolID(resolved)
	if err != nil {
		return nil, err
	}
	labels := []string{
		"phase:" + phase,
		"capacity-pool:" + poolID,
		"zone:" + zone,
		"provider:" + resolved.Provider,
		"arch:" + resolved.Arch,
		"build-mode:" + resolved.BuildMode,
		"profile:" + resolved.ProfileID,
		"image:" + resolved.ImageID + "@" + resolved.ImageGeneration,
	}
	if resolved.EgressPolicy.ID != "" && resolved.EgressPolicyDigest != "" {
		labels = append(labels,
			"egress:"+resolved.EgressPolicy.ID+"@"+resolved.EgressPolicyDigest,
		)
	}
	return NormalizeExecutorCapabilities(labels)
}

func (m *Manager) resolveCapacityPools() ([]SchedulerCapacityPoolDefinition, error) {
	buildCatalog := m.BuildCatalog()
	if buildCatalog == nil {
		return nil, fmt.Errorf("build catalog is unavailable")
	}
	provider := strings.TrimSpace(m.CloudSettings().Provider)
	if provider == "" {
		provider = strings.TrimSpace(m.config.CloudProvider)
	}
	zones := make(map[string]struct{}, len(m.config.ExecutorZones))
	for _, zone := range m.config.ExecutorZones {
		zone = strings.TrimSpace(zone)
		if zone != "" {
			zones[zone] = struct{}{}
		}
	}
	if len(zones) == 0 {
		zones[defaultExecutionZone] = struct{}{}
	}
	seen := make(map[string]struct{})
	pools := make([]SchedulerCapacityPoolDefinition, 0, len(buildCatalog.Profiles))
	for _, profile := range buildCatalog.Profiles {
		resolved, err := buildCatalog.Resolve(catalog.ResolveRequest{
			ProfileID: profile.ID,
		})
		if err != nil || resolved.Provider != provider {
			continue
		}
		zone := strings.TrimSpace(resolved.ExecutionZone)
		if zone == "" {
			zone = defaultExecutionZone
		}
		if _, supported := zones[zone]; !supported {
			continue
		}
		pool, err := capacityPoolDefinition(resolved)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[pool.ID]; exists {
			continue
		}
		seen[pool.ID] = struct{}{}
		pools = append(pools, pool)
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].ID < pools[j].ID })
	if len(pools) == 0 {
		return nil, fmt.Errorf(
			"no capacity pool matches provider %q and executor zones %v",
			provider, m.config.ExecutorZones,
		)
	}
	return pools, nil
}

func (m *Manager) resolveExecutorCapabilities() ([]string, error) {
	if len(m.config.ExecutorCapabilities) > 0 {
		return m.bindCapacityInstance(m.config.ExecutorCapabilities)
	}
	buildCatalog := m.BuildCatalog()
	if buildCatalog == nil {
		return nil, fmt.Errorf("build catalog is unavailable")
	}
	provider := strings.TrimSpace(m.CloudSettings().Provider)
	if provider == "" {
		provider = strings.TrimSpace(m.config.CloudProvider)
	}
	zones := make(map[string]struct{}, len(m.config.ExecutorZones))
	for _, zone := range m.config.ExecutorZones {
		zone = strings.TrimSpace(zone)
		if zone != "" {
			zones[zone] = struct{}{}
		}
	}
	if len(zones) == 0 {
		zones[defaultExecutionZone] = struct{}{}
	}

	var labels []string
	for _, profile := range buildCatalog.Profiles {
		resolved, err := buildCatalog.Resolve(catalog.ResolveRequest{ProfileID: profile.ID})
		if err != nil || resolved.Provider != provider {
			continue
		}
		zone := strings.TrimSpace(resolved.ExecutionZone)
		if zone == "" {
			zone = defaultExecutionZone
		}
		if _, supported := zones[zone]; !supported {
			continue
		}
		request := &BuildRequest{ResolvedContext: resolved}
		for _, phase := range []string{"provision", "build", "verify", "publish"} {
			required, requirementErr := PhaseCapabilityRequirements(request, phase)
			if requirementErr != nil {
				return nil, requirementErr
			}
			labels = append(labels, required...)
		}
	}
	if len(labels) == 0 {
		return nil, fmt.Errorf(
			"no catalog profile matches provider %q and executor zones %v",
			provider, m.config.ExecutorZones,
		)
	}
	return m.bindCapacityInstance(labels)
}

func workerKindCapabilities(labels []string, kind string) []string {
	result := append([]string(nil), labels...)
	result = append(result, "worker-kind:"+kind)
	normalized, err := NormalizeExecutorCapabilities(result)
	if err != nil {
		// kind is a compile-time constant and labels were already normalized.
		// Returning the original set fails soft without weakening hard routing.
		return labels
	}
	return normalized
}

func (m *Manager) bindCapacityInstance(labels []string) ([]string, error) {
	if instanceID := strings.TrimSpace(
		m.config.ExecutorCapacityInstanceID,
	); instanceID != "" {
		labels = append(
			append([]string(nil), labels...),
			"capacity-instance:"+instanceID,
		)
	}
	return NormalizeExecutorCapabilities(labels)
}
