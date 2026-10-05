package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/template"
)

// CallbackLimitMatcher evaluates an Ansible limit pattern over an inventory document, the way
// ansible-inventory does. The dispatcher satisfies it.
type CallbackLimitMatcher interface {
	// LimitHosts returns the hosts of content that limit selects.
	LimitHosts(ctx context.Context, content, limit string) ([]string, error)
}

// Bounds on evaluating a template's limit for a callback.
const (
	// limitCacheTTL is how long an evaluation is reused. A fleet booting at once asks the same
	// question of the same inventory many times in a few seconds, and an edit to the inventory or
	// the limit is a different question, so a short life is all the reuse needs.
	limitCacheTTL = 30 * time.Second
	// limitCheckTimeout bounds one evaluation.
	limitCheckTimeout = 30 * time.Second
	// limitCheckWorkers is how many evaluations run at once, each one an ansible-inventory process.
	limitCheckWorkers = 4
	// maxLimitCacheEntries bounds the cache, past which expired entries are pruned.
	maxLimitCacheEntries = 256
)

// limitCache remembers briefly which hosts a limit selects in an inventory, so a fleet booting at
// once runs ansible-inventory once rather than once per host. An entry is keyed by a digest of the
// inventory's content and by the limit, so an edit to either asks Ansible again. A failed
// evaluation is never kept.
type limitCache struct {
	// matcher evaluates a limit when the cache has no live answer.
	matcher CallbackLimitMatcher
	// sem bounds how many evaluations run at once.
	sem chan struct{}
	// mu guards entries.
	mu sync.Mutex
	// entries maps a content digest and limit to its evaluation.
	entries map[string]*limitEntry
	// now reads the clock. Nil means time.Now.
	now func() time.Time
}

// limitEntry is one evaluation, finished once done is closed.
type limitEntry struct {
	// done is closed when hosts and err are set.
	done chan struct{}
	// hosts are the hosts the limit selected.
	hosts []string
	// err is why the evaluation failed.
	err error
	// expires is when the answer stops being reused.
	expires time.Time
}

// newLimitCache returns a cache over m.
func newLimitCache(m CallbackLimitMatcher) *limitCache {
	return &limitCache{matcher: m, sem: make(chan struct{}, limitCheckWorkers),
		entries: make(map[string]*limitEntry)}
}

// clock returns the cache's time source, defaulting to the real one.
func (lc *limitCache) clock() time.Time {
	if lc.now != nil {
		return lc.now()
	}
	return time.Now()
}

// hosts returns the hosts limit selects in content, from a live entry when there is one. A caller
// arriving while the same evaluation runs waits for it rather than starting another.
func (lc *limitCache) hosts(ctx context.Context, content, limit string) ([]string, error) {
	sum := sha256.Sum256([]byte(content))
	key := hex.EncodeToString(sum[:]) + "\x00" + limit
	lc.mu.Lock()
	if e, ok := lc.entries[key]; ok {
		select {
		case <-e.done:
			if e.err == nil && lc.clock().Before(e.expires) {
				lc.mu.Unlock()
				return e.hosts, nil
			}
		default:
			lc.mu.Unlock()
			select {
			case <-e.done:
				return e.hosts, e.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	if len(lc.entries) >= maxLimitCacheEntries {
		now := lc.clock()
		for k, e := range lc.entries {
			select {
			case <-e.done:
				if !now.Before(e.expires) {
					delete(lc.entries, k)
				}
			default:
			}
		}
	}
	e := &limitEntry{done: make(chan struct{})}
	lc.entries[key] = e
	lc.mu.Unlock()

	// The evaluation is not tied to the request that started it: a host that gives up waiting must
	// not fail every other host waiting on the same answer.
	evalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), limitCheckTimeout)
	defer cancel()
	select {
	case lc.sem <- struct{}{}:
		e.hosts, e.err = lc.matcher.LimitHosts(evalCtx, content, limit)
		<-lc.sem
	case <-evalCtx.Done():
		e.err = evalCtx.Err()
	}
	e.expires = lc.clock().Add(limitCacheTTL)
	close(e.done)
	if e.err != nil {
		lc.mu.Lock()
		if lc.entries[key] == e {
			delete(lc.entries, key)
		}
		lc.mu.Unlock()
	}
	return e.hosts, e.err
}

// limitRefusal returns the status and reason the template's own limit refuses host, or an empty
// reason when it does not. A template with no limit, or one whose callbacks replace the limit as
// AWX's do, refuses nothing. Otherwise the host must be one the limit selects in the stored
// inventory, decided by Ansible's own pattern rules, and a limit that cannot be evaluated refuses
// the callback rather than guessing either way.
func (c *callbacks) limitRefusal(ctx context.Context, t *template.Template, inv *inventory.Inventory,
	host string) (int, string) {
	limit := strings.TrimSpace(t.Limit)
	mode := template.NormalizeCallbackLimit(t.CallbackLimit)
	if limit == "" || mode == template.CallbackLimitReplace {
		return 0, ""
	}
	if c.limits == nil {
		c.log.Warn("server: provisioning callback refused: its template keeps its limit and this "+
			"server cannot run ansible-inventory to check it", zap.String("template", t.ID))
		return http.StatusConflict, "this template keeps its own limit on callbacks, and checking " +
			"the calling host against it needs ansible-inventory on this server. Install " +
			"ansible-core, or set the template's callback_limit to replace"
	}
	hosts, err := c.limits.hosts(ctx, inv.Content, limit)
	if err != nil {
		// What Ansible printed is not logged: an inventory that does not parse can be quoted back
		// in it, and the inventory may hold a password.
		c.log.Warn("server: provisioning callback refused: the template's limit could not be "+
			"evaluated", zap.String("template", t.ID), zap.String("limit", limit))
		return http.StatusServiceUnavailable, "the calling host could not be checked against " +
			"this template's limit, so the callback is refused. Call back again shortly"
	}
	if !slices.Contains(hosts, host) {
		c.log.Warn("server: provisioning callback refused: the calling host is outside its "+
			"template's limit", zap.String("template", t.ID), zap.String("host", host),
			zap.String("limit", limit))
		return http.StatusForbidden, "the calling host " + host + " is outside this template's " +
			"limit, and the template keeps its limit on callbacks (callback_limit intersect). " +
			"Set callback_limit to replace to launch for any matched host, as AWX does"
	}
	return 0, ""
}
