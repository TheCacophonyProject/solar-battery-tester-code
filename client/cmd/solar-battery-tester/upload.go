// solar-battery-tester - Tests a solar battery pack using the solar-battery-tester HAT.
// Copyright (C) 2025, The Cacophony Project
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// Run zips wait in testDataDir until the server has them, then move here.
	uploadedDirName = "uploaded"
	// Runs the server won't ever accept are put aside here rather than being
	// offered again at the start of every test.
	rejectedDirName = "rejected"

	uploadTimeout = 1 * time.Minute
	// The startup check is only a small GET, and holding up the tester on an
	// unreachable server helps nobody.
	checkTimeout    = 15 * time.Second
	uploadAttempts  = 3
	uploadRetryWait = 10 * time.Second
	// The service can start before the network is up, so one failed connection
	// at boot shouldn't be reported as a misconfigured tester.
	checkAttempts  = 3
	checkRetryWait = 5 * time.Second
)

// serverConfig is where finished runs get posted. An empty URL turns uploading
// off, leaving the zips in testDataDir for the next run to pick up, so a tester
// with no network still gets through a battery.
type serverConfig struct {
	url      string
	username string
	password string
}

func (c serverConfig) enabled() bool {
	return c.url != ""
}

// rejectedError is an upload the server refused outright, as opposed to one
// that failed for a reason worth trying again.
type rejectedError struct {
	err error
}

func (e rejectedError) Error() string { return e.err.Error() }

func (e rejectedError) Unwrap() error { return e.err }

// runVerdict is the server's reply: it unpacks the run and applies the same
// pass/fail checks as plot_results.py.
type runVerdict struct {
	Run      string   `json:"run"`
	Overall  string   `json:"overall"`
	Missing  []string `json:"missing"`
	Profiles map[string]struct {
		Overall string `json:"overall"`
		Checks  []struct {
			Name     string `json:"name"`
			Passed   bool   `json:"passed"`
			Measured string `json:"measured"`
			Limit    string `json:"limit"`
		} `json:"checks"`
	} `json:"profiles"`
}

// passed is whether the server's checks on the run came back clean. An empty
// verdict -- a server that answered with something unexpected -- isn't counted
// as a failed battery.
func (v *runVerdict) passed() bool {
	return v.Overall == "" || strings.EqualFold(v.Overall, "PASS")
}

// failures names the checks the server failed, for the line that says why the
// tester is flashing red.
func (v *runVerdict) failures() []string {
	failed := []string{}
	for profile, p := range v.Profiles {
		for _, c := range p.Checks {
			if !c.Passed {
				failed = append(failed, fmt.Sprintf("%s %s (%s, limit %s)",
					profile, c.Name, c.Measured, c.Limit))
			}
		}
	}
	sort.Strings(failed)
	return failed
}

// verdictError is the server's verdict as the error that ends a run, so a pack
// the server failed flashes red like any other failure. nil when it passed.
func (v *runVerdict) verdictError() error {
	if v.passed() {
		return nil
	}
	if failed := v.failures(); len(failed) > 0 {
		return fmt.Errorf("the server's checks failed: %s", strings.Join(failed, "; "))
	}
	if len(v.Missing) > 0 {
		// Nothing failed a limit because there was nothing to check: the run
		// stopped before those readings were recorded.
		return fmt.Errorf("the server couldn't check the run: it has no %s readings",
			strings.Join(v.Missing, ", "))
	}
	return fmt.Errorf("the server's checks failed: %s", v.Overall)
}

// log prints the verdict, spelling out any check the server failed so the
// operator doesn't have to go and look it up in the web app.
func (v *runVerdict) log() {
	log.Infof("Server verdict for %s: %s", v.Run, v.Overall)
	for profile, p := range v.Profiles {
		log.Infof("  %s: %s", profile, p.Overall)
		for _, c := range p.Checks {
			if !c.Passed {
				log.Warnf("    FAIL %s: measured %s, limit %s", c.Name, c.Measured, c.Limit)
			}
		}
	}
	if len(v.Missing) > 0 {
		log.Warnf("  Run had no readings for: %s", strings.Join(v.Missing, ", "))
	}
}

