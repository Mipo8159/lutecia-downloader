package main

import (
	"encoding/json"
	"errors"
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

// fake plays the heartbeat server: /bundles like BundlesController, /heartbeat/* like HeartbeatController.
type fake struct {
	mu       sync.Mutex
	ids      []string
	files    map[string]map[string]string
	short    string // "<id>/<file>" served cut in half
	pullTo   string // a POST /heartbeat/runs creates this bundle ("" = pull adds nothing)
	pullFail string // a POST /heartbeat/runs ends 'failed' with this note
	busy     bool   // a pull is already running: POST gets 409, then it ends after one poll
	polls    int
	posts    int
	// The checkpoint of the one client the tests play (heartbeat keeps one per IP), as DownloadsController does.
	checkpoint string
	puts       []string // every PUT /downloads/checkpoint, in order
	putFail    int      // fail this many PUTs with a 500
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
	case r.Method == "POST" && r.URL.Path == "/heartbeat/runs":
		f.posts++
		if f.busy {
			w.WriteHeader(409)
			io.WriteString(w, `{"message":"a run is already going"}`)
			return
		}
		w.WriteHeader(202)
		io.WriteString(w, `{"runId":7}`)
	case r.URL.Path == "/heartbeat/status":
		io.WriteString(w, `{"active":{"runId":6}}`)
	case strings.HasPrefix(r.URL.Path, "/heartbeat/runs/"):
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
	case r.Method == "GET" && r.URL.Path == "/downloads":
		list := []map[string]any{}
		for _, id := range f.ids {
			if f.checkpoint != "" && id <= f.checkpoint {
				continue
			}
			var fs []map[string]any
			for name, body := range f.files[id] {
				fs = append(fs, map[string]any{"name": name, "size": len(body)})
			}
			list = append(list, map[string]any{"id": id, "files": fs})
		}
		body := map[string]any{"client": "203.0.113.1", "checkpoint": nil, "checkpointAt": nil, "firstDownload": f.checkpoint == "", "bundles": list}
		if f.checkpoint != "" {
			body["checkpoint"], body["checkpointAt"] = f.checkpoint, "2026-11-10T09:30:00"
		}
		json.NewEncoder(w).Encode(body)
	case r.Method == "PUT" && r.URL.Path == "/downloads/checkpoint":
		var b struct{ Bundle string }
		json.NewDecoder(r.Body).Decode(&b)
		f.puts = append(f.puts, b.Bundle)
		if f.putFail > 0 {
			f.putFail--
			w.WriteHeader(500)
			return
		}
		known := false
		for _, id := range f.ids {
			known = known || id == b.Bundle
		}
		if !known {
			w.WriteHeader(400)
			io.WriteString(w, `{"message":"no committed bundle"}`)
			return
		}
		if b.Bundle > f.checkpoint {
			f.checkpoint = b.Bundle
		}
		io.WriteString(w, `{}`)
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

const (
	productsHead = "External ID,Name,Price\r\n"
	contactsHead = "First name,Mobile,Comment\r\n"
)

// Bundle 1: two products, one contact. Bundle 2: P1's price changed, a new product, the contact bought again.
func twoBundles() map[string]map[string]string {
	return map[string]map[string]string{
		"2026-10-14_030000": {
			"products.csv":     productsHead + "IVF-P1,Lipstick,10.00\r\nIVF-P2,\"Perfume, 50ml\",99.00\r\n",
			"contacts.csv":     contactsHead + "ილია,555,Key: C-IVF-7091; Purchases: 1\r\n",
			"IMPORT_ORDER.txt": "1. products.csv\r\n",
			"summary.json":     "{}",
		},
		"2026-10-14_060000": {
			"products.csv": productsHead + "IVF-P1,Lipstick,12.00\r\nVST-P3,\"Gown\nlong\",5.00\r\n",
			"contacts.csv": contactsHead + "ილია,555,Key: C-IVF-7091; Purchases: 2\r\n",
		},
	}
}

func testConfig(t *testing.T, server string) config {
	return config{Server: server, Token: token, Folder: t.TempDir(), State: filepath.Join(t.TempDir(), "downloaded.txt")}
}

// readRaw: a file's text as it is (files this program did not write, such as fixtures it must leave alone).
func readRaw(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// read returns a file's text. Every .csv the client gets must start with a UTF-8 BOM (Windows Excel and Bitrix need it
// to see Georgian as UTF-8): read fails the test without one, then returns the text after it.
func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(path, ".csv") {
		rest, ok := strings.CutPrefix(string(b), "\ufeff")
		if !ok {
			t.Fatalf("%s: no UTF-8 BOM", filepath.Base(path))
		}
		return rest
	}
	return string(b)
}

func TestFoldsEveryNewBundleIntoOneFilePerEntity(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000", "2026-10-14_060000"}, files: twoBundles()}
	cfg := testConfig(t, newFake(t, f).URL)

	res, err := run(cfg, noPull, io.Discard)
	if err != nil || !reflect.DeepEqual(res.New, []string{"2026-10-14_030000", "2026-10-14_060000"}) {
		t.Fatalf("%+v %v", res, err)
	}
	if filepath.Dir(res.Folder) != cfg.Folder || !datedFolder.MatchString(filepath.Base(res.Folder)) ||
		!strings.HasSuffix(res.Folder, " (everything)") {
		t.Fatalf("the first download goes into a dated '(everything)' folder: %q", res.Folder)
	}
	if got := read(t, filepath.Join(res.Folder, "products.csv")); got != productsHead+
		"IVF-P1,Lipstick,12.00\r\nIVF-P2,\"Perfume, 50ml\",99.00\r\nVST-P3,\"Gown\r\nlong\",5.00\r\n" { // CRLF file: CRLF inside quotes too
		t.Fatalf("products: newer row must replace in place, new rows appended:\n%s", got)
	}
	if got := read(t, filepath.Join(res.Folder, "offline_contacts.csv")); got != contactsHead+"ილია,555,Key: C-IVF-7091; Purchases: 2\r\n" {
		t.Fatalf("contacts: same Key in Comment = same contact:\n%s", got)
	}
	var names []string
	entries, _ := os.ReadDir(res.Folder)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !reflect.DeepEqual(names, []string{manifestName, "excel", "offline_contacts.csv", "products.csv"}) {
		t.Fatalf("the folder must hold the two import files and their Excel copies: %v", names)
	}
	if f.checkpoint != "2026-10-14_060000" {
		t.Fatalf("the server's checkpoint must move to the last bundle: %q", f.checkpoint)
	}
	if !reflect.DeepEqual(res.Files, []fileRows{{"products.csv", 3}, {"offline_contacts.csv", 1}}) {
		t.Fatalf("import order and counts: %+v", res.Files)
	}
}

// The five files the client gets since 2026-11-03: online_* arrive under their own names, merge by Deal key / Contact
// key like the offline ones, and sit in Bitrix's import order (contacts before deals, offline before online).
func TestOnlineFilesMergeAndSortNextToOffline(t *testing.T) {
	dealsHead := "ID,Deal key,Deal name,Contact key,Product,Price,Quantity\r\n"
	onlineContactsHead := "First name,Mobile,Contact key,Online orders\r\n"
	f := &fake{ids: []string{"2026-11-03_030000", "2026-11-03_060000"}, files: map[string]map[string]string{
		"2026-11-03_030000": {
			"products.csv":        productsHead + "IVF-P1,Lipstick,10.00\r\n",
			"contacts.csv":        contactsHead + "ილია,555,Key: C-IVF-7091\r\n",
			"online_contacts.csv": onlineContactsHead + "Ana,599,P-1,1\r\n",
			"deals.csv":           dealsHead + "1,IVF-R1,IVF-101 #1,P-1,Lipstick,10.00,1\r\n",
			"online_deals.csv":    dealsHead + "1,WEB-21,WEB #21,P-1,Lipstick,10.00,1\r\n1,,,,Patch,5.00,1\r\n",
		},
		"2026-11-03_060000": {
			"online_contacts.csv": onlineContactsHead + "Ana,599,P-1,2\r\n",
			"online_deals.csv":    dealsHead + "1,WEB-22,WEB #22 2026-11-03,P-1,Lipstick,10.00,1\r\n",
		},
	}}
	cfg := testConfig(t, newFake(t, f).URL)
	res, err := run(cfg, noPull, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := []fileRows{{"products.csv", 1}, {"offline_contacts.csv", 1}, {"online_contacts.csv", 1},
		{"offline_deals.csv", 1}, {"online_deals.csv", 2}}
	if !reflect.DeepEqual(res.Files, want) {
		t.Fatalf("five files, import order, counts: %+v", res.Files)
	}
	if got := read(t, filepath.Join(res.Folder, "online_contacts.csv")); got != onlineContactsHead+"Ana,599,P-1,2\r\n" {
		t.Fatalf("online contact: the newer row replaces the older one:\n%s", got)
	}
	if got := read(t, filepath.Join(res.Folder, "online_deals.csv")); got != dealsHead+
		"1,WEB-21,WEB #21,P-1,Lipstick,10.00,1\r\n1,,,,Patch,5.00,1\r\n2,WEB-22,WEB #22 2026-11-03,P-1,Lipstick,10.00,1\r\n" {
		t.Fatalf("online deals: appended and renumbered:\n%s", got)
	}
	off, on := read(t, filepath.Join(res.Folder, "offline_deals.csv")), read(t, filepath.Join(res.Folder, "online_deals.csv"))
	if strings.SplitN(off, "\r\n", 2)[0] != strings.SplitN(on, "\r\n", 2)[0] {
		t.Fatal("offline and online deals must keep the same header")
	}
	if msg := summary(cfg, res); !strings.Contains(msg, "encoding UTF-8, separator comma") || !strings.Contains(msg, "products.csv, then offline_contacts.csv, then online_contacts.csv, "+
		"then offline_deals.csv, then online_deals.csv") || !strings.Contains(msg, "2 deals") {
		t.Fatalf("summary:\n%s", msg)
	}
}

// The point of checkpoints: the second download goes into its own dated folder with only what is new, and the
// first folder is left exactly as it was.
func TestEachLaterDownloadGetsItsOwnFolderWithOnlyWhatIsNew(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000"}, files: twoBundles()}
	cfg := testConfig(t, newFake(t, f).URL)
	first, err := run(cfg, noPull, io.Discard)
	if err != nil || !first.FirstDownload {
		t.Fatal(first, err)
	}
	before := read(t, filepath.Join(first.Folder, "products.csv"))

	f.ids = append(f.ids, "2026-10-14_060000")
	res, err := run(cfg, noPull, io.Discard)
	if err != nil || !reflect.DeepEqual(res.New, []string{"2026-10-14_060000"}) || res.FirstDownload {
		t.Fatalf("%+v %v", res, err)
	}
	if res.Folder == first.Folder || strings.Contains(res.Folder, "everything") {
		t.Fatalf("a new plain dated folder: %q", res.Folder)
	}
	if got := read(t, filepath.Join(res.Folder, "products.csv")); got != productsHead+"IVF-P1,Lipstick,12.00\r\nVST-P3,\"Gown\r\nlong\",5.00\r\n" {
		t.Fatalf("only the second bundle's rows:\n%s", got)
	}
	if read(t, filepath.Join(first.Folder, "products.csv")) != before {
		t.Fatal("the first folder must not change")
	}
	if !reflect.DeepEqual(res.Waiting, []string{filepath.Base(first.Folder), filepath.Base(res.Folder)}) {
		t.Fatalf("both folders, oldest first (same minute too): %v", res.Waiting)
	}
	msg := summary(cfg, res)
	for _, want := range []string{"only what is new or changed since the previous download (data up to 2026-10-14 03:00)",
		"Import the older folders first", filepath.Base(first.Folder), "2 products"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("summary lacks %q:\n%s", want, msg)
		}
	}
	if !reflect.DeepEqual(f.puts, []string{"2026-10-14_030000", "2026-10-14_030000", "2026-10-14_060000"}) {
		t.Fatalf("checkpoint after each folder (plus the start-up resend of the last one): %v", f.puts)
	}
}

func TestImportedAndDeletedMeansNothingComesBack(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000"}, files: twoBundles()}
	cfg := testConfig(t, newFake(t, f).URL)
	first, err := run(cfg, noPull, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(first.Folder) // the client imported it, then deleted it

	res, err := run(cfg, noPull, io.Discard)
	if err != nil || len(res.New) != 0 || res.Folder != "" || len(res.Waiting) != 0 {
		t.Fatalf("nothing new: no folder must come back: %+v %v", res, err)
	}
	if msg := summary(cfg, res); !strings.Contains(msg, "up to date") || strings.Contains(msg, "still there") {
		t.Fatal(msg)
	}
	if entries, _ := os.ReadDir(cfg.Folder); len(entries) != 0 {
		t.Fatalf("no empty folder for a download without data: %d entries", len(entries))
	}
}

func TestNothingNewButFoldersStillThere(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000"}, files: twoBundles()}
	cfg := testConfig(t, newFake(t, f).URL)
	first, _ := run(cfg, noPull, io.Discard)
	res, err := run(cfg, noPull, io.Discard)
	if err != nil || len(res.New) != 0 || res.Folder != "" || !reflect.DeepEqual(res.Waiting, []string{filepath.Base(first.Folder)}) {
		t.Fatalf("%+v %v", res, err)
	}
	if msg := summary(cfg, res); !strings.Contains(msg, "still there") || !strings.Contains(msg, filepath.Base(first.Folder)) {
		t.Fatal(msg)
	}
}

// Same office (the server knows its IP), a fresh unzip: only what is new since that office's last download.
func TestServerCheckpointMeansAFreshCopyGetsOnlyWhatIsNew(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000", "2026-10-14_060000"}, files: twoBundles(), checkpoint: "2026-10-14_030000"}
	cfg := testConfig(t, newFake(t, f).URL)
	res, err := run(cfg, noPull, io.Discard)
	if err != nil || !reflect.DeepEqual(res.New, []string{"2026-10-14_060000"}) || res.FirstDownload || strings.Contains(res.Folder, "everything") {
		t.Fatalf("%+v %v", res, err)
	}
	if res.Since != "2026-10-14 03:00" || strings.Contains(summary(cfg, res), "first download") {
		t.Fatalf("%+v\n%s", res, summary(cfg, res))
	}
}

// A copy that downloaded before checkpoints existed: its list says what its production folder holds. That is its
// baseline: the server learns it first, and only newer data comes, into a dated folder; production is left alone.
func TestCopyFromBeforeCheckpointsKeepsItsBaseline(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000", "2026-10-14_060000"}, files: twoBundles()}
	cfg := testConfig(t, newFake(t, f).URL)
	writeFiles(t, cfg.production(), map[string]string{"products.csv": "old data"})
	appendState(cfg.State, "2026-10-14_030000")

	res, err := run(cfg, noPull, io.Discard)
	if err != nil || !reflect.DeepEqual(res.New, []string{"2026-10-14_060000"}) || res.FirstDownload {
		t.Fatalf("%+v %v", res, err)
	}
	if !reflect.DeepEqual(f.puts, []string{"2026-10-14_030000", "2026-10-14_060000"}) || f.checkpoint != "2026-10-14_060000" {
		t.Fatalf("baseline first, then the new folder: %v %q", f.puts, f.checkpoint)
	}
	if readRaw(t, filepath.Join(cfg.production(), "products.csv")) != "old data" {
		t.Fatal("production must be left alone")
	}
	if !reflect.DeepEqual(res.Waiting, []string{"production", filepath.Base(res.Folder)}) ||
		!strings.Contains(summary(cfg, res), "   production\n") {
		t.Fatalf("an unimported production folder comes first: %v\n%s", res.Waiting, summary(cfg, res))
	}
}

// The folder was made but the server never heard of it (connection lost at the very end): the next run tells the
// server, and the same data does not come a second time.
func TestLostCheckpointIsSentAgainNotDownloadedAgain(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000"}, files: twoBundles(), putFail: 1}
	cfg := testConfig(t, newFake(t, f).URL)
	var out strings.Builder
	first, err := run(cfg, noPull, &out)
	if err != nil || first.Folder == "" || f.checkpoint != "" || !strings.Contains(out.String(), "next run tells it again") {
		t.Fatalf("%+v %v %q", first, err, out.String())
	}
	res, err := run(cfg, noPull, io.Discard)
	if err != nil || res.Folder != "" || len(res.New) != 0 || f.checkpoint != "2026-10-14_030000" {
		t.Fatalf("%+v %v %q", res, err, f.checkpoint)
	}
}

// Two windows at once (a double double-click) would put the same data into two folders: the second is turned away.
func TestSecondWindowIsTurnedAway(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000"}, files: twoBundles()}
	cfg := testConfig(t, newFake(t, f).URL)
	writeFiles(t, cfg.Folder, map[string]string{lockName: "123 now"})
	if _, err := run(cfg, fastPull, io.Discard); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("want already running, got %v", err)
	}
	if f.posts != 0 || len(f.puts) != 0 {
		t.Fatal("the second window must not touch the server")
	}
	if names, _ := datedFolders(cfg.Folder); len(names) != 0 {
		t.Fatal(names)
	}
	// a lock left by a computer switched off mid-run is taken over
	old := time.Now().Add(-3 * time.Hour)
	os.Chtimes(filepath.Join(cfg.Folder, lockName), old, old)
	res, err := run(cfg, noPull, io.Discard)
	if err != nil || res.Folder == "" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Folder, lockName)); err == nil {
		t.Fatal("the lock must be gone after the run")
	}
}

