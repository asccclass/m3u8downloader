package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Segment struct {
	Index  int
	URL    string
	KeyURI string
	IV     []byte
	SeqNum int
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
	webMode := flag.Bool("web", false, "Start local Web UI")
	listen := flag.String("listen", "127.0.0.1:8080", "Web loopback address")
	outputRoot := flag.String("output-root", "downloads", "Web output directory")
	profileFile := flag.String("profiles", "config/site-profiles.json", "Site profiles file")
	initConfig := flag.Bool("init-config", false, "建立具隨機金鑰的 envfile（不覆寫既有檔案）")
	flag.Parse()
	if *initConfig {
		return initializeConfig("envfile")
	}
	if *webMode {
		return runWeb(*listen, *outputRoot, *profileFile)
	}

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
		fmt.Println("  go run . -url \"https://example.com/video-page\" -out \"video.mp4\" -wait 10")
		fmt.Println("  go run . -m3u8 \"https://example.com/index.m3u8\" -out \"video.mp4\"")
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
	return discoverContext(context.Background(), targetURL, wait, nil, nil)
}
func downloadPlaylist(m3u8URL, outputFile string, concurrency int, keepTs bool, headers ...http.Header) error {
	return downloadContext(context.Background(), m3u8URL, outputFile, concurrency, keepTs, newDownloadClient(m3u8URL, headers...), func(p Progress) {
		fmt.Printf("%s: %d/%d (failed %d)\n", p.Phase, p.Succeeded+p.Failed, p.Total, p.Failed)
	})
}
