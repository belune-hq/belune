package handler

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/docker/docker/pkg/stdcopy"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/pkg/netutil"
	"github.com/belune-hq/belune/internal/runtime"
)

// platformServices is the allowlist of infrastructure containers whose logs the
// Server → Maintenance panel may read. The value is the compose-service label
// used to resolve the container (Caddy is special-cased to the configured name).
// Anything outside this map is refused: this endpoint must never become a way to
// read arbitrary containers' logs by name.
var platformServices = map[string]string{
	"belune":   "belune",
	"caddy":    "caddy",
	"redis":    "redis",
	"postgres": "postgres",
	"buildkit": "buildkit",
}

// platformLogTail bounds how many trailing lines a single fetch returns.
const platformLogTail = 1000

type platformLogsResponse struct {
	Service string `json:"service"`
	Content string `json:"content"`
}

// GetPlatformLogs returns the tail of a platform container's logs as a text blob.
// GET /api/maintenance/logs?service=… (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Get Logs
//apidoc:order 4
func (h *Handler) GetPlatformLogs(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	svcLabel, ok := platformServices[service]
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown service")
		return
	}

	name, err := h.resolvePlatformContainer(r.Context(), service, svcLabel)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s container not found", service))
		return
	}

	rt, err := h.runtimes.Local(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to reach the Docker host")
		return
	}

	rc, err := rt.ContainerLogsTail(r.Context(), name, platformLogTail)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("failed to read logs: %v", err))
		return
	}
	defer rc.Close()

	// Platform containers are not TTY, so the stream is stdcopy-multiplexed.
	// Fold stdout and stderr into one buffer to preserve rough ordering for
	// display; the client-side viewer detects levels heuristically.
	var buf bytes.Buffer
	if _, err := stdcopy.StdCopy(&buf, &buf, rc); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("failed to read logs: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, platformLogsResponse{Service: service, Content: buf.String()})
}

type serverIPResponse struct {
	// Effective is the address the TLS DNS precheck actually uses (may be empty
	// when nothing is set and autodetection can't find a routable address).
	Effective string `json:"effective"`
	// Source explains where Effective came from: "manual" (the public_ip setting),
	// "env" (BELUNE_PUBLIC_IP), "detected" (kernel egress IP), or "unknown".
	Source string `json:"source"`
}

// effectiveServerIP resolves the address a user's DNS/SSH should point at, most-
// specific first: the public_ip setting → BELUNE_PUBLIC_IP env → kernel egress
// autodetect. source is one of manual|env|detected|unknown. Mirrors the worker's
// TLS DNS check so the panel, the SSH tunnel hint, and the precheck agree.
func (h *Handler) effectiveServerIP(ctx context.Context) (ip, source string) {
	if s, err := h.queries.GetSetting(ctx, config.SettingPublicIP); err == nil {
		if v := strings.TrimSpace(s.Value); v != "" {
			return v, "manual"
		}
	}
	if h.cfg.PublicIP != "" && net.ParseIP(h.cfg.PublicIP) != nil {
		return h.cfg.PublicIP, "env"
	}
	if detected := netutil.DetectEgressIP(); detected != "" {
		return detected, "detected"
	}
	return "", "unknown"
}

// GetServerIP reports the effective server IP and where it came from, so the
// Server IP panel can show the actual auto-detected address instead of a bare
// "auto-detected".
// GET /api/maintenance/server-ip (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Get Server IP
//apidoc:order 1
func (h *Handler) GetServerIP(w http.ResponseWriter, r *http.Request) {
	ip, source := h.effectiveServerIP(r.Context())
	writeJSON(w, http.StatusOK, serverIPResponse{Effective: ip, Source: source})
}

// selfContainerRe extracts the 64-hex container ID from the bind-mount paths
// (/etc/hostname, /etc/hosts, …) Docker injects into /proc/self/mountinfo.
var selfContainerRe = regexp.MustCompile(`/containers/([0-9a-f]{64})`)

// selfContainerID returns the ID of the container this process runs in, so the
// log viewer can read Belune's own logs regardless of its compose service name.
// Empty when it can't be determined (e.g. not running in a container).
func selfContainerID() string {
	// Most reliable across cgroup v1/v2: the container's own directory appears in
	// the mount source of the files Docker bind-mounts into it.
	if data, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		if m := selfContainerRe.FindSubmatch(data); m != nil {
			return string(m[1])
		}
	}
	// Fallback: Docker defaults the container hostname to the short ID, which the
	// Docker API accepts as a container reference.
	if host, err := os.Hostname(); err == nil {
		return host
	}
	return ""
}

