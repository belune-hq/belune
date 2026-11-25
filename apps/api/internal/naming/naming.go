package naming

import (
	"fmt"
	"regexp"
	"strings"
)

var nonAlphanumeric = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify converts a name to a URL/container-safe slug.
func Slugify(name string) string {
	s := strings.ToLower(name)
	s = nonAlphanumeric.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}

// ContainerName returns the container name.
// The appSlug stored in the DB is already the fully-qualified container name
// in the format "{projectSlug}-{baseSlug}-{applicationID[:8]}", so it is
// returned directly. The other parameters are kept for call-site compatibility.
func ContainerName(_, appSlug, _ string) string {
	return appSlug
}

// IntermediateContainerName returns the previous naming format: "{projectSlug}-{applicationID[:8]}"
func IntermediateContainerName(projectSlug, applicationID string) string {
	return fmt.Sprintf("%s-%s", projectSlug, applicationID[:8])
}

// OldContainerName returns the legacy container name: "belune-{applicationID[:8]}"
func OldContainerName(applicationID string) string {
	return fmt.Sprintf("belune-%s", applicationID[:8])
}

// ImageTag returns the image tag: "{appSlug}:{deploymentID[:8]}"
// appSlug is already the fully-qualified name "{projectSlug}-{baseSlug}-{applicationID[:8]}".
func ImageTag(_, appSlug, _, deploymentID string) string {
	return fmt.Sprintf("%s:%s", appSlug, deploymentID[:8])
}

// ProjectNetworkName returns the Docker network name scoped to a project: "belune-{projectSlug}"
func ProjectNetworkName(projectSlug string) string {
	return fmt.Sprintf("belune-%s", projectSlug)
}

// CNBCacheVolumeName returns the Docker volume name holding the CNB
// (Cloud Native Buildpacks) build cache for a single application. Keying by
// application ID keeps layer history isolated per app: one misbehaving build
// cannot poison another app's cache, and clearing is a single volume delete.
func CNBCacheVolumeName(applicationID string) string {
	if len(applicationID) < 8 {
		return fmt.Sprintf("belune-cnb-cache-%s", applicationID)
	}
	return fmt.Sprintf("belune-cnb-cache-%s", applicationID[:8])
}

// CNBLaunchCacheVolumeName is the companion launch-layer cache volume. Pack
// separates build-time and launch-time layers into two distinct caches;
// colocating naming here avoids drift between the two.
func CNBLaunchCacheVolumeName(applicationID string) string {
	if len(applicationID) < 8 {
		return fmt.Sprintf("belune-cnb-launch-%s", applicationID)
	}
	return fmt.Sprintf("belune-cnb-launch-%s", applicationID[:8])
}

// AppVolumeName returns the Docker named-volume name for a persistent
// application volume. volumeName is the already-slugified per-application name
// (unique within the app via a DB constraint), so the result is stable across
// redeploys and collision-free between apps. The deploy worker reconstructs
// this name to reattach the same volume on every deploy.
func AppVolumeName(applicationID, volumeName string) string {
	id := applicationID
	if len(id) >= 8 {
		id = id[:8]
	}
	return fmt.Sprintf("belune-vol-%s-%s", id, volumeName)
}

// VolumeNameFor decides the Docker name in one place. Adopted volumes carry
// their literal name; managed volumes keep deriving from immutable inputs.
func VolumeNameFor(applicationID, volumeName, externalName string) string {
	if externalName != "" {
		return externalName
	}
	return AppVolumeName(applicationID, volumeName)
}
