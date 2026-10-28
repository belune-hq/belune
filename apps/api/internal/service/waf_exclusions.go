package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/store/generated"
)

// Errors a caller can tell apart: each maps to a different answer.
var (
	// ErrWAFExclusionLimit is a full list. It names the ceiling because "too many"
	// with no number is not something an operator can act on.
	ErrWAFExclusionLimit = fmt.Errorf("there are already %d exclusions, which is the most Belune will render; remove one you no longer need", proxy.MaxWAFExclusions)
	// ErrWAFExclusionDuplicate is the same carve-out twice.
	ErrWAFExclusionDuplicate = errors.New("that exclusion already exists")
	// ErrWAFExclusionApplication is a scope that names an application that is not there.
	ErrWAFExclusionApplication = errors.New("that application does not exist")
	// ErrWAFExclusionNotFound is a delete of one that is already gone.
	ErrWAFExclusionNotFound = errors.New("exclusion not found")
)

// WAFExclusionInvalid is a submitted exclusion that is refused, and why.
type WAFExclusionInvalid struct{ Reason string }

func (e *WAFExclusionInvalid) Error() string { return e.Reason }

// WAFExclusionView is an exclusion as the API shows it.
type WAFExclusionView struct {
	ID          int64
	RuleID      int
	Shape       string
	Collection  string
	TargetName  string
	PathPattern string
	// ApplicationID is empty for an install-wide exclusion.
	ApplicationID   string
	ApplicationName string
	// Hosts are the application's domains now. Empty for an install-wide one.
	Hosts []string
	// Inert is an application-scoped exclusion whose application has no domains:
	// it applies to nothing, which is shown rather than hidden.
	Inert     bool
	Note      string
	CreatedAt time.Time
}

// WAFExclusionInput is a request to create one.
type WAFExclusionInput struct {
	proxy.WAFExclusionSpec
	// ApplicationID scopes it to an application; empty is install-wide.
	ApplicationID string
	Note          string
	CreatedBy     string
}

// WAFExclusionService owns the operator's carve-outs from the rule set.
type WAFExclusionService struct {
	queries *generated.Queries
}

func NewWAFExclusionService(queries *generated.Queries) *WAFExclusionService {
	return &WAFExclusionService{queries: queries}
}

// List returns every exclusion, oldest first.
func (s *WAFExclusionService) List(ctx context.Context) ([]WAFExclusionView, error) {
	rows, err := s.queries.ListWAFExclusions(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing WAF exclusions: %w", err)
	}
	out := make([]WAFExclusionView, 0, len(rows))
	for _, r := range rows {
		out = append(out, viewOfRow(r))
	}
	return out, nil
}

func viewOfRow(r generated.ListWAFExclusionsRow) WAFExclusionView {
	spec := proxy.WAFExclusionSpec{
		RuleID:      int(r.RuleID),
		Collection:  r.Collection.String,
		TargetName:  r.TargetName.String,
		PathPattern: r.PathPattern.String,
	}
	v := WAFExclusionView{
		ID: r.ID, RuleID: spec.RuleID, Shape: spec.Shape(),
		Collection: spec.Collection, TargetName: spec.TargetName, PathPattern: spec.PathPattern,
		Note: r.Note, CreatedAt: r.CreatedAt.Time, Hosts: []string{},
	}
	if r.ApplicationID.Valid {
		v.ApplicationID = uuidString(r.ApplicationID)
		v.ApplicationName = r.ApplicationName
		v.Hosts = append(v.Hosts, r.Hostnames...)
		v.Inert = len(r.Hostnames) == 0
	}
	return v
}

// Create validates and stores an exclusion.
//
// Validation is the function the renderer applies too, so a row saved here is a row
// the renderer will honour, and one it would refuse never gets in. The rule-wide
// shape additionally needs a note: it is the broadest carve-out available, and
// the reason has to outlive the person who typed it.
func (s *WAFExclusionService) Create(ctx context.Context, in WAFExclusionInput) (WAFExclusionView, error) {
	if err := proxy.ValidateWAFExclusion(in.WAFExclusionSpec); err != nil {
		return WAFExclusionView{}, &WAFExclusionInvalid{Reason: err.Error()}
	}
	note := strings.TrimSpace(in.Note)
	if len(note) > 500 {
		return WAFExclusionView{}, &WAFExclusionInvalid{Reason: "the note can be at most 500 characters"}
	}
	if in.Shape() == "rule" && note == "" {
		return WAFExclusionView{}, &WAFExclusionInvalid{
			Reason: "turning a rule off needs a note saying why: it is the broadest exclusion there is, and the reason should outlive whoever added it",
		}
	}

	var appID pgtype.UUID
	if in.ApplicationID != "" {
		if err := appID.Scan(in.ApplicationID); err != nil {
			return WAFExclusionView{}, &WAFExclusionInvalid{Reason: "application is not a valid id"}
		}
		if _, err := s.queries.GetApplication(ctx, appID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return WAFExclusionView{}, ErrWAFExclusionApplication
			}
			return WAFExclusionView{}, fmt.Errorf("looking up the application: %w", err)
		}
	}

	var createdBy pgtype.UUID
	if in.CreatedBy != "" {
		_ = createdBy.Scan(in.CreatedBy)
	}
	text := func(v string) pgtype.Text { return pgtype.Text{String: v, Valid: v != ""} }

	row, err := s.queries.CreateWAFExclusion(ctx, generated.CreateWAFExclusionParams{
		RuleID:        int32(in.RuleID),
		Collection:    text(in.Collection),
		TargetName:    text(in.TargetName),
		PathPattern:   text(in.PathPattern),
		ApplicationID: appID,
		Note:          note,
		CreatedBy:     createdBy,
		Column8:       int32(proxy.MaxWAFExclusions),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return WAFExclusionView{}, ErrWAFExclusionLimit
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return WAFExclusionView{}, ErrWAFExclusionDuplicate
	}
	if err != nil {
		return WAFExclusionView{}, fmt.Errorf("saving the exclusion: %w", err)
	}

	// Read it back through the list, so the answer carries the application's name
	// and domains exactly as the list will show them.
	all, err := s.List(ctx)
	if err != nil {
		return WAFExclusionView{}, err
	}
	for _, v := range all {
		if v.ID == row.ID {
			return v, nil
		}
	}
	return WAFExclusionView{}, fmt.Errorf("the exclusion was saved but could not be read back")
}

// Delete removes one and returns what it was, for the audit trail.
func (s *WAFExclusionService) Delete(ctx context.Context, id int64) (WAFExclusionView, error) {
	all, err := s.List(ctx)
	if err != nil {
		return WAFExclusionView{}, err
	}
	var was *WAFExclusionView
	for i := range all {
		if all[i].ID == id {
			was = &all[i]
			break
		}
	}
	if was == nil {
		return WAFExclusionView{}, ErrWAFExclusionNotFound
	}
	if _, err := s.queries.DeleteWAFExclusion(ctx, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return WAFExclusionView{}, ErrWAFExclusionNotFound
		}
		return WAFExclusionView{}, fmt.Errorf("removing the exclusion: %w", err)
	}
	return *was, nil
}
