import Foundation

/// Explicitly labeled sample data for screenshots and demos. Every demo bot has
/// `isDemo = true`, the UI badges it "Demo", and the engine is never invoked for
/// it. The contexts below are hand-written in the engine's own JSON shape so they
/// exercise exactly the same decoding and rendering paths as real inspections.
public enum DemoData {
    public struct Bundle {
        public var bots: [DatabaseBot]
        public var records: [InspectionRecord]
        public var messages: [ChatMessage]
        public var notifications: [AppNotification]
        public var samples: [MetricSample]
        public var logs: [UUID: [LogEntry]]
    }

    struct Spec {
        let name: String, color: BotColor, env: DatabaseEnvironment, host: String, db: String
        let pinned: Bool, findings: [(String, String, Severity, String, String, String?)]
        let conns: Int, size: Int64, tps: Double
    }

    static let specs: [Spec] = [
        Spec(name: "Remotely Production", color: .blue, env: .production, host: "db.remotely.example", db: "remotely",
             pinned: true, findings: [
                ("unused_indexes", "public.orders", .warn, "Three potentially unused indexes occupy 4.2 GB",
                 "orders_created_idx, orders_status_idx and orders_legacy_ref_idx have recorded 0 scans since statistics were reset 41 days ago.",
                 "Review their usage on every replica before considering removal."),
                ("query_regression", "q:-81233409", .warn, "A query on orders became 2.3× slower than the previous baseline",
                 "Mean execution time rose from 38 ms to 87 ms across 18.4k calls since yesterday's baseline.",
                 "Run “Explain SQL” on the query to see whether the plan changed."),
             ], conns: 42, size: 26_628_797_235, tps: 812),
        Spec(name: "PGBox", color: .green, env: .production, host: "pgbox.internal", db: "pgbox",
             pinned: true, findings: [], conns: 18, size: 3_400_000_000, tps: 120),
        Spec(name: "Ekomspace", color: .orange, env: .production, host: "ekom.example.net", db: "ekom",
             pinned: true, findings: [
                ("autovacuum_behind", "public.events", .warn, "Autovacuum is behind on public.events",
                 "public.events has 31% dead tuples (4.1M) and was last vacuumed 3 days ago.",
                 "Lower autovacuum_vacuum_scale_factor for this table."),
             ], conns: 63, size: 9_900_000_000, tps: 340),
        Spec(name: "Local Postgres", color: .black, env: .local, host: "localhost", db: "app_dev",
             pinned: false, findings: [], conns: 4, size: 180_000_000, tps: 3),
        Spec(name: "Staging", color: .black, env: .staging, host: "staging-db.example.net", db: "app",
             pinned: false, findings: [], conns: 9, size: 2_100_000_000, tps: 14),
    ]

