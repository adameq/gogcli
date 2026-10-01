package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExecuteDriveTreeJSON(t *testing.T) {
	svc, closeSrv := newDriveTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/files") {
			http.NotFound(w, r)
			return
		}
		requireQuery(t, r, "supportsAllDrives", "true")
		requireQuery(t, r, "includeItemsFromAllDrives", "true")

		q := r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "'root' in parents"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{
					{
						"id":           "folder1",
						"name":         "Reports",
						"mimeType":     driveMimeFolder,
						"modifiedTime": "2026-01-01T00:00:00Z",
					},
					{
						"id":           "file1",
						"name":         "root.txt",
						"mimeType":     "text/plain",
						"size":         "12",
						"modifiedTime": "2026-01-02T00:00:00Z",
					},
					{
						"id":           "shortcut1",
						"name":         "Reports elsewhere",
						"mimeType":     driveMimeShortcut,
						"modifiedTime": "2026-01-02T00:00:00Z",
						"shortcutDetails": map[string]any{
							"targetId":       "folder-target",
							"targetMimeType": driveMimeFolder,
						},
					},
				},
			})
		case strings.Contains(q, "'folder1' in parents"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{
					{
						"id":           "file2",
						"name":         "child.txt",
						"mimeType":     "text/plain",
						"size":         "5",
						"modifiedTime": "2026-01-03T00:00:00Z",
					},
				},
			})
		default:
			t.Fatalf("unexpected query: %q", q)
		}
	}))
	defer closeSrv()

	result := executeWithDriveTestService(t, []string{"--json", "--account", "a@example.com", "drive", "tree", "--parent", "root", "--depth", "2"}, svc)
	if result.err != nil {
		t.Fatalf("Execute: %v\nstderr=%s", result.err, result.stderr)
	}

	var parsed struct {
		Items []driveTreeItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &parsed); err != nil {
		t.Fatalf("json parse: %v\nout=%q", err, result.stdout)
	}
	if len(parsed.Items) != 4 {
		t.Fatalf("items len = %d, want 4: %#v", len(parsed.Items), parsed.Items)
	}
	if parsed.Items[2].Path != "Reports elsewhere" || driveShortcutDetailsTargetID(parsed.Items[2].ShortcutDetails) != "folder-target" {
		t.Fatalf("shortcut item = %#v", parsed.Items[2])
	}
	if parsed.Items[3].Path != "Reports/child.txt" {
		t.Fatalf("nested path = %q, want Reports/child.txt", parsed.Items[3].Path)
	}

	for _, tc := range []struct {
		name       string
		args       []string
		wantHeader string
	}{
		{
			name:       "tree",
			args:       []string{"--plain", "--account", "a@example.com", "drive", "tree", "--parent", "root", "--depth", "2"},
			wantHeader: "PATH\tTYPE\tSIZE\tMODIFIED\tID\n",
		},
		{
			name:       "inventory",
			args:       []string{"--plain", "--account", "a@example.com", "drive", "inventory", "--parent", "root", "--depth", "2"},
			wantHeader: "PATH\tTYPE\tSIZE\tMODIFIED\tOWNER\tID\n",
		},
	} {
		t.Run(tc.name+" plain schema", func(t *testing.T) {
			plainResult := executeWithDriveTestService(t, tc.args, svc)
			if plainResult.err != nil {
				t.Fatalf("Execute: %v\nstderr=%s", plainResult.err, plainResult.stderr)
			}
			if !strings.HasPrefix(plainResult.stdout, tc.wantHeader) {
				t.Fatalf("plain output header = %q, want prefix %q", plainResult.stdout, tc.wantHeader)
			}
			if strings.Contains(plainResult.stdout, "TARGET_ID") {
				t.Fatalf("plain output schema changed unexpectedly: %q", plainResult.stdout)
			}
		})
	}
}

