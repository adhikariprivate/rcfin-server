package imageresize

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Cache entries are hex identity hashes, optionally followed by
// ":<width>x<height>q=<quality>" for resized variants and ".<pid>" for
// in-progress temporary files.
var isCacheFile = regexp.MustCompile(`^[0-9a-f]{32}(:[0-9]+x[0-9]+q=[0-9]+)?$`)

// CleanCache periodically evicts cache files that have not been accessed
// within maxAge, then sleeps for interval. It only touches files matching
// the current identity-hash naming scheme, so entries from other tools are
// left alone. Pass maxAge <= 0 to disable eviction.
func (r *Resizer) CleanCache(maxAge, interval time.Duration) {
	for {
		if maxAge > 0 && r.cachedir != "" {
			cutoff := time.Now().Add(-maxAge)
			entries, err := os.ReadDir(r.cachedir)
			if err != nil {
				log.Printf("image cache cleanup: %v", err)
			} else {
				for _, entry := range entries {
					if entry.IsDir() {
						continue
					}
					name := entry.Name()
					base := strings.TrimSuffix(name, r.tmpExt)
					if !isCacheFile.MatchString(base) {
						continue
					}
					info, err := entry.Info()
					if err != nil {
						continue
					}
					if info.ModTime().Before(cutoff) {
						_ = os.Remove(filepath.Join(r.cachedir, name))
					}
				}
			}
		}
		time.Sleep(interval)
	}
}
