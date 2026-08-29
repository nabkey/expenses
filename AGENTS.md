# AGENTS.md

Notes for anyone (human or agent) changing this tool.

## What it is

`bills` (Go, chromedp) pulls statement PDFs from Xfinity and Verizon into
`bills/<site>/`, trimming Verizon's call-record pages. Build with
`go build -o bin/bills ./cmd/bills`; `go vet ./...` must stay clean.

## Non-negotiables

- **Never commit** `.chrome-profile/` (live sessions, `cookies.json`),
  `bills/` (account numbers, addresses), `.inspect/` dumps, or `bills.json`
  (claim amounts). They're in `.gitignore`; keep them there. Keep real
  account numbers, invoice numbers, totals and addresses out of code
  comments, docs and test fixtures too — fixtures use made-up values.
- The user signs in themselves. Don't add credential storage or automated
  password entry; the design is "human does login/MFA once, code takes over."
- Downloads must stay idempotent: an existing `bills/<site>/<date>.pdf` is
  skipped unless `-force`.

## How the browser layer works (`internal/browser`)

- Chrome runs on `.chrome-profile/` with `--use-mock-keychain
  --password-store=basic` so cookies are readable in both headed and headless
  launches. `--enable-automation` is deliberately omitted and
  `--disable-blink-features=AutomationControlled` set; headless also rewrites
  the `HeadlessChrome` UA. Both carriers sit behind Akamai bot management.
- `Close()` snapshots all cookies (session cookies included) to
  `.chrome-profile/cookies.json`; `Launch()` restores them. This is what keeps
  Verizon signed in between runs.
- The chromedp context is *not* derived from the signal context — Ctrl-C only
  unblocks waits so `Close()` can still save cookies; a watchdog force-exits
  after 15 s or on a second signal.
- Every `Network.responseReceived` is logged (`NetLog()`), with
  `Done`/`Failed` set from loading events. Sites read SPA API responses from
  this log via `site.apiBody(...)`. Bodies are only readable until the tab
  navigates again, so always take a `mark := time.Now()` *before* navigating
  and only accept entries after it.
- `-attach` connects to a `bills session` Chrome (`--remote-debugging-port`).
  It works in a fresh tab and never cancels the first chromedp context —
  cancelling that would send `Browser.close` to the shared browser.

## Per-site facts (verified 2026-08-28)

**Xfinity** (`internal/site/xfinity.go`)
- History: `https://customer.xfinity.com/billing/services/statement/history`.
  Login persists on the profile.
- The SPA calls `api.sc.xfinity.com/session/csp/selfhelp/account/me/bill`
  (statement ids + dates) and `.../account/me/bills` (amounts, periods) with a
  bearer token we don't have — read them from the network log.
- Each statement renders at `statement/current#/<YYYY-MM-DD>` (the billing
  date). A hash-only navigation doesn't reload the SPA, so bounce through
  `about:blank`.
- "Print/Save Statement (PDF)" fetches the PDF over XHR, wraps it in a Blob,
  `window.open`s a `blob:` URL and revokes it. We hook `URL.createObjectURL`
  to keep the Blob and stub `window.open` so the SPA doesn't navigate the tab.
- Components are `<prism-*>` Stencil elements with *scoped* CSS, not shadow
  DOM — `querySelectorAll` works. Signed-in test: any
  `prism-lineitem[data-testid="statement-history-item"]`.
- A "We'll be back in a bit" page is a real HTTP 503 from Xfinity (seen ~20
  min on 2026-08-28); `Status()` returns `ErrUnavailable` for it.

**Verizon** (`internal/site/verizon.go`)
- History: `https://www.verizon.com/digital/nsa/secure/ui/bill/history/`.
  Login is session-cookie only; `secure.verizon.com/signin?...&goto=` is the
  login wall.
- One call, `gw/bill/billpaymenthistory/bill_history`, returns every cycle
  (`billEndDate` MM/DD/YYYY, `billAmount`, `billCycleRange`); the page's
  pagination is client-side.
