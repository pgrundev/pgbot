import Foundation

/// The desktop app's only way to learn anything about a database: typed calls
/// onto the existing pgbot CLI's machine contracts. No diagnostics live in Swift.
public protocol DiagnosticEngine: Sendable {
    func version() async throws -> String
    func inspect(_ target: EngineTarget) async throws -> EngineContext
    func ask(_ target: EngineTarget, question: String, ai: AIConfiguration, apiKey: String?) async throws -> String
    func explainSQL(_ target: EngineTarget, sql: String) async throws -> String
    func logs(_ target: EngineTarget, last: Int) async throws -> [LogEntry]
    func activity(_ target: EngineTarget) async throws -> [SessionRow]
    func why(_ target: EngineTarget, fingerprint: String) async throws -> String
    func diff(_ target: EngineTarget, since: String) async throws -> String
    func setupSQL(role: String, database: String, includeLogs: Bool) async throws -> String
    func runSQL(_ target: EngineTarget, sql: String, maxRows: Int) async throws -> SQLRunOutput
}

/// What the engine needs to reach one database. Holds the secret DSN in memory
/// only for the duration of a call.
public struct EngineTarget: Sendable {
    public var dsn: String
    public var password: String?
    public var sshTunnel: String?
    public init(dsn: String, password: String?, sshTunnel: String? = nil) {
        self.dsn = dsn; self.password = password; self.sshTunnel = sshTunnel
    }

    public init(connection: DatabaseConnection, password: String?) {
        self.init(dsn: ConnectionString.build(connection, password: password), password: password)
    }

    var environment: [String: String] {
        var env = ["PGBOT_DATABASE_URL": dsn]
        if let t = sshTunnel, !t.isEmpty { env["PGBOT_SSH_TUNNEL"] = t }
        return env
    }

    var secrets: [String] { [dsn] + [password].compactMap { $0 } }
}

public struct PGBotEngine: DiagnosticEngine {
    /// Contract majors this build understands (schema/pgbot-context-1.x).
    public static let supportedSchemaMajor = 1
    public let runner: EngineRunner

    public init(runner: EngineRunner) { self.runner = runner }

    public func version() async throws -> String { try await runner.version() }

    public func inspect(_ t: EngineTarget) async throws -> EngineContext {
        let out = try await runner.run(.init(
            arguments: ["inspect", "--json", "--window", "3s"],
            environment: t.environment, timeout: .seconds(120), secrets: t.secrets))
        // 0 = clean, 1 = warnings, 2 = criticals — all are successful inspections.
        guard [0, 1, 2].contains(out.exitCode), !out.stdout.isEmpty else {
            throw EngineError.classify(stderr: out.stderr)
        }
        let ctx: EngineContext
        do { ctx = try EngineJSON.decodeContext(out.stdout) } catch {
            throw EngineError.invalidOutput("Could not read the inspection report: \(error.localizedDescription)")
        }
        guard ctx.schemaMajor == Self.supportedSchemaMajor else { throw EngineError.incompatibleSchema(found: ctx.schemaVersion) }
        return ctx
    }

