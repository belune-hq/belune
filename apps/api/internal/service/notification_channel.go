package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/notify"
	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/store/generated"
)

// NotificationChannelService manages third-party notification channels. Each
// channel's provider config is stored as a keyring-encrypted JSON blob (every
// field in it may be a secret), mirroring BackupDestinationService: reads are
// masked to presence only and edits replace the whole blob.
type NotificationChannelService struct {
	queries       *generated.Queries
	keyring       *crypto.Keyring
	registry      *notify.Registry
	publicBaseURL string
}

func NewNotificationChannelService(queries *generated.Queries, keyring *crypto.Keyring, registry *notify.Registry, publicBaseURL string) *NotificationChannelService {
	return &NotificationChannelService{
		queries:       queries,
		keyring:       keyring,
		registry:      registry,
		publicBaseURL: strings.TrimRight(publicBaseURL, "/"),
	}
}

// SaveChannelParams carries the fields for creating or updating a channel.
// Config is the raw provider config JSON; nil on update preserves the stored
// blob (the type is immutable, so the config shape can't change out from under it).
type SaveChannelParams struct {
	Name    string
	Type    string
	Events  []string
	Enabled bool
	Config  json.RawMessage
}

// Create validates and inserts a new channel, encrypting its config.
func (s *NotificationChannelService) Create(ctx context.Context, createdBy string, p SaveChannelParams) (generated.NotificationChannel, error) {
	if len(p.Config) == 0 {
		return generated.NotificationChannel{}, fmt.Errorf("config is required")
	}
	if err := s.validate(p.Type, p.Events, p.Config); err != nil {
		return generated.NotificationChannel{}, err
	}
	enc, err := s.encryptConfig(p.Config)
	if err != nil {
		return generated.NotificationChannel{}, err
	}
	return s.queries.CreateNotificationChannel(ctx, generated.CreateNotificationChannelParams{
		Name:            p.Name,
		Type:            p.Type,
		ConfigEncrypted: enc,
		Events:          normalizeEvents(p.Events),
		Enabled:         p.Enabled,
		CreatedBy:       parseUUID(createdBy),
	})
}

// Update mutates name, events, enabled and (optionally) config. When Config is
// nil the stored blob is preserved. The type is never changed; config is
// re-validated against the existing type.
func (s *NotificationChannelService) Update(ctx context.Context, id pgtype.UUID, p SaveChannelParams) (generated.NotificationChannel, error) {
	existing, err := s.queries.GetNotificationChannel(ctx, id)
	if err != nil {
		return generated.NotificationChannel{}, err
	}
	if err := s.validateEvents(p.Events); err != nil {
		return generated.NotificationChannel{}, err
	}

	var enc []byte
	if len(p.Config) > 0 {
		// Fill any secret the operator left blank (masked on read) from the stored
		// config, then validate + encrypt the merged result.
		merged := p.Config
		if stored, derr := s.decryptConfig(existing.ConfigEncrypted); derr == nil {
			merged = notify.MergeSecrets(existing.Type, stored, p.Config)
		}
		if err := s.validateConfig(existing.Type, merged); err != nil {
			return generated.NotificationChannel{}, err
		}
		if enc, err = s.encryptConfig(merged); err != nil {
			return generated.NotificationChannel{}, err
		}
	}
	return s.queries.UpdateNotificationChannel(ctx, generated.UpdateNotificationChannelParams{
		ID:              id,
		Name:            p.Name,
		Events:          normalizeEvents(p.Events),
		Enabled:         p.Enabled,
		ConfigEncrypted: enc, // nil preserves existing (COALESCE in query)
	})
}

// SetEnabled toggles a channel without touching its config, backing the
// immediate-effect enable/disable Switch in the UI.
func (s *NotificationChannelService) SetEnabled(ctx context.Context, id pgtype.UUID, enabled bool) (generated.NotificationChannel, error) {
	return s.queries.SetNotificationChannelEnabled(ctx, generated.SetNotificationChannelEnabledParams{
		ID:      id,
		Enabled: enabled,
	})
}

// List returns all channels without their config (the query omits the encrypted
// column), so the result is inherently secret-free.
func (s *NotificationChannelService) List(ctx context.Context) ([]generated.ListNotificationChannelsRow, error) {
	return s.queries.ListNotificationChannels(ctx)
}

// Get returns a single raw channel row (including the encrypted config).
func (s *NotificationChannelService) Get(ctx context.Context, id pgtype.UUID) (generated.NotificationChannel, error) {
	return s.queries.GetNotificationChannel(ctx, id)
}

// RedactedConfig decrypts a channel's config and strips its secret fields,
// returning JSON safe to send to the admin UI for prefilling an edit form.
// Returns "{}" when the config can't be decrypted.
func (s *NotificationChannelService) RedactedConfig(channelType string, encrypted []byte) json.RawMessage {
	raw, err := s.decryptConfig(encrypted)
	if err != nil {
		return json.RawMessage("{}")
	}
	return notify.RedactConfig(channelType, raw)
}

// Delete removes a channel by id.
func (s *NotificationChannelService) Delete(ctx context.Context, id pgtype.UUID) error {
	return s.queries.DeleteNotificationChannel(ctx, id)
}

