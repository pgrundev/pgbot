import Foundation

// MARK: - Bot identity

public enum BotColor: String, Codable, Sendable, CaseIterable, Identifiable {
    case blue, green, orange, purple, pink, yellow, red, teal, gray, black
    public var id: String { rawValue }
    public var label: String { rawValue.capitalized }
}

public enum AvatarShape: String, Codable, Sendable, CaseIterable {
    case circle, roundedSquare
}

public struct BotAvatar: Codable, Sendable, Equatable, Hashable {
    public var color: BotColor
    public var shape: AvatarShape
    public init(color: BotColor = .blue, shape: AvatarShape = .roundedSquare) { self.color = color; self.shape = shape }
}

public enum DatabaseEnvironment: String, Codable, Sendable, CaseIterable, Identifiable {
    case production, staging, development, local, custom
    public var id: String { rawValue }
    public var label: String { rawValue.capitalized }
}

public enum SSLMode: String, Codable, Sendable, CaseIterable, Identifiable {
    case disable, allow, prefer, require
    case verifyCA = "verify-ca"
    case verifyFull = "verify-full"
    public var id: String { rawValue }
}

// MARK: - Connection

/// Non-secret connection settings. The password lives only in the Keychain,
/// keyed by the bot id; it is never persisted here or written to a log.
public struct DatabaseConnection: Codable, Sendable, Equatable, Hashable {
    public var host: String
    public var port: Int
    public var database: String
    public var username: String
    public var sslMode: SSLMode
    public var connectTimeoutSeconds: Int
    /// Extra non-secret libpq parameters from a pasted URL (e.g. options, target_session_attrs).
    public var extraParameters: [String: String]

    public init(host: String = "localhost", port: Int = 5432, database: String = "postgres",
                username: String = "postgres", sslMode: SSLMode = .prefer, connectTimeoutSeconds: Int = 10,
                extraParameters: [String: String] = [:]) {
        self.host = host; self.port = port; self.database = database; self.username = username
        self.sslMode = sslMode; self.connectTimeoutSeconds = connectTimeoutSeconds; self.extraParameters = extraParameters
    }

    /// Server identity, independent of the logical database: one PostgreSQL
    /// instance can host many databases (and many bots).
    public var instanceKey: String { "\(host.lowercased()):\(port)" }

    /// A display-safe description — no password, no full connection string.
    public var safeSummary: String { "\(username)@\(host):\(port)/\(database)" }
}

/// A PostgreSQL server; several logical databases (bots) may share one.
public struct DatabaseInstance: Codable, Sendable, Equatable, Hashable, Identifiable {
    public var id: String          // host:port
    public var provider: String?   // engine-detected: rds, aurora, cloudsql, azure, supabase, neon…
    public var versionText: String?
}

// MARK: - Monitoring

public struct MonitoringSchedule: Codable, Sendable, Equatable, Hashable {
    public var enabled: Bool = true
    /// Seconds between background inspections.
    public var intervalSeconds: Int = 3600
    /// Hour of day (local) for the daily report, nil = off.
    public var dailyReportHour: Int? = 9
    /// Minimum severity that produces a notification.
    public var notifyMinimum: Severity = .warn
    public var notifyResolved: Bool = true
    /// Finding keys (id|object) the user chose to ignore in this app.
    public var ignoredFindingKeys: Set<String> = []
    /// Local history retention in days.
    public var retentionDays: Int = 30
    public init() {}

    public static let intervalChoices: [(String, Int)] = [
        ("Every 15 minutes", 900), ("Every 30 minutes", 1800), ("Every hour", 3600),
        ("Every 6 hours", 21600), ("Every day", 86400),
    ]
}

public enum ScheduledTaskKind: String, Codable, Sendable, CaseIterable {
    case inspect, dailyReport, unusedIndexes, slowQueries
    public var label: String {
        switch self {
        case .inspect: "Inspect the database"
        case .dailyReport: "Send a morning health report"
        case .unusedIndexes: "Check for unused indexes"
        case .slowQueries: "Analyze slow queries"
        }
    }
}

public struct ScheduledTask: Codable, Sendable, Equatable, Hashable, Identifiable {
    public var id: UUID = UUID()
    public var kind: ScheduledTaskKind
    public var intervalSeconds: Int
    public var lastRun: Date?
    public init(kind: ScheduledTaskKind, intervalSeconds: Int) { self.kind = kind; self.intervalSeconds = intervalSeconds }
}

