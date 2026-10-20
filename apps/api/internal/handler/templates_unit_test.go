package handler

import (
	"testing"

	"github.com/belune-hq/belune/internal/template"
)

func TestValidateTemplateInputs(t *testing.T) {
	m := &template.Manifest{
		Inputs: []template.Input{
			{Key: "admin_email", Label: "Email", Required: true, Validation: "email"},
			{Key: "site", Label: "Site", Validation: "url"},
			{Key: "opt", Label: "Opt", Default: "def"},
		},
	}

	t.Run("required missing", func(t *testing.T) {
		if _, reason := validateTemplateInputs(m, map[string]string{}); reason == "" {
			t.Fatal("expected required-input error")
		}
	})
	t.Run("bad email", func(t *testing.T) {
		if _, reason := validateTemplateInputs(m, map[string]string{"admin_email": "nope"}); reason == "" {
			t.Fatal("expected email validation error")
		}
	})
	t.Run("bad url", func(t *testing.T) {
		_, reason := validateTemplateInputs(m, map[string]string{"admin_email": "a@b.com", "site": "ftp://x"})
		if reason == "" {
			t.Fatal("expected url validation error")
		}
	})
	t.Run("applies default and passes", func(t *testing.T) {
		out, reason := validateTemplateInputs(m, map[string]string{"admin_email": "a@b.com"})
		if reason != "" {
			t.Fatalf("unexpected error: %s", reason)
		}
		if out["opt"] != "def" {
			t.Errorf("default not applied, got %q", out["opt"])
		}
	})
}

func TestTemplateDBConn(t *testing.T) {
	pg := templateDBConn("postgres", "proj-db-abcd1234", map[string]string{"user": "postgres", "password": "pw", "database": "db"})
	if pg.Host != "proj-db-abcd1234" || pg.Port != "5432" {
		t.Errorf("postgres host/port wrong: %+v", pg)
	}
	if pg.URL != "postgresql://postgres:pw@proj-db-abcd1234:5432/db" {
		t.Errorf("postgres url wrong: %s", pg.URL)
	}

	mongo := templateDBConn("mongo", "h", map[string]string{"username": "admin", "password": "pw"})
	if mongo.User != "admin" || mongo.Port != "27017" {
		t.Errorf("mongo conn wrong: %+v", mongo)
	}

	redis := templateDBConn("redis", "h", map[string]string{"password": "pw"})
	if redis.URL != "redis://:pw@h:6379" {
		t.Errorf("redis url wrong: %s", redis.URL)
	}
}

func TestFirstRoutableService(t *testing.T) {
	m := &template.Manifest{Services: []template.Service{
		{Name: "worker", Port: 0},
		{Name: "web", Port: 8080},
	}}
	svc := firstRoutableService(m)
	if svc == nil || svc.Name != "web" {
		t.Fatalf("expected web, got %+v", svc)
	}

	none := firstRoutableService(&template.Manifest{Services: []template.Service{{Name: "x"}}})
	if none != nil {
		t.Errorf("expected nil for no routable service, got %+v", none)
	}
}
