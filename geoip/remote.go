package geoip

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
)

// API 描述一个远程 IP 查询接口，内置的有 IPAPI 和 IPWhois
type API struct {
	Name  string
	Langs []string // 支持的语言，请求的语言不在其中时使用第一个
	// URL 返回请求地址，key 是付费版密钥（可能为空）
	URL func(ip netip.Addr, lang, key string) string
	// Parse 解析响应体，没有该 IP 的数据时返回 ErrNotFound
	Parse func(body []byte) (Location, error)
	// Exhausted 根据响应头判断额度是否已用完，返回需要暂停的时长；可以为 nil
	Exhausted func(h http.Header) time.Duration
}

// RemoteOptions 是远程数据源的选项，零值可用
type RemoteOptions struct {
	Key     string        // 付费版密钥
	Limit   rate.Limit    // 每秒最多请求数，<= 0 表示不限速
	Burst   int           // 令牌桶的突发容量，默认 1
	Timeout time.Duration // 单次请求超时，默认 3s
	Cache   *Cache        // 结果缓存，nil 表示不缓存
	Client  *http.Client  // 默认使用内置的 client（支持 HTTP_PROXY / HTTPS_PROXY 环境变量）
}

const (
	breakerThreshold   = 5                // 连续失败多少次后熔断
	breakerCooldown    = 30 * time.Second // 首次熔断暂停多久，之后每次熔断翻倍
	maxBreakerCooldown = 10 * time.Minute
	defaultBackoff     = time.Minute // 429 响应没有 Retry-After 时暂停多久
	maxPause           = 24 * time.Hour
	userAgent          = "geoip-hub/1.0 (+https://github.com/gaoyaxuan/geoip-hub)"
)

var defaultClient = &http.Client{Transport: func() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 16
	return t
}()}

// Remote 是远程 API 数据源，调用方再频繁也不会打爆上游：
//   - 结果（包括"查无数据"）写入 Cache，同一 IP 在有效期内不再请求；
//   - 同一 IP 的并发请求合并为一次上游请求；
//   - 令牌桶限流，没有令牌时立即返回 ErrLimited，由 Resolver 降级到下一个数据源，不排队等待；
//   - 收到 429 或响应头显示额度用完时，按 Retry-After 等信息暂停请求；
//   - 连续失败 5 次熔断 30 秒；恢复后再失败一次立即重新熔断，且暂停时间翻倍（最长 10 分钟），
//     上游长时间不可用时，用来探测的请求也很少，不会频繁拖慢调用方。
type Remote struct {
	api     API
	opts    RemoteOptions
	limiter *rate.Limiter
	group   singleflight.Group

	requests, failures, limited atomic.Uint64

	mu     sync.Mutex
	fails  int       // 连续失败次数
	trips  int       // 连续熔断次数，决定下次暂停多久
	paused time.Time // 暂停请求直到该时间
}

func NewRemote(api API, opts RemoteOptions) *Remote {
	if opts.Limit <= 0 {
		opts.Limit = rate.Inf
	}
	opts.Burst = max(opts.Burst, 1)
	if opts.Timeout <= 0 {
		opts.Timeout = 3 * time.Second
	}
	if opts.Client == nil {
		opts.Client = defaultClient
	}
	return &Remote{api: api, opts: opts, limiter: rate.NewLimiter(opts.Limit, opts.Burst)}
}

func (r *Remote) Name() string { return r.api.Name }

func (r *Remote) Lookup(ctx context.Context, ip netip.Addr, lang string) (Location, error) {
	if len(r.api.Langs) > 0 && !slices.Contains(r.api.Langs, lang) {
		lang = r.api.Langs[0]
	}
	key := r.api.Name + "|" + lang + "|" + ip.String()
	if loc, found, ok := r.opts.Cache.get(key); ok {
		if !found {
			return Location{}, ErrNotFound
		}
		return loc, nil
	}
	// 上游请求不随调用方取消：可能还有合并进来的调用方在等，结果也要写入缓存
	ch := r.group.DoChan(key, func() (any, error) {
		loc, err := r.fetch(ip, lang)
		if err == nil || errors.Is(err, ErrNotFound) {
			r.opts.Cache.set(key, loc, err == nil)
		}
		return loc, err
	})
	select {
	case res := <-ch:
		return res.Val.(Location), res.Err
	case <-ctx.Done():
		return Location{}, ctx.Err()
	}
}

func (r *Remote) fetch(ip netip.Addr, lang string) (Location, error) {
	if !r.allow() {
		r.limited.Add(1)
		return Location{}, ErrLimited
	}
	r.requests.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), r.opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.api.URL(ip, lang, r.opts.Key), nil)
	if err != nil {
		return Location{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := r.opts.Client.Do(req)
	if err != nil {
		return Location{}, r.fail(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return Location{}, r.fail(err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		r.pause(retryAfter(resp.Header), "上游返回 429")
		return Location{}, ErrLimited
	default:
		return Location{}, r.fail(fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body[:min(len(body), 200)])))
	}
	if r.api.Exhausted != nil {
		if d := r.api.Exhausted(resp.Header); d > 0 {
			r.pause(d, "额度已用完")
		}
	}
	loc, err := r.api.Parse(body)
	if err == nil && loc.Country == "" {
		err = ErrNotFound
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Location{}, r.fail(fmt.Errorf("parse response: %w", err))
	}
	r.mu.Lock()
	r.fails, r.trips = 0, 0
	r.mu.Unlock()
	return loc, err
}

// allow 判断现在能否请求上游：不在暂停期，且限流器有令牌
func (r *Remote) allow() bool {
	r.mu.Lock()
	paused := time.Now().Before(r.paused)
	r.mu.Unlock()
	return !paused && r.limiter.Allow()
}

// fail 记录一次失败，连续失败达到阈值时熔断
func (r *Remote) fail(err error) error {
	r.failures.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fails++; r.fails >= breakerThreshold {
		r.fails = breakerThreshold - 1 // 暂停结束后只要再失败一次就重新熔断
		r.pauseLocked(min(breakerCooldown<<r.trips, maxBreakerCooldown), "连续失败: "+err.Error())
		r.trips = min(r.trips+1, 10)
	}
	return err
}

func (r *Remote) pause(d time.Duration, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pauseLocked(d, reason)
}

func (r *Remote) pauseLocked(d time.Duration, reason string) {
	d = min(d, maxPause)
	if until := time.Now().Add(d); until.After(r.paused) {
		r.paused = until
		slog.Warn("远程数据源暂停请求", "provider", r.api.Name, "duration", d.Round(time.Second), "reason", reason)
	}
}

func (r *Remote) stats(s *Stats) {
	s.Requests, s.Failures, s.Limited = r.requests.Load(), r.failures.Load(), r.limited.Load()
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Now().Before(r.paused) {
		s.PausedUntil = r.paused
	}
}

// retryAfter 解析 Retry-After 响应头（秒数或 HTTP 日期）
func retryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
		return time.Duration(sec) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && time.Until(t) > 0 {
		return time.Until(t)
	}
	return defaultBackoff
}
