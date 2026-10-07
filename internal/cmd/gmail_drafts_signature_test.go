package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/api/gmail/v1"

	"github.com/openclaw/gogcli/internal/app"
)

type draftSignatureCapture struct {
	raw           string
	sigFetchedFor []string
}

// newSignatureComposeService serves send-as (primary me@example.com plus
// alias@example.com), per-address signatures (an empty value in signatures
// means "configured but empty"), the reply source message, an existing draft d1,
// and captures the Raw of whatever draft is created (POST) or updated (PUT).
func newSignatureComposeService(t *testing.T, signatures map[string]string, source map[string]any) (*gmail.Service, *draftSignatureCapture, func()) {
	t.Helper()
	captured := &draftSignatureCapture{}
	svc, cleanup := newGmailServiceForTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		const sendAsPrefix = "/gmail/v1/users/me/settings/sendAs/"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/gmail/v1/users/me/settings/sendAs":
			sendAsListHandler(w)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, sendAsPrefix):
			email := strings.TrimPrefix(r.URL.Path, sendAsPrefix)
			captured.sigFetchedFor = append(captured.sigFetchedFor, email)
			_ = json.NewEncoder(w).Encode(map[string]any{"sendAsEmail": email, "signature": signatures[email]})
		case r.Method == http.MethodGet && r.URL.Path == "/gmail/v1/users/me/messages/msg-1":
			_ = json.NewEncoder(w).Encode(source)
		case r.Method == http.MethodGet && r.URL.Path == "/gmail/v1/users/me/messages/orig-msg-1":
			_ = json.NewEncoder(w).Encode(mockOriginalMessage(false))
		case r.Method == http.MethodGet && r.URL.Path == "/gmail/v1/users/me/drafts/d1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "d1", "message": map[string]any{"id": "m1"}})
		case r.Method == http.MethodPost && r.URL.Path == "/gmail/v1/users/me/messages/send":
			writeGmailSendResponse(t, w, r, &captured.raw)
		case (r.Method == http.MethodPost && r.URL.Path == "/gmail/v1/users/me/drafts") ||
			(r.Method == http.MethodPut && r.URL.Path == "/gmail/v1/users/me/drafts/d1"):
			var draft gmail.Draft
			if err := json.NewDecoder(r.Body).Decode(&draft); err != nil {
				t.Fatalf("decode draft: %v", err)
			}
			raw, err := base64.RawURLEncoding.DecodeString(draft.Message.Raw)
			if err != nil {
				t.Fatalf("decode raw: %v", err)
			}
			captured.raw = string(raw)
			writeDraftCreatedResponse(w)
		default:
			http.NotFound(w, r)
		}
	})
	return svc, captured, cleanup
}

func primarySignature() map[string]string {
	return map[string]string{"me@example.com": `<div>Kind regards<br>Me Person</div>`}
}

func TestGmailDraftsCreate_AppendsSendAsSignatureToPlainAndHTML(t *testing.T) {
	svc, got, cleanup := newSignatureComposeService(t, primarySignature(), nil)
	defer cleanup()

	result := executeWithGmailTestService(t, []string{
		"--account", "me@example.com", "gmail", "drafts", "create",
		"--to", "a@example.com", "--subject", "Hi", "--body", "Body", "--body-html", "<p>Body</p>", "--signature",
	}, svc)
	if result.err != nil {
		t.Fatalf("drafts create: %v", result.err)
	}
	if !strings.Contains(got.raw, "Body\r\n\r\n--\r\nKind regards\r\nMe Person") {
		t.Fatalf("plain signature missing:\n%s", got.raw)
	}
	if !strings.Contains(got.raw, `<p>Body</p>`+"\r\n\r\n"+`<div class="gmail_signature"><div>Kind regards<br>Me Person</div></div>`) {
		t.Fatalf("html signature missing:\n%s", got.raw)
	}
}

func TestGmailDraftsCreate_SignatureStaysAboveQuote(t *testing.T) {
	svc, got, cleanup := newSignatureComposeService(t, primarySignature(), mockReplySourceMessage())
	defer cleanup()

	result := executeWithGmailTestService(t, []string{
		"--account", "me@example.com", "gmail", "drafts", "create",
		"--reply-to-message-id", "msg-1", "--quote", "--body", "Reply", "--signature",
	}, svc)
	if result.err != nil {
		t.Fatalf("drafts create: %v", result.err)
	}
	sig := strings.Index(got.raw, "Kind regards")
	quote := strings.Index(got.raw, "Original plain body.")
	if sig < 0 || quote < 0 || sig > quote {
		t.Fatalf("signature (at %d) must precede quote (at %d):\n%s", sig, quote, got.raw)
	}
}

