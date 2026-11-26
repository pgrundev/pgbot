import Foundation

// A read-only Swift mirror of the subset of pgbot's versioned Context contract
// (internal/model/context.go, schema/pgbot-context-*.json) that the desktop app
// renders. The contract is additive-only, so unknown keys are ignored and every
// optional section stays optional: a missing section means "not collected", and
// a section whose `exactness` is "unavailable" means "could not be collected".
// Nothing here is computed by the app — these are the engine's numbers.

public struct EngineContext: Codable, Sendable, Equatable {
    public var schemaVersion: String
    public var collectedAt: Date
    public var fingerprint: String
    public var server: ServerInfo
    public var window: Window?
    public var health: HealthSection?
    public var activity: ActivitySection?
    public var locks: LocksSection?
    public var queries: QueriesSection?
    public var tables: TablesSection?
    public var indexes: IndexesSection?
    public var wal: WALSection?
    public var replication: ReplicationSection?
    public var settings: SettingsSection?
    public var limits: Limits?
    public var deltas: Deltas?
    public var deltaSuppressedReason: String?
    public var events: [Event]?
    public var waitProfile: WaitProfile?
    public var findings: [Finding]
    public var configWarnings: [String]?

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version", collectedAt = "collected_at", fingerprint, server, window
        case health, activity, locks, queries, tables, indexes, wal, replication, settings, limits, deltas
        case deltaSuppressedReason = "delta_suppressed_reason", events, waitProfile = "wait_profile"
        case findings, configWarnings = "config_warnings"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        schemaVersion = try c.decode(String.self, forKey: .schemaVersion)
        collectedAt = try c.decode(Date.self, forKey: .collectedAt)
        fingerprint = try c.decodeIfPresent(String.self, forKey: .fingerprint) ?? ""
        server = try c.decode(ServerInfo.self, forKey: .server)
        window = try c.decodeIfPresent(Window.self, forKey: .window)
        health = try c.decodeIfPresent(HealthSection.self, forKey: .health)
        activity = try c.decodeIfPresent(ActivitySection.self, forKey: .activity)
        locks = try c.decodeIfPresent(LocksSection.self, forKey: .locks)
        queries = try c.decodeIfPresent(QueriesSection.self, forKey: .queries)
        tables = try c.decodeIfPresent(TablesSection.self, forKey: .tables)
        indexes = try c.decodeIfPresent(IndexesSection.self, forKey: .indexes)
        wal = try c.decodeIfPresent(WALSection.self, forKey: .wal)
        replication = try c.decodeIfPresent(ReplicationSection.self, forKey: .replication)
        settings = try c.decodeIfPresent(SettingsSection.self, forKey: .settings)
        limits = try c.decodeIfPresent(Limits.self, forKey: .limits)
        deltas = try c.decodeIfPresent(Deltas.self, forKey: .deltas)
        deltaSuppressedReason = try c.decodeIfPresent(String.self, forKey: .deltaSuppressedReason)
        events = try c.decodeIfPresent([Event].self, forKey: .events)
        waitProfile = try c.decodeIfPresent(WaitProfile.self, forKey: .waitProfile)
        // `findings` is always emitted, but as `null` when empty in some paths.
        findings = try c.decodeIfPresent([Finding].self, forKey: .findings) ?? []
        configWarnings = try c.decodeIfPresent([String].self, forKey: .configWarnings)
    }

    /// The major.minor of the contract, for compatibility checks.
    public var schemaMajor: Int { Int(schemaVersion.split(separator: ".").first ?? "0") ?? 0 }
}

public enum Exactness: String, Codable, Sendable {
    case sampled, cumulative, scraped, unavailable
    case unknown
    public init(from decoder: Decoder) throws {
        let raw = try decoder.singleValueContainer().decode(String.self)
        self = Exactness(rawValue: raw) ?? .unknown
    }
}

/// Every engine section carries its exactness and, when unavailable, why.
public protocol EngineSection {
    var exactness: Exactness? { get }
    var reason: String? { get }
}

public extension EngineSection {
    var isAvailable: Bool { exactness != .unavailable }
}

