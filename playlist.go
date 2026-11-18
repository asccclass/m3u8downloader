package main

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

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
	valid := false
	var currentKey *KeyInfo
	mediaSeq := 0
	index := 0

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if !valid {
			if line != "#EXTM3U" {
				return nil, fmt.Errorf("expected a direct M3U8 URL, got invalid playlist")
			}
			valid = true
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-KEY:") && !strings.Contains(line, "METHOD=AES-128") && !strings.Contains(line, "METHOD=NONE") {
			return nil, fmt.Errorf("unsupported encryption")
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
	if !valid {
		return nil, fmt.Errorf("empty playlist")
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

	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024+1))
	if len(data) > 64*1024*1024 {
		return nil, fmt.Errorf("segment exceeds 64 MiB")
	}
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
		keyData, err = io.ReadAll(io.LimitReader(keyResp.Body, 33))
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

	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("密文長度不是 BlockSize 的倍數")
	}

	mode := cipher.NewCBCDecrypter(block, iv)
	decrypted := make([]byte, len(data))
	mode.CryptBlocks(decrypted, data)

	return decrypted, nil
}
