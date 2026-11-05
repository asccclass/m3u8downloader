package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestSherryStartupConfigAndGracefulShutdown(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	for _, k := range []string{"SystemName", "SessionCookieAuthKey", "SessionCookieEncryptionKey", "TLSAutoEnabled", "sslCertification", "sslKeyfile"} {
		t.Setenv(k, "")
	}
	if e := runWebContext(context.Background(), "0.0.0.0:8080", "out", "profiles.json"); e == nil {
		t.Fatal("non-loopback accepted")
	}
	if e := runWebContext(context.Background(), "127.0.0.1:0", "out", "profiles.json"); e == nil {
		t.Fatal("missing envfile accepted")
	}
	if e := initializeConfig("envfile"); e != nil {
		t.Fatal(e)
	}
	original, _ := os.ReadFile("envfile")
	if e := initializeConfig("envfile"); e == nil {
		t.Fatal("init overwrites existing file")
	}
	after, _ := os.ReadFile("envfile")
	if !bytes.Equal(original, after) {
		t.Fatal("envfile modified")
	}
	// godotenv intentionally does not override existing environment variables.
	for _, line := range strings.Split(string(original), "\n") {
		if strings.HasPrefix(line, "SessionCookieAuthKey=") {
			t.Setenv("SessionCookieAuthKey", strings.TrimPrefix(line, "SessionCookieAuthKey="))
		}
	}
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	addr := ln.Addr().String()
	if e = runWebContext(context.Background(), addr, "out", "config/profiles.json"); e == nil || !strings.Contains(e.Error(), "無法監聽") {
		t.Fatal("occupied port not checked", e)
	}
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runWebContext(ctx, addr, "out", "config/profiles.json") }()
	client := &http.Client{Timeout: second}
	var ready bool
	for deadline := time.Now().Add(5 * second); time.Now().Before(deadline); {
		resp, e := client.Get("http://" + addr + "/api/v1/health")
		if e == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("sherryserver failed to start")
	}
	wsCtx, stop := context.WithTimeout(context.Background(), 5*second)
	defer stop()
	c, _, e := websocket.Dial(wsCtx, "ws://"+addr+"/ws", nil)
	if e != nil {
		t.Fatal(e)
	}
	defer c.CloseNow()
	if _, _, e = c.Read(wsCtx); e != nil {
		t.Fatal(e)
	}
	entered := make(chan struct{})
	released := make(chan struct{})
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(released) }))
	defer source.Close()
	request, _ := json.Marshal(JobRequest{URL: source.URL, Mode: "m3u8"})
	resp, e := client.Post("http://"+addr+"/api/v1/jobs", "application/json", bytes.NewReader(request))
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatal("shutdown fixture job rejected", resp.StatusCode)
	}
	select {
	case <-entered:
	case <-time.After(3 * second):
		t.Fatal("job did not start")
	}
	cancel()
	select {
	case e = <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * second):
		t.Fatal("shutdown did not finish")
	}
	// Events already buffered before shutdown may still be delivered first.
	for {
		if _, _, e = c.Read(wsCtx); e != nil {
			break
		}
	}
	if wsCtx.Err() != nil {
		t.Fatal("websocket did not close before its read deadline")
	}
	select {
	case <-released:
	case <-time.After(second):
		t.Fatal("network work survived shutdown")
	}
}
func TestLongWebSocketHeartbeats(t *testing.T) {
	if testing.Short() {
		t.Skip("61 second long-connection acceptance")
	}
	a, s := testApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 70*second)
	defer cancel()
	conn, _, e := websocket.Dial(ctx, strings.Replace(s.URL, "http", "ws", 1)+"/ws", nil)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.CloseNow()
	start := time.Now()
	count := 0
	seq := uint64(0)
	for time.Since(start) < 60*second {
		_, b, e := conn.Read(ctx)
		if e != nil {
			t.Fatal(e)
		}
		var ev wireEvent
		json.Unmarshal(b, &ev)
		if ev.Seq <= seq {
			t.Fatal("non-monotonic heartbeat seq")
		}
		seq = ev.Seq
		if ev.Type == "heartbeat" {
			count++
		}
	}
	if count < 4 {
		t.Fatal("missing heartbeats", count)
	}
	a.jobs.hub.mu.Lock()
	n := len(a.jobs.hub.clients)
	a.jobs.hub.mu.Unlock()
	if n != 1 {
		t.Fatal("long connection unexpectedly removed")
	}
}
func TestLargeFileTransferPastServerTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("large transfer acceptance")
	}
	a, s := testApp(t)
	s.Config.WriteTimeout = second // Actual file handler must clear this deadline.
	id := newID()
	dir := filepath.Join(a.jobs.root, id)
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	data := bytes.Repeat([]byte("0123456789abcdef"), 640*1024)
	if e := os.WriteFile(filepath.Join(dir, "large.mp4"), data, 0600); e != nil {
		t.Fatal(e)
	}
	a.jobs.mu.Lock()
	a.jobs.jobs[id] = &Job{ID: id, State: "succeeded", Videos: []Video{{ID: "video", Ready: true, OutputName: "large.mp4"}}}
	a.jobs.mu.Unlock()
	resp, e := s.Client().Get(s.URL + "/api/v1/jobs/" + id + "/videos/video/file")
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	start := time.Now()
	received := []byte{}
	buf := make([]byte, 128*1024)
	for {
		n, e := resp.Body.Read(buf)
		received = append(received, buf[:n]...)
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		time.Sleep(140 * time.Millisecond)
	}
	if time.Since(start) < 10*second || !bytes.Equal(received, data) {
		t.Fatal("transfer failed or did not exceed 10 seconds", time.Since(start), len(received))
	}
}
