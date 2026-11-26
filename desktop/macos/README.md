# PGBot for macOS

A native SwiftUI app on top of the pgbot engine: one chat-first bot per
PostgreSQL database, with proactive reports, metrics and logs.

The app contains **no diagnostics**. Every score, finding, metric and log
line comes from the bundled `pgbot` binary's machine contracts
(`inspect --json`, `logs --json`, `activity --json`, `ask`, `sql explain`,
`sql run`, `why`, `diff`, `init`). The Swift side does presentation, scheduling,
persistence, and security plumbing only.

## Build and run

Requirements: macOS 14+, Swift 5.10 (Command Line Tools are enough), Go (to build the engine).

```sh
desktop/macos/scripts/build-app.sh            # → desktop/macos/build/PGBot.app (signed ad hoc)
open desktop/macos/build/PGBot.app
```

* The engine is built from this repo for arm64 and amd64 and merged with `lipo`.
  The Swift binary is universal when full Xcode is present. With Command Line
  Tools it is host-architecture only.
* `PGBOT_AUTH_BROKER_URL=https://… build-app.sh` enables Google/GitHub sign-in
  (see Authentication). Without it, the app offers "Continue without an account".
* `CODESIGN_IDENTITY="Developer ID Application: …"` signs for distribution
  (notarization is not scripted yet).

### Tests

```sh
cd desktop/macos
swift run PGBotKitChecks                                   # 49 unit checks
PGBOT_TEST_DSN='postgresql://pgbot_ro:…@127.0.0.1:5432/postgres?sslmode=disable' \
PGBOT_ENGINE_PATH=../../bin/pgbot swift run PGBotKitChecks # + 8 live-engine checks
```

Command Line Tools ship without XCTest, so the suite is an executable runner
that exits non-zero on failure. It covers:

* the health-score formula
* JSON contract decoding
* connection-string parsing
* redaction
* the Keychain
* finding diff and notification dedup
* scheduler backoff and sleep/wake
* SQLite persistence, including a check that no password reaches the store file
* PKCE and the auth lifecycle
* engine-runner security: argv, environment isolation, stderr scrubbing, timeout, cancellation, concurrency limit
* PSQL mode: meta-command translation, pattern quoting, refused commands, the
  `sql run --json` contract
* a live run against a real database, including PSQL mode's read-only guarantees

The engine side of PSQL mode is covered by `go test ./cmd/pgbot/ -run 'SQL|Refuse'`
(plus `TestIntegration_sqlRun` with `PGBOT_TEST_SUPERUSER_DSN`).
`.github/workflows/desktop-macos.yml` runs all of it on macOS CI and builds the
bundle. UI tests are not written yet; they need Xcode.

### Launch scenarios (screenshots, UI automation)

```sh
open PGBot.app --args -PGBotScenario demo [-PGBotRoute metrics|logs] [-PGBotSelect 2] [-PGBotAppearance dark]
PGBOT_DATA_DIR=/tmp/pgbot-ui PGBOT_SCENARIO_DSN='postgresql://…' PGBot.app/Contents/MacOS/PGBot -PGBotScenario local
```

Add `-PGBotSettings '{accent = pink; textSize = large; bubbleStyle = plain; density = compact;}'` to
overlay any setting, and `-PGBotOpenSettings YES -settingsTab appearance` to open Settings on a tab.
Scenarios use an in-memory session and never persist settings or the demo flag.
`PGBOT_DATA_DIR` isolates the store and uses a separate Keychain service.

## Architecture

```
SwiftUI views (Sources/PGBot)
  └─ AppModel             state, routing, conversations, notifications
      ├─ InspectionScheduler   per-bot intervals, backoff, sleep/wake, global pause
      ├─ PGBotEngine           typed calls → EngineRunner → bundled pgbot binary
      ├─ Store                 SQLite (bots, messages, inspections, metric samples, notifications)
      ├─ KeychainStore         DB passwords, AI keys, auth session
      └─ AuthService           OAuth 2.0 + PKCE via a broker
PGBotKit (Sources/PGBotKit)    all of the above except the views; no SwiftUI, fully checkable
```

| Need | Reused from the engine | New in the app |
|---|---|---|
| Health, findings, metrics | `inspect --json` (Context schema 1.x) | Swift `Codable` mirror; additive keys ignored |
| Health score | `render.HealthScore` formula | Ported 1:1 (see below), plus a completeness gate |
| What changed | Baseline store, `monitor.Compare` semantics | Same id+object diff in Swift; announcement ledger |
| AI answers | `pgbot ask` (grounded on findings, safety footer) | Provider picker, Keychain keys, consent sheet |
| Explain SQL | `sql explain` (plan-only, read-only txn) | SQL over stdin, rendered plan |
| PSQL mode | `sql run --json` (one READ ONLY txn, always rolled back) | psql meta-commands as catalog queries, result tables |
| Logs | `logs --json` (literals scrubbed) | Filters, search, "Explain with PGBot" (re-redacted) |
| Sessions | `activity --json` | Table message |
| Read-only role | `init --role … --database …` | Setup-SQL sheet in Add Database |