func TestGmailDraftsCreate_AutoFromAliasUsesAliasSignature(t *testing.T) {
	source := map[string]any{
		"id": "msg-1", "threadId": "thread-1",
		"payload": map[string]any{"headers": []map[string]any{
			{"name": "Message-ID", "value": "<original@example.com>"},
			{"name": "From", "value": "alice@example.com"},
			{"name": "To", "value": "alias@example.com"},
			{"name": "Subject", "value": "Hi"},
		}},
	}
	svc, got, cleanup := newSignatureComposeService(t, map[string]string{
		"me@example.com":    "<div>primary sig</div>",
		"alias@example.com": "<div>alias sig</div>",
	}, source)
	defer cleanup()

	result := executeWithGmailTestService(t, []string{
		"--account", "me@example.com", "gmail", "drafts", "create",
		"--reply-to-message-id", "msg-1", "--body", "Reply", "--signature", "--auto-from-addressed-alias",
	}, svc)
	if result.err != nil {
		t.Fatalf("drafts create: %v", result.err)
	}
	if strings.Join(got.sigFetchedFor, ",") != "alias@example.com" {
		t.Fatalf("signature fetched for %v, want only alias@example.com", got.sigFetchedFor)
	}
	if !strings.Contains(got.raw, "Reply\r\n\r\n--\r\nalias sig") {
		t.Fatalf("alias signature missing:\n%s", got.raw)
	}
}

func TestGmailDraftsCreate_SignatureFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signature.txt")
	if err := os.WriteFile(path, []byte("Local Sig\nhttps://example.com"), 0o600); err != nil {
		t.Fatalf("write signature file: %v", err)
	}
	svc, got, cleanup := newSignatureComposeService(t, nil, nil)
	defer cleanup()

	result := executeWithGmailTestService(t, []string{
		"--account", "me@example.com", "gmail", "drafts", "create",
		"--to", "a@example.com", "--subject", "Hi", "--body", "Body", "--signature-file", path,
	}, svc)
	if result.err != nil {
		t.Fatalf("drafts create: %v", result.err)
	}
	if !strings.Contains(got.raw, "Body\r\n\r\n--\r\nLocal Sig\r\nhttps://example.com") {
		t.Fatalf("file signature missing:\n%s", got.raw)
	}
}

func TestGmailDraftsCreate_EmptySignatureWarnsAndCreatesDraft(t *testing.T) {
	svc, got, cleanup := newSignatureComposeService(t, map[string]string{"me@example.com": ""}, nil)
	defer cleanup()

	result := executeWithGmailTestService(t, []string{
		"--account", "me@example.com", "gmail", "drafts", "create",
		"--to", "a@example.com", "--subject", "Hi", "--body", "Body", "--signature",
	}, svc)
	if result.err != nil {
		t.Fatalf("drafts create: %v", result.err)
	}
	if !strings.Contains(result.stderr, "Warning: no signature configured for me@example.com") {
		t.Fatalf("expected warning on stderr, got %q", result.stderr)
	}
	if strings.Contains(result.stdout, "Warning") || got.raw == "" {
		t.Fatalf("stdout must stay clean and draft must be created; stdout=%q raw=%q", result.stdout, got.raw)
	}
}

func TestGmailDrafts_SignatureOptionsAreValidated(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"create conflict", []string{"drafts", "create", "--to", "a@example.com", "--subject", "Hi", "--body", "B", "--signature", "--signature-file", "sig.txt"}, "use only one of --signature/--signature-from or --signature-file"},
		{"update conflict", []string{"drafts", "update", "d1", "--to", "a@example.com", "--subject", "Hi", "--body", "B", "--signature-from", "alias@example.com", "--signature-file", "sig.txt"}, "use only one of --signature/--signature-from or --signature-file"},
		{"forward conflict", []string{"drafts", "forward", "orig-msg-1", "--to", "a@example.com", "--signature", "--signature-file", "sig.txt"}, "use only one of --signature/--signature-from or --signature-file"},
		{"create raw-file", []string{"drafts", "create", "--raw-file", "-", "--signature"}, "--raw-file cannot be combined with --signature"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, cleanup := newSignatureComposeService(t, nil, nil)
			defer cleanup()
			result := executeWithGmailTestService(t, append([]string{"--account", "me@example.com", "gmail"}, tc.args...), svc)
			if result.err == nil || !strings.Contains(result.err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", result.err, tc.want)
			}
		})
	}
}

