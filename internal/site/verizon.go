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

// Verizon (wireless): the "Bill & payment history" page is a React app that
// loads every bill cycle from one gateway call (pagination is client-side).
// Its "Review bill PDF" button simply window.open()s a cookie-authenticated
// gateway URL keyed by the cycle end date, so we fetch that directly.
type verizon struct{}

// Verizon returns the My Verizon (wireless) portal. Its PDFs include per-call
// "Talk activity" pages, which get trimmed off.
func Verizon() Site { return &verizon{} }

const (
	vzHistoryURL = "https://www.verizon.com/digital/nsa/secure/ui/bill/history/"
	vzHistoryAPI = "/digital/nsa/secure/gw/bill/billpaymenthistory/bill_history"
	vzPDFURL     = "https://www.verizon.com/digital/nsa/secure/gw/bill/billpaymenthistory/bill_pdfdoc?startMonthDate=%s&channelId=VZW-DOTCOM"
)

func (v *verizon) Name() string          { return "verizon" }
func (v *verizon) HistoryURL() string    { return vzHistoryURL }
func (v *verizon) TrimCallRecords() bool { return true }

func (v *verizon) Status(b *browser.Browser) (Status, error) {
	loc, err := b.Location()
	if err != nil {
		return StatusUnknown, err
	}
	u, err := url.Parse(loc)
	if err != nil {
		return StatusUnknown, err
	}
	h := strings.ToLower(u.Host)
	p := strings.ToLower(u.Path)
	if strings.Contains(h, "login") || strings.Contains(p, "/signin") || strings.Contains(p, "/login") ||
		strings.Contains(p, "vzauth") || b.HasPasswordField() {
		return StatusLoginPage, nil
	}
	if !strings.HasSuffix(h, "verizon.com") {
		return StatusUnknown, nil
	}
	var rows int
	_ = b.Eval(`document.querySelectorAll('button[data-track="Review bill PDF"]').length`, &rows)
	if rows > 0 {
		return StatusLoggedIn, nil
	}
	if strings.Contains(p, "/secure/") && pageHasText(b, `\bsign out\b`) {
		return StatusLoggedIn, nil
	}
	return StatusUnknown, nil
}

type vzHistory struct {
	Body struct {
		BillingHistory struct {
			BillCycle []struct {
				BillEndDate    string `json:"billEndDate"`    // MM/DD/YYYY
				BillAmount     string `json:"billAmount"`     // "$123.45"
				BillCycleRange string `json:"billCycleRange"` // "07/22/2026 - 08/21/2026"
			} `json:"billCycle"`
		} `json:"billingHistory"`
	} `json:"body"`
}

func (v *verizon) Statements(b *browser.Browser, since time.Time) ([]Statement, error) {
	mark := time.Now()
	if err := b.Navigate(vzHistoryURL); err != nil {
		return nil, err
	}
	body, err := apiBody(b, mark, 45*time.Second, func(u string) bool { return strings.HasSuffix(u, vzHistoryAPI) })
	if err != nil {
		return nil, fmt.Errorf("bill history: %w", err)
	}
	var h vzHistory
	if err := json.Unmarshal(body, &h); err != nil {
		return nil, fmt.Errorf("parse bill history: %w", err)
	}
	var out []Statement
	for _, c := range h.Body.BillingHistory.BillCycle {
		d, err := time.Parse("01/02/2006", c.BillEndDate)
		if err != nil {
			continue
		}
		if d.Before(since) {
			continue
		}
		out = append(out, Statement{Date: d, Amount: c.BillAmount, Label: c.BillCycleRange, Ref: c.BillEndDate})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.After(out[j].Date) })
	return out, nil
}

func (v *verizon) Download(b *browser.Browser, st Statement) ([]byte, error) {
	// fetch() must run from a verizon.com page for the session cookies to apply.
	if loc, _ := b.Location(); !strings.Contains(loc, "verizon.com/digital/nsa/secure/") {
		if err := b.Navigate(vzHistoryURL); err != nil {
			return nil, err
		}
	}
	data, err := b.FetchBytes(fmt.Sprintf(vzPDFURL, url.QueryEscape(st.Ref)))
	if err != nil {
		return nil, err
	}
	if !pdfx.IsPDF(data) {
		return nil, fmt.Errorf("bill_pdfdoc for %s did not return a PDF (%d bytes)", st.Ref, len(data))
	}
	return data, nil
}
