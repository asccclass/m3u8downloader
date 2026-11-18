package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const second = time.Second

type LogEntry struct {
	Time    string `json:"time"`
	Message string `json:"message"`
}
type Video struct {
	ID                string  `json:"id"`
	Index             int     `json:"index"`
	Source            string  `json:"source"`
	OutputName        string  `json:"outputName"`
	State             string  `json:"state"`
	Phase             string  `json:"phase"`
	SegmentsTotal     int     `json:"segmentsTotal"`
	SegmentsSucceeded int     `json:"segmentsSucceeded"`
	SegmentsFailed    int     `json:"segmentsFailed"`
	SegmentsProcessed int     `json:"segmentsProcessed"`
	Percent           float64 `json:"segmentProgressPercent"`
	Bytes             int64   `json:"bytesReceived"`
	Speed             float64 `json:"bytesPerSecond"`
	Size              int64   `json:"size"`
	TSName            string  `json:"tsName,omitempty"`
	Ready             bool    `json:"ready"`
	Error             string  `json:"error,omitempty"`
	Revision          int     `json:"revision"`
	source            string
}
type Job struct {
	ID              string         `json:"id"`
	RetryOf         string         `json:"retryOf,omitempty"`
	ProfileID       string         `json:"profileId,omitempty"`
	ProfileName     string         `json:"profileName,omitempty"`
	ProfileRevision int            `json:"profileRevision,omitempty"`
	Source          string         `json:"source"`
	OutputName      string         `json:"outputName"`
	Mode            string         `json:"mode"`
	Options         map[string]any `json:"options"`
	State           string         `json:"state"`
	Phase           string         `json:"phase"`
	CreatedAt       time.Time      `json:"createdAt"`
	StartedAt       *time.Time     `json:"startedAt,omitempty"`
	FinishedAt      *time.Time     `json:"finishedAt,omitempty"`
	Videos          []Video        `json:"videos"`
	Logs            []LogEntry     `json:"logs"`
	Succeeded       int            `json:"succeeded"`
	Failed          int            `json:"failed"`
	Revision        int            `json:"revision"`
	Error           string         `json:"error,omitempty"`
	input           Resolved
	cancel          context.CancelFunc
}
type idemEntry struct {
	Hash string
	ID   string
}
type JobManager struct {
	mu       sync.Mutex
	jobs     map[string]*Job
	order    []string
	queue    []string
	idem     map[string]idemEntry
	root     string
	hub      *Hub
	revision int
	ctx      context.Context
	cancel   context.CancelFunc
	wake     chan struct{}
	done     chan struct{}
	closing  bool
}

func newManager(root string) (*JobManager, error) {
	abs, e := filepath.Abs(root)
	if e != nil {
		return nil, e
	}
	if e = os.MkdirAll(abs, 0700); e != nil {
		return nil, e
	}
	f, e := os.CreateTemp(abs, ".write-check-")
	if e != nil {
		return nil, fmt.Errorf("輸出目錄不可寫入")
	}
	f.Close()
	os.Remove(f.Name())
	ctx, cancel := context.WithCancel(context.Background())
	m := &JobManager{jobs: map[string]*Job{}, idem: map[string]idemEntry{}, root: abs, hub: newHub(), ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go m.worker()
	return m, nil
}
func maskedURL(raw string) string {
	u, e := url.Parse(raw)
	if e != nil {
		return "（來源已隱藏）"
	}
	return u.Scheme + "://" + u.Host + "/…"
}
func terminal(s string) bool {
	return s == "succeeded" || s == "failed" || s == "partial_failed" || s == "cancelled"
}
func (m *JobManager) snapshotLocked() map[string]any {
	js := []Job{}
	for i := len(m.order) - 1; i >= 0; i-- {
		if j := m.jobs[m.order[i]]; j != nil {
			js = append(js, clone(*j))
		}
	}
	return map[string]any{"jobs": js, "revision": m.revision}
}
func (m *JobManager) snapshot() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}
func (m *JobManager) get(id string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil {
		return Job{}, missing()
	}
	return clone(*j), nil
}

