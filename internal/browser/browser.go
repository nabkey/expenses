// Package browser wraps chromedp with a persistent Chrome profile so that a
// login done interactively (headed) can be reused by later headless runs.
package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"
)

// Debug turns on chromedp's internal error logging (protocol noise).
var Debug = os.Getenv("BILLS_DEBUG") != ""

// Options configures a Launch or Attach.
type Options struct {
	ProfileDir  string // persistent user-data-dir (Launch only)
	DownloadDir string // where browser downloads land
	Headless    bool   // Launch only
	DebugPort   int    // Launch only: expose DevTools on this port so others can Attach
	CookieFile  string // if set: restore cookies from here on Launch, save on Close
}

// Browser is a single Chrome instance with one tab.
type Browser struct {
	Ctx      context.Context
	Headless bool
	Attached bool // connected to someone else's Chrome; Close leaves it running
	Opts     Options

	allocCancel context.CancelFunc
	tabCancel   context.CancelFunc // Attach only: closes just our tab

	netMu  sync.Mutex
	netLog []NetEntry
}

// NetEntry is one observed HTTP response.
type NetEntry struct {
	RequestID string    `json:"request_id"`
	URL       string    `json:"url"`
	Mime      string    `json:"mime"`
	Status    int64     `json:"status"`
	Type      string    `json:"type"`
	At        time.Time `json:"at"`
	Done      bool      `json:"done"`   // body fully received
	Failed    bool      `json:"failed"` // loading failed
}

// FindChrome returns the Chrome binary to use. BILLS_CHROME overrides.
func FindChrome() string {
	if p := os.Getenv("BILLS_CHROME"); p != "" {
		return p
	}
	for _, p := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "" // let chromedp search PATH
}

func contextOpts() []chromedp.ContextOption {
	if Debug {
		return nil
	}
	return []chromedp.ContextOption{chromedp.WithErrorf(func(string, ...interface{}) {})}
}

// Launch starts Chrome on o.ProfileDir.
func Launch(parent context.Context, o Options) (*Browser, error) {
	for _, d := range []string{o.ProfileDir, o.DownloadDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}

	// Deliberately NOT using chromedp.DefaultExecAllocatorOptions: it adds
	// --enable-automation (sets navigator.webdriver, shows the "controlled by
	// automated software" bar) which bot-detection on carrier sites keys on.
	opts := []chromedp.ExecAllocatorOption{
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.UserDataDir(o.ProfileDir),
		chromedp.WindowSize(1400, 1000),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		// Keep cookie encryption keys consistent between headed and headless
		// launches and avoid macOS Keychain prompts.
		chromedp.Flag("use-mock-keychain", true),
		chromedp.Flag("password-store", "basic"),
		chromedp.Flag("disable-background-timer-throttling", true),
		chromedp.Flag("disable-backgrounding-occluded-windows", true),
		chromedp.Flag("disable-renderer-backgrounding", true),
		chromedp.Flag("disable-search-engine-choice-screen", true),
		chromedp.Flag("hide-crash-restore-bubble", true),
		chromedp.Flag("disable-features", "Translate,OptimizationHints,MediaRouter,PrivacySandboxSettings4"),
	}
	if p := FindChrome(); p != "" {
		opts = append(opts, chromedp.ExecPath(p))
	}
	if o.Headless {
		opts = append(opts, chromedp.Flag("headless", true))
	}
	if o.DebugPort > 0 {
		opts = append(opts, chromedp.Flag("remote-debugging-port", strconv.Itoa(o.DebugPort)))
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(parent, opts...)
	ctx, _ := chromedp.NewContext(allocCtx, contextOpts()...)
	b := &Browser{Ctx: ctx, Headless: o.Headless, Opts: o, allocCancel: allocCancel}

	var ua string
	if err := chromedp.Run(ctx,
		network.Enable(),
		browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllowAndName).
			WithDownloadPath(o.DownloadDir).
			WithEventsEnabled(true),
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, _, _, ua0, _, err := browser.GetVersion().Do(ctx)
			ua = ua0
			return err
		}),
	); err != nil {
		allocCancel()
		return nil, fmt.Errorf("launch chrome: %w", err)
	}
	// Headless Chrome advertises itself in the UA string; present as regular Chrome.
	if o.Headless && strings.Contains(ua, "HeadlessChrome") {
		ua = strings.ReplaceAll(ua, "HeadlessChrome", "Chrome")
		_ = chromedp.Run(ctx, emulation.SetUserAgentOverride(ua))
	}
	b.startNetLog()
	if o.CookieFile != "" {
		if err := b.LoadCookies(o.CookieFile); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not restore cookies: %v\n", err)
		}
	}
	return b, nil
}

