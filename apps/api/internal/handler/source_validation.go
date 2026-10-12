package handler

import "github.com/weiliang79/belune/internal/service"

// The rules for where an application comes from, and for the branch and root
// directory values, live in service/source_validation.go so the REST handlers
// and the MCP tools share one definition. These delegates keep the handlers'
// call sites unchanged.

type sourceFields = service.SourceFields

func validateSource(f sourceFields) error { return service.ValidateSource(f) }

func validBranchName(branch string) bool { return service.ValidBranchName(branch) }

func validRootDirectory(dir string) bool { return service.ValidRootDirectory(dir) }