// checkServer confirms the server is reachable and that it takes the configured
// credentials, so a tester with the wrong details in its config says so at
// startup instead of at the end of a test.
//
// It asks for the list of batteries, which is the cheapest thing on the server
// that sits behind the same authentication as an upload. The first ask is made
// without credentials: a server that answers it isn't checking credentials at
// all, and saying "the credentials work" in that case would be claiming
// something that hasn't been tested -- a server run straight from gunicorn,
// with no nginx in front of it, takes any username and password there is.
func checkServer(cfg serverConfig) error {
	if !cfg.enabled() {
		return nil
	}

	status, body, err := askServer(cfg, false)
	if err != nil {
		return err
	}

	switch status {
	case http.StatusOK:
		if err := looksLikeTheRunServer(cfg, body); err != nil {
			return err
		}
		if cfg.username != "" {
			log.Warnf("Server at %s answered without asking for a username or password, "+
				"so the ones in the config were not checked. Anything would be accepted.", cfg.url)
		} else {
			log.Infof("Server at %s answered. It asks for no username or password.", cfg.url)
		}
		return nil

	case http.StatusUnauthorized:
		if cfg.username == "" {
			return fmt.Errorf("the server at %s wants a username and password, and none is configured", cfg.url)
		}

	default:
		return fmt.Errorf("the server at %s answered %s -- is that the right address?",
			cfg.url, http.StatusText(status))
	}

	// It wants credentials, so now the configured ones get a real test.
	status, body, err = askServer(cfg, true)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
		if err := looksLikeTheRunServer(cfg, body); err != nil {
			return err
		}
		log.Infof("Server at %s accepted the username %q and its password.", cfg.url, cfg.username)
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("the server at %s rejected the username %q and its password", cfg.url, cfg.username)
	default:
		return fmt.Errorf("the server at %s answered %s -- is that the right address?",
			cfg.url, http.StatusText(status))
	}
}

// askServer gets the battery list, with or without the configured credentials.
// A network error is retried: the service can start before the network is up,
// and one dropped packet at boot shouldn't look like a misconfigured tester.
func askServer(cfg serverConfig, withAuth bool) (status int, body []byte, err error) {
	url := strings.TrimSuffix(cfg.url, "/") + "/api/batteries"
	client := &http.Client{Timeout: checkTimeout}

	for attempt := 1; attempt <= checkAttempts; attempt++ {
		var req *http.Request
		req, err = http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return 0, nil, fmt.Errorf("%s isn't a usable address: %v", cfg.url, err)
		}
		if withAuth && cfg.username != "" {
			req.SetBasicAuth(cfg.username, cfg.password)
		}

		var resp *http.Response
		resp, err = client.Do(req)
		if err != nil {
			if attempt < checkAttempts {
				log.Debugf("Reaching %s (attempt %d of %d): %v", cfg.url, attempt, checkAttempts, err)
				time.Sleep(checkRetryWait)
			}
			continue
		}
		body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return 0, nil, fmt.Errorf("reading the reply from %s: %v", url, err)
		}
		return resp.StatusCode, body, nil
	}
	return 0, nil, fmt.Errorf("can't reach the server at %s: %v", cfg.url, err)
}

// looksLikeTheRunServer checks the reply came from the battery run server, and
// not from whatever else might be listening on that address.
func looksLikeTheRunServer(cfg serverConfig, body []byte) error {
	var reply map[string]json.RawMessage
	if err := json.Unmarshal(body, &reply); err != nil {
		return fmt.Errorf("%s answered, but not as the battery run server does", cfg.url)
	}
	if _, ok := reply["batteries"]; !ok {
		return fmt.Errorf("%s answered, but not as the battery run server does", cfg.url)
	}
	return nil
}

