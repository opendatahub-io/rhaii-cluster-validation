package config

import (
	"fmt"
	"os"
	"strings"

	imagereferences "github.com/opendatahub-io/rhaii-cluster-validation/manifests/image-references"
	"gopkg.in/yaml.v3"
)

const (
	// EnvValidatorImage is the environment variable for overriding the validator container image.
	EnvValidatorImage = "RELATED_IMAGE_RHAII_CLUSTER_VALIDATOR"
	// EnvToolsImage is the environment variable for overriding the tools container image.
	EnvToolsImage = "RELATED_IMAGE_RHAII_VALIDATOR_TOOLS"
)

// imageReferencesManifest mirrors the YAML structure in image-references.yaml.
type imageReferencesManifest struct {
	RelatedImages []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	} `yaml:"relatedImages"`
}

// ResolveImages returns the validator and tools container images by:
// 1. Loading embedded defaults from image-references.yaml
// 2. Overriding with environment variables if set
func ResolveImages() (validatorImage, toolsImage string, err error) {
	var manifest imageReferencesManifest
	if err := yaml.Unmarshal([]byte(imagereferences.ImageReferencesYAML), &manifest); err != nil {
		return "", "", fmt.Errorf("failed to parse embedded image references: %w", err)
	}

	for _, img := range manifest.RelatedImages {
		switch img.Name {
		case EnvValidatorImage:
			validatorImage = img.Value
		case EnvToolsImage:
			toolsImage = img.Value
		}
	}

	// Environment variables override embedded defaults
	if v := os.Getenv(EnvValidatorImage); v != "" {
		validatorImage = v
	}
	if v := os.Getenv(EnvToolsImage); v != "" {
		toolsImage = v
	}

	var missing []string
	if validatorImage == "" {
		missing = append(missing, EnvValidatorImage)
	}
	if toolsImage == "" {
		missing = append(missing, EnvToolsImage)
	}
	if len(missing) > 0 {
		return "", "", fmt.Errorf("missing required image(s): %v — set via image-references.yaml or environment variables", missing)
	}

	for _, ref := range []string{validatorImage, toolsImage} {
		if !strings.Contains(ref, "@sha256:") {
			fmt.Fprintf(os.Stderr, "Warning: image %q uses a mutable tag; consider pinning by digest or setting %s / %s\n",
				ref, EnvValidatorImage, EnvToolsImage)
			break
		}
	}

	return validatorImage, toolsImage, nil
}
