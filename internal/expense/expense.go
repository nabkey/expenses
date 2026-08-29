// Package expense turns cached statements into PDFs ready for an expense
// system (SAP Concur / ExpenseIt): a generated cover sheet stating the
// vendor, transaction date, amount claimed and expense type, followed by
// the untouched statement.
//
// The transaction date is the statement's automatic-payment date, so a
// statement is only prepared once that date has passed.
package expense

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"expenses/internal/pdfx"
)

// Profile says how a site's statements are claimed. Vendor, service and the
// parser are built in; the amount and expense type are personal and come
// from bills.json (see Config) or the -amount/-type flags.
type Profile struct {
	Site        string  // bills/<site>/
	Vendor      string  // vendor name for the claim
	ExpenseType string  // exact expense-type name in the expense system
	Amount      float64 // amount claimed per statement, USD
	Service     string  // description prefix, e.g. "Home internet service"
	parse       func(text string) (*Bill, error)
}

var profiles = map[string]Profile{
	"xfinity": {
		Site:    "xfinity",
		Vendor:  "Comcast Xfinity",
		Service: "Home internet service",
		parse:   parseXfinity,
	},
	"verizon": {
		Site:    "verizon",
		Vendor:  "Verizon Wireless",
		Service: "Mobile phone service",
		parse:   parseVerizon,
	},
}

// ProfileFor returns the built-in claim profile for a site (no amount or
// expense type yet — apply a Config or flags, then Validate).
func ProfileFor(site string) (Profile, bool) {
	p, ok := profiles[strings.ToLower(site)]
	return p, ok
}

// Config is the user's bills.json: per-site claim settings.
type Config struct {
	Expense map[string]ProfileConfig `json:"expense"`
}

// ProfileConfig overrides parts of a Profile.
type ProfileConfig struct {
	Vendor      string  `json:"vendor,omitempty"`
	ExpenseType string  `json:"expense_type"`
	Amount      float64 `json:"amount"`
	Service     string  `json:"service,omitempty"`
}

