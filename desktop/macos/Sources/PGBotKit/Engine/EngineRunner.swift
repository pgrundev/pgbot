import Foundation

/// Runs the bundled pgbot binary.
///
/// Security invariants (each is asserted in EngineRunnerChecks):
///  * Credentials never appear in argv — the DSN goes in `PGBOT_DATABASE_URL`,
///    AI keys in `PGBOT_AI_API_KEY`, SQL over stdin. `ps` shows only flags.
///  * The child gets a minimal, allow-listed environment, not the app's: a stray
///    `DATABASE_URL` or `OPENAI_API_KEY` in the user's session can't redirect a
///    run or leak a key to a provider the user didn't choose.
///  * stderr is redacted before it reaches any UI, log, or error.
///  * Every run has a timeout, honors Task cancellation (SIGTERM, then SIGKILL),
///    and goes through a global concurrency limit.
public actor EngineRunner {
    public struct Invocation: Sendable {
        public var arguments: [String]
        public var environment: [String: String]
        public var stdin: Data?
        public var timeout: Duration
        /// Values to scrub from stderr in addition to the generic redaction.
        public var secrets: [String]
        public init(arguments: [String], environment: [String: String] = [:], stdin: Data? = nil,
                    timeout: Duration = .seconds(90), secrets: [String] = []) {
            self.arguments = arguments; self.environment = environment; self.stdin = stdin
            self.timeout = timeout; self.secrets = secrets
        }
    }

    public struct Output: Sendable {
        public var exitCode: Int32
        public var stdout: Data
        /// Already redacted.
        public var stderr: String
    }

    public let binary: URL
    public let stateDirectory: URL
    private let limiter: AsyncLimiter

    public init(binary: URL, stateDirectory: URL, maxConcurrent: Int = 3) {
        self.binary = binary
        self.stateDirectory = stateDirectory
        self.limiter = AsyncLimiter(limit: maxConcurrent)
    }

    /// The child's whole environment. Nothing is inherited but locale/PATH basics.
    nonisolated func childEnvironment(_ extra: [String: String]) -> [String: String] {
        let parent = ProcessInfo.processInfo.environment
        var env: [String: String] = [
            "PATH": "/usr/bin:/bin:/usr/sbin:/sbin",
            "HOME": parent["HOME"] ?? NSHomeDirectory(),
            "LANG": parent["LANG"] ?? "en_US.UTF-8",
            "NO_COLOR": "1",
            // Baselines live in the app's container, separate from the CLI's.
            "XDG_STATE_HOME": stateDirectory.path,
            "XDG_CONFIG_HOME": stateDirectory.appendingPathComponent("config").path,
        ]
        if let tmp = parent["TMPDIR"] { env["TMPDIR"] = tmp }
        // SSH tunnels need the agent socket (the user opts in per bot).
        if extra["PGBOT_SSH_TUNNEL"] != nil, let sock = parent["SSH_AUTH_SOCK"] { env["SSH_AUTH_SOCK"] = sock }
        env.merge(extra) { $1 }
        return env
    }

    public func run(_ inv: Invocation) async throws -> Output {
        try await limiter.withPermit {
            try await self.execute(inv)
        }
    }

    private func execute(_ inv: Invocation) async throws -> Output {
        try Task.checkCancellation()
        guard FileManager.default.isExecutableFile(atPath: binary.path) else { throw EngineError.engineMissing(binary.path) }
        try? FileManager.default.createDirectory(at: stateDirectory, withIntermediateDirectories: true)

        let process = Process()
        process.executableURL = binary
        process.arguments = inv.arguments
        process.environment = childEnvironment(inv.environment)
        process.currentDirectoryURL = stateDirectory
        let out = Pipe(), err = Pipe(), input = Pipe()
        process.standardOutput = out
        process.standardError = err
        process.standardInput = input

        // Drain both pipes concurrently so a chatty child can't deadlock on a full buffer.
        let stdoutTask = Task.detached { out.fileHandleForReading.readDataToEndOfFile() }
        let stderrTask = Task.detached { err.fileHandleForReading.readDataToEndOfFile() }

        let box = ProcessBox(process)
        let exit: Int32 = try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Int32, Error>) in
                process.terminationHandler = { p in cont.resume(returning: p.terminationStatus) }
                do { try process.run() } catch {
                    process.terminationHandler = nil
                    cont.resume(throwing: EngineError.launchFailed(error.localizedDescription)); return
                }
                if let data = inv.stdin { input.fileHandleForWriting.write(data) }
                try? input.fileHandleForWriting.close()
                Task.detached { [timeout = inv.timeout] in
                    try? await Task.sleep(for: timeout)
                    if box.isRunning { box.timedOut = true; box.stop() }
                }
            }
        } onCancel: {
            box.stop()
        }

        let stdout = await stdoutTask.value
        let rawErr = String(decoding: await stderrTask.value, as: UTF8.self)
        let stderr = Redactor.scrub(rawErr, secrets: inv.secrets)
        if Task.isCancelled { throw CancellationError() }
        if box.timedOut { throw EngineError.timedOut(seconds: Int(inv.timeout.components.seconds)) }
        return Output(exitCode: exit, stdout: stdout, stderr: stderr)
    }

    /// `pgbot --version` → "0.11.1".
    public func version() async throws -> String {
        let o = try await run(.init(arguments: ["--version"], timeout: .seconds(10)))
        let text = String(decoding: o.stdout, as: UTF8.self)
        return text.split(separator: " ").last.map { String($0).trimmingCharacters(in: .whitespacesAndNewlines) } ?? text
    }
}

