package eventwatcher

import (
	"testing"

	"github.com/belune-hq/belune/internal/runtime"
)

// ListContainers returns stopped containers too (All: true), and they keep
// their labels — so the state filter is the only thing preventing an exited
// container from being read as a live service.
func TestRunningIndex(t *testing.T) {
	containers := []runtime.ContainerInfo{
		{
			Name: "proj-app-up", Status: "running",
			Labels: map[string]string{labelApplicationID: "app-up"},
		},
		{
			// The regression: after a host or control-plane restart every
			// container sits in "exited" but still carries its labels.
			Name: "proj-app-down", Status: "exited",
			Labels: map[string]string{labelApplicationID: "app-down"},
		},
		{
			Name: "db-up", Status: "running",
			Labels: map[string]string{labelDatabaseID: "db-up"},
		},
		{
			Name: "db-down", Status: "exited",
			Labels: map[string]string{labelDatabaseID: "db-down"},
		},
		{
			// Created but never started is not up either.
			Name: "proj-app-created", Status: "created",
			Labels: map[string]string{labelApplicationID: "app-created"},
		},
		// Legacy database with no label, recognised by name.
		{Name: "legacy-db", Status: "running", Labels: map[string]string{}},
	}

	idx := buildContainerIndex(containers)

	if got, ok := idx.runningApps["app-up"]; !ok || got != "proj-app-up" {
		t.Errorf("running app missing or misnamed: %q, present=%v", got, ok)
	}
	for _, id := range []string{"app-down", "app-created"} {
		if _, ok := idx.runningApps[id]; ok {
			t.Errorf("%s: a non-running container must not count as a live app", id)
		}
	}

	if !idx.runningDBs["db-up"] {
		t.Error("running database should be indexed")
	}
	if idx.runningDBs["db-down"] {
		t.Error("exited database must not count as running")
	}

	if !idx.runningNames["legacy-db"] {
		t.Error("running unlabelled container should be indexed by name")
	}
	if idx.runningNames["proj-app-down"] || idx.runningNames["db-down"] {
		t.Error("exited containers must not appear in the name index")
	}

	// "created" is a passing phase, not a settled outcome, so it is transient
	// rather than simply absent — reconcile must leave such a row alone.
	if !idx.transientApps["app-created"] {
		t.Error("a created-not-started container should be transient, not treated as gone")
	}
	if idx.transientApps["app-down"] {
		t.Error("an exited container is settled, not transient")
	}
}

// Containers are created with RestartPolicy "unless-stopped", so a crash-looping
// container genuinely sits in "restarting". Reconciliation samples only at
// startup and on reconnect, so it must not turn that passing phase into a stored
// verdict — least of all "stopped", which means deliberate here.
func TestTransientStatesAreNotSettled(t *testing.T) {
	transient := []string{"restarting", "removing", "created", "paused"}
	for _, st := range transient {
		if !isTransientState(st) {
			t.Errorf("%q should be transient", st)
		}
	}
	for _, st := range []string{"running", "exited", "dead"} {
		if isTransientState(st) {
			t.Errorf("%q is a settled state, not transient", st)
		}
	}

	idx := buildContainerIndex([]runtime.ContainerInfo{
		{
			Name: "proj-app-flapping", Status: "restarting",
			Labels: map[string]string{labelApplicationID: "app-flapping"},
		},
		{
			Name: "db-flapping", Status: "restarting",
			Labels: map[string]string{labelDatabaseID: "db-flapping"},
		},
	})

	if _, ok := idx.runningApps["app-flapping"]; ok {
		t.Error("a restarting container is not proof the app is up")
	}
	if !idx.transientApps["app-flapping"] {
		t.Error("a restarting application must be marked transient so reconcile skips it")
	}
	if !idx.transientDBs["db-flapping"] {
		t.Error("a restarting database must be marked transient so reconcile skips it")
	}
	if !idx.transientNames["db-flapping"] {
		t.Error("transient containers should be indexed by name for legacy databases")
	}
}

// An empty host must yield empty (non-nil) sets, so reconcile treats every
// application as stopped rather than panicking or skipping the correction.
func TestRunningIndexEmpty(t *testing.T) {
	idx := buildContainerIndex(nil)
	if idx.runningApps == nil || idx.runningDBs == nil || idx.runningNames == nil {
		t.Fatal("runningIndex must return initialised maps")
	}
	if len(idx.runningApps) != 0 || len(idx.runningDBs) != 0 || len(idx.runningNames) != 0 {
		t.Errorf("expected empty sets, got %d/%d/%d", len(idx.runningApps), len(idx.runningDBs), len(idx.runningNames))
	}
}
