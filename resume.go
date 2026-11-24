package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type resumeMetadata struct {
	Source string `json:"sourceHash"`
	ETag   string `json:"etag"`
	Total  int64  `json:"total"`
}

func resourceHash(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

func strongETag(s string) bool {
	return len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"'
}

// Never append based on size alone. A strong ETag and If-Range bind stored
// bytes to one representation. Without a validator, safely restart at zero.
func readResourceFile(ctx context.Context, client *http.Client, target, part string, limit int64) ([]byte, error) {
	var last error
	for attempt := 0; attempt < resourceAttempts; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		retry, err := resumeAttempt(ctx, client, target, part, limit)
		if err == nil {
			return os.ReadFile(part)
		}
		last = err
		if !retry {
			return nil, err
		}
		if attempt+1 < resourceAttempts {
			backoff := time.Duration(1<<attempt) * time.Second
			if backoff > 8*time.Second {
				backoff = 8 * time.Second
			}
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return nil, last
}

func regularOrMissing(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("續傳暫存路徑不是一般檔案")
	}
	return nil
}

func resumeAttempt(ctx context.Context, client *http.Client, target, part string, limit int64) (bool, error) {
	if err := regularOrMissing(part); err != nil {
		return false, err
	}
	if err := regularOrMissing(part + ".json"); err != nil {
		return false, err
	}
	m := resumeMetadata{Source: resourceHash(target), Total: -1}
	var offset int64
	if raw, err := os.ReadFile(part + ".json"); err == nil {
		var saved resumeMetadata
		if json.Unmarshal(raw, &saved) == nil && saved.Source == m.Source && strongETag(saved.ETag) {
			if info, e := os.Stat(part); e == nil && info.Size() <= limit && (saved.Total < 0 || info.Size() <= saved.Total) {
				m, offset = saved, info.Size()
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		req.Header.Set("If-Range", m.ETag)
	}
	resp, err := client.Do(req)
	if err != nil {
		return true, fmt.Errorf("網路錯誤: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		total, e := strconv.ParseInt(strings.TrimPrefix(resp.Header.Get("Content-Range"), "bytes */"), 10, 64)
		if offset > 0 && strings.HasPrefix(resp.Header.Get("Content-Range"), "bytes */") && e == nil &&
			total == offset && m.Total == total && resp.Header.Get("ETag") == m.ETag {
			return false, nil
		}
		return false, fmt.Errorf("HTTP 416：無法確認暫存檔完整性，未視為完成")
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return retryableResponse(resp), responseStatusError(resp)
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return false, fmt.Errorf("來源未提供原始位元組，無法安全續傳")
	}
	expected := resp.ContentLength
	if resp.StatusCode == http.StatusPartialContent {
		start, end, total, e := parseContentRange(resp.Header.Get("Content-Range"))
		if e != nil || offset == 0 || start != offset || resp.Header.Get("ETag") != m.ETag ||
			(m.Total >= 0 && total != m.Total) || (expected >= 0 && expected != end-start+1) {
			return false, fmt.Errorf("HTTP 206：Content-Range 或 ETag 不符，拒絕附加資料")
		}
		m.Total, expected = total, end-start+1
	} else {
		// Range ignored or If-Range no longer matches: replace the old bytes.
		offset = 0
		m = resumeMetadata{Source: resourceHash(target), ETag: resp.Header.Get("ETag"), Total: resp.ContentLength}
	}
	if m.Total > limit || (expected >= 0 && expected > limit-offset) {
		return false, fmt.Errorf("segment exceeds %d MiB", limit/(1024*1024))
	}
	flags := os.O_CREATE | os.O_WRONLY
	if offset == 0 {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return false, err
	}
	raw, _ := json.Marshal(m)
	if err = os.WriteFile(part+".json", raw, 0600); err != nil {
		return false, err
	}
	n, copyErr := copyContext(ctx, f, io.LimitReader(resp.Body, limit-offset+1))
	if err := f.Close(); err != nil {
		return false, err
	}
	if n > limit-offset {
		return false, fmt.Errorf("segment exceeds %d MiB", limit/(1024*1024))
	}
	if copyErr != nil {
		return true, fmt.Errorf("傳輸中斷，已保留續傳暫存: %w", copyErr)
	}
	if expected >= 0 && n != expected {
		return true, fmt.Errorf("回應長度不符，已保留續傳暫存")
	}
	if m.Total >= 0 && offset+n != m.Total {
		return true, fmt.Errorf("切片尚未下載完整")
	}
	return false, nil
}

func parseContentRange(s string) (int64, int64, int64, error) {
	bad := fmt.Errorf("無效 Content-Range")
	if !strings.HasPrefix(s, "bytes ") {
		return 0, 0, 0, bad
	}
	parts := strings.Split(strings.TrimPrefix(s, "bytes "), "/")
	if len(parts) != 2 {
		return 0, 0, 0, bad
	}
	bounds := strings.Split(parts[0], "-")
	if len(bounds) != 2 {
		return 0, 0, 0, bad
	}
	start, e1 := strconv.ParseInt(bounds[0], 10, 64)
	end, e2 := strconv.ParseInt(bounds[1], 10, 64)
	total, e3 := strconv.ParseInt(parts[1], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || start < 0 || end < start || total <= end {
		return 0, 0, 0, bad
	}
	return start, end, total, nil
}

func prepareResumeDir(output string) (string, error) {
	dir := filepath.Join(filepath.Dir(output), ".resume-"+resourceHash(filepath.Base(output))[:16])
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("續傳目錄無效")
	}
	return dir, nil
}
