package service

import (
	"errors"

	"github.com/jackc/pgx/v5/pgtype"
)

// ValidateContainerPort turns a caller's port into the column value: the
// range rule for every container port in the API, application and domain alike.
//
// 0 clears the port back to NULL, so resolution falls through to the first
// domain's port and then 8080 — the same "0 is unset" convention as cpu_limit
// and the health tuning fields. Whether an absent value means "leave it alone"
// is the caller's call, not this function's: SetHealthCheck takes a *int and
// only calls this when the key was present, because the port is not part of
// the health-check unit and omitting it must not clear it.
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
