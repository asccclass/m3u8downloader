package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/chromedp/chromedp"
)

func TestClearanceConcurrentRecoveryAndScope(t *testing.T) {
	var acquisitions atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "browser-ua" && r.Header.Get("Cookie") == "cf_clearance=valid" {
			fmt.Fprint(w, "ok")
			return
		}
		w.Header().Set("Server", "cloudflare")
		w.WriteHeader(403)
	}))
	defer s.Close()
	tr := newClearanceTransport()
	tr.acquire = func(ctx context.Context, target, referer string) (*clearanceSession, error) {
		acquisitions.Add(1)
		u, _ := url.Parse(target)
		jar, _ := cookiejar.New(nil)
		jar.SetCookies(u, []*http.Cookie{{Name: "cf_clearance", Value: "valid", Path: "/"}})
		return &clearanceSession{ua: "browser-ua", jar: jar}, nil
	}
	c := &http.Client{Transport: tr}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := c.Get(s.URL + "/segment")
			if e != nil {
				t.Error(e)
				return
			}
			defer r.Body.Close()
			if r.StatusCode != 200 {
				t.Error(r.StatusCode)
			}
		}()
	}
	wg.Wait()
	if acquisitions.Load() != 1 {
		t.Fatal("duplicate browsers", acquisitions.Load())
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.UserAgent() == "browser-ua" {
			t.Error("session leaked across ports")
		}
	}))
	defer other.Close()
	r, e := c.Get(other.URL)
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
}

func TestClearanceFailureCachedAndCancelable(t *testing.T) {
	tr := newClearanceTransport()
	a := &clearanceAttempt{done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", "https://example.test", nil)
	if _, e := tr.withSession(r, a, "https://example.test"); e != context.Canceled {
		t.Fatal(e)
	}
	a.err = fmt.Errorf("failed")
	close(a.done)
	r, _ = http.NewRequest("GET", "https://example.test", nil)
	if _, e := tr.withSession(r, a, "https://example.test"); e == nil {
		t.Fatal("missing failure")
	}
}

func TestClearanceFailureAttemptedOnce(t *testing.T) {
	var count atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/challenge" {
			w.Header().Set("Cf-Mitigated", "challenge")
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer s.Close()
	tr := newClearanceTransport()
	tr.acquire = func(context.Context, string, string) (*clearanceSession, error) {
		count.Add(1)
		return nil, fmt.Errorf("browser unavailable")
	}
	c := &http.Client{Transport: tr}
	r, err := c.Get(s.URL + "/ordinary")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if count.Load() != 0 {
		t.Fatal("ordinary 403 launched browser")
	}
	for i := 0; i < 3; i++ {
		if _, err := c.Get(s.URL + "/challenge"); err == nil {
			t.Fatal("expected verification error")
		}
	}
	if count.Load() != 1 {
		t.Fatal("failed verification repeated", count.Load())
	}
}

func TestAcquireClearanceBrowser(t *testing.T) {
	if !dependencies()["chrome"] {
		t.Skip("Chrome required")
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<script>setTimeout(()=>document.cookie='cf_clearance=test; path=/', 500)</script>`)
	}))
	defer s.Close()
	// Use the same installed browser discovery as production.
	ctx, cancel := chromedp.NewContext(context.Background())
	defer cancel()
	session, err := acquireClearance(ctx, s.URL+"/index.m3u8", s.URL+"/page")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(s.URL)
	if session.ua == "" || len(session.jar.Cookies(u)) != 1 {
		t.Fatal("missing browser session")
	}
}
