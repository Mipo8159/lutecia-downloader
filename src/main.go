// Lutecia downloader (Windows, macOS, Linux): double-click it, and it
//  1. asks the heartbeat server to pull the latest data from APEX now, and waits for it (with a ticking timer);
//  2. downloads every bundle it has not downloaded before, oldest first, and merges each one into ONE file per
//     entity in "Lutecia for Bitrix/production" next to it, and copies a small linked part into ".../test";
//  3. says how many rows are ready and opens that folder.
//
// Bundles are deltas. The files hold everything downloaded and not imported yet: the client imports them, then
// deletes them, and the next click starts new ones. A newer row replaces the row with the same key, so forgetting
// to delete only re-imports some rows, never loses one. What was downloaded is remembered in the user's settings
// folder (one list per server), so deleting the files or the folder never brings old data back.
//
// Settings: LUTECIA_SERVER and LUTECIA_TOKEN in .env next to the program (environment variables win). The client
// never has to find that (hidden) file: when it is missing, incomplete or its token is refused, the program makes
// it from env.example where needed and opens it in Notepad / TextEdit.
package main

import (
	"bufio"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

const (
	folderName     = "Lutecia for Bitrix"
	productionName = "production"
	testName       = "test"
	excelName      = "excel"
)

//go:embed env.example
var envTemplate []byte

// settingsError: .env needs the client's attention. It is opened for them, with this message.
type settingsError struct{ msg string }

func (e settingsError) Error() string { return e.msg }

var (
	bundleID = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}_\d{6}$`)
	fileName = regexp.MustCompile(`^[A-Za-z0-9_]+\.(csv|json|txt)$`)
	// Bitrix needs this order: deals and contacts refer to products and companies by name.
	importOrder = []string{"products.csv", "companies.csv", "offline_contact.csv", "offline_deals.csv", "b2b_deals.csv"}
	nouns       = map[string]string{"products.csv": "products", "companies.csv": "companies", "offline_contact.csv": "contacts",
		"offline_deals.csv": "deals", "b2b_deals.csv": "deals"}
	// The server's name for a file -> the name the client gets. Others keep their name.
	localNames = map[string]string{"contacts.csv": "offline_contact.csv", "deals.csv": "offline_deals.csv"}
)

type config struct {
	Server string // http(s)://host:port
	Token  string
	Folder string // "Lutecia for Bitrix": holds the production and test folders
	State  string // the list of downloaded bundle ids
}

type bundle struct {
	ID    string `json:"id"`
	Files []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"files"`
}

// fileRows: one import file in the folder and how many rows it holds.
type fileRows struct {
	Name string
	Rows int
}

type result struct {
	New        []string   // bundles downloaded this time, oldest first
	Files      []fileRows // what the folder holds now, in import order
	TestReady  bool       // the test folder was made from these files just now
	ExcelReady bool       // the excel folder holds a copy of each of these files
}

func (c config) production() string { return filepath.Join(c.Folder, productionName) }
func (c config) test() string       { return filepath.Join(c.Folder, testName) }
func (c config) excel() string      { return filepath.Join(c.Folder, excelName) }

