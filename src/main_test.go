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

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
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
	if got := read(t, filepath.Join(cfg.production(), "products.csv")); got != productsHead+
		"IVF-P1,Lipstick,12.00\r\nIVF-P2,\"Perfume, 50ml\",99.00\r\nVST-P3,\"Gown\r\nlong\",5.00\r\n" { // CRLF file: CRLF inside quotes too
		t.Fatalf("products: newer row must replace in place, new rows appended:\n%s", got)
	}
	if got := read(t, filepath.Join(cfg.production(), "offline_contact.csv")); got != contactsHead+"ილია,555,Key: C-IVF-7091; Purchases: 2\r\n" {
		t.Fatalf("contacts: same Key in Comment = same contact:\n%s", got)
	}
	entries, _ := os.ReadDir(cfg.production())
	if len(entries) != 2 {
		t.Fatalf("the folder must hold only the two import files, got %d entries", len(entries))
	}
	if !reflect.DeepEqual(res.Files, []fileRows{{"products.csv", 3}, {"offline_contact.csv", 1}}) {
		t.Fatalf("import order and counts: %+v", res.Files)
	}
}

func TestNotImportedYetMeansAddedToTheSameFiles(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000"}, files: twoBundles()}
	cfg := testConfig(t, newFake(t, f).URL)
	if _, err := run(cfg, noPull, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.ids = append(f.ids, "2026-10-14_060000")
	res, err := run(cfg, noPull, io.Discard)
	if err != nil || !reflect.DeepEqual(res.New, []string{"2026-10-14_060000"}) {
		t.Fatalf("%+v %v", res, err)
	}
	if !reflect.DeepEqual(res.Files, []fileRows{{"products.csv", 3}, {"offline_contact.csv", 1}}) {
		t.Fatalf("second click without importing must keep the first click's rows: %+v", res.Files)
	}
}

func TestImportedAndDeletedMeansNewFilesWithOnlyNewRows(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000"}, files: twoBundles()}
	cfg := testConfig(t, newFake(t, f).URL)
	if _, err := run(cfg, noPull, io.Discard); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(cfg.production(), "products.csv")) // the client imported both, then deleted them
	os.Remove(filepath.Join(cfg.production(), "offline_contact.csv"))

	res, err := run(cfg, noPull, io.Discard)
	if err != nil || len(res.New) != 0 || len(res.Files) != 0 {
		t.Fatalf("nothing new: no files must come back: %+v %v", res, err)
	}
	if msg := summary(cfg, res); !strings.Contains(msg, "nothing new") {
		t.Fatal(msg)
	}

	f.ids = append(f.ids, "2026-10-14_060000")
	res, err = run(cfg, noPull, io.Discard)
	if err != nil || !reflect.DeepEqual(res.Files, []fileRows{{"products.csv", 2}, {"offline_contact.csv", 1}}) {
		t.Fatalf("%+v %v", res, err)
	}
	if msg := summary(cfg, res); !strings.Contains(msg, "products.csv, then offline_contact.csv") || !strings.Contains(msg, "2 products") {
		t.Fatal(msg)
	}
}

func TestNothingNewButFilesStillThere(t *testing.T) {
	f := &fake{ids: []string{"2026-10-14_030000"}, files: twoBundles()}
	cfg := testConfig(t, newFake(t, f).URL)
	run(cfg, noPull, io.Discard)
	res, err := run(cfg, noPull, io.Discard)
	if err != nil || len(res.New) != 0 || len(res.Files) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if msg := summary(cfg, res); !strings.Contains(msg, "No new data since last time") {
		t.Fatal(msg)
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
	if err := mergeCSV(dst, src); err == nil || !strings.Contains(err.Error(), "Import it into Bitrix24 and delete it") {
		t.Fatalf("want a columns-changed error, got %v", err)
	}
	if read(t, dst) != before {
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
		cfg.Folder != filepath.Join(dir, folderName) || !strings.Contains(cfg.State, "lutecia-downloader") {
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