    public static func make(now: Date = Date()) -> Bundle {
        var bots: [DatabaseBot] = [], records: [InspectionRecord] = [], messages: [ChatMessage] = []
        var notes: [AppNotification] = [], samples: [MetricSample] = [], logs: [UUID: [LogEntry]] = [:]
        for (i, s) in specs.enumerated() {
            let id = UUID(uuidString: String(format: "D3E00000-0000-4000-8000-%012d", i + 1))!
            let ctx = try! EngineJSON.decodeContext(contextJSON(s, at: now))
            let rec = InspectionRecord(botID: id, startedAt: now.addingTimeInterval(-8), finishedAt: now, context: ctx)
            var bot = DatabaseBot(id: id, displayName: s.name, description: "Sample database (demo)",
                                  avatar: BotAvatar(color: s.color, shape: i == 1 ? .roundedSquare : .circle), environment: s.env,
                                  connection: DatabaseConnection(host: s.host, database: s.db, username: "pgbot_ro", sslMode: .require),
                                  pinned: s.pinned, sortOrder: i, isDemo: true,
                                  status: s.findings.isEmpty ? .healthy : .degraded, lastInspectedAt: now.addingTimeInterval(-300))
            bot.lastScore = rec.score.value
            bot.lastSummary = i == 0 ? "2 new findings · Query regression"
                : (s.findings.isEmpty ? "Healthy · " + (s.env == .local ? "Development" : "No new findings")
                   : "1 warning · Autovacuum is behind")
            bot.unreadCount = i == 0 ? 2 : (s.findings.isEmpty ? 0 : 1)
            bots.append(bot); records.append(rec)
            samples += history(botID: id, base: s, score: rec.score.value, now: now)
            logs[id] = demoLogs(now: now)
        }

        // A day in the life of the hero bot.
        let hero = bots[0], cal = Calendar.current
        func today(_ h: Int, _ m: Int) -> Date { cal.date(bySettingHour: h, minute: m, second: 0, of: now) ?? now }
        let r0 = records[0]
        messages.append(ReportComposer.healthReport(bot: hero, record: r0, previous: nil, firstRun: true, scheduled: false)
            .with(createdAt: today(8, 2)))
        var morning = ReportComposer.healthReport(bot: hero, record: r0, previous: nil, firstRun: false, scheduled: true)
        morning.createdAt = today(9, 0)
        messages.append(morning)
        let q = ChatMessage(botID: hero.id, role: .user, payload: .text("Why is the orders query slower?"), createdAt: today(10, 26))
        messages.append(q)
        messages.append(ChatMessage(botID: hero.id, role: .assistant, payload: .text("""
            The slowdown is in one statement: the `orders` lookup by `customer_id` and `status` (queryid -81233409). \
            Its mean time rose from **38 ms to 87 ms** across 18.4k calls since yesterday's baseline, while the call rate stayed flat — \
            so this is per-call cost, not load.

            What the evidence supports:
            - Shared-buffer reads for that query rose sharply, consistent with a plan that touches more pages.
            - No configuration change or stats reset was recorded in the window.

            What it doesn't show: whether the plan itself changed. pgbot doesn't keep historical plans, so I can't confirm that.

            **Next step:** run *Explain SQL* on the query to see its current plan.
            """), createdAt: today(10, 27), inReplyTo: q.id, modelName: "demo"))
        messages.append(ChatMessage(botID: hero.id, role: .assistant, payload: .resolved([
            Finding(id: "connections_high", severity: .warn, title: "Connection usage returned to normal levels", detail: "")
        ]), createdAt: today(12, 45)))
        for b in bots.dropFirst() {
            let r = records.first { $0.botID == b.id }!
            messages.append(ReportComposer.healthReport(bot: b, record: r, previous: nil, firstRun: true, scheduled: false))
        }

        notes = [
            AppNotification(botID: hero.id, category: .regression, severity: .warn, title: "Remotely Production — Warning",
                            body: "A query on orders became 2.3× slower", createdAt: today(10, 24), dedupKey: "demo1"),
            AppNotification(botID: hero.id, category: .warning, severity: .warn, title: "Remotely Production — Warning",
                            body: "Three potentially unused indexes occupy 4.2 GB", createdAt: today(11, 15), dedupKey: "demo2"),
            AppNotification(botID: bots[2].id, category: .warning, severity: .warn, title: "Ekomspace — Warning",
                            body: "Autovacuum is behind on public.events", createdAt: today(7, 40), dedupKey: "demo3"),
            AppNotification(botID: hero.id, category: .resolved, severity: .info, title: "Remotely Production — resolved",
                            body: "Connection usage returned to normal levels", createdAt: today(12, 45), read: true, dedupKey: "demo4"),
            AppNotification(botID: bots[1].id, category: .scheduledReport, severity: .info, title: "PGBox — daily report",
                            body: "Healthy. No warnings or critical findings.", createdAt: today(9, 0), read: true, dedupKey: "demo5"),
        ]
        return Bundle(bots: bots, records: records, messages: messages, notifications: notes, samples: samples, logs: logs)
    }

