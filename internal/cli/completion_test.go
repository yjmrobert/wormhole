package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/threads"
)

// TestThreadCompletionUsesLocalStore verifies thread IDs and titles come from storage.
func TestThreadCompletionUsesLocalStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.db")
	store, err := threads.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	thread := threads.Thread{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Peer: "alice", Title: "Review retry policy", State: threads.StatePending, StagedKickoff: "question"}
	if err := store.Put(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	app := &App{OpenStore: func() (*threads.Store, error) { return threads.Open(path) }}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	values, directive := app.completeThreads(&common{}, true)(cmd, nil, "01AR")
	if directive != cobra.ShellCompDirectiveNoFileComp || len(values) != 1 || !strings.HasPrefix(values[0], thread.ID+"\t") {
		t.Fatalf("values %v directive %v", values, directive)
	}
}
