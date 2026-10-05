package service

import (
	"context"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/weiliang79/belune/internal/pkg/crypto"
	"github.com/weiliang79/belune/internal/store"
	"github.com/weiliang79/belune/internal/store/generated"
)

// EnvKeyRegex is the shape of a valid variable name, shared by the REST
// replace endpoint and the merge below so the two cannot disagree.
var EnvKeyRegex = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// MaxEnvVarsPerMerge bounds one call. The caller of a merge may be an LLM
// composing the list itself; a runaway list is a mistake worth refusing rather
// than encrypting and writing.
const MaxEnvVarsPerMerge = 100

// InvalidEnvVarsError is a problem with the submitted variables, safe to show
// the caller verbatim; anything else returned from Merge is internal.
type InvalidEnvVarsError struct{ Msg string }

func (e *InvalidEnvVarsError) Error() string { return e.Msg }

// EnvVarService writes an application's environment variables.
type EnvVarService struct {
	db      *pgxpool.Pool
	queries *generated.Queries
	keyring *crypto.Keyring
	apps    *ApplicationService
}

func NewEnvVarService(db *pgxpool.Pool, queries *generated.Queries, keyring *crypto.Keyring, apps *ApplicationService) *EnvVarService {
	return &EnvVarService{db: db, queries: queries, keyring: keyring, apps: apps}
}

// EnvVarSet is one variable to set. IsSecret is a pointer so "not mentioned"
// (keep what is stored, or default for a new key) differs from an explicit
// false.
type EnvVarSet struct {
	Key      string
	Value    string
	IsSecret *bool
}

// EnvVarResult reports what a merge did to one variable. It carries no value:
// the point of a secret is that it is not echoed back.
type EnvVarResult struct {
	Key      string `json:"key"`
	IsSecret bool   `json:"is_secret"`
	Created  bool   `json:"created"`
}

// Merge sets the given variables and leaves every other variable exactly as it
// is — including its stored ciphertext, which is never rewritten. This is
// deliberately NOT the REST update's behaviour, which replaces the whole set
// and deletes whatever it does not list; an assistant forwarding two variables
// to that would delete the rest, secrets included. Merge never deletes, and
// never calls DeleteEnvVarsNotIn.
//
// A new key defaults to secret: an assistant setting an API key it did not
// label would otherwise store it as a plain value that the list endpoint shows
// to anyone with read access. An existing key keeps its stored flag unless the
// caller says otherwise.
//
// Everything is validated and encrypted before the first write, and the writes
// share one transaction, so a bad entry never leaves the set half-applied. The
// caller authorizes access.
func (s *EnvVarService) Merge(ctx context.Context, appID pgtype.UUID, vars []EnvVarSet) ([]EnvVarResult, error) {
	if len(vars) == 0 {
		return nil, &InvalidEnvVarsError{"no variables given"}
	}
	if len(vars) > MaxEnvVarsPerMerge {
		return nil, &InvalidEnvVarsError{fmt.Sprintf("too many variables: at most %d per call", MaxEnvVarsPerMerge)}
	}

	existing, err := s.queries.ListEnvVarsByApplication(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("loading existing env vars: %w", err)
	}
	stored := make(map[string]bool, len(existing))
	for _, ev := range existing {
		stored[ev.Key] = ev.IsSecret
	}

	type prepared struct {
		key       string
		encrypted []byte
		isSecret  bool
		created   bool
	}
	out := make([]prepared, 0, len(vars))
	seen := make(map[string]struct{}, len(vars))
	for _, v := range vars {
		if !EnvKeyRegex.MatchString(v.Key) {
			return nil, &InvalidEnvVarsError{fmt.Sprintf("invalid env var key: %q", v.Key)}
		}
		// Which duplicate wins would depend on order, so refuse instead.
		if _, dup := seen[v.Key]; dup {
			return nil, &InvalidEnvVarsError{fmt.Sprintf("duplicate env var key: %q", v.Key)}
		}
		seen[v.Key] = struct{}{}

		wasSecret, exists := stored[v.Key]
		isSecret := true
		switch {
		case v.IsSecret != nil:
			isSecret = *v.IsSecret
		case exists:
			isSecret = wasSecret
		}

		encrypted, err := s.keyring.Encrypt([]byte(v.Value))
		if err != nil {
			return nil, fmt.Errorf("encrypting value: %w", err)
		}
		out = append(out, prepared{key: v.Key, encrypted: encrypted, isSecret: isSecret, created: !exists})
	}

	if err := store.WithTx(ctx, s.db, func(q *generated.Queries) error {
		for _, p := range out {
			if _, err := q.UpsertEnvVar(ctx, generated.UpsertEnvVarParams{
				ApplicationID:  appID,
				Key:            p.key,
				ValueEncrypted: p.encrypted,
				IsSecret:       p.isSecret,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("saving env vars: %w", err)
	}

	// Same marker the REST update stamps: the running container still has the
	// old values until it is reloaded.
	s.apps.MarkConfigChanged(ctx, appID)

	results := make([]EnvVarResult, len(out))
	for i, p := range out {
		results[i] = EnvVarResult{Key: p.key, IsSecret: p.isSecret, Created: p.created}
	}
	return results, nil
}
