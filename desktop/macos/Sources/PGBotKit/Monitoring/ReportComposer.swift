import Foundation

/// Turns engine output into conversation messages. Every number and sentence
/// comes from the engine's findings; this only chooses what to say first.
public enum ReportComposer {
    public static func healthReport(bot: DatabaseBot, record: InspectionRecord, previous: InspectionRecord?,
                                    firstRun: Bool, scheduled: Bool) -> ChatMessage {
        let live = record.context.findings.filter { !$0.isSuppressed && !bot.schedule.ignoredFindingKeys.contains($0.key) }
        let notable = live.filter { $0.severity >= .warn }
        let changes = previous.map { FindingChanges.compare(previous: $0.context.findings, current: record.context.findings) }
        // One sentence; the card supplies its own title ("Your daily database report", …).
        let criticals = notable.filter { $0.severity == .critical }.count
        let issues = notable.count == 1 ? "one issue" : "\(spelled(notable.count)) issues"
        let headline: String
        if record.score.completeness == .unavailable {
            headline = "I connected, but this role can't see enough to grade the database."
        } else if notable.isEmpty {
            headline = firstRun ? "I analyzed your database and didn't find anything that needs attention."
                : "Your database is healthy. No warnings or critical findings."
        } else if criticals > 0 {
            headline = "I found \(issues) that need attention, including \(criticals == 1 ? "one critical finding" : "\(spelled(criticals)) critical findings")."
        } else {
            headline = firstRun ? "I analyzed your database and found \(issues) worth reviewing."
                : "Your database is healthy overall, but I found \(issues) worth reviewing."
        }
        let payload = HealthReportPayload(
            headline: headline, score: record.score, previousScore: previous?.score.value,
            findings: live.sorted { ($0.severity.rank, $0.impact?.score ?? 0) > ($1.severity.rank, $1.impact?.score ?? 0) },
            resolvedCount: changes?.resolved.count ?? 0,
            serverVersion: record.context.server.displayVersion, inspectedAt: record.finishedAt, isScheduled: scheduled,
            isFirstRun: firstRun)
        return ChatMessage(botID: bot.id, role: .assistant, payload: .healthReport(payload), createdAt: record.finishedAt)
    }

    static func spelled(_ n: Int) -> String {
        let words = ["zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine"]
        return n < words.count ? words[n] : "\(n)"
    }

    /// Proactive messages for a background run: only what changed, never a repeat.
    public static func proactive(bot: DatabaseBot, changes: FindingChanges, at: Date) -> [ChatMessage] {
        var out: [ChatMessage] = []
        for f in changes.escalated { out.append(.init(botID: bot.id, role: .assistant, payload: .finding(f, change: .escalated), createdAt: at)) }
        for f in changes.new where f.severity >= .warn { out.append(.init(botID: bot.id, role: .assistant, payload: .finding(f, change: .new), createdAt: at)) }
        if !changes.resolved.isEmpty { out.append(.init(botID: bot.id, role: .assistant, payload: .resolved(changes.resolved), createdAt: at)) }
        return out
    }

    /// One-line sidebar summary: "2 new findings · Lock contention" / "Healthy · Last checked …".
    public static func sidebarSummary(record: InspectionRecord, changes: FindingChanges?) -> String {
        let live = record.context.findings.filter { !$0.isSuppressed }
        if record.score.completeness == .unavailable { return "Limited visibility · grant pg_monitor" }
        let crit = live.filter { $0.severity == .critical }, warn = live.filter { $0.severity == .warn }
        if let top = (crit + warn).first {
            let newCount = changes?.worsened(minimum: .warn).count ?? 0
            let lead = !crit.isEmpty ? "\(crit.count) critical" : (newCount > 0 ? "\(newCount) new" : "\(warn.count) warning\(warn.count == 1 ? "" : "s")")
            return "\(lead) · \(shortTitle(top.title))"
        }
        return "Healthy · No new findings"
    }

    static func shortTitle(_ s: String) -> String { s.count > 42 ? String(s.prefix(40)) + "…" : s }

    /// Tabular payloads for deterministic plus-menu actions, read from the latest inspection.
    public static func queriesTable(_ c: EngineContext, sortBy: KeyPath<QueryStat, Double> = \.totalMS, title: String) -> MessagePayload {
        guard let q = c.queries, q.isAvailable, let top = q.top, !top.isEmpty else {
            return .error(title: "Query statistics unavailable",
                          detail: c.queries?.reason ?? "pg_stat_statements isn't enabled for this database.",
                          hint: "Enable the pg_stat_statements extension (Show setup SQL lists the provider-specific steps).")
        }
        let rows = top.sorted { $0[keyPath: sortBy] > $1[keyPath: sortBy] }.prefix(10).map {
            [Format.ms($0.totalMS), Format.ms($0.meanMS), Format.count($0.calls), Format.sqlPreview($0.query)]
        }
        return .table(title: title, columns: ["Total", "Mean", "Calls", "Query"], rows: Array(rows))
    }