// MARK: - Bot

public enum ConnectionStatus: String, Codable, Sendable {
    case unknown, connecting, healthy, degraded, critical, unreachable, paused
}

public struct DatabaseBot: Codable, Sendable, Equatable, Hashable, Identifiable {
    public var id: UUID
    public var displayName: String
    public var description: String
    public var avatar: BotAvatar
    public var environment: DatabaseEnvironment
    public var customGroup: String?
    public var connection: DatabaseConnection
    public var pinned: Bool
    public var muted: Bool
    public var sortOrder: Int
    public var schedule: MonitoringSchedule
    public var tasks: [ScheduledTask]
    public var createdAt: Date
    /// Demo bots carry sample data and never touch a real engine.
    public var isDemo: Bool

    // Latest status, cached for the sidebar (the source of truth is the latest inspection).
    public var status: ConnectionStatus
    public var lastInspectedAt: Date?
    public var lastSummary: String?
    public var lastScore: Int?
    public var unreadCount: Int

    public init(id: UUID = UUID(), displayName: String, description: String = "", avatar: BotAvatar = .init(),
                environment: DatabaseEnvironment = .production, customGroup: String? = nil,
                connection: DatabaseConnection, pinned: Bool = false, muted: Bool = false, sortOrder: Int = 0,
                schedule: MonitoringSchedule = .init(), tasks: [ScheduledTask] = [], createdAt: Date = .now,
                isDemo: Bool = false, status: ConnectionStatus = .unknown, lastInspectedAt: Date? = nil,
                lastSummary: String? = nil, lastScore: Int? = nil, unreadCount: Int = 0) {
        self.id = id; self.displayName = displayName; self.description = description; self.avatar = avatar
        self.environment = environment; self.customGroup = customGroup; self.connection = connection
        self.pinned = pinned; self.muted = muted; self.sortOrder = sortOrder; self.schedule = schedule
        self.tasks = tasks; self.createdAt = createdAt; self.isDemo = isDemo; self.status = status
        self.lastInspectedAt = lastInspectedAt; self.lastSummary = lastSummary; self.lastScore = lastScore
        self.unreadCount = unreadCount
    }

    public var keychainAccount: String { "db:\(id.uuidString)" }
    public var groupName: String { customGroup ?? environment.label }
}

// MARK: - Inspections

/// One stored engine run for a bot.
public struct InspectionRecord: Codable, Sendable, Equatable, Identifiable {
    public var id: UUID
    public var botID: UUID
    public var startedAt: Date
    public var finishedAt: Date
    public var context: EngineContext
    public var score: HealthScore
    public init(id: UUID = UUID(), botID: UUID, startedAt: Date, finishedAt: Date, context: EngineContext) {
        self.id = id; self.botID = botID; self.startedAt = startedAt; self.finishedAt = finishedAt
        self.context = context; self.score = HealthScore.from(context)
    }
}

/// A point-in-time metric extracted from an inspection, for history charts.
public struct MetricSample: Codable, Sendable, Equatable, Hashable {
    public var botID: UUID
    public var metric: MetricKey
    public var at: Date
    public var value: Double
    public init(botID: UUID, metric: MetricKey, at: Date, value: Double) {
        self.botID = botID; self.metric = metric; self.at = at; self.value = value
    }
}

public enum MetricKey: String, Codable, Sendable, CaseIterable {
    case healthScore, connections, connectionUtilization, tps, cacheHitRatio, rollbackRatio
    case deadlocksPerMin, dbSizeBytes, walBytesPerSec, replicationLagSec, blockedSessions, tempBytesPerSec
    case activeSessions, idleInTransaction

    public var label: String {
        switch self {
        case .healthScore: "Health score"
        case .connections: "Connections"
        case .connectionUtilization: "Connection utilization"
        case .tps: "Transactions / sec"
        case .cacheHitRatio: "Cache hit ratio"
        case .rollbackRatio: "Rollback rate"
        case .deadlocksPerMin: "Deadlocks / min"
        case .dbSizeBytes: "Database size"
        case .walBytesPerSec: "WAL generation"
        case .replicationLagSec: "Replication lag"
        case .blockedSessions: "Blocked sessions"
        case .tempBytesPerSec: "Temp file writes"
        case .activeSessions: "Active sessions"
        case .idleInTransaction: "Idle in transaction"
        }
    }

