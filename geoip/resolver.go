package geoip

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// Step 是查询链中的一环
type Step struct {
	Provider Provider
	// Country 非空时（ISO 代码，如 "CN"），只有属于该国家的结果才直接采用；
	// 其他结果先暂存，链上后面的数据源都没有结果时再拿来兜底。
	// 例如 ip2region@CN：国内 IP 用 ip2region，海外 IP 交给后面的 GeoLite2。
	Country string
}

func (s Step) String() string {
	if s.Country == "" {
		return s.Provider.Name()
	}
	return s.Provider.Name() + "@" + s.Country
}

// Resolver 按查询链依次查询：数据源出错、限流、熔断或没有数据时自动降级到下一个
type Resolver struct {
	steps     []Step
	providers map[string]Provider
	served    map[string]*atomic.Uint64 // 各数据源作为最终结果返回的次数
}

func NewResolver(steps ...Step) *Resolver {
	r := &Resolver{steps: steps, providers: map[string]Provider{}, served: map[string]*atomic.Uint64{}}
	for _, s := range steps {
		r.providers[s.Provider.Name()] = s.Provider
		r.served[s.Provider.Name()] = new(atomic.Uint64)
	}
	return r
}

// Lookup 依次查询链上的数据源，返回第一个可用的结果。
// 内网等保留地址直接返回 Bogon 结果；所有数据源都没有数据时返回 ErrNotFound。
func (r *Resolver) Lookup(ctx context.Context, ip netip.Addr, lang string) (Location, error) {
	return r.lookup(ctx, r.steps, ip, lang)
}

// LookupFrom 跳过查询链，只查询指定名称的数据源
func (r *Resolver) LookupFrom(ctx context.Context, source string, ip netip.Addr, lang string) (Location, error) {
	p, ok := r.providers[source]
	if !ok {
		return Location{}, fmt.Errorf("%w: %q", ErrUnknownSource, source)
	}
	return r.lookup(ctx, []Step{{Provider: p}}, ip, lang)
}

func (r *Resolver) lookup(ctx context.Context, steps []Step, ip netip.Addr, lang string) (Location, error) {
	ip = ip.Unmap().WithZone("")
	if IsBogon(ip) {
		return Location{IP: ip.String(), Bogon: true}, nil
	}
	if lang = NormalizeLang(lang); lang == "" {
		lang = DefaultLang
	}
	var (
		fallback *Location
		errs     []error
	)
	for _, s := range steps {
		loc, err := s.Provider.Lookup(ctx, ip, lang)
		if err != nil {
			if !errors.Is(err, ErrNotFound) {
				errs = append(errs, fmt.Errorf("%s: %w", s.Provider.Name(), err))
			}
			continue
		}
		loc.IP, loc.Source = ip.String(), s.Provider.Name()
		if s.Country != "" && !strings.EqualFold(loc.CountryCode, s.Country) {
			if fallback == nil {
				fallback = &loc
			}
			continue
		}
		return r.serve(loc), nil
	}
	if fallback != nil {
		return r.serve(*fallback), nil
	}
	if len(errs) > 0 {
		return Location{IP: ip.String()}, errors.Join(errs...)
	}
	return Location{IP: ip.String()}, ErrNotFound
}

func (r *Resolver) serve(loc Location) Location {
	r.served[loc.Source].Add(1)
	return loc
}

// Chain 返回查询链，如 ["ipapi", "ip2region@CN", "geolite"]
func (r *Resolver) Chain() []string {
	chain := make([]string, len(r.steps))
	for i, s := range r.steps {
		chain[i] = s.String()
	}
	return chain
}

// Stats 是数据源的运行统计，Requests 之后的字段只有远程数据源才有
type Stats struct {
	Name        string    `json:"name"`
	Served      uint64    `json:"served"`                // 作为最终结果返回的次数
	Requests    uint64    `json:"requests,omitempty"`    // 实际发出的请求数（不含缓存命中）
	Failures    uint64    `json:"failures,omitempty"`    // 请求失败次数
	Limited     uint64    `json:"limited,omitempty"`     // 因限流或暂停被跳过的次数
	PausedUntil time.Time `json:"paused_until,omitzero"` // 熔断或额度用完，暂停到何时
}

func (r *Resolver) Stats() []Stats {
	var list []Stats
	for _, s := range r.steps {
		name := s.Provider.Name()
		if slices.ContainsFunc(list, func(st Stats) bool { return st.Name == name }) {
			continue
		}
		st := Stats{Name: name, Served: r.served[name].Load()}
		if remote, ok := s.Provider.(*Remote); ok {
			remote.stats(&st)
		}
		list = append(list, st)
	}
	return list
}

// Close 关闭实现了 io.Closer 的数据源
func (r *Resolver) Close() error {
	var errs []error
	for _, p := range r.providers {
		if c, ok := p.(io.Closer); ok {
			errs = append(errs, c.Close())
		}
	}
	return errors.Join(errs...)
}
