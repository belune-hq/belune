package caddy

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
)

// The path-forwarding contract, shared with the frontend preview. Both sides
// answer to the same fixture, so the preview cannot quietly start telling the
// operator something the proxy does not do — see web/src/lib/domain-path.test.ts.
//
// The file lives outside this Go module, so `go test` does not track it as an
// input and will replay a cached PASS after it changes. CI runs -count=1 for
// exactly this reason; do not remove that flag.
const forwardingFixture = "../../../../web/src/lib/path-forwarding.fixture.json"

type forwardingCase struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	StripPath    bool   `json:"strip_path"`
	InternalPath string `json:"internal_path"`
	Request      string `json:"request"`
	Expected     string `json:"expected"`
}

// applyRewrites interprets the rewrite handlers buildRoute actually emitted, the
// way Caddy would. It deliberately reads the generated config rather than
// recomputing the answer from cfg: a second implementation of the rules would
// agree with the first by construction and prove nothing.
func applyRewrites(t *testing.T, route caddyRoute, requestURI string) string {
	t.Helper()

	uri := requestURI
	for _, h := range route.Handle {
		if h["handler"] != "rewrite" {
			continue
		}

		if prefix, ok := h["strip_path_prefix"].(string); ok {
			// Caddy strips the prefix from the path and normalises what is left;
			// stripping "/strip" from "/strip" leaves "/".
			path, query, hasQuery := strings.Cut(uri, "?")
			path = strings.TrimPrefix(path, prefix)
			if path == "" {
				path = "/"
			}
			uri = path
			if hasQuery {
				uri += "?" + query
			}
			continue
		}

		if tmpl, ok := h["uri"].(string); ok {
			// The template is literally "<internal>{http.request.uri}".
			uri = strings.ReplaceAll(tmpl, "{http.request.uri}", uri)
		}
	}
	return uri
}

func TestForwarding_MatchesSharedFixture(t *testing.T) {
	raw, err := os.ReadFile(forwardingFixture)
	require.NoError(t, err, "read %s — if the fixture moved, update this test rather "+
		"than deleting it: it is the only thing keeping the frontend preview honest",
		forwardingFixture)

	var fixture struct {
		Cases []forwardingCase `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.NotEmpty(t, fixture.Cases)

	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			route, err := buildRoute(proxy.RouteConfig{
				Hostname:     "shop.com",
				Path:         c.Path,
				StripPath:    c.StripPath,
				InternalPath: c.InternalPath,
				TargetURL:    "http://app:3000",
			})
			require.NoError(t, err)

			got := applyRewrites(t, route, c.Request)
			assert.Equal(t, c.Expected, got,
				"the handlers built for path=%q strip=%v internal=%q would forward %q as %q, "+
					"but the shared contract says %q. Every expected value in that fixture was "+
					"observed against a real Caddy, so this is the builder drifting, not the fixture.",
				c.Path, c.StripPath, c.InternalPath, c.Request, got, c.Expected)
		})
	}
}
