package diagnostic

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gentleman-Programming/engram/internal/store"
	_ "modernc.org/sqlite"
)

func newDiagnosticTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, _ := newDiagnosticTestStoreWithDataDir(t)
	return s
}

func newDiagnosticTestStoreWithDataDir(t *testing.T) (*store.Store, string) {
	t.Helper()
	cfg, err := store.DefaultConfig()
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.DataDir = t.TempDir()
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, cfg.DataDir
}

func seedDiagnosticPendingMutation(t *testing.T, dataDir, project, entity, entityKey, op, payload string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "engram.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(
		`INSERT INTO sync_mutations (target_key, entity, entity_key, op, payload, source, project) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		store.DefaultSyncTargetKey, entity, entityKey, op, payload, store.SyncSourceLocal, project,
	); err != nil {
		t.Fatalf("insert sync mutation %q: %v", entityKey, err)
	}
}

func TestSQLiteLockContentionBranches(t *testing.T) {
	s := newDiagnosticTestStore(t)
	tests := []struct {
		name       string
		snapshot   store.SQLiteLockSnapshot
		probeErr   error
		wantStatus string
		wantReason string
	}{
		{
			name:       "healthy snapshot is ok",
			snapshot:   store.SQLiteLockSnapshot{JournalMode: "wal", BusyTimeoutMS: 5000, CheckpointBusy: 0, CheckpointLog: 2, CheckpointedFrames: 2},
			wantStatus: StatusOK,
			wantReason: CheckSQLiteLockContention + "_ok",
		},
		{
			name:       "checkpoint busy is warning",
			snapshot:   store.SQLiteLockSnapshot{JournalMode: "wal", BusyTimeoutMS: 5000, CheckpointBusy: 3, CheckpointLog: 7, CheckpointedFrames: 4},
			wantStatus: StatusWarning,
			wantReason: "sqlite_lock_contention_detected",
		},
		{
			name:       "probe failure is error",
			probeErr:   errors.New("probe unavailable"),
			wantStatus: StatusError,
			wantReason: "sqlite_lock_probe_failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report, err := NewRunner().RunOne(context.Background(), Scope{
				Store:   s,
				Project: "engram",
				ReadSQLiteLockSnapshot: func(context.Context) (store.SQLiteLockSnapshot, error) {
					return tc.snapshot, tc.probeErr
				},
			}, CheckSQLiteLockContention)
			if err != nil {
				t.Fatalf("RunOne: %v", err)
			}
			if report.Status != tc.wantStatus || report.Checks[0].ReasonCode != tc.wantReason {
				t.Fatalf("status=%s reason=%s report=%+v", report.Status, report.Checks[0].ReasonCode, report)
			}
		})
	}
}

func TestRegistryLookupAndOrdering(t *testing.T) {
	codes := RegisteredCodes()
	want := []string{CheckManualSessionNameProjectMismatch, CheckSessionProjectDirectoryMismatch, CheckSQLiteLockContention, CheckSyncMutationRequiredFields}
	if strings.Join(codes, ",") != strings.Join(want, ",") {
		t.Fatalf("RegisteredCodes = %v, want %v", codes, want)
	}
	if _, err := DefaultRegistry().Lookup("not_real"); err == nil {
		t.Fatal("expected invalid check error")
	}
}

func TestRunnerRollsUpBlockedFindings(t *testing.T) {
	s := newDiagnosticTestStore(t)
	runner := NewRunnerWithRegistry(NewRegistry(fakeBlockedCheck{}))
	report, err := runner.RunOne(context.Background(), Scope{Store: s, Project: "engram", Now: time.Now()}, "fake_blocked")
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if report.Status != StatusBlocked || report.Summary.Blocked != 1 {
		t.Fatalf("status=%s summary=%+v", report.Status, report.Summary)
	}
	if got := report.Checks[0].Findings[0].ReasonCode; got != "fake_blocked_reason" {
		t.Fatalf("reason_code=%q", got)
	}
}

type fakeBlockedCheck struct{}

func (fakeBlockedCheck) Code() string { return "fake_blocked" }
func (fakeBlockedCheck) Run(context.Context, Scope) (CheckResult, error) {
	return resultFromFindings("fake_blocked", map[string]any{"evaluated": true}, []Finding{{CheckID: "fake_blocked", Severity: SeverityBlocking, ReasonCode: "fake_blocked_reason", Message: "blocked", Why: "test", Evidence: mustJSON(map[string]any{"ok": false}), SafeNextStep: "none"}}), nil
}

func TestSessionProjectDirectoryMismatchFinding(t *testing.T) {
	s := newDiagnosticTestStore(t)
	if err := s.CreateSession("s1", "api", "/work/web"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	report, err := NewRunner().RunOne(context.Background(), Scope{
		Store:   s,
		Project: "api",
		DetectProject: func(dir string) (DetectedProject, bool) {
			if dir == "/work/web" {
				return DetectedProject{Project: "web", Source: "test", Path: dir}, true
			}
			return DetectedProject{}, false
		},
	}, CheckSessionProjectDirectoryMismatch)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if report.Status != StatusWarning || len(report.Checks[0].Findings) != 1 {
		t.Fatalf("report=%+v", report)
	}
}

func TestRunnerRunAllHealthyEvaluatesEveryMVPCheck(t *testing.T) {
	s := newDiagnosticTestStore(t)
	if err := s.CreateSession("manual-save-engram", "engram", "/work/engram"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	report, err := NewRunner().RunAll(context.Background(), Scope{
		Store:   s,
		Project: "engram",
		ReadSQLiteLockSnapshot: func(context.Context) (store.SQLiteLockSnapshot, error) {
			return store.SQLiteLockSnapshot{JournalMode: "wal", BusyTimeoutMS: 5000, CheckpointBusy: 0}, nil
		},
	})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if report.Status != StatusOK || report.Summary.OK != 4 || len(report.Checks) != 4 {
		t.Fatalf("report=%+v", report)
	}
	for _, check := range report.Checks {
		if check.Result != StatusOK || len(check.Evidence) == 0 {
			t.Fatalf("expected ok check with evidence, got %+v", check)
		}
	}
}

func TestSyncMutationRequiredFieldsSeparatesQuarantinedEvidenceFromBlockingWork(t *testing.T) {
	s, dataDir := newDiagnosticTestStoreWithDataDir(t)
	seedDiagnosticPendingMutation(t, dataDir, "engram", store.SyncEntitySession, "poison", store.SyncOpUpsert, `{"id":"poison"}`)

	runCheck := func(stage string) Report {
		t.Helper()
		report, err := NewRunner().RunOne(context.Background(), Scope{Store: s, Project: "engram"}, CheckSyncMutationRequiredFields)
		if err != nil {
			t.Fatalf("RunOne %s: %v", stage, err)
		}
		return report
	}

	if report := runCheck("before quarantine"); report.Status != StatusBlocked {
		t.Fatalf("expected blocked report before quarantine, got %+v", report)
	}

	quarantine, err := s.QuarantineIrreparableSyncMutations("engram", true)
	if err != nil || len(quarantine.Actions) != 1 {
		t.Fatalf("quarantine report=%+v err=%v", quarantine, err)
	}

	report := runCheck("after quarantine")
	if report.Status == StatusBlocked || report.Summary.Blocked != 0 {
		t.Fatalf("quarantined mutation still blocks doctor: %+v", report)
	}
	check := report.Checks[0]
	if check.Result == StatusBlocked || check.Severity == SeverityBlocking {
		t.Fatalf("quarantined mutation still blocks the check: %+v", check)
	}
	if len(check.Findings) != 1 {
		t.Fatalf("expected the quarantined row to stay visible as evidence, got %+v", check.Findings)
	}
	finding := check.Findings[0]
	if finding.Severity != SeverityInfo || finding.ReasonCode != "sync_mutation_quarantined" || finding.RequiresConfirmation {
		t.Fatalf("unexpected quarantined finding: %+v", finding)
	}
	var evidence map[string]any
	if err := json.Unmarshal(finding.Evidence, &evidence); err != nil {
		t.Fatalf("finding evidence invalid: %v", err)
	}
	if evidence["entity_key"] != "poison" || evidence["disposition"] != store.SyncMutationDispositionQuarantined {
		t.Fatalf("quarantined evidence lost mutation identity: %v", evidence)
	}
	if reason, _ := evidence["disposition_reason"].(string); strings.TrimSpace(reason) == "" {
		t.Fatalf("quarantined evidence lost the disposition reason: %v", evidence)
	}

	seedDiagnosticPendingMutation(t, dataDir, "engram", store.SyncEntityObservation, "obs-missing", store.SyncOpUpsert, `{"sync_id":"obs-missing"}`)
	report = runCheck("with new blocking work")
	if report.Status != StatusBlocked {
		t.Fatalf("quarantined evidence masked genuinely blocking work: %+v", report)
	}
	check = report.Checks[0]
	if len(check.Findings) != 2 {
		t.Fatalf("expected blocking and quarantined findings, got %+v", check.Findings)
	}
	if check.Findings[0].Severity != SeverityBlocking || check.Findings[0].ReasonCode != "sync_mutation_payload_missing_required_fields" {
		t.Fatalf("blocking finding must lead the roll-up: %+v", check.Findings[0])
	}
	if check.ReasonCode != "sync_mutation_payload_missing_required_fields" {
		t.Fatalf("check reason code should describe the blocking finding, got %q", check.ReasonCode)
	}
	if check.Findings[1].ReasonCode != "sync_mutation_quarantined" {
		t.Fatalf("quarantined evidence dropped: %+v", check.Findings[1])
	}
}
