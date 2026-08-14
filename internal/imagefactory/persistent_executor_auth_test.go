package imagefactory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readPersistentExecutorFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "image-factory", "persistent-executor", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestPersistentExecutorPackerSupportsExclusivePasswordOrTokenAuth(t *testing.T) {
	template := readPersistentExecutorFile(t, "template.pkr.hcl")
	for _, required := range []string{
		`variable "proxmox_token"`,
		`variable "proxmox_password"`,
		`token                    = var.proxmox_token != "" ? var.proxmox_token : null`,
		`password                 = var.proxmox_password != "" ? var.proxmox_password : null`,
	} {
		if !strings.Contains(template, required) {
			t.Fatalf("persistent executor Packer template is missing %q", required)
		}
	}

	runner := readPersistentExecutorFile(t, "run.sh")
	for _, required := range []string{
		"set exactly one of PKR_VAR_proxmox_token or PKR_VAR_proxmox_password",
		"PVE password authentication requires user@realm without a token ID",
		"Content-Type: application/x-www-form-urlencoded",
		"--data-binary @-",
		"Cookie: PVEAuthCookie=",
	} {
		if !strings.Contains(runner, required) {
			t.Fatalf("persistent executor runner is missing %q", required)
		}
	}
	if strings.Contains(runner, `--data-urlencode "password=`) {
		t.Fatal("PVE password is exposed in curl process arguments")
	}
}

func TestPersistentExecutorPackerUsesEphemeralSSHKeyByDefault(t *testing.T) {
	template := readPersistentExecutorFile(t, "template.pkr.hcl")
	for _, required := range []string{
		`variable "ssh_private_key_file"`,
		`ssh_private_key_file = var.ssh_private_key_file != "" ? var.ssh_private_key_file : null`,
	} {
		if !strings.Contains(template, required) {
			t.Fatalf("persistent executor Packer template is missing %q", required)
		}
	}

	runner := readPersistentExecutorFile(t, "run.sh")
	if !strings.Contains(runner, `if [[ -n $ssh_private_key_file ]]; then`) {
		t.Fatal("persistent executor runner still requires an operator SSH key")
	}
	if !strings.Contains(runner, "SSH private key must be a regular non-symlink file") {
		t.Fatal("optional operator SSH key lost its regular-file boundary")
	}
}

func TestPersistentExecutorRunnerUsesDeclaredProviderArtifactVariable(t *testing.T) {
	runner := readPersistentExecutorFile(t, "run.sh")
	if !strings.Contains(runner,
		"PKR_VAR_terraform_proxmox_provider PKR_VAR_terraform_proxmox_provider_sha256") {
		t.Fatal("persistent executor runner does not verify the declared Terraform provider artifact")
	}
	if strings.Contains(runner, "PKR_VAR_terraform_proxmox_provider_binary") {
		t.Fatal("persistent executor runner reconstructs a nonexistent provider variable")
	}
}

func TestPersistentExecutorImageRejectsUnboundEgressCapability(t *testing.T) {
	for _, name := range []string{"run.sh", "provision.sh"} {
		contents := readPersistentExecutorFile(t, name)
		for _, required := range []string{
			"egress:[a-zA-Z0-9][a-zA-Z0-9+._/-]*@sha256:[a-f0-9]{64}",
		} {
			if !strings.Contains(contents, required) {
				t.Fatalf("%s does not fail closed on an unbound egress capability", name)
			}
		}
	}
	runner := readPersistentExecutorFile(t, "run.sh")
	if !strings.Contains(runner,
		"egress capability must bind policy ID and digest as egress:<id>@sha256:<hex>") {
		t.Fatal("persistent executor runner does not explain the egress binding failure")
	}
	sanitizer := readPersistentExecutorFile(t, "sanitize-and-gate.sh")
	if !strings.Contains(sanitizer, `grep -Fq ",${PE_EGRESS_CAPABILITY}"`) {
		t.Fatal("persistent executor sanitizer does not bind the installed egress capability")
	}
}

func TestPersistentExecutorTemplateRefreshesCloneNetworkAfterCloudInit(t *testing.T) {
	provision := readPersistentExecutorFile(t, "provision.sh")
	for _, required := range []string{
		"portage-cloud-init-network-refresh.service",
		"After=systemd-networkd.service cloud-init-network.service",
		"Before=portage-capacity-executor.service",
		"ClientIdentifier=mac",
		"networkctl reconfigure eth0",
	} {
		if !strings.Contains(provision, required) {
			t.Fatalf("persistent executor provisioner is missing %q", required)
		}
	}

	sanitizer := readPersistentExecutorFile(t, "sanitize-and-gate.sh")
	if !strings.Contains(sanitizer, "-name '10-cloud-init-*.network' -delete") {
		t.Fatal("persistent executor sanitizer seals a Packer MAC-specific network file")
	}
	if !strings.Contains(sanitizer,
		"systemctl is-enabled portage-cloud-init-network-refresh.service") {
		t.Fatal("persistent executor gate does not enforce the clone network refresh unit")
	}
}

func TestPersistentExecutorServiceExportsRuntimeCredentialEnvironment(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "systemd",
		"portage-capacity-executor.service")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	service := string(data)
	if !strings.Contains(service,
		"EnvironmentFile=/etc/portage-engine/executor.conf") {
		t.Fatal("persistent executor service does not export runtime credentials to SDK chains")
	}
	if strings.Index(service, "EnvironmentFile=/etc/portage-engine/executor.conf") >
		strings.Index(service, "ExecStart=/usr/local/bin/portage-server") {
		t.Fatal("runtime environment is loaded after the executor starts")
	}
}
