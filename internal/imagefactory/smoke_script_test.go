package imagefactory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSmokePinsGuestSSHHostKeyThroughPVEQGA(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "image-factory", "smoke-offline.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	contents := string(data)
	for _, required := range []string{
		`guest-host-key -common "$common_config" -node "$node" -vmid "$vmid"`,
		`ssh-keygen -lf "$work_dir/known_hosts"`,
		`-o StrictHostKeyChecking=yes`,
		`__PORTAGE_ENGINE_OFFLINE_TERRAFORM_PROVIDERS__`,
		`terraform_cli_config="$work_dir/terraform.rc"`,
		`apply_started=0`,
		`pre-apply-cleanup.log`,
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("smoke runner lost SSH identity binding %q", required)
		}
	}
	if strings.Contains(contents, "StrictHostKeyChecking=accept-new") {
		t.Fatal("smoke runner again accepts an unauthenticated first SSH host key")
	}
}
