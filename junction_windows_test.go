//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWindowsJunctionCannotEscapeOutputRoot(t *testing.T) {
	a, s := testApp(t)
	outside := t.TempDir()
	if e := os.WriteFile(filepath.Join(outside, "private.mp4"), []byte("outside-secret"), 0600); e != nil {
		t.Fatal(e)
	}
	id := newID()
	link := filepath.Join(a.jobs.root, id)
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", "New-Item -ItemType Junction -Path $env:M3U8_TEST_LINK -Target $env:M3U8_TEST_TARGET -ErrorAction Stop | Out-Null")
	cmd.Env = append(os.Environ(), "M3U8_TEST_LINK="+link, "M3U8_TEST_TARGET="+outside)
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatal("junction fixture failed", e, string(out))
	}
	defer os.Remove(link)
	a.jobs.mu.Lock()
	a.jobs.jobs[id] = &Job{ID: id, State: "succeeded", Videos: []Video{{ID: "v", OutputName: "private.mp4", Ready: true}}}
	a.jobs.mu.Unlock()
	code, b := callAPI(t, s, "GET", "/jobs/"+id+"/videos/v/file", nil, nil)
	if code != 404 {
		t.Fatal("junction escape", code, string(b))
	}
}

func TestDeleteJobDoesNotFollowNestedJunction(t *testing.T) {
	a, s := testApp(t)
	j := deletionFixture(t, a, "succeeded")
	dir := filepath.Join(a.jobs.root, j.ID)
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	outside := t.TempDir()
	externalFile := filepath.Join(outside, "keep.mp4")
	os.WriteFile(externalFile, []byte("preserved"), 0600)
	link := filepath.Join(dir, "external")
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", "New-Item -ItemType Junction -Path $env:M3U8_TEST_LINK -Target $env:M3U8_TEST_TARGET -ErrorAction Stop | Out-Null")
	cmd.Env = append(os.Environ(), "M3U8_TEST_LINK="+link, "M3U8_TEST_TARGET="+outside)
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatal("junction fixture failed", e, string(out))
	}
	defer os.Remove(link)
	code, b := callAPI(t, s, "DELETE", "/jobs/"+j.ID, nil, nil)
	if code != 204 {
		t.Fatal(code, string(b))
	}
	if data, e := os.ReadFile(externalFile); e != nil || string(data) != "preserved" {
		t.Fatal("deletion followed external junction", e)
	}
	if _, e := os.Stat(dir); !os.IsNotExist(e) {
		t.Fatal("job directory not removed", e)
	}
}
