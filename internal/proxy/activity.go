package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
)

// Activity is what one visitor did at one host on one day that may be
// password guessing or scanning. Ordinary visits aren't kept.
type Activity struct {
	Day      time.Time // local midnight
	Host     string    // "" for names not turned on
	Asked    string    // the name asked for when Host is "", at most 253 bytes
	Visitor  string    // IP, or /64 for IPv6
	Failed   int       // sign-ins the app refused
	Blocked  int       // sign-ins blocked by the limit
	NotFound int       // pages not found
	Last     time.Time
}

func (a Activity) key() string {
	k := a.Day.Format(time.DateOnly) + "|" + a.Host + "|" + a.Visitor
	if a.Asked != "" { // keys saved before Asked existed stay the same
		k += "|" + a.Asked
	}
	return k
}

func (a Activity) signIn() bool { return a.Failed+a.Blocked > 0 }

type kind int

const (
	failed kind = iota
	blocked
	notFound
)

const (
	window      = 30 * 24 * time.Hour
	maxActivity = 10_000
	// ScanMin is how many pages not found make a visitor show as scanning;
	// phones miss a few too.
	ScanMin = 20
)

var bucket = []byte("activity")

// activity of the last 30 days. Visitor IPs never leave this server: kept
// in memory, saved to the local database by Keep.
type activity struct {
	mu      sync.Mutex
	now     func() time.Time
	by      map[string]*Activity
	changed bool // since the last save
}

func newActivity() activity {
	return activity{now: time.Now, by: map[string]*Activity{}}
}

// flag counts a sign-in or a page not found against visitor.
func (s *activity) flag(host, visitor string, k kind) {
	s.count(Activity{Host: host, Visitor: visitor}, k)
}

// flagUnknown counts a request for a name not turned on, keeping the name
// so the owner can tell a stale bookmark from someone guessing names.
func (s *activity) flagUnknown(asked, visitor string) {
	s.count(Activity{Asked: asked[:min(len(asked), 253)], Visitor: visitor}, notFound) // visitors pick the name: longest DNS name
}

func (s *activity) count(a Activity, k kind) {
	now := s.now()
	y, m, d := now.Date()
	a.Day = time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _ := s.add(a)
	switch k {
	case failed:
		p.Failed++
	case blocked:
		p.Blocked++
	default:
		p.NotFound++
	}
	p.Last = now
}

// add returns the entry for a's key, adding a if there is none. Callers
// hold s.mu.
func (s *activity) add(a Activity) (p *Activity, added bool) {
	s.changed = true
	if p := s.by[a.key()]; p != nil {
		return p, false
	}
	if len(s.by) >= maxActivity {
		s.evict()
	}
	s.by[a.key()] = &a
	return &a, true
}

// evict drops the oldest entry, sign-ins last.
// ponytail: a scan from many addresses pushes out the oldest scans, never
// sign-ins, at a full pass per new visitor once full; cap sign-ins too or
// index by age if one fills it alone.
func (s *activity) evict() {
	var victim *Activity
	for _, a := range s.by {
		if victim == nil || victim.signIn() && !a.signIn() || victim.signIn() == a.signIn() && a.Last.Before(victim.Last) {
			victim = a
		}
	}
	delete(s.by, victim.key())
}

// all returns the activity of the last 30 days, newest first.
func (s *activity) all() []Activity {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var l []Activity
	for _, a := range s.by {
		if now.Sub(a.Last) < window {
			l = append(l, *a)
		}
	}
	slices.SortFunc(l, func(a, b Activity) int { return b.Last.Compare(a.Last) })
	return l
}

// load adds the activity saved in db to what was flagged since starting.
// Unreadable entries are dropped: this is history, nothing is turned on or
// off by it.
func (s *activity) load(db *bbolt.DB) error {
	return db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return b.ForEach(func(k, v []byte) error {
			var a Activity
			if err := json.Unmarshal(v, &a); err != nil || a.key() != string(k) || a.Failed < 0 || a.Blocked < 0 || a.NotFound < 0 || a.Failed+a.Blocked+a.NotFound == 0 {
				slog.Warn("dropping unreadable activity", "key", string(k), "err", err)
				s.changed = true
				return nil
			}
			if p, added := s.add(a); !added { // flagged before loading
				p.Failed += a.Failed
				p.Blocked += a.Blocked
				p.NotFound += a.NotFound
			}
			return nil
		})
	})
}

// save forgets what is older than the window and, if anything changed,
// rewrites the bucket with the rest.
func (s *activity) save(db *bbolt.DB) error {
	now := s.now()
	s.mu.Lock()
	for k, a := range s.by {
		if now.Sub(a.Last) >= window {
			delete(s.by, k)
			s.changed = true
		}
	}
	if !s.changed {
		s.mu.Unlock()
		return nil
	}
	put := map[string][]byte{}
	for k, a := range s.by {
		put[k], _ = json.Marshal(a)
	}
	s.changed = false
	s.mu.Unlock()
	err := db.Update(func(tx *bbolt.Tx) error {
		if tx.Bucket(bucket) != nil {
			if err := tx.DeleteBucket(bucket); err != nil {
				return err
			}
		}
		b, err := tx.CreateBucket(bucket)
		if err != nil {
			return err
		}
		for k, v := range put {
			if err := b.Put([]byte(k), v); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.mu.Lock()
		s.changed = true // again next time
		s.mu.Unlock()
	}
	return err
}

// Keep loads the activity saved in the database at path, saves it there
// every minute and once more when ctx is done. A database that can't be
// read is set aside; without one, activity is kept in memory only. Returns
// once ctx is done; call it once per Router.
func (rt *Router) Keep(ctx context.Context, path string) {
	// bbolt panics on some damaged files. History must not take the
	// tunnel down with it.
	defer func() {
		if p := recover(); p != nil {
			setAside(path, p) // the next start begins a new one
			<-ctx.Done()
		}
	}()
	open := func() (*bbolt.DB, error) { return bbolt.Open(path, 0o600, &bbolt.Options{Timeout: time.Second}) }
	db, err := open()
	// bbolt tells damage apart from other failures only in part ("file size
	// too small" is no sentinel). Setting aside keeps the old file, so any
	// failure but another client holding it gets a new one.
	if err != nil && !errors.Is(err, berrors.ErrTimeout) {
		setAside(path, err)
		db, err = open()
	}
	if err != nil {
		slog.Error("activity can't be saved and is lost on restart", "path", path, "err", err)
		<-ctx.Done()
		return
	}
	defer db.Close()
	if err := rt.seen.load(db); err != nil {
		slog.Error("could not load saved activity", "err", err)
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for done := false; !done; {
		select {
		case <-t.C:
		case <-ctx.Done():
			done = true
		}
		if err := rt.seen.save(db); err != nil {
			slog.Error("could not save activity", "err", err)
		}
	}
}

// setAside renames a broken database, so a new one can start.
func setAside(path string, why any) {
	slog.Error("activity database is broken, setting it aside", "path", path, "err", why)
	if err := os.Rename(path, path+".broken"); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("could not set the activity database aside", "path", path, "err", err)
	}
}
