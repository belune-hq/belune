package status_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/belune-hq/belune/internal/status"
)

func TestValidTransition(t *testing.T) {
	tests := []struct {
		name string
		from string
		to   string
		want bool
	}{
		// Allowed forward progressions
		{"pending->building", status.DeploymentPending, status.DeploymentBuilding, true},
		// pending->deploying is intentional: image-type apps skip the build step entirely.
		{"pending->deploying", status.DeploymentPending, status.DeploymentDeploying, true},
		{"building->deploying", status.DeploymentBuilding, status.DeploymentDeploying, true},
		{"deploying->success", status.DeploymentDeploying, status.DeploymentSuccess, true},

		// Any state -> failed is always permitted (error catch-all)
		{"pending->failed", status.DeploymentPending, status.DeploymentFailed, true},
		{"building->failed", status.DeploymentBuilding, status.DeploymentFailed, true},
		{"deploying->failed", status.DeploymentDeploying, status.DeploymentFailed, true},
		{"success->failed", status.DeploymentSuccess, status.DeploymentFailed, true},
		{"failed->failed", status.DeploymentFailed, status.DeploymentFailed, true},

		// Terminal states must not advance further
		{"success->deploying", status.DeploymentSuccess, status.DeploymentDeploying, false},
		{"success->building", status.DeploymentSuccess, status.DeploymentBuilding, false},
		{"success->pending", status.DeploymentSuccess, status.DeploymentPending, false},
		{"failed->success", status.DeploymentFailed, status.DeploymentSuccess, false},
		{"failed->deploying", status.DeploymentFailed, status.DeploymentDeploying, false},
		{"failed->building", status.DeploymentFailed, status.DeploymentBuilding, false},

		// No backwards transitions
		{"deploying->building", status.DeploymentDeploying, status.DeploymentBuilding, false},
		{"deploying->pending", status.DeploymentDeploying, status.DeploymentPending, false},
		{"building->pending", status.DeploymentBuilding, status.DeploymentPending, false},

		// Unknown from-state
		{"unknown->building", "unknown", status.DeploymentBuilding, false},
		{"empty->building", "", status.DeploymentBuilding, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := status.ValidTransition(tc.from, tc.to)
			assert.Equal(t, tc.want, got)
		})
	}
}
