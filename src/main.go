// Lutecia downloader (Windows, macOS, Linux): double-click it, and it
//  1. asks the sync server to pull the latest data from APEX now, and waits for that pull (--no-pull skips it);
//  2. downloads every CSV bundle made since the last click into a folder next to it
//     ("Bitrix import/<bundle>/*.csv"), oldest first.
//
// Bundles are deltas: each one must be imported into Bitrix exactly once, in order. So what was downloaded is
// remembered in "Bitrix import\downloaded.txt", not by which folders exist: deleting an imported folder never
// brings it back (that would import its deals twice).
package main

import (
	"bufio"
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
	"strings"
	"time"
)

const (
	configName = ".env" // next to the click-files; .env.example shows the keys
	stateName  = "downloaded.txt"
)

var (
	bundleID = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}_\d{6}$`)
	fileName = regexp.MustCompile(`^[A-Za-z0-9_]+\.(csv|json|txt)$`)
)

type config struct {
	Server string // http(s)://host:port
	Token  string
	Folder string // absolute
}

type bundle struct {
	ID       string `json:"id"`
	Finished string `json:"finished"`
	Status   string `json:"status"`
	Files    []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"files"`
}

func main() {
	noPull := flag.Bool("no-pull", false, "only download what the server already has; don't ask it to pull from APEX first")
	home := flag.String("home", "", "kit folder: holds "+configName+" and the downloads (default: this program's folder)")
	flag.Parse()
	dir := *home
	if dir == "" {
		dir = exeDir()
	}
	err := func() error {
		cfg, err := loadConfig(dir)
		if err != nil {
			return err
		}
		got, err := run(cfg, options{Pull: !*noPull, PollEvery: 3 * time.Second, PullTimeout: 30 * time.Minute}, os.Stdout)
		if len(got) > 0 {
			openFolder(cfg.Folder)
		}
		return err
	}()
	if err != nil {
		fmt.Printf("\nERROR: %v\n", err)
	}
	if runtime.GOOS == "windows" { // double-clicked: keep the window open so the message can be read
		fmt.Print("\nPress Enter to close.")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	if err != nil {
		os.Exit(1)
	}
}

type options struct {
	Pull        bool          // ask the server for a fresh APEX pull first
	PollEvery   time.Duration // how often to check on that pull
	PullTimeout time.Duration // stop waiting (the pull goes on; its bundle comes with the next click)
}

// run optionally pulls fresh data, then downloads every bundle not yet in downloaded.txt.
// Returns the new bundle ids, oldest first.
func run(cfg config, opts options, out io.Writer) ([]string, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		ResponseHeaderTimeout: 60 * time.Second,
	}}
	fmt.Fprintf(out, "Server: %s\nFolder: %s\n\n", cfg.Server, cfg.Folder)
	if opts.Pull {
		if err := pull(client, cfg, opts, out); err != nil {
			var fatal fatalError
			if errors.As(err, &fatal) {
				return nil, err
			}
			// APEX down, VPN dropped, pull still running: what the server already has is still correct to import.
			fmt.Fprintf(out, "WARNING: no fresh data this time: %v\nDownloading what the server already has.\n\n", err)
		}
	}

	var list []bundle
	if err := getJSON(client, cfg, "/bundles", &list); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.Folder, 0o755); err != nil {
		return nil, err
	}
	done, err := readState(filepath.Join(cfg.Folder, stateName))
	if err != nil {
		return nil, err
	}

	var got []string
	for _, b := range list {
		if done[b.ID] {
			continue
		}
		if !bundleID.MatchString(b.ID) {
			return got, fmt.Errorf("server sent an unexpected bundle name %q", b.ID)
		}
		fmt.Fprintf(out, "Downloading %s ...\n", b.ID)
		if err := fetchBundle(client, cfg, b, out); err != nil {
			return got, fmt.Errorf("bundle %s: %w (nothing from it was kept; just run this again)", b.ID, err)
		}
		if err := appendState(filepath.Join(cfg.Folder, stateName), b.ID); err != nil {
			return got, err
		}
		got = append(got, b.ID)
	}

	if len(got) == 0 {
		fmt.Fprintln(out, "Nothing new. Everything is already downloaded.")
		return nil, nil
	}
	fmt.Fprintf(out, "\nDone: %d new folder(s). Import them into Bitrix in THIS order, each one once:\n", len(got))
	for i, id := range got {
		fmt.Fprintf(out, "  %d. %s   (see IMPORT_ORDER.txt inside)\n", i+1, id)
	}
	return got, nil
}

