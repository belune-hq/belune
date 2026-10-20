package service_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/store/generated"
)

func pendingChangeTS(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func TestApplicationPendingChange(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name string
		app  generated.Application
		want string
	}{
		{
			name: "nothing changed",
			app:  generated.Application{LastDeployedAt: pendingChangeTS(now)},
			want: "",
		},
		{
			name: "config marker set",
			app:  generated.Application{LastDeployedAt: pendingChangeTS(now), ConfigChangedAt: pendingChangeTS(now)},
			want: "config",
		},
		{
			name: "source marker set",
			app:  generated.Application{LastDeployedAt: pendingChangeTS(now), SourceChangedAt: pendingChangeTS(now)},
			want: "source",
		},
		{
			// Source outranks config: a deploy applies both, so offering
			// "Reload to apply" here would name an action that cannot finish
			// the job.
			name: "both set reports the stronger one",
			app: generated.Application{
				LastDeployedAt:  pendingChangeTS(now),
				ConfigChangedAt: pendingChangeTS(now),
				SourceChangedAt: pendingChangeTS(now),
			},
			want: "source",
		},
		{
			// The false positive the suppression rule exists to prevent: an
			// app configured before its first deploy would otherwise read
			// "needs redeploy" from birth.
			name: "suppressed until the first successful deploy",
			app: generated.Application{
				ConfigChangedAt: pendingChangeTS(now),
				SourceChangedAt: pendingChangeTS(now),
			},
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := service.ApplicationPendingChange(c.app); got != c.want {
				t.Errorf("ApplicationPendingChange() = %q, want %q", got, c.want)
			}
		})
	}
}