    public func ask(_ t: EngineTarget, question: String, ai: AIConfiguration, apiKey: String?) async throws -> String {
        if ai.provider.needsKey && (apiKey ?? "").isEmpty { throw EngineError.aiNotConfigured }
        // --yes: the app has already shown its own disclosure and recorded consent.
        let out = try await runner.run(.init(
            arguments: ["ask", "--yes", "--window", "2s", question],
            environment: t.environment.merging(ai.environment(apiKey: apiKey)) { $1 },
            timeout: .seconds(240), secrets: t.secrets + [apiKey].compactMap { $0 }))
        let text = String(decoding: out.stdout, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        guard out.exitCode == 0, !text.isEmpty else {
            let err = EngineError.classify(stderr: out.stderr)
            if case .failed(let d) = err { throw EngineError.aiFailed(d) }
            throw err
        }
        // The engine already redacts its own output; this guards an echo of a pasted secret.
        return Redactor.scrub(text, secrets: t.secrets + [apiKey].compactMap { $0 })
    }

    public func explainSQL(_ t: EngineTarget, sql: String) async throws -> String {
        // SQL travels on stdin: it can carry literals and must not show in `ps`.
        let out = try await runner.run(.init(
            arguments: ["sql", "explain", "--no-color"], environment: t.environment,
            stdin: Data(sql.utf8), timeout: .seconds(60), secrets: t.secrets))
        guard out.exitCode == 0 else { throw EngineError.classify(stderr: out.stderr) }
        return String(decoding: out.stdout, as: UTF8.self)
    }

    public func logs(_ t: EngineTarget, last: Int) async throws -> [LogEntry] {
        let out = try await runner.run(.init(
            arguments: ["logs", "--json", "--last", String(last)], environment: t.environment,
            timeout: .seconds(45), secrets: t.secrets))
        guard out.exitCode == 0 else { throw EngineError.classify(stderr: out.stderr) }
        return EngineJSON.decodeLines(LogEntry.self, from: out.stdout)
    }

    public func activity(_ t: EngineTarget) async throws -> [SessionRow] {
        let out = try await runner.run(.init(
            arguments: ["activity", "--json"], environment: t.environment, timeout: .seconds(30), secrets: t.secrets))
        guard out.exitCode == 0 else { throw EngineError.classify(stderr: out.stderr) }
        return EngineJSON.decodeLines(SessionRow.self, from: out.stdout)
    }

    public func why(_ t: EngineTarget, fingerprint: String) async throws -> String {
        let out = try await runner.run(.init(
            arguments: ["why", "--no-color", "--fingerprint", fingerprint, "--window", "168h"],
            environment: t.environment, timeout: .seconds(60), secrets: t.secrets))
        guard out.exitCode == 0 else { throw EngineError.classify(stderr: out.stderr) }
        return String(decoding: out.stdout, as: UTF8.self)
    }

    public func diff(_ t: EngineTarget, since: String) async throws -> String {
        let out = try await runner.run(.init(
            arguments: ["diff", "--since", since], environment: t.environment, timeout: .seconds(120), secrets: t.secrets))
        guard [0, 1, 2].contains(out.exitCode) else { throw EngineError.classify(stderr: out.stderr) }
        return String(decoding: out.stdout, as: UTF8.self)
    }

    /// `pgbot sql run --json`: the user's own SQL in a READ ONLY transaction that
    /// is always rolled back. SQL travels on stdin. A statement error still
    /// returns output (with `error` set); only connection problems throw.
    public func runSQL(_ t: EngineTarget, sql: String, maxRows: Int = 500) async throws -> SQLRunOutput {
        let out = try await runner.run(.init(
            arguments: ["sql", "run", "--json", "--max-rows", String(maxRows), "--statement-timeout", "30s"],
            environment: t.environment, stdin: Data(sql.utf8), timeout: .seconds(120), secrets: t.secrets))
        if !out.stdout.isEmpty, let parsed = try? JSONDecoder().decode(SQLRunOutput.self, from: out.stdout) {
            return parsed
        }
        if out.exitCode == 64 {
            return SQLRunOutput(results: [], error: SQLRunError(message: out.stderr.replacingOccurrences(of: "pgbot: ", with: "")
                .trimmingCharacters(in: .whitespacesAndNewlines)), elapsedMS: 0)
        }
        throw EngineError.classify(stderr: out.stderr)
    }

    /// `pgbot init` without a connection: prints the read-only role recipe.
    public func setupSQL(role: String, database: String, includeLogs: Bool) async throws -> String {
        var args = ["init", "--role", role, "--database", database]
        if includeLogs { args.append("--logs") }
        let out = try await runner.run(.init(arguments: args, timeout: .seconds(15)))
        guard out.exitCode == 0 else { throw EngineError.classify(stderr: out.stderr) }
        return String(decoding: out.stdout, as: UTF8.self)
    }
}

/// Where to find the engine: an override, the app bundle, then a CLI install.
public enum EngineLocator {
    public static func locate(override: String? = nil, bundle: Bundle = .main) -> URL? {
        var candidates: [String] = []
        if let o = override, !o.isEmpty { candidates.append((o as NSString).expandingTildeInPath) }
        if let env = ProcessInfo.processInfo.environment["PGBOT_ENGINE_PATH"] { candidates.append(env) }
        if let res = bundle.resourceURL?.appendingPathComponent("engine/pgbot").path { candidates.append(res) }
        if let exe = bundle.executableURL?.deletingLastPathComponent().appendingPathComponent("pgbot").path { candidates.append(exe) }
        candidates += ["/opt/homebrew/bin/pgbot", "/usr/local/bin/pgbot", NSHomeDirectory() + "/go/bin/pgbot"]
        return candidates.first { FileManager.default.isExecutableFile(atPath: $0) }.map(URL.init(fileURLWithPath:))
    }
}

// MARK: - Check hooks (used by PGBotKitChecks; not part of the app's surface)

public extension EngineTarget {
    var environmentForChecks: [String: String] { environment }
    var secretsForChecks: [String] { secrets }
}
