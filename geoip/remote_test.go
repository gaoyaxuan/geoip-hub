package geoip

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/time/rate"
)

const okBody = `{"status":"success","country":"美国","countryCode":"US","regionName":"加利福尼亚州","city":"Mountain View","isp":"Google LLC","as":"AS15169 Google LLC"}`

var ip1, ip2 = netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("8.8.4.4")

// upstream 在内存中模拟上游 API，不发起真实网络请求
type upstream struct {
	calls   atomic.Int32
	respond func() (status int, header http.Header, body string)
}

func (u *upstream) RoundTrip(r *http.Request) (*http.Response, error) {
	u.calls.Add(1)
	status, header, body := u.respond()
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func ok() (int, http.Header, string) { return http.StatusOK, nil, okBody }

// newTestRemote 创建使用 ip-api 响应格式的远程数据源
func newTestRemote(u *upstream, opts RemoteOptions) *Remote {
	opts.Client = &http.Client{Transport: u}
	return NewRemote(IPAPI, opts)
}

func TestRemoteCachesResults(t *testing.T) {
	u := &upstream{respond: ok}
	r := newTestRemote(u, RemoteOptions{Cache: NewCache(100, time.Hour, time.Minute)})
	for range 3 {
		loc, err := r.Lookup(t.Context(), ip1, "zh-CN")
		if err != nil || loc.City != "Mountain View" || loc.ASN != 15169 || loc.Org != "Google LLC" {
			t.Fatalf("Lookup = %+v, %v", loc, err)
		}
	}
	if n := u.calls.Load(); n != 1 {
		t.Fatalf("上游请求 %d 次，应为 1 次", n)
	}
	r.Lookup(t.Context(), ip1, "en") // 不同语言分开缓存
	if n := u.calls.Load(); n != 2 {
		t.Fatalf("上游请求 %d 次，应为 2 次", n)
	}
}

func TestRemoteCachesNotFound(t *testing.T) {
	u := &upstream{respond: func() (int, http.Header, string) {
		return http.StatusOK, nil, `{"status":"fail","message":"reserved range"}`
	}}
	r := newTestRemote(u, RemoteOptions{Cache: NewCache(100, time.Hour, time.Minute)})
	for range 2 {
		if _, err := r.Lookup(t.Context(), ip1, "en"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	}
	if n := u.calls.Load(); n != 1 {
		t.Fatalf("上游请求 %d 次，应为 1 次", n)
	}
}

func TestRemoteMergesConcurrentRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		u := &upstream{respond: func() (int, http.Header, string) { <-release; return ok() }}
		r := newTestRemote(u, RemoteOptions{})
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				if _, err := r.Lookup(t.Context(), ip1, "en"); err != nil {
					t.Error(err)
				}
			})
		}
		synctest.Wait() // 20 个调用方都在等同一个上游请求
		close(release)
		wg.Wait()
		if n := u.calls.Load(); n != 1 {
			t.Fatalf("上游请求 %d 次，应为 1 次", n)
		}
	})
}

func TestRemoteRateLimit(t *testing.T) {
	u := &upstream{respond: ok}
	r := newTestRemote(u, RemoteOptions{Limit: rate.Every(time.Hour), Burst: 1})
	if _, err := r.Lookup(t.Context(), ip1, "en"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Lookup(t.Context(), ip2, "en"); !errors.Is(err, ErrLimited) {
		t.Fatalf("err = %v, want ErrLimited", err)
	}
	var st Stats
	r.stats(&st)
	if u.calls.Load() != 1 || st.Requests != 1 || st.Limited != 1 {
		t.Fatalf("calls = %d, stats = %+v", u.calls.Load(), st)
	}
}

func TestRemotePausesOn429(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var limited atomic.Bool
		limited.Store(true)
		u := &upstream{respond: func() (int, http.Header, string) {
			if limited.Load() {
				return http.StatusTooManyRequests, http.Header{"Retry-After": {"120"}}, ""
			}
			return ok()
		}}
		r := newTestRemote(u, RemoteOptions{})
		for _, ip := range []netip.Addr{ip1, ip2} {
			if _, err := r.Lookup(t.Context(), ip, "en"); !errors.Is(err, ErrLimited) {
				t.Fatalf("err = %v, want ErrLimited", err)
			}
		}
		if n := u.calls.Load(); n != 1 {
			t.Fatalf("暂停期间不应请求上游，实际请求 %d 次", n)
		}
		limited.Store(false)
		time.Sleep(120 * time.Second)
		if _, err := r.Lookup(t.Context(), ip2, "en"); err != nil {
			t.Fatalf("暂停结束后应恢复请求: %v", err)
		}
	})
}

func TestRemotePausesWhenQuotaExhausted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		u := &upstream{respond: func() (int, http.Header, string) {
			return http.StatusOK, http.Header{"X-Rl": {"0"}, "X-Ttl": {"30"}}, okBody
		}}
		r := newTestRemote(u, RemoteOptions{})
		if _, err := r.Lookup(t.Context(), ip1, "en"); err != nil {
			t.Fatal(err) // 本次成功，但额度已经用完
		}
		if _, err := r.Lookup(t.Context(), ip2, "en"); !errors.Is(err, ErrLimited) {
			t.Fatalf("err = %v, want ErrLimited", err)
		}
		time.Sleep(31 * time.Second)
		if _, err := r.Lookup(t.Context(), ip2, "en"); err != nil {
			t.Fatal(err)
		}
		if n := u.calls.Load(); n != 2 {
			t.Fatalf("上游请求 %d 次，应为 2 次", n)
		}
	})
}

