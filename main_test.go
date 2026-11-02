package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestDownloadHeadersReachPlaylistSegmentsAndKeys(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Referer() != "https://example.com/watch" || r.UserAgent() != "test-player" || r.Header.Get("Cookie") != "session=test" {
			t.Errorf("missing download headers for %s", r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/master.m3u8", http.StatusFound)
		case "/master.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nmedia.m3u8\n")
		case "/media.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key\"\n#EXTINF:1,\nsegment.ts\n#EXT-X-ENDLIST\n")
		case "/segment.ts", "/key":
			w.Write(make([]byte, 16))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newDownloadClient(server.URL, http.Header{
		"Referer": {"https://example.com/watch"}, "User-Agent": {"test-player"}, "Cookie": {"session=test"},
	})
	segments, err := parsePlaylist(server.URL+"/start", make(map[string]bool), client)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 {
		t.Fatalf("got %d segments, want 1", len(segments))
	}
	cache := make(map[string][]byte)
	if _, err := downloadAndDecryptSegment(client, segments[0], &cache, &sync.Mutex{}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 5 {
		t.Fatalf("got %d requests, want 5", requests.Load())
	}
}

func TestDownloadCookieStaysOnOriginalHost(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Error("cookie leaked to another host")
		}
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-ENDLIST\n")
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "session=test" {
			t.Error("missing cookie on original host")
		}
		http.Redirect(w, r, other.URL+"/index.m3u8", http.StatusFound)
	}))
	defer origin.Close()
	client := newDownloadClient(origin.URL, http.Header{"Cookie": {"session=test"}})
	for _, address := range []string{origin.URL, other.URL} {
		if _, err := parsePlaylist(address, make(map[string]bool), client); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCloudflareForbiddenExplainsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "cloudflare")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	_, err := parseM3U8(server.URL)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403: Cloudflare") {
		t.Fatalf("expected Cloudflare denial, got %v", err)
	}
}

func runWithArgs(t *testing.T, args ...string) error {
	t.Helper()
	previousFlags, previousArgs := flag.CommandLine, os.Args
	t.Cleanup(func() {
		flag.CommandLine, os.Args = previousFlags, previousArgs
	})
	flag.CommandLine = flag.NewFlagSet("m3u8downloader", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = append([]string{"m3u8downloader"}, args...)
	return run()
}

func TestDirectM3U8RejectsConflictingFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"url", []string{"-url", "https://example.com/page"}, "-url 與 -m3u8 不可同時使用"},
		{"page mode", []string{"-mode", "page"}, "-m3u8 僅可搭配"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"-m3u8", "https://example.com/playlist"}, tc.args...)
			if err := runWithArgs(t, args...); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}

func TestDirectM3U8SkipsBrowserWithoutExtension(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required by run")
	}
	var requested atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/playlist" || r.URL.Query().Get("token") != "signed" {
			t.Errorf("unexpected playlist request: %s", r.URL)
		}
		requested.Store(true)
		// An empty playlist stops before conversion, so no video fixture is needed.
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-ENDLIST\n")
	}))
	defer server.Close()
	err := runWithArgs(t, "-m3u8", server.URL+"/playlist?token=signed", "-wait", "0")
	if err == nil || !strings.Contains(err.Error(), "1 個 M3U8 下載失敗") {
		t.Fatalf("expected an empty-playlist download failure, got %v", err)
	}
	if !requested.Load() {
		t.Fatal("direct mode did not request the extensionless playlist")
	}
}

func TestParsePlaylistRelativeURLsAndKeys(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start.m3u8":
			http.Redirect(w, r, "/media/master.m3u8?token=signed", http.StatusFound)
		case "/media/master.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nlow/index.m3u8\n")
		case "/media/low/index.m3u8":
			if r.URL.Query().Get("token") != "signed" {
				t.Error("child playlist lost authorization query")
			}
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:42\n#EXT-X-KEY:METHOD=AES-128,URI=\"../../key\"\n#EXTINF:1,\n../first.ts\n#EXT-X-KEY:METHOD=NONE\n#EXTINF:1,\n/second.ts?token=own\n#EXT-X-ENDLIST\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	segments, err := parseM3U8(server.URL + "/start.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 {
		t.Fatalf("got %d segments, want 2", len(segments))
	}
	if segments[0].URL != server.URL+"/media/first.ts?token=signed" ||
		segments[0].KeyURI != server.URL+"/key?token=signed" || segments[0].SeqNum != 42 {
		t.Fatalf("incorrect first segment: %+v", segments[0])
	}
	if segments[1].URL != server.URL+"/second.ts?token=own" ||
		segments[1].KeyURI != "" || segments[1].SeqNum != 43 {
		t.Fatalf("incorrect second segment: %+v", segments[1])
	}
}

func TestParsePlaylistRejectsCycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nloop.m3u8\n")
	}))
	defer server.Close()
	if _, err := parseM3U8(server.URL + "/loop.m3u8"); err == nil {
		t.Fatal("expected an error for a playlist cycle")
	}
}

func TestDownloadPlaylistRejectsMissingSegments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\ngood.ts\n#EXTINF:1,\nmissing.ts\n#EXT-X-ENDLIST\n")
		case "/good.ts":
			fmt.Fprint(w, "segment content")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	out := filepath.Join(t.TempDir(), "incomplete.mp4")
	err := downloadPlaylist(server.URL+"/index.m3u8", out, 2, false)
	if err == nil || !strings.Contains(err.Error(), "1 個切片下載失敗") {
		t.Fatalf("expected a segment failure, got %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("incomplete download must not create an output file: %v", err)
	}
}
