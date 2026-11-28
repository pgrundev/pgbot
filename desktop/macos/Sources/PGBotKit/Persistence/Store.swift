import Foundation
import SQLite3

/// Local-first persistence in one SQLite file (WAL mode). Rows are JSON
/// documents keyed and indexed by the columns the app filters on. No model
/// stored here has a password field — secrets live only in the Keychain.
public final class Store: @unchecked Sendable {
    public enum StoreError: LocalizedError {
        case open(String), sql(String)
        public var errorDescription: String? {
            switch self { case .open(let m): "Couldn't open local data: \(m)"; case .sql(let m): "Local data error: \(m)" }
        }
    }

    private var db: OpaquePointer?
    private let lock = NSRecursiveLock()
    private let encoder = EngineJSON.encoder()
    private let decoder = EngineJSON.decoder()
    public let url: URL?

    /// `nil` url = in-memory (checks, demo previews).
    public init(url: URL?) throws {
        self.url = url
        if let url { try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true) }
        let path = url?.path ?? ":memory:"
        guard sqlite3_open_v2(path, &db, SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE | SQLITE_OPEN_FULLMUTEX, nil) == SQLITE_OK else {
            throw StoreError.open(String(cString: sqlite3_errmsg(db)))
        }
        if let url { try? FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path) }
        try migrate()
    }

    deinit { sqlite3_close(db) }

    private func migrate() throws {
        try exec("""
        PRAGMA journal_mode=WAL;
        PRAGMA foreign_keys=ON;
        CREATE TABLE IF NOT EXISTS bots(id TEXT PRIMARY KEY, json TEXT NOT NULL);
        CREATE TABLE IF NOT EXISTS messages(id TEXT PRIMARY KEY, bot_id TEXT NOT NULL, created_at REAL NOT NULL, json TEXT NOT NULL);
        CREATE INDEX IF NOT EXISTS messages_bot ON messages(bot_id, created_at);
        CREATE TABLE IF NOT EXISTS inspections(id TEXT PRIMARY KEY, bot_id TEXT NOT NULL, finished_at REAL NOT NULL, json TEXT NOT NULL);
        CREATE INDEX IF NOT EXISTS inspections_bot ON inspections(bot_id, finished_at);
        CREATE TABLE IF NOT EXISTS samples(bot_id TEXT NOT NULL, metric TEXT NOT NULL, at REAL NOT NULL, value REAL NOT NULL);
        CREATE INDEX IF NOT EXISTS samples_bot ON samples(bot_id, metric, at);
        CREATE TABLE IF NOT EXISTS notifications(id TEXT PRIMARY KEY, bot_id TEXT NOT NULL, created_at REAL NOT NULL, read INTEGER NOT NULL, json TEXT NOT NULL);
        CREATE INDEX IF NOT EXISTS notifications_time ON notifications(created_at);
        CREATE TABLE IF NOT EXISTS kv(key TEXT PRIMARY KEY, value TEXT NOT NULL);
        """)
    }

    // MARK: Bots

    public func saveBot(_ bot: DatabaseBot) throws {
        try run("INSERT INTO bots(id,json) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET json=excluded.json",
                [bot.id.uuidString, try json(bot)])
    }

    public func bots() throws -> [DatabaseBot] {
        try query("SELECT json FROM bots", []) { try decode(DatabaseBot.self, $0.text(0)) }
            .sorted { ($0.sortOrder, $0.createdAt) < ($1.sortOrder, $1.createdAt) }
    }

    /// Removes a bot and everything that belongs to it.
    public func deleteBot(_ id: UUID) throws {
        try transaction {
            for table in ["messages", "inspections", "samples", "notifications"] {
                try run("DELETE FROM \(table) WHERE bot_id=?", [id.uuidString])
            }
            try run("DELETE FROM bots WHERE id=?", [id.uuidString])
        }
    }

    // MARK: Conversation

    public func append(_ m: ChatMessage) throws {
        try run("INSERT OR REPLACE INTO messages(id,bot_id,created_at,json) VALUES(?,?,?,?)",
                [m.id.uuidString, m.botID.uuidString, m.createdAt.timeIntervalSince1970, try json(m)])
    }

    public func deleteMessage(_ id: UUID) throws { try run("DELETE FROM messages WHERE id=?", [id.uuidString]) }

    public func messages(for botID: UUID, limit: Int = 500) throws -> [ChatMessage] {
        try query("SELECT json FROM (SELECT json, created_at FROM messages WHERE bot_id=? ORDER BY created_at DESC LIMIT ?) ORDER BY created_at ASC",
                  [botID.uuidString, limit]) { try decode(ChatMessage.self, $0.text(0)) }
    }

    public func clearConversation(_ botID: UUID) throws { try run("DELETE FROM messages WHERE bot_id=?", [botID.uuidString]) }

    // MARK: Inspections & metrics

    public func save(_ r: InspectionRecord) throws {
        try transaction {
            try run("INSERT OR REPLACE INTO inspections(id,bot_id,finished_at,json) VALUES(?,?,?,?)",
                    [r.id.uuidString, r.botID.uuidString, r.finishedAt.timeIntervalSince1970, try json(r)])
            for s in MetricKey.samples(from: r) {
                try run("INSERT INTO samples(bot_id,metric,at,value) VALUES(?,?,?,?)",
                        [s.botID.uuidString, s.metric.rawValue, s.at.timeIntervalSince1970, s.value])
            }
        }
    }

    public func latestInspection(for botID: UUID) throws -> InspectionRecord? {
        try inspections(for: botID, limit: 1).first
    }

    public func inspections(for botID: UUID, limit: Int) throws -> [InspectionRecord] {
        try query("SELECT json FROM inspections WHERE bot_id=? ORDER BY finished_at DESC LIMIT ?", [botID.uuidString, limit]) {
            try decode(InspectionRecord.self, $0.text(0))
        }
    }

    public func samples(for botID: UUID, metric: MetricKey, since: Date) throws -> [MetricPoint] {
        try query("SELECT at,value FROM samples WHERE bot_id=? AND metric=? AND at>=? ORDER BY at",
                  [botID.uuidString, metric.rawValue, since.timeIntervalSince1970]) {
            MetricPoint(at: Date(timeIntervalSince1970: $0.double(0)), value: $0.double(1))
        }
    }

    /// Bulk insert for demo history.
    public func insertSamples(_ s: [MetricSample]) throws {
        try transaction {
            for x in s {
                try run("INSERT INTO samples(bot_id,metric,at,value) VALUES(?,?,?,?)",
                        [x.botID.uuidString, x.metric.rawValue, x.at.timeIntervalSince1970, x.value])
            }
        }
    }

    /// Drops history older than each bot's retention.
    public func prune(botID: UUID, olderThan cutoff: Date) throws {
        let t = cutoff.timeIntervalSince1970, id = botID.uuidString
        try transaction {
            try run("DELETE FROM samples WHERE bot_id=? AND at<?", [id, t])
            // keep the newest inspection even if old: it is the diff baseline
            try run("""
                DELETE FROM inspections WHERE bot_id=? AND finished_at<? AND id NOT IN
                (SELECT id FROM inspections WHERE bot_id=? ORDER BY finished_at DESC LIMIT 1)
                """, [id, t, id])
        }
    }

    // MARK: Notifications

    public func save(_ n: AppNotification) throws {
        try run("INSERT OR REPLACE INTO notifications(id,bot_id,created_at,read,json) VALUES(?,?,?,?,?)",
                [n.id.uuidString, n.botID.uuidString, n.createdAt.timeIntervalSince1970, n.read ? 1 : 0, try json(n)])
    }

    public func notifications(limit: Int = 300) throws -> [AppNotification] {
        try query("SELECT json FROM notifications ORDER BY created_at DESC LIMIT ?", [limit]) {
            try decode(AppNotification.self, $0.text(0))
        }
    }

    // MARK: Key-value settings

    public func setValue<T: Encodable>(_ v: T, for key: String) throws {
        try run("INSERT INTO kv(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", [key, try json(v)])
    }

    public func value<T: Decodable>(_ type: T.Type, for key: String) -> T? {
        (try? query("SELECT value FROM kv WHERE key=?", [key]) { try decode(T.self, $0.text(0)) })?.first
    }

    /// The stored JSON for a key, for callers that merge it with newer defaults.
    public func rawValue(for key: String) -> Data? {
        (try? query("SELECT value FROM kv WHERE key=?", [key]) { Data($0.text(0).utf8) })?.first
    }

    public func wipe() throws {
        try exec("DELETE FROM bots; DELETE FROM messages; DELETE FROM inspections; DELETE FROM samples; DELETE FROM notifications; DELETE FROM kv;")
    }

    // MARK: - SQLite plumbing

    private func json<T: Encodable>(_ v: T) throws -> String { String(decoding: try encoder.encode(v), as: UTF8.self) }
    private func decode<T: Decodable>(_ t: T.Type, _ s: String) throws -> T { try decoder.decode(t, from: Data(s.utf8)) }

    private func exec(_ sql: String) throws {
        lock.lock(); defer { lock.unlock() }
        var err: UnsafeMutablePointer<CChar>?
        if sqlite3_exec(db, sql, nil, nil, &err) != SQLITE_OK {
            let m = err.map { String(cString: $0) } ?? "unknown"; sqlite3_free(err)
            throw StoreError.sql(m)
        }
    }

    private func transaction(_ body: () throws -> Void) throws {
        lock.lock(); defer { lock.unlock() }
        try exec("BEGIN IMMEDIATE")
        do { try body(); try exec("COMMIT") } catch { try? exec("ROLLBACK"); throw error }
    }

    struct Row {
        let stmt: OpaquePointer
        func text(_ i: Int32) -> String { sqlite3_column_text(stmt, i).map { String(cString: $0) } ?? "" }
        func double(_ i: Int32) -> Double { sqlite3_column_double(stmt, i) }
    }

    private static let transient = unsafeBitCast(-1, to: sqlite3_destructor_type.self)

    private func prepare(_ sql: String, _ args: [Any]) throws -> OpaquePointer {
        var stmt: OpaquePointer?
        guard sqlite3_prepare_v2(db, sql, -1, &stmt, nil) == SQLITE_OK, let s = stmt else {
            throw StoreError.sql(String(cString: sqlite3_errmsg(db)))
        }
        for (i, a) in args.enumerated() {
            let idx = Int32(i + 1)
            switch a {
            case let v as String: sqlite3_bind_text(s, idx, v, -1, Self.transient)
            case let v as Int: sqlite3_bind_int64(s, idx, Int64(v))
            case let v as Double: sqlite3_bind_double(s, idx, v)
            default: sqlite3_bind_null(s, idx)
            }
        }
        return s
    }

    private func run(_ sql: String, _ args: [Any]) throws {
        lock.lock(); defer { lock.unlock() }
        let s = try prepare(sql, args)
        defer { sqlite3_finalize(s) }
        guard sqlite3_step(s) == SQLITE_DONE else { throw StoreError.sql(String(cString: sqlite3_errmsg(db))) }
    }

    private func query<T>(_ sql: String, _ args: [Any], _ map: (Row) throws -> T) throws -> [T] {
        lock.lock(); defer { lock.unlock() }
        let s = try prepare(sql, args)
        defer { sqlite3_finalize(s) }
        var out: [T] = []
        while sqlite3_step(s) == SQLITE_ROW { out.append(try map(Row(stmt: s))) }
        return out
    }
}