    /// Every sample is a direct read of an engine field — nothing is derived
    /// beyond unit-free ratios the engine already reports.
    public static func samples(from r: InspectionRecord) -> [MetricSample] {
        let c = r.context, at = c.collectedAt
        var out: [(MetricKey, Double?)] = [
            (.healthScore, r.score.completeness == .unavailable ? nil : Double(r.score.value)),
            (.connections, c.limits.map { Double($0.connectionsUsed) }),
            (.connectionUtilization, c.limits?.utilization),
            (.tps, c.health?.tps), (.cacheHitRatio, c.health?.cacheHitRatio),
            (.rollbackRatio, c.health?.rollbackRatio), (.deadlocksPerMin, c.health?.deadlocksPerMin),
            (.tempBytesPerSec, c.health?.tempBytesPerSec),
            (.dbSizeBytes, c.tables?.dbSizeBytes.map(Double.init)), (.walBytesPerSec, c.wal?.bytesPerSec),
            (.blockedSessions, c.locks?.blockedCount.map(Double.init)),
            (.activeSessions, c.activity?.active.map(Double.init)),
            (.idleInTransaction, c.activity?.idleInTransaction.map(Double.init)),
        ]
        if let rep = c.replication {
            let lags = (rep.replicas ?? []).compactMap(\.replayLagSec) + [rep.receiverLagSec].compactMap { $0 }
            out.append((.replicationLagSec, lags.max()))
        }
        return out.compactMap { k, v in v.map { MetricSample(botID: r.botID, metric: k, at: at, value: $0) } }
    }
}

// MARK: - Conversation

public enum ChatRole: String, Codable, Sendable { case user, assistant, system }

/// The structured payload a message renders. Production payloads only ever come
/// from engine output; `.text` from the assistant comes from `pgbot ask`.
public enum MessagePayload: Codable, Sendable, Equatable {
    case text(String)
    case healthReport(HealthReportPayload)
    case finding(Finding, change: FindingChange?)
    case findingsList(title: String, findings: [Finding])
    case sql(title: String, sql: String)
    case plan(sql: String, reading: String)
    case table(title: String, columns: [String], rows: [[String]])
    case metric(MetricKey, points: [MetricPoint])
    case connectionEvent(String, ok: Bool)
    case monitoringEvent(String)
    case resolved([Finding])
    case error(title: String, detail: String, hint: String?)
    /// What the user typed in PSQL mode.
    case psqlInput(String)
    /// Results of `pgbot sql run` (read-only), or a console notice.
    case psqlResult(PSQLResultPayload)
}

public enum FindingChange: String, Codable, Sendable { case new, escalated, deescalated, resolved }

public struct MetricPoint: Codable, Sendable, Equatable, Hashable {
    public var at: Date
    public var value: Double
    public init(at: Date, value: Double) { self.at = at; self.value = value }
}

public struct HealthReportPayload: Codable, Sendable, Equatable {
    public var headline: String
    public var score: HealthScore
    public var previousScore: Int?
    public var findings: [Finding]
    public var resolvedCount: Int
    public var serverVersion: String
    public var inspectedAt: Date
    public var isScheduled: Bool
    /// Optional so reports saved by earlier versions still decode.
    public var isFirstRun: Bool?
    public init(headline: String, score: HealthScore, previousScore: Int?, findings: [Finding],
                resolvedCount: Int, serverVersion: String, inspectedAt: Date, isScheduled: Bool, isFirstRun: Bool? = nil) {
        self.isFirstRun = isFirstRun
        self.headline = headline; self.score = score; self.previousScore = previousScore; self.findings = findings
        self.resolvedCount = resolvedCount; self.serverVersion = serverVersion; self.inspectedAt = inspectedAt
        self.isScheduled = isScheduled
    }
}

public struct ChatMessage: Codable, Sendable, Equatable, Identifiable {
    public var id: UUID
    public var botID: UUID
    public var role: ChatRole
    public var payload: MessagePayload
    public var createdAt: Date
    /// For assistant answers: the question that produced it (enables Regenerate).
    public var inReplyTo: UUID?
    public var modelName: String?

