package expense

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"

	"expenses/internal/pdfx"
)

// Page-1 text shaped like pdftotext -layout output, with made-up details.
const xfinityPage1 = `
                                                             Account Number             Billing Date      Services From                        Page
                                                             1234 56 789 0004321        Aug 10, 2026      Aug 14, 2026 to Sep 13, 2026         1 of 3

Hello Pat Example,

 Your bill at a glance
 For 100 MAIN ST, SPRINGFIELD, IL, 62701-1234

 Previous balance                                             $149.99
 Credit card payment - thank you        Aug 02                -$149.99
 Balance forward                                               $0.00
 Regular monthly charges                Page 3                $149.99
 New charges                                                 $149.99

 Amount due                                               $149.99

   Thanks for paying by Automatic Payment
Your automatic payment on Sep 01, 2026, will include your
amount due, plus or minus any payment related activities.

                                                                               Automatic payment                       Sep 01, 2026
                                                                               Please pay                              $149.99
                                                                               Credit card payment will be applied Sep 01, 2026
`

const verizonPage1 = `
                                                                                  Account: 123456789-00001
PO BOX 489
                                                                                  Invoice: 1234567890
NEWARK, NJ 07101-0489
                                                                                  Billing period: Dec 22 - Jan 21, 2026

PAT EXAMPLE

100 MAIN ST
                                                                                  Late fee policy
SPRINGFIELD, IL   62701-1234

   Balance from last bill                                  $0.00
   This month's charges                                 $312.45
   Total due on Feb 13                                 $312.45

   You have Auto Pay scheduled for Feb 6, 2026.

                                                      Bill date                    January 21, 2026
                                                      Total Amount Due
                                                      Deducted from bank account on 02/06/26
                                                      DO NOT MAIL PAYMENT                                          $312.45

                                                                     P.O. BOX 15062
 Please see back for instructions on writing to us.
                                                                     ALBANY, NY 12212-5062
`

func d(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParseXfinity(t *testing.T) {
	b, err := parseXfinity(xfinityPage1)
	if err != nil {
		t.Fatal(err)
	}
	want := Bill{
		BillDate: d("2026-08-10"), PeriodFrom: d("2026-08-14"), PeriodTo: d("2026-09-13"),
		AutoPay: d("2026-09-01"), Total: "$149.99", Account: "4321",
		Payment: "Credit card (automatic payment)", Location: "Springfield, IL",
	}
	if *b != want {
		t.Errorf("got  %+v\nwant %+v", *b, want)
	}
}

func TestParseVerizon(t *testing.T) {
	b, err := parseVerizon(verizonPage1)
	if err != nil {
		t.Fatal(err)
	}
	want := Bill{
		BillDate: d("2026-01-21"), PeriodFrom: d("2025-12-22"), PeriodTo: d("2026-01-21"),
		AutoPay: d("2026-02-06"), Total: "$312.45", Account: "0001",
		Payment: "Bank account (Auto Pay)", Location: "Springfield, IL",
	}
	if *b != want {
		t.Errorf("got  %+v\nwant %+v", *b, want)
	}
}

func TestParseVerizonDeductFallback(t *testing.T) {
	text := strings.Replace(verizonPage1, "You have Auto Pay scheduled for Feb 6, 2026.", "", 1)
	b, err := parseVerizon(text)
	if err != nil {
		t.Fatal(err)
	}
	if !b.AutoPay.Equal(d("2026-02-06")) {
		t.Errorf("AutoPay = %s, want 2026-02-06", b.AutoPay.Format("2006-01-02"))
	}
}

func TestParseMissingAutoPay(t *testing.T) {
	text := strings.NewReplacer(
		"Your automatic payment on Sep 01, 2026,", "",
		"Automatic payment                       Sep 01, 2026", "",
		"Credit card payment will be applied Sep 01, 2026", "",
	).Replace(xfinityPage1)
	if _, err := parseXfinity(text); err == nil {
		t.Error("expected an error without an auto-pay date")
	}
}

func TestCoverAndPrepend(t *testing.T) {
	p, _ := ProfileFor("xfinity")
	p = p.Apply(ProfileConfig{ExpenseType: "Internet", Amount: 100})
	b, err := parseXfinity(xfinityPage1)
	if err != nil {
		t.Fatal(err)
	}
	b.Source = "bills/xfinity/2026-08-10.pdf"
	cover := Cover(b, p, d("2026-09-10"))
	if !bytes.HasPrefix(cover, []byte("%PDF")) {
		t.Fatal("cover is not a PDF")
	}
	conf := model.NewDefaultConfiguration()
	if err := api.Validate(bytes.NewReader(cover), conf); err != nil {
		t.Fatalf("cover fails validation: %v", err)
	}
	for _, s := range []string{"EXPENSE COVER SHEET", "Comcast Xfinity", "Sep 1, 2026", "$100.00 USD", "Internet", "Springfield, IL"} {
		if !bytes.Contains(cover, []byte(escape(s))) {
			t.Errorf("cover text lacks %q", s)
		}
	}

	dir := t.TempDir()
	in := filepath.Join(dir, "in.pdf")
	out := filepath.Join(dir, "out.pdf")
	if err := os.WriteFile(in, cover, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := pdfx.Prepend(cover, in, out); err != nil {
		t.Fatal(err)
	}
	if n, err := pdfx.PageCount(out); err != nil || n != 2 {
		t.Errorf("merged page count = %d, %v; want 2", n, err)
	}
}

func TestOutputName(t *testing.T) {
	p, _ := ProfileFor("verizon")
	p.Amount = 42
	got := p.OutputName(&Bill{AutoPay: d("2026-02-06")})
	if got != "Verizon_2026-02-06_42.00.pdf" {
		t.Errorf("OutputName = %q", got)
	}
}

func TestPrepareGating(t *testing.T) {
	// Prepare reads real PDFs, so use the cover sheet itself as a stand-in
	// statement whose text we know parses as an Xfinity bill? It doesn't —
	// so only check the "unparseable statement" path here.
	root := t.TempDir()
	site := filepath.Join(root, "bills", "xfinity")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	p, _ := ProfileFor("xfinity")
	junk := Cover(&Bill{Source: "x/y.pdf"}, p, d("2026-01-01"))
	if err := os.WriteFile(filepath.Join(site, "2026-01-13.pdf"), junk, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Prepare(filepath.Join(root, "bills"), filepath.Join(root, "out"), p, d("2026-03-01"), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Status != Failed {
		t.Errorf("results = %+v; want one Failed", res)
	}
}

func TestConfigAndValidate(t *testing.T) {
	p, _ := ProfileFor("xfinity")
	if err := p.Validate(); err == nil {
		t.Error("expected Validate to fail without amount/expense_type")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "bills.json")
	if err := os.WriteFile(path, []byte(`{"expense": {"xfinity": {"expense_type": "Internet", "amount": 100}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	p = p.Apply(cfg.Expense["xfinity"])
	if err := p.Validate(); err != nil {
		t.Errorf("Validate after config: %v", err)
	}
	if p.Vendor != "Comcast Xfinity" || p.Amount != 100 || p.ExpenseType != "Internet" {
		t.Errorf("profile = %+v", p)
	}
	if _, err := LoadConfig(filepath.Join(dir, "missing.json")); err != nil {
		t.Errorf("missing config should be fine: %v", err)
	}
}
