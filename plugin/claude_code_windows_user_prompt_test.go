package plugin_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestClaudeCodeWindowsPromptResolverRejectsNonStringProject(t *testing.T) {
	powershellPath := claudeCodePowerShell(t)
	adapterPath := filepath.Join(repoRoot(t), "plugin", "claude-code", "scripts", "user-prompt-submit.ps1")

	var mu sync.Mutex
	resolutionRequests := 0
	promptWrites := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch r.URL.Path {
		case "/project/current":
			resolutionRequests++
			_, _ = w.Write([]byte(`{"project":42,"project_source":"config"}`))
		case "/prompts":
			promptWrites++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	sessionID := "resolver-type-boundary"
	stateFile := filepath.Join(os.TempDir(), "engram-claude-"+sessionID+"-tools-loaded")
	_ = os.Remove(stateFile)
	t.Cleanup(func() { _ = os.Remove(stateFile) })

	run := exec.Command(powershellPath, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", adapterPath)
	run.Env = withoutEngramPort(os.Environ())
	run.Env = append(run.Env, "ENGRAM_PORT="+port)
	run.Stdin = strings.NewReader(`{"session_id":"` + sessionID + `","cwd":"C:/workspace","prompt":"persist this prompt"}`)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("run UserPromptSubmit adapter: %v: %s", err, output)
	}

	mu.Lock()
	defer mu.Unlock()
	if resolutionRequests != 1 {
		t.Fatalf("canonical resolution requests = %d, want 1", resolutionRequests)
	}
	if promptWrites != 0 {
		t.Fatalf("prompt writes = %d, want 0 for a non-string canonical project", promptWrites)
	}
}

func claudeCodePowerShell(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"pwsh", "powershell.exe"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	t.Skip("requires PowerShell")
	return ""
}

func withoutEngramPort(environment []string) []string {
	filtered := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(strings.ToUpper(entry), "ENGRAM_PORT=") {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}
