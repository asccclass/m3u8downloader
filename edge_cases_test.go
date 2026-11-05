package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMissingDependenciesRemainUsable(t *testing.T) {
	a, s := testApp(t)
	a.dependencyCheck = func() map[string]bool { return map[string]bool{"ffmpeg": true, "chrome": false} }
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "#EXTM3U\n#EXT-X-ENDLIST\n") }))
	defer source.Close()
	code, b := callAPI(t, s, "POST", "/jobs", JobRequest{URL: source.URL, Mode: "m3u8"}, nil)
	if code != 202 {
		t.Fatal("direct mode unnecessarily requires Chrome", code, string(b))
	}
	waitJob(t, a.jobs, decodeJob(t, b).ID)
	code, _ = callAPI(t, s, "POST", "/jobs", JobRequest{URL: source.URL, Mode: "page"}, nil)
	if code != 503 {
		t.Fatal("page should require Chrome")
	}
	a.dependencyCheck = func() map[string]bool { return map[string]bool{"ffmpeg": false, "chrome": false} }
	code, _ = callAPI(t, s, "GET", "/health", nil, nil)
	if code != 200 {
		t.Fatal("health unavailable")
	}
	code, _ = callAPI(t, s, "POST", "/jobs", JobRequest{URL: source.URL, Mode: "m3u8"}, nil)
	if code != 503 {
		t.Fatal("missing FFmpeg accepted")
	}
}
func TestFIFOOrder(t *testing.T) {
	first := make(chan struct{})
	entered := make(chan string, 3)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- r.URL.Path
		if r.URL.Path == "/1" {
			select {
			case <-first:
			case <-r.Context().Done():
				return
			}
		}
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-ENDLIST\n")
	}))
	defer source.Close()
	a, _ := testApp(t)
	o := Resolved{Mode: "m3u8", OutputName: "out.mp4", Concurrency: 1}
	o.URL = source.URL + "/1"
	j1, e := a.jobs.add(o, "", "", "", nil)
	if e != nil {
		t.Fatal(e)
	}
	if <-entered != "/1" {
		t.Fatal("first task")
	}
	o.URL = source.URL + "/2"
	j2, _ := a.jobs.add(o, "", "", "", nil)
	o.URL = source.URL + "/3"
	j3, _ := a.jobs.add(o, "", "", "", nil)
	close(first)
	waitJob(t, a.jobs, j1.ID)
	waitJob(t, a.jobs, j2.ID)
	waitJob(t, a.jobs, j3.ID)
	if <-entered != "/2" || <-entered != "/3" {
		t.Fatal("not FIFO")
	}
}
func TestRequestTimeoutAndRedirectByteCounter(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stall" {
			<-r.Context().Done()
			return
		}
		switch r.URL.Path {
		case "/index":
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\n/redirect.ts\n#EXT-X-ENDLIST\n")
		case "/redirect.ts":
			http.Redirect(w, r, "/final.ts", 302)
		default:
			fmt.Fprint(w, "media bytes")
		}
	}))
	defer source.Close()
	client := profileClient(source.URL, nil)
	client.Timeout = 100 * time.Millisecond
	start := time.Now()
	err := downloadContext(context.Background(), source.URL+"/stall", filepath.Join(t.TempDir(), "out.mp4"), 1, false, client, nil)
	if err == nil || time.Since(start) > second {
		t.Fatal("client timeout lost", err)
	}
	var received int64
	downloadContext(context.Background(), source.URL+"/index", filepath.Join(t.TempDir(), "out.mp4"), 1, false, profileClient(source.URL, nil), func(p Progress) { received = p.Bytes })
	if received != int64(len("media bytes")) {
		t.Fatal("redirect byte counter", received)
	}
}
func TestDiskFailureKeepTSAndHTMLRejection(t *testing.T) {
	data := tsFixture(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index":
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\n/seg.ts\n#EXT-X-ENDLIST\n")
		case "/html":
			fmt.Fprint(w, "<!doctype html><title>not a playlist</title>")
		default:
			w.Write(data)
		}
	}))
	defer source.Close()
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	os.WriteFile(blocked, []byte("file"), 0600)
	if e := downloadContext(context.Background(), source.URL+"/index", filepath.Join(blocked, "out.mp4"), 1, false, profileClient(source.URL, nil), nil); e == nil {
		t.Fatal("disk write failure ignored")
	}
	if e := downloadContext(context.Background(), source.URL+"/index", filepath.Join(root, "out.mp4"), 1, true, profileClient(source.URL, nil), nil); e != nil {
		t.Fatal(e)
	}
	ts, _ := filepath.Glob(filepath.Join(root, "*.ts"))
	if len(ts) != 1 {
		t.Fatal("keepTs ignored")
	}
	if _, e := parseM3U8(source.URL + "/html"); e == nil || !strings.Contains(e.Error(), "direct M3U8") {
		t.Fatal("HTML accepted", e)
	}
}
func TestCancelActiveRemux(t *testing.T) {
	data := tsFixture(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index" {
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\n/seg.ts\n#EXT-X-ENDLIST\n")
		} else {
			for i := 0; i < 2048; i++ {
				if _, e := w.Write(data); e != nil {
					return
				}
			}
		}
	}))
	defer source.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	start := time.Now()
	err := downloadContext(ctx, source.URL+"/index", filepath.Join(root, "out.mp4"), 1, false, profileClient(source.URL, nil), func(p Progress) {
		if p.Phase == "remuxing" {
			time.AfterFunc(10*time.Millisecond, cancel)
		}
	})
	if err == nil || ctx.Err() == nil || time.Since(start) > 5*second {
		t.Fatal("active remux cancellation failed", err)
	}
	files, _ := filepath.Glob(filepath.Join(root, "*"))
	if len(files) != 0 {
		t.Fatal("cancelled remux left files", files)
	}
}

func TestThroughputReturnsToZeroDuringSilentSegment(t *testing.T) {
	data := tsFixture(t)
	resume := make(chan struct{})
	sent := make(chan struct{})
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index" {
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\n/seg.ts\n#EXT-X-ENDLIST\n")
			return
		}
		w.Write(data[:1024])
		w.(http.Flusher).Flush()
		close(sent)
		select {
		case <-resume:
			w.Write(data[1024:])
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(source.Close)
	a, _ := testApp(t)
	j, e := a.jobs.add(Resolved{URL: source.URL + "/index", Mode: "m3u8", OutputName: "idle.mp4", Concurrency: 1}, "", "", "", nil)
	if e != nil {
		t.Fatal(e)
	}
	<-sent
	sawPositive := false
	zero := false
	for deadline := time.Now().Add(5 * second); time.Now().Before(deadline); {
		current, _ := a.jobs.get(j.ID)
		if len(current.Videos) > 0 {
			v := current.Videos[0]
			if v.Speed > 0 {
				sawPositive = true
			}
			if sawPositive && v.Speed == 0 && v.Bytes == 1024 && v.State == "running" {
				zero = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	close(resume)
	final := waitJob(t, a.jobs, j.ID)
	if !sawPositive || !zero || final.State != "succeeded" {
		t.Fatal("idle throughput did not reach zero", sawPositive, zero, final.State)
	}
}