func TestDriveReportingPreservesRepeatedPlacements(t *testing.T) {
	svc, closeSrv := newDriveTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/files") {
			http.NotFound(w, r)
			return
		}

		q := r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "'root' in parents"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{
					{"id": "a", "name": "A", "mimeType": driveMimeFolder},
					{"id": "b", "name": "B", "mimeType": driveMimeFolder},
				},
			})
		case strings.Contains(q, "'a' in parents"), strings.Contains(q, "'b' in parents"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{
					{"id": "shared-folder", "name": "Shared", "mimeType": driveMimeFolder},
				},
			})
		case strings.Contains(q, "'shared-folder' in parents"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{
					{"id": "shared-file", "name": "data.bin", "mimeType": "application/octet-stream", "size": "10"},
				},
			})
		default:
			t.Fatalf("unexpected query: %q", q)
		}
	}))
	defer closeSrv()

	treeResult := executeWithDriveTestService(t, []string{
		"--json", "--account", "a@example.com",
		"drive", "tree", "--parent", "root", "--depth", "3",
	}, svc)
	if treeResult.err != nil {
		t.Fatalf("tree: %v\nstderr=%s", treeResult.err, treeResult.stderr)
	}

	var tree struct {
		Items []driveTreeItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(treeResult.stdout), &tree); err != nil {
		t.Fatalf("tree JSON: %v\nout=%q", err, treeResult.stdout)
	}
	paths := make(map[string]string, len(tree.Items))
	for _, item := range tree.Items {
		paths[item.Path] = item.ID
	}
	for path, id := range map[string]string{
		"A/Shared":          "shared-folder",
		"B/Shared":          "shared-folder",
		"A/Shared/data.bin": "shared-file",
		"B/Shared/data.bin": "shared-file",
	} {
		if got := paths[path]; got != id {
			t.Errorf("tree path %q id = %q, want %q; items=%#v", path, got, id, tree.Items)
		}
	}

	truncatedResult := executeWithDriveTestService(t, []string{
		"--json", "--account", "a@example.com",
		"drive", "tree", "--parent", "root", "--depth", "3", "--max", "3",
	}, svc)
	if truncatedResult.err != nil {
		t.Fatalf("truncated tree: %v\nstderr=%s", truncatedResult.err, truncatedResult.stderr)
	}
	var truncated struct {
		Items     []driveTreeItem `json:"items"`
		Truncated bool            `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(truncatedResult.stdout), &truncated); err != nil {
		t.Fatalf("truncated tree JSON: %v\nout=%q", err, truncatedResult.stdout)
	}
	if len(truncated.Items) != 3 || !truncated.Truncated {
		t.Fatalf("truncated tree = items %d truncated %t, want 3/true", len(truncated.Items), truncated.Truncated)
	}

	duResult := executeWithDriveTestService(t, []string{
		"--json", "--account", "a@example.com",
		"drive", "du", "--parent", "root", "--depth", "2", "--sort", "path",
	}, svc)
	if duResult.err != nil {
		t.Fatalf("du: %v\nstderr=%s", duResult.err, duResult.stderr)
	}

	var du struct {
		Folders []driveDuSummary `json:"folders"`
	}
	if err := json.Unmarshal([]byte(duResult.stdout), &du); err != nil {
		t.Fatalf("du JSON: %v\nout=%q", err, duResult.stdout)
	}
	summaries := make(map[string]driveDuSummary, len(du.Folders))
	for _, summary := range du.Folders {
		summaries[summary.Path] = summary
	}
	for _, path := range []string{".", "A", "B", "A/Shared", "B/Shared"} {
		summary, ok := summaries[path]
		if !ok {
			t.Errorf("missing du path %q: %#v", path, du.Folders)
			continue
		}
		wantSize := int64(20)
		wantFiles := 2
		if path != "." {
			wantSize = 10
			wantFiles = 1
		}
		if summary.Size != wantSize || summary.Files != wantFiles {
			t.Errorf("du path %q = size %d files %d, want size %d files %d", path, summary.Size, summary.Files, wantSize, wantFiles)
		}
	}
}

func TestDriveDuCountsShortcutWithoutTargetContent(t *testing.T) {
	svc, closeSrv := newDriveTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/files") {
			http.NotFound(w, r)
			return
		}
		if q := r.URL.Query().Get("q"); !strings.Contains(q, "'root' in parents") {
			t.Fatalf("unexpected query: %q", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"files": []map[string]any{
				{"id": "target", "name": "target.bin", "mimeType": "application/octet-stream", "size": "10"},
				{
					"id":       "shortcut",
					"name":     "target link",
					"mimeType": driveMimeShortcut,
					"size":     "999",
					"shortcutDetails": map[string]any{
						"targetId":       "target",
						"targetMimeType": "application/octet-stream",
					},
				},
			},
		})
	}))
	defer closeSrv()

	result := executeWithDriveTestService(t, []string{
		"--json", "--account", "a@example.com",
		"drive", "du", "--parent", "root",
	}, svc)
	if result.err != nil {
		t.Fatalf("du: %v\nstderr=%s", result.err, result.stderr)
	}
	var parsed struct {
		Folders []driveDuSummary `json:"folders"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &parsed); err != nil {
		t.Fatalf("du JSON: %v\nout=%q", err, result.stdout)
	}
	if len(parsed.Folders) != 1 {
		t.Fatalf("folders = %#v, want root only", parsed.Folders)
	}
	root := parsed.Folders[0]
	if root.Path != "." || root.Size != 10 || root.Files != 2 {
		t.Fatalf("root summary = %#v, want size 10 and 2 file placements", root)
	}
}