// The program stopped right after making the folder: neither its list nor the server heard of it. The folder's own
// list keeps the same data from coming a second time.
func TestFolderMadeButNotListedDoesNotComeAgain(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000"}, files: twoBundles(), putFail: 1}
	cfg := testConfig(t, newFake(t, f).URL)
	first, err := run(cfg, noPull, io.Discard)
	if err != nil || first.Folder == "" {
		t.Fatal(first, err)
	}
	os.Remove(cfg.State)
	res, err := run(cfg, noPull, io.Discard)
	if err != nil || res.Folder != "" || f.checkpoint != "2026-10-14_030000" {
		t.Fatalf("%+v %v %q", res, err, f.checkpoint)
	}
}

func TestDatedFoldersSortByTime(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"2026-11-17 09-00", "2026-11-16 14-05 (2)", "2026-11-16 14-05", "2026-11-16 14-05 (everything)",
		"2026-11-16 14-05 (10)", "production", "excel", ".2026-11-17 10-00.partial-123", "2026-11-16 notes"} {
		os.MkdirAll(filepath.Join(dir, n), 0o755)
	}
	got, err := datedFolders(dir)
	want := []string{"2026-11-16 14-05 (everything)", "2026-11-16 14-05", "2026-11-16 14-05 (2)", "2026-11-16 14-05 (10)", "2026-11-17 09-00"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%v %v", got, err)
	}
}

