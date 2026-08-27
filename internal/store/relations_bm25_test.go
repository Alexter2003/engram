package store

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

type rankedCandidate struct {
	id    int64
	score float64
}

func TestFindCandidates_DefaultMaxRankRetainsStrongMatchesBeforeLimit(t *testing.T) {
	s := setupRelationsStore(t)
	const title = "anchor beacon canyon delta ember forest galaxy harbor"
	for i := 0; i < 8; i++ {
		addTestObs(t, s, fmt.Sprintf("%s strong %d", title, i), "decision", "testproject", "project")
	}
	for i := 0; i < 8; i++ {
		addTestObs(t, s, fmt.Sprintf("anchor unrelated candidate %d", i), "decision", "testproject", "project")
	}
	savedID, _ := addTestObs(t, s, title, "decision", "testproject", "project")

	ranks := allCandidateRanks(t, s, savedID, "testproject", "project")
	eligible := ranksAtMost(ranks, defaultCandidateBM25MaxRank)
	if len(eligible) <= 6 { // limit * 3 from the broken implementation
		t.Fatalf("fixture must produce more than limit*3 default-eligible matches, got %d scores=%+v", len(eligible), ranks)
	}
	if eligible[0].score >= eligible[len(eligible)-1].score {
		t.Fatalf("expected FTS5's strongest match to have a smaller raw rank: first=%f last=%f", eligible[0].score, eligible[len(eligible)-1].score)
	}

	candidates, err := s.FindCandidates(savedID, CandidateOptions{Project: "testproject", Scope: "project", Limit: 2, SkipInsert: true})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidate count: got %d, want 2", len(candidates))
	}
	for i, candidate := range candidates {
		if candidate.ID != eligible[i].id || candidate.Score != eligible[i].score {
			t.Fatalf("candidate %d: got (%d, %f), want best eligible (%d, %f)", i, candidate.ID, candidate.Score, eligible[i].id, eligible[i].score)
		}
		if i > 0 && candidates[i-1].Score > candidate.Score {
			t.Fatalf("candidates not best-first: %f then %f", candidates[i-1].Score, candidate.Score)
		}
	}
}

func TestFindCandidates_BM25MaxRankAndDeprecatedFloor(t *testing.T) {
	s := setupRelationsStore(t)
	const title = "anchor beacon canyon delta ember forest galaxy harbor"
	for i := 0; i < 4; i++ {
		addTestObs(t, s, fmt.Sprintf("%s strong %d", title, i), "decision", "testproject", "project")
	}
	addTestObs(t, s, "anchor unrelated candidate", "decision", "testproject", "project")
	savedID, _ := addTestObs(t, s, title, "decision", "testproject", "project")
	ranks := allCandidateRanks(t, s, savedID, "testproject", "project")
	if len(ranks) < 2 || ranks[0].score >= ranks[len(ranks)-1].score {
		t.Fatalf("fixture must provide ordered strong and weak FTS5 ranks: %+v", ranks)
	}

	zero := 0.0
	all, err := s.FindCandidates(savedID, CandidateOptions{Project: "testproject", Scope: "project", Limit: 10, BM25MaxRank: &zero, SkipInsert: true})
	if err != nil {
		t.Fatalf("FindCandidates with explicit zero max rank: %v", err)
	}
	if len(all) != len(ranks) {
		t.Fatalf("explicit zero max rank should retain all negative FTS5 ranks: got %d, want %d", len(all), len(ranks))
	}

	ceiling := (ranks[0].score + ranks[len(ranks)-1].score) / 2
	strict, err := s.FindCandidates(savedID, CandidateOptions{Project: "testproject", Scope: "project", Limit: 10, BM25MaxRank: &ceiling, SkipInsert: true})
	if err != nil {
		t.Fatalf("FindCandidates with custom max rank: %v", err)
	}
	for _, candidate := range strict {
		if candidate.Score > ceiling {
			t.Fatalf("rank %f exceeds configured max rank %f", candidate.Score, ceiling)
		}
	}

	legacy, err := s.FindCandidates(savedID, CandidateOptions{Project: "testproject", Scope: "project", Limit: 10, BM25Floor: &zero, SkipInsert: true})
	if err != nil {
		t.Fatalf("FindCandidates with deprecated floor: %v", err)
	}
	if len(legacy) != 0 {
		t.Fatalf("deprecated zero floor must retain its legacy >= 0 behavior, got %d candidates", len(legacy))
	}
	if _, err := s.FindCandidates(savedID, CandidateOptions{BM25MaxRank: &zero, BM25Floor: &zero}); err == nil {
		t.Fatal("expected conflicting max rank and deprecated floor to be rejected")
	}
}

