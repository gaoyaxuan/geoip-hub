package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gaoyaxuan/geoip-hub/geoip"
)

// stubProvider 对 1.0.0.1 返回查无数据，其他 IP 返回固定结果，并记录收到的语言
type stubProvider struct {
	mu   sync.Mutex
	lang string
}

func (p *stubProvider) Name() string { return "stub" }

func (p *stubProvider) Lookup(_ context.Context, ip netip.Addr, lang string) (geoip.Location, error) {
	p.mu.Lock()
	p.lang = lang
	p.mu.Unlock()
	if ip.String() == "1.0.0.1" {
		return geoip.Location{}, geoip.ErrNotFound
	}
	return geoip.Location{Country: "测试国", CountryCode: "TS", Region: "测试省", City: "测试市"}, nil
}

func (p *stubProvider) lastLang() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lang
}

func newTestServer(t *testing.T, trusted string) (http.Handler, *stubProvider) {
	t.Helper()
	prefixes, err := parsePrefixes(trusted)
	if err != nil {
		t.Fatal(err)
	}
	p := &stubProvider{}
	s := &server{resolver: geoip.NewResolver(geoip.Step{Provider: p}), lang: "zh-CN", trusted: prefixes, batchMax: 3, started: time.Now()}
	return s.routes(), p
}

func serve(t *testing.T, h http.Handler, r *http.Request) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	return w, body
}

func request(method, target, contentType, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	r.RemoteAddr = "8.8.4.4:1234"
	return r
}

func TestQueryCompatibleWithOldAPI(t *testing.T) {
	h, _ := newTestServer(t, "")
	for name, r := range map[string]*http.Request{
		"GET 路径":     request("GET", "/8.8.8.8", "", ""),
		"POST 路径":    request("POST", "/8.8.8.8", "", ""),
		"GET query":  request("GET", "/?ip=8.8.8.8", "", ""),
		"POST 表单":    request("POST", "/", "application/x-www-form-urlencoded", "ip=8.8.8.8"),
		"POST JSON":  request("POST", "/", "application/json", `{"ip":"8.8.8.8"}`),
		"不带 ip 查客户端": request("GET", "/?ip=", "", ""),
	} {
		w, body := serve(t, h, r)
		want := "8.8.8.8"
		if strings.HasPrefix(name, "不带") {
			want = "8.8.4.4"
		}
		if w.Code != 200 || body["code"] != 200.0 || body["ip"] != want || body["city"] != "测试市" || body["source"] != "stub" {
			t.Errorf("%s: %d %v", name, w.Code, body)
		}
	}
}

func TestQueryResults(t *testing.T) {
	h, _ := newTestServer(t, "")
	tests := []struct {
		target string
		check  func(map[string]any) bool
	}{
		{"/abc", func(b map[string]any) bool { return b["code"] == 400.0 && b["message"] == "Invalid IP address" }},
		{"/8.8.8.8?source=nope", func(b map[string]any) bool { return b["code"] == 400.0 && b["message"] == "Unknown source" }},
		{"/8.8.8.8?source=stub", func(b map[string]any) bool { return b["code"] == 200.0 && b["source"] == "stub" }},
		{"/1.0.0.1", func(b map[string]any) bool { return b["code"] == 200.0 && b["country"] == "" && b["source"] == nil }},
		{"/192.168.1.1", func(b map[string]any) bool { return b["code"] == 200.0 && b["bogon"] == true }},
	}
	for _, tt := range tests {
		if w, body := serve(t, h, request("GET", tt.target, "", "")); w.Code != 200 || !tt.check(body) {
			t.Errorf("%s: %d %v", tt.target, w.Code, body)
		}
	}
}

func TestClientIPEndpoint(t *testing.T) {
	h, _ := newTestServer(t, "")
	w, _ := serve(t, h, request("GET", "/ip", "", ""))
	if w.Body.String() != "8.8.4.4" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("纯文本: %q %s", w.Body.String(), w.Header().Get("Content-Type"))
	}
	for _, r := range []*http.Request{
		request("GET", "/ip?json=true", "", ""),
		request("POST", "/ip", "application/json", `{"json":true}`),
	} {
		if _, body := serve(t, h, r); body["code"] != 200.0 || body["ip"] != "8.8.4.4" {
			t.Errorf("%s %s: %v", r.Method, r.URL, body)
		}
	}
}

