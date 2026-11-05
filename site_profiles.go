package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

type AppError struct {
	Status        int
	Code, Message string
}

func (e *AppError) Error() string { return e.Message }
func bad(s string) error          { return &AppError{400, "INVALID_INPUT", s} }
func conflict(s string) error     { return &AppError{409, "PROFILE_CONFLICT", s} }
func missing() error              { return &AppError{404, "NOT_FOUND", "找不到資料"} }
func newID() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}

type HeaderRule struct {
	Name        string   `json:"name"`
	Value       string   `json:"value,omitempty"`
	TargetHosts []string `json:"targetHosts,omitempty"`
	Sensitive   bool     `json:"sensitive,omitempty"`
	Save        bool     `json:"save,omitempty"`
	HasValue    bool     `json:"hasValue,omitempty"`
	Persisted   bool     `json:"persisted,omitempty"`
	Clear       bool     `json:"clear,omitempty"`
	Scope       string   `json:"scope,omitempty"`
}
type SiteProfile struct {
	ID              string       `json:"id"`
	Name            string       `json:"name"`
	Enabled         bool         `json:"enabled"`
	Revision        int          `json:"revision"`
	PageHosts       []string     `json:"pageHosts"`
	MediaHosts      []string     `json:"mediaHosts"`
	Mode            string       `json:"mode"`
	DownloadHeaders []HeaderRule `json:"downloadHeaders"`
	BrowserHeaders  []HeaderRule `json:"browserHeaders"`
	QueryDefaults   []HeaderRule `json:"queryDefaults"`
	WaitSeconds     int          `json:"waitSeconds"`
	Concurrency     int          `json:"concurrency"`
	KeepTs          bool         `json:"keepTs"`
}
type profileDisk struct {
	SchemaVersion int           `json:"schemaVersion"`
	Profiles      []SiteProfile `json:"profiles"`
}
type ProfileStore struct {
	mu    sync.Mutex
	path  string
	items map[string]SiteProfile
}

