package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func testApp(t *testing.T) (*WebApp, *httptest.Server) {
	t.Helper()
	root := t.TempDir()
	ps, e := openProfiles(filepath.Join(root, "config", "profiles.json"))
	if e != nil {
		t.Fatal(e)
	}
	jm, e := newManager(filepath.Join(root, "downloads"))
	if e != nil {
		t.Fatal(e)
	}
	a := &WebApp{profiles: ps, jobs: jm}
	s := httptest.NewUnstartedServer(a.handler())
	a.allowedHost = s.Listener.Addr().String()
	s.Start()
	t.Cleanup(func() { jm.close(); s.Close() })
	return a, s
}
func callAPI(t *testing.T, s *httptest.Server, method, path string, input any, headers map[string]string) (int, []byte) {
	t.Helper()
	var b []byte
	if input != nil {
		b, _ = json.Marshal(input)
	}
	req, _ := http.NewRequest(method, s.URL+"/api/v1"+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, e := s.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	out, e := io.ReadAll(resp.Body)
	if e != nil {
		t.Fatal(e)
	}
	return resp.StatusCode, out
}
func decodeJob(t *testing.T, b []byte) Job {
	t.Helper()
	var j Job
	if e := json.Unmarshal(b, &j); e != nil {
		t.Fatal(e, string(b))
	}
	return j
}
func waitJob(t *testing.T, m *JobManager, id string) Job {
	t.Helper()
	deadline := time.Now().Add(20 * second)
	for time.Now().Before(deadline) {
		j, e := m.get(id)
		if e != nil {
			t.Fatal(e)
		}
		if terminal(j.State) {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job timeout", id)
	return Job{}
}
func waitState(t *testing.T, m *JobManager, id, state string) {
	t.Helper()
	deadline := time.Now().Add(5 * second)
	for time.Now().Before(deadline) {
		j, _ := m.get(id)
		if j.State == state {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state %s not reached", state)
}
func defaultProfile(host string) SiteProfile {
	return SiteProfile{Name: "測試設定", Enabled: true, Mode: "m3u8", MediaHosts: []string{host}, WaitSeconds: 6, Concurrency: 3}
}

var fixtureOnce sync.Once
var fixtureData []byte
var fixtureErr error

func tsFixture(t *testing.T) []byte {
	t.Helper()
	if !dependencies()["ffmpeg"] {
		t.Skip("FFmpeg required")
	}
	fixtureOnce.Do(func() {
		dir, e := os.MkdirTemp("", "m3u8-fixture-")
		if e != nil {
			fixtureErr = e
			return
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "clip.ts")
		out, e := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=128x96:rate=10", "-t", "0.4", "-c:v", "mpeg2video", "-f", "mpegts", path).CombinedOutput()
		if e != nil {
			fixtureErr = fmt.Errorf("fixture: %w %s", e, out)
			return
		}
		fixtureData, fixtureErr = os.ReadFile(path)
	})
	if fixtureErr != nil {
		t.Fatal(fixtureErr)
	}
	return fixtureData
}

func TestProfilePersistenceSecretsAndVersion(t *testing.T) {
	a, _ := testApp(t)
	p := defaultProfile("example.com")
	p.DownloadHeaders = []HeaderRule{{Name: "Referer", Value: "https://page.example/"}, {Name: "Cookie", Value: "session=never-return-this", Save: true}, {Name: "X-Transient", Value: "not-on-disk", Sensitive: true}}
	p.QueryDefaults = []HeaderRule{{Name: "token", Value: "query-secret", Scope: "playlist", Save: true}}
	saved, e := a.profiles.save(p, true)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(saved)
	if bytes.Contains(b, []byte("never-return")) || bytes.Contains(b, []byte("query-secret")) {
		t.Fatal("secret leaked", string(b))
	}
	stored, _ := os.ReadFile(a.profiles.path)
	if bytes.Contains(stored, []byte("not-on-disk")) {
		t.Fatal("transient secret persisted")
	}
	reopened, e := openProfiles(a.profiles.path)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := reopened.get(saved.ID)
	if raw.DownloadHeaders[1].Value != "session=never-return-this" {
		t.Fatal("saved cookie missing")
	}
	if _, e = a.profiles.save(saved, false); e != nil {
		t.Fatal(e)
	}
	if _, e = a.profiles.save(saved, false); e == nil {
		t.Fatal("stale revision accepted")
	}
	updated, _ := a.profiles.get(saved.ID)
	updated.DownloadHeaders[1].Clear = true
	if _, e = a.profiles.save(updated, false); e != nil {
		t.Fatal(e)
	}
	updated, _ = a.profiles.get(saved.ID)
	if updated.DownloadHeaders[1].Value != "" {
		t.Fatal("secret not cleared")
	}
	if e = a.profiles.remove(saved.ID, updated.Revision); e != nil {
		t.Fatal(e)
	}
	if len(a.profiles.list()) != 0 {
		t.Fatal("delete failed")
	}
}
func TestProfileWriteFailureAndCorruptionPreservesOriginal(t *testing.T) {
	a, _ := testApp(t)
	p, e := a.profiles.save(defaultProfile("example.com"), true)
	if e != nil {
		t.Fatal(e)
	}
	original, _ := os.ReadFile(a.profiles.path)
	a.profiles.path = filepath.Join(a.profiles.path, "cannot-write.json")
	p.Name = "changed"
	if _, e = a.profiles.save(p, false); e == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	if a.profiles.list()[0].Name == "changed" {
		t.Fatal("failed write committed to memory")
	}
	badPath := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(badPath, []byte("{invalid"), 0600)
	if _, e = openProfiles(badPath); e == nil {
		t.Fatal("corruption accepted")
	}
	b, _ := os.ReadFile(badPath)
	if string(b) != "{invalid" || len(original) == 0 {
		t.Fatal("corrupt file replaced")
	}
}
func TestProfileResolveModesHostsAndOverrides(t *testing.T) {
	a, _ := testApp(t)
	p := defaultProfile("cdn.example.com")
	p.PageHosts = []string{"page.example.com"}
	p.DownloadHeaders = []HeaderRule{{Name: "X-Player", Value: "default"}}
	p.QueryDefaults = []HeaderRule{{Name: "token", Value: "new", Scope: "playlist", Save: true}, {Name: "extra", Value: "yes", Scope: "playlist", Save: true}}
	saved, e := a.profiles.save(p, true)
	if e != nil {
		t.Fatal(e)
	}
	n := 4
	req := JobRequest{URL: "https://cdn.example.com/x?z=%2f&token=old", ProfileSelection: "explicit", ProfileID: saved.ID, ProfileRevision: saved.Revision, Concurrency: &n, DownloadHeaders: []HeaderRule{{Name: "x-player", Value: "once"}}}
	o, _, e := a.profiles.resolve(req, false)
	if e != nil {
		t.Fatal(e)
	}
	if o.Mode != "m3u8" || o.Concurrency != 4 || o.DownloadHeaders[0].Value != "once" || o.URL != "https://cdn.example.com/x?z=%2f&token=old&extra=yes" {
		t.Fatalf("bad resolution: %+v", o)
	}
	raw, _ := a.profiles.get(saved.ID)
	if raw.DownloadHeaders[0].Value != "default" {
		t.Fatal("override persisted")
	}
	req.URL = "https://page.example.com/watch"
	if _, _, e = a.profiles.resolve(req, false); e == nil {
		t.Fatal("page accepted in direct-only profile")
	}
	req.URL = "https://cdn.example.com.evil.test/x"
	if _, _, e = a.profiles.resolve(req, false); e == nil {
		t.Fatal("suffix spoof accepted")
	}
	req.URL = "https://cdn.example.com/x"
	req.ProfileRevision = 999
	if _, _, e = a.profiles.resolve(req, false); e == nil {
		t.Fatal("stale preview accepted")
	}
	p.ID = ""
	p.Name = "second"
	a.profiles.save(p, true)
	req.ProfileSelection = "auto"
	_, cs, e := a.profiles.resolve(req, true)
	if e == nil || len(cs) != 2 {
		t.Fatal("ambiguous CDN not reported")
	}
	for _, tc := range []struct {
		p, u string
		want bool
	}{{"*.example.com", "https://a.example.com", true}, {"example.com", "https://a.example.com", false}, {"example.com:443", "https://example.com", true}, {"example.com:443", "http://example.com", false}, {"例子.台灣", "https://例子.台灣", true}} {
		u, _ := url.Parse(tc.u)
		if hostMatch(tc.p, u) != tc.want {
			t.Fatal(tc)
		}
	}
	zero := 0
	if _, _, e = a.profiles.resolve(JobRequest{URL: "https://unmatched.example/x", ProfileSelection: "auto", Concurrency: &zero}, true); e == nil {
		t.Fatal("unmatched auto bypasses validation")
	}
}
func TestValidationAndHeaderScopes(t *testing.T) {
	for _, name := range []string{"../x", "C:\\x", "CON.mp4", "a/b", "NUL", "file."} {
		if _, e := safeName(name); e == nil {
			t.Error("accepted unsafe name", name)
		}
	}
	for _, rule := range []HeaderRule{{Name: "Host", Value: "x"}, {Name: "X-Test", Value: "x\r\nY: z"}, {Name: "Cookie", Value: "a=b", TargetHosts: []string{"*.example.com"}}} {
		if validateRules([]HeaderRule{rule}, false) == nil {
			t.Error("invalid header accepted", rule.Name)
		}
	}
	var leaks atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Player") != "" {
			leaks.Add(1)
		}
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-ENDLIST\n")
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "session=secret" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing origin headers")
		}
		http.Redirect(w, r, other.URL, 302)
	}))
	defer origin.Close()
	rules := []HeaderRule{{Name: "Cookie", Value: "session=secret", TargetHosts: []string{strings.TrimPrefix(other.URL, "http://"), strings.TrimPrefix(origin.URL, "http://")}}, {Name: "Authorization", Value: "Bearer secret"}, {Name: "X-Player", Value: "test"}}
	if _, e := parsePlaylist(origin.URL, map[string]bool{}, profileClient(origin.URL, rules)); e != nil {
		t.Fatal(e)
	}
	if leaks.Load() != 0 {
		t.Fatal("headers leaked across redirect")
	}
}
func TestAPIProfilesAndGuards(t *testing.T) {
	_, s := testApp(t)
	status, b := callAPI(t, s, "POST", "/site-profiles", defaultProfile("example.com"), nil)
	if status != 201 {
		t.Fatal(status, string(b))
	}
	var p SiteProfile
	json.Unmarshal(b, &p)
	for _, tc := range []struct {
		method, path string
		input        any
		headers      map[string]string
		status       int
	}{{"GET", "/health", nil, map[string]string{"Origin": "https://evil.example"}, 403}, {"POST", "/jobs", JobRequest{URL: "file:///secret"}, nil, 400}, {"POST", "/jobs", JobRequest{URL: "https://example.com/a", OutputName: "../secret"}, nil, 400}, {"PUT", "/site-profiles/" + p.ID, p, map[string]string{}, 200}, {"PUT", "/site-profiles/" + p.ID, p, nil, 409}, {"DELETE", "/site-profiles/" + p.ID, nil, map[string]string{"If-Match": "2"}, 204}} {
		code, out := callAPI(t, s, tc.method, tc.path, tc.input, tc.headers)
		if code != tc.status {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, code, out)
		}
	}
	req, _ := http.NewRequest("GET", s.URL+"/api/v1/health", nil)
	req.Host = "evil.example"
	resp, e := s.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("host spoof accepted")
	}
	for _, path := range []string{"/downloads/", "/envfile", "/config/site-profiles.json", "/sendsocketmessageinstring"} {
		resp, e := s.Client().Get(s.URL + path)
		if e != nil {
			t.Fatal(e)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatal(path, resp.StatusCode)
		}
	}
}