public struct ServerInfo: Codable, Sendable, Equatable {
    public var versionNum: Int
    public var versionText: String
    public var database: String
    public var provider: String?
    public var inRecovery: Bool?
    public var viaPooler: Bool?
    public var uptimeSeconds: Int64?
    public var extensions: [String]?
    public var capabilities: [String]?
    public var hasPgMonitor: Bool?

    enum CodingKeys: String, CodingKey {
        case versionNum = "version_num", versionText = "version_text", database, provider
        case inRecovery = "in_recovery", viaPooler = "via_pooler", uptimeSeconds = "uptime_seconds"
        case extensions, capabilities, hasPgMonitor = "has_pg_monitor"
    }

    /// "PostgreSQL 16.3" from version_num 160003.
    public var displayVersion: String {
        guard versionNum > 0 else { return "PostgreSQL" }
        let major = versionNum / 10000, minor = versionNum % 100
        return "PostgreSQL \(major).\(minor)"
    }
}

public struct Window: Codable, Sendable, Equatable {
    public var sampleSeconds: Double?
    public var statsResetAt: Date?
    public var windowAgeSeconds: Int64?
    enum CodingKeys: String, CodingKey {
        case sampleSeconds = "sample_seconds", statsResetAt = "stats_reset_at", windowAgeSeconds = "window_age_seconds"
    }
}

public struct HealthSection: Codable, Sendable, Equatable, EngineSection {
    public var exactness: Exactness?
    public var reason: String?
    public var connections: Int?
    public var tps: Double?
    public var commitsPerSec: Double?
    public var rollbacksPerSec: Double?
    public var rollbackRatio: Double?
    public var cacheHitRatio: Double?
    public var deadlocksPerMin: Double?
    public var tempBytesPerSec: Double?
    public var tuplesReturnedPerSec: Double?
    public var tuplesWrittenPerSec: Double?
    enum CodingKeys: String, CodingKey {
        case exactness, reason, connections, tps, commitsPerSec = "commits_per_sec"
        case rollbacksPerSec = "rollbacks_per_sec", rollbackRatio = "rollback_ratio"
        case cacheHitRatio = "cache_hit_ratio", deadlocksPerMin = "deadlocks_per_min"
        case tempBytesPerSec = "temp_bytes_per_sec", tuplesReturnedPerSec = "tuples_returned_per_sec"
        case tuplesWrittenPerSec = "tuples_written_per_sec"
    }
}

public struct ConnGroup: Codable, Sendable, Equatable, Hashable {
    public var appName: String
    public var user: String
    public var state: String
    public var count: Int
    enum CodingKeys: String, CodingKey { case appName = "app_name", user, state, count }
}

public struct ActivitySection: Codable, Sendable, Equatable, EngineSection {
    public var exactness: Exactness?
    public var reason: String?
    public var total: Int?
    public var active: Int?
    public var idle: Int?
    public var idleInTransaction: Int?
    public var waiting: Int?
    public var waitEvents: [String: Int]?
    public var longestXactSec: Double?
    public var longestIdleXactSec: Double?
    public var longestActiveSec: Double?
    public var connections: [ConnGroup]?
    public var autovacuumWorkers: Int?
    enum CodingKeys: String, CodingKey {
        case exactness, reason, total, active, idle, idleInTransaction = "idle_in_transaction", waiting
        case waitEvents = "wait_events", longestXactSec = "longest_xact_sec"
        case longestIdleXactSec = "longest_idle_xact_sec", longestActiveSec = "longest_active_sec"
        case connections, autovacuumWorkers = "autovacuum_workers"
    }
}

public struct BlockingRow: Codable, Sendable, Equatable, Hashable {
    public var blockedPID: Int
    public var blockingPIDs: [Int64]
    public var waitEvent: String?
    public var waitSeconds: Double
    public var blockedQuery: String
    enum CodingKeys: String, CodingKey {
        case blockedPID = "blocked_pid", blockingPIDs = "blocking_pids", waitEvent = "wait_event"
        case waitSeconds = "wait_seconds", blockedQuery = "blocked_query"
    }
}

public struct LocksSection: Codable, Sendable, Equatable, EngineSection {
    public var exactness: Exactness?
    public var reason: String?
    public var blockedCount: Int?
    public var chains: [BlockingRow]?
    enum CodingKeys: String, CodingKey { case exactness, reason, blockedCount = "blocked_count", chains }
}