    static func history(botID: UUID, base: Spec, score: Int, now: Date) -> [MetricSample] {
        var out: [MetricSample] = []
        var rng = SeededRNG(seed: UInt64(base.conns * 7919))
        let hours = 24 * 30
        for h in stride(from: hours, through: 0, by: -1) {
            let at = now.addingTimeInterval(Double(-h) * 3600)
            let hour = Double(Calendar.current.component(.hour, from: at))
            let diurnal = 0.65 + 0.35 * sin((hour - 8) / 24 * 2 * .pi)
            func jitter(_ x: Double) -> Double { x * (0.92 + 0.16 * Double.random(in: 0...1, using: &rng)) }
            let growth = 1 - Double(h) / Double(hours) * 0.06
            out += [
                .init(botID: botID, metric: .connections, at: at, value: (jitter(Double(base.conns) * diurnal)).rounded()),
                .init(botID: botID, metric: .connectionUtilization, at: at, value: jitter(Double(base.conns) * diurnal) / 100),
                .init(botID: botID, metric: .tps, at: at, value: jitter(base.tps * diurnal)),
                .init(botID: botID, metric: .cacheHitRatio, at: at, value: min(0.9999, 0.990 + 0.004 * Double.random(in: 0...1, using: &rng))),
                .init(botID: botID, metric: .dbSizeBytes, at: at, value: Double(base.size) * growth),
                .init(botID: botID, metric: .walBytesPerSec, at: at, value: jitter(base.tps * 2100 * diurnal)),
                .init(botID: botID, metric: .replicationLagSec, at: at, value: jitter(0.012)),
                .init(botID: botID, metric: .healthScore, at: at, value: Double(h < 30 ? score : min(100, score + 3))),
                .init(botID: botID, metric: .deadlocksPerMin, at: at, value: h % 97 == 0 ? 1 : 0),
                .init(botID: botID, metric: .activeSessions, at: at, value: (jitter(Double(base.conns) * 0.2 * diurnal)).rounded()),
            ]
        }
        return out
    }

    static func demoLogs(now: Date) -> [LogEntry] {
        let rows: [(Double, String, String, String, String?)] = [
            (-60, "LOG", "info", "checkpoint complete: wrote 1832 buffers (11.2%); 0 WAL file(s) added", nil),
            (-240, "ERROR", "error", "deadlock detected", "Process 48211 waits for ShareLock on transaction 99812; blocked by process 48190."),
            (-600, "WARNING", "warn", "there is already a transaction in progress", nil),
            (-900, "ERROR", "error", "canceling statement due to statement timeout", "STATEMENT: SELECT … FROM orders WHERE customer_id = $1"),
            (-1500, "FATAL", "error", "password authentication failed for user \"report_ro\"", nil),
            (-2400, "LOG", "info", "automatic vacuum of table \"remotely.public.events\": index scans: 1", nil),
            (-3600, "LOG", "query", "duration: 2412.118 ms  statement: SELECT … FROM orders o JOIN customers c …", nil),
        ]
        return rows.map { LogEntry(time: now.addingTimeInterval($0.0), severity: $0.1, level: $0.2, message: $0.3, detail: $0.4,
                                    user: "app", database: "remotely", pid: 48000 + Int(-$0.0) % 900, app: "api") }
    }

