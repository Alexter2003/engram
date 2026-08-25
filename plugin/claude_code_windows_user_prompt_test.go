package plugin_test

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestClaudeCodeWindowsPromptResolverRejectsMalformedCanonicalProject(t *testing.T) {
	powershellPath := claudeCodePowerShell(t)
	adapterPath := filepath.Join(repoRoot(t), "plugin", "claude-code", "scripts", "user-prompt-submit.ps1")

	for _, test := range []struct {
		name       string
		resolution string
	}{
		{name: "non-string project", resolution: `{"project":42,"project_source":"config"}`},
		{name: "incorrectly cased project property", resolution: `{"Project":"canonical-project","project_source":"config"}`},
		{name: "incorrectly cased project source property", resolution: `{"project":"canonical-project","Project_Source":"config"}`},
		{name: "incorrectly cased project source value", resolution: `{"project":"canonical-project","project_source":"CONFIG"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			resolutionRequests := 0
			promptWrites := 0
			port := claudeCodeWindowsPromptServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()

				switch r.URL.Path {
				case "/project/current":
					resolutionRequests++
					_, _ = w.Write([]byte(test.resolution))
				case "/prompts":
					promptWrites++
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
					w.WriteHeader(http.StatusNotFound)
				}
			}))

			runClaudeCodeWindowsPromptHook(t, powershellPath, adapterPath, port, "resolver-malformed", "persist this prompt")

			mu.Lock()
			defer mu.Unlock()
			if resolutionRequests != 1 {
				t.Fatalf("canonical resolution requests = %d, want 1", resolutionRequests)
			}
			if promptWrites != 0 {
				t.Fatalf("prompt writes = %d, want 0 for malformed canonical project", promptWrites)
			}
		})
	}
}

func TestClaudeCodeWindowsPromptResolverPersistsCanonicalProject(t *testing.T) {
	powershellPath := claudeCodePowerShell(t)
	adapterPath := filepath.Join(repoRoot(t), "plugin", "claude-code", "scripts", "user-prompt-submit.ps1")

	var mu sync.Mutex
	resolutionRequests := 0
	promptWrites := 0
	var promptPayload struct {
		SessionID string `json:"session_id"`
		Project   string `json:"project"`
		Content   string `json:"content"`
	}
	port := claudeCodeWindowsPromptServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch r.URL.Path {
		case "/project/current":
			resolutionRequests++
			_, _ = w.Write([]byte(`{"project":"canonical-project","project_source":"config"}`))
		case "/prompts":
			promptWrites++
			if r.Method != http.MethodPost {
				t.Errorf("prompt method = %s, want POST", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&promptPayload); err != nil {
				t.Errorf("decode prompt payload: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	runClaudeCodeWindowsPromptHook(t, powershellPath, adapterPath, port, "canonical-persistence", "persist this prompt")

	mu.Lock()
	defer mu.Unlock()
	if resolutionRequests != 1 {
		t.Fatalf("canonical resolution requests = %d, want 1", resolutionRequests)
	}
	if promptWrites != 1 {
		t.Fatalf("prompt writes = %d, want 1", promptWrites)
	}
	if promptPayload.SessionID != "canonical-persistence" || promptPayload.Project != "canonical-project" || promptPayload.Content != "persist this prompt" {
		t.Fatalf("prompt payload = %+v, want canonical project-bearing payload", promptPayload)
	}
}

func claudeCodeWindowsPromptServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on IPv4 loopback: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address = %T, want *net.TCPAddr", listener.Addr())
	}
	return strconv.Itoa(tcpAddr.Port)
}

func runClaudeCodeWindowsPromptHook(t *testing.T, powershellPath, adapterPath, port, sessionID, prompt string) {
	t.Helper()
	stateFile := filepath.Join(os.TempDir(), "engram-claude-"+sessionID+"-tools-loaded")
	_ = os.Remove(stateFile)
	t.Cleanup(func() { _ = os.Remove(stateFile) })

	run := exec.Command(powershellPath, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", adapterPath)
	run.Env = withoutEngramPort(os.Environ())
	run.Env = append(run.Env, "ENGRAM_PORT="+port)
	run.Stdin = strings.NewReader(`{"session_id":"` + sessionID + `","cwd":"C:/workspace","prompt":"` + prompt + `"}`)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("run UserPromptSubmit adapter: %v: %s", err, output)
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
