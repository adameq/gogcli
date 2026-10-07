package cmd

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/api/gmail/v1"
)

const (
	searchTestHTMLBody  = `<html><body><style>p{color:red}</style><p>Hello&nbsp;there</p><a href="https://track.example/x">link</a> see https://evil.example/path</body></html>`
	searchTestPlainBody = "Plain text with https://example.com/page inside"
)

type searchStub struct {
	mu            sync.Mutex
	getFormats    []string
	getHeaderSets [][]string
	// headerOverrides replaces served header values by name; set before running.
	headerOverrides map[string]string
}

func (s *searchStub) gets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.getFormats)
}

// newSearchStub serves one message per id. Like Gmail, metadata-format reads
// return only the headers named in metadataHeaders, so a test cannot pass by
// reading a header the command never asked for.
func newSearchStub(t *testing.T, ids []string, htmlBody, plainBody string) (*gmail.Service, *searchStub) {
	t.Helper()
	stub := &searchStub{}
	allHeaders := []map[string]string{
		{"name": "From", "value": "Me <me@example.com>"},
		{"name": "To", "value": "A &amp; B <a@one.example>, b@two.example"},
		{"name": "Cc", "value": "c@three.example"},
		{"name": "Bcc", "value": "Hidden https://bcc.example/p <h@four.example>"},
		{"name": "Subject", "value": "See https://subject.example/x"},
		{"name": "Date", "value": "Tue, 28 Jul 2026 03:36:00 +0000"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(path, "/users/me/messages"):
			msgs := make([]map[string]any, 0, len(ids))
			for _, id := range ids {
				msgs = append(msgs, map[string]any{"id": id, "threadId": "t-" + id})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": msgs})
		case strings.Contains(path, "/users/me/messages/"):
			format := r.URL.Query().Get("format")
			wanted := r.URL.Query()["metadataHeaders"]
			stub.mu.Lock()
			stub.getFormats = append(stub.getFormats, format)
			stub.getHeaderSets = append(stub.getHeaderSets, wanted)
			stub.mu.Unlock()

			headers := make([]map[string]string, 0, len(allHeaders))
			for _, h := range allHeaders {
				stub.mu.Lock()
				override, ok := stub.headerOverrides[h["name"]]
				stub.mu.Unlock()
				if ok {
					h = map[string]string{"name": h["name"], "value": override}
				}
				if format == "metadata" && !slices.Contains(wanted, h["name"]) {
					continue
				}
				headers = append(headers, h)
			}
			payload := map[string]any{"headers": headers}
			if format == "full" {
				payload["mimeType"] = "multipart/alternative"
				payload["parts"] = []map[string]any{
					{"mimeType": "text/plain", "body": map[string]any{"data": base64.URLEncoding.EncodeToString([]byte(plainBody))}},
					{"mimeType": "text/html", "body": map[string]any{"data": base64.URLEncoding.EncodeToString([]byte(htmlBody))}},
				}
				if plainBody == "" {
					payload["parts"] = []map[string]any{
						{"mimeType": "text/html", "body": map[string]any{"data": base64.URLEncoding.EncodeToString([]byte(htmlBody))}},
					}
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":       path[strings.LastIndex(path, "/")+1:],
				"threadId": "t",
				"labelIds": []string{"SENT"},
				"payload":  payload,
			})
		case strings.Contains(path, "/users/me/labels"):
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]any{{"id": "SENT", "name": "SENT", "type": "system"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return newGmailServiceFromServer(t, srv), stub
}

func runSearchJSON(t *testing.T, svc *gmail.Service, extra ...string) (raw map[string][]map[string]any, stdout string) {
	t.Helper()
	args := append([]string{"--json", "--account", "a@b.com", "gmail", "messages", "search", "in:sent"}, extra...)
	result := executeWithGmailTestService(t, args, svc)
	if result.err != nil {
		t.Fatalf("Execute: %v\nstderr=%q", result.err, result.stderr)
	}
	var envelope struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &envelope); err != nil {
		t.Fatalf("decode: %v\nstdout=%q", err, result.stdout)
	}
	return map[string][]map[string]any{"messages": envelope.Messages}, result.stdout
}

func TestGmailMessagesSearch_DefaultOmitsRecipientsAndAsksForSummaryHeadersOnly(t *testing.T) {
	svc, stub := newSearchStub(t, []string{"m1", "m2"}, searchTestHTMLBody, searchTestPlainBody)
	raw, stdout := runSearchJSON(t, svc)

	if len(raw["messages"]) != 2 {
		t.Fatalf("messages = %d, want 2", len(raw["messages"]))
	}
	for _, key := range []string{`"to"`, `"cc"`, `"bcc"`, `"body"`} {
		if strings.Contains(stdout, key) {
			t.Fatalf("default output must not contain %s: %s", key, stdout)
		}
	}
	if got := raw["messages"][0]["subject"]; got != "See https://subject.example/x" {
		t.Fatalf("default subject must stay unsanitized, got %q", got)
	}
	for _, set := range stub.getHeaderSets {
		if !slices.Equal(set, gmailMessageSummaryMetadataHeaders) {
			t.Fatalf("metadataHeaders = %v, want %v", set, gmailMessageSummaryMetadataHeaders)
		}
	}
	if gmailMessageSummaryMetadataHeaders[len(gmailMessageSummaryMetadataHeaders)-1] != "Date" || len(gmailMessageSummaryMetadataHeaders) != 3 {
		t.Fatalf("shared summary header list was mutated: %v", gmailMessageSummaryMetadataHeaders)
	}
}

func TestGmailMessagesSearch_IncludeRecipientsMetadataUsesSameGet(t *testing.T) {
	svc, stub := newSearchStub(t, []string{"m1", "m2", "m3"}, searchTestHTMLBody, searchTestPlainBody)
	raw, stdout := runSearchJSON(t, svc, "--include-recipients")

	if got := stub.gets(); got != 3 {
		t.Fatalf("message gets = %d, want exactly one per result (3)", got)
	}
	for i, set := range stub.getHeaderSets {
		if stub.getFormats[i] != "metadata" {
			t.Fatalf("format = %q, want metadata", stub.getFormats[i])
		}
		for _, want := range []string{"From", "Subject", "Date", "To", "Cc", "Bcc"} {
			if !slices.Contains(set, want) {
				t.Fatalf("metadataHeaders %v missing %s", set, want)
			}
		}
	}
	msg := raw["messages"][0]
	if msg["to"] != "A &amp; B <a@one.example>, b@two.example" || msg["cc"] != "c@three.example" || msg["bcc"] != "Hidden https://bcc.example/p <h@four.example>" {
		t.Fatalf("recipients = to:%q cc:%q bcc:%q", msg["to"], msg["cc"], msg["bcc"])
	}
	if strings.Contains(stdout, `"body"`) {
		t.Fatalf("unexpected body: %s", stdout)
	}
}

func TestGmailMessagesSearch_IncludeRecipientsFullReadsExistingPayload(t *testing.T) {
	svc, stub := newSearchStub(t, []string{"m1"}, searchTestHTMLBody, searchTestPlainBody)
	raw, _ := runSearchJSON(t, svc, "--include-body", "--include-recipients")

	if got := stub.gets(); got != 1 || stub.getFormats[0] != "full" {
		t.Fatalf("gets=%d formats=%v, want one full get", got, stub.getFormats)
	}
	if len(stub.getHeaderSets[0]) != 0 {
		t.Fatalf("full get must not send metadataHeaders: %v", stub.getHeaderSets[0])
	}
	msg := raw["messages"][0]
	if msg["to"] == "" || msg["cc"] != "c@three.example" || msg["bcc"] == "" {
		t.Fatalf("recipients missing in full mode: %v", msg)
	}
}

func TestGmailMessagesSearch_WrapsAddressHeaders(t *testing.T) {
	for _, projected := range []bool{false, true} {
		svc, _ := newSearchStub(t, []string{"m1"}, searchTestHTMLBody, searchTestPlainBody)
		args := []string{"--json", "--account", "a@b.com", "gmail", "messages", "search", "in:sent", "--include-recipients", "--wrap-untrusted"}
		if projected {
			args = append(args, "--results-only", "--select", "id,from,to,cc,bcc")
		}

		result := executeWithGmailTestService(t, args, svc)
		if result.err != nil {
			t.Fatal(result.err)
		}

		var messages []map[string]any
		if projected {
			if err := json.Unmarshal([]byte(result.stdout), &messages); err != nil {
				t.Fatal(err)
			}
		} else {
			var payload struct {
				Messages []map[string]any `json:"messages"`
			}
			if err := json.Unmarshal([]byte(result.stdout), &payload); err != nil {
				t.Fatal(err)
			}

			messages = payload.Messages
		}

		if len(messages) != 1 || messages[0]["id"] != "m1" {
			t.Fatalf("unexpected messages: %#v", messages)
		}

		for _, key := range []string{"from", "to", "cc", "bcc"} {
			value, _ := messages[0][key].(string)
			if !strings.Contains(value, "<<<EXTERNAL_UNTRUSTED_CONTENT") {
				t.Errorf("projected=%t: %s was not wrapped: %q", projected, key, value)
			}
		}
	}
}

func TestGmailMessagesSearch_WrapsDottedAddressProjection(t *testing.T) {
	svc, _ := newSearchStub(t, []string{"m1"}, searchTestHTMLBody, searchTestPlainBody)
	fields := []string{"messages.0.id", "messages.0.from", "messages.0.to", "messages.0.cc", "messages.0.bcc"}
	result := executeWithGmailTestService(t, []string{
		"--json", "--account", "a@b.com", "gmail", "messages", "search", "in:sent",
		"--include-recipients", "--wrap-untrusted", "--select", strings.Join(fields, ","),
	}, svc)
	if result.err != nil {
		t.Fatal(result.err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(result.stdout), &got); err != nil {
		t.Fatal(err)
	}

	if got[fields[0]] != "m1" {
		t.Fatalf("message ID changed: %#v", got)
	}

	for _, key := range fields[1:] {
		value, _ := got[key].(string)
		if !strings.Contains(value, "<<<EXTERNAL_UNTRUSTED_CONTENT") {
			t.Errorf("%s was not wrapped: %q", key, value)
		}
	}
}

func TestGmailMessagesSearch_IncludeBodyWithoutSanitizeIsUnchanged(t *testing.T) {
	svc, _ := newSearchStub(t, []string{"m1"}, searchTestHTMLBody, searchTestPlainBody)
	raw, _ := runSearchJSON(t, svc, "--include-body")

	msg := raw["messages"][0]
	if msg["body"] != searchTestPlainBody {
		t.Fatalf("body = %q, want the raw text/plain part", msg["body"])
	}
	if msg["subject"] != "See https://subject.example/x" {
		t.Fatalf("subject must stay unsanitized: %q", msg["subject"])
	}

	svc, _ = newSearchStub(t, []string{"m1"}, searchTestHTMLBody, searchTestPlainBody)
	raw, _ = runSearchJSON(t, svc, "--include-body", "--body-format", "html")
	if msg := raw["messages"][0]; msg["body"] != searchTestHTMLBody {
		t.Fatalf("html body = %q, want raw html part", msg["body"])
	}
}

func TestGmailMessagesSearch_SanitizeBodyMatchesGmailGet(t *testing.T) {
	cases := map[string]struct {
		html, plain string
	}{
		"html-only": {html: searchTestHTMLBody},
		"plain":     {html: searchTestHTMLBody, plain: searchTestPlainBody},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			svc, _ := newSearchStub(t, []string{"m1"}, tc.html, tc.plain)
			raw, stdout := runSearchJSON(t, svc, "--include-body", "--sanitize-content")
			got, _ := raw["messages"][0]["body"].(string)

			// Parity: build the exact payload gmail get would receive and
			// run it through the get --sanitize-content path.
			parts := []*gmail.MessagePart{}
			if tc.plain != "" {
				parts = append(parts, &gmail.MessagePart{MimeType: "text/plain", Body: &gmail.MessagePartBody{Data: base64.URLEncoding.EncodeToString([]byte(tc.plain))}})
			}
			parts = append(parts, &gmail.MessagePart{MimeType: "text/html", Body: &gmail.MessagePartBody{Data: base64.URLEncoding.EncodeToString([]byte(tc.html))}})
			if tc.plain == "" {
				parts = parts[len(parts)-1:]
			}
			want := sanitizedGmailMessage(&gmail.Message{Payload: &gmail.MessagePart{MimeType: "multipart/alternative", Parts: parts}}, true, false).Body
			if want == "" || got != want {
				t.Fatalf("search body %q != get --sanitize-content body %q", got, want)
			}
			if strings.ContainsAny(got, "<>") && tc.plain == "" {
				t.Fatalf("html leaked into sanitized body: %q", got)
			}
			if strings.Contains(stdout, "evil.example") || strings.Contains(stdout, "example.com/page") || strings.Contains(stdout, "track.example") {
				t.Fatalf("url leaked: %s", stdout)
			}
			if !strings.Contains(got, "[url removed]") {
				t.Fatalf("expected url placeholder in %q", got)
			}
		})
	}
}

func TestGmailMessagesSearch_SanitizeAndRecipientsTogether(t *testing.T) {
	svc, stub := newSearchStub(t, []string{"m1"}, searchTestHTMLBody, "")
	raw, _ := runSearchJSON(t, svc, "--include-body", "--sanitize-content", "--include-recipients")

	if got := stub.gets(); got != 1 {
		t.Fatalf("gets = %d, want 1", got)
	}
	msg := raw["messages"][0]
	if msg["to"] != "A & B <a@one.example>, b@two.example" {
		t.Fatalf("sanitized to = %q", msg["to"])
	}
	if msg["subject"] != "See [url removed]" {
		t.Fatalf("sanitized subject = %q", msg["subject"])
	}
	if body, _ := msg["body"].(string); body == "" || strings.Contains(body, "<") || strings.Contains(body, "evil.example") {
		t.Fatalf("body not sanitized: %q", body)
	}
}

func TestGmailMessagesSearch_SanitizeAndRecipientsWithoutBodyStaysMetadata(t *testing.T) {
	svc, stub := newSearchStub(t, []string{"m1", "m2"}, searchTestHTMLBody, searchTestPlainBody)
	raw, stdout := runSearchJSON(t, svc, "--include-recipients", "--sanitize-content")

	if got := stub.gets(); got != 2 {
		t.Fatalf("message gets = %d, want exactly one per result (2)", got)
	}
	for i, set := range stub.getHeaderSets {
		if stub.getFormats[i] != "metadata" {
			t.Fatalf("format = %q, want metadata", stub.getFormats[i])
		}
		for _, want := range []string{"From", "Subject", "Date", "To", "Cc", "Bcc"} {
			if !slices.Contains(set, want) {
				t.Fatalf("metadataHeaders %v missing %s", set, want)
			}
		}
	}
	msg := raw["messages"][0]
	want := map[string]string{
		"subject": "See [url removed]",
		"to":      "A & B <a@one.example>, b@two.example",
		"cc":      "c@three.example",
		"bcc":     "Hidden [url removed] <h@four.example>",
	}
	for key, value := range want {
		if msg[key] != value {
			t.Fatalf("%s = %q, want %q", key, msg[key], value)
		}
	}
	if strings.Contains(stdout, `"body"`) || strings.Contains(stdout, "bcc.example") || strings.Contains(stdout, "subject.example") {
		t.Fatalf("unexpected body or unsanitized URL: %s", stdout)
	}
}

func TestGmailMessagesSearch_SanitizeWithoutBodySanitizesHeadersOnly(t *testing.T) {
	svc, stub := newSearchStub(t, []string{"m1"}, searchTestHTMLBody, searchTestPlainBody)
	raw, stdout := runSearchJSON(t, svc, "--sanitize-content")

	if stub.getFormats[0] != "metadata" {
		t.Fatalf("format = %q, want metadata", stub.getFormats[0])
	}
	if raw["messages"][0]["subject"] != "See [url removed]" {
		t.Fatalf("subject = %v", raw["messages"][0]["subject"])
	}
	if strings.Contains(stdout, `"body"`) || strings.Contains(stdout, `"to"`) {
		t.Fatalf("unexpected fields: %s", stdout)
	}
}

func TestGmailMessagesSearch_SanitizeRejectsHTMLBodyFormat(t *testing.T) {
	svc, stub := newSearchStub(t, []string{"m1"}, searchTestHTMLBody, searchTestPlainBody)
	result := executeWithGmailTestService(t, []string{
		"--json", "--account", "a@b.com", "gmail", "messages", "search", "in:sent",
		"--include-body", "--body-format", "html", "--sanitize-content",
	}, svc)
	if result.err == nil || !strings.Contains(result.err.Error(), "--sanitize-content cannot be used with --body-format html") {
		t.Fatalf("err = %v", result.err)
	}
	if stub.gets() != 0 {
		t.Fatal("must fail before any Gmail call")
	}
}

func TestGmailMessagesSearch_SanitizedTableKeepsEncodedControlsInsideCells(t *testing.T) {
	svc, stub := newSearchStub(t, []string{"m1"}, searchTestHTMLBody, searchTestPlainBody)
	stub.mu.Lock()
	stub.headerOverrides = map[string]string{
		"From":    "Me&#9;Mallory&#10;<me@example.com>",
		"Subject": "left&#9;right&#10;next row https://subject.example/x",
	}
	stub.mu.Unlock()

	result := executeWithGmailTestService(t, []string{"--plain", "--account", "a@b.com", "gmail", "messages", "search", "in:sent", "--sanitize-content"}, svc)
	if result.err != nil {
		t.Fatalf("Execute: %v\nstderr=%q", result.err, result.stderr)
	}

	lines := strings.Split(strings.TrimSuffix(result.stdout, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("table has %d rows, want header + 1 message: %q", len(lines), result.stdout)
	}
	for i, line := range lines {
		if cols := strings.Split(line, "\t"); len(cols) != 6 {
			t.Fatalf("row %d has %d columns, want 6: %q", i, len(cols), line)
		}
	}
	cols := strings.Split(lines[1], "\t")
	if cols[3] != "Me Mallory <me@example.com>" {
		t.Fatalf("FROM = %q", cols[3])
	}
	if cols[4] != "left right next row [url removed]" {
		t.Fatalf("SUBJECT = %q", cols[4])
	}
}

func TestGmailMessagesSearch_SanitizedJSONKeepsDecodedControls(t *testing.T) {
	svc, stub := newSearchStub(t, []string{"m1"}, searchTestHTMLBody, searchTestPlainBody)
	stub.mu.Lock()
	stub.headerOverrides = map[string]string{"Subject": "left&#9;right"}
	stub.mu.Unlock()

	raw, _ := runSearchJSON(t, svc, "--sanitize-content")
	if got := raw["messages"][0]["subject"]; got != "left\tright" {
		t.Fatalf("JSON subject = %q, want decoded tab preserved", got)
	}
}

func TestMCPGmailSearchBuildsRecipientAndSanitizeArgs(t *testing.T) {
	tool := findMCPTool(t, "gmail_search")
	args, err := tool.BuildArgs(mcp.CallToolRequest{Params: mcp.CallToolParams{
		Arguments: map[string]any{
			"query":              "in:sent",
			"include_body":       true,
			"include_recipients": true,
			"sanitize_content":   true,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--include-body", "--include-recipients", "--sanitize-content"} {
		if !slices.Contains(args, want) {
			t.Fatalf("args %v missing %s", args, want)
		}
	}

	args, err = tool.BuildArgs(mcp.CallToolRequest{Params: mcp.CallToolParams{
		Arguments: map[string]any{"query": "in:sent"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(args, "--include-recipients") || slices.Contains(args, "--sanitize-content") {
		t.Fatalf("defaults must not enable new flags: %v", args)
	}
}
