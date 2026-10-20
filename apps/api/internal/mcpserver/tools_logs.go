package mcpserver

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/docker/docker/pkg/stdcopy"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
)

const (
	defaultLogTailLines = 200
	maxLogTailLines     = 1000
)

type applicationLogsInput struct {
	ApplicationID string `json:"application_id" jsonschema:"the application's id"`
	Tail          int    `json:"tail,omitempty" jsonschema:"number of most recent log lines to return (default 200, max 1000)"`
}

// registerLogTools registers the one tool in this package that reaches
// outside the database: a live container log tail, read straight from the
// runtime rather than the historical container_logs table.
//
// ⚠️ Uses ContainerLogsTail, never ContainerLogs — the latter is unbounded
// and is what the platform log viewer explicitly avoids reading in full.
// Logs are returned verbatim, with no redaction beyond what
// internal/pkg/redact already strips elsewhere in the product: connection
// strings and API keys printed at container boot land in the tool's output
// exactly as Docker recorded them. Whoever connects a client to this server
// is choosing to hand it that.
func registerLogTools(srv *mcp.Server, queries *generated.Queries, runtimes runtime.Runtimes) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_application_logs",
		Description: "Tail an application's live container output — the last N lines, not a " +
			"stream. Returned verbatim: container logs often contain connection strings or API " +
			"keys printed at startup. Defaults to 200 lines, capped at 1000.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in applicationLogsInput) (*mcp.CallToolResult, any, error) {
		appID, err := parseUUID(in.ApplicationID)
		if err != nil {
			return nil, nil, err
		}
		// Not authorizeApplication: that would re-join applications to
		// projects a second time via GetApplicationOwnerUserID for data this
		// query's own join to projects (for project_slug/server_id) already
		// has. Same pin + ownership check, inlined against this row instead.
		row, err := queries.GetApplicationLogAccess(ctx, appID)
		if err != nil {
			return nil, nil, notFoundOr(ctx, "application not found")
		}
		if !pinAllows(ctx, uuidToString(row.ProjectID)) {
			return nil, nil, errAccessDenied
		}
		if !canAccessOwned(ctx, row.ProjectUserID, row.ProjectShared) {
			return nil, nil, errAccessDenied
		}

		tail := in.Tail
		if tail <= 0 {
			tail = defaultLogTailLines
		}
		if tail > maxLogTailLines {
			tail = maxLogTailLines
		}

		rt, err := runtimes.For(ctx, row.ServerID)
		if err != nil {
			return nil, nil, internalError("failed to reach the application's server", err)
		}

		containerName := naming.ContainerName(row.ProjectSlug, row.Slug, uuidToString(row.ID))
		rc, err := rt.ContainerLogsTail(ctx, containerName, tail)
		if err != nil {
			return nil, nil, internalError("failed to read container logs", err)
		}
		defer rc.Close()

		// The container is not a TTY, so the stream is stdcopy-multiplexed: an
		// 8-byte header per frame whose length bytes are frequently printable
		// ASCII, so reading it raw puts stray letters in front of real log
		// text. Demuxed the same way GetPlatformLogs and
		// updateHelperFailureReason do. Unlike those, the RFC3339Nano
		// timestamp Docker prefixes every line with is kept rather than
		// stripped — a display pane already has its own relative-time chrome,
		// but an assistant reading this text has only what's in it, and "when
		// did this happen" is exactly the kind of thing worth keeping.
		var buf bytes.Buffer
		_, copyErr := stdcopy.StdCopy(&buf, &buf, rc)
		if copyErr != nil && buf.Len() == 0 {
			return nil, nil, internalError("failed to read container logs", copyErr)
		}

		// Container output is arbitrary bytes, not guaranteed valid UTF-8 — a
		// binary crash dump, or a multi-byte sequence cut off at the tail
		// boundary. json.Marshal would silently substitute U+FFFD for any
		// invalid byte anyway; doing it explicitly here makes that a
		// documented choice instead of an implicit side effect.
		text := stripANSI(strings.ToValidUTF8(buf.String(), "�"))
		if copyErr != nil {
			// Some bytes were captured before the stream ended abnormally —
			// still worth returning, but silently presenting a partial read
			// as a complete one would be worse than flagging it. Logged
			// server-side (see internalError's own reasoning) rather than
			// embedding copyErr's raw text in what's otherwise log content.
			slog.Warn("mcpserver: container log stream ended before EOF, returning a truncated tail", "error", copyErr)
			text += "\n[... log read ended early; output may be truncated]"
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, nil, nil
	})
}

// maxBuildLogBytes bounds get_deployment_logs on top of the line cap: a
// build log is stored as one blob and a single line (a minified bundle
// echoed by a build step) can be arbitrarily long.
const maxBuildLogBytes = 256 << 10

// ansiRe matches terminal escape sequences: CSI (colour, cursor movement) and
// OSC (window title, hyperlinks), plus bare two-byte escapes. The `\u001b`
// alternative is the JSON-escaped ESC — build logs are NDJSON, so a coloured
// message is stored with the six characters "\u001b" instead of the raw byte.
var ansiRe = regexp.MustCompile(`(?:\x1b|\\u001[bB])(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)

// stripANSI removes terminal formatting from log text handed to an assistant.
//
// Logs are otherwise returned verbatim on purpose: that contract exists to
// rule out *partial redaction of secrets*, which would give a false sense of
// safety. Colour codes are terminal formatting, not content, so stripping
// them creates no such illusion — and the dashboard already does the same
// (logs/parse.ts stripAnsi), so a human sees clean logs while an assistant
// would otherwise read `[33mWARN[0m`.
//
// ⚠️ Deliberately done here, in the tool, and not in ContainerLogsTail or the
// stdcopy demux: those feed the log viewer's SSE path too, and stripping
// there would silently change what it receives. The duplication with the
// frontend is intentional.
func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// tailLog returns at most the last n lines of s, further capped to maxBytes
// (keeping the end, starting on a line boundary where possible), with ANSI
// stripped and invalid UTF-8 replaced. A marker line says when the head was
// dropped so the reader does not mistake a tail for the whole log.
func tailLog(s string, n, maxBytes int) string {
	s = strings.TrimRight(s, "\n")
	dropped := 0
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		dropped = len(lines) - n
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxBytes {
		out = out[len(out)-maxBytes:]
		// Land on a line boundary if there is one, else on a rune boundary.
		if i := strings.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		} else {
			for len(out) > 0 && !utf8.RuneStart(out[0]) {
				out = out[1:]
			}
		}
		dropped = -1
	}
	out = stripANSI(strings.ToValidUTF8(out, "�"))
	switch {
	case dropped < 0:
		out = "[... earlier output omitted]\n" + out
	case dropped > 0:
		out = fmt.Sprintf("[... %d earlier lines omitted]\n", dropped) + out
	}
	return out
}
