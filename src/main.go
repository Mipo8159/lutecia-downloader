// Lutecia downloader (Windows, macOS, Linux): double-click it, and it
//  1. asks the heartbeat server to pull the latest data from APEX now, and waits for it (with a ticking timer);
//  2. asks the server what this computer still needs (GET /downloads: the server keeps a checkpoint per client IP,
//     the last bundle that client finished): the first time everything, after that only what is new since then;
//  3. merges those bundles into ONE file per entity in a NEW dated folder, "Lutecia for Bitrix/<date time>" next to
//     it (the first one is "<date time> (everything)"), with Excel copies (and, in the first one, a small test set);
//  4. only then moves the server's checkpoint forward (PUT /downloads/checkpoint), says how many rows are ready and
//     opens the folder.
//
// The client imports each dated folder once, oldest first, then deletes it. The bundles of each download are also
// listed next to the program (one list per server): on start the newest one is sent to the server as this computer's
// checkpoint. So a checkpoint lost on the way, a changed office IP, or a copy that downloaded before checkpoints
// existed (its "production" folder is its baseline) never gets old data again.
//
// -fresh starts over: the server forgets this computer's downloads (DELETE /downloads/checkpoint, this IP only), the
// earlier folders and the list move into "Lutecia for Bitrix/Before fresh start <date time>", and the run is a first
// download again, with everything.
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
	"encoding/json"
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
	productionName = "production" // before checkpoints: one folder all downloads were merged into
	testName       = "test"
	excelName      = "excel"
	trackingName   = "tracking"  // in a dated folder: the shop's site tracking (tracking_*.csv), apart from the import files
	trackingPrefix = "tracking_" // the server's tracking files
	stampLayout    = "2006-01-02 15-04"     // a dated folder's name: no ':' (Windows), and name order is time order
	everything     = " (everything)"        // the first download's folder
	freshPrefix    = "Before fresh start "  // -fresh moves what this computer had into "<prefix><date time>"
	manifestName   = ".lutecia-bundles.txt" // in a dated folder: the bundles it holds
	lockName       = ".lutecia-running"     // in "Lutecia for Bitrix" while a download runs
	staleLock      = 2 * time.Hour          // longer than any run (the pull wait stops at 30 minutes)
)

//go:embed env.example
var envTemplate []byte

// settingsError: .env needs the client's attention. It is opened for them, with this message.
type settingsError struct{ msg string }

func (e settingsError) Error() string { return e.msg }

var (
	bundleID = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}_\d{6}$`)
	// The folders this program makes, one per download: "2026-11-16 14-05", "... (everything)", "... (2)".
	datedFolder = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}-\d{2}( \(everything\))?( \(\d+\))?$`)
	fileName    = regexp.MustCompile(`^[A-Za-z0-9_]+\.(csv|json|txt)$`)
	// Bitrix needs this order: deals and contacts refer to products and companies by name, deals to contacts.
	// online_*: the web shop's customers and orders; online_deals has exactly the columns of offline_deals.
	importOrder = []string{"products.csv", "companies.csv", "offline_contacts.csv", "online_contacts.csv",
		"offline_deals.csv", "online_deals.csv", "b2b_deals.csv"}
	nouns = map[string]string{"products.csv": "products", "companies.csv": "companies", "offline_contacts.csv": "contacts",
		"online_contacts.csv": "contacts", "offline_deals.csv": "deals", "online_deals.csv": "deals", "b2b_deals.csv": "deals",
		"tracking_views.csv": "rows", "tracking_contacts.csv": "contacts"}
	trackingOrder = []string{"tracking_views.csv", "tracking_contacts.csv"}
	// The server's name for a file -> the name the client gets. Others keep their name.
	localNames = map[string]string{"contacts.csv": "offline_contacts.csv", "deals.csv": "offline_deals.csv"}
)

type config struct {
	Server string // http(s)://host:port
	Token  string
	Folder string // "Lutecia for Bitrix": holds one dated folder per download
	State  string // the list of downloaded bundle ids
}

