package fusiondata

import (
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
)

// pathCache is a TTL-based cache mapping (parentID, childName) -> NavItem.
// It avoids repeated API calls when rclone traverses the same directories.
type pathCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]cacheEntry
}

type cacheEntry struct {
	item    NavItem
	expires time.Time
}

func newPathCache(ttl time.Duration) *pathCache {
	return &pathCache{
		ttl:     ttl,
		entries: make(map[string]cacheEntry),
	}
}

func cacheKey(parentID, childName string) string {
	return parentID + "/" + childName
}

// getChild returns a cached NavItem or nil if not found/expired.
func (c *pathCache) getChild(parentID, childName string) *NavItem {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := cacheKey(parentID, childName)
	entry, ok := c.entries[key]
	if !ok || time.Now().After(entry.expires) {
		if ok {
			delete(c.entries, key)
		}
		return nil
	}
	item := entry.item // copy
	return &item
}

// putChild stores a NavItem in the cache.
func (c *pathCache) putChild(parentID, childName string, item *NavItem) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := cacheKey(parentID, childName)
	c.entries[key] = cacheEntry{
		item:    *item,
		expires: time.Now().Add(c.ttl),
	}
}

// replaceChildren atomically removes all existing children of a parent
// and replaces them with the given items.
func (c *pathCache) replaceChildren(parentID string, items []NavItem) {
	fs.Debugf(nil, "cache.replaceChildren: parentID=%q count=%d", parentID, len(items))
	c.mu.Lock()
	defer c.mu.Unlock()

	// Delete all existing children of this parent.
	prefix := parentID + "/"
	for key := range c.entries {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			delete(c.entries, key)
		}
	}

	// Add all new children.
	for i := range items {
		key := cacheKey(parentID, items[i].Name)
		c.entries[key] = cacheEntry{item: items[i], expires: time.Now().Add(c.ttl)}
	}
}

// invalidate removes all cached children of a parent.
func (c *pathCache) invalidate(parentID string) {
	fs.Debugf(nil, "cache.invalidate: parentID=%q", parentID)
	c.mu.Lock()
	defer c.mu.Unlock()

	prefix := parentID + "/"
	for key := range c.entries {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			delete(c.entries, key)
		}
	}
}
