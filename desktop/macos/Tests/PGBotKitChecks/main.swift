// PGBotKit check suite.
//
// Command Line Tools ship without XCTest, so this is a self-contained runner:
//   swift run PGBotKitChecks                     # unit checks
//   PGBOT_TEST_DSN=postgres://… PGBOT_ENGINE_PATH=../../bin/pgbot swift run PGBotKitChecks
//                                                # + live engine integration
// Exit status is non-zero if any check fails.
import Foundation
import PGBotKit

nonisolated(unsafe) var failures: [String] = []
nonisolated(unsafe) var passed = 0
nonisolated(unsafe) var current = ""

func expect(_ cond: @autoclosure () -> Bool, _ msg: String = "", file: StaticString = #fileID, line: UInt = #line) {
    if !cond() { failures.append("\(current): \(msg) (\(file):\(line))") }
}
func expectEq<T: Equatable>(_ a: T, _ b: T, _ msg: String = "", file: StaticString = #fileID, line: UInt = #line) {
    if a != b { failures.append("\(current): \(msg) — got \(a), want \(b) (\(file):\(line))") }
}
func check(_ name: String, _ body: () throws -> Void) {
    current = name; let before = failures.count
    do { try body() } catch { failures.append("\(name): threw \(error)") }
    if failures.count == before { passed += 1; print("  ✓ \(name)") } else { print("  ✗ \(name)") }
}
func checkAsync(_ name: String, _ body: () async throws -> Void) async {
    current = name; let before = failures.count
    do { try await body() } catch { failures.append("\(name): threw \(error)") }
    if failures.count == before { passed += 1; print("  ✓ \(name)") } else { print("  ✗ \(name)") }
}

func finding(_ id: String, _ sev: Severity, object: String? = nil, title: String? = nil, suppressed: Bool = false) -> Finding {
    Finding(id: id, object: object, severity: sev, title: title ?? id, detail: "", suppressed: suppressed)
}

let scratch = FileManager.default.temporaryDirectory.appendingPathComponent("pgbot-checks-\(UUID().uuidString)")
try FileManager.default.createDirectory(at: scratch, withIntermediateDirectories: true)
defer { try? FileManager.default.removeItem(at: scratch) }

// MARK: - Health score