// pending is the server's answer to GET /downloads.
type pending struct {
	Client        string   `json:"client"`
	Checkpoint    *string  `json:"checkpoint"`
	CheckpointAt  *string  `json:"checkpointAt"`
	FirstDownload bool     `json:"firstDownload"`
	Bundles       []bundle `json:"bundles"`
	History       []struct {
		N          int     `json:"n"`
		At         string  `json:"at"`
		Folder     string  `json:"folder"`
		First      bool    `json:"first"`
		FromBundle *string `json:"fromBundle"`
		ToBundle   string  `json:"toBundle"`
		Bundles    int     `json:"bundles"`
	} `json:"history"`
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
	New           []string   // bundles downloaded this time, oldest first
	Folder        string     // the dated folder made this time ("" = nothing new)
	Files         []fileRows // what that folder holds, in import order
	FirstDownload bool       // the server had nothing for this computer: the folder holds everything
	Since         string     // the data this computer had before, "2026-11-03 16:56" (from the server's checkpoint)
	Waiting       []string   // every folder still there to import, oldest first ("production" from older copies first)
	TestReady     bool       // the test folder in it was made
	ExcelReady    bool       // the excel folder in it holds a copy of each file
	Tracking      []fileRows // what its tracking folder holds (none: no tracking folder)
}

func (c config) production() string { return filepath.Join(c.Folder, productionName) }