// uploadRun posts one run zip to the server, moving it into the uploaded folder
// once the server has it. Runs are filed on the server under batteryID.
func uploadRun(cfg serverConfig, zipPath string, batteryID int) (*runVerdict, error) {
	if !cfg.enabled() {
		return nil, fmt.Errorf("no server configured (set --server-url), leaving %s to upload later", zipPath)
	}

	var verdict *runVerdict
	var lastErr error
	for attempt := 1; attempt <= uploadAttempts; attempt++ {
		var retryable bool
		verdict, retryable, lastErr = postRun(cfg, zipPath, batteryID)
		if lastErr == nil {
			break
		}
		if !retryable {
			return nil, rejectedError{lastErr}
		}
		log.Warnf("Upload attempt %d of %d failed: %v", attempt, uploadAttempts, lastErr)
		if attempt < uploadAttempts {
			time.Sleep(uploadRetryWait)
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}

	if err := markUploaded(zipPath); err != nil {
		// The server has the run, so this isn't fatal; it just means the zip
		// gets offered again on the next run, which the server de-duplicates.
		log.Errorf("Moving %s into %s: %v", zipPath, uploadedDirName, err)
	}
	return verdict, nil
}

// postRun does a single upload attempt. The retryable return says whether it's
// worth trying again: a network hiccup or a server-side error is, but a rejected
// upload (bad credentials, unreadable zip) will be rejected just as fast again.
func postRun(cfg serverConfig, zipPath string, batteryID int) (verdict *runVerdict, retryable bool, err error) {
	f, err := os.Open(zipPath)
	if err != nil {
		return nil, false, fmt.Errorf("opening %s: %v", zipPath, err)
	}
	defer f.Close()

	// The zip is a few MB at most, so it's simpler to build the whole body in
	// memory than to stream it and lose the ability to retry.
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := mw.WriteField("battery_id", fmtI(batteryID)); err != nil {
		return nil, false, err
	}
	if err := mw.WriteField("tester", testerName()); err != nil {
		return nil, false, err
	}
	part, err := mw.CreateFormFile("zipfile", filepath.Base(zipPath))
	if err != nil {
		return nil, false, err
	}
	if _, err := io.Copy(part, f); err != nil {
		return nil, false, fmt.Errorf("reading %s: %v", zipPath, err)
	}
	if err := mw.Close(); err != nil {
		return nil, false, err
	}

	url := strings.TrimSuffix(cfg.url, "/") + "/runs"
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if cfg.username != "" {
		req.SetBasicAuth(cfg.username, cfg.password)
	}

	client := &http.Client{Timeout: uploadTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("posting to %s: %v", url, err)
	}
	defer resp.Body.Close()

	reply, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, true, fmt.Errorf("reading reply from %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		// 5xx is the server having a bad day; 4xx means it won't ever accept this.
		return nil, resp.StatusCode >= 500,
			fmt.Errorf("%s returned %s: %s", url, resp.Status, strings.TrimSpace(string(reply)))
	}

	verdict = &runVerdict{}
	if err := json.Unmarshal(reply, verdict); err != nil {
		return nil, false, fmt.Errorf("reading verdict from %s: %v", url, err)
	}
	return verdict, false, nil
}

// uploadPending offers the server every run zip still sitting in testDataDir,
// which is how a run that finished while the network was down eventually gets
// there. Returns the number still waiting afterwards.
func uploadPending(cfg serverConfig) int {
	pending, err := pendingZips()
	if err != nil {
		log.Errorf("Looking for runs to upload: %v", err)
		return 0
	}
	if len(pending) == 0 {
		return 0
	}
	if !cfg.enabled() {
		log.Warnf("%d run(s) waiting to be uploaded, but no server is configured (set --server-url).", len(pending))
		return len(pending)
	}

	log.Infof("Uploading %d run(s) from earlier tests.", len(pending))
	waiting := 0
	for _, zipPath := range pending {
		batteryID, err := batteryIDFromRunName(filepath.Base(zipPath))
		if err != nil {
			log.Errorf("Skipping %s: %v", zipPath, err)
			continue
		}
		verdict, err := uploadRun(cfg, zipPath, batteryID)
		if err != nil {
			log.Errorf("Uploading %s: %v", zipPath, err)
			var rejected rejectedError
			if errors.As(err, &rejected) {
				setAside(zipPath, rejectedDirName)
				continue
			}
			waiting++
			continue
		}
		log.Infof("Uploaded %s.", filepath.Base(zipPath))
		verdict.log()
	}
	return waiting
}

// pendingZips lists the run zips in testDataDir that the server hasn't taken yet.
func pendingZips() ([]string, error) {
	entries, err := os.ReadDir(testDataDir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %v", testDataDir, err)
	}
	zips := []string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".zip") {
			continue
		}
		zips = append(zips, filepath.Join(testDataDir, e.Name()))
	}
	return zips, nil
}

// markUploaded moves a zip the server has taken out of the pending folder.
func markUploaded(zipPath string) error {
	return moveInto(zipPath, uploadedDirName)
}

// setAside stops a zip being offered to the server again, keeping it on disk.
func setAside(zipPath, dirName string) {
	log.Warnf("The server won't take %s, moving it to %s/ — it won't be offered again.",
		filepath.Base(zipPath), dirName)
	if err := moveInto(zipPath, dirName); err != nil {
		log.Errorf("Moving %s into %s: %v", zipPath, dirName, err)
	}
}

func moveInto(zipPath, dirName string) error {
	dir := filepath.Join(testDataDir, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.Rename(zipPath, filepath.Join(dir, filepath.Base(zipPath)))
}

// testerName identifies the rig that ran the test. It goes in the run's name as
// well as alongside it on the server, so it is held to characters that are safe
// in a filename and that can't be mistaken for the name's own separators.
func testerName() string {
	name, err := os.Hostname()
	if err != nil {
		log.Warnf("Reading hostname: %v", err)
		return "unknown"
	}
	name = strings.Trim(unsafeTesterChars.ReplaceAllString(name, "-"), "-")
	if name == "" {
		return "unknown"
	}
	return name
}

var unsafeTesterChars = regexp.MustCompile(`[^A-Za-z0-9.-]+`)
