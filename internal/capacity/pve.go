package capacity

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/slchris/portage-engine/internal/builder"
	"github.com/slchris/portage-engine/internal/catalog"
	"github.com/slchris/portage-engine/internal/iac"
)

const PVEExecutorBootstrapDMIV1 = "pve-dmi-v1"

// PVEExecutorTemplate is an operator allowlist entry. It is intentionally
// separate from a disposable job-builder image: the template must start a
// listener-free persistent executor and derive its capacity UUID from the
// exact PVE/DMI product name portage-capacity-<uuid>.
type PVEExecutorTemplate struct {
	ImageID           string               `json:"image_id"`
	ImageGeneration   string               `json:"image_generation"`
	Template          string               `json:"template"`
	BootstrapContract string               `json:"bootstrap_contract"`
	Spec              map[string]string    `json:"spec,omitempty"`
	EgressPolicy      catalog.EgressPolicy `json:"egress_policy"`
}

type PVEProviderConfig struct {
	Endpoint     string
	Insecure     bool
	Node         string
	Nodes        []string
	Storage      string
	Network      string
	WorkspaceDir string
	Credentials  *iac.CloudCredentials
	Templates    []PVEExecutorTemplate
}

type PVEProvider struct {
	manager     *iac.Manager
	config      PVEProviderConfig
	templates   map[string]PVEExecutorTemplate
	credentials *iac.CloudCredentials
}

func pveTemplateKey(imageID, generation string) string {
	return strings.TrimSpace(imageID) + "\x00" + strings.TrimSpace(generation)
}

func NewPVEProvider(config PVEProviderConfig) (*PVEProvider, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" ||
		endpoint.User != nil || endpoint.RawQuery != "" ||
		endpoint.Fragment != "" {
		return nil, fmt.Errorf("capacity PVE endpoint must be an HTTPS URL without credentials, query, or fragment")
	}
	if !filepath.IsAbs(config.WorkspaceDir) {
		return nil, fmt.Errorf("capacity PVE workspace directory must be absolute")
	}
	if config.Credentials == nil {
		return nil, fmt.Errorf("capacity PVE credentials are required")
	}
	tokenConfigured := config.Credentials.PVETokenID != "" ||
		config.Credentials.PVETokenSecret != ""
	passwordConfigured := config.Credentials.PVEUsername != "" ||
		config.Credentials.PVEPassword != ""
	if (tokenConfigured && (config.Credentials.PVETokenID == "" ||
		config.Credentials.PVETokenSecret == "")) ||
		(passwordConfigured && (config.Credentials.PVEUsername == "" ||
			config.Credentials.PVEPassword == "")) ||
		(!tokenConfigured && !passwordConfigured) {
		return nil, fmt.Errorf("capacity PVE credentials require a complete token or username/password pair")
	}
	templates := make(map[string]PVEExecutorTemplate, len(config.Templates))
	for _, template := range config.Templates {
		if err := validatePVEExecutorTemplate(template); err != nil {
			return nil, err
		}
		key := pveTemplateKey(template.ImageID, template.ImageGeneration)
		if _, exists := templates[key]; exists {
			return nil, fmt.Errorf(
				"duplicate capacity executor template for %s@%s",
				template.ImageID, template.ImageGeneration,
			)
		}
		templates[key] = template
	}
	if len(templates) == 0 {
		return nil, fmt.Errorf("capacity PVE template allowlist is empty")
	}
	manager := iac.NewManager(iac.WithWorkspaceDir(config.WorkspaceDir))
	credentials := *config.Credentials
	manager.SetCredentialResolver(func(provider string) *iac.CloudCredentials {
		if provider != "pve" {
			return nil
		}
		copy := credentials
		return &copy
	})
	return &PVEProvider{
		manager: manager, config: config, templates: templates,
		credentials: &credentials,
	}, nil
}

