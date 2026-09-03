package nano

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Work cache, the NanChat way: every work value is remembered by the root it
// was computed for, and the moment a block is accepted the work for the
// *next* block (whose root is the new frontier) is requested in the
// background. The next send then finds its work ready instead of waiting on
// a work server or, worse, minutes of CPU on a small VPS.

const workCacheMax = 64

var workCache = struct {
	sync.Mutex
	loaded bool
	m      map[string]string // root hex → work hex
}{m: map[string]string{}}

func workCachePath() string {
	home := os.Getenv("SAIL_HOME")
	if home == "" {
		home = "."
	}
	return filepath.Join(home, "work-cache.json")
}

func workCacheLoad() {
	if workCache.loaded {
		return
	}
	workCache.loaded = true
	if b, err := os.ReadFile(workCachePath()); err == nil {
		json.Unmarshal(b, &workCache.m)
	}
}

// CachedWork returns remembered work for root, if any.
func CachedWork(root [32]byte, threshold uint64) (string, bool) {
	workCache.Lock()
	defer workCache.Unlock()
	workCacheLoad()
	w, ok := workCache.m[hexUpper(root[:])]
	if ok && !ValidWork(root, w, threshold) {
		delete(workCache.m, hexUpper(root[:]))
		return "", false
	}
	return w, ok
}

// RememberWork stores work for root and persists the cache.
func RememberWork(root [32]byte, work string) {
	workCache.Lock()
	defer workCache.Unlock()
	workCacheLoad()
	if len(workCache.m) >= workCacheMax {
		workCache.m = map[string]string{} // simple garbage collection, as NanChat does
	}
	workCache.m[hexUpper(root[:])] = work
	if b, err := json.Marshal(workCache.m); err == nil {
		os.WriteFile(workCachePath(), b, 0o600)
	}
}

// PrecomputeWork requests work for root in the background and caches it.
// Called with the hash of a block that was just accepted: that hash is the
// root of this account's next block. Send difficulty is used so the value
// serves a send or a receive alike.
func (a *Account) PrecomputeWork(root [32]byte) {
	go func() {
		if _, ok := CachedWork(root, SendThreshold); ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if w, err := a.Client.WorkGenerate(ctx, hexUpper(root[:]), SendThreshold); err == nil && ValidWork(root, w, SendThreshold) {
			RememberWork(root, w)
		}
	}()
}

func hexUpper(b []byte) string { return strings.ToUpper(hex.EncodeToString(b)) }
