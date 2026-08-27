package main

import (
	"strings"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/store"
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