func TestGmailDrafts_DryRunReportsSignatureFlags(t *testing.T) {
	cases := map[string][]string{
		"create":       {"drafts", "create", "--to", "a@example.com", "--subject", "Hi", "--body", "B"},
		"update":       {"drafts", "update", "d1", "--to", "a@example.com", "--subject", "Hi", "--body", "B"},
		"forward":      {"drafts", "forward", "orig-msg-1", "--to", "a@example.com"},
		"send forward": {"forward", "orig-msg-1", "--to", "a@example.com"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			full := append([]string{"--json", "--dry-run", "--account", "me@example.com", "gmail"}, args...)
			full = append(full, "--signature", "--signature-from", "alias@example.com")
			result := executeWithTestRuntime(t, full, &app.Runtime{Services: app.Services{
				Gmail: func(context.Context, string) (*gmail.Service, error) {
					return nil, errors.New("service must not be acquired during dry-run")
				},
			}})
			if result.err != nil {
				t.Fatalf("dry-run: %v", result.err)
			}
			var payload struct {
				Request map[string]any `json:"request"`
			}
			if err := json.Unmarshal([]byte(result.stdout), &payload); err != nil {
				t.Fatalf("decode dry-run output: %v\n%s", err, result.stdout)
			}
			want := map[string]any{"signature": true, "signature_from": "alias@example.com", "signature_file": ""}
			for key, value := range want {
				if payload.Request[key] != value {
					t.Fatalf("request[%q] = %#v, want %#v\n%s", key, payload.Request[key], value, result.stdout)
				}
			}
		})
	}
}

func TestGmailDraftsCreate_SignatureFromAlias(t *testing.T) {
	svc, got, cleanup := newSignatureComposeService(t, map[string]string{
		"me@example.com":    "<div>primary sig</div>",
		"alias@example.com": "<div>alias sig</div>",
	}, nil)
	defer cleanup()

	result := executeWithGmailTestService(t, []string{
		"--account", "me@example.com", "gmail", "drafts", "create",
		"--to", "a@example.com", "--subject", "Hi", "--body", "Body", "--signature-from", "alias@example.com",
	}, svc)
	if result.err != nil {
		t.Fatalf("drafts create: %v", result.err)
	}
	if !strings.Contains(got.raw, "Body\r\n\r\n--\r\nalias sig") || strings.Contains(got.raw, "primary sig") {
		t.Fatalf("expected only the alias signature:\n%s", got.raw)
	}
}

func TestGmailDraftsForward_SignatureFromAlias(t *testing.T) {
	t.Setenv("GOG_TIMEZONE", "UTC")
	svc, got, cleanup := newSignatureComposeService(t, map[string]string{
		"me@example.com":    "<div>primary sig</div>",
		"alias@example.com": "<div>alias sig</div>",
	}, nil)
	defer cleanup()

	result := executeWithGmailTestService(t, []string{
		"--account", "me@example.com", "gmail", "drafts", "forward", "orig-msg-1",
		"--to", "a@example.com", "--note", "FYI", "--signature-from", "alias@example.com",
	}, svc)
	if result.err != nil {
		t.Fatalf("drafts forward: %v", result.err)
	}
	if !strings.Contains(got.raw, "FYI\r\n\r\n--\r\nalias sig\r\n\r\n---------- Forwarded message") || strings.Contains(got.raw, "primary sig") {
		t.Fatalf("expected only the alias signature:\n%s", got.raw)
	}
}

func TestGmailDraftsForward_ImageOnlySignatureLeavesNoLoneSeparator(t *testing.T) {
	t.Setenv("GOG_TIMEZONE", "UTC")
	svc, got, cleanup := newSignatureComposeService(t, map[string]string{
		"me@example.com": `<img src="https://example.com/logo.png">`,
	}, nil)
	defer cleanup()

	result := executeWithGmailTestService(t, []string{
		"--account", "me@example.com", "gmail", "drafts", "forward", "orig-msg-1",
		"--to", "a@example.com", "--signature",
	}, svc)
	if result.err != nil {
		t.Fatalf("drafts forward: %v", result.err)
	}
	if strings.Contains(got.raw, "--\r\n\r\n---------- Forwarded message") || strings.Contains(got.raw, "\r\n--\r\n") {
		t.Fatalf("plain part has a lone signature separator:\n%s", got.raw)
	}
	if !strings.Contains(got.raw, `<div class="gmail_signature"><img src="https://example.com/logo.png"></div>`) {
		t.Fatalf("html signature block missing:\n%s", got.raw)
	}
}