func TestDriveTreeRejectsFolderCycle(t *testing.T) {
	svc, closeSrv := newDriveTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/files") {
			http.NotFound(w, r)
			return
		}

		q := r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "'root' in parents"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{
					{"id": "a", "name": "A", "mimeType": driveMimeFolder},
				},
			})
		case strings.Contains(q, "'a' in parents"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{
					{"id": "root", "name": "Root again", "mimeType": driveMimeFolder},
				},
			})
		default:
			t.Fatalf("unexpected query: %q", q)
		}
	}))
	defer closeSrv()

	result := executeWithDriveTestService(t, []string{
		"--json", "--account", "a@example.com",
		"drive", "tree", "--parent", "root", "--depth", "3",
	}, svc)
	if result.err == nil {
		t.Fatalf("expected cycle error, got stdout=%q stderr=%q", result.stdout, result.stderr)
	}
	if !strings.Contains(result.err.Error(), `drive folder cycle detected at "A/Root again" (id root)`) {
		t.Fatalf("cycle error = %q", result.err)
	}
}

func TestListDriveChildrenRejectsRepeatedPageToken(t *testing.T) {
	t.Parallel()

	var listCalls atomic.Int32
	svc, closeSvc := newDriveTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/files") {
			http.NotFound(w, r)
			return
		}
		n := listCalls.Add(1)
		if n > 2 {
			http.Error(w, "unexpected extra page request", http.StatusBadRequest)
			return
		}
		wantToken := ""
		if n == 2 {
			wantToken = "stuck"
		}
		requireSupportsAllDrives(t, r)
		requireQuery(t, r, "includeItemsFromAllDrives", "true")
		requireQuery(t, r, "corpora", "allDrives")
		requireQuery(t, r, "pageSize", "1000")
		requireQuery(t, r, "orderBy", "folder,name")
		requireQuery(t, r, "pageToken", wantToken)
		if q := r.URL.Query().Get("q"); !strings.Contains(q, "'root' in parents") {
			t.Fatalf("query = %q, want root parent", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"files": []any{
				map[string]any{"id": "child-1", "name": "a.txt", "mimeType": "text/plain"},
			},
			"nextPageToken": "stuck",
		})
	}))
	defer closeSvc()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	files, err := listDriveChildren(ctx, svc, "", driveTreeFields, true)
	if err == nil || !strings.Contains(err.Error(), "repeated page token") {
		t.Fatalf("err = %v after %d list calls", err, listCalls.Load())
	}
	if files != nil {
		t.Fatalf("unexpected partial children: %#v", files)
	}
	if got := listCalls.Load(); got != 2 {
		t.Fatalf("list calls = %d, want 2", got)
	}
	t.Logf("err = %v after %d list calls", err, listCalls.Load())
}

