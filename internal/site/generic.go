package site

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/chromedp/chromedp"

	"expenses/internal/browser"
	"expenses/internal/pdfx"
)

// generic scrapes a bill-history page for view/download controls that sit
// next to a date (and usually an amount). It is the starting point for a
// portal whose DOM hasn't been mapped yet.
type generic struct {
	name       string
	historyURL string
	loggedIn   func(u *url.URL) bool
	trim       bool
	// prepare runs after navigating to historyURL, before scraping (e.g. to
	// expand "older bills"). Optional.
	prepare func(b *browser.Browser) error
}

func (g *generic) Name() string          { return g.name }
func (g *generic) HistoryURL() string    { return g.historyURL }
func (g *generic) TrimCallRecords() bool { return g.trim }

func (g *generic) Status(b *browser.Browser) (Status, error) {
	loc, err := b.Location()
	if err != nil {
		return StatusUnknown, err
	}
	u, err := url.Parse(loc)
	if err != nil {
		return StatusUnknown, err
	}
	if !g.loggedIn(u) || b.HasPasswordField() {
		return StatusLoginPage, nil
	}
	// Right host, no password box. Only call it signed in once we can see
	// account content: dated bill links, or a sign-out control.
	cands, _ := Scrape(b)
	for _, c := range cands {
		if _, ok := ParseDate(c.Date); ok {
			return StatusLoggedIn, nil
		}
	}
	if pageHasText(b, `\b(sign out|log out|logout)\b`) {
		return StatusLoggedIn, nil
	}
	return StatusUnknown, nil
}

// Candidate is one scraped control (exposed for `bills inspect`).
type Candidate struct {
	Idx    int    `json:"idx"`
	Text   string `json:"text"`
	Href   string `json:"href"`
	Aria   string `json:"aria"`
	Date   string `json:"date"`
	Amount string `json:"amount"`
	Tag    string `json:"tag"`
}

// scrapeJS collects every link/button that looks like a "view/download bill"
// control, walking up the DOM to find the nearest date and dollar amount.
// Each control gets a data-bills-idx attribute so we can click it later.
const scrapeJS = `(() => {
	const months = '(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Sept|Oct|Nov|Dec)[a-z]*\\.?';
	const dateRe = new RegExp(
		'(?:' + months + '\\s+\\d{1,2},?\\s+\\d{4})' +
		'|(?:\\d{1,2}/\\d{1,2}/\\d{2,4})' +
		'|(?:\\d{4}-\\d{2}-\\d{2})' +
		'|(?:' + months + '\\s+\\d{4})', 'i');
	const amtRe = /\$\s?-?\d[\d,]*\.\d{2}/;
	const ctlRe = /\b(view|download|pdf|statement|print)\b/i;
	const out = [];
	const els = Array.from(document.querySelectorAll('a, button, [role=button], [role=link]'));
	els.forEach((el, i) => {
		const aria = el.getAttribute('aria-label') || '';
		const text = ((el.innerText || el.textContent || '') + ' ' + aria).replace(/\s+/g, ' ').trim();
		const href = el.href || el.getAttribute('href') || '';
		if (!ctlRe.test(text) && !/pdf/i.test(href)) return;
		let node = el, date = '', amount = '', depth = 0;
		while (node && depth < 8) {
			const t = (node.innerText || '').replace(/\s+/g, ' ');
			const dm = t.match(dateRe); if (dm && !date) date = dm[0];
			const am = t.match(amtRe); if (am && !amount) amount = am[0];
			if (date && amount) break;
			node = node.parentElement; depth++;
		}
		el.setAttribute('data-bills-idx', String(i));
		out.push({idx: i, text: text.slice(0, 120), href: href, aria: aria, date: date, amount: amount, tag: el.tagName});
	});
	return out;
})()`

func isHTTP(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// Scrape runs the generic scraper once and returns raw candidates.
func Scrape(b *browser.Browser) ([]Candidate, error) {
	var cands []Candidate
	err := b.Eval(scrapeJS, &cands)
	return cands, err
}

func (g *generic) Statements(b *browser.Browser, since time.Time) ([]Statement, error) {
	if loc, _ := b.Location(); !strings.HasPrefix(loc, g.historyURL) {
		if err := b.Navigate(g.historyURL); err != nil {
			return nil, err
		}
	}
	if g.prepare != nil {
		if err := g.prepare(b); err != nil {
			return nil, err
		}
	}
	var cands []Candidate
	err := b.WaitUntil(25*time.Second, 1500*time.Millisecond, func() (bool, error) {
		cands = nil
		if err := b.Eval(scrapeJS, &cands); err != nil {
			return false, err
		}
		for _, c := range cands {
			if _, ok := ParseDate(c.Date); ok {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return nil, fmt.Errorf("bill history page never showed any bill links (%v) — try `bills inspect %s`", err, g.name)
	}

	byDate := map[string]Statement{}
	for _, c := range cands {
		d, ok := ParseDate(c.Date)
		if !ok || d.Before(since) {
			continue
		}
		st := Statement{Date: d, Amount: c.Amount, Label: c.Text, Ref: c.Href, Idx: c.Idx}
		key := d.Format("2006-01-02")
		if prev, dup := byDate[key]; dup {
			// Keep whichever control has a real URL.
			if isHTTP(prev.Ref) || !isHTTP(st.Ref) {
				continue
			}
		}
		byDate[key] = st
	}
	out := make([]Statement, 0, len(byDate))
	for _, st := range byDate {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.After(out[j].Date) })
	return out, nil
}

// clickAction clicks the scraped control, falling back to a synthetic click
// when chromedp can't scroll it into view.
func clickAction(sel string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := chromedp.Run(cctx, chromedp.Click(sel, chromedp.ByQuery, chromedp.NodeVisible)); err == nil {
			return nil
		}
		return chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`(() => { const e = document.querySelector(%q); if (!e) throw new Error('control vanished'); e.click(); return true; })()`, sel), nil))
	})
}