    public init(id: UUID = UUID(), botID: UUID, role: ChatRole, payload: MessagePayload, createdAt: Date = .now,
                inReplyTo: UUID? = nil, modelName: String? = nil) {
        self.id = id; self.botID = botID; self.role = role; self.payload = payload; self.createdAt = createdAt
        self.inReplyTo = inReplyTo; self.modelName = modelName
    }

    /// Plain text for search, copy, and export.
    public var plainText: String {
        switch payload {
        case .text(let s): return s
        case .healthReport(let r): return "\(r.headline)\nHealth score \(r.score.value)/100\n" + r.findings.map { "• \($0.title)" }.joined(separator: "\n")
        case .finding(let f, _): return "\(f.title)\n\(f.detail)"
        case .findingsList(let t, let fs): return t + "\n" + fs.map { "• \($0.title)" }.joined(separator: "\n")
        case .sql(let t, let s): return "\(t)\n\(s)"
        case .plan(let s, let r): return "\(s)\n\n\(r)"
        case .table(let t, let cols, let rows): return ([t, cols.joined(separator: "\t")] + rows.map { $0.joined(separator: "\t") }).joined(separator: "\n")
        case .metric(let k, _): return k.label
        case .connectionEvent(let s, _), .monitoringEvent(let s): return s
        case .resolved(let fs): return "Resolved: " + fs.map(\.title).joined(separator: ", ")
        case .error(let t, let d, let h): return [t, d, h ?? ""].joined(separator: "\n")
        case .psqlInput(let s): return s
        case .psqlResult(let r): return r.plainText
        }
    }
}

// MARK: - Notifications

public enum NotificationCategory: String, Codable, Sendable, CaseIterable {
    case criticalFinding, warning, scoreChange, regression, connectivity, resolved, scheduledReport
    public var label: String {
        switch self {
        case .criticalFinding: "Critical findings"
        case .warning: "New warnings"
        case .scoreChange: "Health score changes"
        case .regression: "Performance regressions"
        case .connectivity: "Connectivity failures"
        case .resolved: "Resolved issues"
        case .scheduledReport: "Scheduled reports"
        }
    }
}

public struct AppNotification: Codable, Sendable, Equatable, Identifiable {
    public var id: UUID
    public var botID: UUID
    public var category: NotificationCategory
    public var severity: Severity
    public var title: String
    public var body: String
    public var createdAt: Date
    public var read: Bool
    /// The message to scroll to when opened.
    public var messageID: UUID?
    /// Dedup key: same bot + key is not re-announced until its state changes.
    public var dedupKey: String
    public init(id: UUID = UUID(), botID: UUID, category: NotificationCategory, severity: Severity, title: String,
                body: String, createdAt: Date = .now, read: Bool = false, messageID: UUID? = nil, dedupKey: String) {
        self.id = id; self.botID = botID; self.category = category; self.severity = severity; self.title = title
        self.body = body; self.createdAt = createdAt; self.read = read; self.messageID = messageID; self.dedupKey = dedupKey
    }
}

public struct QuietHours: Codable, Sendable, Equatable {
    public var enabled: Bool = false
    public var startHour: Int = 22
    public var endHour: Int = 7
    public init() {}
    public func contains(_ date: Date, calendar: Calendar = .current) -> Bool {
        guard enabled else { return false }
        let h = calendar.component(.hour, from: date)
        return startHour <= endHour ? (h >= startHour && h < endHour) : (h >= startHour || h < endHour)
    }
}

// MARK: - Account

public enum AuthProvider: String, Codable, Sendable, CaseIterable { case google, github, local }

public struct User: Codable, Sendable, Equatable {
    public var id: String
    public var name: String
    public var email: String?
    public var avatarURL: URL?
    public var provider: AuthProvider
    public init(id: String, name: String, email: String?, avatarURL: URL?, provider: AuthProvider) {
        self.id = id; self.name = name; self.email = email; self.avatarURL = avatarURL; self.provider = provider
    }
}

public enum SubscriptionPlan: String, Codable, Sendable { case free, pro }

public struct Subscription: Codable, Sendable, Equatable {
    public var plan: SubscriptionPlan = .free
    public init() {}
    /// Everything diagnostic is free; Pro gates only future cloud capabilities.
    public func allows(_ feature: ProFeature) -> Bool { plan == .pro }
}

public enum ProFeature: String, Sendable { case cloudMonitoring, slack, email, webhooks, teams, extendedHistory }
