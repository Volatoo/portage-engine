package imagefactory

import (
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
