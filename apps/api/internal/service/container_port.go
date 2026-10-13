package service

import (
	"errors"

	"github.com/jackc/pgx/v5/pgtype"
)

// ValidateContainerPort turns a caller's port into the column value.
//
// 0 clears the port back to NULL, so resolution falls through to the first
// domain's port and then 8080. The convention is deliberate and shared with
// every other "unset" in this API (cpu_limit 0 is unlimited, a health tuning
// field of 0 is the default): a nullable field in the body would make an
// omitted key and an explicit null indistinguishable once decoded, and the
// handler and the MCP tool would each have to pick a reading. Callers MUST
// treat a missing value as an error rather than as 0 — see the REST handler.
//
// Shared by REST and MCP so the two cannot disagree about a valid port.
func ValidateContainerPort(port int) (pgtype.Int4, error) {
	if port == 0 {
		return pgtype.Int4{}, nil
	}
	if port < 1 || port > 65535 {
		return pgtype.Int4{}, errors.New("port must be between 1 and 65535, or 0 to clear it")
	}
	return pgtype.Int4{Int32: int32(port), Valid: true}, nil
}

// ValidateDomainContainerPort is the same range rule for a domain's per-route
// override, where "unset" means something different: an application's 0 is an
// explicit clear of a stored value, but a domain's port is written whole on
// every create and update, so an absent value and 0 both simply mean "no
// override" and store NULL. Storing NULL for 0 is routing-neutral
// (ResolveUpstreamPort ignores anything not > 0) and keeps a Valid zero from
// reaching the health-check resolution as port 0.
func ValidateDomainContainerPort(port *int32) (pgtype.Int4, error) {
	if port == nil || *port == 0 {
		return pgtype.Int4{}, nil
	}
	return ValidateContainerPort(int(*port))
}
