package edmproxy

import (
	"net/http"
	"path"
	"strings"
	"sync"
)

// imageCache keeps the images of a device's web interface in memory.
//
// The proxy path contains a new token each time a device is opened, so the
// browser cache never hits and every EDM start loads a few hundred images
// again. The switch's web server opens one connection per request and refuses
// connections for a while after bursts. The images never change for a given
// firmware; they are served from here after the first load. The cache of a
// device is dropped when the device changes, and images are served with the
// same isolation headers as everything else below /d/{token}/.
type imageCache struct {
	mu    sync.Mutex
	items map[int64]map[string]cachedImage
	size  map[int64]int
}

type cachedImage struct {
	ctype string
	body  []byte
}

const (
	maxCachedImage = 256 << 10
	maxImageCache  = 8 << 20 // per device
)

func newImageCache() *imageCache {
	return &imageCache{items: map[int64]map[string]cachedImage{}, size: map[int64]int{}}
}

// cacheableImage reports whether a request asks for an image the cache may keep.
func cacheableImage(r *http.Request, sub string) bool {
	if r.Method != http.MethodGet || r.URL.RawQuery != "" {
		return false
	}
	switch strings.ToLower(path.Ext(sub)) {
	case ".gif", ".png", ".jpg", ".jpeg", ".ico", ".bmp":
		return true
	}
	return false
}

func (c *imageCache) get(device int64, sub string) (cachedImage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	img, ok := c.items[device][sub]
	return img, ok
}

func (c *imageCache) put(device int64, sub, ctype string, body []byte) {
	if len(body) > maxCachedImage || !strings.HasPrefix(ctype, "image/") {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.items[device]
	if m == nil {
		m = map[string]cachedImage{}
		c.items[device] = m
	}
	if _, ok := m[sub]; ok || c.size[device]+len(body) > maxImageCache {
		return
	}
	m[sub] = cachedImage{ctype: ctype, body: body}
	c.size[device] += len(body)
}

func (c *imageCache) forget(device int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, device)
	delete(c.size, device)
}
