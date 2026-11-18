package runtime

import (
	"fmt"
	"strings"

	"github.com/distribution/reference"
)

// RegistryAuth is resolved only for application pulls, never ambient daemon auth.
type RegistryAuth struct {
	Username      string
	Password      string `json:"-"`
	ServerAddress string
}

func RegistryHost(image string) (string, error) {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", fmt.Errorf("invalid image reference: %w", err)
	}
	return reference.Domain(named), nil
}

// RegistryCredentialHost accepts a host, not an image path or credential URL.
func RegistryCredentialHost(value string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(value))
	if host == "" || len(host) > 300 || strings.ContainsAny(host, "/@?# \t\r\n") {
		return "", fmt.Errorf("registry host must be a hostname with an optional port")
	}
	if host == "index.docker.io" {
		host = "docker.io"
	}
	domain, err := RegistryHost(host + "/belune-validation")
	if err != nil || domain != host {
		return "", fmt.Errorf("registry host must include a dot or port, or be localhost")
	}
	return domain, nil
}
