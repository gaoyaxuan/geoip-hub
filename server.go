package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gaoyaxuan/geoip-hub/geoip"
)

// 与旧版保持一致：HTTP 状态码始终为 200，结果看响应体中的 code
const (
	codeOK      = 200
	codeInvalid = 400
	codeFailed  = 500

	batchWorkers = 8
	maxBodyBytes = 1 << 20
)

type server struct {
	resolver *geoip.Resolver
	cache    *geoip.Cache
	lang     string         // 默认语言
	trusted  []netip.Prefix // 可信代理，只有直连地址可信时才读取 X-Forwarded-For / X-Real-IP
	batchMax int
	started  time.Time
}

func newServer(cfg *config, r *geoip.Resolver, c *geoip.Cache) *server {
	return &server{resolver: r, cache: c, lang: cfg.lang, trusted: cfg.trusted, batchMax: cfg.batchMax, started: time.Now()}
}

type ipResponse struct {
	Code int `json:"code"`
	geoip.Location
}

type errorResponse struct {
	Code    int    `json:"code"`
	IP      string `json:"ip,omitempty"`
	Message string `json:"message"`
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		mux.HandleFunc(method+" /{$}", s.handleQuery)   // /?ip=8.8.8.8，不带 ip 时查询客户端自己
		mux.HandleFunc(method+" /{ip}", s.handleQuery)  // /8.8.8.8
		mux.HandleFunc(method+" /ip", s.handleClientIP) // 只返回客户端 IP
	}
	mux.HandleFunc("POST /batch", s.handleBatch)
	mux.HandleFunc("GET /stats", s.handleStats)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })
	return withCORS(mux)
}

func (s *server) handleQuery(w http.ResponseWriter, r *http.Request) {
	params, err := readParams(w, r)
	if err != nil {
		writeJSON(w, errorResponse{Code: codeInvalid, Message: "Request failed"})
		return
	}
	ip := cmp.Or(r.PathValue("ip"), params.Get("ip"))
	if ip == "" {
		ip = s.clientIP(r).String()
	}
	writeJSON(w, s.lookup(r.Context(), ip, params.Get("source"), s.langOf(r, params)))
}

// handleClientIP 只返回客户端 IP，默认为纯文本，json=true 时返回 JSON
func (s *server) handleClientIP(w http.ResponseWriter, r *http.Request) {
	params, err := readParams(w, r)
	if err != nil {
		writeJSON(w, errorResponse{Code: codeInvalid, Message: "Request failed"})
		return
	}
	ip := s.clientIP(r).String()
	if asJSON, _ := strconv.ParseBool(params.Get("json")); asJSON {
		writeJSON(w, struct {
			Code int    `json:"code"`
			IP   string `json:"ip"`
		}{codeOK, ip})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, ip)
}

// handleBatch 批量查询，请求体为 IP 数组：["8.8.8.8", "1.1.1.1"]，结果按顺序返回
func (s *server) handleBatch(w http.ResponseWriter, r *http.Request) {
	var ips []string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&ips); err != nil || len(ips) == 0 {
		writeJSON(w, errorResponse{Code: codeInvalid, Message: `Request body must be a JSON array of IPs, e.g. ["8.8.8.8"]`})
		return
	}
	if len(ips) > s.batchMax {
		writeJSON(w, errorResponse{Code: codeInvalid, Message: fmt.Sprintf("Too many IPs (max %d)", s.batchMax)})
		return
	}
	query := r.URL.Query()
	lang := s.langOf(r, query)
	results := make([]any, len(ips))
	sem := make(chan struct{}, batchWorkers)
	var wg sync.WaitGroup
	for i, ip := range ips {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			results[i] = s.lookup(r.Context(), ip, query.Get("source"), lang)
		})
	}
	wg.Wait()
	writeJSON(w, struct {
		Code int   `json:"code"`
		Data []any `json:"data"`
	}{codeOK, results})
}

func (s *server) handleStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, struct {
		Code      int              `json:"code"`
		Uptime    string           `json:"uptime"`
		Chain     []string         `json:"chain"`
		Cache     geoip.CacheStats `json:"cache"`
		Providers []geoip.Stats    `json:"providers"`
	}{codeOK, time.Since(s.started).Round(time.Second).String(), s.resolver.Chain(), s.cache.Stats(), s.resolver.Stats()})
}

// lookup 查询单个 IP，返回可以直接输出的响应
func (s *server) lookup(ctx context.Context, raw, source, lang string) any {
	ip, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return errorResponse{Code: codeInvalid, IP: raw, Message: "Invalid IP address"}
	}
	var loc geoip.Location
	if source != "" {
		loc, err = s.resolver.LookupFrom(ctx, source, ip, lang)
	} else {
		loc, err = s.resolver.Lookup(ctx, ip, lang)
	}
	switch {
	case err == nil, errors.Is(err, geoip.ErrNotFound): // 查无数据时与旧版一致，返回空字段
		return ipResponse{Code: codeOK, Location: loc}
	case errors.Is(err, geoip.ErrUnknownSource):
		return errorResponse{Code: codeInvalid, IP: raw, Message: "Unknown source"}
	default:
		slog.Warn("查询失败", "ip", raw, "err", err)
		return errorResponse{Code: codeFailed, IP: raw, Message: "Request failed"}
	}
}

// langOf 依次取 lang 参数、Accept-Language 请求头、默认语言
func (s *server) langOf(r *http.Request, params url.Values) string {
	return cmp.Or(geoip.NormalizeLang(params.Get("lang")), geoip.NormalizeLang(r.Header.Get("Accept-Language")), s.lang)
}

// clientIP 返回客户端 IP。直连地址是可信代理时才读取 X-Forwarded-For / X-Real-IP，
// 规则与 gin 的 ClientIP 一致：从右往左跳过可信代理，取第一个不可信的地址，全部可信时取最左边的。
func (s *server) clientIP(r *http.Request) netip.Addr {
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	ip := remote.Addr().Unmap()
	if !s.isTrusted(ip) {
		return ip
	}
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		hops := strings.Split(strings.Join(xff, ","), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
			if err != nil {
				break
			}
			if hop = hop.Unmap(); i == 0 || !s.isTrusted(hop) {
				return hop
			}
		}
	}
	if real, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); err == nil {
		return real.Unmap()
	}
	return ip
}

func (s *server) isTrusted(ip netip.Addr) bool {
	return slices.ContainsFunc(s.trusted, func(p netip.Prefix) bool { return p.Contains(ip) })
}

// readParams 合并 URL query、表单和 JSON body 中的参数（兼容旧版 gin ShouldBind 的用法）
func readParams(w http.ResponseWriter, r *http.Request) (url.Values, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if err := r.ParseMultipartForm(maxBodyBytes); err != nil && !errors.Is(err, http.ErrNotMultipart) {
			return nil, err
		}
		return r.Form, nil
	}
	params := r.URL.Query()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	for k, v := range body {
		if v != nil {
			params.Set(k, fmt.Sprint(v))
		}
	}
	return params, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

// withCORS 允许浏览器跨域调用
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