func TestListDriveChildrenLaterPages(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name           string
		failSecondPage bool
	}{
		{name: "success preserves page order"},
		{name: "later error discards partial children", failSecondPage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var listCalls atomic.Int32
			svc, closeSvc := newDriveTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/files") {
					http.NotFound(w, r)
					return
				}
				requireSupportsAllDrives(t, r)
				requireQuery(t, r, "pageSize", "1000")
				requireQuery(t, r, "orderBy", "folder,name")
				if q := r.URL.Query().Get("q"); !strings.Contains(q, "'parent' in parents") {
					t.Fatalf("query = %q, want parent folder", q)
				}
				n := listCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				switch n {
				case 1:
					requireQuery(t, r, "pageToken", "")
					_, _ = io.WriteString(w, `{"files":[{"id":"child-1","name":"1.txt","mimeType":"text/plain"}],"nextPageToken":"page-2"}`)
				case 2:
					requireQuery(t, r, "pageToken", "page-2")
					if tc.failSecondPage {
						http.Error(w, `{"error":{"code":403,"message":"page two denied"}}`, http.StatusForbidden)
						return
					}
					_, _ = io.WriteString(w, `{"files":[{"id":"child-2","name":"2.txt","mimeType":"text/plain"}]}`)
				default:
					http.Error(w, "unexpected extra page request", http.StatusBadRequest)
				}
			}))
			defer closeSvc()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			files, err := listDriveChildren(ctx, svc, "parent", driveTreeFields, true)
			if tc.failSecondPage {
				if err == nil || !strings.Contains(err.Error(), "page two denied") || files != nil {
					t.Fatalf("files = %#v, err = %v; want nil children and provider error", files, err)
				}
			} else if err != nil || len(files) != 2 || files[0].Id != "child-1" || files[1].Id != "child-2" {
				t.Fatalf("files = %#v, err = %v; want both pages in order", files, err)
			}
			if got := listCalls.Load(); got != 2 {
				t.Fatalf("list calls = %d, want 2", got)
			}
		})
	}
}

func TestExecuteDriveReportingRejectsRepeatedPageToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "tree", args: []string{"drive", "tree", "--parent", "root", "--depth", "1"}},
		{name: "inventory", args: []string{"drive", "inventory", "--parent", "root", "--depth", "1"}},
		{name: "du", args: []string{"drive", "du", "--parent", "root", "--depth", "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var listCalls atomic.Int32
			svc, closeSvc := newDriveTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/files") {
					http.NotFound(w, r)
					return
				}
				n := listCalls.Add(1)
				if n > 2 {
					http.Error(w, "unexpected extra page request", http.StatusBadRequest)
					return
				}
				requireSupportsAllDrives(t, r)
				if n == 1 {
					requireQuery(t, r, "pageToken", "")
				} else {
					requireQuery(t, r, "pageToken", "stuck")
				}
				if q := r.URL.Query().Get("q"); !strings.Contains(q, "'root' in parents") {
					t.Fatalf("query = %q, want root parent", q)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"files": []any{
						map[string]any{"id": "loop", "name": "stuck.txt", "mimeType": "text/plain"},
					},
					"nextPageToken": "stuck",
				})
			}))
			defer closeSvc()

			args := append([]string{"--json", "--account", "owner@example.com", "--no-input"}, tc.args...)
			result := executeWithDriveTestService(t, args, svc)
			wantError := `list Drive folder root: pagination loop: repeated page token "stuck"`
			if result.err == nil || !strings.Contains(result.err.Error(), wantError) || ExitCode(result.err) != 1 {
				t.Fatalf("err = %v, exit = %d, stderr = %q; want folder list error", result.err, ExitCode(result.err), result.stderr)
			}
			if !strings.Contains(result.stderr, wantError) {
				t.Fatalf("stdout = %q, stderr = %q; want only the list error", result.stdout, result.stderr)
			}
			assertOnlyJSONError(t, result.stdout, 1)
			if got := listCalls.Load(); got != 2 {
				t.Fatalf("list calls = %d, want 2", got)
			}
			t.Logf("%v; no success output", result.err)
		})
	}
}
