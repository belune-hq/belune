package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/belune-hq/belune/internal/build/railpack"
)

type serverService struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"` // "running" | "error"
}

type serverServicesResponse struct {
	Healthy  int             `json:"healthy"`
	Total    int             `json:"total"`
	Services []serverService `json:"services"`
}

// GetServerServices reports the health of the platform's infrastructure
// services (Caddy, Docker, PostgreSQL, Redis, BuildKit) for the Server page
// (admin only). Each probe is bounded by a short timeout so one hung dependency
// can't stall the response. GET /api/server/services
//
//apidoc:tag platform/maintenance
//apidoc:title Get Server Services
//apidoc:order 3
func (h *Handler) GetServerServices(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	statusOf := func(ok bool) string {
		if ok {
			return "running"
		}
		return "error"
	}

	services := []serverService{
		{
			Name:        "Caddy",
			Description: "Reverse proxy / TLS",
			Status: statusOf(func() bool {
				_, err := h.proxy.ListRoutes(ctx)
				return err == nil
			}()),
		},
		{
			Name:        "Docker daemon",
			Description: "Container runtime",
			Status: statusOf(func() bool {
				rt, err := h.runtimes.Local(ctx)
				if err != nil {
					return false
				}
				_, err = rt.ListContainers(ctx)
				return err == nil
			}()),
		},
		{
			Name:        "PostgreSQL",
			Description: "Primary datastore",
			Status:      statusOf(h.db.Ping(ctx) == nil),
		},
		{
			Name:        "Redis",
			Description: "Cache & job queue",
			Status:      statusOf(h.rdb != nil && h.rdb.Ping(ctx).Err() == nil),
		},
		{
			Name:        "BuildKit",
			Description: "Image builder",
			Status:      statusOf(railpack.CheckBuildKit() == nil),
		},
	}

	healthy := 0
	for _, s := range services {
		if s.Status == "running" {
			healthy++
		}
	}

	writeJSON(w, http.StatusOK, serverServicesResponse{
		Healthy:  healthy,
		Total:    len(services),
		Services: services,
	})
}
