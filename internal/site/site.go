// Package site knows how to list and download statements from each carrier's
// bill-history page.
package site

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"expenses/internal/browser"
)

// Statement is one bill on the history page.
type Statement struct {
	Date   time.Time // statement / billing date; used for the file name
	Amount string
	Label  string // e.g. billing period, for display
	Ref    string // site-specific handle (statement id, href, ...)
	Idx    int    // generic scraper: data-bills-idx attribute stamped on the control
}

// Status describes what the current page looks like.
type Status int

const (
	StatusUnknown   Status = iota // still loading / can't tell
	StatusLoggedIn                // inside the portal with account content rendered
	StatusLoginPage               // parked on a sign-in / MFA page
)

func (s Status) String() string {
	switch s {
	case StatusLoggedIn:
		return "signed in"
	case StatusLoginPage:
		return "login page"
	}
	return "unknown"
}

// ErrUnavailable means the portal is showing an outage/maintenance page.
var ErrUnavailable = errors.New("portal is showing its maintenance page; try again later")

// Site is a carrier portal.
type Site interface {
	Name() string
	HistoryURL() string
	// Status inspects the *current* page: signed in with account content
	// rendered, parked on a login/MFA page, or can't tell yet.
	Status(b *browser.Browser) (Status, error)
	// Statements lists bills (history page already navigated to).
	Statements(b *browser.Browser, since time.Time) ([]Statement, error)
	// Download returns the statement PDF bytes.
	Download(b *browser.Browser, st Statement) ([]byte, error)
	// TrimCallRecords says whether PDFs should be cut at the call-record pages.
	TrimCallRecords() bool
}

// All returns every supported site.
func All() []Site { return []Site{Xfinity(), Verizon()} }

// Select resolves a CLI argument to sites.
func Select(name string) ([]Site, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "all":
		return All(), nil
	case "xfinity":
		return []Site{Xfinity()}, nil
	case "verizon":
		return []Site{Verizon()}, nil
	}
	return nil, fmt.Errorf("unknown site %q (want xfinity, verizon, or all)", name)
}

var monthPrefixRe = regexp.MustCompile(`^([A-Za-z]{3})[A-Za-z]*(\s+.*)$`)

// ParseDate handles the date formats carrier portals typically render.
func ParseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	s = strings.NewReplacer(".", "", ",", "").Replace(s)
	if m := monthPrefixRe.FindStringSubmatch(s); m != nil {
		s = m[1] + m[2]
	}
	for _, layout := range []string{"Jan 2 2006", "1/2/2006", "1/2/06", "2006-01-02", "Jan 2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// pageHasText reports whether the visible page text matches re.
func pageHasText(b *browser.Browser, re string) bool {
	var ok bool
	_ = b.Eval(fmt.Sprintf(`/%s/i.test(document.body ? document.body.innerText : '')`, re), &ok)
	return ok
}

// latestResponse returns the most recent logged 200 response after `since`
// whose URL (sans query) satisfies match, or nil.
func latestResponse(b *browser.Browser, since time.Time, match func(url string) bool) *browser.NetEntry {
	log := b.NetLog()
	for i := len(log) - 1; i >= 0; i-- {
		if log[i].At.Before(since) {
			continue
		}
		bare := strings.SplitN(log[i].URL, "?", 2)[0]
		if match(bare) && log[i].Status == 200 {
			e := log[i]
			return &e
		}
	}
	return nil
}

// apiBody waits for a completed 200 response logged after `since` whose URL
// satisfies match and returns its body. If a body can't be read (the tab
// navigated again, or the SPA re-issued the call), it keeps waiting for a
// newer response until timeout.
func apiBody(b *browser.Browser, since time.Time, timeout time.Duration, match func(url string) bool) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	tried := map[string]bool{}
	for time.Now().Before(deadline) {
		e := latestResponse(b, since, match)
		if e != nil && e.Done && !tried[e.RequestID] {
			tried[e.RequestID] = true
			body, err := b.ResponseBody(e.RequestID)
			if err == nil {
				return body, nil
			}
			lastErr = err
		}
		b.Sleep(700 * time.Millisecond)
		if b.Ctx.Err() != nil {
			return nil, b.Ctx.Err()
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("read API response: %w", lastErr)
	}
	return nil, errors.New("API response never arrived")
}
