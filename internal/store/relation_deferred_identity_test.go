package store

import (
	"encoding/json"
	"sort"
	"testing"
)

// ─── Helpers ──────────────────────────────────────────────────────────────────

// relationDeferredPayloadIDs returns the payload sync_id of every relation row
// in sync_apply_deferred. The payload identity is used instead of the row key
// so the assertions describe which mutations were preserved rather than how
// they happen to be keyed.
func relationDeferredPayloadIDs(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`
		SELECT ifnull(json_extract(payload, '$.sync_id'), '')
		FROM sync_apply_deferred
		WHERE entity = 'relation'
	`)
	if err != nil {
		t.Fatalf("relationDeferredPayloadIDs: query: %v", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("relationDeferredPayloadIDs: scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("relationDeferredPayloadIDs: rows: %v", err)
	}
	sort.Strings(ids)
	return ids
}

// countRelationDeferredRows returns how many relation rows exist in
// sync_apply_deferred regardless of key.
func countRelationDeferredRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM sync_apply_deferred WHERE entity = 'relation'`,
	).Scan(&n); err != nil {
		t.Fatalf("countRelationDeferredRows: %v", err)
	}
	return n
}

// countRelationDeferredRowsForPayload returns how many relation rows carry the
// given payload sync_id.
func countRelationDeferredRowsForPayload(t *testing.T, s *Store, relSyncID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`
		SELECT count(*)
		FROM sync_apply_deferred
		WHERE entity = 'relation'
		  AND ifnull(json_extract(payload, '$.sync_id'), '') = ?
	`, relSyncID).Scan(&n); err != nil {
		t.Fatalf("countRelationDeferredRowsForPayload: %v", err)
	}
	return n
}

// relationPayloadJSON builds a well-formed relation payload for the given
// endpoints.
func relationPayloadJSON(t *testing.T, relSyncID, sourceID, targetID string) string {
	t.Helper()
	actor := "test-actor"
	kind := "test"
	raw, err := json.Marshal(syncRelationPayload{
		SyncID:         relSyncID,
		SourceID:       sourceID,
		TargetID:       targetID,
		Relation:       RelationRelated,
		JudgmentStatus: JudgmentStatusJudged,
		MarkedByActor:  &actor,
		MarkedByKind:   &kind,
		Project:        "proj-apply",
		CreatedAt:      "2026-04-26T10:00:00Z",
		UpdatedAt:      "2026-04-26T10:00:00Z",
	})
	if err != nil {
		t.Fatalf("relationPayloadJSON: marshal: %v", err)
	}
	return string(raw)
}

// blankEntityKeyRelationMutation builds a relation mutation whose payload is
// well-formed but whose entity_key is blank. applyRelationUpsertTx rejects it as
// terminal evidence, so it is the shape that used to collapse rows together.
func blankEntityKeyRelationMutation(t *testing.T, relSyncID, sourceID, targetID string) SyncMutation {
	t.Helper()
	return SyncMutation{
		Entity:    SyncEntityRelation,
		EntityKey: "",
		Op:        SyncOpUpsert,
		Payload:   relationPayloadJSON(t, relSyncID, sourceID, targetID),
		Source:    SyncSourceRemote,
		Project:   "proj-apply",
	}
}

// ─── Issue #838 — relation dead-letter identity ───────────────────────────────

// Two distinct failed relation mutations that both carry a blank entity_key must
// each keep their own evidence row. Keying the row on entity_key directly made
// the second mutation overwrite the first.
func TestRelationDeadLetter_DistinctBlankEntityKeysKeepSeparateRows(t *testing.T) {
	s, syncA, syncB := setupSyncApplyStore(t)

	mutations := []SyncMutation{
		blankEntityKeyRelationMutation(t, "rel-a", syncA, syncB),
		blankEntityKeyRelationMutation(t, "rel-b", syncA, syncB),
	}
	if err := s.ApplyPulledChunk(DefaultSyncTargetKey, "chunk-blank-entity-keys", mutations); err != nil {
		t.Fatalf("ApplyPulledChunk: %v", err)
	}

	got := relationDeferredPayloadIDs(t, s)
	want := []string{"rel-a", "rel-b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("deferred payload identities: want %v, got %v", want, got)
	}
}

// Two distinct failed relation mutations that share the same non-blank entity_key
// but describe different relations must also keep their own rows.
func TestRelationDeadLetter_DistinctPayloadsUnderSharedEntityKeyKeepSeparateRows(t *testing.T) {
	s, syncA, syncB := setupSyncApplyStore(t)

	mutations := []SyncMutation{
		{
			Entity:    SyncEntityRelation,
			EntityKey: "rel-shared",
			Op:        SyncOpUpsert,
			Payload:   relationPayloadJSON(t, "rel-c", syncA, syncB),
			Source:    SyncSourceRemote,
			Project:   "proj-apply",
		},
		{
			Entity:    SyncEntityRelation,
			EntityKey: "rel-shared",
			Op:        SyncOpUpsert,
			Payload:   relationPayloadJSON(t, "rel-d", syncA, syncB),
			Source:    SyncSourceRemote,
			Project:   "proj-apply",
		},
	}
	if err := s.ApplyPulledChunk(DefaultSyncTargetKey, "chunk-shared-entity-key", mutations); err != nil {
		t.Fatalf("ApplyPulledChunk: %v", err)
	}

	got := relationDeferredPayloadIDs(t, s)
	want := []string{"rel-c", "rel-d"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("deferred payload identities: want %v, got %v", want, got)
	}
}

// A genuine redelivery of the same discarded mutation must collapse onto one
// row, so evidence does not accumulate one row per delivery.
func TestRelationDeadLetter_RedeliveryOfSameMutationStaysOneRow(t *testing.T) {
	s, syncA, syncB := setupSyncApplyStore(t)

	mutation := blankEntityKeyRelationMutation(t, "rel-redelivered", syncA, syncB)
	for _, chunkID := range []string{"chunk-first-delivery", "chunk-second-delivery"} {
		if err := s.ApplyPulledChunk(DefaultSyncTargetKey, chunkID, []SyncMutation{mutation}); err != nil {
			t.Fatalf("ApplyPulledChunk %s: %v", chunkID, err)
		}
	}

	if got := countRelationDeferredRows(t, s); got != 1 {
		t.Fatalf("rows after redelivery: want 1, got %d", got)
	}
	if got := countRelationDeferredRowsForPayload(t, s, "rel-redelivered"); got != 1 {
		t.Fatalf("rows for rel-redelivered: want 1, got %d", got)
	}
}

// A relation that first fails and later applies successfully must have its
// evidence row cleaned up. The row was written under the blank entity_key, so a
// cleanup keyed on the payload sync_id alone could never reach it.
func TestRelationDeadLetter_SuccessfulApplyCleansUpBlankKeyedRow(t *testing.T) {
	s, syncA, syncB := setupSyncApplyStore(t)

	failed := blankEntityKeyRelationMutation(t, "rel-orphan", syncA, syncB)
	if err := s.ApplyPulledChunk(DefaultSyncTargetKey, "chunk-orphan-failure", []SyncMutation{failed}); err != nil {
		t.Fatalf("ApplyPulledChunk (failure): %v", err)
	}
	if got := countRelationDeferredRowsForPayload(t, s, "rel-orphan"); got != 1 {
		t.Fatalf("evidence row for rel-orphan: want 1, got %d", got)
	}

	applied := failed
	applied.EntityKey = "rel-orphan"
	if err := s.ApplyPulledChunk(DefaultSyncTargetKey, "chunk-orphan-success", []SyncMutation{applied}); err != nil {
		t.Fatalf("ApplyPulledChunk (success): %v", err)
	}

	if got := countRelationRows(t, s, "rel-orphan"); got != 1 {
		t.Fatalf("memory_relations rows for rel-orphan: want 1, got %d", got)
	}
	if got := countRelationDeferredRows(t, s); got != 0 {
		t.Fatalf("orphaned deferred rows after successful apply: want 0, got %d", got)
	}
}

// The retry contract is unchanged for retryable failures: the row is keyed on the
// relation's own sync_id, replay reaches it, and the successful apply removes it.
func TestRelationDeferred_FKMissThenReplaySucceedsWithoutOrphan(t *testing.T) {
	s, syncA, _ := setupSyncApplyStore(t)

	missingTarget := "obs-missing-" + newSyncID("x")
	relSyncID := newSyncID("rel")
	mutation := SyncMutation{
		Entity:    SyncEntityRelation,
		EntityKey: relSyncID,
		Op:        SyncOpUpsert,
		Payload:   relationPayloadJSON(t, relSyncID, syncA, missingTarget),
		Source:    SyncSourceRemote,
		Project:   "proj-apply",
	}
	if err := s.ApplyPulledChunk(DefaultSyncTargetKey, "chunk-fk-miss", []SyncMutation{mutation}); err != nil {
		t.Fatalf("ApplyPulledChunk: %v", err)
	}

	status, _ := getDeferredRow(t, s, relSyncID)
	if status != "deferred" {
		t.Fatalf("apply_status: want deferred, got %q", status)
	}

	// The missing endpoint arrives; rewrite the payload the way a later delivery
	// would and let replay drive the retry.
	if err := s.CreateSession("ses-late", "proj-apply", "/tmp/late"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, arrived := addTestObsSession(t, s, "ses-late", "Late Obs", "decision", "proj-apply", "project")
	if _, err := s.db.Exec(
		`UPDATE sync_apply_deferred SET payload = ? WHERE sync_id = ?`,
		relationPayloadJSON(t, relSyncID, syncA, arrived), relSyncID,
	); err != nil {
		t.Fatalf("update deferred payload: %v", err)
	}

	res, err := s.ReplayDeferredForScope(DefaultSyncTargetKey, "proj-apply")
	if err != nil {
		t.Fatalf("ReplayDeferredForScope: %v", err)
	}
	if res.Succeeded != 1 {
		t.Fatalf("succeeded: want 1, got %d", res.Succeeded)
	}
	if got := countRelationDeferredRows(t, s); got != 0 {
		t.Fatalf("orphaned deferred rows after replay: want 0, got %d", got)
	}
	if got := countRelationRows(t, s, relSyncID); got != 1 {
		t.Fatalf("memory_relations rows: want 1, got %d", got)
	}
}

// ─── Rows written by the previous identity scheme ─────────────────────────────

// Legacy rows carry no entity_key or op, because the old writer only stored the
// key in sync_id. Replay must still reconstruct a valid mutation from them.
func TestReplayDeferred_LegacyRowWithoutEntityKeyStillApplies(t *testing.T) {
	s, syncA, syncB := setupSyncApplyStore(t)

	relSyncID := newSyncID("rel-legacy")
	if _, err := s.db.Exec(`
		INSERT INTO sync_apply_deferred
			(sync_id, entity, payload, target_key, project, scope_class, entity_key, op, apply_status, retry_count, first_seen_at)
		VALUES (?, 'relation', ?, ?, 'proj-apply', 'scoped', '', '', 'deferred', 0, datetime('now'))
	`, relSyncID, relationPayloadJSON(t, relSyncID, syncA, syncB), DefaultSyncTargetKey); err != nil {
		t.Fatalf("insert legacy deferred row: %v", err)
	}

	res, err := s.ReplayDeferredForScope(DefaultSyncTargetKey, "proj-apply")
	if err != nil {
		t.Fatalf("ReplayDeferredForScope: %v", err)
	}
	if res.Succeeded != 1 {
		t.Fatalf("succeeded: want 1, got %d", res.Succeeded)
	}
	if got := countRelationDeferredRows(t, s); got != 0 {
		t.Fatalf("legacy row still present after successful replay: got %d", got)
	}
	if got := countRelationRows(t, s, relSyncID); got != 1 {
		t.Fatalf("memory_relations rows: want 1, got %d", got)
	}
}

// A legacy dead row is rekeyed rather than duplicated when the very same
// mutation is redelivered.
func TestRelationDeadLetter_RedeliveryRetiresLegacyRowForSameMutation(t *testing.T) {
	s, syncA, syncB := setupSyncApplyStore(t)

	mutation := blankEntityKeyRelationMutation(t, "rel-legacy-dead", syncA, syncB)
	// The old writer keyed the row on the blank entity_key and stored no entity_key.
	if _, err := s.db.Exec(`
		INSERT INTO sync_apply_deferred
			(sync_id, entity, payload, target_key, project, scope_class, entity_key, op, apply_status, retry_count, first_seen_at)
		VALUES ('', 'relation', ?, ?, 'proj-apply', 'scoped', '', '', 'dead', 0, datetime('now'))
	`, mutation.Payload, DefaultSyncTargetKey); err != nil {
		t.Fatalf("insert legacy dead row: %v", err)
	}

	if err := s.ApplyPulledChunk(DefaultSyncTargetKey, "chunk-legacy-redelivery", []SyncMutation{mutation}); err != nil {
		t.Fatalf("ApplyPulledChunk: %v", err)
	}

	if got := countRelationDeferredRows(t, s); got != 1 {
		t.Fatalf("rows after legacy redelivery: want 1, got %d", got)
	}
	var syncID string
	if err := s.db.QueryRow(`SELECT sync_id FROM sync_apply_deferred WHERE entity = 'relation'`).Scan(&syncID); err != nil {
		t.Fatalf("read surviving row: %v", err)
	}
	if syncID == "" {
		t.Fatal("surviving row is still keyed on the blank entity_key")
	}
}

// A legacy row that holds a different mutation's payload is never retired by an
// unrelated redelivery: it is the only remaining evidence of that mutation.
func TestRelationDeadLetter_LegacyRowForOtherMutationIsPreserved(t *testing.T) {
	s, syncA, syncB := setupSyncApplyStore(t)

	// The collapsed row left behind by the old scheme holds rel-b's payload.
	if _, err := s.db.Exec(`
		INSERT INTO sync_apply_deferred
			(sync_id, entity, payload, target_key, project, scope_class, entity_key, op, apply_status, retry_count, first_seen_at)
		VALUES ('', 'relation', ?, ?, 'proj-apply', 'scoped', '', '', 'dead', 0, datetime('now'))
	`, relationPayloadJSON(t, "rel-b", syncA, syncB), DefaultSyncTargetKey); err != nil {
		t.Fatalf("insert legacy collapsed row: %v", err)
	}

	mutation := blankEntityKeyRelationMutation(t, "rel-a", syncA, syncB)
	if err := s.ApplyPulledChunk(DefaultSyncTargetKey, "chunk-legacy-unrelated", []SyncMutation{mutation}); err != nil {
		t.Fatalf("ApplyPulledChunk: %v", err)
	}

	got := relationDeferredPayloadIDs(t, s)
	want := []string{"rel-a", "rel-b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("deferred payload identities: want %v, got %v", want, got)
	}
}

// Legacy rows stay reachable through the audit surface after the identity change.
func TestGetDeferred_LegacyRelationRowStaysReachable(t *testing.T) {
	s, syncA, syncB := setupSyncApplyStore(t)

	relSyncID := newSyncID("rel-audit")
	insertDeferredRow(t, s, relSyncID, SyncEntityRelation, relationPayloadJSON(t, relSyncID, syncA, syncB), 0, "dead")

	row, err := s.GetDeferred(relSyncID)
	if err != nil {
		t.Fatalf("GetDeferred: %v", err)
	}
	if row.SyncID != relSyncID || row.ApplyStatus != "dead" {
		t.Fatalf("GetDeferred: got sync_id=%q status=%q", row.SyncID, row.ApplyStatus)
	}
}