### Security invariants

These are enforced in `EngineRunner` and asserted by the checks.

* **No credentials in argv.** The DSN goes in `PGBOT_DATABASE_URL`, AI keys in
  `PGBOT_AI_API_KEY`, and SQL over stdin. `ps` shows flags only.
* **Allow-listed child environment.** The engine does not inherit the app's
  environment, so a stray `DATABASE_URL`/`OPENAI_API_KEY` can't redirect a run
  or leak a key. `XDG_STATE_HOME` points at the app's own baseline store,
  separate from the CLI's.
* **Redacted stderr.** Engine stderr is redacted (DSN passwords, `key=value`
  secrets, provider keys, bearer tokens, e-mails, plus the exact password)
  before it reaches the UI or a message.
* **Bounded runs.** Every run has a timeout and SIGTERM→SIGKILL cancellation,
  under a global concurrency limit of 3.
* **Secrets only in the Keychain.** Passwords and keys are stored as
  `…AfterFirstUnlockThisDeviceOnly` items: never synced, and readable for
  background monitoring once the user has logged in. The SQLite file is 0600
  and has no password field.
* **Read-only.** Nothing is executed beyond the engine's read-only
  inspections and PSQL mode's `sql run`, which runs in a READ ONLY transaction
  that is always rolled back and refuses transaction control, COPY, dblink and
  server actions before sending anything. Destructive remediation (DROP/ALTER/…) is shown as text for the
  user to review, and the engine's `safety.blocking_caveats` render as red
  guards on the finding.
* **Explicit AI consent.** Asking an external AI provider requires consent
  per provider and endpoint. Local endpoints (Ollama) skip it because nothing
  leaves the Mac.

### Health score

The engine computes `100 − 10·critical − 3·warn − 1·info` (suppressed findings
skipped, floor 0) in `internal/render/dashboard.go`, but does not emit it in
`--json`. The app mirrors that formula in `HealthScore.engineScore`, pinned by
checks. The live check compares it with the CLI's own
`Database health: N/100`.

The app also refuses a confident grade when Health and Activity are both
unavailable (shown as "—"), and marks the grade "Incomplete" when any core
section is unavailable or `pg_monitor` is missing.

**Suggested engine follow-up:** emit `health_score` in the Context JSON
(additive, schema 1.7.0) so there is a single source of truth.

### Monitoring and notifications

* Each bot has its own interval and daily-report hour. Failures back off from
  1 min, doubling, capped at the bot's own interval.
* On sleep the scheduler suspends. On wake, each overdue bot runs once.
* The UI states plainly that monitoring only covers time while the Mac is
  awake and PGBot is open (or in the menu bar).
* Notifications come from the id+object diff and are deduplicated by a
  per-bot ledger. An unchanged finding is never re-announced, even across
  failed runs or restarts. An outage is announced once, and so is the
  recovery. Mute, quiet hours, severity threshold, and per-category toggles
  are respected for system banners. The in-app center always records.

### Authentication

`AuthService` implements the authorization-code flow with PKCE (S256), state
validation (constant-time), token refresh, and sign-out. The browser leg uses
`ASWebAuthenticationSession`. Because the app must not hold Google/GitHub
client secrets, token exchange goes through a **PGBot auth broker**, which
does not exist yet. It must expose:

* `GET  {broker}/oauth/{google|github}/authorize?response_type=code&client_id=pgbot-desktop&redirect_uri=pgbot://auth/callback&code_challenge=…&code_challenge_method=S256&state=…`
* `POST {broker}/oauth/token` (form), with `grant_type=authorization_code|refresh_token`. Returns
  `{access_token, refresh_token, expires_in, user:{id,name,email,avatar_url}, plan}`

Until the broker is configured, sign-in buttons are disabled and the user can
continue with a local-only profile.

## Not done yet

* **Auth broker service.** Required for real Google/GitHub sign-in (see above).
* **Universal Swift binary and notarization.** Both need full Xcode.
* **XCUITest suite.** Launch scenarios are in place for it.
* **Streaming AI responses.** `pgbot ask` returns the whole answer; the UI
  shows elapsed time and Stop.
* **Provider infrastructure metrics** (CloudWatch, Cloud SQL, Azure Monitor)
  and **provider log APIs** (Supabase, Neon). The UI says these are
  unavailable rather than estimating them.
* **Pro / cloud features.** Cloud monitoring, Slack/email/webhooks, teams,
  billing and sync are modelled (`Subscription`, `ProFeature`) but not built.
* **Ignore rules.** App-level "Ignore" is local to the app and does not write
  the engine's `[[ignore]]` rules.
* **Per-bot AI provider overrides.** There is one global provider.
