package geoip

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
)

type fakeProvider struct {
	name   string
	calls  atomic.Int32
	lookup func(ip netip.Addr, lang string) (Location, error)
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Lookup(_ context.Context, ip netip.Addr, lang string) (Location, error) {
	f.calls.Add(1)
	return f.lookup(ip, lang)
}

func returns(loc Location, err error) func(netip.Addr, string) (Location, error) {
	return func(netip.Addr, string) (Location, error) { return loc, err }
}

func TestResolverFallback(t *testing.T) {
	broken := &fakeProvider{name: "broken", lookup: returns(Location{}, errors.New("timeout"))}
	empty := &fakeProvider{name: "empty", lookup: returns(Location{}, ErrNotFound)}
	good := &fakeProvider{name: "good", lookup: returns(Location{Country: "美国", CountryCode: "US"}, nil)}
	r := NewResolver(Step{Provider: broken}, Step{Provider: empty}, Step{Provider: good})

	loc, err := r.Lookup(t.Context(), ip1, "")
	if err != nil || loc.Source != "good" || loc.IP != "8.8.8.8" || loc.Country != "美国" {
		t.Fatalf("Lookup = %+v, %v", loc, err)
	}
	if st := r.Stats(); len(st) != 3 || st[2].Served != 1 || st[0].Served != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestResolverCountryPreference(t *testing.T) {
	cnIP := netip.MustParseAddr("114.114.114.114")
	ip2region := &fakeProvider{name: "ip2region", lookup: func(ip netip.Addr, _ string) (Location, error) {
		if ip == cnIP {
			return Location{Country: "中国", CountryCode: "CN", City: "南京市"}, nil
		}
		return Location{Country: "United States", CountryCode: "US"}, nil
	}}
	geolite := &fakeProvider{name: "geolite", lookup: returns(Location{Country: "美国", CountryCode: "US"}, nil)}
	r := NewResolver(Step{Provider: ip2region, Country: "CN"}, Step{Provider: geolite})

	if loc, _ := r.Lookup(t.Context(), cnIP, ""); loc.Source != "ip2region" || geolite.calls.Load() != 0 {
		t.Fatalf("国内 IP 应直接用 ip2region: %+v", loc)
	}
	if loc, _ := r.Lookup(t.Context(), ip1, ""); loc.Source != "geolite" || loc.Country != "美国" {
		t.Fatalf("海外 IP 应交给 geolite: %+v", loc)
	}
	geolite.lookup = returns(Location{}, ErrNotFound)
	if loc, _ := r.Lookup(t.Context(), ip1, ""); loc.Source != "ip2region" || loc.Country != "United States" {
		t.Fatalf("后面都查不到时应用暂存的结果兜底: %+v", loc)
	}
}

func TestResolverSkipsBogon(t *testing.T) {
	p := &fakeProvider{name: "p", lookup: returns(Location{Country: "X"}, nil)}
	r := NewResolver(Step{Provider: p})
	loc, err := r.Lookup(t.Context(), netip.MustParseAddr("192.168.1.1"), "")
	if err != nil || !loc.Bogon || loc.IP != "192.168.1.1" || p.calls.Load() != 0 {
		t.Fatalf("Lookup = %+v, %v, calls = %d", loc, err, p.calls.Load())
	}
}

func TestResolverErrors(t *testing.T) {
	empty := &fakeProvider{name: "empty", lookup: returns(Location{}, ErrNotFound)}
	r := NewResolver(Step{Provider: empty})
	if loc, err := r.Lookup(t.Context(), ip1, ""); !errors.Is(err, ErrNotFound) || loc.IP != "8.8.8.8" {
		t.Fatalf("Lookup = %+v, %v, want ErrNotFound", loc, err)
	}

	boom := errors.New("boom")
	r = NewResolver(Step{Provider: empty}, Step{Provider: &fakeProvider{name: "broken", lookup: returns(Location{}, boom)}})
	if _, err := r.Lookup(t.Context(), ip1, ""); !errors.Is(err, boom) || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want boom", err)
	}
}

func TestResolverLookupFrom(t *testing.T) {
	a := &fakeProvider{name: "a", lookup: returns(Location{Country: "A"}, nil)}
	b := &fakeProvider{name: "b", lookup: returns(Location{Country: "B"}, nil)}
	r := NewResolver(Step{Provider: a}, Step{Provider: b})
	if loc, err := r.LookupFrom(t.Context(), "b", ip1, ""); err != nil || loc.Source != "b" {
		t.Fatalf("LookupFrom = %+v, %v", loc, err)
	}
	if _, err := r.LookupFrom(t.Context(), "nope", ip1, ""); !errors.Is(err, ErrUnknownSource) {
		t.Fatalf("err = %v, want ErrUnknownSource", err)
	}
}

func TestResolverNormalizesInput(t *testing.T) {
	var gotIP netip.Addr
	var gotLang string
	p := &fakeProvider{name: "p", lookup: func(ip netip.Addr, lang string) (Location, error) {
		gotIP, gotLang = ip, lang
		return Location{Country: "X"}, nil
	}}
	r := NewResolver(Step{Provider: p})
	r.Lookup(t.Context(), netip.MustParseAddr("::ffff:8.8.8.8"), "en-US,en;q=0.9")
	if gotIP != ip1 || gotLang != "en" {
		t.Fatalf("ip = %s, lang = %s", gotIP, gotLang)
	}
	r.Lookup(t.Context(), ip1, "")
	if gotLang != DefaultLang {
		t.Fatalf("lang = %s, want %s", gotLang, DefaultLang)
	}
}
