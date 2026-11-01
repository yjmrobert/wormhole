package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	selfupdate "github.com/yjmrobert/wormhole/internal/update"
)

type fakeReleaseUpdater struct {
	automaticResult selfupdate.Result
	manualResult    selfupdate.Result
	err             error
	enabled         bool
	automaticCalls  int
	manualCalls     int
}

func (f *fakeReleaseUpdater) CheckAndUpdate(context.Context, string) (selfupdate.Result, error) {
	f.automaticCalls++
	return f.automaticResult, f.err
}

func (f *fakeReleaseUpdater) ForceUpdate(context.Context, string) (selfupdate.Result, error) {
	f.manualCalls++
	return f.manualResult, f.err
}

func (f *fakeReleaseUpdater) Enabled() bool { return f.enabled }
func (f *fakeReleaseUpdater) SetEnabled(enabled bool) error {
	f.enabled = enabled
	return nil
}

func updateTestApp(u *fakeReleaseUpdater, out, errOut *bytes.Buffer, reexec *bool) *App {
	return &App{
		Stdin:      strings.NewReader(""),
		Stdout:     out,
		Stderr:     errOut,
		IsTerminal: func() bool { return true },
		Version:    "1.0.0",
		NewUpdater: func() (releaseUpdater, error) { return u, nil },
		Reexec: func() error {
			*reexec = true
			return nil
		},
	}
}

func TestAutomaticUpdateReexecsEligibleCommand(t *testing.T) {
	u := &fakeReleaseUpdater{automaticResult: selfupdate.Result{Checked: true, Updated: true, Tag: "v1.1.0"}}
	var out, errOut bytes.Buffer
	reexec := false
	app := updateTestApp(u, &out, &errOut, &reexec)
	code := app.Run(context.Background(), []string{"init", "--config", filepath.Join(t.TempDir(), "config.yaml")})
	if code != ExitOK || u.automaticCalls != 1 || !reexec {
		t.Fatalf("code %d calls %d reexec %t; stderr %s", code, u.automaticCalls, reexec, errOut.String())
	}
}

func TestAutomaticUpdateSkipsJSONAndFailuresAreNonFatal(t *testing.T) {
	u := &fakeReleaseUpdater{err: errors.New("offline")}
	var out, errOut bytes.Buffer
	reexec := false
	app := updateTestApp(u, &out, &errOut, &reexec)
	code := app.Run(context.Background(), []string{"init", "--json", "--config", filepath.Join(t.TempDir(), "config.yaml")})
	if code != ExitOK || u.automaticCalls != 0 {
		t.Fatalf("JSON update ran: code %d calls %d", code, u.automaticCalls)
	}
	code = app.Run(context.Background(), []string{"init", "--config", filepath.Join(t.TempDir(), "config.yaml")})
	if code != ExitOK || u.automaticCalls != 1 || !strings.Contains(errOut.String(), "update check failed: offline") {
		t.Fatalf("failure was fatal or hidden: code %d calls %d stderr %q", code, u.automaticCalls, errOut.String())
	}
}

func TestUpdateCommandEnableAndForce(t *testing.T) {
	u := &fakeReleaseUpdater{manualResult: selfupdate.Result{Checked: true, Latest: "1.1.0", Tag: "v1.1.0", Updated: true}}
	var out, errOut bytes.Buffer
	reexec := false
	app := updateTestApp(u, &out, &errOut, &reexec)
	if code := app.Run(context.Background(), []string{"update", "--enable", "--json"}); code != ExitOK {
		t.Fatalf("enable code %d: %s", code, errOut.String())
	}
	if !u.enabled {
		t.Fatal("update --enable did not persist the setting")
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"update", "--json"}); code != ExitOK {
		t.Fatalf("update code %d: %s", code, errOut.String())
	}
	var res updateOutput
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Updated || res.Tag != "v1.1.0" || u.manualCalls != 1 || u.automaticCalls != 0 || reexec {
		t.Fatalf("unexpected result %+v manual %d auto %d reexec %t", res, u.manualCalls, u.automaticCalls, reexec)
	}
}
