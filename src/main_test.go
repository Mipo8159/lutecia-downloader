package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const token = "t0k"

// fake plays the sync server: /bundles like BundlesController, /sync/* like SyncController.
type fake struct {
	mu       sync.Mutex
	ids      []string
	files    map[string]map[string]string
	short    string // "<id>/<file>" served cut in half
	pullTo   string // a POST /sync/runs creates this bundle ("" = pull adds nothing)
	pullFail string // a POST /sync/runs ends 'failed' with this note
	busy     bool   // a pull is already running: POST gets 409, then it ends after one poll
	polls    int
	posts    int
}

func newFake(t *testing.T, f *fake) *httptest.Server {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+token {
		w.WriteHeader(401)
		return
	}
	switch {
	case r.Method == "POST" && r.URL.Path == "/sync/runs":
		f.posts++
		if f.busy {
			w.WriteHeader(409)
			io.WriteString(w, `{"message":"a run is already going"}`)
			return
		}
		w.WriteHeader(202)
		io.WriteString(w, `{"runId":7}`)
	case r.URL.Path == "/sync/status":
		io.WriteString(w, `{"active":{"runId":6}}`)
	case strings.HasPrefix(r.URL.Path, "/sync/runs/"):
		f.polls++
		if f.polls < 2 {
			io.WriteString(w, `{"status":"running","note":null}`)
			return
		}
		if f.pullFail != "" {
			io.WriteString(w, `{"status":"failed","note":"`+f.pullFail+`"}`)
			return
		}
		if f.pullTo != "" {
			f.ids = append(f.ids, f.pullTo)
			f.pullTo = ""
		}
		io.WriteString(w, `{"status":"ok","note":"12 rows"}`)
	case r.URL.Path == "/bundles":
		list := []map[string]any{}
		for _, id := range f.ids {
			var fs []map[string]any
			for name, body := range f.files[id] {
				fs = append(fs, map[string]any{"name": name, "size": len(body)})
			}
			list = append(list, map[string]any{"id": id, "files": fs})
		}
		json.NewEncoder(w).Encode(list)
	default:
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/bundles/"), "/files/")
		if len(parts) != 2 {
			w.WriteHeader(404)
			return
		}
		body, ok := f.files[parts[0]][parts[1]]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if parts[0]+"/"+parts[1] == f.short {
			body = body[:len(body)/2]
		}
		io.WriteString(w, body)
	}
}

var noPull = options{}
var fastPull = options{Pull: true, PollEvery: time.Millisecond, PullTimeout: 5 * time.Second}

func twoBundles() map[string]map[string]string {
	return map[string]map[string]string{
		"2026-10-14_030000": {"deals.csv": "Title\r\nA\r\n", "summary.json": "{}"},
		"2026-10-14_060000": {"deals.csv": "Title\r\nB\r\n"},
	}
}