    public static func indexesTable(_ c: EngineContext) -> MessagePayload {
        guard let ix = c.indexes, ix.isAvailable else {
            return .error(title: "Index statistics unavailable", detail: c.indexes?.reason ?? "Not collected.", hint: nil)
        }
        let unused = ix.unused ?? []
        if unused.isEmpty {
            return .text("No potentially unused indexes among the \(ix.total ?? 0) indexes inspected.")
        }
        let rows = unused.prefix(15).map { [$0.schema + "." + $0.name, $0.table, Format.bytes($0.bytes), Format.count($0.scans)] }
        return .table(title: "Potentially unused indexes (\(Format.bytes(unused.reduce(0) { $0 + $1.bytes })))",
                      columns: ["Index", "Table", "Size", "Scans"], rows: Array(rows))
    }

    public static func locksTable(_ c: EngineContext) -> MessagePayload {
        guard let l = c.locks, l.isAvailable else {
            return .error(title: "Lock information unavailable", detail: c.locks?.reason ?? "Not collected.", hint: nil)
        }
        let chains = l.chains ?? []
        if chains.isEmpty { return .text("No blocked sessions at the time of the last inspection.") }
        return .table(title: "\(l.blockedCount ?? chains.count) blocked session(s)", columns: ["Blocked PID", "Blocked by", "Waiting", "Query"],
                      rows: chains.map { ["\($0.blockedPID)", $0.blockingPIDs.map(String.init).joined(separator: ", "),
                                          Format.seconds($0.waitSeconds), Format.sqlPreview($0.blockedQuery)] })
    }

    public static func replicationTable(_ c: EngineContext) -> MessagePayload {
        guard let r = c.replication, r.isAvailable else {
            return .error(title: "Replication information unavailable", detail: c.replication?.reason ?? "Not collected.", hint: nil)
        }
        var rows: [[String]] = (r.replicas ?? []).map {
            ["Replica \($0.appName ?? $0.clientAddr)", $0.state, $0.replayLagSec.map(Format.seconds) ?? "idle", $0.syncState]
        }
        rows += (r.slots ?? []).map { ["Slot \($0.name)", $0.active ? "active" : "inactive", Format.bytes($0.retainedBytes) + " retained", $0.type] }
        if let lag = r.receiverLagSec { rows.append(["This standby", "receiving", Format.seconds(lag), "—"]) }
        if rows.isEmpty { return .text(r.isReplica == true ? "This is a standby with no lag reported." : "No replicas or replication slots are configured.") }
        return .table(title: "Replication", columns: ["Member", "State", "Lag", "Mode"], rows: rows)
    }

    public static func sessionsTable(_ s: [SessionRow]) -> MessagePayload {
        if s.isEmpty { return .text("No client sessions right now.") }
        let rows = s.sorted { ($0.xactAgeS ?? 0) > ($1.xactAgeS ?? 0) }.prefix(20).map {
            ["\($0.pid)", $0.user ?? "", $0.state, $0.wait ?? "", $0.xactAgeS.map(Format.seconds) ?? "", Format.sqlPreview($0.query ?? "")]
        }
        return .table(title: "\(s.count) session(s)", columns: ["PID", "User", "State", "Wait", "Xact age", "Query"], rows: Array(rows))
    }
}

/// Shared, locale-aware formatting.
public enum Format {
    public static func bytes(_ b: Int64) -> String {
        let f = ByteCountFormatter()
        f.countStyle = .binary
        f.allowsNonnumericFormatting = false // "0 bytes", not "Zero KB"
        return f.string(fromByteCount: b)
    }
    public static func bytes(_ b: Double) -> String { bytes(Int64(b)) }
    public static func count(_ n: Int64) -> String { n.formatted(.number.notation(.compactName)) }
    public static func percent(_ r: Double, digits: Int = 1) -> String { (r).formatted(.percent.precision(.fractionLength(digits))) }
    public static func ms(_ ms: Double) -> String {
        switch ms {
        case ..<1: String(format: "%.2f ms", ms)
        case ..<1000: String(format: "%.0f ms", ms)
        case ..<60_000: String(format: "%.1f s", ms / 1000)
        case ..<3_600_000: String(format: "%.1f min", ms / 60_000)
        default: String(format: "%.1f h", ms / 3_600_000)
        }
    }
    public static func seconds(_ s: Double) -> String { ms(s * 1000) }
    public static func sqlPreview(_ q: String, limit: Int = 90) -> String {
        let one = q.split(whereSeparator: \.isWhitespace).joined(separator: " ")
        return one.count > limit ? String(one.prefix(limit - 1)) + "…" : one
    }
}
