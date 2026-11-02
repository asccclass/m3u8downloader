package main

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

type Segment struct {
	Index  int
	URL    string
	KeyURI string
	IV     []byte
	SeqNum int
}

type DownloadResult struct {
	Index int
	Data  []byte
	Error error
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "[錯誤] %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	targetURL := flag.String("url", "", "網頁網址或完整的 M3U8 網址 (含驗證參數)")
	m3u8URL := flag.String("m3u8", "", "直接下載 M3U8 網址，跳過網頁解析 (不可與 -url 同時使用)")
	outputFile := flag.String("out", "output.mp4", "最終輸出的影片檔名 (自動轉為 mp4)")
	concurrency := flag.Int("c", 6, "並發下載線程數")
	keepTs := flag.Bool("keep-ts", false, "是否保留中間產生的原始 .ts 檔案")
	waitTime := flag.Int("wait", 6, "等待網頁載入與播放的時間 (秒)")
	mode := flag.String("mode", "auto", "輸入類型: auto、page 或 m3u8 (無副檔名的播放清單請指定 m3u8)")
	referer := flag.String("referer", "", "下載請求的 Referer (影片播放頁網址)")
	userAgent := flag.String("user-agent", "", "下載請求的 User-Agent")
	cookie := flag.String("cookie", "", "下載來源的 Cookie (僅傳送至播放清單的原始主機)")
	flag.Parse()

	if *m3u8URL != "" {
		if *targetURL != "" {
			return fmt.Errorf("-url 與 -m3u8 不可同時使用")
		}
		if *mode != "auto" && *mode != "m3u8" {
			return fmt.Errorf("-m3u8 僅可搭配 -mode auto 或 m3u8")
		}
		*targetURL = *m3u8URL
		*mode = "m3u8"
	}
	if *targetURL == "" {
		fmt.Println("請提供網頁或 M3U8 網址，例如:")
		fmt.Println("  go run main.go -url \"https://example.com/video-page\" -out \"video.mp4\" -wait 10")
		fmt.Println("  go run main.go -m3u8 \"https://example.com/index.m3u8\" -out \"video.mp4\"")
		return nil
	}
	if *concurrency < 1 || *waitTime < 0 {
		return fmt.Errorf("-c 必須大於 0，-wait 不可小於 0")
	}
	parsed, err := url.Parse(*targetURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("-url 或 -m3u8 必須是有效的 HTTP 或 HTTPS 網址")
	}
	if *mode != "auto" && *mode != "page" && *mode != "m3u8" {
		return fmt.Errorf("-mode 必須是 auto、page 或 m3u8")
	}
	if *outputFile == "" {
		return fmt.Errorf("-out 不可為空白")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("找不到 ffmpeg，請先安裝並加入 PATH: %w", err)
	}

	urls := []string{*targetURL}
	if *mode == "page" || (*mode == "auto" && !strings.HasSuffix(strings.ToLower(parsed.Path), ".m3u8")) {
		urls, err = discoverM3U8(*targetURL, time.Duration(*waitTime)*time.Second)
		if err != nil {
			return err
		}
	}
	if len(urls) == 0 {
		return fmt.Errorf("等待期間未捕獲任何 M3U8 網址，可增加 -wait 或先確認影片是否需要手動播放")
	}
	fmt.Printf("共取得 %d 個 M3U8 網址，開始逐一下載。\n", len(urls))
	headers := make(http.Header)
	for name, value := range map[string]string{"Referer": *referer, "User-Agent": *userAgent, "Cookie": *cookie} {
		if value != "" {
			headers.Set(name, value)
		}
	}
	failures := 0
	for i, playlistURL := range urls {
		output := outputPath(*outputFile, i, len(urls))
		fmt.Printf("\n[%d/%d] %s -> %s\n", i+1, len(urls), playlistURL, output)
		if err := downloadPlaylist(playlistURL, output, *concurrency, *keepTs, headers); err != nil {
			failures++
			fmt.Fprintf(os.Stderr, "[下載失敗] %s: %v\n", playlistURL, err)
		}
	}
	fmt.Printf("\n全部處理完畢：成功 %d，失敗 %d。\n", len(urls)-failures, failures)
	if failures > 0 {
		return fmt.Errorf("%d 個 M3U8 下載失敗", failures)
	}
	return nil
}

func outputPath(name string, index, total int) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	if total > 1 {
		return fmt.Sprintf("%s_%03d.mp4", base, index+1)
	}
	return base + ".mp4"
}

