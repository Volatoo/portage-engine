package imagefactory

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackerProvisionReconcilesWorldAndImageSetWithSamePolicy(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "image-factory", "packer", "scripts", "provision.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	contents := string(data)
	for _, set := range []string{"@world", "@portage-engine-image"} {
		required := "emerge --verbose --update --deep --newuse --with-bdeps=y " + set
		if !strings.Contains(contents, required) {
			t.Fatalf("Packer provision does not fully reconcile %s", set)
		}
	}
	if strings.Contains(contents, "--keep-going @portage-engine-image") {
		t.Fatal("Packer image set again uses a weaker policy than world reconciliation")
	}
}

func TestPackerSuccessorRefreshesCloneNetworkIdentity(t *testing.T) {
	t.Parallel()
	provisionData, err := os.ReadFile(filepath.Join("..", "..", "image-factory", "packer", "scripts", "provision.sh"))
	if err != nil {
		t.Fatal(err)
	}
	gateData, err := os.ReadFile(filepath.Join("..", "..", "image-factory", "packer", "scripts", "sanitize-and-gate.sh"))
	if err != nil {
		t.Fatal(err)
	}
	provision, gate := string(provisionData), string(gateData)
	for _, required := range []string{
		"portage-cloud-init-network-refresh.service",
		"After=systemd-networkd.service cloud-init-network.service",
		"ClientIdentifier=mac",
		"networkctl reload",
		"networkctl reconfigure eth0",
	} {
		if !strings.Contains(provision, required) {
			t.Fatalf("Packer successor network refresh is missing %q", required)
		}
	}
	for _, required := range []string{
		"systemctl is-enabled portage-cloud-init-network-refresh.service",
		"-name '10-cloud-init-*.network' -delete",
		"-name '10-cloud-init-*.network.d'",
	} {
		if !strings.Contains(gate, required) {
			t.Fatalf("Packer successor network gate is missing %q", required)
		}
	}
}

func TestPackerDesktopProvisionEnablesConcreteDisplayManager(t *testing.T) {
	t.Parallel()
	provisionPath := filepath.Join("..", "..", "image-factory", "packer", "scripts", "provision.sh")
	provisionData, err := os.ReadFile(provisionPath)
	if err != nil {
		t.Fatal(err)
	}
	provision := string(provisionData)
	for _, required := range []string{
		`install -d -o "$desktop_user" -g "$desktop_user" -m 0750 "/home/$desktop_user/.config"`,
		"systemctl enable lightdm.service",
		"systemctl is-enabled lightdm.service",
		"systemctl is-enabled display-manager.service",
		"systemctl set-default graphical.target",
	} {
		if !strings.Contains(provision, required) {
			t.Fatalf("Packer desktop provision is missing %q", required)
		}
	}
	if strings.Contains(provision, "systemctl enable display-manager.service") {
		t.Fatal("Packer still attempts to enable the display-manager alias instead of the concrete LightDM unit")
	}

	gatePath := filepath.Join("..", "..", "image-factory", "packer", "scripts", "sanitize-and-gate.sh")
	gateData, err := os.ReadFile(gatePath)
	if err != nil {
		t.Fatal(err)
	}
	gate := string(gateData)
	for _, required := range []string{
		`test "$(stat -c '%U:%G' /home/portage-e2e/.config)" = portage-e2e:portage-e2e`,
		"runuser --user portage-e2e -- test -w /home/portage-e2e/.config",
		"systemctl is-enabled lightdm.service",
		"systemctl is-enabled display-manager.service",
		`test "$(systemctl get-default)" = graphical.target`,
	} {
		if !strings.Contains(gate, required) {
			t.Fatalf("Packer desktop gate is missing %q", required)
		}
	}
}

func TestPackerDesktopFixturesAreProvisionedAndDigestGated(t *testing.T) {
	t.Parallel()
	provisionData, err := os.ReadFile(filepath.Join("..", "..", "image-factory", "packer", "scripts", "provision.sh"))
	if err != nil {
		t.Fatal(err)
	}
	gateData, err := os.ReadFile(filepath.Join("..", "..", "image-factory", "packer", "scripts", "sanitize-and-gate.sh"))
	if err != nil {
		t.Fatal(err)
	}
	templateData, err := os.ReadFile(filepath.Join("..", "..", "image-factory", "packer", "template.pkr.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"editor-fixture.txt", "webview-fixture.html"} {
		fixture, err := os.ReadFile(filepath.Join("..", "..", "image-factory", "desktop", "fixtures", name))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(fixture)
		for surface, contents := range map[string]string{
			"Packer template":  string(templateData),
			"provision script": string(provisionData),
			"sanitize gate":    string(gateData),
		} {
			if !strings.Contains(contents, name) {
				t.Fatalf("%s does not bind %s", surface, name)
			}
		}
		if !strings.Contains(string(gateData), hex.EncodeToString(digest[:])) {
			t.Fatalf("sanitize gate does not bind %s digest", name)
		}
	}
}
