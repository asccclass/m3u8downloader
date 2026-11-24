package main

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// Keep redirects and cookies in the outer net/http client: every redirect must
// pass through the existing host-scoped header and clearance rules again.
type browserTLSTransport struct{ client tlsclient.HttpClient }

var chromeVersion = regexp.MustCompile(`(?:HeadlessChrome|Chrome)/(\d+)\.`)

func browserTLSProfile(ua string) profiles.ClientProfile {
	match := chromeVersion.FindStringSubmatch(ua)
	if len(match) != 2 {
		return profiles.DefaultClientProfile
	}
	major, _ := strconv.Atoi(match[1])
	best := 0
	p := profiles.DefaultClientProfile
	for name, candidate := range profiles.MappedTLSClients {
		if !strings.HasPrefix(name, "chrome_") {
			continue
		}
		v, err := strconv.Atoi(strings.TrimPrefix(name, "chrome_"))
		if err == nil && v <= major && v > best {
			best, p = v, candidate
		}
	}
	return p
}

func newBrowserTLSTransport(ua string) (*browserTLSTransport, error) {
	c, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(),
		tlsclient.WithClientProfile(browserTLSProfile(ua)),
		tlsclient.WithNotFollowRedirects(), tlsclient.WithTimeoutSeconds(120),
		tlsclient.WithTransportOptions(&tlsclient.TransportOptions{DisableCompression: true}),
	)
	if err != nil {
		return nil, fmt.Errorf("初始化 TLS 用戶端失敗: %w", err)
	}
	return &browserTLSTransport{client: c}, nil
}

func (t *browserTLSTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	req, err := fhttp.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
	if err != nil {
		return nil, err
	}
	req.Host, req.ContentLength, req.GetBody = r.Host, r.ContentLength, r.GetBody
	for name, values := range r.Header {
		req.Header[strings.ToLower(name)] = append([]string(nil), values...)
	}
	req.Header[fhttp.HeaderOrderKey] = []string{"accept", "accept-language", "cookie", "range", "if-range", "referer", "user-agent", "accept-encoding"}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	h := make(http.Header)
	for name, values := range resp.Header {
		h[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
	return &http.Response{Status: resp.Status, StatusCode: resp.StatusCode,
		Proto: resp.Proto, ProtoMajor: resp.ProtoMajor, ProtoMinor: resp.ProtoMinor,
		Header: h, Body: resp.Body, ContentLength: resp.ContentLength,
		TransferEncoding: resp.TransferEncoding, Close: resp.Close, Uncompressed: resp.Uncompressed,
		Request: r}, nil
}

func (t *browserTLSTransport) CloseIdleConnections() { t.client.CloseIdleConnections() }
