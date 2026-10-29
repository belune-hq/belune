package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/store/generated"
)

// ErrWAFBlockingApplication is a toggle of an application that is not there.
var ErrWAFBlockingApplication = errors.New("that application does not exist")

// WAFBlockingView is an application as the blocking list shows it.
type WAFBlockingView struct {
	ApplicationID   string
	ApplicationName string
	ProjectName     string
	Blocking        bool
	// Hosts are the application's domains now: what turning blocking on affects.
	Hosts []string
	// SharedHosts are the hosts another application that has not opted in also
	// serves. They stay detection-only whatever this one is set to.
	SharedHosts []string
}

// WAFBlockingService owns which applications the WAF enforces on.
type WAFBlockingService struct {
	queries *generated.Queries
}

func NewWAFBlockingService(queries *generated.Queries) *WAFBlockingService {
	return &WAFBlockingService{queries: queries}
}

func blockingView(r generated.ListWAFBlockingApplicationsRow) WAFBlockingView {
	return WAFBlockingView{
		ApplicationID: uuidToString(r.ID), ApplicationName: r.Name, ProjectName: r.ProjectName,
		Blocking: r.WafBlocking, Hosts: r.Hostnames, SharedHosts: r.SharedHostnames,
	}
}

// List returns every application that serves a domain.
func (s *WAFBlockingService) List(ctx context.Context) ([]WAFBlockingView, error) {
	rows, err := s.queries.ListWAFBlockingApplications(ctx, pgtype.UUID{})
	if err != nil {
		return nil, fmt.Errorf("listing the applications WAF blocking can apply to: %w", err)
	}
	out := make([]WAFBlockingView, 0, len(rows))
	for _, r := range rows {
		out = append(out, blockingView(r))
	}
	return out, nil
}

// Set turns blocking on or off for one application and returns it as it now
// stands, along with what it was, for the audit trail.
func (s *WAFBlockingService) Set(ctx context.Context, applicationID pgtype.UUID, enabled bool) (was, now WAFBlockingView, err error) {
	before, err := s.queries.ListWAFBlockingApplications(ctx, applicationID)
	if err != nil {
		return was, now, fmt.Errorf("reading the application: %w", err)
	}
	if len(before) == 0 {
		// Either it does not exist or it serves no domain, and blocking applies to
		// neither. Say the second plainly rather than saving a switch that does nothing.
		return was, now, ErrWAFBlockingApplication
	}
	if _, err := s.queries.SetApplicationWAFBlocking(ctx, generated.SetApplicationWAFBlockingParams{ID: applicationID, WafBlocking: enabled}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return was, now, ErrWAFBlockingApplication
		}
		return was, now, fmt.Errorf("saving the WAF mode: %w", err)
	}
	after, err := s.queries.ListWAFBlockingApplications(ctx, applicationID)
	if err != nil || len(after) == 0 {
		return was, now, fmt.Errorf("the WAF mode was saved but could not be read back: %w", err)
	}
	return blockingView(before[0]), blockingView(after[0]), nil
}
