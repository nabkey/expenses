// Package pdfx trims carrier bills at the point where per-call usage
// records begin.
package pdfx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/ledongthuc/pdf"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

var (
	// Section headings Verizon (and most carriers) use for itemized usage.
	headingRe = regexp.MustCompile(`(?im)^\s*(talk activity|call details?|call detail records?|voice activity|talk, text (and|&) data activity|usage details?|detailed usage|itemized calls?|messaging activity)\b`)
	// Clock times like "9:59 AM" — every call-record row has one.
	timeRe = regexp.MustCompile(`(?i)\b\d{1,2}:\d{2}\s?(?:AM|PM)\b`)
	// Phone numbers like 555-123-4567 / 555.123.4567 / 555 123 4567.
	phoneRe = regexp.MustCompile(`\b\d{3}[-.\s]\d{3}[-.\s]\d{4}\b`)
)

// PageVerdict explains why a page was (or wasn't) classified as usage detail.
type PageVerdict struct {
	Page    int
	Records bool
	Heading bool
	Phones  int
	Times   int
}

// Analysis is the result of scanning a bill.
type Analysis struct {
	Pages    int
	Cut      int // 1-based page where call records begin; 0 = none found
	Verdicts []PageVerdict
}

func (v PageVerdict) String() string {
	mark := "keep"
	if v.Records {
		mark = "RECORDS"
	}
	h := ""
	if v.Heading {
		h = " heading"
	}
	return fmt.Sprintf("p%-3d %-7s phones=%-3d times=%-3d%s", v.Page, mark, v.Phones, v.Times, h)
}

// IsPDF reports whether data starts with the PDF magic.
func IsPDF(data []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(data, "\r\n\t "), []byte("%PDF"))
}

// Analyze finds the first page that looks like itemized call records.
func Analyze(path string) (*Analysis, error) {
	texts, err := PageTexts(path)
	if err != nil {
		return nil, err
	}
	a := &Analysis{Pages: len(texts)}
	for i, t := range texts {
		v := PageVerdict{
			Page:    i + 1,
			Phones:  len(phoneRe.FindAllString(t, -1)),
			Times:   len(timeRe.FindAllString(t, -1)),
			Heading: headingRe.MatchString(t),
		}
		// A page of call records has many timestamped rows with phone numbers.
		// A section heading plus at least a couple of timestamped rows also counts.
		v.Records = (v.Phones >= 3 && v.Times >= 3) || (v.Heading && v.Times >= 2)
		if v.Records && i > 0 && a.Cut == 0 {
			a.Cut = i + 1
		}
		a.Verdicts = append(a.Verdicts, v)
	}
	return a, nil
}

// Trim writes a copy of in to out with call-record pages removed. If
// keepPartial is true the page on which records start is kept (it may also
// contain the tail of the charges); otherwise it is dropped.
func Trim(in, out string, keepPartial bool) (*Analysis, error) {
	a, err := Analyze(in)
	if err != nil {
		return nil, err
	}
	if a.Cut == 0 {
		return a, copyFile(in, out)
	}
	last := a.Cut - 1
	if keepPartial {
		last = a.Cut
	}
	if last < 1 {
		last = 1
	}
	if last > a.Pages {
		last = a.Pages
	}
	if err := keepPages(in, out, last); err != nil {
		return nil, fmt.Errorf("trim: %w", err)
	}
	return a, nil
}

// keepPages writes pages 1..last of in to out. Carrier PDFs (Verizon's come
// from an AFP converter) routinely fail pdfcpu's validator on cosmetic
// details like link-annotation QuadPoints, so read without validating;
// fall back to poppler's pdfseparate/pdfunite if pdfcpu can't cope at all.
func keepPages(in, out string, last int) error {
	err := keepPagesPdfcpu(in, out, last)
	if err == nil {
		return nil
	}
	if perr := keepPagesPoppler(in, out, last); perr == nil {
		return nil
	} else {
		return fmt.Errorf("pdfcpu: %v; poppler: %v", err, perr)
	}
}

func keepPagesPdfcpu(in, out string, last int) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pdfcpu panicked: %v", r)
		}
	}()
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	defer f.Close()
	conf := model.NewDefaultConfiguration()
	conf.ValidationMode = model.ValidationRelaxed
	conf.Cmd = model.TRIM
	ctx, err := api.ReadContext(f, conf)
	if err != nil {
		return err
	}
	if err := ctx.EnsurePageCount(); err != nil {
		return err
	}
	if last > ctx.PageCount {
		last = ctx.PageCount
	}
	if err := api.OptimizeContext(ctx); err != nil {
		return err
	}
	pageNrs := make([]int, 0, last)
	for i := 1; i <= last; i++ {
		pageNrs = append(pageNrs, i)
	}
	dest, err := pdfcpu.ExtractPages(ctx, pageNrs, false)
	if err != nil {
		return err
	}
	w, err := os.Create(out)
	if err != nil {
		return err
	}
	if err := api.WriteContext(dest, w); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

func keepPagesPoppler(in, out string, last int) error {
	sep, err := exec.LookPath("pdfseparate")
	if err != nil {
		return errors.New("pdfseparate not installed (brew install poppler)")
	}
	unite, err := exec.LookPath("pdfunite")
	if err != nil {
		return errors.New("pdfunite not installed (brew install poppler)")
	}
	tmp, err := os.MkdirTemp("", "bills-trim-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if outb, err := exec.Command(sep, "-f", "1", "-l", strconv.Itoa(last), in, filepath.Join(tmp, "p-%d.pdf")).CombinedOutput(); err != nil {
		return fmt.Errorf("pdfseparate: %v: %s", err, strings.TrimSpace(string(outb)))
	}
	args := make([]string, 0, last+1)
	for i := 1; i <= last; i++ {
		args = append(args, filepath.Join(tmp, fmt.Sprintf("p-%d.pdf", i)))
	}
	args = append(args, out)
	if outb, err := exec.Command(unite, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("pdfunite: %v: %s", err, strings.TrimSpace(string(outb)))
	}
	return nil
}

// PageTexts extracts the text of every page. Uses poppler's pdftotext when
// available (best layout fidelity), falling back to a pure-Go extractor.
func PageTexts(path string) ([]string, error) {
	if exe, err := exec.LookPath("pdftotext"); err == nil {
		out, err := exec.Command(exe, "-layout", path, "-").Output()
		if err == nil {
			pages := strings.Split(string(out), "\f")
			if n := len(pages); n > 0 && strings.TrimSpace(pages[n-1]) == "" {
				pages = pages[:n-1]
			}
			return pages, nil
		}
	}
	return pageTextsGo(path)
}

func pageTextsGo(path string) (pages []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pdf text extraction failed: %v", r)
		}
	}()
	f, r, err := pdf.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			pages = append(pages, "")
			continue
		}
		t, err := p.GetPlainText(nil)
		if err != nil {
			t = ""
		}
		pages = append(pages, t)
	}
	return pages, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
