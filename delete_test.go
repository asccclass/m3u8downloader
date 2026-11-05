package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func deletionFixture(t *testing.T, a *WebApp, state string) Job {
	t.Helper()
	j := Job{ID: newID(), State: state, OutputName: "movie.mp4", Videos: []Video{{ID: newID(), OutputName: "movie.mp4", Ready: true}}}
	a.jobs.mu.Lock()
	a.jobs.jobs[j.ID] = &j
	a.jobs.order = append(a.jobs.order, j.ID)
	a.jobs.idem["delete-"+j.ID] = idemEntry{Hash: "original", ID: j.ID}
	a.jobs.mu.Unlock()
	return j
}

func TestDeleteJobRemovesOutputsAndBroadcasts(t *testing.T) {
	a, s := testApp(t)
	j := deletionFixture(t, a, "succeeded")
	other := deletionFixture(t, a, "succeeded")
	dir := filepath.Join(a.jobs.root, j.ID)
	if e := os.MkdirAll(filepath.Join(dir, "nested"), 0700); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"movie.mp4", "temp_merged_123.ts", "nested/temporary"} {
		if e := os.WriteFile(filepath.Join(dir, name), []byte("data"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	otherDir := filepath.Join(a.jobs.root, other.ID)
	os.Mkdir(otherDir, 0700)
	os.WriteFile(filepath.Join(otherDir, "movie.mp4"), []byte("keep"), 0600)
	ctx, cancel := context.WithTimeout(context.Background(), 5*second)
	defer cancel()
	c, _, e := websocket.Dial(ctx, strings.Replace(s.URL, "http", "ws", 1)+"/ws", nil)
	if e != nil {
		t.Fatal(e)
	}
	defer c.CloseNow()
	if _, _, e = c.Read(ctx); e != nil {
		t.Fatal(e)
	}
	code, b := callAPI(t, s, "DELETE", "/jobs/"+j.ID, nil, nil)
	if code != 204 {
		t.Fatal(code, string(b))
	}
	if _, e = os.Stat(dir); !os.IsNotExist(e) {
		t.Fatal("job directory still exists", e)
	}
	if _, e = os.Stat(filepath.Join(otherDir, "movie.mp4")); e != nil {
		t.Fatal("another job was modified", e)
	}
	if _, e = a.jobs.get(j.ID); e == nil {
		t.Fatal("deleted job still present")
	}
	a.jobs.mu.Lock()
	_, cached := a.jobs.idem["delete-"+j.ID]
	a.jobs.mu.Unlock()
	if cached {
		t.Fatal("deleted idempotency entry still present")
	}
	_, b, e = c.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	var event wireEvent
	json.Unmarshal(b, &event)
	if event.Type != "job.removed" || event.JobID != j.ID {
		t.Fatal("missing removal event", string(b))
	}
	code, b = callAPI(t, s, "DELETE", "/jobs/"+j.ID, nil, nil)
	if code != 204 {
		t.Fatal("repeated delete failed", code, string(b))
	}
}

func TestDeleteJobAfterFilesMovedOrDirectoryMissing(t *testing.T) {
	for _, scenario := range []string{"mp4 moved", "directory missing", "output root missing"} {
		t.Run(scenario, func(t *testing.T) {
			a, s := testApp(t)
			j := deletionFixture(t, a, "failed")
			dir := filepath.Join(a.jobs.root, j.ID)
			os.Mkdir(dir, 0700)
			moved := filepath.Join(t.TempDir(), "saved.mp4")
			if scenario == "mp4 moved" {
				original := filepath.Join(dir, "movie.mp4")
				os.WriteFile(original, []byte("saved video"), 0600)
				if e := os.Rename(original, moved); e != nil {
					t.Fatal(e)
				}
				os.WriteFile(filepath.Join(dir, "temp_merged_123.ts"), []byte("leftover"), 0600)
			} else if scenario == "directory missing" {
				if e := os.Remove(dir); e != nil {
					t.Fatal(e)
				}
			} else {
				os.Remove(dir)
				if e := os.Remove(a.jobs.root); e != nil {
					t.Fatal(e)
				}
			}
			code, b := callAPI(t, s, "DELETE", "/jobs/"+j.ID, nil, nil)
			if code != 204 {
				t.Fatal("missing file should not be an error", code, string(b))
			}
			if _, e := os.Stat(dir); !os.IsNotExist(e) {
				t.Fatal("old directory not removed", e)
			}
			if scenario == "mp4 moved" {
				data, e := os.ReadFile(moved)
				if e != nil || string(data) != "saved video" {
					t.Fatal("moved file was modified", e)
				}
			}
		})
	}
}

func TestDeleteJobRejectsActiveAndInvalidTargets(t *testing.T) {
	a, s := testApp(t)
	for _, state := range []string{"queued", "running", "cancelling"} {
		j := deletionFixture(t, a, state)
		code, b := callAPI(t, s, "DELETE", "/jobs/"+j.ID, nil, nil)
		if code != 409 {
			t.Fatal(state, code, string(b))
		}
		if _, e := a.jobs.get(j.ID); e != nil {
			t.Fatal("active job removed")
		}
	}
	if e := a.jobs.deleteJob("../outside"); e == nil {
		t.Fatal("invalid id accepted")
	}
	if e := removeJobDirectory(a.jobs.root, "../outside"); e == nil {
		t.Fatal("directory traversal accepted")
	}
	if e := removeJobDirectory(a.jobs.root, "."); e == nil {
		t.Fatal("deleting the output root itself was accepted")
	}
	j := deletionFixture(t, a, "cancelled")
	code, _ := callAPI(t, s, "DELETE", "/jobs/"+j.ID, nil, map[string]string{"Origin": "https://evil.example"})
	if code != 403 {
		t.Fatal("cross-origin deletion accepted", code)
	}
	code, _ = callAPI(t, s, "DELETE", "/jobs/"+newID(), nil, nil)
	if code != 204 {
		t.Fatal("unknown job deletion should be idempotent", code)
	}
}
