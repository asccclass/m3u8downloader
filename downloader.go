package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

type Progress struct {
	Phase                    string
	TSName                   string
	Total, Succeeded, Failed int
	Bytes                    int64
}
type contextTransport struct {
	ctx      context.Context
	base     http.RoundTripper
	count    *atomic.Int64
	segments map[string]bool
}

func (t *contextTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(t.ctx, cancel)
	if t.ctx.Err() != nil {
		cancel()
	}
	r = r.Clone(ctx)
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		stop()
		cancel()
		return resp, err
	}
	count := t.count != nil && t.segments[r.URL.String()]
	for prior := r; !count && prior.Response != nil; {
		prior = prior.Response.Request
		if prior == nil {
			break
		}
		count = t.count != nil && t.segments[prior.URL.String()]
	}
	if count && resp.StatusCode == http.StatusOK {
		resp.Body = &countBody{ReadCloser: resp.Body, n: t.count}
	}
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel, stop: stop}
	return resp, err
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	stop   func() bool
}

func (b *cancelBody) Close() error { e := b.ReadCloser.Close(); b.stop(); b.cancel(); return e }

type countBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (b *countBody) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	return n, e
}

// Each segment is bounded in memory and spooled to disk; memory does not grow
// with the duration of a movie. Only the coordinator emits progress callbacks.
func downloadContext(ctx context.Context, source, output string, concurrency int, keep bool, client *http.Client, emit func(Progress)) error {
	if concurrency < 1 {
		return fmt.Errorf("無效並行數")
	}
	var bytes atomic.Int64
	tr := &contextTransport{ctx: ctx, base: client.Transport}
	if tr.base == nil {
		tr.base = http.DefaultTransport
	}
	cc := *client
	cc.Transport = tr
	p := Progress{Phase: "parsing"}
	report := func() {
		p.Bytes = bytes.Load()
		if emit != nil {
			emit(p)
		}
	}
	report()
	segments, err := parsePlaylist(source, map[string]bool{}, &cc)
	if err != nil {
		return err
	}
	if len(segments) == 0 {
		return fmt.Errorf("播放清單沒有可下載的切片")
	}
	if err = os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return fmt.Errorf("無法建立輸出目錄")
	}
	spool, err := os.MkdirTemp(filepath.Dir(output), ".segments-")
	if err != nil {
		return fmt.Errorf("無法建立切片暫存目錄")
	}
	defer os.RemoveAll(spool)
	tr.segments = map[string]bool{}
	for _, s := range segments {
		tr.segments[s.URL] = true
	}
	tr.count = &bytes
	p.Phase = "downloading"
	p.Total = len(segments)
	report()
	tasks := make(chan Segment)
	results := make(chan error, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	keys := map[string][]byte{}
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range tasks {
				if ctx.Err() != nil {
					continue
				}
				data, e := downloadAndDecryptSegment(&cc, s, &keys, &mu)
				if e == nil {
					e = os.WriteFile(filepath.Join(spool, fmt.Sprintf("%09d", s.Index)), data, 0600)
				}
				results <- e
			}
		}()
	}
	go func() {
		defer close(tasks)
		for _, s := range segments {
			select {
			case tasks <- s:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
loop:
	for {
		select {
		case e, ok := <-results:
			if !ok {
				break loop
			}
			if e != nil {
				p.Failed++
			} else {
				p.Succeeded++
			}
			report()
		case <-tick.C:
			report()
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if p.Failed > 0 {
		return fmt.Errorf("%d 個切片下載失敗，未輸出不完整的影片", p.Failed)
	}
	p.Phase = "merging"
	report()
	ts, err := os.CreateTemp(filepath.Dir(output), "temp_merged_*.ts")
	if err != nil {
		return fmt.Errorf("無法建立 TS")
	}
	tsName := ts.Name()
	closed := false
	preserve := false
	defer func() {
		if !closed {
			ts.Close()
		}
		if !preserve {
			os.Remove(tsName)
		}
	}()
	for _, s := range segments {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		f, e := os.Open(filepath.Join(spool, fmt.Sprintf("%09d", s.Index)))
		if e != nil {
			return fmt.Errorf("切片讀取失敗")
		}
		_, e = copyContext(ctx, ts, f)
		f.Close()
		if e != nil {
			return fmt.Errorf("合併寫入失敗")
		}
	}
	err = ts.Close()
	closed = true
	if err != nil {
		return fmt.Errorf("關閉 TS 失敗")
	}
	preserve = keep
	p.TSName = filepath.Base(tsName)
	p.Phase = "remuxing"
	report()
	tmp, err := os.CreateTemp(filepath.Dir(output), ".output-*.mp4")
	if err != nil {
		return fmt.Errorf("無法建立 MP4")
	}
	tmpName := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpName)
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-y", "-v", "error", "-i", tsName, "-c", "copy", "-movflags", "+faststart", tmpName)
	cmd.WaitDelay = 2 * time.Second
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		preserve = true
		return fmt.Errorf("FFmpeg 轉封裝失敗，已保留 TS %s", filepath.Base(tsName))
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err = os.Rename(tmpName, output); err != nil {
		return fmt.Errorf("發布 MP4 失敗")
	}
	p.Phase = "completed"
	report()
	return nil
}
func copyContext(ctx context.Context, w io.Writer, r io.Reader) (int64, error) {
	var n int64
	buf := make([]byte, 128*1024)
	for {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		nr, e := r.Read(buf)
		if nr > 0 {
			nw, we := w.Write(buf[:nr])
			n += int64(nw)
			if we != nil {
				return n, we
			}
			if nw != nr {
				return n, io.ErrShortWrite
			}
		}
		if e == io.EOF {
			return n, nil
		}
		if e != nil {
			return n, e
		}
	}
}

func discoverContext(parent context.Context, source string, wait time.Duration, headers []HeaderRule, onFound func(string)) ([]string, error) {
	ctx, cancel := chromedp.NewContext(parent)
	defer cancel()
	ctx, timeout := context.WithTimeout(ctx, wait+30*time.Second)
	defer timeout()
	eventCtx, stop := context.WithCancel(ctx)
	defer stop()
	events := chromedp.Events(eventCtx, network.RequestWillBeSent)
	var urls []string
	seen := map[string]bool{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev, e := range events {
			if e != nil {
				return
			}
			u, e := url.Parse(ev.Request.URL)
			if e == nil && (u.Scheme == "http" || u.Scheme == "https") && strings.Contains(strings.ToLower(u.Path), ".m3u8") && !seen[ev.Request.URL] {
				seen[ev.Request.URL] = true
				urls = append(urls, ev.Request.URL)
				if onFound != nil {
					onFound(ev.Request.URL)
				}
			}
		}
	}()
	var interceptDone chan struct{}
	err := chromedp.Do(ctx, chromedp.Func(func(c context.Context, t *chromedp.Target) error {
		if _, e := cdp.Call(c, t, network.Enable, network.EnableParams{}); e != nil {
			return e
		}
		if len(headers) == 0 {
			return nil
		}
		paused := chromedp.Events(eventCtx, fetch.RequestPaused)
		interceptDone = make(chan struct{})
		go func() {
			defer close(interceptDone)
			for ev, e := range paused {
				if e != nil {
					return
				}
				h := http.Header{}
				for k, v := range ev.Request.Headers {
					h.Set(k, fmt.Sprint(v))
				}
				applyRules(h, headers, ev.Request.URL, source, false)
				entries := []*fetch.HeaderEntry{}
				for k, vs := range h {
					for _, v := range vs {
						entries = append(entries, &fetch.HeaderEntry{Name: k, Value: v})
					}
				}
				if _, e = cdp.Call(c, t, fetch.ContinueRequest, fetch.ContinueRequestParams{RequestID: ev.RequestID, Headers: entries}); e != nil {
					return
				}
			}
		}()
		_, e := cdp.Call(c, t, fetch.Enable, fetch.EnableParams{})
		return e
	}), chromedp.Navigate(source), chromedp.Sleep(wait))
	stop()
	<-done
	if interceptDone != nil {
		<-interceptDone
	}
	if parent.Err() != nil {
		return nil, parent.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("網頁偵測失敗（請確認 Chrome 與來源可用）")
	}
	return urls, nil
}
