package service

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/store/generated"
)

// ServerService resolves rows in the servers table. Until the agent exists the
// only server is the control plane's own host, so the only thing callers need
// from here is its id, to place newly created projects on it.
type ServerService struct {
	queries *generated.Queries

	mu      sync.RWMutex
	localID pgtype.UUID
}

func NewServerService(queries *generated.Queries) *ServerService {
	return &ServerService{queries: queries}
}

// LocalServerID returns the id of the control plane's own server row.
//
// The lookup is by is_local, never a hardcoded UUID: the row is seeded with a
// generated id precisely so no magic value leaks into code or tests. Its id is
// then cached for the process lifetime — a migration creates the row and a
// partial unique index keeps it the only one, so it can neither change nor
// disappear underneath us. Only the id is cached; the observed facts on that
// row (last_seen_at, agent_version, …) change constantly and must be read.
func (s *ServerService) LocalServerID(ctx context.Context) (pgtype.UUID, error) {
	s.mu.RLock()
	cached := s.localID
	s.mu.RUnlock()
	if cached.Valid {
		return cached, nil
	}

	server, err := s.queries.GetLocalServer(ctx)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("looking up the local server: %w", err)
	}

	s.mu.Lock()
	s.localID = server.ID
	s.mu.Unlock()

	return server.ID, nil
}
