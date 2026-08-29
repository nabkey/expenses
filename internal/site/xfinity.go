package site

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"expenses/internal/browser"
	"expenses/internal/pdfx"
)

// Xfinity: customer.xfinity.com is a single-page app. The statement-history
// page calls api.sc.xfinity.com (bearer-token auth, so we can't call it
// ourselves) and renders <prism-lineitem> rows. We read the SPA's own API
// responses off the network log for the list, then open each statement at
// statement/current#/<billing-date> and click "Print/Save Statement (PDF)",
// which fetches the PDF as an XHR we can read the body of.
type xfinity struct{}

// Xfinity returns the Comcast/Xfinity residential portal.
func Xfinity() Site { return &xfinity{} }

const (
	xfHistoryURL   = "https://customer.xfinity.com/billing/services/statement/history"
	xfStatementURL = "https://customer.xfinity.com/billing/services/statement/current#/"
)

func (x *xfinity) Name() string          { return "xfinity" }
func (x *xfinity) HistoryURL() string    { return xfHistoryURL }
func (x *xfinity) TrimCallRecords() bool { return false }

func (x *xfinity) Status(b *browser.Browser) (Status, error) {
	loc, err := b.Location()
	if err != nil {
		return StatusUnknown, err
	}
	u, err := url.Parse(loc)
	if err != nil {
		return StatusUnknown, err
	}
	h := strings.ToLower(u.Host)
	if strings.HasPrefix(h, "login.") || strings.HasPrefix(h, "oauth.") || strings.HasPrefix(h, "idm.") || b.HasPasswordField() {
		return StatusLoginPage, nil
	}
	if !strings.HasSuffix(h, "xfinity.com") {
		return StatusUnknown, nil
	}
	if pageHasText(b, `We'll be back in a bit`) {
		return StatusUnknown, ErrUnavailable
	}
	var rows int
	_ = b.Eval(`document.querySelectorAll('prism-lineitem[data-testid="statement-history-item"]').length`, &rows)
	if rows > 0 {
		return StatusLoggedIn, nil
	}
	if pageHasText(b, `\b(sign out|log out)\b`) {
		return StatusLoggedIn, nil
	}
	return StatusUnknown, nil
}

type xfBillList struct {
	Statements []struct {
		ID            string `json:"id"`
		StatementDate string `json:"statementDate"`
	} `json:"statements"`
}

type xfBillMeta struct {
	Amount    float64 `json:"amount"`
	IssueDate string  `json:"issue_date"`
	From      string  `json:"billing_period_from"`
	To        string  `json:"billing_period_to"`
}

func (x *xfinity) Statements(b *browser.Browser, since time.Time) ([]Statement, error) {
	// The SPA requests .../account/me/bill (statement ids + dates) and
	// .../account/me/bills (amounts, periods) while rendering the history.
	// Response bodies only stay readable until the tab navigates again, so
	// load the page here and only look at responses from this load.
	isList := func(u string) bool { return strings.HasSuffix(u, "/selfhelp/account/me/bill") }
	isMeta := func(u string) bool { return strings.HasSuffix(u, "/selfhelp/account/me/bills") }
	mark := time.Now()
	if err := b.Navigate(xfHistoryURL); err != nil {
		return nil, err
	}
	body, err := apiBody(b, mark, 45*time.Second, isList)
	if err != nil {
		return nil, fmt.Errorf("statement list: %w", err)
	}
	var bl xfBillList
	if err := json.Unmarshal(body, &bl); err != nil {
		return nil, fmt.Errorf("parse statement list: %w", err)
	}

	meta := map[string]xfBillMeta{}
	// The amounts call usually lands right after the list; give it a moment.
	if body, err := apiBody(b, mark, 10*time.Second, isMeta); err == nil {
		var items []xfBillMeta
		if json.Unmarshal(body, &items) == nil {
			for _, it := range items {
				meta[it.IssueDate] = it
			}
		}
	}

	var out []Statement
	for _, s := range bl.Statements {
		d, err := time.Parse(time.RFC3339, s.StatementDate)
		if err != nil {
			continue
		}
		d = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
		if d.Before(since) {
			continue
		}
		st := Statement{Date: d, Ref: s.ID}
		if m, ok := meta[d.Format("2006-01-02")]; ok {
			st.Amount = fmt.Sprintf("$%.2f", m.Amount)
			st.Label = m.From + " to " + m.To
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.After(out[j].Date) })
	return out, nil
}

func (x *xfinity) Download(b *browser.Browser, st Statement) ([]byte, error) {
	// A hash-only change wouldn't reload the SPA, so bounce through blank.
	if err := b.Navigate("about:blank"); err != nil {
		return nil, err
	}
	target := xfStatementURL + st.Date.Format("2006-01-02")
	if err := b.Navigate(target); err != nil {
		return nil, err
	}
	// Wait for the statement to render its PDF control and stamp it.
	err := b.WaitUntil(45*time.Second, time.Second, func() (bool, error) {
		var ok bool
		err := b.Eval(`(() => {
			const el = Array.from(document.querySelectorAll('button, a, prism-button'))
				.find(e => /Print\/Save Statement|Statement PDF/i.test(e.textContent || ''));
			if (!el) return false;
			el.setAttribute('data-bills-pdf', '1');
			return true;
		})()`, &ok)
		return ok, err
	})
	if err != nil {
		return nil, fmt.Errorf("statement page for %s never showed a PDF button", st.Date.Format("2006-01-02"))
	}
	// Sanity check: the page should be showing this billing date.
	want := st.Date.Format("Jan 2, 2006")
	if !pageHasText(b, strings.ReplaceAll(strings.ReplaceAll(want, ".", `\.`), ",", ",?")) {
		return nil, fmt.Errorf("statement page did not show billing date %s", want)
	}

	// The button fetches the PDF over XHR, wraps it in a Blob, and opens a
	// blob: URL in a new window (then revokes it). Hold on to the Blob by
	// hooking createObjectURL, and hand window.open a dummy so the SPA
	// doesn't fall back to navigating this tab.
	if err := b.Eval(`
		window.__billsBlobs = [];
		window.__billsOpened = [];
		if (!window.__billsHooked) {
			window.__billsHooked = true;
			const orig = URL.createObjectURL.bind(URL);
			URL.createObjectURL = (obj) => { if (obj instanceof Blob) window.__billsBlobs.push(obj); return orig(obj); };
			window.open = (u) => { window.__billsOpened.push(String(u)); return {focus(){}, close(){}, closed: false, location: {}, document: {}}; };
		}
		true`, nil); err != nil {
		return nil, err
	}
	mark := time.Now()
	if err := b.Eval(`document.querySelector('[data-bills-pdf]').click(); true`, nil); err != nil {
		return nil, err
	}
	var n int
	_ = b.WaitUntil(45*time.Second, 500*time.Millisecond, func() (bool, error) {
		_ = b.Eval(`window.__billsBlobs.length`, &n)
		return n > 0, nil
	})
	if n > 0 {
		for i := 0; i < n; i++ {
			if body, err := b.BlobBytes(fmt.Sprintf(`window.__billsBlobs[%d]`, i)); err == nil && pdfx.IsPDF(body) {
				return body, nil
			}
		}
	}
	// Fallback: read the XHR body straight off the network.
	data, netErr := pdfFromNetwork(b, mark, 10*time.Second)
	if netErr == nil {
		return data, nil
	}
	var opened []string
	_ = b.Eval(`window.__billsOpened`, &opened)
	return nil, fmt.Errorf("clicked the PDF button but got no PDF (blobs=%d; %v; window.open calls: %v)", n, netErr, opened)
}