func discoverM3U8(targetURL string, wait time.Duration) ([]string, error) {
	ctx, cancel := chromedp.NewContext(context.Background())
	defer cancel()
	ctx, timeoutCancel := context.WithTimeout(ctx, wait+30*time.Second)
	defer timeoutCancel()
	fmt.Printf("正在開啟網頁並監聽 M3U8 請求，載入後等待 %v: %s\n", wait, targetURL)
	// 先訂閱再導覽，避免漏掉頁面初始化時發出的請求。
	eventsCtx, eventsCancel := context.WithCancel(ctx)
	defer eventsCancel()
	requests := chromedp.Events(eventsCtx, network.RequestWillBeSent)
	var urls []string
	seen := make(map[string]bool)
	done := make(chan error, 1)
	go func() {
		for ev, err := range requests {
			if err != nil {
				done <- err
				return
			}
			requestURL := ev.Request.URL
			parsed, err := url.Parse(requestURL)
			if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") &&
				strings.Contains(strings.ToLower(parsed.Path), ".m3u8") && !seen[requestURL] {
				seen[requestURL] = true
				urls = append(urls, requestURL)
				fmt.Printf("[捕獲 M3U8] %s\n", requestURL)
			}
		}
		done <- nil
	}()
	err := chromedp.Do(ctx,
		chromedp.Func(func(ctx context.Context, target *chromedp.Target) error {
			_, err := cdp.Call(ctx, target, network.Enable, network.EnableParams{})
			return err
		}),
		chromedp.Navigate(targetURL),
		chromedp.Sleep(wait),
	)
	eventsCancel()
	listenErr := <-done // 等待收集停止後才讀取 urls，避免資料競爭。
	if err != nil {
		return nil, fmt.Errorf("網頁偵測失敗: %w", err)
	}
	if listenErr != nil && listenErr != context.Canceled {
		return nil, fmt.Errorf("網路事件監聽失敗: %w", listenErr)
	}
	return urls, nil
}

func downloadPlaylist(m3u8URL, outputFile string, concurrency int, keepTs bool, headers ...http.Header) error {
	startTime := time.Now()
	fmt.Printf("[1/5] 正在解析 M3U8 索引: %s\n", m3u8URL)

	client := newDownloadClient(m3u8URL, headers...)
	segments, err := parsePlaylist(m3u8URL, make(map[string]bool), client)
	if err != nil {
		return fmt.Errorf("解析 M3U8 失敗: %w", err)
	}
	if len(segments) == 0 {
		return fmt.Errorf("播放清單沒有可下載的切片")
	}

	total := len(segments)
	fmt.Printf("[2/5] 成功解析，共找到 %d 個影片切片。開始並發下載與解密...\n", total)

	results := make(chan DownloadResult, total)
	tasks := make(chan Segment, total)

	var wg sync.WaitGroup
	keyCache := make(map[string][]byte)
	var keyMutex sync.Mutex

	for i := 0; i < concurrency; i++ {
		go func() {
			for seg := range tasks {
				data, err := downloadAndDecryptSegment(client, seg, &keyCache, &keyMutex)
				results <- DownloadResult{
					Index: seg.Index,
					Data:  data,
					Error: err,
				}
				wg.Done()
			}
		}()
	}

	for _, seg := range segments {
		wg.Add(1)
		tasks <- seg
	}
	close(tasks)

	go func() {
		wg.Wait()
		close(results)
	}()

	downloadedBuffers := make([][]byte, total)
	successCount := 0
	errorCount := 0

	for res := range results {
		if res.Error != nil {
			fmt.Printf("\n[警告] 切片 #%d 下載/解密失敗: %v", res.Index, res.Error)
			errorCount++
		} else {
			downloadedBuffers[res.Index] = res.Data
			successCount++
		}
		fmt.Printf("\r進度: %d / %d (成功: %d, 失敗: %d)", successCount+errorCount, total, successCount, errorCount)
	}
	fmt.Println()

	if errorCount > 0 {
		return fmt.Errorf("%d 個切片下載失敗，未輸出不完整的影片", errorCount)
	}

	// 先輸出成暫存的 .ts 檔案
	fmt.Printf("[3/5] 下載完成！正在組合為暫存 TS 檔...\n")
	if err := os.MkdirAll(filepath.Dir(outputFile), 0755); err != nil {
		return fmt.Errorf("無法建立輸出目錄: %w", err)
	}
	outFile, err := os.CreateTemp(filepath.Dir(outputFile), "temp_merged_*.ts")
	if err != nil {
		return fmt.Errorf("無法建立暫存檔案: %w", err)
	}
	tempTsFile := outFile.Name()

	for i := 0; i < total; i++ {
		if downloadedBuffers[i] != nil {
			_, err := outFile.Write(downloadedBuffers[i])
			if err != nil {
				outFile.Close()
				os.Remove(tempTsFile)
				return fmt.Errorf("寫入檔案失敗 (切片 #%d): %w", i, err)
			}
		}
	}
	if err := outFile.Close(); err != nil {
		os.Remove(tempTsFile)
		return fmt.Errorf("關閉暫存檔案失敗: %w", err)
	}

	// [4/5] 轉碼為 MP4
	fmt.Printf("[4/5] 正在透過 FFmpeg 無損轉封裝為 MP4: %s\n", outputFile)
	err = convertTsToMp4(tempTsFile, outputFile)
	if err != nil {
		return fmt.Errorf("FFmpeg 轉封裝失敗，已保留 TS 檔案 %s: %w", tempTsFile, err)
	}

	// 清理暫存 TS 檔
	if !keepTs {
		os.Remove(tempTsFile)
	} else {
		fmt.Printf("已依要求保留原始 TS 檔案: %s\n", tempTsFile)
	}

	elapsed := time.Since(startTime)
	fmt.Printf("[5/5] 大功告成！總耗時: %v\n", elapsed)
	fmt.Printf("最終 MP4 影片儲存於: %s\n", outputFile)
	return nil
}

