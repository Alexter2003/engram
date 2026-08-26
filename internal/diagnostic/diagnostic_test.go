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
	return s
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
	want := []string{CheckInvalidSessionIdentity, CheckManualSessionNameProjectMismatch, CheckSessionProjectDirectoryMismatch, CheckSQLiteLockContention, CheckSyncMutationRequiredFields}
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
	if report.Status != StatusOK || report.Summary.OK != 5 || len(report.Checks) != 5 {
		t.Fatalf("report=%+v", report)
	}
	for _, check := range report.Checks {
		if check.Result != StatusOK || len(check.Evidence) == 0 {
			t.Fatalf("expected ok check with evidence, got %+v", check)
		}
	}
}

func TestInvalidSessionIdentityCheckReportsSourceReferencesAndJournal(t *testing.T) {
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

	db, err := sql.Open("sqlite", filepath.Join(cfg.DataDir, "engram.db"))
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`INSERT INTO sessions (id, project, directory) VALUES ('', 'engram', '/tmp/engram');
		INSERT INTO observations (sync_id, session_id, type, title, content, project, scope, normalized_hash, revision_count, duplicate_count, created_at, updated_at)
		VALUES ('obs-empty-session', '', 'bugfix', 'title', 'content', 'engram', 'project', 'hash', 1, 1, datetime('now'), datetime('now'));
		INSERT INTO user_prompts (sync_id, session_id, content, project, created_at) VALUES ('prompt-empty-session', '', 'prompt', 'engram', datetime('now'));
		INSERT INTO sync_mutations (target_key, entity, entity_key, op, payload, source, project) VALUES ('cloud', 'session', '', 'upsert', '{"id":"","project":"engram","directory":"/tmp/engram"}', 'local', 'engram');`); err != nil {
		t.Fatalf("seed corrupt identity: %v", err)
	}

	report, err := NewRunner().RunOne(context.Background(), Scope{Store: s, Project: "engram"}, CheckInvalidSessionIdentity)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if report.Status != StatusBlocked || len(report.Checks[0].Findings) != 1 {
		t.Fatalf("report=%+v", report)
	}
	var evidence store.InvalidSessionIdentityEvidence
	if err := json.Unmarshal(report.Checks[0].Findings[0].Evidence, &evidence); err != nil {
		t.Fatalf("decode evidence: %v", err)
	}
	if evidence.ObservationCount != 1 || evidence.PromptCount != 1 || evidence.InvalidJournalCount != 1 {
		t.Fatalf("evidence=%+v", evidence)
	}

	plan, err := BuildRepairPlan(context.Background(), Scope{Store: s, Project: "engram"}, report, CheckInvalidSessionIdentity, RepairModeApply)
	if err != nil {
		t.Fatalf("BuildRepairPlan: %v", err)
	}
	if plan.Status != "noop" || len(plan.Actions) != 0 || len(plan.Skipped) != 1 || plan.Skipped[0].ReasonCode != "cannot_repair_without_explicit_canonical_session_id" {
		t.Fatalf("repair plan=%+v", plan)
	}
}

func TestInvalidSessionIdentityEvidenceAttributesOnlyMatchingJournalMutations(t *testing.T) {
	s := newDiagnosticTestStore(t)
	if _, err := s.DB().Exec(`
		INSERT INTO sessions (id, project, directory) VALUES ('', 'engram', '/tmp/empty');
		INSERT INTO sessions (id, project, directory) VALUES (' ', 'engram', '/tmp/space');
		INSERT INTO sync_mutations (target_key, entity, entity_key, op, payload, source, project) VALUES
			('cloud', 'session', '', 'upsert', 'not json', 'local', 'engram'),
			('cloud', 'session', 'valid-key', 'upsert', '{"id":"","directory":"/tmp"}', 'local', 'engram'),
			('cloud', 'session', ' ', 'upsert', '{"id":"other","directory":"/tmp"}', 'local', 'engram'),
			('cloud', 'session', 'other', 'upsert', '{"id":" ","directory":"/tmp"}', 'local', 'engram'),
			('cloud', 'session', 'unrelated', 'upsert', '{"id":"different","directory":"/tmp"}', 'local', 'engram');
	`); err != nil {
		t.Fatalf("seed invalid session journal: %v", err)
	}

	evidence, err := s.ListInvalidSessionIdentityEvidence("engram")
	if err != nil {
		t.Fatalf("ListInvalidSessionIdentityEvidence: %v", err)
	}
	counts := make(map[string]int64, len(evidence))
	for _, item := range evidence {
		counts[item.SessionID] = item.InvalidJournalCount
	}
	if counts[""] != 2 || counts[" "] != 2 {
		t.Fatalf("invalid journal counts=%v, want empty=2 whitespace=2", counts)
	}
}

func TestInvalidSessionIdentityEvidenceDoesNotAttributeUnmatchedWhitespaceIdentities(t *testing.T) {
	s := newDiagnosticTestStore(t)
	if _, err := s.DB().Exec(`
		INSERT INTO sessions (id, project, directory) VALUES ('', 'engram', '/tmp/empty');
		INSERT INTO sync_mutations (target_key, entity, entity_key, op, payload, source, project)
		VALUES ('cloud', 'session', char(9), 'upsert', '{"id":"\n","directory":"/tmp"}', 'local', 'engram');
	`); err != nil {
		t.Fatalf("seed unmatched whitespace journal: %v", err)
	}

	evidence, err := s.ListInvalidSessionIdentityEvidence("engram")
	if err != nil {
		t.Fatalf("ListInvalidSessionIdentityEvidence: %v", err)
	}
	if len(evidence) != 1 || evidence[0].InvalidJournalCount != 0 {
		t.Fatalf("evidence=%+v, want one unassigned empty-session mutation", evidence)
	}
}

func TestInvalidSessionIdentityEvidenceDoesNotAttributeMalformedPayloadWithoutExactKey(t *testing.T) {
	s := newDiagnosticTestStore(t)
	if _, err := s.DB().Exec(`
		INSERT INTO sessions (id, project, directory) VALUES ('', 'engram', '/tmp/empty');
		INSERT INTO sync_mutations (target_key, entity, entity_key, op, payload, source, project)
		VALUES ('cloud', 'session', 'nonmatching-key', 'upsert', 'not json', 'local', 'engram');
	`); err != nil {
		t.Fatalf("seed malformed session journal: %v", err)
	}

	evidence, err := s.ListInvalidSessionIdentityEvidence("engram")
	if err != nil {
		t.Fatalf("ListInvalidSessionIdentityEvidence: %v", err)
	}
	if len(evidence) != 1 || evidence[0].InvalidJournalCount != 0 {
		t.Fatalf("evidence=%+v, want one unassigned malformed mutation", evidence)
	}
}
