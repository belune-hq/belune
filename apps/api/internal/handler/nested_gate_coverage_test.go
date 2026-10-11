package handler_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEveryNestedRouteReachesTheOwnershipGate is the tripwire for the defect
// class v0.1.19 closed, not just the instance.
//
// A route under /api/projects/{projectId}/<kind>/{childId} is NOT protected by
// middleware.RequirePinnedProjectMatch alone: that compares the pin to the URL
// and cannot know whether the child belongs to the project named in it. The
// parentage check lives in handler.ownedAccess, reached through canAccessOwned
// or isOwnerOnly. A nested handler that calls neither — directly or through a
// resolver — is unprotected, and nothing else would say so.
//
// ⚠️ This exists because the author of that fix tried to audit this surface by
// hand three times and got three different wrong answers (22 ungated, then 11,
// then 0), each time because the codebase reaches the gate through differently
// named resolvers: canAccessApplication, isDomainOwner, projectFromPath,
// resolveVolumeForBackup, destinationInProject. A machine should be doing this.
//
// Source-matched, in the idiom TestRequireRoleCallsAgreeOnRoleSet already uses
// for routes.go. Two hops is enough today and is asserted below, so a deeper
// chain fails loudly here rather than silently going unchecked.
func TestEveryNestedRouteReachesTheOwnershipGate(t *testing.T) {
	routes, err := os.ReadFile(filepath.Join("..", "server", "routes.go"))
	require.NoError(t, err)

	// Registrations carrying BOTH a {projectId} and a child id. Those are the
	// ones the middleware cannot fully judge.
	nested := regexp.MustCompile(
		`"/api/projects/\{projectId\}/[^"]*\{(?:application|database|domain|envVar|deployment|dest|backup|volume|mount|hook)[A-Za-z]*\}[^"]*",\s*h\.([A-Z]\w+)\)`)
	handlers := map[string]bool{}
	for _, m := range nested.FindAllSubmatch(routes, -1) {
		handlers[string(m[1])] = true
	}
	require.Greater(t, len(handlers), 40,
		"expected the nested surface to be substantial; if this collapsed, the pattern above stopped matching and this test is asserting nothing")

	bodies := handlerBodies(t)

	// A call that is known to reach ownedAccess.
	gate := regexp.MustCompile(`\b(canAccess\w+|is\w*Owner|ownedAccess)\(`)
	// Any other method on the handler, which may be a resolver that gates.
	callsOut := regexp.MustCompile(`\bh\.([a-z]\w+)\(`)

	reaches := func(name string, depth int) bool {
		seen := map[string]bool{}
		var walk func(string, int) bool
		walk = func(fn string, d int) bool {
			if d < 0 || seen[fn] {
				return false
			}
			seen[fn] = true
			body, ok := bodies[fn]
			if !ok {
				return false
			}
			if gate.MatchString(body) {
				return true
			}
			for _, m := range callsOut.FindAllStringSubmatch(body, -1) {
				if walk(m[1], d-1) {
					return true
				}
			}
			return false
		}
		return walk(name, depth)
	}

	var ungated []string
	for name := range handlers {
		if !reaches(name, 2) {
			ungated = append(ungated, name)
		}
	}
	sort.Strings(ungated)

	require.Empty(t, ungated,
		"these handlers serve a route nested under {projectId} but never reach canAccessOwned/isOwnerOnly, so nothing checks the child belongs to the project in the URL: %v", ungated)
}

// handlerBodies maps every method on *Handler to its source text.
func handlerBodies(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	decl := regexp.MustCompile(`(?m)^func \(h \*Handler\) (\w+)\(`)
	out := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		text := string(src)
		locs := decl.FindAllStringSubmatchIndex(text, -1)
		for i, loc := range locs {
			end := len(text)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			out[text[loc[2]:loc[3]]] = text[loc[0]:end]
		}
	}
	require.Greater(t, len(out), 100, "expected to find the handler methods; the declaration pattern may have drifted")
	return out
}