print("Health score")
check("engine formula: 10 per critical, 3 per warning, 1 per info") {
    expectEq(HealthScore.engineScore([]), 100)
    expectEq(HealthScore.engineScore([finding("a", .critical), finding("b", .warn), finding("c", .info)]), 86)
    expectEq(HealthScore.engineScore([finding("a", .warn), finding("b", .warn)]), 94)
}
check("suppressed findings don't count; floor at zero") {
    expectEq(HealthScore.engineScore([finding("a", .critical, suppressed: true)]), 100)
    expectEq(HealthScore.engineScore((0..<15).map { finding("c\($0)", .critical) }), 0)
}
check("demo hero bot scores 94 from two real-shaped warnings") {
    let d = DemoData.make()
    expectEq(d.records[0].score.value, 94)
    expectEq(d.records[0].score.warnings, 2)
    expectEq(d.records[0].score.critical, 0)
    expect(d.bots.allSatisfy(\.isDemo), "every demo bot must be flagged isDemo")
}
check("unavailable sections make the grade partial or unavailable, never silently healthy") {
    let base = String(decoding: DemoData.contextJSONForChecks(), as: UTF8.self)
    let partial = base.replacingOccurrences(of: #""queries":{"exactness":"cumulative""#, with: #""queries":{"exactness":"unavailable","reason":"pg_stat_statements is not installed""#)
    let p = HealthScore.from(try EngineJSON.decodeContext(Data(partial.utf8)))
    expectEq(p.completeness, .partial)
    expect(p.gaps.contains { $0.contains("pg_stat_statements") }, "gap carries the engine's reason")
    let none = partial
        .replacingOccurrences(of: #""health":{"exactness":"sampled""#, with: #""health":{"exactness":"unavailable""#)
        .replacingOccurrences(of: #""activity":{"exactness":"scraped""#, with: #""activity":{"exactness":"unavailable""#)
    let u = HealthScore.from(try EngineJSON.decodeContext(Data(none.utf8)))
    expectEq(u.completeness, .unavailable)
    expectEq(HealthBands().band(for: u), .gray)
}
check("bands: 90+ green, 70+ yellow, 50+ orange, else red") {
    func s(_ v: Int) -> HealthScore { HealthScore(value: v, critical: 0, warnings: 0, info: 0, completeness: .complete, gaps: []) }
    let b = HealthBands()
    expectEq(b.band(for: s(94)), .green); expectEq(b.band(for: s(70)), .yellow)
    expectEq(b.band(for: s(50)), .orange); expectEq(b.band(for: s(49)), .red)
}

// MARK: - JSON contract

print("Engine JSON")
check("decodes Go RFC 3339 timestamps with nanoseconds") {
    let json = String(decoding: DemoData.contextJSONForChecks(), as: UTF8.self)
        .replacing(#/"collected_at":"[^"]+"/#, with: #""collected_at":"2026-11-25T09:14:03.123456789+01:00""#)
    let c = try EngineJSON.decodeContext(Data(json.utf8))
    expectEq(Int(c.collectedAt.timeIntervalSince1970), 1_795_594_443) // 2026-11-25T08:14:03Z
}
check("tolerates unknown keys and null findings (additive contract)") {
    let json = #"{"schema_version":"1.9.0","collected_at":"2026-01-01T00:00:00Z","fingerprint":"x","server":{"version_num":170002,"version_text":"","database":"d","future_field":1},"findings":null,"brand_new_section":{"a":1}}"#
    let c = try EngineJSON.decodeContext(Data(json.utf8))
    expectEq(c.findings.count, 0); expectEq(c.server.displayVersion, "PostgreSQL 17.2"); expectEq(c.schemaMajor, 1)
}
check("newline-delimited logs skip non-JSON lines") {
    let data = Data("""
    {"time":"2026-01-01T00:00:00Z","severity":"ERROR","level":"error","message":"deadlock detected"}
    not json
    {"time":"2026-01-01T00:00:01.5Z","severity":"LOG","level":"info","message":"checkpoint"}
    """.utf8)
    let rows = EngineJSON.decodeLines(LogEntry.self, from: data)
    expectEq(rows.count, 2); expectEq(rows[0].message, "deadlock detected")
}

// MARK: - Connection strings & redaction

print("Security")
check("parses a pasted URL; password goes out of band, never into settings") {
    let p = try ConnectionString.parse("postgresql://app%40corp:s3cr%3At%2F@db.example.com:6543/shop?sslmode=require&application_name=x")
    expectEq(p.connection.host, "db.example.com"); expectEq(p.connection.port, 6543)
    expectEq(p.connection.username, "app@corp"); expectEq(p.connection.database, "shop")
    expectEq(p.connection.sslMode, .require); expectEq(p.password, "s3cr:t/")
    expectEq(p.connection.extraParameters["application_name"], "x")
    let encoded = String(decoding: try JSONEncoder().encode(p.connection), as: UTF8.self)
    expect(!encoded.contains("s3cr"), "settings JSON must not contain the password")
}
check("password in the query string is treated as a secret too") {
    let p = try ConnectionString.parse("postgres://u@h/db?password=hunter22")
    expectEq(p.password, "hunter22"); expect(p.connection.extraParameters["password"] == nil)
}
check("build round-trips awkward passwords") {
    let c = DatabaseConnection(host: "h", port: 5432, database: "my db", username: "u:x", sslMode: .verifyFull)
    let pw = "p@ss:w/rd?#%&="
    let back = try ConnectionString.parse(ConnectionString.build(c, password: pw))
    expectEq(back.password, pw); expectEq(back.connection.username, "u:x"); expectEq(back.connection.database, "my db")
    expectEq(back.connection.sslMode, .verifyFull)
}
check("rejects non-Postgres URLs with a helpful message") {
    do { _ = try ConnectionString.parse("mysql://h/db"); expect(false, "should throw") }
    catch let e as ConnectionString.ParseError { expectEq(e, .unsupportedScheme("mysql")) }
}
check("redacts DSN passwords, key=value secrets, provider keys, bearer tokens, emails") {
    let s = Redactor.redact("""
    dial postgresql://admin:Sup3rS3cret@db:5432/x failed; password=abc123 api_key='zzz' \
    sk-ant-REDACTED Bearer eyJhbGciOiJIUzI1NiJ9.e30 user bob@example.com
    """)
    for leak in ["Sup3rS3cret", "abc123", "zzz", "sk-ant-api03", "eyJhbGci", "bob@example.com"] {
        expect(!s.contains(leak), "leaked \(leak) in: \(s)")
    }
    expect(s.contains("admin:•••@db"), "keeps the user for context")
}
check("SQL literal redaction for AI-bound logs") {
    let s = Redactor.redactSQLLiterals("SELECT * FROM users WHERE email = 'a@b.co' AND id = 42 AND t2.col = 'it''s'")
    expect(!s.contains("a@b.co") && !s.contains("42") && !s.contains("it''s"), s)
    expect(s.contains("t2.col"), "identifiers with digits survive: \(s)")
}
check("scrub removes an exact secret even if the patterns miss it") {
    expectEq(Redactor.scrub("weird token tok3n-xyz here", secrets: ["tok3n-xyz"]), "weird token ••• here")
}
check("engine error classification gives actionable categories") {
    func p(_ s: String) -> EngineError.ConnectionProblem? { if case .connection(let p, _) = EngineError.classify(stderr: s) { return p }; return nil }
    expectEq(p("pgbot: failed to connect: FATAL: password authentication failed for user \"x\""), .authentication)
    expectEq(p("dial tcp 10.0.0.1:5432: connect: connection refused"), .unreachable)
    expectEq(p("tls: failed to verify certificate: x509: unknown authority"), .ssl)
    expectEq(p("ERROR: permission denied for function pg_ls_waldir"), .permission)
    expectEq(p(#"FATAL: database "nope" does not exist"#), .databaseMissing)
    expectEq(p("context deadline exceeded"), .timeout)
    // An unknown role is a credentials problem, not an unreachable server, even
    // though the message starts with "connect:".
    expectEq(p(#"pgbot: connect: connect to 127.0.0.1:5432: FATAL: role "ghost" does not exist (SQLSTATE 28000)"#), .authentication)
    expectEq(p("connect: FATAL: password authentication failed for user \"x\" (SQLSTATE 28P01)"), .authentication)
    expectEq(p(#"connect: FATAL: database "nope" does not exist (SQLSTATE 3D000)"#), .databaseMissing)
}

// MARK: - Keychain

print("Keychain")
check("in-memory store set/get/delete") {
    let s = InMemorySecretStore()
    try s.set("a", for: "db:1"); expectEq(try s.get("db:1"), "a")
    try s.set("b", for: "db:1"); expectEq(try s.get("db:1"), "b")
    try s.delete("db:1"); expectEq(try s.get("db:1"), nil)
}
if ProcessInfo.processInfo.environment["PGBOT_SKIP_KEYCHAIN"] == nil {
    check("macOS Keychain round-trip (isolated service, cleaned up)") {
        let k = KeychainStore(service: "dev.pgbot.desktop.checks.\(UUID().uuidString)")
        defer { try? k.delete("db:check") }
        try k.set("pässwörd", for: "db:check"); expectEq(try k.get("db:check"), "pässwörd")
        try k.set("rotated", for: "db:check"); expectEq(try k.get("db:check"), "rotated")
        try k.delete("db:check"); expectEq(try k.get("db:check"), nil)
        try k.delete("db:check") // idempotent
    }
}

// MARK: - Finding diff & notifications

print("Monitoring")
check("compare: new / escalated / de-escalated / resolved / unchanged by id+object") {
    let prev = [finding("a", .warn, object: "t1"), finding("b", .critical), finding("c", .warn), finding("d", .info)]
    let cur = [finding("a", .critical, object: "t1"), finding("b", .warn), finding("c", .warn, title: "renamed"), finding("e", .warn)]
    let ch = FindingChanges.compare(previous: prev, current: cur)
    expectEq(ch.escalated.map(\.id), ["a"]); expectEq(ch.deescalated.map(\.id), ["b"])
    expectEq(ch.unchanged.map(\.id), ["c"], "a title change is not a new finding")
    expectEq(ch.new.map(\.id), ["e"]); expectEq(ch.resolved.map(\.id), ["d"])
}
check("same id on a different object is a different finding") {
    let ch = FindingChanges.compare(previous: [finding("bloat", .warn, object: "t1")], current: [finding("bloat", .warn, object: "t2")])
    expectEq(ch.new.count, 1); expectEq(ch.resolved.count, 1)
}
check("suppressed and user-ignored findings are invisible to the diff") {
    let ch = FindingChanges.compare(previous: [], current: [finding("a", .critical, suppressed: true), finding("b", .warn)], ignoring: ["b"])
    expect(ch.isEmpty)
}
check("notification dedup: an unchanged finding is announced once across runs") {
    var ledger = AnnouncementLedger()
    let s = HealthScore(value: 97, critical: 0, warnings: 1, info: 0, completeness: .complete, gaps: [])
    let f = finding("x", .warn)
    let n1 = NotificationPlanner.plan(botName: "B", changes: FindingChanges.compare(previous: [], current: [f]),
                                      scoreBefore: nil, scoreAfter: s, ledger: &ledger, prefs: .init())
    expectEq(n1.count, 1)
    // A failed run in between loses `previous`: diff says "new" again, ledger says "already told them".
    let n2 = NotificationPlanner.plan(botName: "B", changes: FindingChanges.compare(previous: [], current: [f]),
                                      scoreBefore: nil, scoreAfter: s, ledger: &ledger, prefs: .init())
    expectEq(n2.count, 0)
    // Escalation is news.
    let n3 = NotificationPlanner.plan(botName: "B", changes: FindingChanges.compare(previous: [f], current: [finding("x", .critical)]),
                                      scoreBefore: 97, scoreAfter: s, ledger: &ledger, prefs: .init())
    expectEq(n3.map(\.category), [.criticalFinding])
    // Resolution of an announced finding is news, once.
    let n4 = NotificationPlanner.plan(botName: "B", changes: FindingChanges.compare(previous: [finding("x", .critical)], current: []),
                                      scoreBefore: 90, scoreAfter: s, ledger: &ledger, prefs: .init())
    expectEq(n4.map(\.category), [.resolved])
}
check("below-threshold findings and disabled categories stay quiet") {
    var ledger = AnnouncementLedger()
    let s = HealthScore(value: 99, critical: 0, warnings: 0, info: 1, completeness: .complete, gaps: [])
    let n = NotificationPlanner.plan(botName: "B", changes: FindingChanges.compare(previous: [], current: [finding("i", .info), finding("w", .warn)]),
                                     scoreBefore: nil, scoreAfter: s, ledger: &ledger, prefs: .init(minimum: .warn, disabled: [.warning]))
    expect(n.isEmpty, "\(n)")
}
check("connectivity outage announced once, recovery announced once") {
    var ledger = AnnouncementLedger()
    let err = EngineError.connection(.unreachable, detail: "refused")
    expect(NotificationPlanner.planFailure(botName: "B", error: err, ledger: &ledger, prefs: .init()) != nil)
    expect(NotificationPlanner.planFailure(botName: "B", error: err, ledger: &ledger, prefs: .init()) == nil)
    let s = HealthScore(value: 100, critical: 0, warnings: 0, info: 0, completeness: .complete, gaps: [])
    let n = NotificationPlanner.plan(botName: "B", changes: .init(), scoreBefore: nil, scoreAfter: s, ledger: &ledger, prefs: .init())
    expectEq(n.map(\.dedupKey), ["connectivity|up"])
}
check("quiet hours wrap midnight") {
    var q = QuietHours(); q.enabled = true; q.startHour = 22; q.endHour = 7
    let cal = Calendar.current, d = Date()
    expect(q.contains(cal.date(bySettingHour: 23, minute: 0, second: 0, of: d)!))
    expect(q.contains(cal.date(bySettingHour: 3, minute: 0, second: 0, of: d)!))
    expect(!q.contains(cal.date(bySettingHour: 12, minute: 0, second: 0, of: d)!))
}

// MARK: - Scheduler

final class Clock: @unchecked Sendable { var now = Date(timeIntervalSince1970: 1_000_000) }
final class Calls: @unchecked Sendable { var ids: [UUID] = []; var outcome: InspectionScheduler.Outcome = .success; let lock = NSLock() }

await checkAsync("scheduler: due order, pause, sleep/wake, per-bot intervals") {
    let clock = Clock(), calls = Calls()
    let s = InspectionScheduler(clock: { clock.now }) { id in calls.lock.withLock { calls.ids.append(id); return calls.outcome } }
    let a = UUID(), b = UUID(), c = UUID()
    await s.register(a, interval: 60, paused: false, firstRunAt: clock.now)
    await s.register(b, interval: 3600, paused: false, firstRunAt: clock.now.addingTimeInterval(-10))
    await s.register(c, interval: 60, paused: true, firstRunAt: clock.now)
    expectEq(await s.dueIDs(), [b, a], "oldest-due first, paused excluded")
    await s.runDue()
    expectEq(Set(calls.ids), [a, b])
    expectEq(await s.dueIDs(), [])
    clock.now += 61
    expectEq(await s.dueIDs(), [a], "a's 60s interval elapsed, b's hour hasn't")
    await s.willSleep(); expectEq(await s.dueIDs(), [], "nothing runs while asleep")
    await s.didWake(); expectEq(await s.dueIDs(), [a], "overdue work runs once on wake")
    await s.setGloballyPaused(true); expectEq(await s.dueIDs(), [])
}
await checkAsync("scheduler: failures back off exponentially, success resets") {
    let clock = Clock(), calls = Calls(); calls.outcome = .failure
    let s = InspectionScheduler(clock: { clock.now }) { _ in calls.outcome }
    let a = UUID()
    await s.register(a, interval: 3600, paused: false, firstRunAt: clock.now)
    await s.runDue()
    expectEq(await s.entry(a)!.nextDue.timeIntervalSince(clock.now), 60)
    clock.now += 60; await s.runDue()
    expectEq(await s.entry(a)!.nextDue.timeIntervalSince(clock.now), 120)
    expectEq(await s.entry(a)!.consecutiveFailures, 2)
    calls.outcome = .success; clock.now += 120; await s.runDue()
    expectEq(await s.entry(a)!.consecutiveFailures, 0)
    expectEq(await s.entry(a)!.nextDue.timeIntervalSince(clock.now), 3600)
    expectEq(InspectionScheduler.backoff(failures: 10, interval: 900, cap: 21600), 900, "never slower than the healthy interval")
}

// MARK: - Persistence

print("Persistence")
check("bots, conversations, inspections and metrics round-trip through SQLite") {
    let url = scratch.appendingPathComponent("store.sqlite")
    let demo = DemoData.make()
    do {
        let s = try Store(url: url)
        for b in demo.bots { try s.saveBot(b) }
        for m in demo.messages { try s.append(m) }
        for r in demo.records { try s.save(r) }
    }
    let s = try Store(url: url) // reopen: persistence, not memory
    expectEq(try s.bots().map(\.displayName), demo.bots.map(\.displayName))
    let hero = demo.bots[0].id
    let msgs = try s.messages(for: hero)
    expectEq(msgs.count, demo.messages.filter { $0.botID == hero }.count)
    expect(msgs.map(\.createdAt) == msgs.map(\.createdAt).sorted(), "chronological")
    let other = try s.messages(for: demo.bots[1].id)
    expect(other.allSatisfy { $0.botID == demo.bots[1].id }, "conversations are per-bot")
    expectEq(try s.latestInspection(for: hero)?.score.value, 94)
    let tps = try s.samples(for: hero, metric: .tps, since: .distantPast)
    expect(!tps.isEmpty, "inspection writes metric samples")
    try s.deleteBot(hero)
    expectEq(try s.messages(for: hero).count, 0)
    let gone = try s.latestInspection(for: hero)
    expect(gone == nil, "inspections removed with the bot")
    expectEq(try s.bots().count, demo.bots.count - 1)
}
check("no password ever reaches the SQLite file") {
    let url = scratch.appendingPathComponent("secret-check.sqlite")
    let secrets = InMemorySecretStore()
    let parsed = try ConnectionString.parse("postgresql://ro:Tr0ub4dor-UNIQUE@db.example.com/app")
    let bot = DatabaseBot(displayName: "x", connection: parsed.connection)
    try secrets.set(parsed.password!, for: bot.keychainAccount)
    do { let s = try Store(url: url); try s.saveBot(bot) }
    for f in [url.path, url.path + "-wal"] where FileManager.default.fileExists(atPath: f) {
        let bytes = try Data(contentsOf: URL(fileURLWithPath: f))
        expect(bytes.range(of: Data("Tr0ub4dor-UNIQUE".utf8)) == nil, "password found in \(f)")
    }
    let perms = (try FileManager.default.attributesOfItem(atPath: url.path)[.posixPermissions] as? Int) ?? 0
    expectEq(perms & 0o077, 0, "store is private to the user")
}

// MARK: - Auth

print("Authentication")
check("PKCE S256 = base64url(SHA-256(verifier)), no padding") {
    // Reference computed independently:
    //   printf %s <verifier> | openssl dgst -sha256 -binary | base64 | tr '+/' '-_' | tr -d '='
    expectEq(PKCE(verifier: "dBjftJeZ4CVP-mB92K9uhbUJU22SpH2AbDj4gfmTU2c").challenge, "T79ClVwiB0HLeuD28gSCyvDxozvQf-CecIwlBdoQMAE")
    let g = PKCE.generate(); expect(g.verifier.count >= 43 && g.verifier.count <= 128)
    expect(PKCE.generate().verifier != g.verifier)
}

struct FakeTransport: AuthTransport {
    let handler: @Sendable (URL, [String: String]) throws -> Data
    func post(_ url: URL, form: [String: String]) async throws -> Data { try handler(url, form) }
}
final class FormLog: @unchecked Sendable { var forms: [[String: String]] = [] }

await checkAsync("auth lifecycle: authorize → callback → token → restore → refresh → sign out") {
    let secrets = InMemorySecretStore(), log = FormLog()
    let cfg = AuthConfiguration(brokerURL: URL(string: "https://auth.pgbot.dev")!)
    let svc = AuthService(config: cfg, secrets: secrets, transport: FakeTransport { _, form in
        log.forms.append(form)
        let exp = form["grant_type"] == "refresh_token" ? 3600 : -1 // first token is already expired → forces refresh on restore
        return Data(#"{"access_token":"at-\#(log.forms.count)","refresh_token":"rt","expires_in":\#(exp),"user":{"id":"u1","name":"Ada","email":"ada@example.com"}}"#.utf8)
    })
    let req = try svc.makeRequest(for: .github)
    let q = URLComponents(url: req.url, resolvingAgainstBaseURL: false)!.queryItems!
    expectEq(q.first { $0.name == "code_challenge_method" }?.value, "S256")
    expectEq(q.first { $0.name == "code_challenge" }?.value, req.pkce.challenge)
    expect(!req.url.absoluteString.contains(req.pkce.verifier), "verifier never leaves the app in the browser leg")
    expect(!req.url.absoluteString.contains("secret"), "no client secret in the app")

    // Forged state is rejected.
    do { _ = try await svc.complete(req, callback: URL(string: "pgbot://auth/callback?code=c&state=evil")!); expect(false) }
    catch let e as AuthError { expectEq(e, .stateMismatch) }

    let session = try await svc.complete(req, callback: URL(string: "pgbot://auth/callback?code=c0de&state=\(req.state)")!)
    expectEq(session.user.name, "Ada"); expectEq(log.forms.last?["code_verifier"], req.pkce.verifier)
    let restored = try await svc.restore()
    expectEq(restored?.accessToken, "at-2", "expired session refreshed on restore")
    expectEq(log.forms.last?["grant_type"], "refresh_token")
    try svc.signOut()
    let afterSignOut = try await svc.restore()
    expect(afterSignOut == nil)
}
await checkAsync("auth: a rejected refresh signs the user out") {
    let secrets = InMemorySecretStore()
    let svc = AuthService(config: .init(brokerURL: URL(string: "https://a.example")!), secrets: secrets,
                          transport: FakeTransport { _, form in
                              if form["grant_type"] == "refresh_token" { throw AuthError.expired }
                              return Data(#"{"access_token":"a","refresh_token":"r","expires_in":-1}"#.utf8) })
    let req = try svc.makeRequest(for: .google)
    _ = try await svc.complete(req, callback: URL(string: "pgbot://auth/callback?code=x&state=\(req.state)")!)
    do { _ = try await svc.restore(); expect(false, "should throw expired") } catch let e as AuthError { expectEq(e, .expired) }
    expectEq(try secrets.get(SecretAccount.authSession), nil)
}
check("unconfigured builds refuse OAuth and never accept a plain-http broker") {
    expect(!AuthConfiguration(brokerURL: nil).isConfigured)
    let svc = AuthService(config: .init(brokerURL: nil), secrets: InMemorySecretStore())
    do { _ = try svc.makeRequest(for: .google); expect(false) } catch let e as AuthError { expectEq(e, .notConfigured) }
}

// MARK: - Engine runner (fake engine)

print("Engine adapter")
let fake = scratch.appendingPathComponent("fake-pgbot")
try """
#!/bin/sh
# Echo what the engine would see, so checks can assert on it.
case "$1" in
  sleep) sleep 30 ;;
  slowok) sleep 1; echo ok ;;
  fail) echo "pgbot: dial postgresql://u:$FAKE_SECRET@h/db failed: connection refused" >&2; exit 3 ;;
  *) printf 'ARGV=%s\\n' "$*"; env | sort ;;
esac
""".write(to: fake, atomically: true, encoding: .utf8)
try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: fake.path)

await checkAsync("credentials travel by environment only; parent secrets are not inherited") {
    setenv("DATABASE_URL", "postgres://leak:leak@evil/db", 1)
    setenv("OPENAI_API_KEY", "sk-parent-leak", 1)
    defer { unsetenv("DATABASE_URL"); unsetenv("OPENAI_API_KEY") }
    let runner = EngineRunner(binary: fake, stateDirectory: scratch.appendingPathComponent("state"))
    let target = EngineTarget(connection: DatabaseConnection(host: "db", username: "ro"), password: "pw-SECRET-1")
    let out = try await runner.run(.init(arguments: ["inspect", "--json"],
                                         environment: target.environmentForChecks, secrets: target.secretsForChecks))
    let text = String(decoding: out.stdout, as: UTF8.self)
    let argv = text.split(separator: "\n").first { $0.hasPrefix("ARGV=") } ?? ""
    expect(!argv.contains("pw-SECRET-1") && !argv.contains("postgres"), "argv: \(argv)")
    expect(text.contains("PGBOT_DATABASE_URL=postgresql://ro:pw-SECRET-1@db"), "DSN delivered via env")
    expect(!text.contains("sk-parent-leak") && !text.contains("leak@evil"), "parent env leaked into the engine")
    expect(text.contains("NO_COLOR=1") && text.contains("XDG_STATE_HOME="), "isolated, colorless engine env")
}
await checkAsync("stderr is redacted, including the exact password") {
    let runner = EngineRunner(binary: fake, stateDirectory: scratch)
    let out = try await runner.run(.init(arguments: ["fail"], environment: ["FAKE_SECRET": "zz-Unusual-77"], secrets: ["zz-Unusual-77"]))
    expectEq(out.exitCode, 3)
    expect(!out.stderr.contains("zz-Unusual-77"), out.stderr)
    if case .connection(.unreachable, _) = EngineError.classify(stderr: out.stderr) {} else { expect(false, "classified as unreachable") }
}
await checkAsync("timeouts kill the engine and surface a timeout error") {
    let runner = EngineRunner(binary: fake, stateDirectory: scratch)
    let t0 = Date()
    do { _ = try await runner.run(.init(arguments: ["sleep"], timeout: .milliseconds(400))); expect(false, "should time out") }
    catch let e as EngineError { if case .timedOut = e {} else { expect(false, "\(e)") } }
    expect(Date().timeIntervalSince(t0) < 5, "returned promptly")
}
await checkAsync("cancellation terminates the child") {
    let runner = EngineRunner(binary: fake, stateDirectory: scratch)
    let task = Task { try await runner.run(.init(arguments: ["sleep"], timeout: .seconds(60))) }
    try await Task.sleep(for: .milliseconds(300)); task.cancel()
    let t0 = Date()
    do { _ = try await task.value; expect(false, "should be cancelled") } catch is CancellationError {} catch { expect(false, "\(error)") }
    expect(Date().timeIntervalSince(t0) < 5)
}
await checkAsync("concurrency limit bounds simultaneous engine runs") {
    let runner = EngineRunner(binary: fake, stateDirectory: scratch, maxConcurrent: 2)
    let t0 = Date()
    try await withThrowingTaskGroup(of: Void.self) { g in
        for _ in 0..<4 { g.addTask { _ = try await runner.run(.init(arguments: ["slowok"])) } }
        try await g.waitForAll()
    }
    let elapsed = Date().timeIntervalSince(t0)
    expect(elapsed >= 1.9, "4 one-second runs at concurrency 2 take ≥2s, took \(elapsed)")
}
await checkAsync("missing engine is a clear, actionable error") {
    let runner = EngineRunner(binary: scratch.appendingPathComponent("nope"), stateDirectory: scratch)
    do { _ = try await runner.run(.init(arguments: [])); expect(false) }
    catch let e as EngineError { if case .engineMissing = e {} else { expect(false, "\(e)") }; expect(!e.hint.isEmpty) }
}

// MARK: - PSQL console

print("PSQL mode")
check("meta-commands translate to catalog queries") {
    if case .meta(let sql, let title) = PSQL.parse("\\dt") { expect(sql.contains("relkind IN ('r', 'p')")); expectEq(title, "List of tables") } else { expect(false) }
    if case .meta(let sql, _) = PSQL.parse("\\di+") { expect(sql.contains("pg_total_relation_size") && sql.contains("\"Table\"")) } else { expect(false) }
    if case .meta(let sql, let title) = PSQL.parse("\\d public.orders") {
        expect(sql.contains("n.nspname ~ '^(public)$'") && sql.contains("c.relname ~ '^(orders)$'")); expectEq(title, "Table public.orders")
    } else { expect(false) }
    expectEq(PSQL.parse("\\x"), .toggleExpanded(nil)); expectEq(PSQL.parse("\\x on"), .toggleExpanded(true))
    expectEq(PSQL.parse("\\?"), .help); expectEq(PSQL.parse("\\q"), .quit)
    expectEq(PSQL.parse("select 1;"), .sql("select 1;"))
}
check("psql patterns: wildcards, case folding, quoted names") {
    if case .meta(let sql, _) = PSQL.parse("\\dt Order*") { expect(sql.contains("'^(order.*)$'"), sql) } else { expect(false) }
    if case .meta(let sql, _) = PSQL.parse("\\dt \"Order\"") { expect(sql.contains("'^(Order)$'"), sql) } else { expect(false) }
}
check("hostile patterns can't break out of the SQL literal") {
    if case .meta(let sql, _) = PSQL.parse("\\dt x'; drop table t; --") {
        expect(sql.contains("'^(x''; drop table t; --)$'"), sql)
        expect(!sql.contains("x'; drop"), "quote must be doubled")
    } else { expect(false) }
}
check("shell, file and session meta-commands are refused") {
    for c in ["\\! rm -rf /", "\\o /tmp/out", "\\copy t to '/tmp/x'", "\\i evil.sql", "\\set AUTOCOMMIT off", "\\c otherdb"] {
        if case .unsupported = PSQL.parse(c) {} else { expect(false, "\(c) should be refused") }
    }
}
check("Return runs only complete statements (psql semantics)") {
    expect(PSQL.isComplete("select 1;"))
    expect(PSQL.isComplete("\\dt"))
    expect(!PSQL.isComplete("select 1"))
    expect(!PSQL.isComplete("select ';"))
    expect(PSQL.isComplete("select ';' ;"))
    expect(!PSQL.isComplete("do $$ begin perform 1; end"))
    expect(PSQL.isComplete("do $$ begin perform 1; end $$;"))
    expect(PSQL.isComplete("select 1; -- note"), "a comment after the semicolon doesn't hide it")
    expect(PSQL.isComplete("select 1 -- note\n;"))
}
check("psql results round-trip through the conversation store format") {
    let m = ChatMessage(botID: UUID(), role: .assistant, payload: .psqlResult(PSQLResultPayload(
        input: "select 1;", title: nil, output: SQLRunOutput(results: [], error: nil, elapsedMS: 1), expanded: false)))
    let data = try EngineJSON.encoder().encode(m)
    expectEq(try EngineJSON.decoder().decode(ChatMessage.self, from: data).payload, m.payload)
}

check("decodes the engine's sql run --json document (contract pinned by TestSQLRunJSONContract)") {
    // Shape emitted by cmd/pgbot/sqlrun.go; a future additive key must not break decoding.
    let json = #"""
    {"sql_run_schema_version":"1.0.0","read_only":true,"future_key":1,
     "results":[{"statement":"select id, note from t","command":"SELECT 2",
       "columns":[{"name":"id","type":"integer"},{"name":"note","type":"text"}],
       "rows":[["1",null],["2","b"]],"row_count":2,"truncated":true,"elapsed_ms":1.5}],
     "error":{"statement_index":1,"statement":"selec 2","message":"syntax error at or near \"selec\"","sqlstate":"42601","position":1},
     "elapsed_ms":3.25}
    """#
    let out = try EngineJSON.decoder().decode(SQLRunOutput.self, from: Data(json.utf8))
    expect(out.readOnly)
    expectEq(out.results.first?.rows, [["1", nil], ["2", "b"]])
    expectEq(out.results.first?.truncated, true)
    expectEq(out.error?.position, 1); expectEq(out.error?.statementIndex, 1)
    expect(out.error?.isRefusal == false)
    let ok = try EngineJSON.decoder().decode(SQLRunOutput.self, from: Data(#"{"read_only":true,"results":[],"elapsed_ms":0}"#.utf8))
    expect(ok.error == nil, "success omits error")
}
check("numeric columns right-align; text doesn't") {
    for t in ["integer", "bigint", "numeric(10,2)", "double precision", "NUMERIC"] { expect(SQLRunColumn(name: "x", type: t).isNumeric, t) }
    for t in ["text", "character varying", "timestamp with time zone", "integer[]", "uuid"] { expect(!SQLRunColumn(name: "x", type: t).isNumeric, t) }
}
check("psql result plain text is tab-separated with NULL as empty") {
    let p = PSQLResultPayload(input: "select a, b from t;", title: nil, output: SQLRunOutput(results: [
        SQLRunResult(statement: "", command: "SELECT 1", columns: [.init(name: "a", type: "int"), .init(name: "b", type: "text")],
                     rows: [["1", nil]], rowCount: 1, truncated: false, elapsedMS: 0),
        SQLRunResult(statement: "", command: "SET", columns: [], rows: [], rowCount: 0, truncated: false, elapsedMS: 0),
    ], error: SQLRunError(message: "boom"), elapsedMS: 0), expanded: false)
    expectEq(p.plainText, "select a, b from t;\na\tb\n1\t\nSET\nERROR: boom")
}
check("a meta-command lookup never reaches the engine as a backslash") {
    for c in ["\\dt", "\\d+ orders", "\\l", "\\dn", "\\di", "\\df", "\\du", "\\dx", "\\conninfo", "\\dv", "\\dm", "\\ds"] {
        guard case .meta(let sql, _) = PSQL.parse(c) else { expect(false, "\(c) should be a meta-command"); continue }
        expect(!sql.contains("\\"), "\(c) → SQL without a backslash")
        expect(sql.lowercased().hasPrefix("select") || sql.lowercased().hasPrefix("with"), "\(c) is a read-only query")
    }
}

// MARK: - Live engine integration

let env = ProcessInfo.processInfo.environment
if let dsn = env["PGBOT_TEST_DSN"], let enginePath = env["PGBOT_ENGINE_PATH"] {
    print("Live engine (\(enginePath))")
    // Resolve a relative PGBOT_ENGINE_PATH against the working directory up front.
    let engineURL = URL(fileURLWithPath: enginePath, relativeTo: URL(fileURLWithPath: FileManager.default.currentDirectoryPath)).standardizedFileURL
    let runner = EngineRunner(binary: engineURL, stateDirectory: scratch.appendingPathComponent("live-state"))
    let engine = PGBotEngine(runner: runner)
    let parsed = try ConnectionString.parse(dsn)
    let target = EngineTarget(connection: parsed.connection, password: parsed.password)

    await checkAsync("engine version is readable") {
        let v = try await engine.version(); expect(!v.isEmpty, v); print("    engine \(v)")
    }
    await checkAsync("real inspection decodes and scores like the CLI") {
        let ctx = try await engine.inspect(target)
        expectEq(ctx.schemaMajor, 1)
        expect(ctx.server.versionNum >= 100000, "server version")
        let score = HealthScore.from(ctx)
        print("    \(ctx.server.displayVersion) · \(ctx.findings.count) findings · score \(score.value) (\(score.completeness.rawValue))")
        // Cross-check against the engine's own rendering of the same number.
        let text = try await runner.run(.init(arguments: ["inspect", "--no-store"], environment: target.environmentForChecks, secrets: target.secretsForChecks))
        let rendered = String(decoding: text.stdout, as: UTF8.self)
        if let m = rendered.firstMatch(of: #/Database health:\s*(\d+)\/100/#) {
            let cliScore = Int(m.1)!
            // The two runs are seconds apart; the finding set can legitimately differ by a sampled finding.
            expect(abs(cliScore - score.value) <= 3, "CLI said \(cliScore), app computed \(score.value)")
        }
        let rec = InspectionRecord(botID: UUID(), startedAt: Date(), finishedAt: Date(), context: ctx)
        expect(!MetricKey.samples(from: rec).isEmpty, "metrics extracted from a real run")
    }
    await checkAsync("multiple simultaneous databases inspect independently") {
        async let a = engine.inspect(target)
        async let b = engine.inspect(target)
        let (x, y) = try await (a, b)
        expectEq(x.fingerprint, y.fingerprint)
    }
    await checkAsync("activity and setup SQL come back structured") {
        let sessions = try await engine.activity(target)
        expect(sessions.allSatisfy { $0.pid > 0 })
        let sql = try await engine.setupSQL(role: "pgbot_ro", database: parsed.connection.database, includeLogs: false)
        expect(sql.contains("pg_monitor"), "init recipe grants pg_monitor")
    }
    await checkAsync("bad credentials classify as an authentication problem without leaking the password") {
        var bad = parsed.connection; bad.username = "pgbot_no_such_user"
        do { _ = try await engine.inspect(EngineTarget(connection: bad, password: "WrongPass-123")); expect(false) }
        catch let e as EngineError {
            if case .connection(let p, let d) = e { expect(p == .authentication || p == .other, "\(p)"); expect(!d.contains("WrongPass-123")) }
            else { expect(false, "\(e)") }
        }
    }
    await checkAsync("PSQL mode: queries return typed rows, writes are blocked, COMMIT is refused") {
        let ok = try await engine.runSQL(target, sql: "select 1 as one, null::text as nothing; select 'b' as two;", maxRows: 10)
        expect(ok.error == nil, "\(String(describing: ok.error))")
        expectEq(ok.results.count, 2)
        expectEq(ok.results.first?.columns.map(\.type), ["integer", "text"])
        let row = ok.results.first?.rows.first ?? []
        expect(row.count == 2 && row[0] == "1" && row[1] == nil, "NULL arrives as nil, not \"\": \(row)")
        let write = try await engine.runSQL(target, sql: "create table pgbot_should_not_exist(a int);", maxRows: 10)
        expectEq(write.error?.sqlstate, "25006")
        let refused = try await engine.runSQL(target, sql: "select 1; commit;", maxRows: 10)
        expect(refused.error?.isRefusal == true && refused.results.isEmpty, "refused before anything ran")
    }
    await checkAsync("PSQL mode: \\dt and \\d run against the real catalog") {
        guard case .meta(let dt, _) = PSQL.parse("\\dt") else { expect(false); return }
        let tables = try await engine.runSQL(target, sql: dt, maxRows: 100)
        expect(tables.error == nil, "\(String(describing: tables.error))")
        guard case .meta(let d, _) = PSQL.parse("\\d pg_catalog.pg_class") else { expect(false); return }
        let described = try await engine.runSQL(target, sql: d, maxRows: 200)
        expect(described.error == nil && described.results.count == 2, "\(String(describing: described.error))")
        expect(described.results.first?.rows.contains { $0.first == "relname" } == true, "pg_class has a relname column")
    }
    await checkAsync("unreachable host classifies as unreachable") {
        var bad = parsed.connection; bad.host = "127.0.0.1"; bad.port = 1; bad.connectTimeoutSeconds = 2
        do { _ = try await engine.inspect(EngineTarget(connection: bad, password: nil)); expect(false) }
        catch let e as EngineError { if case .connection(.unreachable, _) = e {} else { expect(false, "\(e)") } }
    }
} else {
    print("Live engine: skipped (set PGBOT_TEST_DSN and PGBOT_ENGINE_PATH)")
}

// MARK: - Summary

print("")
if failures.isEmpty {
    print("All \(passed) checks passed.")
    exit(0)
} else {
    print("\(failures.count) failure(s), \(passed) passed:")
    failures.forEach { print("  • \($0)") }
    exit(1)
}
