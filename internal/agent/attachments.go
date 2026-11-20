// Attachment retention: every downloaded image/voice lands in
// dataDir/attachments content-addressed (hash of key/URL) and nothing
// ever removed them — the one unbounded-growth point the earlier caps
// (sessions, usage, dedup, cli-sessions) missed. These files are
// re-fetched on demand, so deleting an aged one only costs a
// re-download if the same attachment is needed again, never a broken
// turn in flight (its bytes are already read).
package agent

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"
)

// sweepAttachments removes plain files under dir older than maxAge
// and reports how many went and how many bytes that reclaimed.
// Subdirectories (none today) are left alone; errors on individual
// files are skipped silently — the sweep is best-effort hygiene.
func sweepAttachments(dir string, maxAge time.Duration) (files, bytes int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0 // no dir yet — nothing downloaded, ever
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		size := info.Size()
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			continue
		}
		files++
		bytes += size
	}
	return files, bytes
}

// sweepLoop runs one sweep immediately, then every interval, until ctx
// is done. Split from StartAttachmentSweeper so tests can drive it
// with a tiny interval.
func sweepLoop(ctx context.Context, dir string, maxAge, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if n, b := sweepAttachments(dir, maxAge); n > 0 {
			log.Printf("attachment sweep: removed %d files (%.1f MB), retention %s", n, float64(b)/1e6, maxAge)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// StartAttachmentSweeper launches the retention loop for
// dataDir/attachments. Retention comes from cfg.AttachmentRetention
// (defaulted by config.Load to 72h); "0" disables sweeping entirely.
func (r *Runtime) StartAttachmentSweeper(ctx context.Context) {
	ret, err := time.ParseDuration(r.cfg.AttachmentRetention)
	if err != nil || ret <= 0 {
		return
	}
	go sweepLoop(ctx, filepath.Join(r.dataDir, "attachments"), ret, time.Hour)
}