- PDF: `GET https://www.verizon.com/digital/nsa/secure/gw/bill/billpaymenthistory/bill_pdfdoc?startMonthDate=<MM/DD/YYYY>&channelId=VZW-DOTCOM`,
  cookie-authenticated; must be fetched from a `verizon.com` page context.
  Files are named by the cycle end date.
- PDFs (Ricoh AFP2PDF) fail pdfcpu validation on link-annotation `QuadPoints`.
  `pdfx.keepPages` reads *without* validating, falling back to poppler
  `pdfseparate`/`pdfunite`.
- "Talk activity" always starts on a fresh page; the page before ends with
  taxes, so page-level trimming loses no charges.

## Trimming (`internal/pdfx`)

Text per page via `pdftotext -layout` (fallback: `ledongthuc/pdf`). A page is
call records if it has ≥3 phone numbers **and** ≥3 `h:mm AM/PM` times, or a
section heading (`Talk activity`, `Call details`, …) plus ≥2 times. The first
such page (never page 1) and everything after it is dropped; `-keep-partial`
keeps that first page. `bills trim x.pdf -explain` prints the verdicts.

## Expense cover sheets (`internal/expense`)

`bills expense` writes `bills/expense/<Vendor>_<auto-pay date>_<amount>.pdf`:
a generated cover page followed by the (trimmed) statement, unaltered. The
cover page exists because ExpenseIt (Concur's OCR) reads only the first two
pages and the last, and page 1 of a bill carries several dates (billing
date, service period, *last* month's payment, next auto-pay) and totals; a
receipt-shaped first page with one vendor / one date / one total is what we
want it to pick. Email carries no metadata (blank subject, body ignored).

- Claim profiles: vendor/service/parser are built in (`profiles` in
  `expense.go`); the expense type and amount come from `bills.json`
  (`expense.LoadConfig`, `Profile.Apply`, `Profile.Validate`), or
  `-amount`/`-type` for a single site. The amount claimed is fixed per site,
  not the bill total.
- Transaction date = the statement's automatic-payment date, and a statement
  is prepared only when that date is strictly before today (`-asof` to
  pretend). Auto-pay dates are in the future when a bill is issued, and
  Concur rejects future-dated expenses.
- Page-1 anchors (verified on 12 months of each, 2026-08-28):
  - Xfinity: header line `<billing date>  <from> to <to>`; `automatic
    payment on <date>` / `Credit card payment will be applied <date>`;
    `Amount due $x`; account `dddd dd ddd ddddddd`; service address on the
    `For <street>, CITY, ST, ZIP` line.
  - Verizon: `Bill date <Month d, yyyy>`; `Billing period: Mon d - Mon d,
    yyyy` (year rolls back when the range wraps); `Auto Pay scheduled for
    <date>` (fallback `Deducted from bank account on mm/dd/yy`); `Total due
    on Mon d $x`; `Account: nnn-nnnnn`. City/state is the `CITY, ST ZIP`
    line under a street line (the vendor's PO-box blocks are skipped).
- The cover PDF is hand-written (`cover.go`): one page, Helvetica /
  Helvetica-Bold (standard fonts, nothing embedded), ASCII only. Keep it
  that way — it must validate in pdfcpu and OCR cleanly.
- `pdfx.Prepend` merges with pdfcpu `MergeRaw` (relaxed validation) and
  falls back to poppler `pdfunite`, same pattern as trimming.
- Outputs are idempotent (skipped when present; `-force` regenerates). Files
  stay well under Concur's 5 MB attachment limit (~0.4 MB).

## Debugging a portal change

```sh
./bin/bills session verizon                  # sign in once; browser stays up
./bin/bills inspect verizon -attach -bodies bill_history   # page + API bodies → .inspect/
./bin/bills js -attach verizon 'document.title'            # or a .js file
./bin/bills fetch verizon -attach -months 2  # iterate until it works
```

`inspect`'s `network.json` shows which API the page called; `-bodies` saves
matching response bodies. Don't ask the user to re-login per iteration —
that's what `session` is for. Delete `.inspect/` when done (personal data).
