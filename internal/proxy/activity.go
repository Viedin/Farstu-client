package proxy

import (
	"slices"
	"sync"
	"time"
)

// Incident is one visitor guessing passwords or hunting for pages, until
// they go quiet for incidentGap.
type Incident struct {
	Host        string // "" for names not turned on
	Visitor     string // IP, or /64 for IPv6
	SignIn      bool   // sign-ins blocked by the limit, else pages not found
	First, Last time.Time
	Count       int
}

const (
	window       = 24 * time.Hour
	incidentGap  = 30 * time.Minute
	scanMin      = 20 // pages not found before a visitor shows as scanning; phones miss a few too
	maxIncidents = 1000
)

// incidents of the last day. Kept in memory only: visitor IPs never leave
// this server.
type incidents struct {
	mu   sync.Mutex
	now  func() time.Time
	list []Incident
}

// flag counts a blocked sign-in or a page not found against visitor.
func (a *incidents) flag(host, visitor string, signIn bool) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	i := slices.IndexFunc(a.list, func(in Incident) bool {
		return in.Host == host && in.Visitor == visitor && in.SignIn == signIn && now.Sub(in.Last) < incidentGap
	})
	if i < 0 {
		a.list = slices.DeleteFunc(a.list, func(in Incident) bool { return now.Sub(in.Last) >= window })
		if len(a.list) >= maxIncidents {
			// ponytail: a scan from many addresses pushes out the oldest
			// scans, never sign-ins; cap sign-ins too if one fills it alone.
			j := max(slices.IndexFunc(a.list, func(in Incident) bool { return !in.SignIn }), 0)
			a.list = slices.Delete(a.list, j, j+1)
		}
		a.list = append(a.list, Incident{Host: host, Visitor: visitor, SignIn: signIn, First: now})
		i = len(a.list) - 1
	}
	a.list[i].Last = now
	a.list[i].Count++
}

// shown returns what needs seeing: every blocked sign-in, and scans,
// newest first.
func (a *incidents) shown() []Incident {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	var s []Incident
	for _, in := range a.list {
		if now.Sub(in.Last) < window && (in.SignIn || in.Count >= scanMin) {
			s = append(s, in)
		}
	}
	slices.SortFunc(s, func(a, b Incident) int { return b.Last.Compare(a.Last) })
	return s
}
