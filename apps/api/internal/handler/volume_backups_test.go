package handler

import "testing"

func TestValidateVolumeBackupSchedule(t *testing.T) {
	i32 := func(v int32) *int32 { return &v }

	cases := []struct {
		name    string
		req     volumeBackupConfigRequest
		wantOK  bool
		wantMsg string
	}{
		{
			name:   "empty schedule is manual-only, valid",
			req:    volumeBackupConfigRequest{Schedule: ""},
			wantOK: true,
		},
		{
			name:   "valid cron",
			req:    volumeBackupConfigRequest{Schedule: "0 0 * * *"},
			wantOK: true,
		},
		{
			name:    "invalid cron",
			req:     volumeBackupConfigRequest{Schedule: "not a cron"},
			wantOK:  false,
			wantMsg: "invalid cron schedule",
		},
		{
			name:   "positive keep_latest",
			req:    volumeBackupConfigRequest{Schedule: "0 0 * * *", KeepLatest: i32(3)},
			wantOK: true,
		},
		{
			name:    "zero keep_latest rejected",
			req:     volumeBackupConfigRequest{KeepLatest: i32(0)},
			wantOK:  false,
			wantMsg: "keep_latest must be a positive number",
		},
		{
			name:    "negative keep_latest rejected",
			req:     volumeBackupConfigRequest{KeepLatest: i32(-1)},
			wantOK:  false,
			wantMsg: "keep_latest must be a positive number",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, ok := validateVolumeBackupSchedule(c.req, "UTC")
			if ok != c.wantOK {
				t.Fatalf("validateVolumeBackupSchedule() ok = %v, want %v (msg=%q)", ok, c.wantOK, msg)
			}
			if !ok && msg != c.wantMsg {
				t.Fatalf("validateVolumeBackupSchedule() msg = %q, want %q", msg, c.wantMsg)
			}
		})
	}
}
