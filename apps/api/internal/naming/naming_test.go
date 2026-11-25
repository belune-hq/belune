package naming

import "testing"

func TestVolumeNameFor(t *testing.T) {
	for _, tc := range []struct{ name, app, volume, external, want string }{
		{"managed unchanged", "12345678-abcd", "data", "", "belune-vol-12345678-data"},
		{"short identifier", "abc", "data", "", "belune-vol-abc-data"},
		{"adopted is literal", "12345678-abcd", "data", "MyApp_pgdata.1", "MyApp_pgdata.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := VolumeNameFor(tc.app, tc.volume, tc.external); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
