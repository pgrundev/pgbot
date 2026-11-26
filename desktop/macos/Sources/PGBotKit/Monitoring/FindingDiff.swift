import Foundation

/// What moved between two inspections — the same semantics as the engine's
/// monitor.Compare (internal/monitor/decide.go): identity is id+object, never
/// the title; suppressed findings are invisible.
public struct FindingChanges: Sendable, Equatable {
    public var new: [Finding] = []
    public var escalated: [Finding] = []
    public var deescalated: [Finding] = []
    /// Carries the OLD finding (its last known severity and wording).
    public var resolved: [Finding] = []
    public var unchanged: [Finding] = []
    public init() {}

    public var isEmpty: Bool { new.isEmpty && escalated.isEmpty && deescalated.isEmpty && resolved.isEmpty }

    public static func compare(previous: [Finding], current: [Finding], ignoring ignored: Set<String> = []) -> FindingChanges {
        let prev = previous.filter { !$0.isSuppressed && !ignored.contains($0.key) }
        let cur = current.filter { !$0.isSuppressed && !ignored.contains($0.key) }
        var before = Dictionary(prev.map { ($0.key, $0) }, uniquingKeysWith: { a, _ in a })
        var ch = FindingChanges()
        for f in cur {
            if let old = before.removeValue(forKey: f.key) {
                if f.severity > old.severity { ch.escalated.append(f) }
                else if f.severity < old.severity { ch.deescalated.append(f) }
                else { ch.unchanged.append(f) }
            } else {
                ch.new.append(f)
            }
        }
        ch.resolved = prev.filter { before[$0.key] != nil }
        let order: (Finding, Finding) -> Bool = { ($0.severity.rank, $0.impact?.score ?? 0) > ($1.severity.rank, $1.impact?.score ?? 0) }
        ch.new.sort(by: order); ch.escalated.sort(by: order); ch.deescalated.sort(by: order)
        ch.resolved.sort(by: order); ch.unchanged.sort(by: order)
        return ch
    }

    /// New + escalated at or above a minimum severity.
    public func worsened(minimum: Severity) -> [Finding] {
        (escalated + new).filter { $0.severity >= minimum }
    }
}

/// Remembers what was announced per bot so an unchanged finding is never
/// announced twice — across restarts, failed runs in between, and flapping.
public struct AnnouncementLedger: Codable, Sendable, Equatable {
    /// finding key → severity last announced
    public var announced: [String: Severity] = [:]
    public var connectivityDown = false
    /// When the user last got a full health report in the chat: the daily
    /// report, or the first report right after the database was added.
    public var lastDailyReport: Date?
    public init() {}

    /// A full report is due once per calendar day, from `hour` on, unless one
    /// has already been shown today. Adding a database at 3 PM shows its first
    /// report; that one counts for today, so no second report follows a minute later.
    public func isDailyReportDue(hour: Int?, now: Date, calendar: Calendar = .current) -> Bool {
        guard let hour, calendar.component(.hour, from: now) >= hour else { return false }
        if let last = lastDailyReport, calendar.isDate(last, inSameDayAs: now) { return false }
        return true
    }
}

public struct PlannedNotification: Sendable, Equatable {
    public var category: NotificationCategory
    public var severity: Severity
    public var title: String
    public var body: String
    public var dedupKey: String
    public var findingKey: String?
    public init(category: NotificationCategory, severity: Severity, title: String, body: String, dedupKey: String, findingKey: String? = nil) {
        self.category = category; self.severity = severity; self.title = title; self.body = body
        self.dedupKey = dedupKey; self.findingKey = findingKey
    }
}

/// Decides which notifications an inspection produces. Pure; the app delivers them.
public enum NotificationPlanner {
    public struct Preferences: Sendable, Equatable {
        public var minimum: Severity
        public var notifyResolved: Bool
        public var disabled: Set<NotificationCategory>
        public init(minimum: Severity = .warn, notifyResolved: Bool = true, disabled: Set<NotificationCategory> = []) {
            self.minimum = minimum; self.notifyResolved = notifyResolved; self.disabled = disabled
        }
    }

    public static func plan(botName: String, changes: FindingChanges, scoreBefore: Int?, scoreAfter: HealthScore,
                            ledger: inout AnnouncementLedger, prefs: Preferences) -> [PlannedNotification] {
        var out: [PlannedNotification] = []

        for f in changes.worsened(minimum: prefs.minimum) where ledger.announced[f.key] != f.severity {
            ledger.announced[f.key] = f.severity
            let cat: NotificationCategory = f.severity == .critical ? .criticalFinding : (isRegression(f) ? .regression : .warning)
            let verb = changes.escalated.contains(f) ? "escalated to \(f.severity.label.lowercased())" : f.severity.label
            out.append(.init(category: cat, severity: f.severity, title: "\(botName) — \(verb)",
                             body: f.title, dedupKey: "\(f.key)|\(f.severity.rawValue)", findingKey: f.key))
        }
        // De-escalations update the ledger silently; they're not alarms.
        for f in changes.deescalated { ledger.announced[f.key] = f.severity }

        let resolvedAnnounced = changes.resolved.filter { ledger.announced[$0.key] != nil }
        for f in changes.resolved { ledger.announced.removeValue(forKey: f.key) }
        if prefs.notifyResolved, !resolvedAnnounced.isEmpty {
            let body = resolvedAnnounced.count == 1 ? resolvedAnnounced[0].title : "\(resolvedAnnounced.count) issues resolved"
            out.append(.init(category: .resolved, severity: .info, title: "\(botName) — resolved", body: body,
                             dedupKey: "resolved|" + resolvedAnnounced.map(\.key).sorted().joined(separator: ","), findingKey: nil))
        }

        if let before = scoreBefore, scoreAfter.completeness != .unavailable, before - scoreAfter.value >= 10 {
            out.append(.init(category: .scoreChange, severity: .warn, title: "\(botName) — health score dropped",
                             body: "Health score went from \(before) to \(scoreAfter.value).",
                             dedupKey: "score|\(scoreAfter.value)", findingKey: nil))
        }
        if ledger.connectivityDown {
            ledger.connectivityDown = false
            out.append(.init(category: .resolved, severity: .info, title: "\(botName) — reachable again",
                             body: "PGBot reconnected and finished an inspection.", dedupKey: "connectivity|up", findingKey: nil))
        }
        return out.filter { !prefs.disabled.contains($0.category) }
    }

    /// A failed run announces an outage once, not every retry.
    public static func planFailure(botName: String, error: EngineError, ledger: inout AnnouncementLedger,
                                   prefs: Preferences) -> PlannedNotification? {
        guard !ledger.connectivityDown, !prefs.disabled.contains(.connectivity) else { return nil }
        guard case .connection = error else { return nil }
        ledger.connectivityDown = true
        return .init(category: .connectivity, severity: .critical, title: "\(botName) — \(error.title.lowercased())",
                     body: error.hint, dedupKey: "connectivity|down", findingKey: nil)
    }

    static func isRegression(_ f: Finding) -> Bool {
        f.impact?.dimension == "latency" || f.id.contains("regress") || f.id.contains("slow")
    }
}
