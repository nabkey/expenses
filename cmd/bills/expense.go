package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"expenses/internal/expense"
	"expenses/internal/site"
)

// cmdExpense prepares Concur-ready PDFs: a generated cover sheet (vendor,
// transaction date = auto-pay date, amount claimed, expense type) followed
// by the statement. Only statements whose auto-pay date has passed are
// prepared; the rest are listed as waiting.
func cmdExpense(args []string) error {
	fs := flag.NewFlagSet("expense", flag.ExitOnError)
	root := fs.String("root", rootDir(), "project directory")
	outDir := fs.String("out", "", "where to write prepared PDFs (default: bills/expense)")
	asOf := fs.String("asof", "", "pretend today is this date (YYYY-MM-DD) when checking auto-pay dates")
	force := fs.Bool("force", false, "regenerate PDFs that already exist")
	amount := fs.Float64("amount", 0, "override the amount claimed (single site only)")
	etype := fs.String("type", "", "override the expense type (single site only)")
	pos := parseFlags(fs, args)
	sites, err := site.Select(first(pos))
	if err != nil {
		return err
	}
	if (*amount != 0 || *etype != "") && len(sites) != 1 {
		return errors.New("-amount and -type apply to one site: bills expense xfinity -amount 130")
	}
	today := time.Now()
	if *asOf != "" {
		if today, err = time.Parse("2006-01-02", *asOf); err != nil {
			return fmt.Errorf("-asof: %w", err)
		}
	}
	y, m, dd := today.Date()
	today = time.Date(y, m, dd, 0, 0, 0, 0, time.UTC) // compare dates, not clock times
	p := newPaths(*root)
	if *outDir == "" {
		*outDir = filepath.Join(p.bills, "expense")
	}
	cfg, err := expense.LoadConfig(p.config)
	if err != nil {
		return err
	}

	var prepared, failed int
	for _, s := range sites {
		prof, ok := expense.ProfileFor(s.Name())
		if !ok {
			return fmt.Errorf("no expense profile for %s", s.Name())
		}
		prof = prof.Apply(cfg.Expense[s.Name()])
		if *amount != 0 {
			prof.Amount = *amount
		}
		if *etype != "" {
			prof.ExpenseType = *etype
		}
		if err := prof.Validate(); err != nil {
			return err
		}
		results, err := expense.Prepare(p.bills, *outDir, prof, today, *force)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] %v\n", s.Name(), err)
			failed++
			continue
		}
		fmt.Printf("[%s] %d statement(s) in %s — %s, $%.2f each\n", s.Name(), len(results), rel(p.root, filepath.Join(p.bills, s.Name())), prof.ExpenseType, prof.Amount)
		for _, r := range results {
			name := strings.TrimSuffix(filepath.Base(r.Source), ".pdf")
			if r.Status == expense.Failed && r.Bill == nil {
				fmt.Printf("  %s  %-16s FAILED: %v\n", name, "", r.Err)
				failed++
				continue
			}
			paid := "paid " + r.Bill.AutoPay.Format("2006-01-02")
			switch r.Status {
			case expense.Prepared:
				prepared++
				fmt.Printf("  %s  %s  prepared %s (%d pages)\n", name, paid, rel(p.root, r.Out), r.Pages)
			case expense.Exists:
				fmt.Printf("  %s  %s  exists   %s\n", name, paid, rel(p.root, r.Out))
			case expense.Waiting:
				if days := int(r.Bill.AutoPay.Sub(today).Hours() / 24); days <= 0 {
					fmt.Printf("  %s  %s  waiting  auto-pay is today\n", name, paid)
				} else {
					fmt.Printf("  %s  %s  waiting  auto-pay is in %d day(s)\n", name, paid, days)
				}
			case expense.Failed:
				failed++
				fmt.Printf("  %s  %s  FAILED: %v\n", name, paid, r.Err)
			}
		}
	}
	if prepared > 0 {
		fmt.Printf("\n%d PDF(s) ready in %s. Email each one as an attachment, blank subject, from your\n"+
			"Concur-verified address to receipts@expenseit.com, then check Available Expenses.\n", prepared, rel(p.root, *outDir))
	}
	if failed > 0 {
		return fmt.Errorf("%d statement(s) could not be prepared", failed)
	}
	return nil
}