func TestGmailDraftsUpdate_AppendsSendAsSignatureToNewBody(t *testing.T) {
	svc, got, cleanup := newSignatureComposeService(t, primarySignature(), nil)
	defer cleanup()

	result := executeWithGmailTestService(t, []string{
		"--account", "me@example.com", "gmail", "drafts", "update", "d1",
		"--to", "a@example.com", "--subject", "Hi", "--body", "New body", "--body-html", "<p>New body</p>", "--signature",
	}, svc)
	if result.err != nil {
		t.Fatalf("drafts update: %v", result.err)
	}
	if !strings.Contains(got.raw, "New body\r\n\r\n--\r\nKind regards\r\nMe Person") {
		t.Fatalf("plain signature missing:\n%s", got.raw)
	}
	if !strings.Contains(got.raw, `<div class="gmail_signature"><div>Kind regards<br>Me Person</div></div>`) {
		t.Fatalf("html signature missing:\n%s", got.raw)
	}
}

func TestGmailDraftsForward_SignatureBetweenNoteAndForwardedMessage(t *testing.T) {
	t.Setenv("GOG_TIMEZONE", "UTC")
	svc, got, cleanup := newSignatureComposeService(t, primarySignature(), nil)
	defer cleanup()

	result := executeWithGmailTestService(t, []string{
		"--account", "me@example.com", "gmail", "drafts", "forward", "orig-msg-1",
		"--to", "a@example.com", "--note", "FYI", "--signature",
	}, svc)
	if result.err != nil {
		t.Fatalf("drafts forward: %v", result.err)
	}
	if !strings.Contains(got.raw, "FYI\r\n\r\n--\r\nKind regards\r\nMe Person\r\n\r\n---------- Forwarded message ---------") {
		t.Fatalf("plain signature not between note and forwarded message:\n%s", got.raw)
	}
	if !strings.Contains(got.raw, `<div>FYI</div><br><div class="gmail_signature"><div>Kind regards<br>Me Person</div></div><br><div class="gmail_quote">`) {
		t.Fatalf("html signature not between note and forwarded message:\n%s", got.raw)
	}
}

func TestGmailForward_SignatureBetweenNoteAndForwardedMessage(t *testing.T) {
	t.Setenv("GOG_TIMEZONE", "UTC")
	svc, got, cleanup := newSignatureComposeService(t, primarySignature(), nil)
	defer cleanup()

	result := executeWithGmailTestService(t, []string{
		"--account", "me@example.com", "gmail", "forward", "orig-msg-1",
		"--to", "a@example.com", "--note", "FYI", "--signature",
	}, svc)
	if result.err != nil {
		t.Fatalf("forward: %v", result.err)
	}
	if !strings.Contains(got.raw, "FYI\r\n\r\n--\r\nKind regards\r\nMe Person\r\n\r\n---------- Forwarded message ---------") {
		t.Fatalf("plain signature not between note and forwarded message:\n%s", got.raw)
	}
	if !strings.Contains(got.raw, `<div>FYI</div><br><div class="gmail_signature"><div>Kind regards<br>Me Person</div></div><br><div class="gmail_quote">`) {
		t.Fatalf("html signature not between note and forwarded message:\n%s", got.raw)
	}
}

func TestGmailDraftsForward_SignatureWithoutNote(t *testing.T) {
	t.Setenv("GOG_TIMEZONE", "UTC")
	svc, got, cleanup := newSignatureComposeService(t, primarySignature(), nil)
	defer cleanup()

	result := executeWithGmailTestService(t, []string{
		"--account", "me@example.com", "gmail", "drafts", "forward", "orig-msg-1",
		"--to", "a@example.com", "--signature",
	}, svc)
	if result.err != nil {
		t.Fatalf("drafts forward: %v", result.err)
	}
	if !strings.Contains(got.raw, "--\r\nKind regards\r\nMe Person\r\n\r\n---------- Forwarded message ---------") {
		t.Fatalf("plain signature missing above forwarded message:\n%s", got.raw)
	}
	if !strings.Contains(got.raw, `<div class="gmail_signature"><div>Kind regards<br>Me Person</div></div><br><div class="gmail_quote">`) {
		t.Fatalf("html signature missing above forwarded message:\n%s", got.raw)
	}
}

func TestGmailDraftsForward_EmptySignatureWarns(t *testing.T) {
	t.Setenv("GOG_TIMEZONE", "UTC")
	svc, got, cleanup := newSignatureComposeService(t, map[string]string{"me@example.com": ""}, nil)
	defer cleanup()

	result := executeWithGmailTestService(t, []string{
		"--account", "me@example.com", "gmail", "drafts", "forward", "orig-msg-1",
		"--to", "a@example.com", "--note", "FYI", "--signature",
	}, svc)
	if result.err != nil {
		t.Fatalf("drafts forward: %v", result.err)
	}
	if !strings.Contains(result.stderr, "Warning: no signature configured for me@example.com") || got.raw == "" {
		t.Fatalf("expected warning and draft; stderr=%q raw=%q", result.stderr, got.raw)
	}
}
