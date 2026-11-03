package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	selfupdate "github.com/yjmrobert/wormhole/internal/update"
)

type deadlineUpdater struct {
	fakeReleaseUpdater
	hasDeadline bool
}

// CheckAndUpdate records whether the automatic update context has a deadline.
func (u *deadlineUpdater) CheckAndUpdate(ctx context.Context, version string) (selfupdate.Result, error) {
	_, u.hasDeadline = ctx.Deadline()
	return u.fakeReleaseUpdater.CheckAndUpdate(ctx, version)
}

// TestAutomaticUpdateSkipsOptOutBeforeConstruction verifies opt-out avoids filesystem initialization.
func TestAutomaticUpdateSkipsOptOutBeforeConstruction(t *testing.T) {
	t.Setenv("WORMHOLE_NO_UPDATE_CHECK", "1")
	var out, errOut bytes.Buffer
	reexec := false
	app := updateTestApp(&fakeReleaseUpdater{}, &out, &errOut, &reexec)
	constructed := 0
	app.NewUpdater = func() (releaseUpdater, error) {
		constructed++
		return &fakeReleaseUpdater{}, nil
	}
	code := app.Run(context.Background(), []string{"init", "--config", filepath.Join(t.TempDir(), "config.yaml")})
	if code != ExitOK || constructed != 0 {
		t.Fatalf("code %d, updater constructions %d", code, constructed)
	}
}

// TestAutomaticUpdateHasDeadline verifies bounded automatic update execution.
func TestAutomaticUpdateHasDeadline(t *testing.T) {
	u := &deadlineUpdater{}
	var out, errOut bytes.Buffer
	reexec := false
	app := updateTestApp(&u.fakeReleaseUpdater, &out, &errOut, &reexec)
	app.NewUpdater = func() (releaseUpdater, error) { return u, nil }
	code := app.Run(context.Background(), []string{"init", "--config", filepath.Join(t.TempDir(), "config.yaml")})
	if code != ExitOK || !u.hasDeadline {
		t.Fatalf("code %d, deadline %t, stderr %q", code, u.hasDeadline, errOut.String())
	}
}

// TestUpdateEnableRejectsDevelopmentBuild verifies misleading opt-in is refused early.
func TestUpdateEnableRejectsDevelopmentBuild(t *testing.T) {
	var out, errOut bytes.Buffer
	reexec := false
	app := updateTestApp(&fakeReleaseUpdater{}, &out, &errOut, &reexec)
	app.Version = "v1.0.0-2-gabc1234"
	constructed := 0
	app.NewUpdater = func() (releaseUpdater, error) {
		constructed++
		return &fakeReleaseUpdater{}, nil
	}
	if code := app.Run(context.Background(), []string{"update", "--enable"}); code != ExitUnavailable || constructed != 0 {
		t.Fatalf("code %d, updater constructions %d", code, constructed)
	}
}

// TestVersionFlagDoesNotRepeatCommit verifies concise Fang version metadata.
func TestVersionFlagDoesNotRepeatCommit(t *testing.T) {
	var out, errOut bytes.Buffer
	app := contractApp(&out, &errOut)
	if code := app.Run(context.Background(), []string{"--version"}); code != ExitOK {
		t.Fatalf("version code %d: %s", code, errOut.String())
	}
	if got := strings.Count(out.String(), app.Commit); got != 1 {
		t.Fatalf("commit appears %d times in %q", got, out.String())
	}
}

// TestCommandDiscoveryMarksInheritedFlagsPersistent verifies consistent flag metadata.
func TestCommandDiscoveryMarksInheritedFlagsPersistent(t *testing.T) {
	var out, errOut bytes.Buffer
	app := contractApp(&out, &errOut)
	if code := app.Run(context.Background(), []string{"commands", "ask", "--flags", "--json"}); code != ExitOK {
		t.Fatalf("commands code %d: %s", code, errOut.String())
	}
	var doc commandDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, flag := range doc.Command.Flags {
		if flag.Name == "json" || flag.Name == "config" || flag.Name == "no-input" {
			if !flag.Persistent {
				t.Errorf("inherited flag %q is not marked persistent", flag.Name)
			}
		}
	}
}