func TestPullsFirstThenDownloadsTheFreshBundle(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000"}, files: twoBundles(), pullTo: "2026-10-14_060000"}
	cfg := testConfig(t, newFake(t, f).URL)
	var out strings.Builder
	res, err := run(cfg, fastPull, &out)
	if err != nil || len(res.New) != 2 || !strings.Contains(out.String(), "Getting the latest data") {
		t.Fatalf("%+v %v\n%s", res, err, out.String())
	}
}

func TestWaitsForAPullThatIsAlreadyRunning(t *testing.T) {
	f := &fake{files: twoBundles(), busy: true, pullTo: "2026-10-14_030000"}
	cfg := testConfig(t, newFake(t, f).URL)
	res, err := run(cfg, fastPull, io.Discard)
	if err != nil || len(res.New) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestFailedPullStillDownloadsWhatIsThere(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000"}, files: twoBundles(), pullFail: "APEX unreachable"}
	cfg := testConfig(t, newFake(t, f).URL)
	var out strings.Builder
	res, err := run(cfg, fastPull, &out)
	if err != nil || len(res.New) != 1 || !strings.Contains(out.String(), "could not read the data: APEX unreachable") {
		t.Fatalf("%+v %v\n%s", res, err, out.String())
	}
}

func TestCutDownloadChangesNothingAndRetries(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000"}, files: twoBundles(), short: "2026-10-14_030000/contacts.csv"}
	cfg := testConfig(t, newFake(t, f).URL)
	if _, err := run(cfg, noPull, io.Discard); err == nil || !strings.Contains(err.Error(), "run this again") {
		t.Fatalf("want a cut-off error, got %v", err)
	}
	if entries, _ := os.ReadDir(cfg.Folder); len(entries) != 0 {
		t.Fatalf("a cut download must leave no files (not even products.csv), got %d", len(entries))
	}
	if f.checkpoint != "" || len(f.puts) != 0 {
		t.Fatalf("a cut download must not move the server's checkpoint: %q %v", f.checkpoint, f.puts)
	}
	f.short = ""
	if res, err := run(cfg, noPull, io.Discard); err != nil || len(res.New) != 1 {
		t.Fatalf("retry: %+v %v", res, err)
	}
}

func TestMergingTheSameBundleTwiceChangesNothing(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.csv")
	dst := filepath.Join(dir, "products.csv")
	os.WriteFile(src, []byte(twoBundles()["2026-10-14_060000"]["products.csv"]), 0o644)
	mergeCSV(dst, src)
	once := read(t, dst)
	if err := mergeCSV(dst, src); err != nil || read(t, dst) != once {
		t.Fatalf("not idempotent: %v\n%s", err, read(t, dst))
	}
}

// A deal spans several rows (one per product) sharing an ID, its Deal key on the first only; every bundle
// numbers its deals from 1. The merged file must keep each deal's rows together and give every deal its own ID.
func TestDealsMergeWholeAndAreRenumbered(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "deals.csv")
	src := filepath.Join(dir, "src.csv")
	head := "ID,Deal key,Contact,Product,Quantity\r\n"
	os.WriteFile(dst, []byte(head+
		"1,IVF-10,Nino,Perfume,1\r\n1,,,Gift bag,1\r\n"+
		"2,IVF-11,Ana,Lipstick,2\r\n"), 0o644)
	os.WriteFile(src, []byte(head+
		"1,IVF-11,Ana,Lipstick,2\r\n"+ // already there: same deal again
		"2,IVF-12,Eka,Cream,1\r\n2,,,Serum,1\r\n2,,,Gift bag,1\r\n"), 0o644)
	if err := mergeCSV(dst, src); err != nil {
		t.Fatal(err)
	}
	want := head +
		"1,IVF-10,Nino,Perfume,1\r\n1,,,Gift bag,1\r\n" +
		"2,IVF-11,Ana,Lipstick,2\r\n" +
		"3,IVF-12,Eka,Cream,1\r\n3,,,Serum,1\r\n3,,,Gift bag,1\r\n"
	if got := read(t, dst); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if n, _ := countRows(dst); n != 3 {
		t.Fatalf("deals: %d", n)
	}
}

