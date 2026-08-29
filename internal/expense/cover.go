package expense

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Cover renders the one-page cover sheet as a PDF. It is deliberately a
// plain, receipt-like page: one vendor, one date, one total, so an OCR
// pass (ExpenseIt reads the first two pages and the last) has nothing to
// guess at. The statement's own numbers are listed separately, smaller.
func Cover(b *Bill, p Profile, generated time.Time) []byte {
	const left, col, right = 72.0, 210.0, 540.0
	pg := &page{}
	y := 80.0

	pg.text(bold, 20, left, y, "EXPENSE COVER SHEET")
	y += 20
	pg.gray(0.35)
	pg.text(regular, 9.5, left, y, fmt.Sprintf("Generated %s from the attached %s statement, which follows unaltered.",
		generated.Format("Jan 2, 2006"), p.Vendor))
	pg.gray(0)
	y += 12
	pg.rule(left, y, right)
	y += 34

	row := func(label, value string, size float64, font string) {
		pg.text(bold, 10.5, left, y, label)
		pg.text(font, size, col, y, value)
		y += 26
	}
	row("Vendor", p.Vendor, 12.5, regular)
	row("Transaction date", b.AutoPay.Format("Jan 2, 2006")+"   (automatic payment date)", 12.5, regular)
	row("Total amount", fmt.Sprintf("$%.2f USD", p.Amount), 16, bold)
	row("Expense type", p.ExpenseType, 12.5, regular)
	row("Payment method", paymentMethod(b.Payment), 12.5, regular)
	if b.Location != "" {
		row("Location", b.Location+", US", 12.5, regular)
	}
	row("Description", fmt.Sprintf("%s, %s - %s", p.Service, b.PeriodFrom.Format("Jan 2"), b.PeriodTo.Format("Jan 2, 2006")), 12.5, regular)

	y += 10
	pg.rule(left, y, right)
	y += 26
	pg.text(bold, 10.5, left, y, "Statement details")
	y += 20
	detail := func(label, value string) {
		pg.gray(0.3)
		pg.text(regular, 9.5, left, y, label)
		pg.gray(0)
		pg.text(regular, 9.5, col, y, value)
		y += 17
	}
	detail("Statement date", b.BillDate.Format("Jan 2, 2006"))
	detail("Service period", b.PeriodFrom.Format("Jan 2, 2006")+" - "+b.PeriodTo.Format("Jan 2, 2006"))
	detail("Statement total", fmt.Sprintf("%s  (amount claimed: $%.2f)", b.Total, p.Amount))
	if b.Account != "" {
		detail("Account", "ending "+b.Account)
	}
	detail("Source", filepath.Base(filepath.Dir(b.Source))+"/"+filepath.Base(b.Source))

	pg.gray(0.35)
	pg.text(regular, 8, left, 730, "This cover sheet summarizes the claim for expense processing. The vendor statement on the")
	pg.text(regular, 8, left, 741, "following pages is the original receipt and has not been modified.")
	pg.gray(0)

	return pg.pdf("Expense cover sheet - " + p.Vendor)
}

// ---------------------------------------------------------------- tiny PDF writer

const (
	regular = "/F1" // Helvetica
	bold    = "/F2" // Helvetica-Bold
)

// page accumulates a content stream for a single US-Letter page. y runs
// downward from the top edge, in points.
type page struct{ ops strings.Builder }

const pageH = 792.0

func (p *page) text(font string, size, x, y float64, s string) {
	fmt.Fprintf(&p.ops, "BT %s %.1f Tf %.1f %.1f Td (%s) Tj ET\n", font, size, x, pageH-y, escape(s))
}

func (p *page) rule(x1, y, x2 float64) {
	fmt.Fprintf(&p.ops, "0.6 w %.1f %.1f m %.1f %.1f l S\n", x1, pageH-y, x2, pageH-y)
}

func (p *page) gray(g float64) { fmt.Fprintf(&p.ops, "%.2f g\n", g) }

// escape makes s safe inside a PDF literal string (ASCII only; the
// standard fonts aren't embedded so anything else isn't guaranteed).
func escape(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r == '\\' || r == '(' || r == ')':
			sb.WriteByte('\\')
			sb.WriteRune(r)
		case r < 0x20 || r > 0x7e:
			sb.WriteByte('?')
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// pdf serializes the page as a minimal PDF 1.4 file with the standard
// Helvetica fonts (no embedding needed).
func (p *page) pdf(title string) []byte {
	var b bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	b.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	obj("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R /F2 5 0 R >> >> /Contents 6 0 R >>")
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>")
	content := p.ops.String()
	obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	obj(fmt.Sprintf("<< /Title (%s) /Producer (bills) >>", escape(title)))
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R /Info %d 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, len(offsets), xref)
	return b.Bytes()
}

// paymentMethod phrases how the bill was paid from the claimant's side.
func paymentMethod(p string) string {
	if p == "" {
		return "Personal account (automatic payment)"
	}
	return "Personal " + strings.ToLower(p[:1]) + p[1:]
}