func parseM3U8(targetURL string) ([]Segment, error) {
	return parsePlaylist(targetURL, make(map[string]bool), newDownloadClient(targetURL))
}

// Apply download headers to every request, including nested playlists and keys.
// Restrict explicit cookies to the initial host, including after redirects.
type downloadTransport struct {
	headers    http.Header
	cookieHost string
}

func (t *downloadTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	request := req.Clone(req.Context())
	for name, values := range t.headers {
		if strings.EqualFold(name, "Cookie") && !strings.EqualFold(request.URL.Host, t.cookieHost) {
			continue
		}
		request.Header[name] = append([]string(nil), values...)
	}
	return http.DefaultTransport.RoundTrip(request)
}

func newDownloadClient(targetURL string, headers ...http.Header) *http.Client {
	configured := make(http.Header)
	for _, values := range headers {
		for name, entries := range values {
			configured[http.CanonicalHeaderKey(name)] = append([]string(nil), entries...)
		}
	}
	parsed, _ := url.Parse(targetURL)
	host := ""
	if parsed != nil {
		host = parsed.Host
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &downloadTransport{headers: configured, cookieHost: host},
	}
}

func responseStatusError(resp *http.Response) error {
	if resp.StatusCode == http.StatusForbidden {
		if strings.EqualFold(resp.Header.Get("Server"), "cloudflare") {
			return fmt.Errorf("HTTP 403: Cloudflare 拒絕存取；請確認瀏覽器是否可播放，並使用相同的 -referer、-user-agent 與 -cookie；仍可能受來源的 IP 或瀏覽器驗證限制")
		}
		return fmt.Errorf("HTTP 403: 來源拒絕存取，請確認網址未過期，或設定播放頁的 -referer、-user-agent 與 -cookie")
	}
	return fmt.Errorf("HTTP 狀態碼異常: %d", resp.StatusCode)
}

func parsePlaylist(targetURL string, visited map[string]bool, client *http.Client) ([]Segment, error) {
	if visited[targetURL] || len(visited) >= 16 {
		return nil, fmt.Errorf("播放清單循環引用或巢狀層數過多: %s", targetURL)
	}
	visited[targetURL] = true
	resp, err := client.Get(targetURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, responseStatusError(resp)
	}

	parsedURL := resp.Request.URL
	baseUrlStr := parsedURL.String()
	searchParams := parsedURL.RawQuery

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var segments []Segment
	var currentKey *KeyInfo
	mediaSeq := 0
	index := 0

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:") {
			fmt.Sscanf(line, "#EXT-X-MEDIA-SEQUENCE:%d", &mediaSeq)
			continue
		}

		if strings.HasPrefix(line, "#EXT-X-KEY:") {
			currentKey = parseKeyTag(line, baseUrlStr, searchParams)
			continue
		}

		if strings.HasPrefix(line, "#") {
			continue
		}

		segURL := resolveMediaURL(baseUrlStr, line, searchParams)

		if strings.Contains(strings.ToLower(strings.SplitN(segURL, "?", 2)[0]), ".m3u8") {
			return parsePlaylist(segURL, visited, client)
		}

		var keyURI string
		var iv []byte
		if currentKey != nil {
			keyURI = currentKey.URI
			iv = currentKey.IV
		}

		segments = append(segments, Segment{
			Index:  index,
			URL:    segURL,
			KeyURI: keyURI,
			IV:     iv,
			SeqNum: mediaSeq,
		})
		mediaSeq++
		index++
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("讀取播放清單失敗: %w", err)
	}
	return segments, nil
}