func validatePVEExecutorTemplate(template PVEExecutorTemplate) error {
	if strings.TrimSpace(template.ImageID) == "" ||
		strings.TrimSpace(template.ImageGeneration) == "" ||
		strings.TrimSpace(template.Template) == "" {
		return fmt.Errorf("capacity PVE template identity is incomplete")
	}
	if template.BootstrapContract != PVEExecutorBootstrapDMIV1 {
		return fmt.Errorf(
			"capacity PVE template %s@%s must use bootstrap contract %s",
			template.ImageID, template.ImageGeneration,
			PVEExecutorBootstrapDMIV1,
		)
	}
	if err := validatePVEBootstrapCICustom(template.Spec["cicustom"]); err != nil {
		return fmt.Errorf(
			"capacity PVE template %s@%s bootstrap: %w",
			template.ImageID, template.ImageGeneration, err,
		)
	}
	for _, forbidden := range []string{
		"resource_name", "name", "vmid", "template", "endpoint", "insecure",
		"smbios_uuid", "start_stopped", "tags", "ssh_keys",
	} {
		if _, exists := template.Spec[forbidden]; exists {
			return fmt.Errorf(
				"capacity PVE template spec must not override %q",
				forbidden,
			)
		}
	}
	if template.EgressPolicy.Mode != catalog.EgressModeEnforce {
		return fmt.Errorf(
			"capacity PVE template %s@%s requires enforced egress policy",
			template.ImageID, template.ImageGeneration,
		)
	}
	if _, err := template.EgressPolicy.Digest(); err != nil {
		return fmt.Errorf("capacity PVE template egress policy: %w", err)
	}
	return nil
}

// validatePVEBootstrapCICustom closes the gap between an image's intentionally
// secret-free state and the first executor heartbeat. The pve-dmi-v1 provider
// has no SSH deployment step, so custom user-data is the configured path that
// provisions executor.conf and its referenced credentials before systemd
// starts the service.
func validatePVEBootstrapCICustom(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf(
			"spec.cicustom must define user=<shared-storage>:snippets/<file>",
		)
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') ||
			(char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') ||
			strings.ContainsRune("._-/,:=", char) {
			continue
		}
		return fmt.Errorf("spec.cicustom contains an invalid character")
	}
	foundUser := false
	for _, component := range strings.Split(value, ",") {
		parts := strings.SplitN(component, "=", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("spec.cicustom contains an invalid component")
		}
		switch parts[0] {
		case "user", "vendor", "network", "meta":
		default:
			return fmt.Errorf("spec.cicustom contains unsupported key %q", parts[0])
		}
		if !validPVESnippetRef(parts[1]) {
			return fmt.Errorf("spec.cicustom %s value must be <storage>:snippets/<file>", parts[0])
		}
		if parts[0] == "user" {
			if foundUser {
				return fmt.Errorf("spec.cicustom defines user data more than once")
			}
			foundUser = true
		}
	}
	if !foundUser {
		return fmt.Errorf(
			"spec.cicustom must define user=<shared-storage>:snippets/<file>",
		)
	}
	return nil
}

