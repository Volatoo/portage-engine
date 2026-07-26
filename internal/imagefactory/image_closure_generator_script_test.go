package imagefactory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageClosureGeneratorBindsAcceptedBaseManifestAndVDB(t *testing.T) {
	t.Parallel()
	scriptPath := filepath.Join("..", "..", "image-factory", "sync", "generate-image-closure.sh")
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	contents := string(script)
	for _, required := range []string{
		"PROFILE_SELECTOR PROFILE_REPOSITORY_COMMIT BASE_IMAGE_MANIFEST MIRROR",
		`profile_repository_commit=$4`,
		`base_image_manifest=$5`,
		`manifest["target"] != "base-systemd"`,
		`"base_image_manifest_sha256"`,
		`"accepted_base_clone_vdb": True`,
		`"vdb_sha256": tree_digest(pathlib.Path(sys.argv[2]))`,
		`"profile_repository_commit": sys.argv[5]`,
		`"profile_repository_tree": sys.argv[6]`,
		`git -C /var/db/repos/gentoo rev-parse HEAD`,
		`git -C /var/db/repos/gentoo status --porcelain --untracked-files=no`,
		`git -C "${profile_repository_root}" rev-parse HEAD`,
		`git -C "${profile_repository_root}" status --porcelain --untracked-files=no`,
		`git -C "${profile_repository_root}" rev-parse 'HEAD^{tree}'`,
		`PORTAGE_CONFIGROOT="${config_root}"`,
		`resolve --fetchonly --verbose --update --deep --newuse --with-bdeps=y @world`,
		`resolve --fetchonly --verbose --with-bdeps=y "${packages[@]}"`,
		`unshare --mount --propagation private`,
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("image closure generator lost accepted-base guard %q", required)
		}
	}
	for _, forbidden := range []string{
		`--emptytree`,
		`stage3_archive`,
		`rm -rf -- /var/db/pkg`,
		`metadata/timestamp.commit`,
	} {
		if strings.Contains(contents, forbidden) {
			t.Fatalf("image closure generator no longer resolves from the accepted base VDB: %q", forbidden)
		}
	}
}
