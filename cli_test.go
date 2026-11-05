package main

import (
	"bytes"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the real main() entry point in an isolated process, including its
// exit status, without loading the Web server, envfile or website profiles.
func TestCLIProcessEntry(t *testing.T) {
	if os.Getenv("M3U8_TEST_CLI_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"m3u8-dl"}, os.Args[i+1:]...)
			flag.CommandLine = flag.NewFlagSet("m3u8-dl", flag.ExitOnError)
			main()
			return
		}
	}
	t.Fatal("missing subprocess argument separator")
}

func runCLIProcess(t *testing.T, dir string, args ...string) ([]byte, error) {
	t.Helper()
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(exe, append([]string{"-test.run=^TestCLIProcessEntry$", "--"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "M3U8_TEST_CLI_PROCESS=1")
	return cmd.CombinedOutput()
}

func TestCLIStandaloneWithoutWebConfiguration(t *testing.T) {
	data := tsFixture(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Referer() != "https://page.example/" || r.UserAgent() != "standalone-cli" || r.Header.Get("Cookie") != "session=cli" {
			w.WriteHeader(403)
			return
		}
		if r.URL.Path == "/playlist" {
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\n/segment.ts\n#EXT-X-ENDLIST\n")
		} else {
			w.Write(data)
		}
	}))
	defer source.Close()
	for _, configuration := range []string{"absent", "invalid"} {
		t.Run(configuration, func(t *testing.T) {
			dir := t.TempDir()
			if configuration == "invalid" {
				os.WriteFile(filepath.Join(dir, "envfile"), []byte("not valid Web configuration"), 0600)
				os.Mkdir(filepath.Join(dir, "config"), 0700)
				os.WriteFile(filepath.Join(dir, "config", "site-profiles.json"), []byte("invalid json"), 0600)
			}
			out, e := runCLIProcess(t, dir, "-m3u8", source.URL+"/playlist", "-out", "videos/standalone.mp4", "-referer", "https://page.example/", "-user-agent", "standalone-cli", "-cookie", "session=cli", "-c", "2")
			if e != nil {
				t.Fatalf("standalone CLI failed: %v\n%s", e, out)
			}
			if stat, e := os.Stat(filepath.Join(dir, "videos", "standalone.mp4")); e != nil || stat.Size() == 0 {
				t.Fatal("CLI did not create its local output", e)
			}
			if _, e := os.Stat(filepath.Join(dir, "downloads")); !os.IsNotExist(e) {
				t.Fatal("CLI created a Web output directory")
			}
			if configuration == "absent" {
				for _, name := range []string{"envfile", "config"} {
					if _, e := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(e) {
						t.Fatalf("CLI created Web configuration %s", name)
					}
				}
			}
			if bytes.Contains(out, []byte("Web 介面：")) {
				t.Fatal("CLI unexpectedly started the Web service")
			}
		})
	}
}

func TestCLIStandalonePageDiscovery(t *testing.T) {
	if !dependencies()["chrome"] {
		t.Skip("Chrome required for CLI page discovery")
	}
	data := tsFixture(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<!doctype html><script>fetch('/index.m3u8')</script>`)
		case "/index.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\n/segment.ts\n#EXT-X-ENDLIST\n")
		default:
			w.Write(data)
		}
	}))
	defer source.Close()
	dir := t.TempDir()
	out, e := runCLIProcess(t, dir, "-url", source.URL+"/page", "-mode", "page", "-wait", "1", "-out", "from-page.mp4")
	if e != nil {
		t.Fatalf("CLI page discovery failed: %v\n%s", e, out)
	}
	if _, e = os.Stat(filepath.Join(dir, "from-page.mp4")); e != nil {
		t.Fatal(e)
	}
}

func TestCLIStandaloneFailureExitStatus(t *testing.T) {
	if !dependencies()["ffmpeg"] {
		t.Skip("FFmpeg required")
	}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.m3u8" {
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\n/missing.ts\n#EXT-X-ENDLIST\n")
		} else {
			w.WriteHeader(404)
		}
	}))
	defer source.Close()
	dir := t.TempDir()
	out, e := runCLIProcess(t, dir, "-m3u8", source.URL+"/index.m3u8", "-out", "incomplete.mp4")
	exit, ok := e.(*exec.ExitError)
	if !ok || exit.ExitCode() != 1 {
		t.Fatalf("expected CLI exit code 1, got %v: %s", e, out)
	}
	if !strings.Contains(string(out), "1 個 M3U8 下載失敗") {
		t.Fatal("CLI failure summary missing", string(out))
	}
	if _, e = os.Stat(filepath.Join(dir, "incomplete.mp4")); !os.IsNotExist(e) {
		t.Fatal("incomplete MP4 was published")
	}
}
