package mcpserver

import (
	"context"
	"errors"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store/generated"
)

// updateApplicationInput uses pointers so "not mentioned" and "set to empty"
// are different things. That distinction is the whole safety of this tool: the
// caller is an LLM composing each call from this schema, and fills in only
// what the user mentioned. A plain string field cannot tell an omitted
// source_repo from a request to clear it, and the REST update — which decodes
// into plain strings — clears whatever is omitted. A tool that inherited that
// would wipe fields on the first partial call.
type updateApplicationInput struct {
	ApplicationID     string  `json:"application_id" jsonschema:"the application's id"`
	Name              *string `json:"name,omitempty" jsonschema:"new display name; cannot be empty"`
	SourceRepo        *string `json:"source_repo,omitempty" jsonschema:"git repository URL (git applications only)"`
	SourceImage       *string `json:"source_image,omitempty" jsonschema:"container image reference (image applications only)"`
	Branch            *string `json:"branch,omitempty" jsonschema:"git ref to build; empty string means the repository's default branch"`
	RootDirectory     *string `json:"root_directory,omitempty" jsonschema:"subdirectory to build from; empty string means the repository root"`
	DockerfilePath    *string `json:"dockerfile_path,omitempty" jsonschema:"path to the Dockerfile; empty string clears it"`
	BuildTypeOverride *string `json:"build_type_override,omitempty" jsonschema:"one of dockerfile, buildpacks, railpack; empty string clears it"`
	BuilderImage      *string `json:"builder_image,omitempty" jsonschema:"buildpacks builder image; empty string clears it"`
}

type updatedApplication struct {
	Application   application `json:"application"`
	ChangedFields []string    `json:"changed_fields"`
	Note          string      `json:"note,omitempty"`
}

// changedFields reports which of the editable fields actually moved, by
// diffing before against after rather than echoing the request: the service
// can override what was asked for (a preview child keeps its own branch), and
// the caller should be told what happened, not what it hoped for.
func changedFields(before, after generated.Application) []string {
	out := []string{}
	for _, f := range []struct {
		name          string
		before, after string
	}{
		{"name", before.Name, after.Name},
		{"source_repo", before.SourceRepo.String, after.SourceRepo.String},
		{"source_image", before.SourceImage.String, after.SourceImage.String},
		{"branch", before.Branch.String, after.Branch.String},
		{"root_directory", before.RootDirectory.String, after.RootDirectory.String},
		{"dockerfile_path", before.DockerfilePath.String, after.DockerfilePath.String},
		{"build_type_override", before.BuildTypeOverride.String, after.BuildTypeOverride.String},
		{"builder_image", before.BuilderImage.String, after.BuilderImage.String},
	} {
		if f.before != f.after {
			out = append(out, f.name)
		}
	}
	return out
}

func registerApplicationActionTools(srv *mcp.Server, d Deps) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "update_application",
		Description: "Change an application's saved settings: its name, source (repository or image), branch, root directory, " +
			"Dockerfile path, build type override or builder image. Only the fields you pass are changed — every field " +
			"you omit keeps its current value, so pass only what the user asked to change. To clear a field, pass an " +
			"empty string for it (allowed for branch, root_directory, dockerfile_path, build_type_override and " +
			"builder_image). The change is saved immediately and there is no confirmation step. " +
			"It does NOT redeploy: the running container is untouched until you call trigger_deployment, and the " +
			"application's pending_change shows that a change is waiting. It cannot change environment variables, " +
			"domains, resource limits, health checks, git credentials or the application's type.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in updateApplicationInput) (*mcp.CallToolResult, any, error) {
		id, err := parseUUID(in.ApplicationID)
		if err != nil {
			return nil, nil, err
		}
		current, err := d.Queries.GetApplication(ctx, id)
		if err != nil {
			return nil, nil, notFoundOr(ctx, "application not found")
		}
		if err := authorizeApplication(ctx, d.Queries, current.ID, current.ProjectID); err != nil {
			return nil, nil, err
		}

		if in.Name == nil && in.SourceRepo == nil && in.SourceImage == nil && in.Branch == nil &&
			in.RootDirectory == nil && in.DockerfilePath == nil && in.BuildTypeOverride == nil && in.BuilderImage == nil {
			return nil, nil, errors.New("no fields to update: pass at least one of name, source_repo, source_image, branch, root_directory, dockerfile_path, build_type_override, builder_image")
		}
		if in.Name != nil && strings.TrimSpace(*in.Name) == "" {
			return nil, nil, errors.New("name cannot be empty")
		}

		// Start from the stored row, then overlay only what was passed. The
		// service's Update writes every column, so anything not seeded here
		// from `current` would be cleared.
		p := service.UpdateApplicationParams{
			Name:              current.Name,
			SourceRepo:        current.SourceRepo.String,
			SourceImage:       current.SourceImage.String,
			DockerfilePath:    current.DockerfilePath.String,
			BuildTypeOverride: current.BuildTypeOverride.String,
			BuilderImage:      current.BuilderImage.String,
			Branch:            current.Branch.String,
			RootDirectory:     current.RootDirectory.String,
			GitIntegrationID:  current.GitIntegrationID,
		}
		overlay := func(dst *string, src *string) {
			if src != nil {
				*dst = *src
			}
		}
		overlay(&p.Name, in.Name)
		overlay(&p.SourceRepo, in.SourceRepo)
		overlay(&p.SourceImage, in.SourceImage)
		overlay(&p.DockerfilePath, in.DockerfilePath)
		overlay(&p.BuildTypeOverride, in.BuildTypeOverride)
		overlay(&p.BuilderImage, in.BuilderImage)
		overlay(&p.Branch, in.Branch)
		overlay(&p.RootDirectory, in.RootDirectory)

		if !service.ValidBranchName(p.Branch) {
			return nil, nil, errors.New("invalid branch name")
		}
		if !service.ValidRootDirectory(p.RootDirectory) {
			return nil, nil, errors.New("invalid root directory")
		}
		// type and build_type are not updatable, so they come from the stored
		// row; the messages are written to say what to do next.
		if err := service.ValidateSource(service.SourceFields{
			Type:              current.Type,
			BuildType:         current.BuildType,
			BuildTypeOverride: p.BuildTypeOverride,
			DockerfilePath:    p.DockerfilePath,
			SourceRepo:        p.SourceRepo,
			SourceImage:       p.SourceImage,
		}); err != nil {
			return nil, nil, err
		}

		updated, err := d.Apps.Update(ctx, current.ID, current, p)
		if err != nil {
			return nil, nil, internalError("failed to update application", err)
		}
		d.Apps.MarkUpdate(ctx, current, updated)

		changed := changedFields(current, updated)
		auditTool(ctx, d.Audit, "update_application", "application", in.ApplicationID, map[string]any{"fields": changed})

		out := updatedApplication{Application: toApplication(updated), ChangedFields: changed}
		if len(changed) == 0 {
			out.Note = "nothing changed: every value passed already matched"
		} else {
			out.Note = "saved, not deployed — call trigger_deployment to apply the change"
		}
		return textResult(out)
	})
}
