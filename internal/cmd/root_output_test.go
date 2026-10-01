package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	"github.com/openclaw/gogcli/internal/app"
)

func assertOnlyJSONError(t *testing.T, stdout string, code int) string {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil || len(envelope) != 1 {
		t.Fatalf("want one JSON error without result fields: %s (%v)", stdout, err)
	}
	var failure struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Class   string `json:"class"`
	}
	if err := json.Unmarshal(envelope["error"], &failure); err != nil {
		t.Fatalf("missing JSON error: %s (%v)", stdout, err)
	}
	if failure.Code != code || failure.Message == "" || failure.Class == "" || stableExitCodes()[failure.Class] != code {
		t.Fatalf("unexpected error contract: %s, want code %d", stdout, code)
	}
	return failure.Message
}

func TestExecuteJSONErrorEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  string
		want bool
	}{
		{"unrelated flag", []string{"drive", "ls", "--body", "--json"}, "", true},
		{"parse", []string{"badcommand", "--json"}, "", true},
		{"runtime", []string{"--json", "drive", "ls", "--max=0"}, "", true},
		{"short", []string{"-j", "drive", "ls", "--max=0"}, "", true},
		{"alias", []string{"badcommand", "--machine"}, "", true},
		{"false", []string{"badcommand", "--json=false"}, "", false},
		{"auto false parse", []string{"badcommand", "--json=false"}, "GOG_AUTO_JSON", false},
		{"auto false runtime", []string{"--json=false", "drive", "ls", "--max=0"}, "GOG_AUTO_JSON", false},
		{"literal", []string{"badcommand", "--", "--json"}, "", false},
		{"flag value", []string{"--account", "--json", "badcommand"}, "", false},
		{"command flag value", []string{"gmail", "send", "--body", "--json", "--bad"}, "", false},
		{"env", []string{"drive", "ls", "--max=0"}, "GOG_JSON", true},
		{"auto", []string{"drive", "ls", "--max=0"}, "GOG_AUTO_JSON", true},
		{"plain override", []string{"--plain", "drive", "ls", "--max=0"}, "GOG_JSON", false},
		{"text", []string{"drive", "ls", "--max=0"}, "", false},
		{"transform", []string{"--json", "--results-only", "--select=id", "drive", "ls", "--max=0"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOG_JSON", "")
			t.Setenv("GOG_AUTO_JSON", "")
			t.Setenv("GOG_PLAIN", "")
			if tc.env != "" {
				t.Setenv(tc.env, "true")
			}
			result := executeWithTestRuntime(t, tc.args, nil)
			if ExitCode(result.err) != 2 || result.stderr == "" {
				t.Fatalf("result = %+v", result)
			}
			if !tc.want {
				if result.stdout != "" {
					t.Fatalf("unexpected stdout: %s", result.stdout)
				}
				return
			}
			var got struct {
				Error struct {
					Code           int
					Message, Class string
				}
			}
			if err := json.Unmarshal([]byte(result.stdout), &got); err != nil {
				t.Fatalf("single JSON envelope: %v; %s", err, result.stdout)
			}
			if got.Error.Code != 2 || got.Error.Class != "usage" || got.Error.Message == "" {
				t.Fatalf("envelope = %+v", got)
			}
		})
	}
	text := executeWithTestRuntime(t, []string{"drive", "ls", "--max=0"}, nil)
	machine := executeWithTestRuntime(t, []string{"--json", "drive", "ls", "--max=0"}, nil)
	if text.stderr != machine.stderr {
		t.Fatalf("stderr changed: %q != %q", text.stderr, machine.stderr)
	}
}

func TestExecuteJSONFailEmptyKeepsSingleResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"messages":[],"nextPageToken":""}`)
	}))
	defer server.Close()
	svc, err := gmail.NewService(context.Background(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	result := executeWithTestRuntime(t, []string{"--json", "--account=user@example.com", "gmail", "messages", "search", "test", "--fail-empty"}, &app.Runtime{Services: app.Services{
		Gmail: func(context.Context, string) (*gmail.Service, error) { return svc, nil },
	}})
	if ExitCode(result.err) != emptyResultsExitCode {
		t.Fatalf("result = %+v", result)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(result.stdout), &got); err != nil {
		t.Fatalf("multiple documents: %v; %s", err, result.stdout)
	}
	if _, ok := got["error"]; ok {
		t.Fatalf("result replaced with error: %s", result.stdout)
	}
	if got["count"] != float64(0) || got["has_more"] != false {
		t.Fatalf("aggregates: %s", result.stdout)
	}
}

func TestExecuteNoArgsStatus(t *testing.T) {
	t.Setenv("GOG_HOME", t.TempDir())
	t.Setenv("GOG_ACCOUNT", "")
	t.Setenv("GOG_KEYRING_BACKEND", "file")
	t.Setenv("GOG_PLAIN", "")
	t.Setenv("GOG_AUTO_JSON", "")
	t.Setenv("GOG_JSON", "")
	result := executeWithTestRuntime(t, nil, runtimeWithAuthStore(&fakeSecretsStore{}))
	if result.err != nil {
		t.Fatalf("result = %+v", result)
	}
	for _, want := range []string{"bin: ", "description: Google Workspace from the terminal", "config_path", "keyring_backend"} {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("missing %q: %s", want, result.stdout)
		}
	}
	if strings.Contains(result.stdout, "Usage:") {
		t.Fatalf("unexpected help: %s", result.stdout)
	}
	t.Setenv("GOG_JSON", "true")
	result = executeWithTestRuntime(t, nil, runtimeWithAuthStore(&fakeSecretsStore{}))
	if result.err != nil || !json.Valid([]byte(result.stdout)) {
		t.Fatalf("JSON status = %+v", result)
	}
	t.Setenv("GOG_ENABLE_COMMANDS_EXACT", "version")
	result = executeWithTestRuntime(t, nil, runtimeWithAuthStore(&fakeSecretsStore{}))
	if result.err == nil || !json.Valid([]byte(result.stdout)) {
		t.Fatalf("no-args bypassed command policy: %+v", result)
	}
}

func TestJSONErrorClasses(t *testing.T) {
	for class, code := range stableExitCodes() {
		if code == 0 {
			continue
		}
		var buf strings.Builder
		emitJSONErrorEnvelope(&buf, &ExitError{Code: code, Err: errors.New("test failure")})
		var got struct{ Error struct{ Class string } }
		if err := json.Unmarshal([]byte(buf.String()), &got); err != nil {
			t.Fatal(err)
		}
		if got.Error.Class != class {
			t.Fatalf("code %d: %q != %q", code, got.Error.Class, class)
		}
	}
}
