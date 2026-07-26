package imagefactory

import (
	"os"
	"strings"
	"testing"
)

func TestPackerBootstrapVerifiesIndependentBundleSignature(t *testing.T) {
	contents, err := os.ReadFile("../../image-factory/bootstrap-offline.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(contents)
	for _, required := range []string{
		`APPROVED_FACTORY_SHA256`,
		`actual_factory_sha256`,
		`bundle-verify`,
		`-signature "${bundle_signature}"`,
		`-public-key "${sync_public_key}"`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("trusted Packer bootstrap is missing %q", required)
		}
	}
	if strings.Index(script, `bundle-verify`) > strings.Index(script, `python3 - "${target}"`) {
		t.Fatal("trusted Packer bootstrap reads execution metadata before verifying the signed bundle")
	}
}