// selfContainer resolves this container's own record from the Docker daemon.
// Shared by selfImage and selfWorkingDir so there is exactly one "find myself"
// loop rather than one per caller.
func (h *Handler) selfContainer(ctx context.Context) (runtime.ContainerInfo, bool) {
	id := selfContainerID()
	if id == "" {
		return runtime.ContainerInfo{}, false
	}
	rt, err := h.runtimes.Local(ctx)
	if err != nil {
		return runtime.ContainerInfo{}, false
	}
	all, err := rt.ListAllContainers(ctx)
	if err != nil {
		return runtime.ContainerInfo{}, false
	}
	for _, c := range all {
		// selfContainerID may be the full ID (from mountinfo) or the short ID
		// (hostname fallback); either is a prefix of the daemon's full ID.
		if strings.HasPrefix(c.ID, id) {
			return c, true
		}
	}
	return runtime.ContainerInfo{}, false
}

// selfWorkingDir returns the host path the update helper runs against —
// docker-compose.yml, .env and scripts/ all live there — read off this
// container's own compose project label. Errors when it can't be determined:
// an update helper with nowhere to mount would silently do nothing, which is
// worse than refusing up front.
func (h *Handler) selfWorkingDir(ctx context.Context) (string, error) {
	c, ok := h.selfContainer(ctx)
	if !ok {
		return "", fmt.Errorf("could not find this container's own record")
	}
	wd := c.Labels["com.docker.compose.project.working_dir"]
	if wd == "" {
		return "", fmt.Errorf("this container has no compose project directory — the in-app updater only works on a docker-compose install")
	}
	return wd, nil
}

// restartableServices is the allowlist for RestartService. Deliberately smaller
// than platformServices: belune (self-restart), postgres (data risk), and
// buildkit are excluded. Restarting Caddy or Redis is recoverable; the others
// are not something an operator should trigger from a web button.
var restartableServices = map[string]string{
	"caddy": "caddy",
	"redis": "redis",
}

// restartTimeout is how long Docker waits for graceful stop before killing.
const restartTimeout = 15

// RestartService restarts an allowlisted platform container in place.
// POST /api/maintenance/restart?service=… (admin only)
//
//apidoc:tag platform/maintenance
func (h *Handler) RestartService(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	svcLabel, ok := restartableServices[service]
	if !ok {
		writeError(w, http.StatusBadRequest, "service is not restartable")
		return
	}

	name, err := h.resolvePlatformContainer(r.Context(), service, svcLabel)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s container not found", service))
		return
	}

	rt, err := h.runtimes.Local(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to reach the Docker host")
		return
	}

	if err := rt.RestartContainer(r.Context(), name, restartTimeout); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("failed to restart %s: %v", service, err))
		return
	}

	h.audit(r, "restart_service", "service", service, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "restarted", "service": service})
}

// resolvePlatformContainer maps a service key to a concrete container. Caddy's
// name is configured explicitly; the rest are found by their compose-service
// label, scoped to the same compose project as Caddy so a same-named container
// from an unrelated stack on the host is never picked.
func (h *Handler) resolvePlatformContainer(ctx context.Context, service, svcLabel string) (string, error) {
	if service == "caddy" && h.cfg.CaddyContainerName != "" {
		return h.cfg.CaddyContainerName, nil
	}
	// Belune runs in a container whose compose service name varies (belune in
	// prod, api in the devcontainer), so resolve its OWN container by ID rather
	// than by a guessed label.
	if service == "belune" {
		if id := selfContainerID(); id != "" {
			return id, nil
		}
		return "", fmt.Errorf("could not determine belune's own container")
	}

	rt, err := h.runtimes.Local(ctx)
	if err != nil {
		return "", err
	}
	all, err := rt.ListAllContainers(ctx)
	if err != nil {
		return "", err
	}

	project := ""
	for _, c := range all {
		if c.Name == h.cfg.CaddyContainerName {
			project = c.Labels["com.docker.compose.project"]
			break
		}
	}

	for _, c := range all {
		if c.Labels["com.docker.compose.service"] != svcLabel {
			continue
		}
		if project != "" && c.Labels["com.docker.compose.project"] != project {
			continue
		}
		return c.ID, nil
	}
	return "", fmt.Errorf("no container for service %q", service)
}
