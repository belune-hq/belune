package worker

import (
	"context"
	"testing"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/notify"
)

func TestAbsoluteLink(t *testing.T) {
	cases := []struct {
		name string
		base string
		rel  string
		want string
	}{
		{"joins relative", "https://belune.example", "/certificates", "https://belune.example/certificates"},
		{"trims trailing slash", "https://belune.example/", "/apps/web", "https://belune.example/apps/web"},
		{"adds leading slash", "https://belune.example", "apps/web", "https://belune.example/apps/web"},
		{"omits when base unset", "", "/certificates", ""},
		{"omits when rel empty", "https://belune.example", "", ""},
		{"passes through absolute", "https://belune.example", "https://other.example/x", "https://other.example/x"},
	}
	for _, c := range cases {
		h := &TaskHandler{Config: &config.Config{PublicBaseURL: c.base}}
		if got := h.absoluteLink(c.rel); got != c.want {
			t.Errorf("%s: absoluteLink(%q) = %q, want %q", c.name, c.rel, got, c.want)
		}
	}
}

// dispatchToChannels must be a safe no-op when nothing is wired, so the notify
// helpers can call it unconditionally from tests that construct a bare handler.
func TestDispatchToChannelsNoopWhenUnwired(t *testing.T) {
	h := &TaskHandler{}
	h.dispatchToChannels(context.Background(), notify.Event{Type: notify.EventDeploymentFailed, Title: "x"})
}