func validPVESnippetRef(value string) bool {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 || parts[0] == "" ||
		!strings.HasPrefix(parts[1], "snippets/") {
		return false
	}
	for _, char := range parts[0] {
		if (char >= 'a' && char <= 'z') ||
			(char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("._-", char) {
			continue
		}
		return false
	}
	name := strings.TrimPrefix(parts[1], "snippets/")
	return name != "" && name != "." && name != ".." &&
		!strings.HasPrefix(name, "/") && !strings.HasPrefix(name, "../") &&
		path.Clean(name) == name
}

func (p *PVEProvider) provisionSpec(
	template PVEExecutorTemplate,
	instance *builder.CapacityInstance,
) map[string]string {
	spec := maps.Clone(template.Spec)
	if spec == nil {
		spec = make(map[string]string)
	}
	spec["endpoint"] = p.config.Endpoint
	spec["insecure"] = fmt.Sprintf("%t", p.config.Insecure)
	spec["resource_name"] = instance.ProviderInstanceID
	spec["smbios_uuid"] = instance.ID
	spec["template"] = template.Template
	spec["start_stopped"] = "true"
	spec["tags"] = "portage-capacity,persistent-executor"
	if p.config.Node != "" {
		spec["node"] = p.config.Node
	}
	if len(p.config.Nodes) > 0 {
		spec["nodes"] = strings.Join(p.config.Nodes, ",")
	}
	if p.config.Storage != "" {
		spec["storage"] = p.config.Storage
	}
	if p.config.Network != "" {
		spec["network"] = p.config.Network
	}
	return spec
}

func (p *PVEProvider) Provision(
	ctx context.Context,
	claim *builder.CapacityActionClaim,
	instance *builder.CapacityInstance,
) (ProvisionResult, error) {
	if err := validatePVEInstanceIdentity(claim, instance); err != nil {
		return ProvisionResult{}, err
	}
	template, ok := p.templates[pveTemplateKey(
		claim.Pool.ImageID, claim.Pool.ImageGeneration,
	)]
	if !ok {
		return ProvisionResult{}, fmt.Errorf(
			"no approved persistent executor template for %s@%s",
			claim.Pool.ImageID, claim.Pool.ImageGeneration,
		)
	}
	if err := ctx.Err(); err != nil {
		return ProvisionResult{}, err
	}
	spec := p.provisionSpec(template, instance)
	provisioned, err := p.manager.Provision(&iac.ProvisionRequest{
		InstanceID:   instance.ProviderInstanceID,
		Provider:     "pve",
		Arch:         claim.Pool.Arch,
		Spec:         spec,
		Credentials:  p.credentials,
		SSH:          &iac.SSHConfig{User: "root"},
		BuildMode:    "native-gentoo",
		EgressPolicy: &template.EgressPolicy,
	})
	if err != nil {
		return ProvisionResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ProvisionResult{}, err
	}
	attributes := map[string]string{
		"bootstrap_contract": template.BootstrapContract,
		"image_id":           template.ImageID,
		"image_generation":   template.ImageGeneration,
		"ip_address":         provisioned.IPAddress,
	}
	for _, key := range []string{"node", "vmid", "pe_egress_enforced"} {
		if value := provisioned.Metadata[key]; value != "" {
			attributes[key] = value
		}
	}
	return ProvisionResult{
		RemoteStateRef: provisioned.TerraformDir,
		Attributes:     attributes,
	}, nil
}

func (p *PVEProvider) Delete(
	ctx context.Context,
	instance *builder.CapacityInstance,
) error {
	if err := validatePVEInstanceIdentity(nil, instance); err != nil {
		return err
	}
	if strings.TrimSpace(instance.RemoteStateRef) == "" {
		return fmt.Errorf("capacity instance has no recorded Terraform state")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Stat(instance.RemoteStateRef); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect capacity Terraform state: %w", err)
		}
		// Deletion may have succeeded and the process may have died after local
		// state cleanup but before PostgreSQL completion. Re-verify only the
		// exact database-owned name through PVE before acknowledging replay.
		return iac.EnsurePVEVMAbsent(
			ctx, p.config.Endpoint, iac.PVEAuth{
				TokenID:     p.credentials.PVETokenID,
				TokenSecret: p.credentials.PVETokenSecret,
				Username:    p.credentials.PVEUsername,
				Password:    p.credentials.PVEPassword,
				Insecure:    p.config.Insecure,
			}, instance.Attributes["node"], instance.ProviderInstanceID,
		)
	}
	if err := p.manager.DestroyRecorded(
		"pve", instance.ProviderInstanceID, instance.RemoteStateRef,
	); err != nil {
		return err
	}
	if err := p.manager.RemoveRecordedWorkspace(instance.RemoteStateRef); err != nil {
		return err
	}
	return ctx.Err()
}

func validatePVEInstanceIdentity(
	claim *builder.CapacityActionClaim,
	instance *builder.CapacityInstance,
) error {
	if instance == nil || instance.Provider != "pve" ||
		strings.TrimSpace(instance.ID) == "" ||
		instance.ProviderInstanceID != "portage-capacity-"+instance.ID {
		return fmt.Errorf("capacity PVE instance identity is not exact")
	}
	if claim != nil && (claim.Pool.Provider != "pve" ||
		claim.Pool.ID != instance.PoolID) {
		return fmt.Errorf("capacity PVE claim and instance do not share a pool")
	}
	return nil
}