// Attach connects to a Chrome that is already running with
// --remote-debugging-port (see `bills session`). Close leaves it running.
func Attach(devtoolsURL string, o Options) (*Browser, error) {
	if err := os.MkdirAll(o.DownloadDir, 0o755); err != nil {
		return nil, err
	}
	// Not tied to the caller's signal context on purpose: cancelling the
	// allocator would make chromedp send Browser.close to the shared Chrome.
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), devtoolsURL)
	firstCtx, _ := chromedp.NewContext(allocCtx, contextOpts()...)
	// The first context latches onto whatever tab already exists (possibly
	// the one a person is signing in on), so do our work in a fresh tab.
	if err := chromedp.Run(firstCtx); err != nil {
		return nil, fmt.Errorf("attach to %s: %w (is `bills session` running?)", devtoolsURL, err)
	}
	ctx, tabCancel := chromedp.NewContext(firstCtx)
	b := &Browser{Ctx: ctx, Attached: true, Opts: o, allocCancel: allocCancel, tabCancel: tabCancel}
	if err := chromedp.Run(ctx,
		network.Enable(),
		browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllowAndName).
			WithDownloadPath(o.DownloadDir).
			WithEventsEnabled(true),
	); err != nil {
		return nil, fmt.Errorf("attach to %s: %w (is `bills session` running?)", devtoolsURL, err)
	}
	b.startNetLog()
	return b, nil
}

// Close shuts Chrome down gracefully so the profile is flushed to disk.
// For an attached browser it only drops our connection.
func (b *Browser) Close() {
	if b.Opts.CookieFile != "" {
		if err := b.SaveCookies(b.Opts.CookieFile); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not save cookies: %v\n", err)
		}
	}
	if b.Attached {
		// Close our tab; leave the shared browser (and its login) alone.
		if b.tabCancel != nil {
			b.tabCancel()
		}
		return
	}
	_ = chromedp.Cancel(b.Ctx)
	b.allocCancel()
}

// ---------------------------------------------------------------- cookies

type cookieJar struct {
	SavedAt time.Time         `json:"saved_at"`
	Cookies []*network.Cookie `json:"cookies"`
}

// SaveCookies writes every cookie (session cookies included — Chrome would
// otherwise drop those on restart) to path.
func (b *Browser) SaveCookies(path string) error {
	var cookies []*network.Cookie
	err := chromedp.Run(b.Ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		cookies, err = storage.GetCookies().Do(ctx)
		return err
	}))
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cookieJar{SavedAt: time.Now(), Cookies: cookies}, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// LoadCookies restores cookies saved by SaveCookies. Missing file is fine.
func (b *Browser) LoadCookies(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var jar cookieJar
	if err := json.Unmarshal(data, &jar); err != nil {
		return err
	}
	now := time.Now()
	params := make([]*network.CookieParam, 0, len(jar.Cookies))
	for _, c := range jar.Cookies {
		p := &network.CookieParam{
			Name:         c.Name,
			Value:        c.Value,
			Domain:       c.Domain,
			Path:         c.Path,
			Secure:       c.Secure,
			HTTPOnly:     c.HTTPOnly,
			SameSite:     c.SameSite,
			PartitionKey: c.PartitionKey,
		}
		if !c.Session && c.Expires > 0 {
			exp := time.Unix(int64(c.Expires), 0)
			if exp.Before(now) {
				continue
			}
			t := cdp.TimeSinceEpoch(exp)
			p.Expires = &t
		}
		params = append(params, p)
	}
	if len(params) == 0 {
		return nil
	}
	return chromedp.Run(b.Ctx, network.SetCookies(params))
}

// ---------------------------------------------------------------- network log

func (b *Browser) startNetLog() {
	mark := func(id network.RequestID, failed bool) {
		b.netMu.Lock()
		defer b.netMu.Unlock()
		for i := len(b.netLog) - 1; i >= 0; i-- {
			if b.netLog[i].RequestID == string(id) {
				b.netLog[i].Done = !failed
				b.netLog[i].Failed = failed
				return
			}
		}
	}
	chromedp.ListenTarget(b.Ctx, func(ev interface{}) {
		switch e := ev.(type) {
		case *network.EventResponseReceived:
			b.netMu.Lock()
			b.netLog = append(b.netLog, NetEntry{
				RequestID: string(e.RequestID),
				URL:       e.Response.URL,
				Mime:      e.Response.MimeType,
				Status:    e.Response.Status,
				Type:      string(e.Type),
				At:        time.Now(),
			})
			b.netMu.Unlock()
		case *network.EventLoadingFinished:
			mark(e.RequestID, false)
		case *network.EventLoadingFailed:
			mark(e.RequestID, true)
		}
	})
}

// NetLog returns a copy of all responses seen so far.
func (b *Browser) NetLog() []NetEntry {
	b.netMu.Lock()
	defer b.netMu.Unlock()
	out := make([]NetEntry, len(b.netLog))
	copy(out, b.netLog)
	return out
}

// ---------------------------------------------------------------- page helpers

// Navigate loads a URL. A navigation that turns into a file download makes
// Chrome report net::ERR_ABORTED; that is not an error for our purposes.
func (b *Browser) Navigate(url string) error {
	err := chromedp.Run(b.Ctx, chromedp.Navigate(url))
	if err != nil && strings.Contains(err.Error(), "net::ERR_ABORTED") {
		return nil
	}
	return err
}