func clone[T any](x T) T { b, _ := json.Marshal(x); var y T; json.Unmarshal(b, &y); return y }
func openProfiles(path string) (*ProfileStore, error) {
	s := &ProfileStore{path: path, items: map[string]SiteProfile{}}
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return s, nil
	}
	if e != nil {
		return nil, fmt.Errorf("無法讀取網站設定")
	}
	var d profileDisk
	if json.Unmarshal(b, &d) != nil || d.SchemaVersion != 1 {
		return nil, fmt.Errorf("網站設定檔損壞或版本不支援；原檔已保留")
	}
	for _, p := range d.Profiles {
		if validateProfile(&p) != nil || p.ID == "" || p.Revision < 1 {
			return nil, fmt.Errorf("網站設定檔內容無效；原檔已保留")
		}
		if _, ok := s.items[p.ID]; ok {
			return nil, fmt.Errorf("網站設定 ID 重複")
		}
		s.items[p.ID] = p
	}
	return s, nil
}
func publicRules(rs []HeaderRule) []HeaderRule {
	rs = clone(rs)
	for i := range rs {
		r := &rs[i]
		r.HasValue = r.Value != ""
		r.Persisted = r.HasValue && (!r.Sensitive || r.Save)
		if r.Sensitive {
			r.Value = ""
		}
	}
	return rs
}
func publicProfile(p SiteProfile) SiteProfile {
	p = clone(p)
	p.DownloadHeaders = publicRules(p.DownloadHeaders)
	p.BrowserHeaders = publicRules(p.BrowserHeaders)
	p.QueryDefaults = publicRules(p.QueryDefaults)
	return p
}
func (s *ProfileStore) list() []SiteProfile {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []SiteProfile{}
	for _, p := range s.items {
		out = append(out, publicProfile(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func (s *ProfileStore) get(id string) (SiteProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.items[id]
	if !ok {
		return p, missing()
	}
	return clone(p), nil
}
func (s *ProfileStore) persist(items map[string]SiteProfile) error {
	dir := filepath.Dir(s.path)
	if e := secureDirectory(dir); e != nil {
		return fmt.Errorf("無法限制設定目錄存取權限")
	}
	d := profileDisk{SchemaVersion: 1, Profiles: []SiteProfile{}}
	for _, p := range items {
		d.Profiles = append(d.Profiles, p)
	}
	sort.Slice(d.Profiles, func(i, j int) bool { return d.Profiles[i].ID < d.Profiles[j].ID })
	b, e := json.MarshalIndent(d, "", "  ")
	if e != nil {
		return fmt.Errorf("設定編碼失敗")
	}
	f, e := os.CreateTemp(dir, ".profiles-*.json")
	if e != nil {
		return fmt.Errorf("設定寫入失敗")
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil || ce != nil {
		return fmt.Errorf("設定寫入失敗")
	}
	if e = replaceFile(name, s.path); e != nil {
		return fmt.Errorf("設定替換失敗；前一版本已保留")
	}
	return nil
}
func mergeStored(rs, old []HeaderRule) []HeaderRule {
	rs = clone(rs)
	for i := range rs {
		r := &rs[i]
		if r.Clear {
			r.Value = ""
		} else if r.Value == "" && r.HasValue {
			for _, o := range old {
				if strings.EqualFold(o.Name, r.Name) && o.Scope == r.Scope {
					r.Value = o.Value
					break
				}
			}
		}
		if r.Sensitive && !r.Save {
			r.Value = ""
		}
		r.Clear = false
		r.HasValue = false
		r.Persisted = false
	}
	return rs
}
func (s *ProfileStore) save(p SiteProfile, create bool) (SiteProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.items[p.ID]
	if create {
		p.ID = newID()
		p.Revision = 1
	} else {
		if old.ID == "" {
			return p, missing()
		}
		if p.Revision != old.Revision {
			return p, conflict("設定已更新，請重新載入")
		}
		p.Revision++
	}
	if e := validateProfile(&p); e != nil {
		return p, e
	}
	p.DownloadHeaders = mergeStored(p.DownloadHeaders, old.DownloadHeaders)
	p.BrowserHeaders = mergeStored(p.BrowserHeaders, old.BrowserHeaders)
	p.QueryDefaults = mergeStored(p.QueryDefaults, old.QueryDefaults)
	next := clone(s.items)
	next[p.ID] = p
	if e := s.persist(next); e != nil {
		return p, e
	}
	s.items = next
	return publicProfile(p), nil
}
func (s *ProfileStore) remove(id string, rev int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.items[id]
	if !ok {
		return missing()
	}
	if p.Revision != rev {
		return conflict("設定版本不符")
	}
	next := clone(s.items)
	delete(next, id)
	if e := s.persist(next); e != nil {
		return e
	}
	s.items = next
	return nil
}
func validURL(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" {
		return nil, bad("請輸入有效 HTTP/HTTPS 網址（不可含帳密）")
	}
	if _, e = canonicalHost(u.Host); e != nil {
		return nil, e
	}
	return u, nil
}
func canonicalHost(h string) (string, error) {
	u, e := url.Parse("http://" + h)
	if e != nil || u.Host != h || u.User != nil || u.RawQuery != "" || u.Path != "" || u.Fragment != "" {
		return "", bad("無效主機設定")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return "", bad("無效主機設定")
	}
	host, e = idna.Lookup.ToASCII(host)
	if e != nil {
		return "", bad("無效網域")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", bad("無效 port")
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return "", bad("無效 port")
			}
		}
		return net.JoinHostPort(host, p), nil
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]", nil
	}
	return host, nil
}
func hostMatch(pattern string, u *url.URL) bool {
	sub := strings.HasPrefix(pattern, "*.")
	pattern = strings.TrimPrefix(pattern, "*.")
	canon, e := canonicalHost(pattern)
	if e != nil {
		return false
	}
	// An omitted port matches this hostname on any port; explicit ports match effective ports.
	pu, _ := url.Parse("http://" + canon)
	host, _ := idna.Lookup.ToASCII(strings.TrimSuffix(strings.ToLower(u.Hostname()), "."))
	ph := pu.Hostname()
	if host != ph && (!sub || !strings.HasSuffix(host, "."+ph)) {
		return false
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return pu.Port() == "" || pu.Port() == port
}
func anyHost(patterns []string, u *url.URL) bool {
	for _, p := range patterns {
		if hostMatch(p, u) {
			return true
		}
	}
	return false
}
func sameOriginHost(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if u.Port() != "" {
			return u.Port()
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}
func sensitiveName(name string) bool {
	n := strings.ToLower(name)
	return n == "cookie" || n == "authorization" || n == "proxy-authorization" || strings.Contains(n, "token") || strings.Contains(n, "secret") || strings.Contains(n, "api-key")
}
func validateRules(rs []HeaderRule, query bool) error {
	if len(rs) > 32 {
		return bad("每類最多 32 筆參數")
	}
	seen := map[string]bool{}
	for i := range rs {
		r := &rs[i]
		r.Name = strings.TrimSpace(r.Name)
		key := strings.ToLower(r.Name) + "/" + r.Scope
		if seen[key] {
			return bad("參數名稱重複")
		}
		seen[key] = true
		if r.Name == "" || len(r.Name) > 128 || len(r.Value) > 8192 || strings.ContainsAny(r.Value, "\r\n\x00") {
			return bad("無效參數或換行字元")
		}
		if query {
			if r.Scope != "page" && r.Scope != "playlist" {
				return bad("Query 須指定 page 或 playlist")
			}
			r.Sensitive = true
		} else {
			for _, c := range r.Name {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
					return bad("無效 Header 名稱")
				}
			}
			switch strings.ToLower(r.Name) {
			case "host", "content-length", "connection", "transfer-encoding", "upgrade", "proxy-authorization", "proxy-connection", "te", "trailer":
				return bad("不允許連線控制 Header")
			}
			if strings.HasPrefix(strings.ToLower(r.Name), "sec-") {
				return bad("不允許 Sec-* Header")
			}
			r.Name = http.CanonicalHeaderKey(r.Name)
			r.Sensitive = r.Sensitive || sensitiveName(r.Name)
			if (r.Name == "Referer" || r.Name == "Origin") && r.Value != "" {
				u, e := validURL(r.Value)
				if e != nil {
					return e
				}
				if u.RawQuery != "" {
					r.Sensitive = true
				}
			}
		}
		for j, h := range r.TargetHosts {
			c, e := canonicalHost(strings.TrimPrefix(h, "*."))
			if e != nil {
				return e
			}
			if strings.HasPrefix(h, "*.") {
				c = "*." + c
			}
			if r.Sensitive && strings.HasPrefix(c, "*.") {
				return bad("敏感 Header 需精確目標主機")
			}
			r.TargetHosts[j] = c
		}
	}
	return nil
}
func validateProfile(p *SiteProfile) error {
	if strings.TrimSpace(p.Name) == "" || utf8.RuneCountInString(p.Name) > 80 {
		return bad("設定名稱必填且最多 80 字")
	}
	if p.Mode != "auto" && p.Mode != "page" && p.Mode != "m3u8" {
		return bad("無效下載模式")
	}
	if p.Mode == "page" && len(p.PageHosts) == 0 {
		return bad("網頁偵測模式需要網站網域")
	}
	if p.Mode == "m3u8" && len(p.MediaHosts) == 0 {
		return bad("直接 M3U8 模式需要媒體網域")
	}
	if p.Concurrency < 1 || p.Concurrency > 16 || p.WaitSeconds < 0 || p.WaitSeconds > 300 {
		return bad("並行數 1–16，等待時間 0–300")
	}
	if len(p.PageHosts)+len(p.MediaHosts) == 0 || len(p.PageHosts)+len(p.MediaHosts) > 64 {
		return bad("請設定 1–64 個網站或媒體主機")
	}
	for _, hosts := range [][]string{p.PageHosts, p.MediaHosts} {
		for i, h := range hosts {
			c, e := canonicalHost(strings.TrimPrefix(h, "*."))
			if e != nil {
				return e
			}
			if strings.HasPrefix(h, "*.") {
				c = "*." + c
			}
			hosts[i] = c
		}
	}
	if e := validateRules(p.DownloadHeaders, false); e != nil {
		return e
	}
	if e := validateRules(p.BrowserHeaders, false); e != nil {
		return e
	}
	for _, r := range p.BrowserHeaders {
		if r.Name == "Cookie" {
			return bad("偵測瀏覽器不支援自訂 Cookie；請使用下載 Cookie")
		}
	}
	return validateRules(p.QueryDefaults, true)
}
func applyRules(h http.Header, rs []HeaderRule, raw, initial string, cookieLimit bool) {
	u, e := url.Parse(raw)
	if e != nil {
		return
	}
	origin, _ := url.Parse(initial)
	for _, r := range rs {
		// Strip inherited configured headers first, including headers copied across redirects.
		h.Del(r.Name)
		allowed := sameOriginHost(u, origin)
		if len(r.TargetHosts) > 0 {
			allowed = anyHost(r.TargetHosts, u)
		}
		if cookieLimit && strings.EqualFold(r.Name, "Cookie") {
			allowed = allowed && sameOriginHost(u, origin)
		}
		if allowed && !r.Clear && r.Value != "" {
			h.Set(r.Name, r.Value)
		}
	}
}

type ruleTransport struct {
	rules   []HeaderRule
	initial string
}

func (t *ruleTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	applyRules(r.Header, t.rules, r.URL.String(), t.initial, true)
	return http.DefaultTransport.RoundTrip(r)
}
func profileClient(initial string, rules []HeaderRule) *http.Client {
	return &http.Client{Timeout: 30 * second, Transport: &ruleTransport{clone(rules), initial}}
}

type JobRequest struct {
	URL              string            `json:"url"`
	ProfileSelection string            `json:"profileSelection"`
	ProfileID        string            `json:"profileId"`
	ProfileRevision  int               `json:"profileRevision"`
	Mode             string            `json:"mode,omitempty"`
	OutputName       string            `json:"outputName"`
	WaitSeconds      *int              `json:"waitSeconds,omitempty"`
	Concurrency      *int              `json:"concurrency,omitempty"`
	KeepTs           *bool             `json:"keepTs,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	DownloadHeaders  []HeaderRule      `json:"downloadHeaders,omitempty"`
	BrowserHeaders   []HeaderRule      `json:"browserHeaders,omitempty"`
	QueryDefaults    []HeaderRule      `json:"queryDefaults,omitempty"`
}
type Resolved struct {
	URL, Mode, OutputName, ProfileID, ProfileName  string
	ProfileRevision, WaitSeconds, Concurrency      int
	KeepTs                                         bool
	DownloadHeaders, BrowserHeaders, QueryDefaults []HeaderRule
}

func mergeRules(base, over []HeaderRule) []HeaderRule {
	out := clone(base)
	for _, r := range over {
		found := false
		for i, b := range out {
			if strings.EqualFold(b.Name, r.Name) && b.Scope == r.Scope {
				if len(r.TargetHosts) == 0 {
					r.TargetHosts = b.TargetHosts
				}
				r.Sensitive = r.Sensitive || b.Sensitive
				out[i] = r
				found = true
				break
			}
		}
		if !found {
			out = append(out, r)
		}
	}
	return out
}
func safeName(name string) (string, error) {
	if name == "" {
		name = "output.mp4"
	}
	if utf8.RuneCountInString(name) > 120 || strings.Contains(name, "..") || strings.ContainsAny(name, "/\\:<>\"|?*\x00\r\n") || strings.Trim(name, " .") != name {
		return "", bad("無效輸出檔名")
	}
	base := strings.TrimSuffix(name, filepath.Ext(name))
	for _, r := range name {
		if r < 32 || r == 127 {
			return "", bad("無效輸出檔名")
		}
	}
	n := strings.ToUpper(strings.SplitN(base, ".", 2)[0])
	if n == "CON" || n == "PRN" || n == "AUX" || n == "NUL" || (len(n) == 4 && (strings.HasPrefix(n, "COM") || strings.HasPrefix(n, "LPT")) && n[3] >= '0' && n[3] <= '9') {
		return "", bad("Windows 保留檔名")
	}
	if base == "" {
		return "", bad("無效輸出檔名")
	}
	return base + ".mp4", nil
}
func (s *ProfileStore) resolve(r JobRequest, preview bool) (Resolved, []SiteProfile, error) {
	o := Resolved{URL: r.URL, Mode: "auto", WaitSeconds: 6, Concurrency: 6}
	u, e := validURL(r.URL)
	if e != nil {
		return o, nil, e
	}
	o.OutputName, e = safeName(r.OutputName)
	if e != nil {
		return o, nil, e
	}
	selection := r.ProfileSelection
	if selection == "" {
		selection = "none"
	}
	candidates := []SiteProfile{}
	if selection != "none" {
		s.mu.Lock()
		for _, p := range s.items {
			if p.Enabled && (anyHost(p.PageHosts, u) || anyHost(p.MediaHosts, u)) {
				candidates = append(candidates, clone(p))
			}
		}
		s.mu.Unlock()
		var p SiteProfile
		switch selection {
		case "auto":
			if len(candidates) > 1 {
				return o, candidates, conflict("多個網站設定符合，請手動選取")
			}
			if len(candidates) == 0 {
				r.ProfileSelection = "none"
				return s.resolve(r, preview)
			}
			p = candidates[0]
			if !preview && (r.ProfileID != p.ID || r.ProfileRevision != p.Revision) {
				return o, nil, conflict("比對結果已更新，請重新預覽")
			}
		case "explicit":
			p, e = s.get(r.ProfileID)
			if e != nil {
				return o, nil, e
			}
			if !p.Enabled {
				return o, nil, conflict("網站設定已停用")
			}
			if !anyHost(p.PageHosts, u) && !anyHost(p.MediaHosts, u) {
				return o, nil, bad("網址不符合選定網站設定")
			}
		default:
			return o, nil, bad("無效網站設定選擇")
		}
		if !preview && r.ProfileRevision != p.Revision {
			return o, nil, conflict("網站設定版本已更新")
		}
		if p.Mode == "m3u8" && anyHost(p.PageHosts, u) && !anyHost(p.MediaHosts, u) {
			return o, nil, bad("此設定僅接受直接 M3U8 URL，請提供媒體網址")
		}
		if p.Mode == "page" && !anyHost(p.PageHosts, u) {
			return o, nil, bad("此設定需要網站網頁網址，請提供頁面網址")
		}
		o.Mode = p.Mode
		o.ProfileID = p.ID
		o.ProfileName = p.Name
		o.ProfileRevision = p.Revision
		o.WaitSeconds = p.WaitSeconds
		o.Concurrency = p.Concurrency
		o.KeepTs = p.KeepTs
		o.DownloadHeaders = clone(p.DownloadHeaders)
		o.BrowserHeaders = clone(p.BrowserHeaders)
		o.QueryDefaults = clone(p.QueryDefaults)
	}
	if r.Mode != "" {
		if o.ProfileID != "" && r.Mode != o.Mode {
			return o, nil, conflict("模式與網站設定不符")
		}
		o.Mode = r.Mode
	}
	if o.Mode != "auto" && o.Mode != "page" && o.Mode != "m3u8" {
		return o, nil, bad("無效下載模式")
	}
	if r.Concurrency != nil {
		o.Concurrency = *r.Concurrency
	}
	if r.WaitSeconds != nil {
		o.WaitSeconds = *r.WaitSeconds
	}
	if r.KeepTs != nil {
		o.KeepTs = *r.KeepTs
	}
	if o.Concurrency < 1 || o.Concurrency > 16 || o.WaitSeconds < 0 || o.WaitSeconds > 300 {
		return o, nil, bad("並行數 1–16，等待時間 0–300")
	}
	for k, v := range r.Headers {
		switch k {
		case "referer":
			k = "Referer"
		case "userAgent":
			k = "User-Agent"
		case "cookie":
			k = "Cookie"
		default:
			return o, nil, bad("請使用自訂 Header 欄位")
		}
		r.DownloadHeaders = append(r.DownloadHeaders, HeaderRule{Name: k, Value: v})
	}
	if e = validateRules(r.DownloadHeaders, false); e != nil {
		return o, nil, e
	}
	if e = validateRules(r.BrowserHeaders, false); e != nil {
		return o, nil, e
	}
	if e = validateRules(r.QueryDefaults, true); e != nil {
		return o, nil, e
	}
	for _, h := range r.BrowserHeaders {
		if h.Name == "Cookie" {
			return o, nil, bad("偵測瀏覽器不支援自訂 Cookie")
		}
	}
	o.DownloadHeaders = mergeRules(o.DownloadHeaders, r.DownloadHeaders)
	o.BrowserHeaders = mergeRules(o.BrowserHeaders, r.BrowserHeaders)
	o.QueryDefaults = mergeRules(o.QueryDefaults, r.QueryDefaults)
	if e = validateRules(o.DownloadHeaders, false); e != nil {
		return o, nil, e
	}
	if e = validateRules(o.BrowserHeaders, false); e != nil {
		return o, nil, e
	}
	if e = validateRules(o.QueryDefaults, true); e != nil {
		return o, nil, e
	}
	scope := "playlist"
	if isPage(o) {
		scope = "page"
	}
	u = addQueryDefaults(u, o.QueryDefaults, scope)
	o.URL = u.String()
	return o, nil, nil
}
func isPage(o Resolved) bool {
	u, _ := url.Parse(o.URL)
	return o.Mode == "page" || (o.Mode == "auto" && !strings.HasSuffix(strings.ToLower(u.Path), ".m3u8"))
}
func templates() []SiteProfile {
	return []SiteProfile{
		{Name: "MissAV（直接 M3U8）", Enabled: true, PageHosts: []string{"missav.ai"}, MediaHosts: []string{"surrit.com"}, Mode: "m3u8", WaitSeconds: 6, Concurrency: 6, DownloadHeaders: []HeaderRule{{Name: "Referer", Value: "https://missav.ai/", TargetHosts: []string{"surrit.com:443"}}, {Name: "User-Agent", Value: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36", TargetHosts: []string{"surrit.com:443"}}}},
		{Name: "HL365（網頁偵測）", Enabled: true, PageHosts: []string{"hl365.com"}, MediaHosts: []string{}, Mode: "page", WaitSeconds: 6, Concurrency: 6},
	}
}

// Preserve signed query bytes and ordering; only append missing defaults.
func addQueryDefaults(u *url.URL, rs []HeaderRule, scope string) *url.URL {
	c := *u
	q := c.Query()
	for _, r := range rs {
		if r.Scope == scope && !r.Clear && r.Value != "" {
			if _, ok := q[r.Name]; !ok {
				extra := url.Values{r.Name: []string{r.Value}}.Encode()
				if c.RawQuery != "" {
					c.RawQuery += "&"
				}
				c.RawQuery += extra
				q.Set(r.Name, r.Value)
			}
		}
	}
	return &c
}