func (g *generic) Download(b *browser.Browser, st Statement) ([]byte, error) {
	var errs []string

	// 1. The control exposed a URL: fetch it with the page's cookies.
	if isHTTP(st.Ref) {
		data, err := b.FetchBytes(st.Ref)
		switch {
		case err != nil:
			errs = append(errs, "fetch href: "+err.Error())
		case pdfx.IsPDF(data):
			return data, nil
		default:
			errs = append(errs, "fetch href: response was not a PDF")
		}
	}

	// 2. Click it and capture the browser download.
	sel := fmt.Sprintf(`[data-bills-idx="%d"]`, st.Idx)
	present := false
	_ = b.Eval(fmt.Sprintf(`!!document.querySelector(%q)`, sel), &present)
	if !present {
		// The page changed since we scraped it (e.g. a previous click
		// navigated away). Reload the history and find this bill again.
		if err := b.Navigate(g.historyURL); err != nil {
			return nil, err
		}
		again, err := g.Statements(b, st.Date.AddDate(0, 0, -1))
		if err != nil {
			return nil, err
		}
		found := false
		for _, s2 := range again {
			if s2.Date.Equal(st.Date) {
				st, found = s2, true
				break
			}
		}
		if !found {
			return nil, errors.New("bill link disappeared after reloading the history page")
		}
		sel = fmt.Sprintf(`[data-bills-idx="%d"]`, st.Idx)
	}
	mark := time.Now()
	data, err := b.CaptureDownload(clickAction(sel), 45*time.Second)
	switch {
	case err != nil:
		errs = append(errs, "click: "+err.Error())
	case pdfx.IsPDF(data):
		return data, nil
	default:
		errs = append(errs, "click: downloaded file was not a PDF")
	}

	// 3. Maybe the click rendered the PDF inline or fetched it via XHR.
	if data, err := pdfFromNetwork(b, mark, 0); err == nil {
		return data, nil
	} else {
		errs = append(errs, err.Error())
	}

	// 4. Or navigated the tab straight to a PDF URL.
	if loc, err := b.Location(); err == nil && strings.Contains(strings.ToLower(loc), "pdf") {
		if body, err := b.FetchBytes(loc); err == nil && pdfx.IsPDF(body) {
			return body, nil
		}
	}
	return nil, fmt.Errorf("no PDF obtained: %s", strings.Join(errs, "; "))
}

// pdfFromNetwork looks for a PDF response logged after mark, waits for its
// body to finish loading, and returns it. With wait > 0 it polls that long.
func pdfFromNetwork(b *browser.Browser, mark time.Time, wait time.Duration) ([]byte, error) {
	var found *browser.NetEntry
	check := func() (bool, error) {
		for _, e := range b.NetLog() {
			if e.At.Before(mark) {
				continue
			}
			bare := strings.ToLower(strings.SplitN(e.URL, "?", 2)[0])
			if strings.Contains(strings.ToLower(e.Mime), "pdf") || strings.HasSuffix(bare, ".pdf") {
				found = &e
				if e.Failed {
					return false, fmt.Errorf("PDF request %s failed to load", e.URL)
				}
				return e.Done, nil // keep polling until the body has arrived
			}
		}
		return false, nil
	}
	if wait > 0 {
		_ = b.WaitUntil(wait, 500*time.Millisecond, check)
	} else {
		_, _ = check()
	}
	if found == nil {
		return nil, errors.New("no PDF response seen on the network")
	}
	body, err := b.ResponseBody(found.RequestID)
	if err == nil && pdfx.IsPDF(body) {
		return body, nil
	}
	if err == nil {
		err = errors.New("body is not a PDF")
	}
	return nil, fmt.Errorf("saw PDF response %s (done=%v) but could not read its body: %v", found.URL, found.Done, err)
}
