package imagefactory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
)

const (
	pveManifestField    = "portage-engine-manifest-v1"
	pveProvenanceField  = "portage-engine-provenance"
	pveImageDigestField = "portage-engine-image"
	// qemu-server declares description.maxLength as 1024 * 8.
	maxPVEVMDescriptionBytes = 8192
)

// PVEManifestRecovery is the exact image-manifest file recovered from an
// authenticated PVE template description. RawManifest must be written without
// reformatting because ManifestDigest binds the original file bytes.
type PVEManifestRecovery struct {
	Manifest       *ImageManifest
	RawManifest    []byte
	ManifestDigest string
	VMID           int
	Node           string
}

func stampedPVEManifestDescription(manifest *ImageManifest, manifestPath string) (string, string, error) {
	if manifest == nil {
		return "", "", fmt.Errorf("image manifest is required")
	}
	raw, err := os.ReadFile(manifestPath) // #nosec G304 -- operator-provided, already validated manifest path.
	if err != nil {
		return "", "", err
	}
	fileManifest, err := loadImageManifestBytes(raw)
	if err != nil {
		return "", "", fmt.Errorf("load stamped image manifest: %w", err)
	}
	if !reflect.DeepEqual(fileManifest, manifest) {
		return "", "", fmt.Errorf("stamped image manifest file does not match the validated manifest")
	}
	digestBytes := sha256.Sum256(raw)
	manifestDigest := "sha256:" + hex.EncodeToString(digestBytes[:])
	description := fmt.Sprintf(
		"Portage Engine candidate %s | %s=%s | %s=%s | %s=%s",
		manifest.ImageID,
		pveProvenanceField, manifestDigest,
		pveImageDigestField, manifest.ImageDigest,
		pveManifestField, base64.RawURLEncoding.EncodeToString(raw),
	)
	if len(description) > maxPVEVMDescriptionBytes {
		return "", "", fmt.Errorf("recoverable PVE image stamp is %d bytes; maximum is %d", len(description), maxPVEVMDescriptionBytes)
	}
	return description, manifestDigest, nil
}

func loadImageManifestBytes(raw []byte) (*ImageManifest, error) {
	if len(raw) == 0 || int64(len(raw)) > maxDefinitionBytes {
		return nil, fmt.Errorf("image manifest size is outside the reviewed bounds")
	}
	var manifest ImageManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("definition contains trailing JSON")
	}
	return validateLoadedImageManifest(&manifest)
}

func pveDescriptionField(description, key string) (string, error) {
	prefix := key + "="
	value := ""
	matches := 0
	for _, field := range strings.Split(description, " | ") {
		if strings.HasPrefix(field, prefix) {
			value = strings.TrimPrefix(field, prefix)
			matches++
		}
	}
	if matches != 1 || value == "" {
		return "", fmt.Errorf("PVE template description requires exactly one %s field", key)
	}
	return value, nil
}

func decodePVEManifestDescription(description, expectedDigest string) (*ImageManifest, []byte, error) {
	if !prefixedSHA256Pattern.MatchString(expectedDigest) {
		return nil, nil, fmt.Errorf("expected PVE manifest digest is invalid")
	}
	provenance, err := pveDescriptionField(description, pveProvenanceField)
	if err != nil {
		return nil, nil, err
	}
	if provenance != expectedDigest {
		return nil, nil, fmt.Errorf("PVE template provenance %q does not match expected %q", provenance, expectedDigest)
	}
	encoded, err := pveDescriptionField(description, pveManifestField)
	if err != nil {
		return nil, nil, err
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, nil, fmt.Errorf("decode recoverable PVE image manifest: %w", err)
	}
	digestBytes := sha256.Sum256(raw)
	if actual := "sha256:" + hex.EncodeToString(digestBytes[:]); actual != expectedDigest {
		return nil, nil, fmt.Errorf("recoverable PVE image manifest digest %q does not match expected %q", actual, expectedDigest)
	}
	manifest, err := loadImageManifestBytes(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("validate recoverable PVE image manifest: %w", err)
	}
	imageDigest, err := pveDescriptionField(description, pveImageDigestField)
	if err != nil {
		return nil, nil, err
	}
	if imageDigest != manifest.ImageDigest {
		return nil, nil, fmt.Errorf("PVE image digest stamp does not match the recovered manifest")
	}
	return manifest, raw, nil
}

// RecoverPVEOutputManifest reads the exact, self-verifying manifest bytes from
// one uniquely named PVE template. expectedDigest must come from retained
// output-stamp evidence or another independently reviewed source.
func RecoverPVEOutputManifest(ctx context.Context, common *CommonConfig, template, expectedDigest, username, token string) (*PVEManifestRecovery, error) {
	if common == nil || template == "" || username == "" || token == "" {
		return nil, fmt.Errorf("PVE config, template, digest, and token credentials are required")
	}
	base := strings.TrimSuffix(common.ProxmoxURL, "/")
	var resources struct {
		Data []struct {
			VMID     json.Number `json:"vmid"`
			Node     string      `json:"node"`
			Name     string      `json:"name"`
			Template json.Number `json:"template"`
			Type     string      `json:"type"`
		} `json:"data"`
	}
	if err := getPVEJSON(ctx, common, base+"/cluster/resources?type=vm", username, token, &resources); err != nil {
		return nil, fmt.Errorf("query PVE candidate templates: %w", err)
	}
	vmid, node, matches := 0, "", 0
	for _, resource := range resources.Data {
		templateFlag, _ := strconv.Atoi(resource.Template.String())
		if resource.Name == template && resource.Type == "qemu" && templateFlag == 1 {
			vmid, _ = strconv.Atoi(resource.VMID.String())
			node = resource.Node
			matches++
		}
	}
	if matches != 1 || vmid < 100 || node == "" {
		return nil, fmt.Errorf("expected exactly one PVE template named %q, found %d", template, matches)
	}
	var config struct {
		Data struct {
			Name        string      `json:"name"`
			Template    json.Number `json:"template"`
			Description string      `json:"description"`
		} `json:"data"`
	}
	endpoint := fmt.Sprintf("%s/nodes/%s/qemu/%d/config", base, url.PathEscape(node), vmid)
	if err := getPVEJSON(ctx, common, endpoint, username, token, &config); err != nil {
		return nil, fmt.Errorf("query PVE candidate template config: %w", err)
	}
	templateFlag, _ := strconv.Atoi(config.Data.Template.String())
	if config.Data.Name != template || templateFlag != 1 {
		return nil, fmt.Errorf("PVE template identity changed during manifest recovery")
	}
	manifest, raw, err := decodePVEManifestDescription(config.Data.Description, expectedDigest)
	if err != nil {
		return nil, err
	}
	if manifest.Template != template {
		return nil, fmt.Errorf("recovered image manifest names template %q, expected %q", manifest.Template, template)
	}
	return &PVEManifestRecovery{Manifest: manifest, RawManifest: raw, ManifestDigest: expectedDigest, VMID: vmid, Node: node}, nil
}