// Deliver sends ev through a single channel, re-checking enabled state at
// delivery time (a channel may have been disabled or deleted between dispatch
// and delivery). It reports delivered=false — with a nil error — when the
// channel should be skipped (missing or disabled), so the worker neither retries
// nor stamps a delivery result. A non-nil error is a genuine send failure the
// worker will retry and, on exhaustion, surface as last_error.
func (s *NotificationChannelService) Deliver(ctx context.Context, id pgtype.UUID, ev notify.Event) (delivered bool, err error) {
	row, err := s.queries.GetNotificationChannel(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil // deleted between dispatch and delivery — drop
		}
		return false, err
	}
	if !row.Enabled {
		return false, nil // disabled after dispatch — drop
	}
	provider, ok := s.registry.Provider(row.Type)
	if !ok {
		return false, fmt.Errorf("unknown channel type %q: %w", row.Type, notify.ErrPermanent)
	}
	raw, err := s.decryptConfig(row.ConfigEncrypted)
	if err != nil {
		return false, fmt.Errorf("%w: %w", err, notify.ErrPermanent)
	}
	if err := provider.Send(ctx, raw, ev); err != nil {
		return false, err
	}
	return true, nil
}

// MarkSent stamps a successful delivery (with the event that was delivered) and
// clears any prior error.
func (s *NotificationChannelService) MarkSent(ctx context.Context, id pgtype.UUID, eventType string) error {
	return s.queries.MarkNotificationChannelSent(ctx, generated.MarkNotificationChannelSentParams{
		ID:            id,
		LastEventType: pgtype.Text{String: eventType, Valid: eventType != ""},
	})
}

// MarkError records the last delivery failure (and the event it was for),
// surfaced on the channel row.
func (s *NotificationChannelService) MarkError(ctx context.Context, id pgtype.UUID, msg, eventType string) error {
	return s.queries.MarkNotificationChannelError(ctx, generated.MarkNotificationChannelErrorParams{
		ID:            id,
		LastError:     pgtype.Text{String: msg, Valid: true},
		LastEventType: pgtype.Text{String: eventType, Valid: eventType != ""},
	})
}

// Test synchronously sends a sample event through a saved channel, returning the
// provider error verbatim so the operator sees exactly why delivery failed.
func (s *NotificationChannelService) Test(ctx context.Context, id pgtype.UUID) error {
	row, err := s.queries.GetNotificationChannel(ctx, id)
	if err != nil {
		return err
	}
	raw, err := s.decryptConfig(row.ConfigEncrypted)
	if err != nil {
		return err
	}
	return s.sendTest(ctx, row.Type, raw, row.Name)
}

// TestConfig sends a sample event through ad-hoc config from the create/edit
// form, before it is saved. When config is empty and fallbackID is set (editing
// without re-entering the secret), the stored config is used instead.
func (s *NotificationChannelService) TestConfig(ctx context.Context, channelType string, config json.RawMessage, fallbackID pgtype.UUID) error {
	raw := config
	// When editing (fallbackID set), reconcile against the stored config so a
	// masked secret left blank tests with the stored value — exactly what Update
	// would persist. An entirely empty submission tests the stored config as-is.
	if fallbackID.Valid {
		if row, err := s.queries.GetNotificationChannel(ctx, fallbackID); err == nil {
			if stored, derr := s.decryptConfig(row.ConfigEncrypted); derr == nil {
				if len(raw) == 0 {
					raw = stored
				} else {
					raw = notify.MergeSecrets(channelType, stored, raw)
				}
			}
		}
	}
	if len(raw) == 0 {
		return fmt.Errorf("configuration is required to test")
	}
	return s.sendTest(ctx, channelType, raw, "")
}

// sendTest validates config and delivers a sample event through the provider.
func (s *NotificationChannelService) sendTest(ctx context.Context, channelType string, config json.RawMessage, name string) error {
	provider, ok := s.registry.Provider(channelType)
	if !ok {
		return fmt.Errorf("unknown channel type %q", channelType)
	}
	if err := provider.ValidateConfig(config); err != nil {
		return err
	}
	body := "This is a test message from Belune."
	if name != "" {
		body = fmt.Sprintf("This is a test message from the %q channel.", name)
	}
	ev := notify.Event{
		Type:       "test",
		Title:      "Belune test notification",
		Body:       body,
		Link:       s.publicBaseURL,
		OccurredAt: time.Now(),
	}
	return provider.Send(ctx, config, ev)
}

// --- validation helpers ---

func (s *NotificationChannelService) validate(channelType string, events []string, config json.RawMessage) error {
	if err := s.validateEvents(events); err != nil {
		return err
	}
	return s.validateConfig(channelType, config)
}

func (s *NotificationChannelService) validateConfig(channelType string, config json.RawMessage) error {
	provider, ok := s.registry.Provider(channelType)
	if !ok {
		return fmt.Errorf("unknown channel type %q", channelType)
	}
	return provider.ValidateConfig(config)
}

func (s *NotificationChannelService) validateEvents(events []string) error {
	for _, e := range events {
		if !notify.IsKnownEvent(e) {
			return fmt.Errorf("unknown event type %q", e)
		}
	}
	return nil
}

// --- config crypto ---

func (s *NotificationChannelService) encryptConfig(config json.RawMessage) ([]byte, error) {
	enc, err := s.keyring.Encrypt(config)
	if err != nil {
		return nil, fmt.Errorf("encrypt channel config: %w", err)
	}
	return enc, nil
}

func (s *NotificationChannelService) decryptConfig(encrypted []byte) (json.RawMessage, error) {
	if len(encrypted) == 0 {
		return nil, fmt.Errorf("channel has no stored config")
	}
	raw, err := s.keyring.Decrypt(encrypted)
	if err != nil {
		return nil, fmt.Errorf("decrypt channel config: %w", err)
	}
	return raw, nil
}

func normalizeEvents(events []string) []string {
	if events == nil {
		return []string{}
	}
	return events
}

func parseUUID(id string) pgtype.UUID {
	var u pgtype.UUID
	if id != "" {
		_ = u.Scan(id)
	}
	return u
}
