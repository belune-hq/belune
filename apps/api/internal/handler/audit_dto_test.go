package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/belune-hq/belune/internal/store/generated"
)

// The details column is JSONB; the DTO must emit it as raw JSON (not a base64
// string) and render a NULL detail as `null`.
func TestAuditLogDTO_DetailsAsRawJSON(t *testing.T) {
	rows := []generated.ListAuditLogsFilteredRow{
		{Action: "upsert_quota", Details: []byte(`{"scope":"user"}`)},
		{Action: "login", Details: nil},
	}

	b, err := json.Marshal(toAuditLogDTOs(rows))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)

	if !strings.Contains(got, `"details":{"scope":"user"}`) {
		t.Errorf("expected raw JSON details object, got: %s", got)
	}
	if !strings.Contains(got, `"details":null`) {
		t.Errorf("expected null for nil details, got: %s", got)
	}
	// A base64 encoding of the JSON bytes must not leak through.
	if strings.Contains(got, "eyJ") {
		t.Errorf("details leaked as base64: %s", got)
	}
}
