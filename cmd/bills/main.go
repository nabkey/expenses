// bills — pull Xfinity and Verizon statements as PDFs for expense filing.
//
//	bills login  [xfinity|verizon|all]   headed Chrome; you sign in, session is saved
//	bills fetch  [xfinity|verizon|all]   headless; caches the last N months of bills
//	bills trim   in.pdf [-o out.pdf]     cut a Verizon PDF at the call-record pages
//	bills expense [xfinity|verizon|all] add an expense cover sheet to each paid statement
//	bills inspect <site|url>             dump a page (screenshot/html/links/network)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"expenses/internal/browser"
	"expenses/internal/pdfx"
	"expenses/internal/site"
)

var errNeedLogin = errors.New("login required")

func usage() {
	fmt.Fprint(os.Stderr, `usage: bills <command> [flags] [site]

commands:
  login   [xfinity|verizon|all]  open Chrome so you can sign in; the session is
                                 saved to .chrome-profile/ for later headless runs
  fetch   [xfinity|verizon|all]  download any statements from the last 12 months
                                 that aren't already in bills/<site>/ (headless;
                                 pops a window automatically if a login expired)
  session [xfinity|verizon|all]  like login, but keeps Chrome open (DevTools on
                                 :9222) so other commands can use it via -attach
  trim    <in.pdf> [-o out.pdf]  drop call-record pages from a Verizon PDF
  expense [xfinity|verizon|all]  write bills/expense/<Site>_<paid date>_<amount>.pdf —
                                 a cover sheet (vendor, auto-pay date, amount, expense
                                 type) followed by the statement — for every statement
                                 whose auto-pay date has passed
  inspect <site|url>             save screenshot/html/links/network of a page
                                 (for debugging when a portal changes)

Run 'bills <command> -h' for flags. BILLS_HOME overrides the working directory.
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		// First signal: let the command wind down (save cookies, close
		// Chrome). If that takes too long, or a second signal arrives, bail.
		<-ctx.Done()
		stop()
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		select {
		case <-sig:
		case <-time.After(15 * time.Second):
		}
		fmt.Fprintln(os.Stderr, "forced exit")
		os.Exit(130)
	}()

	var err error
	switch os.Args[1] {
	case "login":
		err = cmdLogin(ctx, os.Args[2:])
	case "session":
		err = cmdSession(ctx, os.Args[2:])
	case "fetch":
		err = cmdFetch(ctx, os.Args[2:])
	case "trim":
		err = cmdTrim(os.Args[2:])
	case "expense":
		err = cmdExpense(os.Args[2:])
	case "inspect":
		err = cmdInspect(ctx, os.Args[2:])
	case "js":
		err = cmdJS(ctx, os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type paths struct{ root, profile, downloads, bills, cookies, config string }

func newPaths(root string) paths {
	return paths{
		root:      root,
		profile:   filepath.Join(root, ".chrome-profile"),
		downloads: filepath.Join(root, ".downloads"),
		bills:     filepath.Join(root, "bills"),
		cookies:   filepath.Join(root, ".chrome-profile", "cookies.json"),
		config:    filepath.Join(root, "bills.json"),
	}
}

const defaultPort = 9222

func devtoolsURL(port int) string { return fmt.Sprintf("http://127.0.0.1:%d", port) }

// open launches Chrome on the profile, or attaches to a running
// `bills session` browser when attach is set.
func open(ctx context.Context, p paths, headless, attach bool, port int) (*browser.Browser, error) {
	o := browser.Options{
		ProfileDir:  p.profile,
		DownloadDir: p.downloads,
		Headless:    headless,
		CookieFile:  p.cookies,
	}
	if attach {
		return browser.Attach(devtoolsURL(port), o)
	}
	// Chrome gets its own context: the signal context only unblocks waits,
	// so a Ctrl-C still lets Close() save cookies and shut Chrome down
	// gracefully. A second Ctrl-C (or a hung shutdown) is handled by the
	// watchdog in main.
	_ = ctx
	return browser.Launch(context.Background(), o)
}

func rootDir() string {
	if v := os.Getenv("BILLS_HOME"); v != "" {
		return v
	}
	wd, _ := os.Getwd()
	return wd
}

// ---------------------------------------------------------------- login

func cmdLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	root := fs.String("root", rootDir(), "project directory")
	timeout := fs.Duration("timeout", 15*time.Minute, "how long to wait for you to sign in")
	pos := parseFlags(fs, args)
	sites, err := site.Select(first(pos))
	if err != nil {
		return err
	}
	p := newPaths(*root)
	b, err := open(ctx, p, false, false, 0)
	if err != nil {
		return err
	}
	defer b.Close()
	for _, s := range sites {
		if err := ensureLoggedIn(ctx, b, s, *timeout); err != nil {
			return err
		}
	}
	fmt.Println("Sessions saved in", p.profile)
	return nil
}

// ---------------------------------------------------------------- session

func cmdSession(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("session", flag.ExitOnError)
	root := fs.String("root", rootDir(), "project directory")
	port := fs.Int("port", defaultPort, "DevTools port to listen on")
	timeout := fs.Duration("timeout", 15*time.Minute, "how long to wait for you to sign in")
	pos := parseFlags(fs, args)
	sites, err := site.Select(first(pos))
	if err != nil {
		return err
	}
	p := newPaths(*root)
	b, err := browser.Launch(context.Background(), browser.Options{
		ProfileDir: p.profile, DownloadDir: p.downloads, CookieFile: p.cookies, DebugPort: *port,
	})
	if err != nil {
		return err
	}
	defer b.Close()
	for _, s := range sites {
		if err := ensureLoggedIn(ctx, b, s, *timeout); err != nil {
			return err
		}
	}
	fmt.Printf("Session browser is up (DevTools %s). In another terminal:\n"+
		"  bills fetch -attach\n  bills inspect verizon -attach\nCtrl-C here to close it.\n", devtoolsURL(*port))
	<-ctx.Done()
	fmt.Println("closing session browser")
	return nil
}

// ensureLoggedIn navigates to the site's history page and, if it bounces to
// a login page, waits for the user to sign in (headed only).
func ensureLoggedIn(ctx context.Context, b *browser.Browser, s site.Site, timeout time.Duration) error {
	if err := b.Navigate(s.HistoryURL()); err != nil {
		return fmt.Errorf("[%s] open %s: %w", s.Name(), s.HistoryURL(), err)
	}
	st, err := settle(b, s, 25*time.Second)
	if err != nil {
		return fmt.Errorf("[%s] %w", s.Name(), err)
	}
	if st == site.StatusLoggedIn {
		fmt.Printf("[%s] signed in\n", s.Name())
		return nil
	}
	if b.Headless {
		return errNeedLogin
	}
	fmt.Printf("[%s] Sign in using the Chrome window (password, MFA, etc).\n"+
		"          I'll continue automatically once your bills show up — or press Enter here.\n", s.Name())
	return waitForLogin(ctx, b, s, timeout)
}

// settle polls until the page is definitively signed-in or a login page,
// giving slow single-page apps time to redirect and render.
func settle(b *browser.Browser, s site.Site, timeout time.Duration) (site.Status, error) {
	deadline := time.Now().Add(timeout)
	for {
		st, err := s.Status(b)
		if errors.Is(err, site.ErrUnavailable) {
			return site.StatusUnknown, err
		}
		if err == nil && st != site.StatusUnknown {
			return st, nil
		}
		if time.Now().After(deadline) || b.Ctx.Err() != nil {
			return site.StatusUnknown, nil
		}
		b.Sleep(1500 * time.Millisecond)
	}
}

func waitForLogin(ctx context.Context, b *browser.Browser, s site.Site, timeout time.Duration) error {
	enter := make(chan struct{}, 1)
	go func() {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !strings.HasSuffix(line, "\n") {
			return // EOF (e.g. no terminal attached) — not a keypress
		}
		select {
		case enter <- struct{}{}:
		default:
		}
	}()
	deadline := time.After(timeout)
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("[%s] gave up waiting for sign-in", s.Name())
		case <-enter:
			if st, _ := s.Status(b); st != site.StatusLoggedIn {
				fmt.Printf("[%s] page doesn't look signed in yet (%s) — continuing anyway\n", s.Name(), st)
			}
			return nil
		case <-tick.C:
			if st, err := s.Status(b); err == nil && st == site.StatusLoggedIn {
				fmt.Printf("[%s] signed in\n", s.Name())
				b.Sleep(2 * time.Second)
				return nil
			}
		}
	}
}

// ---------------------------------------------------------------- fetch

func cmdFetch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	root := fs.String("root", rootDir(), "project directory")
	months := fs.Int("months", 12, "how many months back to keep cached")
	headed := fs.Bool("headed", false, "show the Chrome window")
	force := fs.Bool("force", false, "re-download bills that are already cached")
	keepPartial := fs.Bool("keep-partial", false, "keep the page on which call records start (Verizon)")
	timeout := fs.Duration("login-timeout", 15*time.Minute, "how long to wait for a sign-in")
	attach := fs.Bool("attach", false, "use the Chrome started by `bills session` instead of launching one")
	port := fs.Int("port", defaultPort, "DevTools port of the session browser (with -attach)")
	pos := parseFlags(fs, args)
	sites, err := site.Select(first(pos))
	if err != nil {
		return err
	}
	p := newPaths(*root)
	since := time.Now().AddDate(0, -*months, 0)

	b, err := open(ctx, p, !*headed, *attach, *port)
	if err != nil {
		return err
	}
	defer func() { b.Close() }()

	var failed []string
	for _, s := range sites {
		err := ensureLoggedIn(ctx, b, s, *timeout)
		if errors.Is(err, errNeedLogin) {
			fmt.Printf("[%s] session expired — opening a Chrome window so you can sign in\n", s.Name())
			b.Close()
			if b, err = open(ctx, p, false, false, 0); err != nil {
				return err
			}
			err = ensureLoggedIn(ctx, b, s, *timeout)
		}
		if err != nil {
			return err
		}
		if err := fetchSite(b, s, p, since, *force, *keepPartial); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] %v\n", s.Name(), err)
			failed = append(failed, s.Name())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("problems with: %s", strings.Join(failed, ", "))
	}
	return nil
}

func fetchSite(b *browser.Browser, s site.Site, p paths, since time.Time, force, keepPartial bool) error {
	stmts, err := s.Statements(b, since)
	if err != nil {
		return err
	}
	if len(stmts) == 0 {
		return fmt.Errorf("no statements since %s found on %s", since.Format("2006-01-02"), s.HistoryURL())
	}
	dir := filepath.Join(p.bills, s.Name())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	fmt.Printf("[%s] %d statement(s) since %s\n", s.Name(), len(stmts), since.Format("2006-01-02"))

	var problems []string
	for _, st := range stmts {
		name := st.Date.Format("2006-01-02")
		out := filepath.Join(dir, name+".pdf")
		if !force {
			if _, err := os.Stat(out); err == nil {
				fmt.Printf("  %s  %-10s cached\n", name, st.Amount)
				continue
			}
		}
		data, err := s.Download(b, st)
		if err != nil {
			fmt.Printf("  %s  %-10s FAILED: %v\n", name, st.Amount, err)
			problems = append(problems, name)
			continue
		}
		if !s.TrimCallRecords() {
			if err := os.WriteFile(out, data, 0o644); err != nil {
				return err
			}
			fmt.Printf("  %s  %-10s saved %s (%d KB)\n", name, st.Amount, rel(p.root, out), len(data)/1024)
			continue
		}
		rawDir := filepath.Join(dir, "raw")
		if err := os.MkdirAll(rawDir, 0o755); err != nil {
			return err
		}
		raw := filepath.Join(rawDir, name+".full.pdf")
		if err := os.WriteFile(raw, data, 0o644); err != nil {
			return err
		}
		a, err := pdfx.Trim(raw, out, keepPartial)
		if err != nil {
			fmt.Printf("  %s  %-10s downloaded but trim FAILED: %v (full copy at %s)\n", name, st.Amount, err, rel(p.root, raw))
			problems = append(problems, name)
			continue
		}
		if a.Cut == 0 {
			fmt.Printf("  %s  %-10s saved %s (%d pages, no call records found)\n", name, st.Amount, rel(p.root, out), a.Pages)
		} else {
			kept := a.Cut - 1
			if keepPartial {
				kept = a.Cut
			}
			fmt.Printf("  %s  %-10s saved %s (%d → %d pages; call records start p.%d)\n", name, st.Amount, rel(p.root, out), a.Pages, kept, a.Cut)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("could not fetch %s", strings.Join(problems, ", "))
	}
	return nil
}

func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return r
	}
	return p
}

// ---------------------------------------------------------------- trim

func cmdTrim(args []string) error {
	fs := flag.NewFlagSet("trim", flag.ExitOnError)
	out := fs.String("o", "", "output file (default: <in>-trimmed.pdf)")
	explain := fs.Bool("explain", false, "print the per-page verdicts")
	keepPartial := fs.Bool("keep-partial", false, "keep the page on which call records start")
	pos := parseFlags(fs, args)
	in := first(pos)
	if in == "" {
		return errors.New("usage: bills trim <in.pdf> [-o out.pdf] [-explain]")
	}
	if *out == "" {
		*out = strings.TrimSuffix(in, filepath.Ext(in)) + "-trimmed.pdf"
	}
	a, err := pdfx.Trim(in, *out, *keepPartial)
	if err != nil {
		return err
	}
	if *explain {
		for _, v := range a.Verdicts {
			fmt.Println(" ", v)
		}
	}
	if a.Cut == 0 {
		fmt.Printf("%s: %d pages, no call records found — copied unchanged to %s\n", in, a.Pages, *out)
		return nil
	}
	kept := a.Cut - 1
	if *keepPartial {
		kept = a.Cut
	}
	fmt.Printf("%s: %d pages, call records start on p.%d — kept %d page(s) → %s\n", in, a.Pages, a.Cut, kept, *out)
	return nil
}

// ---------------------------------------------------------------- inspect

func cmdInspect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	root := fs.String("root", rootDir(), "project directory")
	outDir := fs.String("out", "", "where to write the dump (default: .inspect/<timestamp>)")
	headed := fs.Bool("headed", false, "show the Chrome window")
	wait := fs.Duration("wait", 6*time.Second, "how long to let the page settle")
	attach := fs.Bool("attach", false, "use the Chrome started by `bills session`")
	port := fs.Int("port", defaultPort, "DevTools port of the session browser (with -attach)")
	bodies := fs.String("bodies", "", "also save bodies of responses whose URL contains this substring")
	pos := parseFlags(fs, args)
	target := first(pos)
	if target == "" {
		return errors.New("usage: bills inspect <xfinity|verizon|url>")
	}
	p := newPaths(*root)
	url := target
	if sites, err := site.Select(target); err == nil && len(sites) == 1 {
		url = sites[0].HistoryURL()
	}
	if *outDir == "" {
		*outDir = filepath.Join(p.root, ".inspect", time.Now().Format("20060102-150405"))
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}

	b, err := open(ctx, p, !*headed, *attach, *port)
	if err != nil {
		return err
	}
	defer b.Close()
	if err := b.Navigate(url); err != nil {
		return err
	}
	b.Sleep(*wait)

	loc, _ := b.Location()
	_ = os.WriteFile(filepath.Join(*outDir, "url.txt"), []byte(loc+"\n"), 0o644)
	if html, err := b.HTML(); err == nil {
		_ = os.WriteFile(filepath.Join(*outDir, "page.html"), []byte(html), 0o644)
	}
	if txt, err := b.Text(); err == nil {
		_ = os.WriteFile(filepath.Join(*outDir, "page.txt"), []byte(txt), 0o644)
	}
	_ = b.Screenshot(filepath.Join(*outDir, "screenshot.png"))
	if cands, err := site.Scrape(b); err == nil {
		writeJSON(filepath.Join(*outDir, "links.json"), cands)
	}
	writeJSON(filepath.Join(*outDir, "network.json"), b.NetLog())
	if *bodies != "" {
		n := 0
		for _, e := range b.NetLog() {
			if !strings.Contains(e.URL, *bodies) {
				continue
			}
			body, err := b.ResponseBody(e.RequestID)
			if err != nil {
				continue
			}
			n++
			name := fmt.Sprintf("body-%02d-%s", n, sanitize(e.URL))
			_ = os.WriteFile(filepath.Join(*outDir, name), body, 0o644)
		}
	}

	fmt.Printf("url: %s\nwrote %s/{url.txt,page.html,page.txt,screenshot.png,links.json,network.json}\n", loc, rel(p.root, *outDir))
	return nil
}

// ---------------------------------------------------------------- js (debug)

// cmdJS navigates to a URL (optional) and evaluates a JS expression or file,
// printing the JSON result. For poking at portals: `bills js -attach <url> <expr|file.js>`.
func cmdJS(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("js", flag.ExitOnError)
	root := fs.String("root", rootDir(), "project directory")
	headed := fs.Bool("headed", false, "show the Chrome window")
	wait := fs.Duration("wait", 5*time.Second, "how long to let the page settle after navigating")
	attach := fs.Bool("attach", false, "use the Chrome started by `bills session`")
	port := fs.Int("port", defaultPort, "DevTools port of the session browser (with -attach)")
	pos := parseFlags(fs, args)
	if len(pos) < 2 {
		return errors.New("usage: bills js [-attach] <url|site|-> <expression|file.js>")
	}
	target, code := pos[0], pos[1]
	if data, err := os.ReadFile(code); err == nil {
		code = string(data)
	}
	p := newPaths(*root)
	b, err := open(ctx, p, !*headed, *attach, *port)
	if err != nil {
		return err
	}
	defer b.Close()
	if target != "-" {
		url := target
		if sites, err := site.Select(target); err == nil && len(sites) == 1 {
			url = sites[0].HistoryURL()
		}
		if err := b.Navigate(url); err != nil {
			return err
		}
		b.Sleep(*wait)
	}
	var out json.RawMessage
	if err := b.Eval(code, &out); err != nil {
		return err
	}
	var pretty interface{}
	if json.Unmarshal(out, &pretty) == nil {
		if data, err := json.MarshalIndent(pretty, "", "  "); err == nil {
			out = data
		}
	}
	fmt.Println(string(out))
	return nil
}

func writeJSON(path string, v interface{}) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

// parseFlags lets flags appear before or after positional arguments
// (`bills trim in.pdf -explain`), which Go's flag package doesn't do alone.
func parseFlags(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		_ = fs.Parse(args)
		if fs.NArg() == 0 {
			return positional
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// sanitize turns a URL into something usable as a file name.
func sanitize(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	var sb strings.Builder
	for _, r := range u {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}
	out := sb.String()
	if len(out) > 120 {
		out = out[len(out)-120:]
	}
	return out
}