    static func contextJSON(_ s: Spec, at: Date) -> Data {
        let ts = ISO8601DateFormatter().string(from: at)
        let findings = s.findings.map { f in
            """
            {"id":"\(f.0)","object":"\(f.1)","severity":"\(f.2.rawValue)","title":"\(f.3)","detail":"\(f.4)",
             "remediation":\(f.5.map { "\"\($0)\"" } ?? "null"),
             "impact":{"score":70,"dimension":"\(f.0 == "query_regression" ? "latency" : "storage")","estimate":"","basis":"demo"},
             "confidence":0.8}
            """
        }.joined(separator: ",")
        let unused = s.findings.contains { $0.0 == "unused_indexes" } ? """
            [{"schema":"public","table":"orders","index":"orders_created_idx","scans":0,"bytes":1932735283},
             {"schema":"public","table":"orders","index":"orders_status_idx","scans":0,"bytes":1395864371},
             {"schema":"public","table":"orders","index":"orders_legacy_ref_idx","scans":0,"bytes":1181116006}]
            """ : "[]"
        let json = """
        {"schema_version":"1.6.0","collected_at":"\(ts)","fingerprint":"demo-\(s.db)",
         "server":{"version_num":160004,"version_text":"PostgreSQL 16.4","database":"\(s.db)","uptime_seconds":3888000,
                   "extensions":["pg_stat_statements"],"capabilities":["pg_stat_statements"],"has_pg_monitor":true},
         "window":{"sample_seconds":3},
         "health":{"exactness":"sampled","connections":\(s.conns),"tps":\(s.tps),"rollback_ratio":0.002,"cache_hit_ratio":0.994,"deadlocks_per_min":0},
         "activity":{"exactness":"scraped","total":\(s.conns),"active":\(max(1, s.conns / 5)),"idle":\(s.conns - s.conns / 5 - 1),"idle_in_transaction":1,"waiting":0,
                     "by_state":{},"longest_xact_sec":4.2,"longest_active_sec":1.1},
         "locks":{"exactness":"scraped","blocked_count":0},
         "queries":{"exactness":"cumulative","enabled":true,"total_exec_ms":91234567,"top":[
            {"queryid":-81233409,"query":"SELECT o.* FROM orders o WHERE o.customer_id = $1 AND o.status = $2 ORDER BY o.created_at DESC LIMIT $3","calls":18400,"total_ms":1600800,"mean_ms":87,"max_ms":2412,"rows":184000,"cache_hit":0.93,"wal_bytes":0},
            {"queryid":5512,"query":"UPDATE carts SET updated_at = now() WHERE id = $1","calls":912000,"total_ms":820800,"mean_ms":0.9,"max_ms":41,"rows":912000,"cache_hit":0.999,"wal_bytes":81234567},
            {"queryid":7731,"query":"INSERT INTO events (kind, payload, created_at) VALUES ($1, $2, $3)","calls":2400000,"total_ms":480000,"mean_ms":0.2,"max_ms":12,"rows":2400000,"cache_hit":0.998,"wal_bytes":912345678},
            {"queryid":9001,"query":"SELECT count(*) FROM sessions WHERE expires_at > now()","calls":1440,"total_ms":201600,"mean_ms":140,"max_ms":390,"rows":1440,"cache_hit":0.97,"wal_bytes":0}]},
         "tables":{"exactness":"scraped","db_size_bytes":\(s.size),"top":[
            {"schema":"public","table":"orders","total_bytes":\(s.size / 3),"live_tuples":48000000,"dead_tuples":900000,"dead_ratio":0.018,"seq_scans":12,"index_scans":9812345},
            {"schema":"public","table":"events","total_bytes":\(s.size / 4),"live_tuples":132000000,"dead_tuples":4100000,"dead_ratio":0.031,"seq_scans":3,"index_scans":120344},
            {"schema":"public","table":"customers","total_bytes":\(s.size / 12),"live_tuples":2100000,"dead_tuples":2100,"dead_ratio":0.001,"seq_scans":40,"index_scans":4412345}]},
         "indexes":{"exactness":"cumulative","total":48,"unused":\(unused)},
         "wal":{"exactness":"sampled","bytes_per_sec":\(s.tps * 2100)},
         "replication":{"exactness":"scraped","is_replica":false,"replicas":[{"client_addr":"10.0.4.12","application_name":"replica-1","state":"streaming","sync_state":"async","replay_lag_sec":0.012,"replay_lag_bytes":8192,"write_lag_bytes":0,"flush_lag_bytes":0}],
                        "slots":[{"name":"replica_1","type":"physical","active":true,"retained_bytes":16777216}]},
         "limits":{"connections_used":\(s.conns),"connections_max":100,"max_xid_age":184000000},
         "findings":[\(findings)]}
        """
        return Data(json.utf8)
    }
}

extension ChatMessage {
    func with(createdAt: Date) -> ChatMessage { var c = self; c.createdAt = createdAt; return c }
}

/// Deterministic randomness so demo charts are stable across launches.
struct SeededRNG: RandomNumberGenerator {
    var state: UInt64
    init(seed: UInt64) { state = seed == 0 ? 0x9E3779B97F4A7C15 : seed }
    mutating func next() -> UInt64 {
        state ^= state << 13; state ^= state >> 7; state ^= state << 17; return state
    }
}

public extension DemoData {
    /// The hero demo context in engine JSON form, for contract checks.
    static func contextJSONForChecks() -> Data { contextJSON(specs[0], at: Date()) }
}
