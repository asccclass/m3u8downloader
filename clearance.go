package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

type clearanceSession struct {
	ua        string
	jar       http.CookieJar
	transport http.RoundTripper
}
type clearanceAttempt struct {
	done    chan struct{}
	session *clearanceSession
	err     error
}

// clearanceTransport transparently handles Cloudflare bot challenges.
//
// Design goals:
//  1. Each unique origin gets exactly one in-flight browser clearance attempt;
//     concurrent segment goroutines block on the same attempt rather than each
//     spawning a browser.
//  2. When a session expires mid-download (cf_clearance cookie is invalidated),
//     the stale entry is replaced with a new attempt.  The re-challenge uses an
//     independent background context so a cancelled segment context (caused by
//     a different failed segment) cannot abort the browser session.
//  3. The caller (readResourceFile) treats CF-403-after-session as a retryable
//     error so the segment is re-tried after the new clearance arrives.
type clearanceTransport struct {
	base    http.RoundTripper
	acquire func(context.Context, string, string) (*clearanceSession, error)
	// bgCtx is a long-lived context used for browser clearance; it is NOT tied
	// to individual request contexts so a cancelled download cannot abort it.
	bgCtx   context.Context
	bgCancel context.CancelFunc
	mu      sync.Mutex
	// attempts tracks the latest clearance state per origin key.
	// A nil value means no attempt has been started yet.
	// Once set, the channel inside the attempt signals completion.
	attempts map[string]*clearanceAttempt
}

func newClearanceTransport() *clearanceTransport {
	ctx, cancel := context.WithCancel(context.Background())
	return &clearanceTransport{
		base:     http.DefaultTransport,
		acquire:  acquireClearance,
		bgCtx:    ctx,
		bgCancel: cancel,
		attempts: map[string]*clearanceAttempt{},
	}
}

