package diagnostic

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gentleman-Programming/engram/internal/cloud/constants"
	"github.com/Gentleman-Programming/engram/internal/store"
)

func newDiagnosticTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, _ := newDiagnosticTestStoreWithConfig(t)
	return s
}

func newDiagnosticTestStoreWithConfig(t *testing.T) (*store.Store, store.Config) {
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
	return s, cfg
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

// TestSyncMutationRequiredFieldsSurfacesNonEnrolledCountFailure proves the
// check fails loudly instead of reporting a clean bill of health when the
// enrollment evidence cannot be read. The enrollment table is dropped after
// migrations so payload validation still succeeds and only the cloud sync
// enrollment lookup fails.
func TestSyncMutationRequiredFieldsSurfacesNonEnrolledCountFailure(t *testing.T) {
	s, cfg := newDiagnosticTestStoreWithConfig(t)

	db, err := sql.Open("sqlite", filepath.Join(cfg.DataDir, "engram.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE sync_enrolled_projects`); err != nil {
		db.Close()
		t.Fatalf("drop sync_enrolled_projects: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close probe db: %v", err)
	}

	report, err := NewRunner().RunOne(context.Background(), Scope{Store: s, Project: "engram"}, CheckSyncMutationRequiredFields)
	if err == nil {
		t.Fatalf("expected non-enrolled count failure, got report=%+v", report)
	}
	if !strings.Contains(err.Error(), "sync_enrolled_projects") {
		t.Fatalf("expected error naming the enrollment table, got %v", err)
	}

	errReport := ErrorReport("engram", err)
	if errReport.Status != StatusError || errReport.Summary.Errors != 1 {
		t.Fatalf("expected error report, got %+v", errReport)
	}
	if errReport.Checks[0].ReasonCode != "diagnostic_error" || !strings.Contains(errReport.Checks[0].Message, "sync_enrolled_projects") {
		t.Fatalf("expected surfaced query failure, got %+v", errReport.Checks[0])
	}
}

// TestSyncMutationRequiredFieldsIgnoresBacklogWithoutCloudEnrollment proves a
// local-only install is never reported as blocked for a non-enrolled backlog.
// The store journals sync mutations unconditionally, so on a device that never
// opted into cloud sync every pending mutation belongs to a non-enrolled
// project: that is the normal steady state, not a fault.
func TestSyncMutationRequiredFieldsIgnoresBacklogWithoutCloudEnrollment(t *testing.T) {
	s := newDiagnosticTestStore(t)
	if err := s.CreateSession("manual-save-engram", "engram", "/work/engram"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	pending, err := s.CountPendingNonEnrolledSyncMutations(store.DefaultSyncTargetKey)
	if err != nil {
		t.Fatalf("CountPendingNonEnrolledSyncMutations: %v", err)
	}
	if len(pending) == 0 {
		t.Fatal("fixture must journal a non-enrolled pending mutation")
	}

	report, err := NewRunner().RunOne(context.Background(), Scope{Store: s, Project: "engram"}, CheckSyncMutationRequiredFields)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if report.Status != StatusOK || len(report.Checks[0].Findings) != 0 {
		t.Fatalf("local-only install must not be blocked, got %+v", report)
	}
}

// TestSyncMutationRequiredFieldsBlocksNonEnrolledBacklogWhenCloudSyncInUse
// proves the issue #688 signal survives: once the device uses cloud sync, a
// project whose pending mutations cannot be delivered is reported as blocked
// with the enrollment guidance, while the enrolled project stays silent.
func TestSyncMutationRequiredFieldsBlocksNonEnrolledBacklogWhenCloudSyncInUse(t *testing.T) {
	s := newDiagnosticTestStore(t)
	if err := s.CreateSession("manual-save-enrolled", "enrolled", "/work/enrolled"); err != nil {
		t.Fatalf("CreateSession enrolled: %v", err)
	}
	if err := s.EnrollProject("enrolled"); err != nil {
		t.Fatalf("EnrollProject: %v", err)
	}
	if err := s.CreateSession("manual-save-local", "local", "/work/local"); err != nil {
		t.Fatalf("CreateSession local: %v", err)
	}

	report, err := NewRunner().RunOne(context.Background(), Scope{Store: s}, CheckSyncMutationRequiredFields)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if report.Status != StatusBlocked || len(report.Checks[0].Findings) != 1 {
		t.Fatalf("expected one blocking finding, got %+v", report)
	}
	finding := report.Checks[0].Findings[0]
	if finding.Severity != SeverityBlocking || finding.ReasonCode != constants.ReasonNonEnrolledPendingMutations {
		t.Fatalf("unexpected finding: %+v", finding)
	}
	if !strings.Contains(string(finding.Evidence), `"project":"local"`) {
		t.Fatalf("expected the non-enrolled project in evidence, got %s", finding.Evidence)
	}
	if !strings.Contains(finding.SafeNextStep, "engram cloud enroll <project>") {
		t.Fatalf("expected enrollment guidance, got %q", finding.SafeNextStep)
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
