package main

import (
	"strings"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/mcp"
	"github.com/Gentleman-Programming/engram/internal/store"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func TestParseBM25RankOption(t *testing.T) {
	for _, tt := range []struct {
		name    string
		raw     string
		want    float64
		wantErr bool
	}{
		{name: "zero is explicit", raw: "0", want: 0},
		{name: "negative max rank", raw: "-2.5", want: -2.5},
		{name: "not a number", raw: "NaN", wantErr: true},
		{name: "infinite", raw: "Inf", wantErr: true},
		{name: "invalid", raw: "rank", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBM25RankOption("--bm25-max-rank", tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected parse error")
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("parseBM25RankOption() = %v, %v; want %v, nil", got, err, tt.want)
			}
		})
	}
}

func TestCmdMCPRejectsBothCandidateRankingOptions(t *testing.T) {
	stubExitWithPanic(t)
	withArgs(t, "engram", "mcp", "--bm25-max-rank", "0", "--bm25-floor", "-2")

	_, stderr, recovered := captureOutputAndRecover(t, func() { cmdMCP(store.Config{}) })
	if recovered == nil {
		t.Fatal("expected cmdMCP to exit for conflicting candidate ranking options")
	}
	if !strings.Contains(stderr, "--bm25-max-rank and deprecated --bm25-floor cannot both be set") {
		t.Fatalf("unexpected conflicting-option error: %q", stderr)
	}
}

func TestCmdMCPForwardsCandidateRankingOptions(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		maxRank   bool
		wantValue float64
	}{
		{name: "max rank equals form", args: []string{"--bm25-max-rank=-1.25"}, maxRank: true, wantValue: -1.25},
		{name: "max rank separate value form", args: []string{"--bm25-max-rank", "-2.5"}, maxRank: true, wantValue: -2.5},
		{name: "deprecated floor equals form", args: []string{"--bm25-floor=-1.25"}, wantValue: -1.25},
		{name: "deprecated floor separate value form", args: []string{"--bm25-floor", "-2.5"}, wantValue: -2.5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, err := store.New(testConfig(t))
			if err != nil {
				t.Fatalf("new store: %v", err)
			}
			oldStoreNew, oldNewServer, oldServe := storeNew, newMCPServerWithConfig, serveMCP
			storeNew = func(store.Config) (*store.Store, error) { return s, nil }
			var got *float64
			newMCPServerWithConfig = func(s *store.Store, cfg mcp.MCPConfig, allowlist map[string]bool) *mcpserver.MCPServer {
				if tt.maxRank {
					got = cfg.BM25MaxRank
				} else {
					got = cfg.BM25Floor
				}
				return mcp.NewServer(s)
			}
			serveMCP = func(*mcpserver.MCPServer, ...mcpserver.StdioOption) error { return nil }
			t.Cleanup(func() {
				storeNew, newMCPServerWithConfig, serveMCP = oldStoreNew, oldNewServer, oldServe
			})

			withArgs(t, append([]string{"engram", "mcp"}, tt.args...)...)
			cmdMCP(testConfig(t))

			if got == nil || *got != tt.wantValue {
				t.Fatalf("candidate ranking option = %v, want %v", got, tt.wantValue)
			}
		})
	}
}