func main() {
	home := flag.String("home", "", "folder with .env and the output (default: this program's folder)")
	noPull := flag.Bool("no-pull", false, "only download what the server already has; don't ask it to pull from APEX first")
	flag.Parse()
	if *home == "" {
		*home = exeDir()
	}

	fmt.Print("Lutecia -> Bitrix24\n\n")
	cfg, err := loadConfig(*home)
	var res result
	if err == nil {
		res, err = run(cfg, options{Pull: !*noPull, PollEvery: 3 * time.Second, PullTimeout: 30 * time.Minute}, os.Stdout)
	}
	var settings settingsError
	if errors.As(err, &settings) {
		fmt.Printf("%s\n\nThe settings file is now open: fill it in, save it, close it, then double-click Lutecia again.\n", settings.msg)
		openEditor(filepath.Join(*home, ".env"))
	} else if err != nil {
		fmt.Printf("\nSomething went wrong:\n  %v\n\nNothing was lost. Try again later; if it keeps happening, send a photo of this window to Sweeft.\n", err)
	} else {
		fmt.Print("\n" + summary(cfg, res) + "\n")
	}

	// A double-clicked window closes with the program: wait, so the message can be read.
	if err == nil && len(res.Files) > 0 {
		fmt.Print("\nPress Enter to open the folder.")
		bufio.NewReader(os.Stdin).ReadString('\n')
		openFolder(cfg.Folder)
	} else {
		fmt.Print("\nPress Enter to close.")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	if err != nil {
		os.Exit(1)
	}
}

// summary is the closing message, for people who don't know what a bundle or CSV is.
func summary(cfg config, res result) string {
	if len(res.Files) == 0 {
		return "Everything is up to date: there is nothing new to import into Bitrix24."
	}
	var b strings.Builder
	if len(res.New) > 0 {
		fmt.Fprintf(&b, "DONE! The new data is ready in the folder\n  %s\n\n", cfg.production())
	} else {
		fmt.Fprintf(&b, "No new data since last time. The files from last time are still in the folder\n  %s\n"+
			"If they are not imported into Bitrix24 yet, import them now.\n\n", cfg.production())
	}
	names := make([]string, len(res.Files))
	for i, f := range res.Files {
		noun := nouns[f.Name]
		if noun == "" {
			noun = "rows"
		}
		fmt.Fprintf(&b, "   %-20s %s %s\n", f.Name, thousands(f.Rows), noun)
		names[i] = f.Name
	}
	fmt.Fprintf(&b, "\nImport into Bitrix24 in this order: %s.\n", strings.Join(names, ", then "))
	b.WriteString("When everything is imported, delete these files. The next time, new files are made.")
	if res.ExcelReady {
		fmt.Fprintf(&b, "\n\nTo look at the data, double-click the files in\n  %s\n"+
			"Those are only for looking: always import the files from production into Bitrix24.\n"+
			"Delete them together with the production files: they hold customer data.", cfg.excel())
	}
	if res.TestReady {
		fmt.Fprintf(&b, "\n\nTo try an import first, use the small files in\n  %s\n"+
			"The test deals have names starting with TEST: delete the test deals in Bitrix24 before importing production,\n"+
			"or those deals are there twice. When you finish testing, delete that folder too: it holds customer data.", cfg.test())
	}
	return b.String()
}

type options struct {
	Pull        bool          // ask the server for a fresh APEX pull first
	PollEvery   time.Duration // how often to check on that pull
	PullTimeout time.Duration // stop waiting (the pull goes on; its data comes with the next click)
}

// run optionally pulls fresh data, then merges every bundle not downloaded before into the folder's files.
func run(cfg config, opts options, out io.Writer) (result, error) {
	// First, before anything that can fail: customer data must not outlive the production files.
	if stuck := cleanExcel(cfg.production(), cfg.excel()); len(stuck) > 0 {
		fmt.Fprintf(out, "Please close %s in Excel and delete it: the data is no longer needed there.\n", strings.Join(stuck, ", "))
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		ResponseHeaderTimeout: 60 * time.Second,
	}}
	if opts.Pull {
		stop := ticker(out, "Getting the latest data")
		err := pull(client, cfg, opts)
		stop(err == nil)
		if err != nil {
			var fatal fatalError
			if errors.As(err, &fatal) {
				return result{}, err
			}
			// APEX down, pull still running: what the server already has is still right to import.
			fmt.Fprintf(out, "Could not get the very latest data just now (%v).\nYou get everything the server already had.\n", err)
		}
	}

	var list []bundle
	if err := getJSON(client, cfg, "/bundles", &list); err != nil {
		return result{}, err
	}
	done, err := readState(cfg.State)
	if err != nil {
		return result{}, err
	}
	var res result
	for _, b := range list {
		if done[b.ID] {
			continue
		}
		if !bundleID.MatchString(b.ID) {
			return result{}, fmt.Errorf("the server sent an unexpected data name %q", b.ID)
		}
		if len(res.New) == 0 {
			fmt.Fprintln(out, "Downloading the new data ...")
		}
		if err := merge(client, cfg, b); err != nil {
			return result{}, err
		}
		if err := appendState(cfg.State, b.ID); err != nil {
			return result{}, err
		}
		res.New = append(res.New, b.ID)
	}
	res.Files, err = countFiles(cfg.production())
	if err != nil {
		return res, err
	}
	if stuck := cleanExcel(cfg.production(), cfg.excel()); len(stuck) > 0 && len(res.New) > 0 {
		fmt.Fprintf(out, "Please close %s in Excel and delete it.\n", strings.Join(stuck, ", "))
	}
	if res.ExcelReady, err = syncExcel(cfg.production(), cfg.excel(), res.Files); err != nil {
		res.ExcelReady = false
		fmt.Fprintf(out, "Not all Excel copies could be made: %v. The production files are fine.\n", err)
	}
	if len(res.Files) > 0 {
		res.TestReady, err = writeTestSlice(cfg.production(), cfg.test())
		if err != nil {
			res.TestReady = false
			fmt.Fprintf(out, "The small test files could not be made (%v). The production files are fine.\n", err)
		}
	}
	return res, nil
}

// merge downloads one bundle's CSV files into a temporary folder, checks each is complete, and only then merges
// them into the import files. A cut connection changes nothing; merging the same bundle twice changes nothing
// either, so a run stopped between merging and appendState is simply repeated by the next click.
func merge(client *http.Client, cfg config, b bundle) error {
	tmp, err := os.MkdirTemp("", "lutecia-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	var got []string
	for _, f := range b.Files {
		if !fileName.MatchString(f.Name) {
			return fmt.Errorf("the server sent an unexpected file name %q", f.Name)
		}
		if !strings.HasSuffix(f.Name, ".csv") {
			continue // IMPORT_ORDER.txt, summary.json: the closing message says it
		}
		if err := fetchFile(client, cfg, b.ID, f.Name, f.Size, filepath.Join(tmp, f.Name)); err != nil {
			return fmt.Errorf("the download was cut off (%w). Just run this again", err)
		}
		got = append(got, f.Name)
	}
	if err := os.MkdirAll(cfg.production(), 0o755); err != nil {
		return err
	}
	for _, name := range got {
		local := name
		if l, ok := localNames[name]; ok {
			local = l
		}
		if err := mergeCSV(filepath.Join(cfg.production(), local), filepath.Join(tmp, name)); err != nil {
			return err
		}
	}
	return nil
}

// countFiles lists the import files in the folder, in import order, with their row counts.
func countFiles(folder string) ([]fileRows, error) {
	entries, err := os.ReadDir(folder)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []fileRows
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".csv") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		n, err := countRows(filepath.Join(folder, e.Name()))
		if err != nil {
			return nil, err
		}
		files = append(files, fileRows{e.Name(), n})
	}
	rank := func(name string) int {
		if i := slices.Index(importOrder, name); i >= 0 {
			return i
		}
		return len(importOrder)
	}
	slices.SortStableFunc(files, func(a, b fileRows) int { return rank(a.Name) - rank(b.Name) })
	return files, nil
}

