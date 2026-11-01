package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type runState struct {
	Status string  `json:"status"` // running | ok | ok-partial | failed | dry-run
	Note   *string `json:"note"`
}

// pull asks the server for a fresh APEX pull and waits for it. A pull that is already running (the 3-hour
// schedule, or a colleague's click) is waited for instead of failing: the server allows one at a time.
func pull(client *http.Client, cfg config, opts options) error {
	res, err := call(client, cfg, http.MethodPost, "/heartbeat/runs", strings.NewReader("{}"))
	if err != nil {
		return err
	}
	var body struct {
		RunID   int `json:"runId"`
		Message any `json:"message"`
		Active  *struct {
			RunID int `json:"runId"`
		} `json:"active"`
	}
	json.NewDecoder(res.Body).Decode(&body)
	res.Body.Close()

	switch res.StatusCode {
	case http.StatusAccepted:
	case http.StatusConflict: // already running: wait for that one
		body.Active = nil
		if err := getJSON(client, cfg, "/heartbeat/status", &body); err != nil {
			return err
		}
		if body.Active == nil {
			return nil // it finished in between; its bundle is in the list already
		}
		body.RunID = body.Active.RunID
	default:
		return fmt.Errorf("the server refused (%s: %v)", res.Status, body.Message)
	}

	start := time.Now()
	for {
		var run runState
		if err := getJSON(client, cfg, fmt.Sprintf("/heartbeat/runs/%d", body.RunID), &run); err != nil {
			return err
		}
		switch run.Status {
		case "running":
		case "ok", "ok-partial":
			return nil
		default:
			note := ""
			if run.Note != nil {
				note = *run.Note
			}
			return fmt.Errorf("the server could not read the data: %s", note)
		}
		if time.Since(start) > opts.PullTimeout {
			return fmt.Errorf("still busy after %s; that data comes with the next click", opts.PullTimeout)
		}
		time.Sleep(opts.PollEvery)
	}
}

func fetchFile(client *http.Client, cfg config, id, name string, size int64, path string) error {
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
		return nil, fatalError{fmt.Errorf("cannot connect to the Lutecia server (%s). Check the internet connection and try again", cfg.Server)}
	}
	switch res.StatusCode {
	case http.StatusUnauthorized:
		res.Body.Close()
		return nil, fatalError{settingsError{"The server did not accept the access key (LUTECIA_TOKEN). Check it with Sweeft."}}
	case http.StatusServiceUnavailable:
		res.Body.Close()
		return nil, fatalError{errors.New("the server is not open for downloads (it has no token set)")}
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