func TestClientIPBehindProxy(t *testing.T) {
	const all = "0.0.0.0/0,::/0"
	tests := []struct {
		name, trusted, remote, xff, realIP, want string
	}{
		{"默认信任所有代理，取最左边", all, "10.0.0.1:80", "1.1.1.1, 2.2.2.2", "", "1.1.1.1"},
		{"从右往左跳过可信代理", "10.0.0.0/8", "10.0.0.1:80", "1.1.1.1, 2.2.2.2, 10.0.0.2", "", "2.2.2.2"},
		{"直连地址不可信时忽略请求头", "10.0.0.0/8", "3.3.3.3:80", "1.1.1.1", "", "3.3.3.3"},
		{"使用 X-Real-IP", all, "10.0.0.1:80", "", "4.4.4.4", "4.4.4.4"},
		{"X-Forwarded-For 非法时回退到 X-Real-IP", all, "10.0.0.1:80", "bad", "4.4.4.4", "4.4.4.4"},
		{"不信任任何代理", "none", "10.0.0.1:80", "1.1.1.1", "", "10.0.0.1"},
		{"IPv6", all, "[::1]:80", "2400:3200::1", "", "2400:3200::1"},
	}
	for _, tt := range tests {
		h, _ := newTestServer(t, tt.trusted)
		r := request("GET", "/ip", "", "")
		r.RemoteAddr = tt.remote
		if tt.xff != "" {
			r.Header.Set("X-Forwarded-For", tt.xff)
		}
		if tt.realIP != "" {
			r.Header.Set("X-Real-IP", tt.realIP)
		}
		if w, _ := serve(t, h, r); w.Body.String() != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, w.Body.String(), tt.want)
		}
	}
}

func TestBatch(t *testing.T) {
	h, _ := newTestServer(t, "")
	_, body := serve(t, h, request("POST", "/batch", "application/json", `["8.8.8.8", "abc", "192.168.1.1"]`))
	data, _ := body["data"].([]any)
	if body["code"] != 200.0 || len(data) != 3 {
		t.Fatalf("batch: %v", body)
	}
	first, second, third := data[0].(map[string]any), data[1].(map[string]any), data[2].(map[string]any)
	if first["city"] != "测试市" || second["code"] != 400.0 || second["ip"] != "abc" || third["bogon"] != true {
		t.Fatalf("batch data: %v", data)
	}
	for _, payload := range []string{`["1.1.1.1","2.2.2.2","3.3.3.3","4.4.4.4"]`, `[]`, `{"ip":"8.8.8.8"}`, `oops`} {
		if _, body := serve(t, h, request("POST", "/batch", "application/json", payload)); body["code"] != 400.0 {
			t.Errorf("%s: %v", payload, body)
		}
	}
}

func TestLanguage(t *testing.T) {
	h, p := newTestServer(t, "")
	tests := []struct{ target, acceptLanguage, want string }{
		{"/8.8.8.8", "", "zh-CN"},
		{"/8.8.8.8", "en-US,en;q=0.9", "en"},
		{"/8.8.8.8?lang=ja", "en-US", "ja"},
		{"/8.8.8.8?lang=xx", "ko", "zh-CN"},
	}
	for _, tt := range tests {
		r := request("GET", tt.target, "", "")
		if tt.acceptLanguage != "" {
			r.Header.Set("Accept-Language", tt.acceptLanguage)
		}
		serve(t, h, r)
		if got := p.lastLang(); got != tt.want {
			t.Errorf("%s (Accept-Language: %s): lang = %s, want %s", tt.target, tt.acceptLanguage, got, tt.want)
		}
	}
}

func TestCORSAndHealth(t *testing.T) {
	h, _ := newTestServer(t, "")
	if w, _ := serve(t, h, request("OPTIONS", "/batch", "", "")); w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("预检请求: %d %v", w.Code, w.Header())
	}
	if w, _ := serve(t, h, request("GET", "/8.8.8.8", "", "")); w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("响应缺少 CORS 头")
	}
	if w, _ := serve(t, h, request("GET", "/healthz", "", "")); w.Body.String() != "ok" {
		t.Fatalf("healthz: %q", w.Body.String())
	}
	_, body := serve(t, h, request("GET", "/stats", "", ""))
	providers, _ := body["providers"].([]any)
	if body["code"] != 200.0 || len(providers) != 1 {
		t.Fatalf("stats: %v", body)
	}
}