func TestWebDownloadAESProgressFilesAndIdempotency(t *testing.T) {
	data := tsFixture(t)
	key := []byte("1234567890123456")
	iv := make([]byte, 16)
	block, _ := aes.NewCipher(key)
	padded := append([]byte(nil), data...)
	if n := len(padded) % 16; n != 0 {
		padded = append(padded, make([]byte, 16-n)...)
	}
	encrypted := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(encrypted, padded)
	var active, maxActive atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Player") != "fixture" || r.Referer() != "https://page.example/" {
			w.WriteHeader(403)
			return
		}
		switch r.URL.Path {
		case "/index":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"/key\",IV=0x00000000000000000000000000000000\n")
			for i := 0; i < 20; i++ {
				fmt.Fprintf(w, "#EXTINF:0.4,\n/seg%d.ts\n", i)
			}
			fmt.Fprint(w, "#EXT-X-ENDLIST\n")
		case "/key":
			w.Write(key)
		default:
			n := active.Add(1)
			for {
				old := maxActive.Load()
				if n <= old || maxActive.CompareAndSwap(old, n) {
					break
				}
			}
			defer active.Add(-1)
			time.Sleep(35 * time.Millisecond)
			w.Write(encrypted)
		}
	}))
	defer source.Close()
	a, s := testApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*second)
	defer cancel()
	ws, _, e := websocket.Dial(ctx, strings.Replace(s.URL, "http", "ws", 1)+"/ws", nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ws.CloseNow()
	var initial wireEvent
	_, b, e := ws.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	json.Unmarshal(b, &initial)
	if initial.Type != "snapshot" || initial.Seq != 1 {
		t.Fatal("missing initial snapshot")
	}
	n := 3
	req := JobRequest{URL: source.URL + "/index", Mode: "m3u8", OutputName: "movie.mp4", Concurrency: &n, DownloadHeaders: []HeaderRule{{Name: "X-Player", Value: "fixture"}, {Name: "Referer", Value: "https://page.example/"}}}
	code, b := callAPI(t, s, "POST", "/jobs", req, map[string]string{"Idempotency-Key": "one"})
	if code != 202 {
		t.Fatal(code, string(b))
	}
	j := decodeJob(t, b)
	code, b = callAPI(t, s, "POST", "/jobs", req, map[string]string{"Idempotency-Key": "one"})
	if code != 202 || decodeJob(t, b).ID != j.ID {
		t.Fatal("idempotency failure")
	}
	req.OutputName = "different"
	code, _ = callAPI(t, s, "POST", "/jobs", req, map[string]string{"Idempotency-Key": "one"})
	if code != 409 {
		t.Fatal("key content mismatch accepted")
	}
	phases := map[string]bool{}
	seq := uint64(1)
	for {
		_, b, e = ws.Read(ctx)
		if e != nil {
			t.Fatal(e)
		}
		var ev wireEvent
		json.Unmarshal(b, &ev)
		if ev.Seq <= seq {
			t.Fatal("nonmonotonic websocket")
		}
		seq = ev.Seq
		if ev.JobID != j.ID {
			continue
		}
		var current Job
		bb, _ := json.Marshal(ev.Data)
		json.Unmarshal(bb, &current)
		phases[current.Phase] = true
		if time.Since(current.CreatedAt) > second && current.State == "queued" {
			t.Fatal("update latency exceeded")
		}
		if ev.Type == "job.finished" {
			break
		}
	}
	j = waitJob(t, a.jobs, j.ID)
	if j.State != "succeeded" || len(j.Videos) != 1 {
		t.Fatalf("job failed: %+v", j)
	}
	v := j.Videos[0]
	if v.Bytes != int64(20*len(encrypted)) || v.SegmentsSucceeded != 20 || v.SegmentsFailed != 0 || !v.Ready {
		t.Fatalf("bad stats: %+v", v)
	}
	if maxActive.Load() > 3 || maxActive.Load() < 2 {
		t.Fatal("concurrency", maxActive.Load())
	}
	for _, phase := range []string{"parsing", "downloading", "merging", "remuxing", "succeeded"} {
		if !phases[phase] {
			t.Error("missing phase", phase)
		}
	}
	path := "/jobs/" + j.ID + "/videos/" + v.ID + "/file"
	code, b = callAPI(t, s, "GET", path, nil, map[string]string{"Range": "bytes=0-15"})
	if code != 206 || len(b) != 16 {
		t.Fatal("range", code, len(b))
	}
	code, _ = callAPI(t, s, "GET", path, nil, map[string]string{"Range": "bytes=999999999-"})
	if code != 416 {
		t.Fatal("invalid range", code)
	}
	file := filepath.Join(a.jobs.root, j.ID, v.OutputName)
	out, e := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=format_name", "-of", "default=nw=1", file).CombinedOutput()
	if e != nil || !bytes.Contains(out, []byte("mp4")) {
		t.Fatal("invalid MP4", e, string(out))
	}
	temp, _ := filepath.Glob(filepath.Join(a.jobs.root, j.ID, "temp_merged_*"))
	if len(temp) != 0 {
		t.Fatal("unexpected TS retained")
	}
	ws.CloseNow()
	ws, _, e = websocket.Dial(ctx, strings.Replace(s.URL, "http", "ws", 1)+"/ws", nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ws.CloseNow()
	_, b, e = ws.Read(ctx)
	if e != nil || !bytes.Contains(b, []byte(`"state":"succeeded"`)) {
		t.Fatal("reconnect snapshot", e, string(b))
	}
}
func TestQueueCancelAndFrozenProfile(t *testing.T) {
	entered := make(chan struct{}, 1)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	t.Cleanup(source.Close)
	a, s := testApp(t)
	host := strings.TrimPrefix(source.URL, "http://")
	p, e := a.profiles.save(defaultProfile(host), true)
	if e != nil {
		t.Fatal(e)
	}
	req := JobRequest{URL: source.URL, ProfileSelection: "explicit", ProfileID: p.ID, ProfileRevision: p.Revision}
	code, b := callAPI(t, s, "POST", "/jobs", req, nil)
	if code != 202 {
		t.Fatal(code, string(b))
	}
	first := decodeJob(t, b)
	<-entered
	ids := []string{}
	for i := 0; i < 20; i++ {
		code, b = callAPI(t, s, "POST", "/jobs", req, nil)
		if code != 202 {
			t.Fatal(i, code, string(b))
		}
		ids = append(ids, decodeJob(t, b).ID)
	}
	code, _ = callAPI(t, s, "POST", "/jobs", req, nil)
	if code != 429 {
		t.Fatal("queue capacity not enforced")
	}
	p.Name = "new name"
	a.profiles.save(p, false)
	frozen, _ := a.jobs.get(ids[0])
	if frozen.ProfileName == p.Name {
		t.Fatal("queued settings changed")
	}
	code, _ = callAPI(t, s, "POST", "/jobs/"+ids[0]+"/cancel", map[string]any{}, nil)
	if code != 202 {
		t.Fatal(code)
	}
	code, _ = callAPI(t, s, "POST", "/jobs/"+ids[0]+"/cancel", map[string]any{}, nil)
	if code != 202 {
		t.Fatal("cancel not idempotent")
	}
	code, _ = callAPI(t, s, "POST", "/jobs", JobRequest{URL: source.URL, Mode: "m3u8"}, nil)
	if code != 202 {
		t.Fatal("capacity not released")
	}
	code, _ = callAPI(t, s, "POST", "/jobs/"+first.ID+"/cancel", map[string]any{}, nil)
	if code != 202 {
		t.Fatal(code)
	}
	j := waitJob(t, a.jobs, first.ID)
	if j.State != "cancelled" {
		t.Fatal(j.State)
	}
}
func TestCoreCancelStagesAndRemuxFailure(t *testing.T) {
	data := tsFixture(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index" {
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\n/segment.ts\n#EXT-X-ENDLIST\n")
		} else {
			w.Write(data)
		}
	}))
	defer source.Close()
	for _, stage := range []string{"parsing", "downloading", "merging", "remuxing"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			root := t.TempDir()
			err := downloadContext(ctx, source.URL+"/index", filepath.Join(root, "movie.mp4"), 2, false, profileClient(source.URL, nil), func(p Progress) {
				if p.Phase == stage {
					cancel()
				}
			})
			if err == nil {
				t.Fatal("cancel ignored")
			}
			files, _ := filepath.Glob(filepath.Join(root, "*"))
			if len(files) > 0 {
				t.Fatal("unfinished files remain", files)
			}
		})
	}
	badSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index" {
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\n/segment.ts\n#EXT-X-ENDLIST\n")
		} else {
			fmt.Fprint(w, "not media")
		}
	}))
	defer badSource.Close()
	root := t.TempDir()
	err := downloadContext(context.Background(), badSource.URL+"/index", filepath.Join(root, "movie.mp4"), 1, false, profileClient(badSource.URL, nil), nil)
	if err == nil || !strings.Contains(err.Error(), "FFmpeg") {
		t.Fatal("expected remux failure", err)
	}
	ts, _ := filepath.Glob(filepath.Join(root, "*.ts"))
	if len(ts) != 1 {
		t.Fatal("failed remux TS not retained")
	}
	if _, e := os.Stat(filepath.Join(root, "movie.mp4")); !os.IsNotExist(e) {
		t.Fatal("failed MP4 published")
	}
}
func TestHubSlowClientAndHistoryBounds(t *testing.T) {
	h := newHub()
	ctx, cancel := context.WithCancel(context.Background())
	slow := &wsClient{msgs: make(chan wireEvent, 1), cancel: cancel}
	h.clients[slow] = true
	h.broadcast("job.updated", "x", "", nil)
	start := time.Now()
	h.broadcast("job.finished", "x", "", nil)
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("slow client blocked hub")
	}
	if ctx.Err() == nil || len(h.clients) != 0 {
		t.Fatal("slow client not removed")
	}
	a, _ := testApp(t)
	a.jobs.mu.Lock()
	for i := 0; i < 105; i++ {
		id := newID()
		a.jobs.jobs[id] = &Job{ID: id, State: "failed"}
		a.jobs.order = append(a.jobs.order, id)
	}
	a.jobs.prune()
	if len(a.jobs.order) != 100 {
		t.Fatal("history capacity")
	}
	j := &Job{}
	for i := 0; i < 600; i++ {
		a.jobs.log(j, "line")
	}
	if len(j.Logs) != 500 {
		t.Fatal("log capacity")
	}
	a.jobs.mu.Unlock()
}
func TestWebSocketLifecycleAndLimits(t *testing.T) {
	a, s := testApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*second)
	defer cancel()
	address := strings.Replace(s.URL, "http", "ws", 1) + "/ws"
	before := runtime.NumGoroutine()
	for i := 0; i < 100; i++ {
		c, _, e := websocket.Dial(ctx, address, nil)
		if e != nil {
			t.Fatal(i, e)
		}
		if _, _, e = c.Read(ctx); e != nil {
			t.Fatal(e)
		}
		c.CloseNow()
	}
	deadline := time.Now().Add(3 * second)
	for time.Now().Before(deadline) {
		a.jobs.hub.mu.Lock()
		n := len(a.jobs.hub.clients)
		a.jobs.hub.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.jobs.hub.mu.Lock()
	n := len(a.jobs.hub.clients)
	a.jobs.hub.mu.Unlock()
	if n != 0 {
		t.Fatal("subscription leak", n)
	}
	if runtime.NumGoroutine() > before+20 {
		t.Fatal("goroutine growth")
	}
	connections := []*websocket.Conn{}
	for i := 0; i < 16; i++ {
		c, _, e := websocket.Dial(ctx, address, nil)
		if e != nil {
			t.Fatal(e)
		}
		connections = append(connections, c)
		if _, _, e = c.Read(ctx); e != nil {
			t.Fatal(e)
		}
	}
	defer func() {
		for _, c := range connections {
			c.CloseNow()
		}
	}()
	c, res, e := websocket.Dial(ctx, address, nil)
	if c != nil {
		c.CloseNow()
	}
	if e == nil || res == nil || res.StatusCode != 503 {
		t.Fatal("connection limit", e)
	}
	_, res, e = websocket.Dial(ctx, address, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"https://evil.example"}}})
	if e == nil || res.StatusCode != 403 {
		t.Fatal("cross-origin accepted")
	}
}
func TestAPISecretErrorsAndFileEscape(t *testing.T) {
	a, s := testApp(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, "token=never-leak-this Cookie: hidden")
	}))
	defer source.Close()
	code, b := callAPI(t, s, "POST", "/jobs", JobRequest{URL: source.URL + "/index.m3u8?token=never-leak-this", Mode: "m3u8", Headers: map[string]string{"cookie": "secret-cookie"}}, nil)
	if code != 202 {
		t.Fatal(code, string(b))
	}
	j := waitJob(t, a.jobs, decodeJob(t, b).ID)
	out, _ := json.Marshal(j)
	if bytes.Contains(out, []byte("never-leak-this")) || bytes.Contains(out, []byte("secret-cookie")) {
		t.Fatal("error secret leaked")
	}
	if j.State != "failed" {
		t.Fatal(j.State)
	}
	outside := filepath.Join(t.TempDir(), "outside.mp4")
	os.WriteFile(outside, []byte("secret"), 0600)
	id := newID()
	dir := filepath.Join(a.jobs.root, id)
	os.Mkdir(dir, 0700)
	link := filepath.Join(dir, "link.mp4")
	if e := os.Symlink(outside, link); e != nil {
		t.Log("symlink creation unavailable:", e)
	} else {
		a.jobs.mu.Lock()
		a.jobs.jobs[id] = &Job{ID: id, State: "succeeded", Videos: []Video{{ID: "v", OutputName: "link.mp4", Ready: true}}}
		a.jobs.mu.Unlock()
		code, b = callAPI(t, s, "GET", "/jobs/"+id+"/videos/v/file", nil, nil)
		if code != 404 || bytes.Contains(b, []byte("secret")) {
			t.Fatal("symlink escaped output root", code)
		}
	}
}