// fetchBundle writes into a hidden ".<id>.partial" folder and renames it only when every file is complete,
// so a cut connection never leaves a folder that looks ready to import.
func fetchBundle(client *http.Client, cfg config, b bundle, out io.Writer) error {
	final := filepath.Join(cfg.Folder, b.ID)
	tmp := filepath.Join(cfg.Folder, "."+b.ID+".partial")
	os.RemoveAll(tmp)
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return err
	}
	for _, f := range b.Files {
		if !fileName.MatchString(f.Name) {
			os.RemoveAll(tmp)
			return fmt.Errorf("server sent an unexpected file name %q", f.Name)
		}
		if err := fetchFile(client, cfg, b.ID, f.Name, f.Size, filepath.Join(tmp, f.Name), out); err != nil {
			os.RemoveAll(tmp)
			return err
		}
	}
	os.RemoveAll(final) // a leftover from a run that stopped between rename and downloaded.txt
	return os.Rename(tmp, final)
}

func fetchFile(client *http.Client, cfg config, id, name string, size int64, path string, out io.Writer) error {
	res, err := get(client, cfg, "/bundles/"+url.PathEscape(id)+"/files/"+url.PathEscape(name))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, res.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if n != size {
		return fmt.Errorf("%s: got %d bytes, expected %d", name, n, size)
	}
	fmt.Fprintf(out, "  %-32s %s\n", name, human(n))
	return nil
}

// fatalError: nothing else will work either (server unreachable, wrong token, downloads switched off).
type fatalError struct{ error }

func (e fatalError) Unwrap() error { return e.error }

// call returns the response for any status except the fatal ones; the caller checks the status.
func call(client *http.Client, cfg config, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, cfg.Server+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fatalError{fmt.Errorf("cannot reach the server at %s. Is this computer on the office network? (%v)", cfg.Server, err)}
	}
	switch res.StatusCode {
	case http.StatusUnauthorized:
		res.Body.Close()
		return nil, fatalError{errors.New("the server rejected LUTECIA_TOKEN in .env")}
	case http.StatusServiceUnavailable:
		res.Body.Close()
		return nil, fatalError{errors.New("the server has no token set, so it is closed to downloads")}
	}
	return res, nil
}

func get(client *http.Client, cfg config, path string) (*http.Response, error) {
	res, err := call(client, cfg, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("server answered %s for %s", res.Status, path)
	}
	return res, nil
}

func getJSON(client *http.Client, cfg config, path string, v any) error {
	res, err := get(client, cfg, path)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return json.NewDecoder(res.Body).Decode(v)
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
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
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

// loadConfig reads LUTECIA_SERVER, LUTECIA_TOKEN and optional LUTECIA_FOLDER from <kit>/.env.
// Environment variables of the same name win over the file (handy for testing; the client never needs them).
func loadConfig(dir string) (config, error) {
	kv := map[string]string{}
	data, err := os.ReadFile(filepath.Join(dir, configName))
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
	for _, k := range []string{"LUTECIA_SERVER", "LUTECIA_TOKEN", "LUTECIA_FOLDER"} {
		if v, ok := os.LookupEnv(k); ok {
			kv[k] = v
		}
	}
	if errors.Is(err, os.ErrNotExist) && kv["LUTECIA_TOKEN"] == "" {
		return config{}, errors.New("no .env next to this program: copy .env.example to .env and fill in LUTECIA_TOKEN")
	}
	cfg := config{Server: strings.TrimRight(kv["LUTECIA_SERVER"], "/"), Token: kv["LUTECIA_TOKEN"], Folder: kv["LUTECIA_FOLDER"]}
	if cfg.Folder == "" {
		cfg.Folder = "Bitrix import"
	}
	if !filepath.IsAbs(cfg.Folder) {
		cfg.Folder = filepath.Join(dir, cfg.Folder)
	}
	if u, err := url.Parse(cfg.Server); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return cfg, errors.New(".env: LUTECIA_SERVER must look like http://host:port")
	}
	if cfg.Token == "" {
		return cfg, errors.New(".env: LUTECIA_TOKEN is empty")
	}
	return cfg, nil
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

func human(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
