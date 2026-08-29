package pdfx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ledongthuc/pdf"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// Prepend writes out = the pages of cover (PDF bytes) followed by every
// page of in. Like keepPages it reads carrier PDFs leniently with pdfcpu
// and falls back to poppler's pdfunite.
func Prepend(cover []byte, in, out string) error {
	err := prependPdfcpu(cover, in, out)
	if err == nil {
		return nil
	}
	if perr := prependPoppler(cover, in, out); perr == nil {
		return nil
	} else {
		return fmt.Errorf("pdfcpu: %v; poppler: %v", err, perr)
	}
}

func prependPdfcpu(cover []byte, in, out string) (err error) {
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
	w, err := os.Create(out)
	if err != nil {
		return err
	}
	if err := api.MergeRaw([]io.ReadSeeker{bytes.NewReader(cover), f}, w, false, conf); err != nil {
		w.Close()
		os.Remove(out)
		return err
	}
	return w.Close()
}

func prependPoppler(cover []byte, in, out string) error {
	unite, err := exec.LookPath("pdfunite")
	if err != nil {
		return errors.New("pdfunite not installed (brew install poppler)")
	}
	tmp, err := os.MkdirTemp("", "bills-cover-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	c := filepath.Join(tmp, "cover.pdf")
	if err := os.WriteFile(c, cover, 0o644); err != nil {
		return err
	}
	if outb, err := exec.Command(unite, c, in, out).CombinedOutput(); err != nil {
		return fmt.Errorf("pdfunite: %v: %s", err, strings.TrimSpace(string(outb)))
	}
	return nil
}

// PageCount returns the number of pages in a PDF. pdfcpu first; carrier
// PDFs that fail its validator go through the pure-Go reader, then pdfinfo.
func PageCount(path string) (int, error) {
	if n, err := pageCountPdfcpu(path); err == nil {
		return n, nil
	}
	if n, err := pageCountGo(path); err == nil {
		return n, nil
	}
	return pageCountPoppler(path)
}

func pageCountPdfcpu(path string) (n int, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pdfcpu panicked: %v", r)
		}
	}()
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	conf := model.NewDefaultConfiguration()
	conf.ValidationMode = model.ValidationRelaxed
	return api.PageCount(f, conf)
}

func pageCountGo(path string) (n int, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pdf reader panicked: %v", r)
		}
	}()
	f, r, err := pdf.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if n = r.NumPage(); n <= 0 {
		return 0, errors.New("no pages")
	}
	return n, nil
}

func pageCountPoppler(path string) (int, error) {
	exe, err := exec.LookPath("pdfinfo")
	if err != nil {
		return 0, errors.New("pdfinfo not installed (brew install poppler)")
	}
	out, err := exec.Command(exe, path).Output()
	if err != nil {
		return 0, fmt.Errorf("pdfinfo: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Pages:") {
			return strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Pages:")))
		}
	}
	return 0, errors.New("pdfinfo: no page count")
}
