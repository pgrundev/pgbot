import Foundation

/// The 0–100 health grade.
///
/// This is the engine's own formula — `render.HealthScore` in
/// internal/render/dashboard.go, the number `pgbot inspect` and `pgbot report`
/// print: 100 minus 10 per critical, 3 per warning, 1 per info finding, skipping
/// suppressed findings, floored at 0. The engine does not (yet) emit the score in
/// `--json`, so it is mirrored here and pinned by a check against those exact
/// rules; keep the two in lock-step (see `HealthScoreChecks`).
///
/// Unlike the CLI, the app refuses to show a confident grade when the engine
/// reports that the evidence was incomplete: a database the role cannot see into
/// must not read as "100, healthy".
public struct HealthScore: Codable, Sendable, Equatable {
    public enum Completeness: String, Codable, Sendable {
        /// Every core section was collected.
        case complete
        /// Some sections were unavailable; the score covers only what was seen.
        case partial
        /// Too little was visible to grade the database at all.
        case unavailable
    }

    public var value: Int
    public var critical: Int
    public var warnings: Int
    public var info: Int
    public var completeness: Completeness
    /// Why the grade is partial/unavailable, in the engine's words.
    public var gaps: [String]

    public init(value: Int, critical: Int, warnings: Int, info: Int, completeness: Completeness, gaps: [String]) {
        self.value = value; self.critical = critical; self.warnings = warnings; self.info = info
        self.completeness = completeness; self.gaps = gaps
    }

    /// The engine formula, applied to engine findings.
    public static func engineScore(_ findings: [Finding]) -> Int {
        var penalty = 0
        for f in findings where !f.isSuppressed {
            switch f.severity {
            case .critical: penalty += 10
            case .warn: penalty += 3
            case .info: penalty += 1
            }
        }
        return max(0, 100 - penalty)
    }

    public static func from(_ c: EngineContext) -> HealthScore {
        let live = c.findings.filter { !$0.isSuppressed }
        var gaps: [String] = []
        let core: [(String, (any EngineSection)?)] = [
            ("Health", c.health), ("Activity", c.activity), ("Tables", c.tables),
            ("Indexes", c.indexes), ("Queries", c.queries), ("Replication", c.replication),
        ]
        var unavailableCore = 0
        for (name, section) in core {
            guard let s = section else { continue }
            if !s.isAvailable {
                unavailableCore += 1
                gaps.append("\(name): \(s.reason ?? "not visible to this role")")
            }
        }
        if c.server.hasPgMonitor == false {
            gaps.append("The monitoring role lacks pg_monitor, so some statistics are hidden.")
        }
        // Health and Activity are the backbone; without both, a grade would be a guess.
        let backboneMissing = (c.health.map { !$0.isAvailable } ?? true) && (c.activity.map { !$0.isAvailable } ?? true)
        let completeness: Completeness = backboneMissing ? .unavailable : (gaps.isEmpty ? .complete : .partial)
        return HealthScore(
            value: engineScore(c.findings),
            critical: live.filter { $0.severity == .critical }.count,
            warnings: live.filter { $0.severity == .warn }.count,
            info: live.filter { $0.severity == .info }.count,
            completeness: completeness, gaps: gaps)
    }
}

/// Presentation bands. Configurable; these are the defaults from the product spec.
public struct HealthBands: Codable, Sendable, Equatable {
    public var green: Int = 90
    public var yellow: Int = 70
    public var orange: Int = 50
    public init() {}

    public enum Band: String, Sendable { case green, yellow, orange, red, gray }

    public func band(for score: HealthScore?) -> Band {
        guard let s = score, s.completeness != .unavailable else { return .gray }
        switch s.value {
        case green...: return .green
        case yellow...: return .yellow
        case orange...: return .orange
        default: return .red
        }
    }
}
