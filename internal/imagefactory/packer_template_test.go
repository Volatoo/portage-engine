package imagefactory

import (
	"os"
	"strings"
	"testing"
)

func TestPackerTemplateMarksRunningImageBuilds(t *testing.T) {
	contents, err := os.ReadFile("../../image-factory/packer/template.pkr.hcl")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "portage-engine;image-factory-build;candidate;") {
		t.Fatal("Packer build VM is missing the image-factory-build scheduling tag")
	}
}

func TestPackerTemplatePinsReviewedDisplayModel(t *testing.T) {
	contents, err := os.ReadFile("../../image-factory/packer/template.pkr.hcl")
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	if !strings.Contains(text, `variable "display_model"`) || !strings.Contains(text, "type   = var.display_model") || !strings.Contains(text, `PE_DISPLAY_MODEL=${var.display_model}`) || !strings.Contains(text, "display_model               = var.display_model") {
		t.Fatal("Packer display hardware is not bound to the plan and manifest")
	}
}
