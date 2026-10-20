package worker

import (
	"strings"
	"testing"

	"github.com/belune-hq/belune/internal/store/generated"
)

func TestDBDumpSpec(t *testing.T) {
	creds := map[string]string{
		"user": "u", "password": "p", "database": "d", "username": "mu",
	}
	cases := []struct {
		dbType         string
		ok             bool
		dumpContains   string
		restoreContain string
	}{
		{"postgres", true, "pg_dump", "psql"},
		{"mysql", true, "mysqldump --single-transaction", "mysql -u"},
		{"mongo", true, "mongodump", "mongorestore"},
		{"redis", false, "", ""},
		{"other", false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.dbType, func(t *testing.T) {
			spec := dbDumpSpec(c.dbType, creds, "")
			if spec.ok != c.ok {
				t.Fatalf("ok = %v, want %v", spec.ok, c.ok)
			}
			if !c.ok {
				return
			}
			dump := strings.Join(spec.dump, " ")
			restore := strings.Join(spec.restore, " ")
			if !strings.Contains(dump, c.dumpContains) {
				t.Errorf("dump %q missing %q", dump, c.dumpContains)
			}
			if !strings.Contains(restore, c.restoreContain) {
				t.Errorf("restore %q missing %q", restore, c.restoreContain)
			}
		})
	}
}

func TestDbDumpSpecTarget(t *testing.T) {
	creds := map[string]string{
		"user": "u", "password": "p", "database": "managed", "root_password": "rp",
		"username": "mu",
	}
	cases := []struct {
		name         string
		dbType       string
		target       string
		dumpContains string
	}{
		{"pg default", "postgres", "", "pg_dump -U 'u' -d 'managed'"},
		{"pg specific", "postgres", "postgres", "pg_dump -U 'u' -d 'postgres'"},
		{"pg cluster", "postgres", "*", "pg_dumpall -U 'u'"},
		{"pg multi", "postgres", "one,two", "pg_dump -U 'u' -d 'one' --create --clean --if-exists --no-owner && PGPASSWORD='p' pg_dump -U 'u' -d 'two' --create"},
		{"mysql default", "mysql", "", "mysqldump --single-transaction --routines --triggers -u root 'managed'"},
		{"mysql cluster", "mysql", "*", "mysqldump --single-transaction --routines --triggers -u root --all-databases"},
		{"mysql multi", "mysql", "one,two", "mysqldump --single-transaction --routines --triggers -u root --databases 'one' 'two'"},
		{"mongo default", "mongo", "", "mongodump --username 'mu'"},
		{"mongo specific", "mongo", "other", "--db 'other' --archive"},
		{"mongo multi", "mongo", "a,b", "--nsInclude 'a.*' --nsInclude 'b.*' --archive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := dbDumpSpec(c.dbType, creds, c.target)
			dump := strings.Join(spec.dump, " ")
			if !strings.Contains(dump, c.dumpContains) {
				t.Errorf("dump %q missing %q", dump, c.dumpContains)
			}
		})
	}
}

func TestShArgEscapesQuotes(t *testing.T) {
	// A password with a single quote must be escaped so it cannot break out of
	// the surrounding single-quoted shell argument.
	got := shArg("p'wd")
	want := `'p'\''wd'`
	if got != want {
		t.Fatalf("shArg = %q, want %q", got, want)
	}
}

func TestDBBackupMethod(t *testing.T) {
	cases := []struct {
		dbType     string
		backupMode string
		want       string
	}{
		{"postgres", "none", "logical"},
		{"mysql", "none", "logical"},
		{"mongo", "none", "logical"},
		{"redis", "none", "none"},
		{"other", "volume_snapshot", "volume_snapshot"},
		{"other", "command", "command"},
		{"other", "none", "none"},
	}
	for _, c := range cases {
		got := dbBackupMethod(generated.Database{Type: c.dbType, BackupMode: c.backupMode})
		if got != c.want {
			t.Errorf("dbBackupMethod(%s,%s) = %q, want %q", c.dbType, c.backupMode, got, c.want)
		}
	}
}

func TestPostgresDataDir(t *testing.T) {
	cases := []struct {
		version string
		want    string
	}{
		{"16", "/var/lib/postgresql/data"},
		{"17", "/var/lib/postgresql/data"},
		{"17.4", "/var/lib/postgresql/data"},
		{"18", "/var/lib/postgresql"},
		{"18.1", "/var/lib/postgresql"},
		{"18-alpine", "/var/lib/postgresql"},
		{"19", "/var/lib/postgresql"},
		{"latest", "/var/lib/postgresql"},
		{"alpine", "/var/lib/postgresql"},
	}
	for _, c := range cases {
		if got := postgresDataDir(c.version); got != c.want {
			t.Errorf("postgresDataDir(%q) = %q, want %q", c.version, got, c.want)
		}
	}
}