// ticker shows "label  | 0:07" on one line, redrawn 5 times a second, so a person sees it is still working.
// Not a terminal (tests): the label is printed once. stop(ok) ends the line.
func ticker(out io.Writer, label string) (stop func(ok bool)) {
	fi, err := os.Stdout.Stat()
	if out != io.Writer(os.Stdout) || err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		fmt.Fprintf(out, "%s ...\n", label)
		return func(bool) {}
	}
	start := time.Now()
	quit, finished := make(chan bool), make(chan struct{})
	go func() {
		defer close(finished)
		for i := 0; ; i++ {
			s := int(time.Since(start).Seconds())
			select {
			case ok := <-quit:
				end := "done"
				if !ok {
					end = ""
				}
				fmt.Fprintf(out, "\r%s  %d:%02d  %s   \n", label, s/60, s%60, end)
				return
			case <-time.After(200 * time.Millisecond):
				fmt.Fprintf(out, "\r%s  %c %d:%02d ", label, `|/-\`[i%4], s/60, s%60)
			}
		}
	}()
	return func(ok bool) {
		quit <- ok
		<-finished
	}
}

// loadConfig reads LUTECIA_SERVER and LUTECIA_TOKEN from <dir>/.env; environment variables of the same name win.
// A missing .env is made from env.example (first double-click), so the client only fills in what it asks for.
func loadConfig(dir string) (config, error) {
	kv := map[string]string{}
	path := filepath.Join(dir, ".env")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && (os.Getenv("LUTECIA_SERVER") == "" || os.Getenv("LUTECIA_TOKEN") == "") {
		if err := os.WriteFile(path, envTemplate, 0o600); err != nil {
			return config{}, fmt.Errorf("could not make the settings file %s: %w", path, err)
		}
		return config{}, settingsError{"First start: Lutecia needs its settings (the server address and your access key)."}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return config{}, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff")) // Notepad may save a BOM
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		kv[strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(k), "export ")))] = v
	}
	for _, k := range []string{"LUTECIA_SERVER", "LUTECIA_TOKEN"} {
		if v, ok := os.LookupEnv(k); ok {
			kv[k] = v
		}
	}
	cfg := config{Server: strings.TrimRight(kv["LUTECIA_SERVER"], "/"), Token: kv["LUTECIA_TOKEN"], Folder: filepath.Join(dir, folderName)}
	if cfg.Token == "" {
		return cfg, settingsError{"The settings are not complete: LUTECIA_TOKEN (your access key) is empty."}
	}
	if err := checkServer(cfg.Server); err != nil {
		return cfg, settingsError{err.Error()}
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = dir
	}
	sum := sha256.Sum256([]byte(cfg.Server)) // one list per server: a test server's bundles never hide production's
	cfg.State = filepath.Join(base, "lutecia-downloader", "downloaded-"+hex.EncodeToString(sum[:6])+".txt")
	return cfg, nil
}

// checkServer: the token travels with every request, so plain http is allowed only inside a private network.
func checkServer(server string) error {
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf(".env: LUTECIA_SERVER must look like https://host or http://10.0.0.5:3000, not %q", server)
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); host == "localhost" || (ip != nil && (ip.IsLoopback() || ip.IsPrivate())) {
		return nil
	}
	return fmt.Errorf(".env: LUTECIA_SERVER %s must start with https:// (plain http only for a local or private address)", server)
}

func readState(path string) (map[string]bool, error) {
	done := map[string]bool{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return done, nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			done[id] = true
		}
	}
	return done, err
}

func appendState(path, id string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.WriteString(id + "\r\n"); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Dir(exe)
}

// openEditor opens the settings in a plain text editor (TextEdit shows hidden files it is handed). Best effort.
func openEditor(path string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("notepad", path).Start()
	case "darwin":
		exec.Command("open", "-e", path).Start()
	default:
		exec.Command("xdg-open", path).Start()
	}
}

// openFolder shows the result in the file manager. Best effort: a headless Linux box simply has none.
func openFolder(path string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("explorer", path).Start()
	case "darwin":
		exec.Command("open", path).Start()
	default:
		exec.Command("xdg-open", path).Start()
	}
}

func thousands(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