func resolveMediaURL(base, reference, query string) string {
	baseURL, err := url.Parse(base)
	if err != nil {
		return reference
	}
	ref, err := url.Parse(reference)
	if err != nil {
		return reference
	}
	resolved := baseURL.ResolveReference(ref)
	if resolved.RawQuery == "" {
		resolved.RawQuery = query
	}
	return resolved.String()
}

type KeyInfo struct {
	URI string
	IV  []byte
}

func parseKeyTag(tagLine, baseUrlStr, searchParams string) *KeyInfo {
	if !strings.Contains(tagLine, "METHOD=AES-128") {
		return nil
	}

	uriStart := strings.Index(tagLine, "URI=\"")
	if uriStart == -1 {
		return nil
	}
	uriStart += 5
	uriEnd := strings.Index(tagLine[uriStart:], "\"")
	if uriEnd == -1 {
		return nil
	}
	keyURI := tagLine[uriStart : uriStart+uriEnd]

	keyURI = resolveMediaURL(baseUrlStr, keyURI, searchParams)

	var ivBytes []byte
	if strings.Contains(tagLine, "IV=0x") {
		ivStart := strings.Index(tagLine, "IV=0x") + 5
		ivEnd := ivStart + 32
		if ivEnd <= len(tagLine) {
			ivHex := tagLine[ivStart:ivEnd]
			if b, err := hex.DecodeString(ivHex); err == nil {
				ivBytes = b
			}
		}
	}

	return &KeyInfo{
		URI: keyURI,
		IV:  ivBytes,
	}
}

func downloadAndDecryptSegment(client *http.Client, seg Segment, keyCache *map[string][]byte, mu *sync.Mutex) ([]byte, error) {
	resp, err := client.Get(seg.URL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, responseStatusError(resp)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if seg.KeyURI == "" {
		return data, nil
	}

	mu.Lock()
	keyData, exists := (*keyCache)[seg.KeyURI]
	if !exists {
		keyResp, err := client.Get(seg.KeyURI)
		if err != nil {
			mu.Unlock()
			return nil, fmt.Errorf("取得金鑰失敗: %v", err)
		}
		defer keyResp.Body.Close()
		if keyResp.StatusCode != http.StatusOK {
			mu.Unlock()
			return nil, fmt.Errorf("取得金鑰失敗: %w", responseStatusError(keyResp))
		}
		keyData, err = io.ReadAll(keyResp.Body)
		if err != nil {
			mu.Unlock()
			return nil, fmt.Errorf("讀取金鑰失敗: %v", err)
		}
		(*keyCache)[seg.KeyURI] = keyData
	}
	mu.Unlock()

	iv := seg.IV
	if len(iv) == 0 {
		iv = make([]byte, 16)
		num := seg.SeqNum
		for i := 15; i >= 12; i-- {
			iv[i] = byte(num & 0xff)
			num >>= 8
		}
	}

	block, err := aes.NewCipher(keyData)
	if err != nil {
		return nil, err
	}

	if len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("密文長度不是 BlockSize 的倍數")
	}

	mode := cipher.NewCBCDecrypter(block, iv)
	decrypted := make([]byte, len(data))
	mode.CryptBlocks(decrypted, data)

	return decrypted, nil
}

// 呼叫系統 ffmpeg 進行無損轉封裝
func convertTsToMp4(tsPath, mp4Path string) error {
	// 執行指令: ffmpeg -y -i input.ts -c copy output.mp4
	cmd := exec.Command("ffmpeg", "-y", "-i", tsPath, "-c", "copy", mp4Path)

	// 若需要看 ffmpeg 詳細轉檔過程，可將下面兩行註解拿掉
	// cmd.Stdout = os.Stdout
	// cmd.Stderr = os.Stderr

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