public struct QueryStat: Codable, Sendable, Equatable, Hashable, Identifiable {
    public var queryID: Int64
    public var query: String
    public var calls: Int64
    public var totalMS: Double
    public var meanMS: Double
    public var maxMS: Double?
    public var rows: Int64?
    public var cacheHit: Double?
    public var walBytes: Int64?
    public var id: Int64 { queryID }
    enum CodingKeys: String, CodingKey {
        case queryID = "queryid", query, calls, totalMS = "total_ms", meanMS = "mean_ms", maxMS = "max_ms"
        case rows, cacheHit = "cache_hit", walBytes = "wal_bytes"
    }
}

public struct QueriesSection: Codable, Sendable, Equatable, EngineSection {
    public var exactness: Exactness?
    public var reason: String?
    public var enabled: Bool?
    public var totalExecMS: Double?
    public var top: [QueryStat]?
    enum CodingKeys: String, CodingKey { case exactness, reason, enabled, totalExecMS = "total_exec_ms", top }
}

public struct TableStat: Codable, Sendable, Equatable, Hashable, Identifiable {
    public var schema: String
    public var name: String
    public var totalBytes: Int64
    public var liveTuples: Int64
    public var deadTuples: Int64
    public var deadRatio: Double
    public var seqScans: Int64?
    public var indexScans: Int64?
    public var lastVacuum: Date?
    public var lastAutovacuum: Date?
    public var lastAnalyze: Date?
    public var lastAutoanalyze: Date?
    public var autovacuumDisabled: Bool?
    public var id: String { schema + "." + name }
    enum CodingKeys: String, CodingKey {
        case schema, name = "table", totalBytes = "total_bytes", liveTuples = "live_tuples"
        case deadTuples = "dead_tuples", deadRatio = "dead_ratio", seqScans = "seq_scans", indexScans = "index_scans"
        case lastVacuum = "last_vacuum", lastAutovacuum = "last_autovacuum", lastAnalyze = "last_analyze"
        case lastAutoanalyze = "last_autoanalyze", autovacuumDisabled = "autovacuum_disabled"
    }
    public var lastAnyVacuum: Date? { [lastVacuum, lastAutovacuum].compactMap { $0 }.max() }
}

public struct TablesSection: Codable, Sendable, Equatable, EngineSection {
    public var exactness: Exactness?
    public var reason: String?
    public var dbSizeBytes: Int64?
    public var top: [TableStat]?
    enum CodingKeys: String, CodingKey { case exactness, reason, dbSizeBytes = "db_size_bytes", top }
}

public struct IndexStat: Codable, Sendable, Equatable, Hashable, Identifiable {
    public var schema: String
    public var table: String
    public var name: String
    public var scans: Int64
    public var bytes: Int64
    public var definition: String?
    public var unique: Bool?
    public var primary: Bool?
    public var id: String { schema + "." + name }
    enum CodingKeys: String, CodingKey { case schema, table, name = "index", scans, bytes, definition, unique, primary }
}

public struct IndexesSection: Codable, Sendable, Equatable, EngineSection {
    public var exactness: Exactness?
    public var reason: String?
    public var total: Int?
    public var unused: [IndexStat]?
    public var largest: [IndexStat]?
}

public struct WALSection: Codable, Sendable, Equatable, EngineSection {
    public var exactness: Exactness?
    public var reason: String?
    public var bytesPerSec: Double?
    public var dirBytes: Int64?
    enum CodingKeys: String, CodingKey { case exactness, reason, bytesPerSec = "bytes_per_sec", dirBytes = "dir_bytes" }
}

public struct ReplicaRow: Codable, Sendable, Equatable, Hashable {
    public var clientAddr: String
    public var appName: String?
    public var state: String
    public var syncState: String
    public var replayLagSec: Double?
    public var replayLagBytes: Int64?
    enum CodingKeys: String, CodingKey {
        case clientAddr = "client_addr", appName = "application_name", state, syncState = "sync_state"
        case replayLagSec = "replay_lag_sec", replayLagBytes = "replay_lag_bytes"
    }
}

public struct ReplicationSlot: Codable, Sendable, Equatable, Hashable {
    public var name: String
    public var type: String
    public var active: Bool
    public var retainedBytes: Int64
    public var walStatus: String?
    enum CodingKeys: String, CodingKey { case name, type, active, retainedBytes = "retained_bytes", walStatus = "wal_status" }
}