// LoadConfig reads bills.json. A missing file is not an error.
func LoadConfig(path string) (Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Apply overlays non-empty settings from c.
func (p Profile) Apply(c ProfileConfig) Profile {
	if c.Vendor != "" {
		p.Vendor = c.Vendor
	}
	if c.ExpenseType != "" {
		p.ExpenseType = c.ExpenseType
	}
	if c.Amount != 0 {
		p.Amount = c.Amount
	}
	if c.Service != "" {
		p.Service = c.Service
	}
	return p
}

// Validate checks the profile has what a claim needs.
func (p Profile) Validate() error {
	var missing []string
	if p.Amount <= 0 {
		missing = append(missing, "amount")
	}
	if p.ExpenseType == "" {
		missing = append(missing, "expense_type")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s: %s not set — copy bills.example.json to bills.json, or pass -amount/-type", p.Site, strings.Join(missing, " and "))
	}
	return nil
}

// Bill is what we read off page 1 of a statement.
type Bill struct {
	Source     string    // statement PDF
	BillDate   time.Time // statement / billing date
	PeriodFrom time.Time // service period
	PeriodTo   time.Time
	AutoPay    time.Time // automatic-payment date = the expense's transaction date
	Total      string    // statement total as printed, e.g. "$123.45"
	Account    string    // last four digits
	Payment    string    // how it is paid, e.g. "Credit card (automatic payment)"
	Location   string    // service address city/state, if found
}

// Parse reads the statement at path.
func (p Profile) Parse(path string) (*Bill, error) {
	if p.parse == nil {
		return nil, fmt.Errorf("no parser for %s", p.Site)
	}
	texts, err := pdfx.PageTexts(path)
	if err != nil {
		return nil, err
	}
	if len(texts) == 0 {
		return nil, errors.New("no text on page 1")
	}
	b, err := p.parse(texts[0])
	if err != nil {
		return nil, err
	}
	b.Source = path
	return b, nil
}

// OutputName is the file name of the prepared PDF: vendor, transaction
// date and amount, so it reads well in an attachment list.
func (p Profile) OutputName(b *Bill) string {
	name := strings.ToUpper(p.Site[:1]) + p.Site[1:]
	return fmt.Sprintf("%s_%s_%.2f.pdf", name, b.AutoPay.Format("2006-01-02"), p.Amount)
}

// Status of one statement after Prepare.
type Status int

const (
	Prepared Status = iota // cover sheet generated and PDF written
	Exists                 // output already present (not regenerated)
	Waiting                // auto-pay date hasn't passed yet
	Failed                 // couldn't parse or write
)

// Result is Prepare's verdict for one statement.
type Result struct {
	Source string
	Bill   *Bill
	Out    string
	Pages  int
	Status Status
	Err    error
}

// Prepare processes every statement in billsDir/<site>, writing prepared
// PDFs to outDir for those whose auto-pay date is before asOf. Existing
// outputs are left alone unless force is set. Results come back in
// statement-date order.
func Prepare(billsDir, outDir string, p Profile, asOf time.Time, force bool) ([]Result, error) {
	files, err := filepath.Glob(filepath.Join(billsDir, p.Site, "*.pdf"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("no statements in %s", filepath.Join(billsDir, p.Site))
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	today := day(asOf)
	var results []Result
	for _, f := range files {
		r := Result{Source: f}
		b, err := p.Parse(f)
		if err != nil {
			r.Status, r.Err = Failed, err
			results = append(results, r)
			continue
		}
		r.Bill = b
		r.Out = filepath.Join(outDir, p.OutputName(b))
		if !today.After(day(b.AutoPay)) {
			r.Status = Waiting
			results = append(results, r)
			continue
		}
		if !force {
			if _, err := os.Stat(r.Out); err == nil {
				r.Status = Exists
				results = append(results, r)
				continue
			}
		}
		cover := Cover(b, p, asOf)
		if err := pdfx.Prepend(cover, f, r.Out); err != nil {
			r.Status, r.Err = Failed, err
			results = append(results, r)
			continue
		}
		if n, err := pdfx.PageCount(r.Out); err == nil {
			r.Pages = n
		}
		r.Status = Prepared
		results = append(results, r)
	}
	return results, nil
}

func day(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// ---------------------------------------------------------------- parsing

const (
	monRe   = `(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]*\.?`
	dateRe  = monRe + ` \d{1,2}, \d{4}`
	moneyRe = `\$\s?([\d,]+\.\d{2})`
)

var (
	// Xfinity page-1 header: "<billing date>   <from> to <to>" on one line.
	xfHeaderRe  = regexp.MustCompile(`(` + dateRe + `)\s+(` + dateRe + `) to (` + dateRe + `)`)
	xfAutoPayRe = regexp.MustCompile(`(?i)(?:automatic payment on|will be applied|Automatic payment\s{2,})\s*(` + dateRe + `)`)
	xfTotalRe   = regexp.MustCompile(`(?im)^\s*(?:Amount due|Please pay)\s+` + moneyRe)
	xfAccountRe = regexp.MustCompile(`\b(\d{4} \d{2} \d{3} \d{7})\b`)
	xfPaymentRe = regexp.MustCompile(`(?i)\b(credit card|debit card|bank|checking) payment will be applied`)

	// Verizon page 1.
	vzBillDateRe = regexp.MustCompile(`(?i)Bill date\s+(` + dateRe + `)`)
	vzPeriodRe   = regexp.MustCompile(`(?i)Billing period:\s*(` + monRe + ` \d{1,2}) - (` + dateRe + `)`)
	vzAutoPayRe  = regexp.MustCompile(`(?i)Auto ?Pay scheduled for (` + dateRe + `)`)
	vzDeductRe   = regexp.MustCompile(`(?i)Deducted from (?:bank|checking|savings)? ?account on (\d{1,2}/\d{1,2}/\d{2,4})`)
	vzTotalRe    = regexp.MustCompile(`(?i)(?:Total due on ` + monRe + ` \d{1,2}|This month's charges)\s+` + moneyRe)
	vzAccountRe  = regexp.MustCompile(`(?i)Account:\s*([\d-]{6,})`)

	// Service address, e.g. "For 275 MAIN ST, SPRINGFIELD, IL, 62701-1234"
	// (Xfinity) or a "SPRINGFIELD, IL  62701" line under a street line.
	forAddrRe  = regexp.MustCompile(`(?m)^\s*For .*?,\s*([A-Z][A-Z .'-]*[A-Z]),\s*([A-Z]{2}),?\s+\d{5}`)
	cityLineRe = regexp.MustCompile(`^\s*([A-Z][A-Z .'-]*[A-Z]),\s*([A-Z]{2})\s+\d{5}`)
	streetRe   = regexp.MustCompile(`^\s*\d+ [A-Z0-9]`)
	poBoxRe    = regexp.MustCompile(`(?i)\bP\.?\s*O\.?\s*BOX\b`)
)

func parseXfinity(text string) (*Bill, error) {
	b := &Bill{}
	var err error
	m := xfHeaderRe.FindStringSubmatch(text)
	if m == nil {
		return nil, errors.New("billing date / service period header not found")
	}
	if b.BillDate, err = parseDate(m[1]); err != nil {
		return nil, err
	}
	if b.PeriodFrom, err = parseDate(m[2]); err != nil {
		return nil, err
	}
	if b.PeriodTo, err = parseDate(m[3]); err != nil {
		return nil, err
	}
	if m = xfAutoPayRe.FindStringSubmatch(text); m == nil {
		return nil, errors.New("no automatic-payment date found (not on auto-pay?)")
	}
	if b.AutoPay, err = parseDate(m[1]); err != nil {
		return nil, err
	}
	if m = xfTotalRe.FindStringSubmatch(text); m == nil {
		return nil, errors.New("amount due not found")
	}
	b.Total = "$" + m[1]
	if m = xfAccountRe.FindStringSubmatch(text); m != nil {
		b.Account = lastDigits(m[1], 4)
	}
	b.Payment = "Automatic payment"
	if m = xfPaymentRe.FindStringSubmatch(text); m != nil {
		b.Payment = capitalize(m[1]) + " (automatic payment)"
	}
	b.Location = findLocation(text)
	return b, nil
}

func parseVerizon(text string) (*Bill, error) {
	b := &Bill{}
	var err error
	m := vzBillDateRe.FindStringSubmatch(text)
	if m == nil {
		return nil, errors.New("bill date not found")
	}
	if b.BillDate, err = parseDate(m[1]); err != nil {
		return nil, err
	}
	if m = vzPeriodRe.FindStringSubmatch(text); m == nil {
		return nil, errors.New("billing period not found")
	}
	if b.PeriodTo, err = parseDate(m[2]); err != nil {
		return nil, err
	}
	if b.PeriodFrom, err = parseDate(fmt.Sprintf("%s, %d", m[1], b.PeriodTo.Year())); err != nil {
		return nil, err
	}
	if b.PeriodFrom.After(b.PeriodTo) { // e.g. Dec 22 - Jan 21, 2026
		b.PeriodFrom = b.PeriodFrom.AddDate(-1, 0, 0)
	}
	switch {
	case vzAutoPayRe.MatchString(text):
		m = vzAutoPayRe.FindStringSubmatch(text)
	case vzDeductRe.MatchString(text):
		m = vzDeductRe.FindStringSubmatch(text)
	default:
		return nil, errors.New("no Auto Pay date found (not on Auto Pay?)")
	}
	if b.AutoPay, err = parseDate(m[1]); err != nil {
		return nil, err
	}
	if m = vzTotalRe.FindStringSubmatch(text); m == nil {
		return nil, errors.New("total due not found")
	}
	b.Total = "$" + m[1]
	if m = vzAccountRe.FindStringSubmatch(text); m != nil {
		b.Account = lastDigits(m[1], 4)
	}
	b.Payment = "Auto Pay"
	if vzDeductRe.MatchString(text) {
		b.Payment = "Bank account (Auto Pay)"
	}
	b.Location = findLocation(text)
	return b, nil
}

// findLocation returns "City, ST" for the service address, or "".
func findLocation(text string) string {
	if m := forAddrRe.FindStringSubmatch(text); m != nil {
		return titleCase(m[1]) + ", " + m[2]
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		m := cityLineRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		// A customer address has a street line just above it; the vendor's
		// remittance blocks have a PO box instead.
		street, box := false, false
		for j := i - 1; j >= 0 && j >= i-4; j-- {
			if poBoxRe.MatchString(lines[j]) {
				box = true
			}
			if streetRe.MatchString(lines[j]) {
				street = true
			}
		}
		if street && !box {
			return titleCase(m[1]) + ", " + m[2]
		}
	}
	return ""
}

func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ".", ""))
	for _, layout := range []string{"Jan 2, 2006", "January 2, 2006", "1/2/06", "1/2/2006", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized date %q", s)
}

func lastDigits(s string, n int) string {
	var d []rune
	for _, r := range s {
		if r >= '0' && r <= '9' {
			d = append(d, r)
		}
	}
	if len(d) > n {
		d = d[len(d)-n:]
	}
	return string(d)
}

func capitalize(s string) string {
	s = strings.ToLower(s)
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func titleCase(s string) string {
	words := strings.Fields(strings.ToLower(s))
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}
