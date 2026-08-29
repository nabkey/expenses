# bills

Downloads Xfinity and Verizon statements as PDFs for expense filing and keeps
the last 12 months cached under `bills/`. Verizon PDFs are cut off where the
per-call "Talk activity" records begin.

Go + [chromedp](https://github.com/chromedp/chromedp) driving the installed
Google Chrome on its own persistent profile (`.chrome-profile/`): sign in once
in a real window (password + MFA), later runs go headless.

## Setup

```sh
brew install poppler            # pdftotext/pdfseparate/pdfunite (optional but recommended)
go build -o bin/bills ./cmd/bills
cp bills.example.json bills.json   # then edit: expense type + amount claimed per site
./bin/bills login               # Chrome opens: sign in to Xfinity, then Verizon
```

## Monthly

```sh
./bin/bills fetch      # download anything new
./bin/bills expense    # cover sheet + statement for every bill whose auto-pay has gone through
```

```
bills/xfinity/<billing date>.pdf
bills/verizon/<cycle end date>.pdf                  trimmed
bills/verizon/raw/<cycle end date>.full.pdf         original
bills/expense/<Vendor>_<auto-pay date>_<amount>.pdf
```

Already-cached statements are skipped. Xfinity stays signed in on the profile.
Verizon only issues session cookies, so a cookie snapshot is saved on exit and
restored on launch; when Verizon's server session finally expires, `fetch`
opens a Chrome window, waits for you to sign in, and carries on.

`expense` prepends a one-page cover sheet to each statement so an expense
system (SAP Concur ExpenseIt) reads the right values instead of guessing
among the bill's dates and totals: vendor, transaction date (the statement's
automatic-payment date), the amount claimed, the Concur expense type, payment
method, location and a description, with the statement's own date/total/account
listed underneath. The expense type and amount claimed per site come from
`bills.json` (gitignored; start from `bills.example.json`), or `-amount` /
`-type` for a single site. A statement is only prepared once its auto-pay date has passed;
until then it is listed as waiting. Existing outputs are skipped unless
`-force`. Email each PDF, blank subject, from your Concur-verified address to
`receipts@expenseit.com`.

## Commands

| command | what it does | flags |
|---|---|---|
| `login [site]` | headed Chrome, wait for sign-in, save session | `-timeout 15m` |
| `fetch [site]` | headless download of anything missing | `-months 12` `-force` `-headed` `-keep-partial` `-login-timeout 15m` `-attach` |
| `trim in.pdf` | cut a Verizon PDF at the call records | `-o out.pdf` `-explain` `-keep-partial` |
| `expense [site]` | cover sheet + statement → `bills/expense/` for every bill whose auto-pay date has passed | `-asof YYYY-MM-DD` `-force` `-amount` `-type` `-out dir` |
| `session [site]` | like `login`, but keeps Chrome open with DevTools on `:9222` | `-port` `-timeout` |
| `inspect <site\|url>` | dump screenshot / html / text / links / network to `.inspect/` | `-attach` `-wait 6s` `-bodies <substr>` `-out dir` |
| `js <site\|url\|-> <expr\|file>` | evaluate JS on a page, print JSON | `-attach` `-wait 5s` |

`site` is `xfinity`, `verizon`, or `all` (default). `-attach` makes a command
use the browser started by `session` (in its own tab) instead of launching one.
All commands take `-root` (default: cwd; or `BILLS_HOME`). `BILLS_CHROME`
overrides the Chrome binary, `BILLS_DEBUG=1` shows chromedp protocol errors.

Nothing personal is in the repo: sessions live in `.chrome-profile/`, bills
in `bills/`, claim settings in `bills.json` — all gitignored.

## Layout

```
cmd/bills/           CLI
internal/browser/    chromedp wrapper: profile, attach, cookie snapshot, downloads, network log
internal/site/       per-portal login detection, statement listing, PDF download
internal/pdfx/       call-record detection + page trimming
internal/expense/    statement parsing (dates, totals), cover-sheet PDF, claim profiles
```

See `AGENTS.md` for how each portal actually works and how to debug it.
