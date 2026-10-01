package mcpserver

import (
	"context"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/server/middleware"
)

// registeredTools asks the real server what it registered, over an in-memory
// transport, so the test cannot share a source with toolScopes.
func registeredTools(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	srv := newServer(nil, nil, nil, nil)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	require.NoError(t, err)
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	defer cs.Close()

	var names []string
	for tool, err := range cs.Tools(ctx, nil) {
		require.NoError(t, err)
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

// Both directions: a registered tool with no declared scope would be refused
// at runtime (fail closed) but is almost certainly a mistake, and a declared
// scope for a tool that does not exist is a stale entry hiding a rename.
func TestToolScopesMatchRegisteredTools(t *testing.T) {
	registered := registeredTools(t)
	require.NotEmpty(t, registered)

	declared := make([]string, 0, len(toolScopes))
	for name, scope := range toolScopes {
		declared = append(declared, name)
		assert.Contains(t, []string{"read", "write", "deploy"}, scope, "tool %s", name)
	}
	sort.Strings(declared)
	assert.Equal(t, registered, declared)
}

// scoped runs requireToolScope for one tools/call carrying the given token
// scopes and reports whether the tool body was reached.
func scoped(t *testing.T, tool string, scopes []string) (reached bool, err error) {
	t.Helper()
	ctx := middleware.ContextWithScopes(context.Background(), scopes)
	h := requireToolScope(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		reached = true
		return nil, nil
	})
	_, err = h(ctx, "tools/call", &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
		Params: &mcp.CallToolParamsRaw{Name: tool},
	})
	return reached, err
}

func TestRequireToolScope(t *testing.T) {
	// Synthetic entries: the gate must work for write/deploy before any real
	// tool of those scopes exists, and keep working if one is renamed.
	toolScopes["probe_write"] = "write"
	toolScopes["probe_deploy"] = "deploy"
	t.Cleanup(func() { delete(toolScopes, "probe_write"); delete(toolScopes, "probe_deploy") })

	for name, tc := range map[string]struct {
		tool   string
		scopes []string
		want   bool
	}{
		"read cannot call write":          {"probe_write", []string{"read"}, false},
		"deploy cannot call write":        {"probe_write", []string{"deploy"}, false},
		"write calls write":               {"probe_write", []string{"write"}, true},
		"read cannot call deploy":         {"probe_deploy", []string{"read"}, false},
		"deploy calls deploy":             {"probe_deploy", []string{"deploy"}, true},
		"write is the umbrella":           {"probe_deploy", []string{"write"}, true},
		"read calls read":                 {"list_projects", []string{"read"}, true},
		"undeclared tool is refused":      {"not_in_table", []string{"write"}, false},
		"nil scopes are refused":          {"list_projects", nil, false},
		"metrics-only cannot call a read": {"list_projects", []string{"metrics"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			reached, err := scoped(t, tc.tool, tc.scopes)
			assert.Equal(t, tc.want, reached)
			assert.Equal(t, !tc.want, err != nil)
		})
	}
}

func TestRequireToolScopePassesOtherMethods(t *testing.T) {
	reached := false
	h := requireToolScope(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		reached = true
		return nil, nil
	})
	_, err := h(context.Background(), "tools/list", nil)
	require.NoError(t, err)
	assert.True(t, reached)
}