// Delete only terminal jobs. Missing files/directories are already clean, so
// moving a downloaded MP4 elsewhere does not prevent removing its old folder.
func (m *JobManager) deleteJob(id string) error {
	if len(id) != 32 {
		return bad("無效任務 ID")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return bad("無效任務 ID")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil {
		return nil
	} // Repeated deletes are idempotent.
	if !terminal(j.State) {
		return conflict("請先取消任務，待處理結束後再刪除")
	}
	if err := removeJobDirectory(m.root, id); err != nil {
		return err
	}
	delete(m.jobs, id)
	remaining := m.order[:0]
	for _, current := range m.order {
		if current != id {
			remaining = append(remaining, current)
		}
	}
	m.order = remaining
	for key, entry := range m.idem {
		if entry.ID == id {
			delete(m.idem, key)
		}
	}
	m.revision++
	m.hub.broadcast("job.removed", id, "", map[string]any{"id": id, "revision": m.revision})
	return nil
}

func removeJobDirectory(outputRoot, id string) error {
	failed := func() error {
		return &AppError{500, "DELETE_FAILED", "無法刪除輸出目錄，請確認檔案未被其他程式使用及目錄權限"}
	}
	// Verify the computed absolute target, then use an os.Root handle so a
	// concurrent directory/junction replacement cannot escape the output root.
	absRoot, err := filepath.Abs(outputRoot)
	if err != nil {
		return failed()
	}
	target := filepath.Join(absRoot, id)
	rel, err := filepath.Rel(absRoot, target)
	if err != nil || rel == "." || rel != id || !filepath.IsLocal(rel) {
		return conflict("任務目錄位置異常，未刪除")
	}
	root, err := os.OpenRoot(absRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return failed()
	}
	defer root.Close()
	info, err := root.Lstat(id)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return failed()
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// Unlink the entry itself, never recursively delete a link's target.
		err = root.Remove(id)
	} else {
		resolvedRoot, e := filepath.EvalSymlinks(absRoot)
		if e != nil {
			return failed()
		}
		resolvedTarget, e := filepath.EvalSymlinks(target)
		if os.IsNotExist(e) {
			return nil
		}
		if e != nil {
			return failed()
		}
		resolvedRel, e := filepath.Rel(resolvedRoot, resolvedTarget)
		if e != nil || resolvedRel != id {
			return conflict("任務目錄位置異常，未刪除")
		}
		err = root.RemoveAll(id)
	}
	if err != nil && !os.IsNotExist(err) {
		return failed()
	}
	return nil
}
func (m *JobManager) publish(j *Job, kind string) {
	m.revision++
	j.Revision++
	m.hub.broadcast(kind, j.ID, "", clone(*j))
}
func (m *JobManager) log(j *Job, msg string) {
	j.Logs = append(j.Logs, LogEntry{time.Now().Format(time.RFC3339), msg})
	if len(j.Logs) > 500 {
		j.Logs = j.Logs[len(j.Logs)-500:]
	}
}
func requestHash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (m *JobManager) existing(key, hash string) (Job, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if key != "" {
		if x, ok := m.idem[key]; ok {
			if x.Hash != hash {
				return Job{}, true, conflict("相同 Idempotency-Key 的內容不同")
			}
			if j := m.jobs[x.ID]; j != nil {
				return clone(*j), true, nil
			}
		}
	}
	return Job{}, false, nil
}
func (m *JobManager) add(o Resolved, key, hash, retry string, seeds []Video) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return Job{}, &AppError{503, "STOPPING", "服務正在關閉"}
	}
	if key != "" {
		if x, ok := m.idem[key]; ok {
			if x.Hash != hash {
				return Job{}, conflict("相同 Idempotency-Key 的內容不同")
			}
			if j := m.jobs[x.ID]; j != nil {
				return clone(*j), nil
			}
		}
	}
	waiting := 0
	for _, id := range m.queue {
		if m.jobs[id] != nil && m.jobs[id].State == "queued" {
			waiting++
		}
	}
	if waiting >= 20 {
		return Job{}, &AppError{429, "QUEUE_FULL", "等待佇列已滿（20）"}
	}
	j := &Job{ID: newID(), RetryOf: retry, Source: maskedURL(o.URL), OutputName: o.OutputName, Mode: o.Mode, ProfileID: o.ProfileID, ProfileName: o.ProfileName, ProfileRevision: o.ProfileRevision, State: "queued", Phase: "queued", CreatedAt: time.Now(), Videos: []Video{}, Logs: []LogEntry{}, input: clone(o), Options: map[string]any{"waitSeconds": o.WaitSeconds, "concurrency": o.Concurrency, "keepTs": o.KeepTs, "downloadHeaders": publicRules(o.DownloadHeaders), "browserHeaders": publicRules(o.BrowserHeaders)}}
	// Job options show header names and scopes, never header values, even for custom headers.
	for _, k := range []string{"downloadHeaders", "browserHeaders"} {
		rs := j.Options[k].([]HeaderRule)
		for i := range rs {
			rs[i].HasValue = rs[i].Value != "" || rs[i].HasValue
			rs[i].Value = ""
		}
		j.Options[k] = rs
	}
	for _, v := range seeds {
		j.Videos = append(j.Videos, Video{ID: newID(), Index: v.Index, Source: maskedURL(v.source), OutputName: v.OutputName, State: "queued", Phase: "queued", source: v.source})
	}
	m.jobs[j.ID] = j
	m.order = append(m.order, j.ID)
	m.queue = append(m.queue, j.ID)
	if key != "" {
		m.idem[key] = idemEntry{hash, j.ID}
	}
	m.log(j, "任務已加入等待佇列")
	m.publish(j, "job.created")
	m.prune()
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return clone(*j), nil
}
func (m *JobManager) prune() {
	count := 0
	for _, id := range m.order {
		if terminal(m.jobs[id].State) {
			count++
		}
	}
	if count <= 100 {
		return
	}
	kept := []string{}
	for _, id := range m.order {
		if count > 100 && terminal(m.jobs[id].State) {
			delete(m.jobs, id)
			for k, v := range m.idem {
				if v.ID == id {
					delete(m.idem, k)
				}
			}
			count--
			m.revision++
			m.hub.broadcast("job.removed", id, "", map[string]any{"id": id, "revision": m.revision})
		} else {
			kept = append(kept, id)
		}
	}
	m.order = kept
}
func (m *JobManager) cancelJob(id string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil {
		return Job{}, missing()
	}
	if j.State == "cancelled" || j.State == "cancelling" {
		return clone(*j), nil
	}
	if terminal(j.State) {
		return Job{}, conflict("任務已結束")
	}
	if j.State == "queued" {
		j.State = "cancelled"
		j.Phase = "cancelled"
		for i := range j.Videos {
			j.Videos[i].State = "cancelled"
			j.Videos[i].Phase = "cancelled"
		}
		now := time.Now()
		j.FinishedAt = &now
		clearSecrets(&j.input)
		m.publish(j, "job.finished")
		pending := m.queue[:0]
		for _, qid := range m.queue {
			if qid != id {
				pending = append(pending, qid)
			}
		}
		m.queue = pending
		m.prune()
	} else {
		j.State = "cancelling"
		if j.cancel != nil {
			j.cancel()
		}
		m.publish(j, "job.updated")
	}
	return clone(*j), nil
}
func clearSecrets(o *Resolved) {
	for _, rs := range [][]HeaderRule{o.DownloadHeaders, o.BrowserHeaders, o.QueryDefaults} {
		for i := range rs {
			if rs[i].Sensitive {
				rs[i].Value = ""
			}
		}
	}
}
func (m *JobManager) retryInput(id string) (Resolved, []Video, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil {
		return Resolved{}, nil, missing()
	}
	if j.State != "failed" && j.State != "partial_failed" {
		return Resolved{}, nil, conflict("只有失敗或部分失敗任務可重試")
	}
	o := clone(j.input)
	vs := []Video{}
	for _, v := range j.Videos {
		if v.State == "failed" {
			c := v
			vs = append(vs, c)
		}
	}
	return o, vs, nil
}
func (m *JobManager) worker() {
	defer close(m.done)
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.wake:
		}
		for {
			m.mu.Lock()
			var j *Job
			for len(m.queue) > 0 {
				id := m.queue[0]
				m.queue = m.queue[1:]
				if x := m.jobs[id]; x != nil && x.State == "queued" {
					j = x
					break
				}
			}
			if j == nil || m.closing {
				m.mu.Unlock()
				break
			}
			ctx, cancel := context.WithCancel(m.ctx)
			j.cancel = cancel
			j.State = "running"
			now := time.Now()
			j.StartedAt = &now
			m.publish(j, "job.updated")
			m.mu.Unlock()
			m.execute(ctx, j.ID)
			cancel()
		}
	}
}
func (m *JobManager) execute(ctx context.Context, id string) {
	m.mu.Lock()
	j := m.jobs[id]
	o := clone(j.input)
	needDiscovery := len(j.Videos) == 0
	m.mu.Unlock()
	var setupErr error
	if needDiscovery {
		urls := []string{o.URL}
		if isPage(o) {
			m.mu.Lock()
			j.Phase = "discovering"
			m.log(j, "正在偵測網頁內的 M3U8")
			m.publish(j, "job.updated")
			m.mu.Unlock()
			urls, setupErr = discoverContext(ctx, o.URL, time.Duration(o.WaitSeconds)*second, o.BrowserHeaders, func(string) { m.mu.Lock(); m.log(j, "捕獲 M3U8 網址"); m.publish(j, "job.updated"); m.mu.Unlock() })
		}
		if setupErr == nil && len(urls) == 0 {
			setupErr = fmt.Errorf("等待期間未捕獲任何 M3U8 網址")
		}
		if setupErr == nil {
			m.mu.Lock()
			for i, u := range urls {
				j.Videos = append(j.Videos, Video{ID: newID(), Index: i + 1, Source: maskedURL(u), OutputName: outputPath(o.OutputName, i, len(urls)), State: "queued", Phase: "queued", source: u})
			}
			m.publish(j, "job.updated")
			m.mu.Unlock()
		}
	}
	if setupErr == nil {
		for i := range j.Videos {
			if ctx.Err() != nil {
				break
			}
			m.mu.Lock()
			v := &j.Videos[i]
			v.State = "running"
			source := v.source
			name := v.OutputName
			m.mu.Unlock()
			// Playlist query defaults are applied separately to discovered media URLs.
			u, _ := url.Parse(source)
			source = addQueryDefaults(u, o.QueryDefaults, "playlist").String()
			var lastEmit time.Time
			lastPhase := ""
			type sample struct {
				t time.Time
				b int64
			}
			samples := []sample{}
			err := downloadContext(ctx, source, filepath.Join(m.root, id, name), o.Concurrency, o.KeepTs, profileClient(source, o.DownloadHeaders), func(p Progress) {
				now := time.Now()
				m.mu.Lock()
				defer m.mu.Unlock()
				v := &j.Videos[i]
				j.Phase = p.Phase
				v.Phase = p.Phase
				v.TSName = p.TSName
				v.SegmentsTotal = p.Total
				v.SegmentsSucceeded = p.Succeeded
				v.SegmentsFailed = p.Failed
				v.SegmentsProcessed = p.Succeeded + p.Failed
				v.Bytes = p.Bytes
				v.Percent = 0
				if p.Total > 0 {
					v.Percent = 100 * float64(v.SegmentsProcessed) / float64(p.Total)
				}
				samples = append(samples, sample{now, p.Bytes})
				for len(samples) > 1 && now.Sub(samples[1].t) >= 3*second {
					samples = samples[1:]
				}
				if elapsed := now.Sub(samples[0].t).Seconds(); elapsed > 0 {
					v.Speed = float64(p.Bytes-samples[0].b) / elapsed
				}
				if p.Phase != "downloading" {
					v.Speed = 0
				}
				changed := p.Phase != lastPhase
				if changed {
					m.log(j, "處理階段："+p.Phase)
				}
				if changed || now.Sub(lastEmit) >= 250*time.Millisecond {
					v.Revision++
					m.publish(j, "job.updated")
					lastEmit = now
					lastPhase = p.Phase
				}
			})
			m.mu.Lock()
			v = &j.Videos[i]
			v.Speed = 0
			if err == nil {
				if !o.KeepTs {
					v.TSName = ""
				}
				v.State = "succeeded"
				v.Phase = "completed"
				v.Ready = true
				if st, e := os.Stat(filepath.Join(m.root, id, name)); e == nil {
					v.Size = st.Size()
				}
				j.Succeeded++
			} else if ctx.Err() != nil {
				if !o.KeepTs {
					v.TSName = ""
				}
				v.State = "cancelled"
			} else {
				v.State = "failed"
				v.Error = safeError(err)
				j.Failed++
				m.log(j, v.Error)
			}
			v.Revision++
			m.publish(j, "job.updated")
			m.mu.Unlock()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil || j.State == "cancelling" {
		j.State = "cancelled"
		for i := range j.Videos {
			if !terminal(j.Videos[i].State) {
				j.Videos[i].State = "cancelled"
			}
		}
	} else if setupErr != nil {
		j.State = "failed"
		j.Error = safeError(setupErr)
	} else if j.Failed > 0 {
		if j.Succeeded > 0 {
			j.State = "partial_failed"
		} else {
			j.State = "failed"
		}
	} else {
		j.State = "succeeded"
	}
	j.Phase = j.State
	now := time.Now()
	j.FinishedAt = &now
	j.cancel = nil
	clearSecrets(&j.input)
	m.log(j, "任務結束："+j.State)
	m.publish(j, "job.finished")
	m.prune()
}
func safeError(e error) string {
	s := e.Error()
	for _, known := range []string{"HTTP 403: Cloudflare", "HTTP 403:", "FFmpeg 轉封裝失敗", "等待期間未捕獲", "網頁偵測失敗", "切片超過", "播放清單沒有", "無法建立", "合併寫入失敗", "發布 MP4 失敗", "expected a direct M3U8", "empty playlist", "unsupported encryption"} {
		if strings.Contains(s, known) {
			switch known {
			case "HTTP 403: Cloudflare":
				return "來源 HTTP 403：Cloudflare 拒絕存取"
			case "HTTP 403:":
				return "來源 HTTP 403：請確認網址及來源標頭"
			case "expected a direct M3U8":
				return "此設定需要直接 M3U8 URL（來源回傳非播放清單）"
			case "FFmpeg 轉封裝失敗":
				return "FFmpeg 轉封裝失敗，已保留 TS（位於任務輸出目錄）"
			default:
				return known
			}
		}
	}
	if strings.Contains(s, "個切片下載失敗") {
		return "切片下載或解密失敗，未發布不完整 MP4"
	}
	if strings.Contains(s, "HTTP 狀態碼異常:") {
		return "來源回傳非成功 HTTP 狀態"
	}
	return "來源讀取或處理失敗，請確認網址、標頭及磁碟空間"
}
func dependencies() map[string]bool {
	_, ff := exec.LookPath("ffmpeg")
	chrome := false
	candidates := []string{"chrome", "google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "headless_shell", "msedge", "C:/Program Files/Google/Chrome/Application/chrome.exe", "C:/Program Files (x86)/Google/Chrome/Application/chrome.exe", "C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe", "C:/Program Files/Microsoft/Edge/Application/msedge.exe", filepath.Join(os.Getenv("USERPROFILE"), "AppData/Local/Google/Chrome/Application/chrome.exe"), filepath.Join(os.Getenv("USERPROFILE"), "AppData/Local/Chromium/Application/chrome.exe"), "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Chromium.app/Contents/MacOS/Chromium"}
	for _, p := range candidates {
		if _, e := exec.LookPath(p); e == nil {
			chrome = true
			break
		}
	}
	return map[string]bool{"ffmpeg": ff == nil, "chrome": chrome}
}
func (m *JobManager) close() {
	m.mu.Lock()
	m.closing = true
	m.cancel()
	for _, j := range m.jobs {
		if j.State == "queued" {
			j.State = "cancelled"
			now := time.Now()
			j.FinishedAt = &now
			clearSecrets(&j.input)
		}
		if j.cancel != nil {
			j.cancel()
		}
	}
	m.mu.Unlock()
	m.hub.close()
	<-m.done
}
