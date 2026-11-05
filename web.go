package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	SherryServer "github.com/asccclass/sherryserver"
)

//go:embed web/*
var webFiles embed.FS

type WebApp struct {
	profiles        *ProfileStore
	jobs            *JobManager
	allowedHost     string
	dependencyCheck func() map[string]bool
}

func (a *WebApp) dependencies() map[string]bool {
	if a.dependencyCheck != nil {
		return a.dependencyCheck()
	}
	return dependencies()
}
func (a *WebApp) checkDependencies(o Resolved) error {
	d := a.dependencies()
	if !d["ffmpeg"] {
		return &AppError{503, "DEPENDENCY_MISSING", "找不到 FFmpeg，請安裝並加入 PATH"}
	}
	if isPage(o) && !d["chrome"] {
		return &AppError{503, "DEPENDENCY_MISSING", "網頁偵測需要 Chrome／Chromium"}
	}
	return nil
}

func runWeb(listen, root, profileFile string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runWebContext(ctx, listen, root, profileFile)
}
func runWebContext(ctx context.Context, listen, root, profileFile string) error {
	host, _, e := net.SplitHostPort(listen)
	if e != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("-listen 僅接受 loopback IP:port，例如 127.0.0.1:8080")
	}
	srv, e := SherryServer.NewServer(listen, "", "")
	if e != nil {
		return e
	}
	defer srv.Logger.Sync()
	if strings.EqualFold(os.Getenv("TLSAutoEnabled"), "true") || os.Getenv("sslCertification") != "" || os.Getenv("sslKeyfile") != "" {
		return fmt.Errorf("本機 Web 模式請停用 TLS 設定")
	}
	profiles, e := openProfiles(profileFile)
	if e != nil {
		return e
	}
	manager, e := newManager(root)
	if e != nil {
		return e
	}
	ln, e := net.Listen("tcp", listen)
	if e != nil {
		manager.close()
		return fmt.Errorf("無法監聽 %s: %w", listen, e)
	}
	app := &WebApp{profiles: profiles, jobs: manager, allowedHost: ln.Addr().String()}
	srv.Server.Handler = app.handler()
	srv.Server.ReadHeaderTimeout = 5 * second
	srv.Server.MaxHeaderBytes = 32 * 1024
	done := make(chan error, 1)
	go func() { done <- srv.Server.Serve(ln) }()
	fmt.Printf("Web 介面：http://%s\n", ln.Addr())
	select {
	case <-ctx.Done():
	case e = <-done:
		if errors.Is(e, http.ErrServerClosed) {
			e = nil
		}
	}
	shutdown, timeout := context.WithTimeout(context.Background(), 15*second)
	defer timeout()
	cleaned := make(chan struct{})
	go func() { manager.close(); close(cleaned) }()
	se := srv.Server.Shutdown(shutdown)
	if se != nil {
		srv.Server.Close()
	}
	select {
	case <-cleaned:
	case <-shutdown.Done():
		srv.Server.Close()
		return fmt.Errorf("關閉逾時")
	}
	if e != nil {
		return e
	}
	return se
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, e error) {
	a := &AppError{500, "INTERNAL_ERROR", "處理失敗，請檢查設定目錄及存取權限"}
	var known *AppError
	if errors.As(e, &known) {
		a = known
	}
	writeJSON(w, a.Status, map[string]any{"error": map[string]string{"code": a.Code, "message": a.Message}})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" {
		return bad("請使用 application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return bad("JSON 無效、欄位不支援或超過 64 KiB")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return bad("僅接受一個 JSON 物件")
	}
	return nil
}
func (a *WebApp) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; style-src 'self'; script-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		if r.Host != a.allowedHost {
			writeError(w, &AppError{403, "FORBIDDEN", "Host 不允許"})
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" {
			u, e := url.Parse(origin)
			if e != nil || u.Scheme != "http" || u.Host != a.allowedHost || u.Path != "" || u.User != nil {
				writeError(w, &AppError{403, "FORBIDDEN", "來源不允許"})
				return
			}
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeError(w, &AppError{403, "FORBIDDEN", "拒絕跨站請求"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (a *WebApp) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"version": "1.0.0", "dependencies": a.dependencies(), "limits": map[string]int{"queue": 20, "history": 100, "websockets": 16}, "profileStorage": "local-plaintext"})
	})
	mux.HandleFunc("GET /api/v1/site-profile-templates", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, templates()) })
	mux.HandleFunc("GET /api/v1/site-profiles", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.profiles.list()) })
	mux.HandleFunc("GET /api/v1/site-profiles/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, e := a.profiles.get(r.PathValue("id"))
		if e != nil {
			writeError(w, e)
			return
		}
		writeJSON(w, 200, publicProfile(p))
	})
	mux.HandleFunc("POST /api/v1/site-profiles", func(w http.ResponseWriter, r *http.Request) { a.saveProfile(w, r, true) })
	mux.HandleFunc("PUT /api/v1/site-profiles/{id}", func(w http.ResponseWriter, r *http.Request) { a.saveProfile(w, r, false) })
	mux.HandleFunc("DELETE /api/v1/site-profiles/{id}", func(w http.ResponseWriter, r *http.Request) {
		rev, _ := strconv.Atoi(strings.Trim(r.Header.Get("If-Match"), "\""))
		if e := a.profiles.remove(r.PathValue("id"), rev); e != nil {
			writeError(w, e)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /api/v1/site-profiles/resolve", func(w http.ResponseWriter, r *http.Request) {
		var req JobRequest
		if e := decode(w, r, &req); e != nil {
			writeError(w, e)
			return
		}
		o, cs, e := a.profiles.resolve(req, true)
		if e != nil {
			status := 400
			code := "INVALID_INPUT"
			var ae *AppError
			if errors.As(e, &ae) {
				status = ae.Status
				code = ae.Code
			}
			pub := []SiteProfile{}
			for _, p := range cs {
				pub = append(pub, publicProfile(p))
			}
			writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": e.Error()}, "candidates": pub})
			return
		}
		writeJSON(w, 200, preview(o))
	})
	mux.HandleFunc("POST /api/v1/jobs", func(w http.ResponseWriter, r *http.Request) { a.createJob(w, r) })
	mux.HandleFunc("GET /api/v1/jobs", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.jobs.snapshot()) })
	mux.HandleFunc("GET /api/v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, e := a.jobs.get(r.PathValue("id"))
		if e != nil {
			writeError(w, e)
			return
		}
		writeJSON(w, 200, j)
	})
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		j, e := a.jobs.cancelJob(r.PathValue("id"))
		if e != nil {
			writeError(w, e)
			return
		}
		writeJSON(w, 202, j)
	})
	mux.HandleFunc("POST /api/v1/jobs/{id}/retry", func(w http.ResponseWriter, r *http.Request) { a.retryJob(w, r) })
	mux.HandleFunc("DELETE /api/v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := a.jobs.deleteJob(r.PathValue("id")); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/jobs/{id}/videos/{videoId}/file", a.file)
	mux.HandleFunc("GET /ws", a.jobs.serveWS)
	assets, _ := fs.Sub(webFiles, "web")
	files := http.FileServer(http.FS(assets))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/app.js", "/style.css":
			files.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	return a.guard(mux)
}
func preview(o Resolved) map[string]any {
	return map[string]any{"profileId": o.ProfileID, "profileName": o.ProfileName, "profileRevision": o.ProfileRevision, "source": maskedURL(o.URL), "mode": o.Mode, "waitSeconds": o.WaitSeconds, "concurrency": o.Concurrency, "keepTs": o.KeepTs, "outputName": o.OutputName, "downloadHeaders": publicRules(o.DownloadHeaders), "browserHeaders": publicRules(o.BrowserHeaders), "queryDefaults": publicRules(o.QueryDefaults)}
}
func (a *WebApp) saveProfile(w http.ResponseWriter, r *http.Request, create bool) {
	var p SiteProfile
	if e := decode(w, r, &p); e != nil {
		writeError(w, e)
		return
	}
	if !create {
		p.ID = r.PathValue("id")
	}
	p, e := a.profiles.save(p, create)
	if e != nil {
		writeError(w, e)
		return
	}
	status := 200
	if create {
		status = 201
		w.Header().Set("Location", "/api/v1/site-profiles/"+p.ID)
	}
	writeJSON(w, status, p)
}
func idempotency(r *http.Request) (string, error) {
	k := r.Header.Get("Idempotency-Key")
	if len(k) > 128 {
		return "", bad("Idempotency-Key 過長")
	}
	return k, nil
}
func (a *WebApp) createJob(w http.ResponseWriter, r *http.Request) {
	var req JobRequest
	if e := decode(w, r, &req); e != nil {
		writeError(w, e)
		return
	}
	key, e := idempotency(r)
	if e != nil {
		writeError(w, e)
		return
	}
	hash := requestHash(req)
	if j, ok, e := a.jobs.existing(key, hash); ok {
		if e != nil {
			writeError(w, e)
		} else {
			writeJSON(w, 202, j)
		}
		return
	}
	o, _, e := a.profiles.resolve(req, false)
	if e == nil {
		e = a.checkDependencies(o)
	}
	if e != nil {
		writeError(w, e)
		return
	}
	j, e := a.jobs.add(o, key, hash, "", nil)
	if e != nil {
		writeError(w, e)
		return
	}
	w.Header().Set("Location", "/api/v1/jobs/"+j.ID)
	writeJSON(w, 202, j)
}
func (a *WebApp) retryJob(w http.ResponseWriter, r *http.Request) {
	var req JobRequest
	if e := decode(w, r, &req); e != nil {
		writeError(w, e)
		return
	}
	id := r.PathValue("id")
	key, e := idempotency(r)
	if e != nil {
		writeError(w, e)
		return
	}
	hash := requestHash(struct {
		ID string
		R  JobRequest
	}{id, req})
	if j, ok, e := a.jobs.existing(key, hash); ok {
		if e != nil {
			writeError(w, e)
		} else {
			writeJSON(w, 202, j)
		}
		return
	}
	o, seeds, e := a.jobs.retryInput(id)
	if e != nil {
		writeError(w, e)
		return
	}
	// Reuse the frozen non-sensitive defaults, unless the user explicitly selects
	// a current profile revision. Sensitive values were wiped at terminal state.
	if req.ProfileSelection != "" && req.ProfileSelection != "none" {
		req.URL = o.URL
		req.OutputName = o.OutputName
		o, _, e = a.profiles.resolve(req, false)
	} else {
		req.URL = o.URL
		req.Mode = o.Mode
		req.OutputName = o.OutputName
		req.ProfileSelection = "none"
		if req.WaitSeconds == nil {
			req.WaitSeconds = &o.WaitSeconds
		}
		if req.Concurrency == nil {
			req.Concurrency = &o.Concurrency
		}
		if req.KeepTs == nil {
			req.KeepTs = &o.KeepTs
		}
		req.DownloadHeaders = mergeRules(o.DownloadHeaders, req.DownloadHeaders)
		req.BrowserHeaders = mergeRules(o.BrowserHeaders, req.BrowserHeaders)
		req.QueryDefaults = mergeRules(o.QueryDefaults, req.QueryDefaults)
		old := o
		o, _, e = a.profiles.resolve(req, false)
		o.ProfileID = old.ProfileID
		o.ProfileName = old.ProfileName
		o.ProfileRevision = old.ProfileRevision
	}
	dep := o
	if len(seeds) > 0 {
		dep.Mode = "m3u8"
	}
	if e == nil {
		e = a.checkDependencies(dep)
	}
	if e != nil {
		writeError(w, e)
		return
	}
	j, e := a.jobs.add(o, key, hash, id, seeds)
	if e != nil {
		writeError(w, e)
		return
	}
	w.Header().Set("Location", "/api/v1/jobs/"+j.ID)
	writeJSON(w, 202, j)
}
func (a *WebApp) file(w http.ResponseWriter, r *http.Request) {
	j, e := a.jobs.get(r.PathValue("id"))
	if e != nil {
		writeError(w, e)
		return
	}
	var video *Video
	for i := range j.Videos {
		if j.Videos[i].ID == r.PathValue("videoId") {
			video = &j.Videos[i]
			break
		}
	}
	if video == nil {
		writeError(w, missing())
		return
	}
	if !video.Ready {
		writeError(w, conflict("成果尚未就緒"))
		return
	}
	root, e := os.OpenRoot(a.jobs.root)
	if e != nil {
		writeError(w, missing())
		return
	}
	defer root.Close()
	f, e := root.Open(j.ID + "/" + video.OutputName)
	if e != nil {
		writeError(w, missing())
		return
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		writeError(w, missing())
		return
	}
	http.NewResponseController(w).SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": video.OutputName}))
	http.ServeContent(w, r, video.OutputName, st.ModTime(), f)
}