func (t *clearanceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	key := strings.ToLower(req.URL.Scheme + "://" + req.URL.Host)
	t.mu.Lock()
	a := t.attempts[key]
	t.mu.Unlock()
	if a != nil {
		return t.withSession(req, a, key)
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil || req.Method != http.MethodGet || resp.StatusCode != http.StatusForbidden ||
		(!strings.EqualFold(resp.Header.Get("Server"), "cloudflare") && resp.Header.Get("Cf-Mitigated") != "challenge") {
		return resp, err
	}
	// Let http.Client store newly issued cookies and the resource retry use
	// them before escalating to a browser. Do not loop on unchanged cookies.
	for _, c := range resp.Cookies() {
		old, e := req.Cookie(c.Name)
		if c.Value != "" && (e != nil || old.Value != c.Value) {
			return resp, nil
		}
	}
	resp.Body.Close()
	return t.launchClearance(req, key)
}

// launchClearance starts a new browser clearance attempt for the given origin
// key, or joins an existing in-flight one. It is called both for the first
// challenge and when an established session is re-challenged.
func (t *clearanceTransport) launchClearance(req *http.Request, key string) (*http.Response, error) {
	t.mu.Lock()
	a := t.attempts[key]
	owner := a == nil
	if owner {
		a = &clearanceAttempt{done: make(chan struct{})}
		t.attempts[key] = a
	}
	t.mu.Unlock()
	if owner {
		// Use the transport's own background context — not req.Context() —
		// so that a cancelled segment does not abort the browser session.
		a.session, a.err = t.acquire(t.bgCtx, req.URL.String(), req.Referer())
		close(a.done)
	}
	return t.withSession(req, a, key)
}

func (t *clearanceTransport) withSession(req *http.Request, a *clearanceAttempt, key string) (*http.Response, error) {
	select {
	case <-req.Context().Done():
		return nil, req.Context().Err()
	case <-t.bgCtx.Done():
		return nil, fmt.Errorf("下載已取消")
	case <-a.done:
	}
	if a.err != nil {
		return nil, fmt.Errorf("HTTP 403: Cloudflare 瀏覽器驗證失敗；請確認 Chrome 可用、來源可播放及 IP 存取限制：%w", a.err)
	}
	r := req.Clone(req.Context())
	r.Header.Set("User-Agent", a.session.ua)
	// Replace cookies with the same name, retaining unrelated explicit cookies.
	cookies := a.session.jar.Cookies(r.URL)
	names := map[string]bool{}
	for _, c := range cookies {
		names[c.Name] = true
	}
	old := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range old {
		if !names[c.Name] {
			r.AddCookie(c)
		}
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	transport := a.session.transport
	if transport == nil {
		transport = t.base
	}
	resp, err := transport.RoundTrip(r)
	if err == nil {
		a.session.jar.SetCookies(r.URL, resp.Cookies())
	}
	// If the CDN re-challenges after the session was once valid (e.g. the
	// cf_clearance cookie expired mid-download), drop the stale session so the
	// next caller triggers a fresh browser clearance. Return a retryable network
	// error so readResourceFile retries and picks up the new session.
	if err == nil && resp.StatusCode == http.StatusForbidden &&
		(strings.EqualFold(resp.Header.Get("Server"), "cloudflare") || resp.Header.Get("Cf-Mitigated") == "challenge") {
		t.mu.Lock()
		if t.attempts[key] == a { // only the first observer resets
			delete(t.attempts, key)
		}
		t.mu.Unlock()
		resp.Body.Close()
		// Return a retryable error; readResourceFile will retry, hit
		// launchClearance, and block until the new browser session is ready.
		return nil, fmt.Errorf("cf_clearance 已過期，重新取得驗證中")
	}
	return resp, err
}

func acquireClearance(parent context.Context, target, referer string) (*clearanceSession, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("無效來源網址")
	}
	entry := u.Scheme + "://" + u.Host + "/"
	if ref, e := url.Parse(referer); e == nil && ref.Scheme == u.Scheme && strings.EqualFold(ref.Host, u.Host) {
		entry = referer
	}
	ctx, cancel := chromedp.NewContext(parent)
	defer cancel()
	ctx, timeout := context.WithTimeout(ctx, 60*time.Second)
	defer timeout()
	if err := chromedp.Do(ctx, chromedp.Navigate(entry)); err != nil {
		return nil, fmt.Errorf("無法載入驗證頁面")
	}
	ua, err := chromedp.Run(ctx, chromedp.Evaluate[string](`navigator.userAgent`))
	if err != nil {
		return nil, fmt.Errorf("無法取得瀏覽器 User-Agent")
	}
	for {
		var cookies []*network.Cookie
		err := chromedp.Do(ctx, chromedp.Func(func(c context.Context, t *chromedp.Target) error {
			result, e := cdp.Call(c, t, network.GetCookies, network.GetCookiesParams{URLs: []string{target}})
			if e == nil {
				cookies = result.Cookies
			}
			return e
		}))
		if err != nil {
			return nil, fmt.Errorf("驗證逾時或瀏覽器已關閉")
		}
		jar, _ := cookiejar.New(nil)
		clearance := false
		for _, c := range cookies {
			if c.Name == "cf_clearance" && c.Value != "" {
				clearance = true
			}
			cookie := &http.Cookie{Name: c.Name, Value: c.Value, Path: c.Path, Secure: c.Secure, HttpOnly: c.HTTPOnly}
			if !c.Session && c.Expires > 0 {
				cookie.Expires = time.Unix(int64(c.Expires), 0)
			}
			// Deliberately host-only. Never broaden browser cookies to sibling CDNs.
			jar.SetCookies(u, []*http.Cookie{cookie})
		}
		if clearance {
			transport, err := newBrowserTLSTransport(ua)
			if err != nil {
				return nil, err
			}
			return &clearanceSession{ua: ua, jar: jar, transport: transport}, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("未取得 cf_clearance，可能需要手動驗證或來源拒絕此瀏覽器")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (t *clearanceTransport) CloseIdleConnections() {
	t.bgCancel()
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, a := range t.attempts {
		select {
		case <-a.done:
			if a.session != nil {
				closeTransport(a.session.transport)
			}
		default:
		}
	}
	closeTransport(t.base)
}

func closeTransport(t http.RoundTripper) {
	if c, ok := t.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}