public struct ReplicationSection: Codable, Sendable, Equatable, EngineSection {
    public var exactness: Exactness?
    public var reason: String?
    public var isReplica: Bool?
    public var replicas: [ReplicaRow]?
    public var receiverLagSec: Double?
    public var slots: [ReplicationSlot]?
    enum CodingKeys: String, CodingKey {
        case exactness, reason, isReplica = "is_replica", replicas, receiverLagSec = "receiver_lag_sec", slots
    }
}

public struct SettingsSection: Codable, Sendable, Equatable, EngineSection {
    public var exactness: Exactness?
    public var reason: String?
    public var overrides: [String: String]?
    public var params: [String: String]?
}

public struct Limits: Codable, Sendable, Equatable {
    public var connectionsUsed: Int
    public var connectionsMax: Int
    public var maxXIDAge: Int64?
    enum CodingKeys: String, CodingKey { case connectionsUsed = "connections_used", connectionsMax = "connections_max", maxXIDAge = "max_xid_age" }
    public var utilization: Double? { connectionsMax > 0 ? Double(connectionsUsed) / Double(connectionsMax) : nil }
    /// Fraction of the ~2^31 transaction-ID wraparound wall consumed.
    public var wraparoundFraction: Double? { maxXIDAge.map { Double($0) / 2_147_483_648.0 } }
}

public struct Delta: Codable, Sendable, Equatable, Hashable {
    public var id: String
    public var subject: String
    public var severity: String
    public var before: Double
    public var after: Double
    public var pctChange: Double?
    public var note: String?
    enum CodingKeys: String, CodingKey { case id, subject, severity, before, after, pctChange = "pct_change", note }
}

public struct Deltas: Codable, Sendable, Equatable {
    public var against: Date
    public var changes: [Delta]?
}

public struct Event: Codable, Sendable, Equatable, Hashable {
    public var kind: String
    public var object: String?
    public var before: String?
    public var after: String?
    public var confidence: Double?
}

public struct WaitBucket: Codable, Sendable, Equatable, Hashable {
    public var type: String
    public var count: Int
    public var share: Double
}

public struct WaitProfile: Codable, Sendable, Equatable {
    public var available: Bool
    public var reason: String?
    public var samples: Int?
    public var windowSeconds: Double?
    public var buckets: [WaitBucket]?
    enum CodingKeys: String, CodingKey { case available, reason, samples, windowSeconds = "window_seconds", buckets }
}

public struct Impact: Codable, Sendable, Equatable, Hashable {
    public var score: Double
    public var dimension: String
    public var estimate: String
    public var basis: String
}

public struct SafetyGuard: Codable, Sendable, Equatable, Hashable {
    public var id: String
    public var action: String
    public var text: String
    public var verify: String?
}

public struct Safety: Codable, Sendable, Equatable, Hashable {
    public var blockingCaveats: [SafetyGuard]?
    enum CodingKeys: String, CodingKey { case blockingCaveats = "blocking_caveats" }
}

public enum Severity: String, Codable, Sendable, Comparable, CaseIterable {
    case info, warn, critical
    public init(from decoder: Decoder) throws {
        let raw = try decoder.singleValueContainer().decode(String.self)
        self = Severity(rawValue: raw) ?? .info
    }
    public var rank: Int { switch self { case .info: 1; case .warn: 2; case .critical: 3 } }
    public static func < (a: Severity, b: Severity) -> Bool { a.rank < b.rank }
    public var label: String { switch self { case .info: "Info"; case .warn: "Warning"; case .critical: "Critical" } }
}

public struct Finding: Codable, Sendable, Equatable, Hashable, Identifiable {
    public var id: String
    public var object: String?
    public var severity: Severity
    public var title: String
    public var detail: String
    public var evidence: [String]?
    public var remediation: String?
    public var impact: Impact?
    public var confidence: Double?
    public var caveats: [String]?
    public var safety: Safety?
    public var suppressed: Bool?
    public var suppressionReason: String?

    enum CodingKeys: String, CodingKey {
        case id, object, severity, title, detail, evidence, remediation, impact, confidence, caveats, safety
        case suppressed, suppressionReason = "suppression_reason"
    }

