package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type runState struct {
	ID     int     `json:"id"`
	Status string  `json:"status"` // running | ok | ok-partial | failed | dry-run
	Note   *string `json:"note"`
}

// pull asks the server for a fresh APEX pull and waits for it. A pull that is already running (the 3-hour
// schedule, or a colleague's click) is waited for instead of failing: the server allows one at a time.
func pull(client *http.Client, cfg config, opts options, out io.Writer) error {
	fmt.Fprintln(out, "Asking the server to pull the latest data from APEX ...")
	res, err := call(client, cfg, http.MethodPost, "/sync/runs", strings.NewReader("{}"))
	if err != nil {
		return err
	}
	var body struct {
		RunID   int    `json:"runId"`
		Message any    `json:"message"`
		Active  *struct {
			RunID int `json:"runId"`
		} `json:"active"`
	}
	json.NewDecoder(res.Body).Decode(&body)
	res.Body.Close()

	switch res.StatusCode {
	case http.StatusAccepted:
	case http.StatusConflict:
		fmt.Fprintln(out, "A pull is already running on the server; waiting for that one.")
		body.Active = nil
		if err := getJSON(client, cfg, "/sync/status", &body); err != nil {
			return err
		}
		if body.Active == nil {
			return nil // it finished in between; its bundle is in the list already
		}
		body.RunID = body.Active.RunID
	default:
		return fmt.Errorf("the server refused the pull (%s: %v)", res.Status, body.Message)
	}

	start := time.Now()
	lastNote := start
	for {
		var run runState
		if err := getJSON(client, cfg, fmt.Sprintf("/sync/runs/%d", body.RunID), &run); err != nil {
			return err
		}
		note := ""
		if run.Note != nil {
			note = *run.Note
		}
		switch run.Status {
		case "running":
		case "ok", "ok-partial":
			fmt.Fprintf(out, "Fresh data pulled in %s: %s.\n\n", time.Since(start).Round(time.Second), note)
			return nil
		default:
			return fmt.Errorf("the pull failed on the server: %s", note)
		}
		if time.Since(start) > opts.PullTimeout {
			return fmt.Errorf("the pull is still running after %s; its data comes with the next click", opts.PullTimeout)
		}
		if time.Since(lastNote) >= 15*time.Second {
			fmt.Fprintf(out, "  still pulling ... %s\n", time.Since(start).Round(time.Second))
			lastNote = time.Now()
		}
		time.Sleep(opts.PollEvery)
	}
}
