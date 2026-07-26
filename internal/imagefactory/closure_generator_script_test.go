package imagefactory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClosureGeneratorUsesSignedStage3VDBAndIsolatedDistdir(t *testing.T) {
	t.Parallel()
	scriptPath := filepath.Join("..", "..", "image-factory", "sync", "generate-closure.sh")
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	contents := string(script)
	for _, required := range []string{
		"PROFILE_SELECTOR STAGE3_ARCHIVE MIRROR",
		`stage3_archive=$4`,
		`mount --bind /var/db/repos "${root}/var/db/repos"`,
		`mount --bind "${distdir}" "${root}/var/cache/distfiles"`,
		`exec chroot "${root}" /usr/bin/env -i`,
		`unshare --mount --propagation private`,
		`resolve --pretend --verbose --update --deep --newuse --with-bdeps=y @world`,
		`resolve --fetchonly --verbose --update --deep --newuse --with-bdeps=y "${packages[@]}"`,
		`"signed_stage3_chroot_vdb": True`,
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("closure generator lost stage3 resolver guard %q", required)
		}
	}
	for _, forbidden := range []string{
		`--emptytree`,
	} {
		if strings.Contains(contents, forbidden) {
			t.Fatalf("closure generator again depends on the wrong VDB: %q", forbidden)
		}
	}
}