/// Holds the Process for cancellation from other threads.
final class ProcessBox: @unchecked Sendable {
    let process: Process
    var timedOut = false
    init(_ p: Process) { process = p }
    var isRunning: Bool { process.isRunning }
    func stop() {
        guard process.isRunning else { return }
        process.terminate() // SIGTERM: the engine closes its read-only transaction cleanly
        let pid = process.processIdentifier
        DispatchQueue.global().asyncAfter(deadline: .now() + 3) { [process] in
            if process.isRunning { kill(pid, SIGKILL) }
        }
    }
}

/// A FIFO counting semaphore for async code.
public actor AsyncLimiter {
    private let limit: Int
    private var inUse = 0
    private var waiters: [CheckedContinuation<Void, Never>] = []
    public init(limit: Int) { self.limit = max(1, limit) }

    public func withPermit<T: Sendable>(_ body: @Sendable () async throws -> T) async throws -> T {
        await acquire()
        defer { release() }
        return try await body()
    }

    private func acquire() async {
        if inUse < limit { inUse += 1; return }
        await withCheckedContinuation { waiters.append($0) }
    }

    private func release() {
        if waiters.isEmpty { inUse -= 1 } else { waiters.removeFirst().resume() }
    }

    public var active: Int { inUse }
}

/// User-facing engine failures. Every case carries a plain explanation and a next step.
public enum EngineError: LocalizedError, Equatable {
    case engineMissing(String)
    case launchFailed(String)
    case timedOut(seconds: Int)
    case incompatibleSchema(found: String)
    case connection(ConnectionProblem, detail: String)
    case failed(String)
    case invalidOutput(String)
    case aiNotConfigured
    case aiFailed(String)

    public enum ConnectionProblem: String, Sendable {
        case unreachable, authentication, ssl, permission, databaseMissing, timeout, other
    }

    public var title: String {
        switch self {
        case .engineMissing: "PGBot engine not found"
        case .launchFailed: "Couldn't start the PGBot engine"
        case .timedOut: "The inspection took too long"
        case .incompatibleSchema: "Engine version mismatch"
        case .connection(let p, _):
            switch p {
            case .unreachable: "Database unreachable"
            case .authentication: "Invalid credentials"
            case .ssl: "SSL connection failed"
            case .permission: "Insufficient permissions"
            case .databaseMissing: "Database not found"
            case .timeout: "Connection timed out"
            case .other: "Couldn't connect"
            }
        case .failed: "The engine reported an error"
        case .invalidOutput: "Unexpected engine output"
        case .aiNotConfigured: "No AI provider configured"
        case .aiFailed: "The AI provider returned an error"
        }
    }

    public var hint: String {
        switch self {
        case .engineMissing: "Reinstall PGBot, or set the engine path in Settings → Advanced."
        case .launchFailed: "Check that the bundled engine isn't quarantined, then try again."
        case .timedOut(let s): "No result after \(s)s. The database may be under heavy load; try again or raise the statement timeout."
        case .incompatibleSchema: "Update PGBot so the app and the engine agree on the report format."
        case .connection(let p, _):
            switch p {
            case .unreachable: "Check the host and port, and that this Mac can reach the server (VPN, firewall, allow-list)."
            case .authentication: "Check the user name and password. PGBot recommends a dedicated read-only role."
            case .ssl: "Try SSL mode “require”, or “verify-full” with the provider's CA installed."
            case .permission: "Grant the role pg_monitor. “Show setup SQL” prints the exact statements."
            case .databaseMissing: "Check the database name."
            case .timeout: "The server didn't answer in time. Raise the connection timeout or check the network."
            case .other: "See details below."
            }
        case .failed: "See details below."
        case .invalidOutput: "Update PGBot; the engine and app may be out of step."
        case .aiNotConfigured: "Add an API key in Settings → AI providers, or use a local model through Ollama."
        case .aiFailed: "Check the API key and model in Settings → AI providers."
        }
    }

    public var detail: String {
        switch self {
        case .engineMissing(let p): p
        case .launchFailed(let s), .failed(let s), .invalidOutput(let s), .aiFailed(let s): s
        case .connection(_, let d): d
        case .incompatibleSchema(let v): "Report schema \(v) is newer than this app understands."
        case .timedOut, .aiNotConfigured: ""
        }
    }

    public var errorDescription: String? { title }

    /// Classifies engine stderr (already redacted) into an actionable problem.
    public static func classify(stderr: String) -> EngineError {
        let s = stderr.lowercased()
        let detail = stderr.trimmingCharacters(in: .whitespacesAndNewlines)
            .replacingOccurrences(of: "pgbot: ", with: "")
        func has(_ needles: String...) -> Bool { needles.contains { s.contains($0) } }
        // SQLSTATE class 28 is invalid authorization: a wrong password (28P01) or a
        // role that doesn't exist / may not log in (28000). Check it before the
        // generic "connect:" prefix below, which every connection error carries.
        if has("password authentication failed", "authentication failed", "no password supplied", "sasl",
               "sqlstate 28000", "sqlstate 28p01") || (has("role \"") && has("does not exist")) {
            return .connection(.authentication, detail: detail)
        }
        if has("ssl", "tls", "certificate", "x509") { return .connection(.ssl, detail: detail) }
        if has("permission denied", "must be superuser", "pg_monitor", "insufficient privilege") {
            return .connection(.permission, detail: detail)
        }
        if has("does not exist") && has("database") { return .connection(.databaseMissing, detail: detail) }
        if has("timeout", "timed out", "deadline exceeded", "i/o timeout") { return .connection(.timeout, detail: detail) }
        if has("connection refused", "no such host", "network is unreachable", "dial tcp", "lookup ", "connect:") {
            return .connection(.unreachable, detail: detail)
        }
        if has("no ai", "api key", "pgbot_ai", "no provider", "ai provider") { return .aiNotConfigured }
        return .failed(detail.isEmpty ? "The engine exited without explaining why." : detail)
    }
}
