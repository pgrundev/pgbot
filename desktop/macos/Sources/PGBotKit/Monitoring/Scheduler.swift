import Foundation

/// Decides WHEN each bot is inspected. It never runs SQL itself; it calls the
/// `perform` closure, which goes through the engine (and its concurrency limit).
///
/// Honest about its limits: it runs only while the app process runs and the Mac
/// is awake. On wake it runs each overdue bot once — no catch-up storm, and no
/// claim of coverage for the time the Mac slept.
public actor InspectionScheduler {
    public enum Outcome: Sendable { case success, failure }

    public struct Entry: Sendable, Equatable {
        public var interval: TimeInterval
        public var nextDue: Date
        public var consecutiveFailures = 0
        public var paused = false
        public var running = false
    }

    public typealias Perform = @Sendable (UUID) async -> Outcome

    private var entries: [UUID: Entry] = [:]
    private var globallyPaused = false
    private var suspendedForSleep = false
    private var loop: Task<Void, Never>?
    private let perform: Perform
    private let clock: @Sendable () -> Date
    private let maxBackoff: TimeInterval

    public init(maxBackoff: TimeInterval = 6 * 3600, clock: @escaping @Sendable () -> Date = { Date() }, perform: @escaping Perform) {
        self.perform = perform; self.clock = clock; self.maxBackoff = maxBackoff
    }

    // MARK: Configuration

    public func register(_ id: UUID, interval: TimeInterval, paused: Bool, firstRunAt: Date? = nil) {
        let now = clock()
        if var e = entries[id] {
            e.interval = interval; e.paused = paused
            e.nextDue = min(e.nextDue, now.addingTimeInterval(interval))
            entries[id] = e
        } else {
            entries[id] = Entry(interval: interval, nextDue: firstRunAt ?? now.addingTimeInterval(interval), paused: paused)
        }
    }

    public func unregister(_ id: UUID) { entries.removeValue(forKey: id) }
    public func setPaused(_ id: UUID, _ paused: Bool) { entries[id]?.paused = paused }
    public func setGloballyPaused(_ p: Bool) { globallyPaused = p }
    public func entry(_ id: UUID) -> Entry? { entries[id] }
    public var isGloballyPaused: Bool { globallyPaused }

    // MARK: Sleep / wake

    public func willSleep() { suspendedForSleep = true }
    public func didWake() { suspendedForSleep = false }

    // MARK: Running

    public func start(tick: Duration = .seconds(20)) {
        loop?.cancel()
        loop = Task { [weak self] in
            while !Task.isCancelled {
                await self?.runDue()
                try? await Task.sleep(for: tick)
            }
        }
    }

    public func stop() { loop?.cancel(); loop = nil }

    /// Bots due now, oldest-due first. Exposed for checks.
    public func dueIDs() -> [UUID] {
        guard !globallyPaused, !suspendedForSleep else { return [] }
        let now = clock()
        return entries.filter { !$0.value.paused && !$0.value.running && $0.value.nextDue <= now }
            .sorted { $0.value.nextDue < $1.value.nextDue }.map(\.key)
    }

    /// Runs everything due, concurrently (the engine bounds actual parallelism).
    public func runDue() async {
        let ids = dueIDs()
        for id in ids { entries[id]?.running = true }
        await withTaskGroup(of: (UUID, Outcome).self) { group in
            for id in ids { group.addTask { [perform] in (id, await perform(id)) } }
            for await (id, outcome) in group { record(id, outcome) }
        }
    }

    /// Run one bot now, outside its schedule ("Run inspection").
    public func runNow(_ id: UUID) async -> Outcome? {
        guard var e = entries[id], !e.running else { return nil }
        e.running = true; entries[id] = e
        let outcome = await perform(id)
        record(id, outcome)
        return outcome
    }

    func record(_ id: UUID, _ outcome: Outcome) {
        guard var e = entries[id] else { return }
        e.running = false
        let now = clock()
        switch outcome {
        case .success:
            e.consecutiveFailures = 0
            e.nextDue = now.addingTimeInterval(e.interval)
        case .failure:
            e.consecutiveFailures += 1
            e.nextDue = now.addingTimeInterval(Self.backoff(failures: e.consecutiveFailures, interval: e.interval, cap: maxBackoff))
        }
        entries[id] = e
    }

    /// Retry delay after a failure: 1 min, 2, 4, 8 … — capped at the bot's own
    /// interval (a failing bot is never retried less often than it would be
    /// inspected when healthy) and at the global cap.
    public static func backoff(failures: Int, interval: TimeInterval, cap: TimeInterval) -> TimeInterval {
        let exp = 60 * pow(2, Double(max(0, failures - 1)))
        return min(exp, max(interval, 60), cap)
    }
}