func main() {
	home := flag.String("home", "", "folder with .env and the output (default: this program's folder)")
	noPull := flag.Bool("no-pull", false, "only download what the server already has; don't ask it to pull from APEX first")
	history := flag.Bool("history", false, "only show this computer's downloads as the server records them; download nothing")
	fresh := flag.Bool("fresh", false, "start over: the server forgets this computer's downloads and this one holds everything\n"+
		"(the earlier folders move into \""+freshPrefix+"<date time>\")")
	flag.Parse()
	if *home == "" {
		*home = exeDir()
	}

	fmt.Print("Lutecia -> Bitrix24\n\n")
	cfg, err := loadConfig(*home)
	var res result
	if err == nil && *history {
		if err = showHistory(cfg, os.Stdout); err == nil {
			fmt.Print("\nPress Enter to close.")
			bufio.NewReader(os.Stdin).ReadString('\n')
			return
		}
	} else if err == nil {
		res, err = run(cfg, options{Pull: !*noPull, Fresh: *fresh, PollEvery: 3 * time.Second, PullTimeout: 30 * time.Minute}, os.Stdout)
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
	if err == nil && (res.Folder != "" || len(res.Waiting) > 0) {
		fmt.Print("\nPress Enter to open the folder.")
		bufio.NewReader(os.Stdin).ReadString('\n')
		if res.Folder != "" {
			openFolder(res.Folder)
		} else {
			openFolder(cfg.Folder)
		}
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
	var b strings.Builder
	if res.Folder == "" {
		b.WriteString("Everything is up to date: there is nothing new since the last download.")
		if len(res.Waiting) > 0 {
			fmt.Fprintf(&b, "\n\nThese folders in\n  %s\nare still there. If they are not imported into Bitrix24 yet, import them now, oldest first:\n", cfg.Folder)
			for _, w := range res.Waiting {
				fmt.Fprintf(&b, "   %s\n", w)
			}
		}
		return strings.TrimRight(b.String(), "\n")
	}
	fmt.Fprintf(&b, "DONE! The new data is ready in the folder\n  %s\n\n", res.Folder)
	if res.FirstDownload {
		b.WriteString("This is the first download on this computer, so it holds everything. If these deals are already in\n" +
			"Bitrix24, do not import offline_deals.csv or online_deals.csv again: they would be there twice.\n\n")
	} else if res.Since != "" {
		fmt.Fprintf(&b, "It holds only what is new or changed since the previous download (data up to %s).\n\n", res.Since)
	}
	if len(res.Files) == 0 {
		b.WriteString("Only new site tracking this time: nothing to import into Bitrix24 from the main folder.\n")
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
	if len(names) > 0 {
		fmt.Fprintf(&b, "\nImport into Bitrix24 in this order: %s.\n", strings.Join(names, ", then "))
	}
	b.WriteString("In the Bitrix24 import choose: encoding UTF-8, separator comma.\n" +
		"Do not open these files in Excel and never save them from Excel: that breaks Georgian letters and phone numbers.\n")
	b.WriteString("When everything is imported, delete this folder. The next download makes a new one with only what is new.")
	if older := slices.DeleteFunc(slices.Clone(res.Waiting), func(w string) bool { return w == filepath.Base(res.Folder) }); len(older) > 0 {
		b.WriteString("\n\nImport the older folders first if they are not imported yet (oldest first):\n")
		for _, w := range older {
			fmt.Fprintf(&b, "   %s\n", w)
		}
		b.WriteString("Delete each folder once it is imported.")
	}
	if res.ExcelReady {
		fmt.Fprintf(&b, "\n\nTo look at the data, double-click the files in\n  %s\n"+
			"Those are only for looking: always import the .csv files into Bitrix24.", filepath.Join(res.Folder, excelName))
	}
	if len(res.Tracking) > 0 {
		fmt.Fprintf(&b, "\n\nSite tracking (who looked at what on lutecia.ge) is in\n  %s\n", filepath.Join(res.Folder, trackingName))
		for _, f := range res.Tracking {
			fmt.Fprintf(&b, "   %-22s %s %s\n", f.Name, thousands(f.Rows), nouns[f.Name])
		}
		b.WriteString("tracking_views is a report: look at it in the excel folder there, do not import it.\n" +
			"tracking_contacts can be imported into Contacts (duplicate control by Contact key, replace), after online_contacts.")
	}
	if res.TestReady {
		fmt.Fprintf(&b, "\n\nTo try an import first, use the small files in\n  %s\n"+
			"The test deals have names starting with TEST: delete the test deals in Bitrix24 before importing the real files,\n"+
			"or those deals are there twice. When you finish testing, delete that folder too: it holds customer data.", filepath.Join(res.Folder, testName))
	}
	return b.String()
}

type options struct {
	Pull        bool          // ask the server for a fresh APEX pull first
	Fresh       bool          // start over first (-fresh): this download is a first one, with everything
	PollEvery   time.Duration // how often to check on that pull
	PullTimeout time.Duration // stop waiting (the pull goes on; its data comes with the next click)
}

// run optionally pulls fresh data, then puts every bundle this computer has not had yet into one new dated folder.
func run(cfg config, opts options, out io.Writer) (result, error) {
	// One window at a time: two at once would put the same data into two folders, imported twice.
	unlock, err := lock(cfg.Folder)
	if err != nil {
		return result{}, err
	}
	defer unlock()
	// First, before anything that can fail: customer data must not outlive the files it was copied from.
	if stuck := cleanAllExcel(cfg); len(stuck) > 0 {
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

	// After the pull (seconds, not minutes, before the server is asked what is new: another computer on this IP has
	// little time to move the checkpoint in between) and before anything reads what this computer has (otherwise its
	// old place would be sent to the server again).
	if opts.Fresh {
		if err := freshStart(client, cfg, out); err != nil {
			return result{}, err
		}
	}
	done, err := readState(cfg.State)
	if err != nil {
		return result{}, err
	}
	// A folder made but not yet on the list (the program stopped right after making it) still counts as downloaded.
	folderOf, err := readManifests(cfg.Folder, done)
	if err != nil {
		return result{}, err
	}
	// Tell the server what this computer already has (a checkpoint lost on the way, a new office IP, a copy from
	// before checkpoints). The server keeps the later of the two, so this never brings old data back.
	if latest := newest(done); latest != "" {
		if err := putCheckpoint(client, cfg, latest, folderOf[latest]); err != nil {
			var fatal fatalError
			if errors.As(err, &fatal) {
				return result{}, err
			}
			fmt.Fprintf(out, "Note: the server did not take this computer's last download (%v).\n", err)
		}
	}
	var p pending
	if err := getJSON(client, cfg, "/downloads", &p); err != nil {
		return result{}, err
	}
	var todo []bundle
	for _, b := range p.Bundles {
		if !bundleID.MatchString(b.ID) {
			return result{}, fmt.Errorf("the server sent an unexpected data name %q", b.ID)
		}
		if !done[b.ID] {
			todo = append(todo, b)
		}
	}
	res := result{FirstDownload: p.FirstDownload && len(done) == 0}
	if p.Checkpoint != nil && bundleID.MatchString(*p.Checkpoint) {
		res.Since = dataTime(*p.Checkpoint)
	}
	if len(todo) > 0 {
		fmt.Fprintln(out, "Downloading the new data ...")
		if res, err = download(client, cfg, todo, res, out); err != nil {
			return result{}, err
		}
	}
	if res.Waiting, err = datedFolders(cfg.Folder); err != nil {
		return res, err
	}
	// A copy from before checkpoints merged everything into "production": that comes before every dated folder.
	if files, _ := countFiles(cfg.production()); len(files) > 0 {
		res.Waiting = append([]string{productionName}, res.Waiting...)
	}
	return res, nil
}

// download builds the dated folder under a hidden name and shows it only when it is complete: a cut connection
// leaves no folder that looks importable. Then it records the bundles here and moves the server's checkpoint.
func download(client *http.Client, cfg config, todo []bundle, res result, out io.Writer) (result, error) {
	if err := os.MkdirAll(cfg.Folder, 0o755); err != nil {
		return res, err
	}
	removeLeftovers(cfg.Folder)
	name := time.Now().Format(stampLayout)
	if res.FirstDownload {
		name += everything
	}
	tmp, err := os.MkdirTemp(cfg.Folder, "."+name+".partial-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp) // gone once renamed; otherwise the half-made folder
	for _, b := range todo {
		if err := merge(client, cfg, b, tmp); err != nil {
			return res, err
		}
	}
	if res.Files, err = countFiles(tmp); err != nil {
		return res, err
	}
	// A file with no rows (nothing changed there) is not something to import.
	res.Files = slices.DeleteFunc(res.Files, func(f fileRows) bool {
		return f.Rows == 0 && os.Remove(filepath.Join(tmp, f.Name)) == nil
	})
	tracking := filepath.Join(tmp, trackingName)
	if res.Tracking, err = countFiles(tracking); err != nil {
		return res, err
	}
	res.Tracking = slices.DeleteFunc(res.Tracking, func(f fileRows) bool {
		return f.Rows == 0 && os.Remove(filepath.Join(tracking, f.Name)) == nil
	})
	if len(res.Tracking) == 0 {
		os.Remove(tracking) // only when empty
	}
	if len(res.Files) == 0 && len(res.Tracking) == 0 {
		// Nothing to import at all: no folder, and the checkpoint stays, so these bundles go into the next real
		// folder and the server's map has no gap.
		fmt.Fprintln(out, "The server's new data holds no changes.")
		return res, nil
	}
	if res.ExcelReady, err = makeExcel(tmp, filepath.Join(tmp, excelName), res.Files); err != nil {
		res.ExcelReady = false
		fmt.Fprintf(out, "Not all Excel copies could be made: %v. The import files are fine.\n", err)
	}
	// The tracking files' own copies, for looking (tracking_views is a report, never imported).
	if len(res.Tracking) > 0 {
		if _, err := makeExcel(tracking, filepath.Join(tracking, excelName), res.Tracking); err != nil {
			fmt.Fprintf(out, "Not all Excel copies of the tracking files could be made: %v.\n", err)
		}
	}
	if res.FirstDownload {
		if res.TestReady, err = writeTestSlice(tmp, filepath.Join(tmp, testName)); err != nil {
			res.TestReady = false
			fmt.Fprintf(out, "The small test files could not be made (%v). The import files are fine.\n", err)
		}
	}
	ids := make([]string, len(todo))
	for i, b := range todo {
		ids[i] = b.ID
	}
	// The folder says what it holds, so the rename below is the one moment it counts as downloaded.
	if err := os.WriteFile(filepath.Join(tmp, manifestName), []byte(strings.Join(ids, "\r\n")+"\r\n"), 0o644); err != nil {
		return res, err
	}
	dst := filepath.Join(cfg.Folder, name)
	for i := 2; exists(dst); i++ { // two downloads in one minute
		dst = filepath.Join(cfg.Folder, fmt.Sprintf("%s (%d)", name, i))
	}
	if err := rename(tmp, dst); err != nil {
		return res, fmt.Errorf("the new folder could not be put in place (%v). Nothing was lost: just run this again", err)
	}
	res.Folder, res.New = dst, ids
	if err := appendState(cfg.State, ids...); err != nil {
		return res, err // the folder's own list keeps it from coming again
	}
	// The folder is safe here and in the list above; if this fails, the next start sends it again.
	if err := putCheckpoint(client, cfg, slices.Max(ids), filepath.Base(dst)); err != nil {
		fmt.Fprintf(out, "Note: the server could not record this download yet (%v); the next run tells it again.\n", err)
	}
	return res, nil
}

// rename retries for a few seconds: on Windows an antivirus or the search indexer may still hold a just-written file.
func rename(from, to string) error {
	err := os.Rename(from, to)
	for i := 0; err != nil && i < 5; i++ {
		time.Sleep(time.Second)
		err = os.Rename(from, to)
	}
	return err
}

// freshStart (-fresh): the server forgets this computer's downloads, and everything this computer remembers of them
// (the dated folders, an old "production" folder, the list of downloaded bundles) moves into one "Before fresh
// start <date time>" folder, which nothing here reads. So this run is a first download with everything.
// The server goes first. If a move fails, what was already moved goes back: this computer is as it was, and the next
// plain run tells the server the old place again, so nothing is hidden or lost.
func freshStart(client *http.Client, cfg config, out io.Writer) error {
	res, err := call(client, cfg, http.MethodDelete, "/downloads/checkpoint", nil)
	if err != nil {
		return err
	}
	var gone struct {
		Downloads int `json:"downloads"`
	}
	err = json.NewDecoder(res.Body).Decode(&gone)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || err != nil {
		return fmt.Errorf("the server did not start over (%s). Nothing was changed", res.Status)
	}
	fmt.Fprintf(out, "Fresh start: the server forgot this computer's %d earlier downloads.\n", gone.Downloads)

	names, err := datedFolders(cfg.Folder)
	if err != nil {
		return err
	}
	var move []string
	for _, n := range append(names, productionName, excelName, testName) {
		if p := filepath.Join(cfg.Folder, n); exists(p) {
			move = append(move, p)
		}
	}
	if exists(cfg.State) {
		move = append(move, cfg.State)
	}
	if len(move) == 0 {
		return nil
	}
	dir := filepath.Join(cfg.Folder, freshPrefix+time.Now().Format(stampLayout))
	for i, base := 2, dir; exists(dir); i++ {
		dir = fmt.Sprintf("%s (%d)", base, i)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for i, p := range move {
		if err := moveAside(p, filepath.Join(dir, filepath.Base(p))); err != nil {
			for j := i - 1; j >= 0; j-- {
				if berr := rename(filepath.Join(dir, filepath.Base(move[j])), move[j]); berr != nil {
					return fmt.Errorf("could not move %s aside (%v), nor %s back (%v). Move everything in\n  %s\nback by hand, "+
						"then run with -fresh again", filepath.Base(p), err, filepath.Base(move[j]), berr, dir)
				}
			}
			os.Remove(dir)
			return fmt.Errorf("could not move %s aside (%v); nothing was moved. Close it if it is open in Excel, "+
				"then run with -fresh again", filepath.Base(p), err)
		}
	}
	fmt.Fprintf(out, "What this computer had is now in\n  %s\nDelete that folder once you no longer need it.\n", dir)
	return nil
}

// moveAside is rename; a test swaps it to see that a failed move puts everything back.
var moveAside = rename

// makeExcel is syncExcel; a test swaps it to see that a failed copy harms nothing.
var makeExcel = syncExcel

// merge downloads one bundle's CSV files into a temporary folder, checks each is complete, and only then merges
// them into the files in dst (a newer row replaces the row with the same key).
func merge(client *http.Client, cfg config, b bundle, dst string) error {
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
	for _, name := range got {
		local := name
		if l, ok := localNames[name]; ok {
			local = l
		}
		into := dst
		if strings.HasPrefix(name, trackingPrefix) {
			into = filepath.Join(dst, trackingName)
			if err := os.MkdirAll(into, 0o755); err != nil {
				return err
			}
		}
		if err := mergeCSV(filepath.Join(into, local), filepath.Join(tmp, name)); err != nil {
			return err
		}
	}
	return nil
}

// datedFolders lists the download folders still in the folder, oldest first (their names sort by time).
func datedFolders(folder string) ([]string, error) {
	entries, err := os.ReadDir(folder)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && datedFolder.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	slices.SortFunc(names, func(a, b string) int { return strings.Compare(folderOrder(a), folderOrder(b)) })
	return names, nil
}

// folderOrder sorts dated folders by time; within one minute "(everything)" first, then the plain name, then (2), (3) ...
func folderOrder(name string) string {
	stamp, rest := name[:len(stampLayout)], name[len(stampLayout):]
	n := 1
	if strings.HasPrefix(rest, everything) {
		n, rest = 0, strings.TrimPrefix(rest, everything)
	}
	if rest != "" {
		fmt.Sscanf(rest, " (%d)", &n)
	}
	return fmt.Sprintf("%s %06d", stamp, n)
}

// cleanAllExcel: in every dated folder (and the old production folder), remove the Excel copy of a deleted file.
func cleanAllExcel(cfg config) (stuck []string) {
	stuck = cleanExcel(cfg.production(), filepath.Join(cfg.Folder, excelName))
	names, _ := datedFolders(cfg.Folder)
	for _, n := range names {
		stuck = append(stuck, cleanExcel(filepath.Join(cfg.Folder, n), filepath.Join(cfg.Folder, n, excelName))...)
	}
	return stuck
}

// removeLeftovers deletes half-made folders of a download that was cut off over an hour ago (a younger one may
// belong to another Lutecia window).
func removeLeftovers(folder string) {
	paths, _ := filepath.Glob(filepath.Join(folder, ".* *.partial-*"))
	for _, p := range paths {
		if fi, err := os.Lstat(p); err == nil && fi.IsDir() && time.Since(fi.ModTime()) > time.Hour {
			os.RemoveAll(p)
		}
	}
}

// readManifests adds the bundles listed in each dated folder to done, and says which folder holds each.
func readManifests(folder string, done map[string]bool) (folderOf map[string]string, err error) {
	folderOf = map[string]string{}
	names, err := datedFolders(folder)
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(folder, n, manifestName))
		if err != nil {
			continue // older folder or none
		}
		for _, line := range strings.Split(string(data), "\n") {
			if id := strings.TrimSpace(line); bundleID.MatchString(id) {
				done[id] = true
				folderOf[id] = n
			}
		}
	}
	return folderOf, err
}

// showHistory prints the server's map of this computer's downloads: the first, then each next one.
func showHistory(cfg config, out io.Writer) error {
	client := &http.Client{Timeout: time.Minute}
	var p pending
	if err := getJSON(client, cfg, "/downloads", &p); err != nil {
		return err
	}
	if len(p.History) == 0 {
		fmt.Fprintf(out, "The server has no downloads from this address (%s) yet: the next one is the first, with everything.\n", p.Client)
		return nil
	}
	fmt.Fprintf(out, "Downloads from this address (%s), as the server records them:\n\n", p.Client)
	for _, h := range p.History {
		what := "first download: everything"
		if h.FromBundle != nil {
			what = "new since " + dataTime(*h.FromBundle)
		}
		updates := "updates"
		if h.Bundles == 1 {
			updates = "update"
		}
		fmt.Fprintf(out, "  %d. %-30s %s, up to %s (%d %s)\n", h.N, h.Folder, what, dataTime(h.ToBundle), h.Bundles, updates)
	}
	return nil
}

// dataTime: bundle id 2026-11-03_165626 -> "2026-11-03 16:56".
func dataTime(id string) string {
	if !bundleID.MatchString(id) {
		return id
	}
	return id[:10] + " " + id[11:13] + ":" + id[13:15]
}

// lock makes the lock file, or says another window is still at work. A lock older than staleLock is left over from
// a computer that was switched off mid-run, and is taken over.
func lock(folder string) (unlock func(), err error) {
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(folder, lockName)
	for try := 0; try < 2; try++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d %s\r\n", os.Getpid(), time.Now().Format(time.RFC3339))
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		fi, serr := os.Stat(path)
		if serr != nil || time.Since(fi.ModTime()) < staleLock {
			break
		}
		os.Remove(path)
	}
	return nil, errors.New("Lutecia is already running in another window. Wait for it to finish, then close this one")
}

// newest is the latest bundle id in the list (ids sort by time).
func newest(done map[string]bool) string {
	latest := ""
	for id := range done {
		if bundleID.MatchString(id) && id > latest {
			latest = id
		}
	}
	return latest
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
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
		if i := slices.Index(trackingOrder, name); i >= 0 {
			return len(importOrder) + i
		}
		return len(importOrder) + len(trackingOrder)
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
	sum := sha256.Sum256([]byte(cfg.Server)) // one list per server: a test server's bundles never hide production's
	// Next to the program, not in the data folder: deleting imported folders never brings old data back, and on start
	// its newest id is sent to the server (the server's checkpoint per office IP covers a fresh unzip).
	cfg.State = filepath.Join(dir, ".lutecia-downloaded-"+hex.EncodeToString(sum[:6])+".txt")
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

func appendState(path string, ids ...string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.WriteString(strings.Join(ids, "\r\n") + "\r\n"); err == nil {
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
