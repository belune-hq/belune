package service

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/service/email"
	"github.com/belune-hq/belune/internal/store/generated"
)

// SMTP settings keys in the generic settings table. The password is stored
// keyring-encrypted (base64 of the envelope) and is redacted from the generic
// settings listing — everything else is non-secret connection metadata.
const (
	SettingSMTPHost      = "smtp_host"
	SettingSMTPPort      = "smtp_port"
	SettingSMTPUser      = "smtp_user"
	SettingSMTPPassword  = "smtp_password_encrypted"
	SettingSMTPFromEmail = "smtp_from_email"
	SettingSMTPFromName  = "smtp_from_name"
	SettingSMTPTLSMode   = "smtp_tls_mode"
)

// SMTPSettingsService reads and writes the mail-server configuration, layering
// DB overrides over the env defaults. It implements email.SMTPResolver so the
// email service picks up changes without a restart.
type SMTPSettingsService struct {
	queries *generated.Queries
	keyring *crypto.Keyring
	cfg     *config.Config
}

func NewSMTPSettingsService(queries *generated.Queries, keyring *crypto.Keyring, cfg *config.Config) *SMTPSettingsService {
	return &SMTPSettingsService{queries: queries, keyring: keyring, cfg: cfg}
}

// SMTPView is the masked, UI-facing view of the effective config: real values
// for the non-secret fields, and only whether a password is set.
type SMTPView struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	User        string `json:"user"`
	FromEmail   string `json:"from_email"`
	FromName    string `json:"from_name"`
	TLSMode     string `json:"tls_mode"`
	PasswordSet bool   `json:"password_set"`
}

// SMTPSaveParams carries a full form submission. Password is optional: nil
// preserves the stored secret (blank field), non-nil replaces it.
type SMTPSaveParams struct {
	Host      string
	Port      int
	User      string
	FromEmail string
	FromName  string
	TLSMode   string
	Password  *string
}

var validTLSModes = map[string]bool{"none": true, "starttls": true, "tls": true}

// ResolveSMTP returns the effective config (DB over env), decrypting the stored
// password. Satisfies email.SMTPResolver.
func (s *SMTPSettingsService) ResolveSMTP(ctx context.Context) (email.SMTPConfig, error) {
	cfg := email.SMTPConfig{
		Host:      s.cfg.SMTPHost,
		Port:      s.cfg.SMTPPort,
		User:      s.cfg.SMTPUser,
		Password:  s.cfg.SMTPPassword,
		FromEmail: s.cfg.SMTPFromEmail,
		FromName:  s.cfg.SMTPFromName,
		TLSMode:   s.cfg.SMTPTLSMode,
	}

	if v, ok := s.read(ctx, SettingSMTPHost); ok {
		cfg.Host = v
	}
	if v, ok := s.read(ctx, SettingSMTPUser); ok {
		cfg.User = v
	}
	if v, ok := s.read(ctx, SettingSMTPFromEmail); ok {
		cfg.FromEmail = v
	}
	if v, ok := s.read(ctx, SettingSMTPFromName); ok {
		cfg.FromName = v
	}
	if v, ok := s.read(ctx, SettingSMTPTLSMode); ok && v != "" {
		cfg.TLSMode = v
	}
	if v, ok := s.read(ctx, SettingSMTPPort); ok {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			cfg.Port = p
		}
	}
	if v, ok := s.read(ctx, SettingSMTPPassword); ok && v != "" {
		pw, err := s.decryptPassword(v)
		if err != nil {
			return cfg, fmt.Errorf("smtp: decrypt stored password: %w", err)
		}
		cfg.Password = pw
	}
	return cfg, nil
}

// Get returns the masked effective config for the settings form.
func (s *SMTPSettingsService) Get(ctx context.Context) (SMTPView, error) {
	cfg, err := s.ResolveSMTP(ctx)
	if err != nil {
		return SMTPView{}, err
	}
	return SMTPView{
		Host:        cfg.Host,
		Port:        cfg.Port,
		User:        cfg.User,
		FromEmail:   cfg.FromEmail,
		FromName:    cfg.FromName,
		TLSMode:     cfg.TLSMode,
		PasswordSet: cfg.Password != "",
	}, nil
}

// Save validates and writes the SMTP settings. A nil Password preserves the
// stored secret.
func (s *SMTPSettingsService) Save(ctx context.Context, p SMTPSaveParams) error {
	mode := strings.TrimSpace(p.TLSMode)
	if mode == "" {
		mode = "starttls"
	}
	if !validTLSModes[mode] {
		return fmt.Errorf("invalid TLS mode %q (expected none, starttls, or tls)", p.TLSMode)
	}
	port := p.Port
	if port == 0 {
		port = 587
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid port %d", p.Port)
	}

	writes := map[string]string{
		SettingSMTPHost:      strings.TrimSpace(p.Host),
		SettingSMTPPort:      strconv.Itoa(port),
		SettingSMTPUser:      strings.TrimSpace(p.User),
		SettingSMTPFromEmail: strings.TrimSpace(p.FromEmail),
		SettingSMTPFromName:  strings.TrimSpace(p.FromName),
		SettingSMTPTLSMode:   mode,
	}
	if p.Password != nil {
		if *p.Password == "" {
			writes[SettingSMTPPassword] = "" // explicit clear
		} else {
			enc, err := s.encryptPassword(*p.Password)
			if err != nil {
				return err
			}
			writes[SettingSMTPPassword] = enc
		}
	} else if _, ok := s.read(ctx, SettingSMTPPassword); !ok {
		// Blank password with none stored yet: adopt the effective (env) password
		// into the DB so the config is self-contained. Otherwise a saved host
		// override would keep pairing with the leftover env password per-field,
		// and the form's "leave blank to keep" would quietly drop it.
		if eff, err := s.ResolveSMTP(ctx); err == nil && eff.Password != "" {
			enc, err := s.encryptPassword(eff.Password)
			if err != nil {
				return err
			}
			writes[SettingSMTPPassword] = enc
		}
	}

	for k, v := range writes {
		if _, err := s.queries.UpsertSetting(ctx, generated.UpsertSettingParams{Key: k, Value: v}); err != nil {
			return fmt.Errorf("save %s: %w", k, err)
		}
	}
	return nil
}

func (s *SMTPSettingsService) read(ctx context.Context, key string) (string, bool) {
	row, err := s.queries.GetSetting(ctx, key)
	if err != nil {
		return "", false
	}
	return row.Value, true
}

func (s *SMTPSettingsService) encryptPassword(plaintext string) (string, error) {
	enc, err := s.keyring.Encrypt([]byte(plaintext))
	if err != nil {
		return "", fmt.Errorf("smtp: encrypt password: %w", err)
	}
	return base64.StdEncoding.EncodeToString(enc), nil
}

func (s *SMTPSettingsService) decryptPassword(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("smtp: decode password: %w", err)
	}
	dec, err := s.keyring.Decrypt(raw)
	if err != nil {
		return "", err
	}
	return string(dec), nil
}