func TestContactsMergeOnTheirContactKeyColumn(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "contacts.csv")
	src := filepath.Join(dir, "src.csv")
	head := "First name,Contact key,Comment\r\n"
	os.WriteFile(dst, []byte(head+"ილია,C-IVF-7091,Purchases: 1\r\nანა,C-IVF-8000,Purchases: 4\r\n"), 0o644)
	os.WriteFile(src, []byte(head+"ილია,C-IVF-7091,Purchases: 2\r\n"), 0o644)
	if err := mergeCSV(dst, src); err != nil {
		t.Fatal(err)
	}
	if got := read(t, dst); got != head+"ილია,C-IVF-7091,Purchases: 2\r\nანა,C-IVF-8000,Purchases: 4\r\n" {
		t.Fatalf("got\n%s", got)
	}
}

// The server added a column (Contact key, 2026-10-19): older rows move into the new layout with it empty, so a
// client whose download spans the change isn't stuck. The newer bundle re-sends those rows with the value anyway.
func TestAddedColumnsCarryOlderRowsOver(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "contacts.csv")
	src := filepath.Join(dir, "src.csv")
	os.WriteFile(dst, []byte("First name,Comment\r\nილია,Key: C-IVF-7091; Purchases: 1\r\nანა,Key: C-IVF-8000; Purchases: 4\r\n"), 0o644)
	os.WriteFile(src, []byte("First name,Contact key,Comment\r\nილია,C-IVF-7091,Key: C-IVF-7091; Purchases: 2\r\n"), 0o644)
	if err := mergeCSV(dst, src); err != nil {
		t.Fatal(err)
	}
	want := "First name,Contact key,Comment\r\n" +
		"ილია,C-IVF-7091,Key: C-IVF-7091; Purchases: 2\r\n" + // found by the Key: in its old Comment, replaced
		"ანა,,Key: C-IVF-8000; Purchases: 4\r\n" // not re-sent yet: kept, its key comes with its own update
	if got := read(t, dst); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestChangedColumnsStopWithoutMerging(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "products.csv")
	src := filepath.Join(dir, "src.csv")
	before := "External ID,Name,Price,Brand\r\nIVF-P1,Lipstick,10.00,Dior\r\n"
	os.WriteFile(dst, []byte(before), 0o644)
	os.WriteFile(src, []byte(productsHead+"IVF-P1,Lipstick,10.00\r\n"), 0o644) // Brand removed
	if err := mergeCSV(dst, src); err == nil || !strings.Contains(err.Error(), "older version") {
		t.Fatalf("want an older-version error, got %v", err)
	}
	if readRaw(t, dst) != before {
		t.Fatal("products.csv must be left as it was")
	}
}

