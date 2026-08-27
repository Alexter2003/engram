package mcp

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	mcppkg "github.com/mark3labs/mcp-go/mcp"
)

func TestMCPConfigCandidateRankingValidation(t *testing.T) {
	zero := 0.0
	floor := -2.0
	for _, tt := range []struct {
		name  string
		cfg   MCPConfig
		valid bool
	}{
		{name: "default max rank", cfg: MCPConfig{}, valid: true},
		{name: "explicit zero max rank", cfg: MCPConfig{BM25MaxRank: &zero}, valid: true},
		{name: "deprecated floor", cfg: MCPConfig{BM25Floor: &floor}, valid: true},
		{name: "both options", cfg: MCPConfig{BM25MaxRank: &zero, BM25Floor: &floor}},
		{name: "non-finite max rank", cfg: MCPConfig{BM25MaxRank: ptrBM25(math.Inf(1))}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validateCandidateRanking()
			if tt.valid && err != nil {
				t.Fatalf("validateCandidateRanking: %v", err)
			}
			if !tt.valid && err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func ptrBM25(value float64) *float64 { return &value }

func TestHandleSaveForwardsBM25MaxRank(t *testing.T) {
	s := newMCPTestStore(t)
	maxRank := -100.0
	h := handleSave(s, MCPConfig{BM25MaxRank: &maxRank}, NewSessionActivity(10*time.Minute))

	for _, title := range []string{
		"JWT authentication token session architecture",
		"JWT token session authentication design",
	} {
		res, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
			"title": title, "content": title, "type": "decision",
		}}})
		if err != nil || res.IsError {
			t.Fatalf("save %q: err=%v result=%q", title, err, callResultText(t, res))
		}
		if title != "JWT token session authentication design" {
			continue
		}
		var envelope map[string]any
		if err := json.Unmarshal([]byte(callResultText(t, res)), &envelope); err != nil {
			t.Fatalf("decode save response: %v", err)
		}
		if judgmentRequired, _ := envelope["judgment_required"].(bool); judgmentRequired {
			t.Fatalf("BM25MaxRank=%v must be forwarded and exclude normal ranks: %s", maxRank, callResultText(t, res))
		}
	}
}

func TestHandleSaveRejectsInvalidCandidateRankingBeforeCreatingObservation(t *testing.T) {
	s := newMCPTestStore(t)
	h := handleSave(s, MCPConfig{BM25MaxRank: ptrBM25(math.Inf(1)), DefaultProject: "test"}, NewSessionActivity(10*time.Minute))

	res, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"title": "must not save", "content": "invalid ranking configuration",
	}}})
	if err != nil {
		t.Fatalf("handleSave: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected tool error, got %q", callResultText(t, res))
	}
	count, err := s.CountObservationsForProject("test")
	if err != nil {
		t.Fatalf("count observations: %v", err)
	}
	if count != 0 {
		t.Fatalf("invalid ranking created %d observations, want 0", count)
	}
}
