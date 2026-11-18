package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func browserContext(t *testing.T) context.Context {
	t.Helper()
	if !dependencies()["chrome"] {
		t.Skip("Chrome required")
	}
	ctx, cancel := chromedp.NewContext(context.Background())
	t.Cleanup(cancel)
	ctx, timeout := context.WithTimeout(ctx, 60*second)
	t.Cleanup(timeout)
	return ctx
}
func evalBrowser(t *testing.T, ctx context.Context, js string) string {
	t.Helper()
	out, e := chromedp.Run(ctx, chromedp.Evaluate[string](js, chromedp.EvalAwaitPromise))
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func browserWait(t *testing.T, ctx context.Context, expression string) {
	t.Helper()
	if _, e := chromedp.Run(ctx, chromedp.Poll[bool](expression, chromedp.WithPollingTimeout(15*second))); e != nil {
		t.Fatalf("browser condition %s: %v", expression, e)
	}
}
func TestBrowserDesktopMobileAndDownloadWorkflow(t *testing.T) {
	data := tsFixture(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.m3u8" {
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\n/seg.ts\n#EXT-X-ENDLIST\n")
		} else {
			time.Sleep(100 * time.Millisecond)
			w.Write(data)
		}
	}))
	defer source.Close()
	a, s := testApp(t)
	ctx := browserContext(t)
	if e := chromedp.Do(ctx, chromedp.EmulateViewport(1440, 1000), chromedp.Navigate(s.URL)); e != nil {
		t.Fatal(e)
	}
	browserWait(t, ctx, `document.querySelector('#connection').textContent.includes('已連線')`)
	evalBrowser(t, ctx, `(()=>{document.querySelector('#job-profile').focus();return 'ok'})()`)
	if e := chromedp.Do(ctx, chromedp.KeyEvent("\t")); e != nil {
		t.Fatal(e)
	}
	if evalBrowser(t, ctx, `document.activeElement.id`) != "job-url" {
		t.Fatal("keyboard navigation failed")
	}
	if v := evalBrowser(t, ctx, `String(document.documentElement.scrollWidth <= innerWidth)`); v != "true" {
		t.Fatal("desktop overflow")
	}
	// Save the editable MissAV template using the visible form.
	evalBrowser(t, ctx, `(()=>{document.querySelector('[data-view=profiles]').click();const s=document.querySelector('#template-select');s.value='0';s.dispatchEvent(new Event('change'));document.querySelector('#profile-name').value='測試影片設定';document.querySelector('#profile-media-hosts').value=`+jsString(strings.TrimPrefix(source.URL, "http://"))+`;document.querySelector('#profile-page-hosts').value='page.example';profileHeaders.set([{name:'User-Agent',value:'browser-test'}]);document.querySelector('#profile-form').requestSubmit();return 'ok'})()`)
	browserWait(t, ctx, `document.querySelector('#profile-list').textContent.includes('測試影片設定')`)
	if len(a.profiles.list()) != 1 {
		t.Fatal("profile not saved by UI")
	}
	evalBrowser(t, ctx, `(()=>{const original=ws.onmessage;ws.onmessage=e=>{original(e);const ev=JSON.parse(e.data);if(ev.type==='job.finished')window.finishedUILag=Date.now()-new Date(ev.timestamp).getTime();};document.querySelector('[data-view=tasks]').click();document.querySelector('#job-url').value=`+jsString(source.URL+"/index.m3u8")+`;document.querySelector('#job-output').value='browser.mp4';document.querySelector('#job-form').requestSubmit();return 'ok'})()`)
	browserWait(t, ctx, `document.querySelector('#jobs').textContent.includes('下載 MP4')`)
	if evalBrowser(t, ctx, `String(window.finishedUILag<=1000)`) != "true" {
		t.Fatal("UI update exceeded 1 second")
	}
	if len(a.jobs.snapshot()["jobs"].([]Job)) != 1 {
		t.Fatal("UI created duplicate job")
	}
	// Close the actual connection: browser reconnect must get the terminal snapshot.
	evalBrowser(t, ctx, `(()=>{window.oldSocket=ws;ws.close();return 'ok'})()`)
	browserWait(t, ctx, `ws !== window.oldSocket && document.querySelector('#connection').textContent.includes('已連線')`)
	if evalBrowser(t, ctx, `String(document.querySelector('#jobs').textContent.includes('browser.mp4'))`) != "true" {
		t.Fatal("reconnect lost jobs")
	}
	if e := os.MkdirAll("test-results", 0700); e != nil {
		t.Fatal(e)
	}
	png, e := chromedp.Run(ctx, chromedp.FullScreenshot(90))
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile("test-results/desktop.png", png, 0600)
	if e = chromedp.Do(ctx, chromedp.EmulateViewport(375, 900)); e != nil {
		t.Fatal(e)
	}
	if evalBrowser(t, ctx, `String(document.documentElement.scrollWidth <= innerWidth)`) != "true" {
		t.Fatal("375px task overflow")
	}
	png, e = chromedp.Run(ctx, chromedp.FullScreenshot(90))
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile("test-results/mobile.png", png, 0600)
	evalBrowser(t, ctx, `(()=>{document.querySelector('[data-view=profiles]').click();return 'ok'})()`)
	if evalBrowser(t, ctx, `String(document.documentElement.scrollWidth <= innerWidth)`) != "true" {
		t.Fatal("375px profile overflow")
	}
	// Download through the real browser fetch path and verify the attachment response.
	if result := evalBrowser(t, ctx, `(async()=>{const a=document.querySelector('#jobs a[download]');const r=await fetch(a.href);return String(r.ok && (await r.arrayBuffer()).byteLength>0 && r.headers.get('content-disposition').includes('attachment'))})()`); result != "true" {
		t.Fatal("browser file download failed", result)
	}
	// A moved MP4 must not prevent the visible delete action from cleaning up.
	completed := a.jobs.snapshot()["jobs"].([]Job)[0]
	dir := filepath.Join(a.jobs.root, completed.ID)
	moved := filepath.Join(t.TempDir(), "moved.mp4")
	if e := os.Rename(filepath.Join(dir, "browser.mp4"), moved); e != nil {
		t.Fatal(e)
	}
	evalBrowser(t, ctx, `(()=>{document.querySelector('[data-view=tasks]').click();window.confirm=()=>true;document.querySelector('[data-action-id$="-delete"]').click();return 'ok'})()`)
	browserWait(t, ctx, `document.querySelector('#jobs').querySelectorAll('.job').length===0 && document.querySelector('#notice').textContent.includes('已刪除任務')`)
	if _, e := os.Stat(dir); !os.IsNotExist(e) {
		t.Fatal("UI deletion did not remove directory", e)
	}
	if _, e := os.Stat(moved); e != nil {
		t.Fatal("UI deletion affected moved file", e)
	}
}
func jsString(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestBrowserDiscoveryHeadersPartialRetryAndCancel(t *testing.T) {
	data := tsFixture(t)
	var failed atomic.Bool
	failed.Store(true)
	var browserSeen, downloadSeen, leaks atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Private") != "" {
			leaks.Add(1)
		}
		fmt.Fprint(w, "ok")
	}))
	defer other.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			if r.Header.Get("X-Browser") == "browser-value" && r.Header.Get("X-Private") == "secret-value" {
				browserSeen.Add(1)
			}
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<!doctype html><script>fetch('/one.m3u8');fetch('/two.m3u8');fetch('/three.m3u8');fetch('%s')</script>`, other.URL)
		case "/one.m3u8", "/two.m3u8", "/three.m3u8":
			if r.Header.Get("X-Download") == "download-value" {
				downloadSeen.Add(1)
				if r.Header.Get("X-Browser") != "" {
					t.Error("browser header used for download")
				}
			}
			if r.URL.Path == "/two.m3u8" && failed.Load() {
				w.WriteHeader(403)
				return
			}
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\n/seg.ts\n#EXT-X-ENDLIST\n")
		default:
			w.Write(data)
		}
	}))
	defer source.Close()
	a, s := testApp(t)
	u, _ := url.Parse(source.URL)
	p := SiteProfile{Name: "HL365 等價測試", Enabled: true, PageHosts: []string{u.Host}, Mode: "page", WaitSeconds: 1, Concurrency: 2, BrowserHeaders: []HeaderRule{{Name: "X-Browser", Value: "browser-value"}, {Name: "X-Private", Value: "secret-value", Sensitive: true, Save: true}}, DownloadHeaders: []HeaderRule{{Name: "X-Download", Value: "download-value"}}}
	saved, e := a.profiles.save(p, true)
	if e != nil {
		t.Fatal(e)
	}
	code, b := callAPI(t, s, "POST", "/jobs", JobRequest{URL: source.URL + "/page", ProfileSelection: "explicit", ProfileID: saved.ID, ProfileRevision: saved.Revision, OutputName: "batch.mp4"}, nil)
	if code != 202 {
		t.Fatal(code, string(b))
	}
	j := waitJob(t, a.jobs, decodeJob(t, b).ID)
	if j.State != "partial_failed" || j.Succeeded != 2 || j.Failed != 1 || len(j.Videos) != 3 {
		t.Fatalf("bad batch: %+v", j)
	}
	if browserSeen.Load() == 0 || downloadSeen.Load() != 3 || leaks.Load() != 0 {
		t.Fatal("headers", browserSeen.Load(), downloadSeen.Load(), leaks.Load())
	}
	failed.Store(false)
	code, b = callAPI(t, s, "POST", "/jobs/"+j.ID+"/retry", JobRequest{}, map[string]string{"Idempotency-Key": "retry-one"})
	if code != 202 {
		t.Fatal(code, string(b))
	}
	retry := waitJob(t, a.jobs, decodeJob(t, b).ID)
	if retry.State != "succeeded" || len(retry.Videos) != 1 || retry.Videos[0].OutputName != "batch_002.mp4" || retry.RetryOf != j.ID {
		t.Fatalf("bad retry: %+v", retry)
	}
	if retry.ID == j.ID {
		t.Fatal("retry overwrites original")
	}
	if _, e := os.Stat(filepath.Join(a.jobs.root, j.ID, "batch_001.mp4")); e != nil {
		t.Fatal("success overwritten")
	}
	wait := 30
	code, b = callAPI(t, s, "POST", "/jobs", JobRequest{URL: source.URL + "/page", Mode: "page", WaitSeconds: &wait}, nil)
	if code != 202 {
		t.Fatal(code, string(b))
	}
	cancelJob := decodeJob(t, b)
	waitState(t, a.jobs, cancelJob.ID, "running")
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	callAPI(t, s, "POST", "/jobs/"+cancelJob.ID+"/cancel", map[string]any{}, nil)
	cancelled := waitJob(t, a.jobs, cancelJob.ID)
	if cancelled.State != "cancelled" || time.Since(start) > 5*second {
		t.Fatal("discovery cancellation failed")
	}
}
