package geoip

import (
	"container/list"
	"sync"
	"time"
)

// Cache 是并发安全的 LRU 缓存，每个条目带过期时间。
// 远程数据源用它缓存查询结果（包括"查无数据"），同一 IP 在有效期内不会重复请求上游；多个数据源可以共享一个 Cache。
type Cache struct {
	mu           sync.Mutex
	size         int
	ttl, missTTL time.Duration
	items        map[string]*list.Element
	order        list.List // 最近使用的在前
	hits, misses uint64
}

type cacheEntry struct {
	key     string
	loc     Location
	found   bool
	expires time.Time
}

// CacheStats 是缓存的统计信息
type CacheStats struct {
	Size   int    `json:"size"`
	Hits   uint64 `json:"hits"`
	Misses uint64 `json:"misses"`
}

// NewCache 创建最多容纳 size 个条目的缓存：有数据的结果缓存 ttl，查无数据的结果缓存 missTTL。
// size <= 0 时返回 nil，表示不缓存。
func NewCache(size int, ttl, missTTL time.Duration) *Cache {
	if size <= 0 {
		return nil
	}
	return &Cache{size: size, ttl: ttl, missTTL: missTTL, items: make(map[string]*list.Element)}
}

// get 返回未过期的缓存，found 为 false 表示缓存的是"查无数据"
func (c *Cache) get(key string) (loc Location, found, ok bool) {
	if c == nil {
		return Location{}, false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, hit := c.items[key]; hit {
		e := el.Value.(*cacheEntry)
		if time.Now().Before(e.expires) {
			c.order.MoveToFront(el)
			c.hits++
			return e.loc, e.found, true
		}
		c.remove(el)
	}
	c.misses++
	return Location{}, false, false
}

func (c *Cache) set(key string, loc Location, found bool) {
	if c == nil {
		return
	}
	ttl := c.ttl
	if !found {
		ttl = c.missTTL
	}
	if ttl <= 0 {
		return
	}
	e := &cacheEntry{key: key, loc: loc, found: found, expires: time.Now().Add(ttl)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value = e
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(e)
	if c.order.Len() > c.size {
		c.remove(c.order.Back())
	}
}

func (c *Cache) remove(el *list.Element) {
	c.order.Remove(el)
	delete(c.items, el.Value.(*cacheEntry).key)
}

func (c *Cache) Stats() CacheStats {
	if c == nil {
		return CacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{Size: c.order.Len(), Hits: c.hits, Misses: c.misses}
}