    public init(id: String, object: String? = nil, severity: Severity, title: String, detail: String,
                evidence: [String]? = nil, remediation: String? = nil, impact: Impact? = nil,
                confidence: Double? = nil, caveats: [String]? = nil, safety: Safety? = nil,
                suppressed: Bool? = nil, suppressionReason: String? = nil) {
        self.id = id; self.object = object; self.severity = severity; self.title = title; self.detail = detail
        self.evidence = evidence; self.remediation = remediation; self.impact = impact; self.confidence = confidence
        self.caveats = caveats; self.safety = safety; self.suppressed = suppressed; self.suppressionReason = suppressionReason
    }

    /// Identity for diffing runs — the engine's own rule (monitor.FindingState.Key):
    /// id + object, never the title (titles embed counts that change run to run).
    public var key: String { object.map { "\(id)|\($0)" } ?? id }
    public var isSuppressed: Bool { suppressed ?? false }
    /// Below 0.5 the engine renders a finding as "possible", not an assertion.
    public var isTentative: Bool { (confidence ?? 1) < 0.5 }
}

// MARK: - Other engine JSON documents

/// One `pgbot logs --json` entry (literals already scrubbed by the engine).
public struct LogEntry: Codable, Sendable, Equatable, Hashable, Identifiable {
    public var time: Date
    public var severity: String
    public var level: String
    public var message: String
    public var detail: String?
    public var user: String?
    public var database: String?
    public var pid: Int?
    public var app: String?
    public var id: String { "\(time.timeIntervalSince1970)-\(pid ?? 0)-\(message.hashValue)" }
}

/// One `pgbot activity --json` session (query scrubbed by the engine).
public struct SessionRow: Codable, Sendable, Equatable, Hashable, Identifiable {
    public var pid: Int
    public var user: String?
    public var db: String?
    public var app: String?
    public var state: String
    public var wait: String?
    public var xactAgeS: Double?
    public var queryAgeS: Double?
    public var query: String?
    public var id: Int { pid }
    enum CodingKeys: String, CodingKey {
        case pid, user, db, app, state, wait, xactAgeS = "xact_age_s", queryAgeS = "query_age_s", query
    }
}

// MARK: - Decoding

public enum EngineJSON {
    /// Go's time.Time marshals as RFC 3339 with optional nanoseconds.
    public static func decoder() -> JSONDecoder {
        let d = JSONDecoder()
        d.dateDecodingStrategy = .custom { dec in
            let s = try dec.singleValueContainer().decode(String.self)
            if let date = parseDate(s) { return date }
            throw DecodingError.dataCorrupted(.init(codingPath: dec.codingPath, debugDescription: "bad date \(s)"))
        }
        return d
    }

    public static func encoder() -> JSONEncoder {
        let e = JSONEncoder()
        e.dateEncodingStrategy = .custom { date, enc in
            var c = enc.singleValueContainer()
            try c.encode(fractional.string(from: date))
        }
        return e
    }

    nonisolated(unsafe) private static let fractional: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter(); f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]; return f
    }()
    nonisolated(unsafe) private static let plain: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter(); f.formatOptions = [.withInternetDateTime]; return f
    }()

    static func parseDate(_ s: String) -> Date? {
        if let d = fractional.date(from: s) ?? plain.date(from: s) { return d }
        // ISO8601DateFormatter accepts at most millisecond precision on some OS
        // versions; Go emits nanoseconds. Trim the fraction to 3 digits and retry.
        if let dot = s.firstIndex(of: "."), let tz = s[dot...].firstIndex(where: { $0 == "Z" || $0 == "+" || $0 == "-" }) {
            let frac = s[s.index(after: dot)..<tz].prefix(3)
            return fractional.date(from: String(s[..<dot]) + "." + frac + String(s[tz...]))
        }
        return nil
    }

    public static func decodeContext(_ data: Data) throws -> EngineContext {
        try decoder().decode(EngineContext.self, from: data)
    }

    /// Parses newline-delimited JSON, skipping lines that are not objects of T.
    public static func decodeLines<T: Decodable>(_ type: T.Type, from data: Data) -> [T] {
        let d = decoder()
        return data.split(separator: UInt8(ascii: "\n")).compactMap { try? d.decode(T.self, from: Data($0)) }
    }
}