func TestFatalErrorsStopEverything(t *testing.T) {
	f := &fake{ids: []string{"../../evil"}, files: map[string]map[string]string{}}
	srv := newFake(t, f)
	bad := testConfig(t, srv.URL)
	bad.Token = "nope"
	if _, err := run(bad, fastPull, io.Discard); err == nil || !strings.Contains(err.Error(), "did not accept the access key") {
		t.Fatalf("want token error, got %v", err)
	}
	if _, err := run(testConfig(t, "http://127.0.0.1:1"), fastPull, io.Discard); err == nil || !strings.Contains(err.Error(), "cannot connect") {
		t.Fatalf("want unreachable error, got %v", err)
	}
	if _, err := run(testConfig(t, srv.URL), noPull, io.Discard); err == nil || !strings.Contains(err.Error(), "unexpected data name") {
		t.Fatalf("want name error, got %v", err)
	}
}

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	var settings settingsError
	if _, err := loadConfig(dir); !errors.As(err, &settings) || read(t, filepath.Join(dir, ".env")) != string(envTemplate) {
		t.Fatalf("a missing .env is made from the template, to be filled in: %v", err)
	}
	if _, err := loadConfig(dir); !errors.As(err, &settings) || !strings.Contains(err.Error(), "LUTECIA_TOKEN") {
		t.Fatalf("the unfilled template asks for the token: %v", err)
	}
	os.WriteFile(filepath.Join(dir, ".env"), []byte("\ufeff# comment\r\nLUTECIA_SERVER=http://10.0.0.5:3000/\r\nLUTECIA_TOKEN=\"abc\"\r\n"), 0o600)
	cfg, err := loadConfig(dir)
	if err != nil || cfg.Server != "http://10.0.0.5:3000" || cfg.Token != "abc" ||
		cfg.Folder != filepath.Join(dir, folderName) || filepath.Dir(cfg.State) != dir {
		t.Fatalf("%+v %v", cfg, err)
	}
	t.Setenv("LUTECIA_SERVER", "https://sync.example.com")
	other, _ := loadConfig(dir)
	if other.State == cfg.State {
		t.Fatal("each server needs its own download list")
	}
	t.Setenv("LUTECIA_TOKEN", "")
	if _, err := loadConfig(dir); !errors.As(err, &settings) {
		t.Fatal("an empty token must be refused, with the settings opened")
	}
}

func TestPlainHTTPOnlyInsideAPrivateNetwork(t *testing.T) {
	for server, ok := range map[string]bool{
		"https://sync.lutecia.ge": true, "http://127.0.0.1:3000": true, "http://localhost:3000": true,
		"http://192.168.11.38:3000": true, "http://10.0.0.5": true,
		"http://sync.lutecia.ge": false, "http://85.114.1.2:3000": false, "ftp://x": false, "sync.lutecia.ge": false,
	} {
		if err := checkServer(server); (err == nil) != ok {
			t.Errorf("%s: %v", server, err)
		}
	}
}