func TestRemoteBreaker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var healthy atomic.Bool
		u := &upstream{respond: func() (int, http.Header, string) {
			if healthy.Load() {
				return ok()
			}
			return http.StatusBadGateway, nil, "bad gateway"
		}}
		r := newTestRemote(u, RemoteOptions{})
		lookup := func() error { _, err := r.Lookup(t.Context(), ip1, "en"); return err }
		failTimes := func(n int) {
			t.Helper()
			for range n {
				if err := lookup(); err == nil || errors.Is(err, ErrLimited) {
					t.Fatalf("err = %v, want 上游错误", err)
				}
			}
		}
		failTimes(breakerThreshold)
		if err := lookup(); !errors.Is(err, ErrLimited) {
			t.Fatalf("熔断后应直接跳过，err = %v", err)
		}
		if n := u.calls.Load(); n != breakerThreshold {
			t.Fatalf("上游请求 %d 次，应为 %d 次", n, breakerThreshold)
		}

		// 熔断结束后再失败一次就立即重新熔断，暂停时间翻倍为 60 秒
		time.Sleep(breakerCooldown)
		failTimes(1)
		time.Sleep(breakerCooldown)
		if err := lookup(); !errors.Is(err, ErrLimited) {
			t.Fatalf("再次熔断的暂停时间应翻倍，err = %v", err)
		}

		// 上游恢复后熔断状态清零，再次熔断仍只暂停 30 秒
		healthy.Store(true)
		time.Sleep(breakerCooldown)
		if err := lookup(); err != nil {
			t.Fatalf("上游恢复后应成功: %v", err)
		}
		healthy.Store(false)
		failTimes(breakerThreshold)
		time.Sleep(breakerCooldown)
		failTimes(1)
	})
}

func TestRemoteCallerCancelStillCaches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		u := &upstream{respond: func() (int, http.Header, string) { <-release; return ok() }}
		r := newTestRemote(u, RemoteOptions{Cache: NewCache(10, time.Hour, time.Minute)})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := r.Lookup(ctx, ip1, "en"); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		close(release)
		synctest.Wait() // 上游请求在后台完成并写入缓存
		if _, err := r.Lookup(t.Context(), ip1, "en"); err != nil {
			t.Fatal(err)
		}
		if n := u.calls.Load(); n != 1 {
			t.Fatalf("上游请求 %d 次，应为 1 次", n)
		}
	})
}

func TestIPAPIParse(t *testing.T) {
	loc, err := IPAPI.Parse([]byte(`{"status":"success","country":"中国","countryCode":"CN","regionName":"浙江","city":"杭州","lat":30.2943,"lon":120.1663,"timezone":"Asia/Shanghai","isp":"Hangzhou Alibaba Advertising Co","org":"Aliyun Computing Co., LTD","as":"AS37963 Hangzhou Alibaba Advertising Co.,Ltd."}`))
	want := Location{
		Country: "中国", CountryCode: "CN", Region: "浙江", City: "杭州",
		ISP: "Hangzhou Alibaba Advertising Co", ASN: 37963, Org: "Aliyun Computing Co., LTD",
		Latitude: 30.2943, Longitude: 120.1663, TimeZone: "Asia/Shanghai",
	}
	if err != nil || loc != want {
		t.Fatalf("Parse = %+v, %v", loc, err)
	}
	if _, err := IPAPI.Parse([]byte(`{"status":"fail","message":"private range"}`)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got := IPAPI.URL(ip1, "zh-CN", ""); !strings.HasPrefix(got, "http://ip-api.com/json/8.8.8.8?") {
		t.Fatalf("免费版 URL = %s", got)
	}
	if got := IPAPI.URL(ip1, "zh-CN", "k"); !strings.HasPrefix(got, "https://pro.ip-api.com/json/8.8.8.8?") || !strings.HasSuffix(got, "&key=k") {
		t.Fatalf("pro 版 URL = %s", got)
	}
}

func TestIPWhoisParse(t *testing.T) {
	loc, err := IPWhois.Parse([]byte(`{"ip":"114.114.114.114","success":true,"type":"IPv4","country":"中国","country_code":"CN","region":"江苏省","city":"南京","latitude":32.0616657,"longitude":118.7777759,"connection":{"asn":137702,"org":"NanJing XinFeng Information Technologies, Inc.","isp":"China Telecom","domain":"greatbit.com"},"timezone":{"id":"Asia/Shanghai"}}`))
	want := Location{
		Country: "中国", CountryCode: "CN", Region: "江苏省", City: "南京",
		ISP: "China Telecom", ASN: 137702, Org: "NanJing XinFeng Information Technologies, Inc.",
		Latitude: 32.0616657, Longitude: 118.7777759, TimeZone: "Asia/Shanghai",
	}
	if err != nil || loc != want {
		t.Fatalf("Parse = %+v, %v", loc, err)
	}
	if _, err := IPWhois.Parse([]byte(`{"ip":"192.168.1.1","success":false,"message":"Reserved range"}`)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestParseASN(t *testing.T) {
	for in, want := range map[string]struct {
		asn  uint
		name string
	}{
		"AS15169 Google LLC": {15169, "Google LLC"},
		"AS4134":             {4134, ""},
		"":                   {0, ""},
		"Unknown":            {0, "Unknown"},
	} {
		if asn, name := parseASN(in); asn != want.asn || name != want.name {
			t.Errorf("parseASN(%q) = %d, %q", in, asn, name)
		}
	}
}