func TestDownloadsOnlyNewBundlesOldestFirst(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000"}, files: twoBundles()}
	cfg := config{Server: newFake(t, f).URL, Token: token, Folder: t.TempDir()}

	got, err := run(cfg, noPull, io.Discard)
	if err != nil || !reflect.DeepEqual(got, []string{"2026-10-14_030000"}) {
		t.Fatalf("first run: %v %v", got, err)
	}
	if b, _ := os.ReadFile(filepath.Join(cfg.Folder, "2026-10-14_030000", "deals.csv")); string(b) != "Title\r\nA\r\n" {
		t.Fatalf("content: %q", b)
	}

	// The client imported and deleted the first folder; a new bundle appears on the server.
	os.RemoveAll(filepath.Join(cfg.Folder, "2026-10-14_030000"))
	f.ids = append(f.ids, "2026-10-14_060000")
	got, err = run(cfg, noPull, io.Discard)
	if err != nil || !reflect.DeepEqual(got, []string{"2026-10-14_060000"}) {
		t.Fatalf("second run: %v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Folder, "2026-10-14_030000")); !os.IsNotExist(err) {
		t.Fatal("a deleted (already imported) bundle came back")
	}
	if got, err = run(cfg, noPull, io.Discard); err != nil || len(got) != 0 {
		t.Fatalf("third run should find nothing: %v %v", got, err)
	}
	if f.posts != 0 {
		t.Fatal("--no-pull must not start a pull")
	}
}

func TestPullsFirstThenDownloadsTheFreshBundle(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000"}, files: twoBundles(), pullTo: "2026-10-14_060000"}
	cfg := config{Server: newFake(t, f).URL, Token: token, Folder: t.TempDir()}
	var out strings.Builder
	got, err := run(cfg, fastPull, &out)
	if err != nil || !reflect.DeepEqual(got, []string{"2026-10-14_030000", "2026-10-14_060000"}) {
		t.Fatalf("%v %v\n%s", got, err, out.String())
	}
	if !strings.Contains(out.String(), "Fresh data pulled") {
		t.Fatal(out.String())
	}
}

func TestWaitsForAPullThatIsAlreadyRunning(t *testing.T) {
	f := &fake{files: twoBundles(), busy: true, pullTo: "2026-10-14_030000"}
	cfg := config{Server: newFake(t, f).URL, Token: token, Folder: t.TempDir()}
	var out strings.Builder
	got, err := run(cfg, fastPull, &out)
	if err != nil || len(got) != 1 || !strings.Contains(out.String(), "already running") {
		t.Fatalf("%v %v\n%s", got, err, out.String())
	}
}

func TestFailedPullStillDownloadsWhatIsThere(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000"}, files: twoBundles(), pullFail: "APEX unreachable"}
	cfg := config{Server: newFake(t, f).URL, Token: token, Folder: t.TempDir()}
	var out strings.Builder
	got, err := run(cfg, fastPull, &out)
	if err != nil || len(got) != 1 || !strings.Contains(out.String(), "WARNING: no fresh data this time: the pull failed on the server: APEX unreachable") {
		t.Fatalf("%v %v\n%s", got, err, out.String())
	}
}

func TestCutDownloadLeavesNothingAndRetries(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000"}, files: map[string]map[string]string{
		"2026-10-14_030000": {"deals.csv": "Title\r\nAAAAAAAA\r\n"}}, short: "2026-10-14_030000/deals.csv"}
	cfg := config{Server: newFake(t, f).URL, Token: token, Folder: t.TempDir()}

	if _, err := run(cfg, noPull, io.Discard); err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("want a size error, got %v", err)
	}
	entries, _ := os.ReadDir(cfg.Folder)
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("left a folder behind: %s", e.Name())
		}
	}
	f.short = ""
	if got, err := run(cfg, noPull, io.Discard); err != nil || len(got) != 1 {
		t.Fatalf("retry: %v %v", got, err)
	}
}

func TestFatalErrorsStopEverything(t *testing.T) {
	f := &fake{ids: []string{"../../evil"}, files: map[string]map[string]string{}}
	srv := newFake(t, f)
	if _, err := run(config{Server: srv.URL, Token: "nope", Folder: t.TempDir()}, fastPull, io.Discard); err == nil || !strings.Contains(err.Error(), "rejected LUTECIA_TOKEN") {
		t.Fatalf("want token error, got %v", err)
	}
	if _, err := run(config{Server: "http://127.0.0.1:1", Token: token, Folder: t.TempDir()}, fastPull, io.Discard); err == nil || !strings.Contains(err.Error(), "cannot reach the server") {
		t.Fatalf("want unreachable error, got %v", err)
	}
	if _, err := run(config{Server: srv.URL, Token: token, Folder: t.TempDir()}, noPull, io.Discard); err == nil || !strings.Contains(err.Error(), "unexpected bundle name") {
		t.Fatalf("want name error, got %v", err)
	}
}

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadConfig(dir); err == nil || !strings.Contains(err.Error(), "copy .env.example") {
		t.Fatalf("missing .env must say what to do, got %v", err)
	}
	os.WriteFile(filepath.Join(dir, ".env"), []byte("\ufeff# comment\r\nLUTECIA_SERVER=http://10.0.0.5:3000/\r\nLUTECIA_TOKEN=\"abc\"\r\n"), 0o600)
	cfg, err := loadConfig(dir)
	if err != nil || cfg.Server != "http://10.0.0.5:3000" || cfg.Token != "abc" || cfg.Folder != filepath.Join(dir, "Bitrix import") {
		t.Fatalf("%+v %v", cfg, err)
	}
	t.Setenv("LUTECIA_TOKEN", "from-env")
	if cfg, _ := loadConfig(dir); cfg.Token != "from-env" {
		t.Fatalf("environment must win over .env: %+v", cfg)
	}
	t.Setenv("LUTECIA_TOKEN", "")
	if _, err := loadConfig(dir); err == nil || !strings.Contains(err.Error(), "LUTECIA_TOKEN is empty") {
		t.Fatalf("want empty-token error, got %v", err)
	}
}