// Location returns the current page URL.
func (b *Browser) Location() (string, error) {
	var u string
	err := chromedp.Run(b.Ctx, chromedp.Location(&u))
	return u, err
}

// Sleep waits, but returns early if the browser context dies.
func (b *Browser) Sleep(d time.Duration) {
	select {
	case <-time.After(d):
	case <-b.Ctx.Done():
	}
}

// Eval runs JS in the page and (if out != nil) decodes the JSON result.
// Promises are awaited.
func (b *Browser) Eval(js string, out interface{}) error {
	return chromedp.Run(b.Ctx, chromedp.Evaluate(js, out, func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
		return p.WithAwaitPromise(true)
	}))
}

// WaitUntil polls cond until it returns true or timeout elapses.
func (b *Browser) WaitUntil(timeout, every time.Duration, cond func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := cond()
		if err == nil && ok {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("timed out: %w", err)
			}
			return errors.New("timed out")
		}
		b.Sleep(every)
		if b.Ctx.Err() != nil {
			return b.Ctx.Err()
		}
	}
}

// HasPasswordField reports whether a visible password input is on the page.
func (b *Browser) HasPasswordField() bool {
	var has bool
	_ = b.Eval(`Array.from(document.querySelectorAll('input[type=password]')).some(e => e.offsetParent !== null)`, &has)
	return has
}

// HTML returns the page's outerHTML.
func (b *Browser) HTML() (string, error) {
	var s string
	err := chromedp.Run(b.Ctx, chromedp.OuterHTML("html", &s, chromedp.ByQuery))
	return s, err
}

// Text returns the page's visible innerText.
func (b *Browser) Text() (string, error) {
	var s string
	err := b.Eval(`document.body ? document.body.innerText : ''`, &s)
	return s, err
}

// Screenshot saves a full-page PNG.
func (b *Browser) Screenshot(path string) error {
	var buf []byte
	if err := chromedp.Run(b.Ctx, chromedp.FullScreenshot(&buf, 80)); err != nil {
		return err
	}
	return os.WriteFile(path, buf, 0o644)
}

// FetchBytes downloads url from inside the page (so cookies/session apply)
// and returns the raw bytes.
func (b *Browser) FetchBytes(url string) ([]byte, error) {
	return b.BlobBytes(fmt.Sprintf(`await fetch(%q, {credentials: 'include'}).then(r => {
		if (!r.ok) throw new Error('HTTP ' + r.status + ' ' + r.statusText);
		return r.blob();
	})`, url))
}

// BlobBytes evaluates expr (which may use await) to a Blob/Response-like
// object with arrayBuffer() and returns its bytes.
func (b *Browser) BlobBytes(expr string) ([]byte, error) {
	js := fmt.Sprintf(`(async () => {
		const blob = (%s);
		if (!blob) throw new Error('no blob');
		const bytes = new Uint8Array(await blob.arrayBuffer());
		let s = '';
		for (let i = 0; i < bytes.length; i += 0x8000) {
			s += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000));
		}
		return btoa(s);
	})()`, expr)
	var b64 string
	if err := b.Eval(js, &b64); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(b64)
}

// ResponseBody fetches the body of an already-completed response by request id.
func (b *Browser) ResponseBody(requestID string) ([]byte, error) {
	var body []byte
	err := chromedp.Run(b.Ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		body, err = network.GetResponseBody(network.RequestID(requestID)).Do(ctx)
		return err
	}))
	return body, err
}

// CaptureDownload runs trigger (e.g. a click) and waits for the resulting
// browser download to finish, returning its bytes.
func (b *Browser) CaptureDownload(trigger chromedp.Action, timeout time.Duration) ([]byte, error) {
	lctx, lcancel := context.WithCancel(b.Ctx)
	defer lcancel()

	done := make(chan string, 8)
	failed := make(chan string, 8)
	chromedp.ListenTarget(lctx, func(ev interface{}) {
		if e, ok := ev.(*browser.EventDownloadProgress); ok {
			switch e.State {
			case browser.DownloadProgressStateCompleted:
				select {
				case done <- e.GUID:
				default:
				}
			case browser.DownloadProgressStateCanceled:
				select {
				case failed <- e.GUID:
				default:
				}
			}
		}
	})

	if err := chromedp.Run(b.Ctx, trigger); err != nil && !strings.Contains(err.Error(), "net::ERR_ABORTED") {
		return nil, err
	}

	select {
	case guid := <-done:
		p := filepath.Join(b.Opts.DownloadDir, guid)
		data, err := os.ReadFile(p)
		_ = os.Remove(p)
		return data, err
	case <-failed:
		return nil, errors.New("download was canceled")
	case <-time.After(timeout):
		return nil, errors.New("no download started")
	case <-b.Ctx.Done():
		return nil, b.Ctx.Err()
	}
}