func TestCandidateRankQueryUsesInclusiveMaxRank(t *testing.T) {
	maxRank := -1.25
	query, threshold, err := candidateRankQuery(CandidateOptions{BM25MaxRank: &maxRank})
	if err != nil {
		t.Fatalf("candidateRankQuery: %v", err)
	}
	if threshold != maxRank || !strings.Contains(query, "fts.rank <= ?") {
		t.Fatalf("max rank query must retain ranks equal to %v: threshold=%v query=%q", maxRank, threshold, query)
	}
}

func TestCandidateRankQueryRejectsNonFiniteRanking(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts CandidateOptions
	}{
		{name: "max rank NaN", opts: CandidateOptions{BM25MaxRank: ptrRank(math.NaN())}},
		{name: "max rank positive infinity", opts: CandidateOptions{BM25MaxRank: ptrRank(math.Inf(1))}},
		{name: "max rank negative infinity", opts: CandidateOptions{BM25MaxRank: ptrRank(math.Inf(-1))}},
		{name: "floor NaN", opts: CandidateOptions{BM25Floor: ptrRank(math.NaN())}},
		{name: "floor positive infinity", opts: CandidateOptions{BM25Floor: ptrRank(math.Inf(1))}},
		{name: "floor negative infinity", opts: CandidateOptions{BM25Floor: ptrRank(math.Inf(-1))}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := candidateRankQuery(tt.opts); err == nil {
				t.Fatal("expected non-finite ranking configuration to be rejected")
			}
		})
	}
}

func ptrRank(value float64) *float64 { return &value }

func TestFindCandidates_FiltersAndSkipInsertWithMaxRank(t *testing.T) {
	s := setupRelationsStore(t)
	const title = "filter anchor beacon source"
	eligibleID, _ := addTestObs(t, s, "filter anchor beacon eligible", "decision", "testproject", "project")
	otherScopeID, _ := addTestObs(t, s, "filter anchor beacon personal", "decision", "testproject", "personal")
	otherProjectID, _ := addTestObs(t, s, "filter anchor beacon foreign", "decision", "other", "project")
	deletedID, _ := addTestObs(t, s, "filter anchor beacon deleted", "decision", "testproject", "project")
	if _, err := s.db.Exec("UPDATE observations SET deleted_at=datetime('now') WHERE id=?", deletedID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	savedID, _ := addTestObs(t, s, title, "decision", "testproject", "project")
	before := tableRowCount(t, s, "memory_relations")
	zero := 0.0
	candidates, err := s.FindCandidates(savedID, CandidateOptions{Project: "testproject", Scope: "project", Limit: 10, BM25MaxRank: &zero, SkipInsert: true})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if tableRowCount(t, s, "memory_relations") != before {
		t.Fatal("SkipInsert wrote pending relations")
	}
	if len(candidates) != 1 || candidates[0].ID != eligibleID {
		t.Fatalf("expected only same-project, same-scope, non-deleted non-self candidate %d; got %+v (excluded: scope=%d project=%d deleted=%d self=%d)", eligibleID, candidates, otherScopeID, otherProjectID, deletedID, savedID)
	}
}

func TestScanProject_DryRunRepeatedVocabularyScalesWithSQLRankFilter(t *testing.T) {
	const project = "scan-rank-scaling"
	const candidateLimit = 10
	const broadTimeout = 15 * time.Second

	var elapsed [3]time.Duration
	for index, observationCount := range []int{20, 50, 100} {
		t.Run(fmt.Sprintf("%d observations", observationCount), func(t *testing.T) {
			s := setupRelationsStore(t)
			for i := 0; i < observationCount; i++ {
				addTestObs(t, s, fmt.Sprintf("shared vocabulary architecture decision pattern %03d", i), "decision", project, "project")
			}

			beforeRelations := tableRowCount(t, s, "memory_relations")
			beforeMutations := tableRowCount(t, s, "sync_mutations")
			started := time.Now()
			first, err := s.ScanProject(ScanOptions{Project: project})
			elapsed[index] = time.Since(started)
			if err != nil {
				t.Fatalf("ScanProject dry-run: %v", err)
			}
			if elapsed[index] > broadTimeout {
				t.Fatalf("ScanProject dry-run took %s; expected under broad %s regression guard", elapsed[index], broadTimeout)
			}

			wantCandidates := observationCount * candidateLimit
			if first.Inspected != observationCount || first.CandidatesFound != wantCandidates {
				t.Fatalf("first dry-run result = %+v; want inspected=%d candidates=%d", first, observationCount, wantCandidates)
			}
			if !first.DryRun || first.RelationsInserted != 0 {
				t.Fatalf("dry-run must not insert relations: %+v", first)
			}
			if got := tableRowCount(t, s, "memory_relations"); got != beforeRelations {
				t.Fatalf("dry-run relation rows = %d, want unchanged %d", got, beforeRelations)
			}
			if got := tableRowCount(t, s, "sync_mutations"); got != beforeMutations {
				t.Fatalf("dry-run sync mutations = %d, want unchanged %d", got, beforeMutations)
			}

			second, err := s.ScanProject(ScanOptions{Project: project})
			if err != nil {
				t.Fatalf("second ScanProject dry-run: %v", err)
			}
			if second.Inspected != first.Inspected || second.CandidatesFound != first.CandidatesFound || second.RelationsInserted != 0 {
				t.Fatalf("dry-run results are not deterministic: first=%+v second=%+v", first, second)
			}
			if got := tableRowCount(t, s, "memory_relations"); got != beforeRelations {
				t.Fatalf("second dry-run relation rows = %d, want unchanged %d", got, beforeRelations)
			}
			if got := tableRowCount(t, s, "sync_mutations"); got != beforeMutations {
				t.Fatalf("second dry-run sync mutations = %d, want unchanged %d", got, beforeMutations)
			}
		})
	}

	// Keep this deliberately broad for shared Windows runners while rejecting the
	// historical curve (20≈3s, 50≈21s, 100>60s), rather than a fixed machine speed.
	if elapsed[1] > elapsed[0]*4+2*time.Second {
		t.Fatalf("20→50 observation growth regressed: 20=%s 50=%s", elapsed[0], elapsed[1])
	}
	if elapsed[2] > elapsed[1]*5/2+2*time.Second {
		t.Fatalf("50→100 observation growth regressed: 50=%s 100=%s", elapsed[1], elapsed[2])
	}
}

func tableRowCount(t *testing.T, s *Store, table string) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func allCandidateRanks(t *testing.T, s *Store, savedID int64, project, scope string) []rankedCandidate {
	t.Helper()
	var title string
	if err := s.db.QueryRow("SELECT title FROM observations WHERE id=?", savedID).Scan(&title); err != nil {
		t.Fatalf("source title: %v", err)
	}
	rows, err := s.db.Query(`
		SELECT o.id, fts.rank
		FROM observations_fts fts
		CROSS JOIN observations o ON o.id = fts.rowid
		WHERE observations_fts MATCH ? AND o.id != ? AND o.deleted_at IS NULL
		  AND ifnull(o.project,'') = ifnull(?,'') AND o.scope = ?
		ORDER BY fts.rank`, sanitizeFTSCandidates(title), savedID, project, scope)
	if err != nil {
		t.Fatalf("raw FTS5 rank query: %v", err)
	}
	defer rows.Close()
	var ranks []rankedCandidate
	for rows.Next() {
		var rank rankedCandidate
		if err := rows.Scan(&rank.id, &rank.score); err != nil {
			t.Fatalf("scan raw FTS5 rank: %v", err)
		}
		ranks = append(ranks, rank)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("raw FTS5 rank rows: %v", err)
	}
	return ranks
}

func ranksAtMost(ranks []rankedCandidate, ceiling float64) []rankedCandidate {
	eligible := make([]rankedCandidate, 0, len(ranks))
	for _, rank := range ranks {
		if rank.score <= ceiling {
			eligible = append(eligible, rank)
		}
	}
	return eligible
}
