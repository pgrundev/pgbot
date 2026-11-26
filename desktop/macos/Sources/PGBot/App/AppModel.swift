import AppKit
import Network
import Observation
import PGBotKit
import ServiceManagement
import SwiftUI
import UserNotifications

/// The app's single source of truth. Views read it; every side effect (engine,
/// Keychain, store, notifications) goes through it.
@MainActor @Observable
final class AppModel {
    // MARK: State

    var auth: AuthPhase = .restoring
    var bots: [DatabaseBot] = []
    var selection: UUID? {
        didSet {
            guard let s = selection else { return }
            markBotRead(s)
            if !demoMode && scenario == nil { try? realStore.setValue(s, for: "lastSelection") }
        }
    }
    var route: DetailRoute = .chat
    var conversations: [UUID: [ChatMessage]] = [:]
    var latest: [UUID: InspectionRecord] = [:]
    var previous: [UUID: InspectionRecord] = [:]
    var inspecting: Set<UUID> = []
    var generating: [UUID: Task<Void, Never>] = [:]
    var notifications: [AppNotification] = []
    var settings = AppSettings() { didSet { if settings != oldValue { persistSettings(oldValue) } } }
    var engineVersion: String?
    var engineProblem: EngineError?
    var demoMode = false
    var isOffline = false
    var scrollTarget: UUID?

    // Sheets / panels
    var showingAddDatabase = false
    var showingSearch = false
    var showingNotifications = false
    var settingsForBot: DatabaseBot?
    var pendingConsent: PendingConsent?
    var explainSQLDraft: String?

    // PSQL mode (per database): the composer becomes a read-only psql console.
    var psqlBots: Set<UUID> = []
    var psqlExpanded = false
    var psqlHistory: [UUID: [String]] = [:]

    struct PendingConsent: Identifiable { let id = UUID(); let botID: UUID; let question: String; let ai: AIConfiguration }

    // MARK: Services

    private(set) var store: Store
    private let realStore: Store
    let secrets: SecretStore
    let authService: AuthService
    private(set) var engine: DiagnosticEngine?
    private var scheduler: InspectionScheduler!
    private var dailyTimer: Timer?
    private var demoBundle: DemoData.Bundle?
    private var observers: [NSObjectProtocol] = []

    /// Launch scenarios for screenshots and UI tests (`-PGBotScenario demo|local`).
    /// They never persist a session or demo flag.
    let scenario = UserDefaults.standard.string(forKey: "PGBotScenario")
    static let env = ProcessInfo.processInfo.environment

    init() {
        // PGBOT_DATA_DIR isolates the store and Keychain service (development, UI tests).
        let isolated = Self.env["PGBOT_DATA_DIR"].map { URL(fileURLWithPath: $0, isDirectory: true) }
        let support = isolated ?? FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("PGBot", isDirectory: true)
        let store: Store
        do { store = try Store(url: support.appendingPathComponent("pgbot.sqlite")) }
        catch { store = try! Store(url: nil) } // degraded: in-memory, surfaced via engineProblem-like banner below
        self.realStore = store
        self.store = store
        self.secrets = KeychainStore(service: isolated == nil ? "dev.pgbot.desktop" : "dev.pgbot.desktop.isolated")
        self.authService = AuthService(config: .fromBundle(), secrets: secrets)
        self.settings = AppSettings.load(store.rawValue(for: "settings"))
        Theme.apply(settings)
        self.demoMode = store.value(Bool.self, for: "demoMode") ?? false
        configureEngine(stateDirectory: support.appendingPathComponent("engine-state", isDirectory: true))
        scheduler = InspectionScheduler { [weak self] id in
            guard let self else { return .failure }
            return await self.scheduledInspection(id)
        }
        observeSystem()
    }

    // MARK: Startup

    func start() async {
        if let scenario {
            auth = .signedIn(AuthenticationSession(user: User(id: "scenario", name: "Alex Example", email: nil, avatarURL: nil, provider: .local),
                                                   accessToken: "", refreshToken: nil, expiresAt: .distantFuture, subscription: .init()))
            demoMode = scenario == "demo"
            await loadWorkspace()
            if let route = UserDefaults.standard.string(forKey: "PGBotRoute") {
                self.route = route == "metrics" ? .metrics : (route == "logs" ? .logs : .chat)
            }
            if let raw = UserDefaults.standard.string(forKey: "PGBotSelect"), let i = Int(raw), bots.indices.contains(i) { selection = bots[i].id }
            if let a = UserDefaults.standard.string(forKey: "PGBotAppearance"), let ap = AppSettings.Appearance(rawValue: a) { settings.appearance = ap }
            // `-PGBotSettings '{…}'` arrives as a string or, parsed by NSArgumentDomain, as a dictionary.
            switch UserDefaults.standard.object(forKey: "PGBotSettings") {
            case let json as String: settings = settings.overlaid(with: Data(json.utf8))
            case let dict as [String: Any]:
                if let data = try? JSONSerialization.data(withJSONObject: dict) { settings = settings.overlaid(with: data) }
            default: break
            }
            // `-PGBotPSQL 'cmd ||| cmd'`: open PSQL mode on the selected database and run each command.
            if let script = UserDefaults.standard.string(forKey: "PGBotPSQL") {
                Task {
                    try? await Task.sleep(for: .seconds(1.5))
                    guard let id = self.selection else { return }
                    self.setPSQLMode(true, for: id)
                    for cmd in script.components(separatedBy: "|||").map({ $0.trimmingCharacters(in: .whitespaces) }) where !cmd.isEmpty {
                        while self.generating[id] != nil || self.inspecting.contains(id) { try? await Task.sleep(for: .milliseconds(200)) }
                        self.runPSQL(cmd, on: id)
                        try? await Task.sleep(for: .milliseconds(300))
                    }
                }
            }
            if UserDefaults.standard.bool(forKey: "PGBotOpenSettings") {
                // After any theme-driven rebuild of the root view has settled.
                DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) {
                    openAppSettings()
                }
            }
            if scenario == "local", let dsn = Self.env["PGBOT_SCENARIO_DSN"], let p = try? ConnectionString.parse(dsn),
               !bots.contains(where: { $0.connection.instanceKey == p.connection.instanceKey && $0.connection.database == p.connection.database }) {
                addBot(DatabaseBot(displayName: "Local Postgres", description: "Development database",
                                   avatar: BotAvatar(color: .purple), environment: .local, connection: p.connection, pinned: true),
                       password: p.password, firstInspection: nil)
            }
            return
        }
        do {
            if let s = try await authService.restore() { auth = .signedIn(s) } else { auth = .signedOut(error: nil) }
        } catch {
            auth = .signedOut(error: (error as? LocalizedError)?.errorDescription ?? error.localizedDescription)
        }
        if case .signedIn = auth { await loadWorkspace() }
    }

    func loadWorkspace() async {
        if demoMode { enterDemo() } else { loadReal() }
        if let engine, engineVersion == nil {
            do { engineVersion = try await engine.version() } catch { engineProblem = error as? EngineError ?? .failed(error.localizedDescription) }
        }
        await scheduler.start()
        startDailyTimer()
        if settings.notificationsEnabled { requestNotificationPermission() }
        updateDockBadge()
    }

    private func loadReal() {
        store = realStore
        bots = (try? store.bots()) ?? []
        for b in bots {
            conversations[b.id] = (try? store.messages(for: b.id)) ?? []
            psqlHistory[b.id] = store.value([String].self, for: "psqlHistory.\(b.id)") ?? []
            let recent = (try? store.inspections(for: b.id, limit: 2)) ?? []
            latest[b.id] = recent.first
            previous[b.id] = recent.dropFirst().first
        }
        notifications = (try? store.notifications()) ?? []
        if selection == nil || !bots.contains(where: { $0.id == selection }) {
            let last = settings.restoreLastSelection ? store.value(UUID.self, for: "lastSelection") : nil
            selection = bots.first { $0.id == last }?.id ?? bots.first?.id
        }
        Task { for b in bots { await registerSchedule(b) } }
    }

    private func configureEngine(stateDirectory: URL) {
        if let bin = EngineLocator.locate(override: settings.enginePathOverride) {
            engine = PGBotEngine(runner: EngineRunner(binary: bin, stateDirectory: stateDirectory, maxConcurrent: 3))
            engineProblem = nil
        } else {
            engine = nil
            engineProblem = .engineMissing("The pgbot engine wasn't found in the app bundle or on this Mac.")
        }
    }

    // MARK: Authentication

    var session: AuthenticationSession? { if case .signedIn(let s) = auth { return s }; return nil }

    func signIn(with provider: AuthProvider, using authenticator: WebAuthenticator) async {
        auth = .signingIn(provider)
        do {
            let req = try authService.makeRequest(for: provider)
            let callback = try await authenticator.authenticate(url: req.url, callbackScheme: authService.config.callbackScheme)
            let session = try await authService.complete(req, callback: callback)
            auth = .signedIn(session)
            await loadWorkspace()
        } catch AuthError.cancelled {
            auth = .signedOut(error: nil)
        } catch {
            auth = .signedOut(error: (error as? LocalizedError)?.errorDescription ?? error.localizedDescription)
        }
    }

    func continueLocally() async {
        let name = NSFullUserName().isEmpty ? "Local user" : NSFullUserName()
        if let s = try? authService.continueLocally(name: name) { auth = .signedIn(s); await loadWorkspace() }
    }

    func signOut() async {
        try? authService.signOut()
        await scheduler.stop()
        for t in generating.values { t.cancel() }
        generating = [:]
        bots = []; conversations = [:]; latest = [:]; previous = [:]; notifications = []; selection = nil
        auth = .signedOut(error: nil)
    }

    // MARK: Demo mode

    func setDemoMode(_ on: Bool) {
        demoMode = on
        try? realStore.setValue(on, for: "demoMode")
        conversations = [:]; latest = [:]; previous = [:]; selection = nil
        if on { enterDemo() } else { loadReal() }
    }

    private func enterDemo() {
        // Demo data lives in its own in-memory store: it can never mix with real history.
        let demoStore = try! Store(url: nil)
        let d = DemoData.make()
        for b in d.bots { try? demoStore.saveBot(b) }
        for r in d.records { try? demoStore.save(r) }
        for m in d.messages { try? demoStore.append(m) }
        for n in d.notifications { try? demoStore.save(n) }
        try? demoStore.insertSamples(d.samples)
        store = demoStore
        demoBundle = d
        bots = d.bots
        for b in bots {
            conversations[b.id] = (try? demoStore.messages(for: b.id)) ?? []
            latest[b.id] = try? demoStore.latestInspection(for: b.id)
        }
        notifications = d.notifications
        selection = bots.first?.id
    }

    func demoLogs(for id: UUID) -> [LogEntry]? { demoBundle?.logs[id] }

    // MARK: Bots

    var selectedBot: DatabaseBot? { bots.first { $0.id == selection } }
    func bot(_ id: UUID) -> DatabaseBot? { bots.first { $0.id == id } }

    func update(_ bot: DatabaseBot) {
        guard let i = bots.firstIndex(where: { $0.id == bot.id }) else { return }
        bots[i] = bot
        try? store.saveBot(bot)
        Task { await registerSchedule(bot) }
    }

    func mutate(_ id: UUID, _ change: (inout DatabaseBot) -> Void) {
        guard var b = bot(id) else { return }
        change(&b)
        update(b)
    }

    /// Adds a bot, stores its password in the Keychain, and opens its conversation.
    /// When the connection test already produced an inspection, it is reused as
    /// the first report instead of inspecting twice.
    func addBot(_ draft: DatabaseBot, password: String?, firstInspection: EngineContext?) {
        var bot = draft
        bot.sortOrder = (bots.map(\.sortOrder).max() ?? -1) + 1
        bot.isDemo = false
        if let p = password, !p.isEmpty { try? secrets.set(p, for: bot.keychainAccount) }
        bots.append(bot)
        try? store.saveBot(bot)
        selection = bot.id
        route = .chat
        post(ChatMessage(botID: bot.id, role: .system, payload: .connectionEvent("Connected to \(bot.connection.host) as \(bot.connection.username)", ok: true)))
        Task {
            await registerSchedule(bot)
            if let ctx = firstInspection {
                let now = Date()
                record(bot.id, InspectionRecord(botID: bot.id, startedAt: now, finishedAt: now, context: ctx), reason: .first)
            } else {
                await inspect(bot.id, reason: .first)
            }
        }
    }

    func remove(_ id: UUID) {
        guard let b = bot(id) else { return }
        try? secrets.delete(b.keychainAccount)
        try? store.deleteBot(id)
        bots.removeAll { $0.id == id }
        conversations[id] = nil; latest[id] = nil; previous[id] = nil
        notifications.removeAll { $0.botID == id }
        generating[id]?.cancel(); generating[id] = nil
        Task { await scheduler.unregister(id) }
        if selection == id { selection = bots.first?.id }
    }

    func move(from source: IndexSet, to destination: Int, within ids: [UUID]) {
        var ordered = ids
        ordered.move(fromOffsets: source, toOffset: destination)
        for (i, id) in ordered.enumerated() { mutate(id) { $0.sortOrder = i } }
        bots.sort { $0.sortOrder < $1.sortOrder }
    }

    func password(for bot: DatabaseBot) -> String? { try? secrets.get(bot.keychainAccount) }

    func setPassword(_ p: String, for bot: DatabaseBot) { try? secrets.set(p, for: bot.keychainAccount) }

    func target(for bot: DatabaseBot) -> EngineTarget {
        EngineTarget(connection: bot.connection, password: password(for: bot))
    }

    var groups: [(name: String, bots: [DatabaseBot])] {
        let order = ["Production", "Staging", "Development", "Local", "Custom"]
        let grouped = Dictionary(grouping: bots, by: \.groupName)
        return grouped.keys.sorted { (order.firstIndex(of: $0) ?? 99, $0) < (order.firstIndex(of: $1) ?? 99, $1) }
            .map { ($0, grouped[$0]!.sorted { $0.sortOrder < $1.sortOrder }) }
    }

    // MARK: Inspection

    enum InspectionReason { case first, manual, scheduled, daily }

    func registerSchedule(_ bot: DatabaseBot) async {
        guard !bot.isDemo else { return }
        let paused = !bot.schedule.enabled || !settings.backgroundMonitoring
        await scheduler.register(bot.id, interval: TimeInterval(bot.schedule.intervalSeconds), paused: paused)
    }

    var monitoringPaused: Bool { !settings.backgroundMonitoring }

    func setMonitoringPaused(_ paused: Bool) {
        settings.backgroundMonitoring = !paused
        Task {
            await scheduler.setGloballyPaused(paused)
            for b in bots { await registerSchedule(b) }
        }
    }

    func runAllInspections() { for b in bots where !b.isDemo { Task { await inspect(b.id, reason: .manual) } } }

    private func scheduledInspection(_ id: UUID) async -> InspectionScheduler.Outcome {
        await inspect(id, reason: .scheduled) ? .success : .failure
    }

    /// Runs the engine for one bot. Returns false on failure.
    @discardableResult
    func inspect(_ id: UUID, reason: InspectionReason) async -> Bool {
        guard let bot = bot(id) else { return false }
        if bot.isDemo {
            if reason == .manual, let rec = latest[id] { post(ReportComposer.healthReport(bot: bot, record: rec, previous: nil, firstRun: false, scheduled: false)) }
            return true
        }
        guard let engine else {
            post(errorMessage(id, engineProblem ?? .engineMissing(""), retry: nil))
            return false
        }
        guard !inspecting.contains(id) else { return true }
        inspecting.insert(id)
        mutate(id) { $0.status = .connecting }
        defer { inspecting.remove(id) }
        let started = Date()
        do {
            let ctx = try await engine.inspect(target(for: bot))
            isOffline = false
            record(id, InspectionRecord(botID: id, startedAt: started, finishedAt: Date(), context: ctx), reason: reason)
            return true
        } catch {
            let e = error as? EngineError ?? .failed(error.localizedDescription)
            if case .connection(.unreachable, _) = e, !NetworkStatus.shared.isReachable { isOffline = true }
            mutate(id) { $0.status = .unreachable; $0.lastSummary = e.title }
            var ledger = ledgerFor(id)
            if let n = NotificationPlanner.planFailure(botName: bot.displayName, error: e, ledger: &ledger, prefs: prefs(for: bot)) {
                deliver(n, bot: bot, messageID: nil)
            }
            saveLedger(ledger, for: id)
            // Only interrupt the conversation for failures the user asked for, or the first of an outage.
            if reason != .scheduled || ledger.connectivityDown { post(errorMessage(id, e, retry: .healthCheck)) }
            return false
        }
    }

    /// Stores a finished inspection and turns it into messages and notifications.
    func record(_ id: UUID, _ rec: InspectionRecord, reason: InspectionReason) {
        guard let bot = bot(id) else { return }
        let prev = latest[id]
        try? store.save(rec)
        try? store.prune(botID: id, olderThan: Date().addingTimeInterval(-Double(bot.schedule.retentionDays) * 86400))
        previous[id] = prev
        latest[id] = rec
        let changes = prev.map { FindingChanges.compare(previous: $0.context.findings, current: rec.context.findings, ignoring: bot.schedule.ignoredFindingKeys) }

        var reportMessage: ChatMessage?
        switch reason {
        case .first, .manual, .daily:
            let m = ReportComposer.healthReport(bot: bot, record: rec, previous: prev, firstRun: reason == .first, scheduled: reason == .daily)
            post(m); reportMessage = m
        case .scheduled:
            for m in ReportComposer.proactive(bot: bot, changes: changes ?? .init(), at: rec.finishedAt) { post(m); reportMessage = reportMessage ?? m }
        }

        var ledger = ledgerFor(id)
        let planned = NotificationPlanner.plan(botName: bot.displayName,
                                               changes: changes ?? FindingChanges.compare(previous: [], current: rec.context.findings, ignoring: bot.schedule.ignoredFindingKeys),
                                               scoreBefore: prev?.score.value, scoreAfter: rec.score, ledger: &ledger, prefs: prefs(for: bot))
        if reason == .daily {
            ledger.lastDailyReport = Date()
            if !settings.disabledCategories.contains(.scheduledReport) {
                deliver(PlannedNotification(category: .scheduledReport, severity: .info, title: "\(bot.displayName) — daily report",
                                            body: "Health score \(rec.score.value)/100. \(rec.score.critical) critical, \(rec.score.warnings) warnings.",
                                            dedupKey: "daily|\(Calendar.current.startOfDay(for: Date()).timeIntervalSince1970)"),
                        bot: bot, messageID: reportMessage?.id)
            }
        }
        // The first report is in front of the user already; don't also ping them.
        if reason != .first { for n in planned { deliver(n, bot: bot, messageID: reportMessage?.id) } }
        saveLedger(ledger, for: id)

        let live = rec.context.findings.filter { !$0.isSuppressed && !bot.schedule.ignoredFindingKeys.contains($0.key) }
        mutate(id) {
            $0.lastInspectedAt = rec.finishedAt
            $0.lastScore = rec.score.completeness == .unavailable ? nil : rec.score.value
            $0.lastSummary = ReportComposer.sidebarSummary(record: rec, changes: changes)
            $0.status = live.contains { $0.severity == .critical } ? .critical : (live.contains { $0.severity == .warn } ? .degraded : .healthy)
            if !$0.schedule.enabled { $0.status = .paused }
        }
    }

    private func startDailyTimer() {
        dailyTimer?.invalidate()
        dailyTimer = Timer.scheduledTimer(withTimeInterval: 60, repeats: true) { [weak self] _ in
            let model = self
            Task { @MainActor in model?.runDueDailyReports() }
        }
    }

    func runDueDailyReports(now: Date = Date()) {
        guard settings.backgroundMonitoring else { return }
        let cal = Calendar.current
        for b in bots where !b.isDemo && b.schedule.enabled {
            guard let hour = b.schedule.dailyReportHour, cal.component(.hour, from: now) >= hour else { continue }
            let ledger = ledgerFor(b.id)
            if let last = ledger.lastDailyReport, cal.isDate(last, inSameDayAs: now) { continue }
            Task { await inspect(b.id, reason: .daily) }
        }
    }

    // MARK: Conversation

    func messages(for id: UUID) -> [ChatMessage] { conversations[id] ?? [] }

    func post(_ m: ChatMessage) {
        conversations[m.botID, default: []].append(m)
        try? store.append(m)
        if m.botID != selection, m.role == .assistant { mutate(m.botID) { $0.unreadCount += 1 } }
        scrollTarget = m.id
    }

    func deleteMessage(_ m: ChatMessage) {
        conversations[m.botID]?.removeAll { $0.id == m.id }
        try? store.deleteMessage(m.id)
    }

    func clearConversation(_ id: UUID) {
        try? store.clearConversation(id)
        conversations[id] = []
    }

    func markBotRead(_ id: UUID) {
        if bot(id)?.unreadCount ?? 0 > 0 { mutate(id) { $0.unreadCount = 0 } }
    }

    var isGenerating: Bool { selection.map { generating[$0] != nil } ?? false }

    /// A question typed into the composer.
    func send(_ text: String, to id: UUID) {
        let q = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !q.isEmpty, bot(id) != nil else { return }
        let userMessage = ChatMessage(botID: id, role: .user, payload: .text(q))
        post(userMessage)
        answer(userMessage)
    }

    /// Answers through `pgbot ask`, which grounds the model on the engine's
    /// deterministic findings and nothing else.
    func answer(_ question: ChatMessage, consentGranted: Bool = false) {
        guard case .text(let q) = question.payload, let bot = bot(question.botID) else { return }
        let id = bot.id
        if bot.isDemo {
            post(ChatMessage(botID: id, role: .assistant, payload: .text(
                "This is a demo database, so there's no live engine to ask. Connect a real database to ask questions grounded on its own findings."),
                inReplyTo: question.id))
            return
        }
        let ai = settings.ai
        let key = ai.provider.needsKey ? (try? secrets.get(SecretAccount.aiKey(ai.provider))) ?? nil : nil
        if ai.provider.needsKey && (key ?? "").isEmpty {
            post(ChatMessage(botID: id, role: .assistant, payload: .error(
                title: "No AI provider configured",
                detail: "Natural-language answers use your own AI provider. Deterministic diagnostics (the + menu) work without one.",
                hint: "Add an API key in Settings → AI providers, or choose Ollama to keep everything on this Mac."), inReplyTo: question.id))
            return
        }
        if !ai.isLocal && !settings.aiConsents.contains(settings.consentKey(for: ai)) && !consentGranted {
            pendingConsent = PendingConsent(botID: id, question: question.id.uuidString, ai: ai)
            return
        }
        guard let engine else { post(errorMessage(id, engineProblem ?? .engineMissing(""), retry: nil)); return }
        let target = target(for: bot)
        generating[id] = Task {
            defer { generating[id] = nil }
            do {
                let text = try await engine.ask(target, question: q, ai: ai, apiKey: key)
                guard !Task.isCancelled else { return }
                post(ChatMessage(botID: id, role: .assistant, payload: .text(text), inReplyTo: question.id,
                                 modelName: ai.model.isEmpty ? ai.provider.label : ai.model))
            } catch is CancellationError {
                post(ChatMessage(botID: id, role: .system, payload: .monitoringEvent("Stopped."), inReplyTo: question.id))
            } catch {
                guard !Task.isCancelled else {
                    post(ChatMessage(botID: id, role: .system, payload: .monitoringEvent("Stopped."), inReplyTo: question.id)); return
                }
                let e = error as? EngineError ?? .aiFailed(error.localizedDescription)
                post(ChatMessage(botID: id, role: .assistant, payload: .error(title: e.title, detail: e.detail, hint: e.hint), inReplyTo: question.id))
            }
        }
    }

    func grantConsent(_ c: PendingConsent) {
        settings.aiConsents.insert(settings.consentKey(for: c.ai))
        pendingConsent = nil
        if let m = messages(for: c.botID).first(where: { $0.id.uuidString == c.question }) { answer(m, consentGranted: true) }
    }

    func stopGenerating(_ id: UUID) { generating[id]?.cancel() }

    func regenerate(_ answerMessage: ChatMessage) {
        guard let qid = answerMessage.inReplyTo, let q = messages(for: answerMessage.botID).first(where: { $0.id == qid }) else { return }
        answer(q)
    }

    /// Plus-menu actions: deterministic engine output, no AI involved.
    func run(_ action: QuickAction, on id: UUID) {
        guard let bot = bot(id) else { return }
        post(ChatMessage(botID: id, role: .user, payload: .text(action.label)))
        let ctx = latest[id]?.context
        func needInspection() -> Bool {
            if ctx == nil { Task { await inspect(id, reason: .manual) }; return true }
            return false
        }
        switch action {
        case .healthCheck:
            Task { await inspect(id, reason: .manual) }
        case .queries:
            if needInspection() { return }
            post(.init(botID: id, role: .assistant, payload: ReportComposer.queriesTable(ctx!, title: "Most expensive queries by total time")))
        case .indexes:
            if needInspection() { return }
            post(.init(botID: id, role: .assistant, payload: ReportComposer.indexesTable(ctx!)))
            let ixFindings = ctx!.findings.filter { $0.id.contains("index") && !$0.isSuppressed }
            if !ixFindings.isEmpty { post(.init(botID: id, role: .assistant, payload: .findingsList(title: "Index findings", findings: ixFindings))) }
        case .locks:
            if needInspection() { return }
            post(.init(botID: id, role: .assistant, payload: ReportComposer.locksTable(ctx!)))
        case .replication:
            if needInspection() { return }
            post(.init(botID: id, role: .assistant, payload: ReportComposer.replicationTable(ctx!)))
        case .connections:
            if bot.isDemo { post(.init(botID: id, role: .assistant, payload: .text("Demo mode: live sessions aren't available for sample databases."))); return }
            engineCall(id, retry: action) { e, t in .init(botID: id, role: .assistant, payload: ReportComposer.sessionsTable(try await e.activity(t))) }
        case .logs:
            route = .logs
        case .explainSQL:
            explainSQLDraft = ""
        case .schema:
            if needInspection() { return }
            let tables = ctx!.tables?.top ?? []
            post(.init(botID: id, role: .assistant, payload: tables.isEmpty ? .text("No user tables were found.")
                : .table(title: "Largest tables", columns: ["Table", "Size", "Rows", "Dead"],
                         rows: tables.map { [$0.id, Format.bytes($0.totalBytes), Format.count($0.liveTuples), Format.percent($0.deadRatio)] })))
        case .report:
            if needInspection() { return }
            let rec = latest[id]!
            post(ReportComposer.healthReport(bot: bot, record: rec, previous: previous[id], firstRun: false, scheduled: false))
        case .whatChanged:
            if bot.isDemo { post(.init(botID: id, role: .assistant, payload: .text("Demo mode: “what changed” needs real baselines from repeated inspections."))); return }
            engineCall(id, retry: action) { e, t in .init(botID: id, role: .assistant, payload: .plan(sql: "", reading: try await e.diff(t, since: "24h"))) }
        }
    }

    // MARK: PSQL mode

    func isPSQL(_ id: UUID) -> Bool { psqlBots.contains(id) }

    func setPSQLMode(_ on: Bool, for id: UUID) {
        guard on != isPSQL(id) else { return }
        if on {
            psqlBots.insert(id)
            post(ChatMessage(botID: id, role: .system, payload: .monitoringEvent("PSQL mode · read-only — type SQL ending with ; or \\? for help")))
        } else {
            psqlBots.remove(id)
            post(ChatMessage(botID: id, role: .system, payload: .monitoringEvent("Back to chat")))
        }
    }

    /// One line of console input: SQL or a psql meta-command. Everything that
    /// reaches the database runs through `pgbot sql run` (READ ONLY, rolled back).
    func runPSQL(_ raw: String, on id: UUID) {
        let input = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !input.isEmpty, let bot = bot(id) else { return }
        var history = psqlHistory[id] ?? []
        if history.last != input { history.append(input) }
        psqlHistory[id] = Array(history.suffix(200))
        if !bot.isDemo { try? store.setValue(psqlHistory[id], for: "psqlHistory.\(id)") }
        post(ChatMessage(botID: id, role: .user, payload: .psqlInput(input)))

        func notice(_ message: String, refusal: Bool = true) {
            post(ChatMessage(botID: id, role: .assistant, payload: .psqlResult(PSQLResultPayload(
                input: input, title: nil,
                output: SQLRunOutput(results: [], error: SQLRunError(message: message, sqlstate: refusal ? "PGBOT" : nil), elapsedMS: 0),
                expanded: psqlExpanded))))
        }

        let command = PSQL.parse(input)
        switch command {
        case .help:
            post(ChatMessage(botID: id, role: .assistant, payload: .text(PSQL.helpText)))
            return
        case .quit:
            setPSQLMode(false, for: id)
            return
        case .toggleExpanded(let v):
            psqlExpanded = v ?? !psqlExpanded
            post(ChatMessage(botID: id, role: .system, payload: .monitoringEvent("Expanded display is \(psqlExpanded ? "on" : "off")")))
            return
        case .unsupported(_, let reason):
            notice(reason)
            return
        case .sql, .meta:
            break
        }
        let (sql, title): (String, String?) = {
            if case .meta(let s, let t) = command { return (s, t) }
            if case .sql(let s) = command { return (s, nil) }
            return ("", nil)
        }()
        guard !bot.isDemo else {
            notice("PSQL mode needs a live database — demo databases are sample data with no server behind them.")
            return
        }
        guard let engine else { post(errorMessage(id, engineProblem ?? .engineMissing(""), retry: nil)); return }
        let target = target(for: bot)
        let expanded = psqlExpanded
        generating[id] = Task {
            defer { generating[id] = nil }
            do {
                let out = try await engine.runSQL(target, sql: sql, maxRows: 500)
                guard !Task.isCancelled else { return }
                post(ChatMessage(botID: id, role: .assistant, payload: .psqlResult(
                    PSQLResultPayload(input: input, title: title, output: out, expanded: expanded))))
            } catch is CancellationError {
                post(ChatMessage(botID: id, role: .system, payload: .monitoringEvent("Query cancelled.")))
            } catch {
                post(errorMessage(id, error as? EngineError ?? .failed(error.localizedDescription), retry: nil))
            }
        }
    }

    func explain(sql: String, on id: UUID) {
        explainSQLDraft = nil
        let s = sql.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !s.isEmpty else { return }
        post(ChatMessage(botID: id, role: .user, payload: .sql(title: "Explain this query", sql: s)))
        guard bot(id)?.isDemo == false else {
            post(.init(botID: id, role: .assistant, payload: .text("Demo mode: plans need a live database."))); return
        }
        engineCall(id, retry: nil) { e, t in .init(botID: id, role: .assistant, payload: .plan(sql: s, reading: try await e.explainSQL(t, sql: s))) }
    }

    /// Hand a log line to the assistant, redacted first.
    func explainLog(_ entry: LogEntry, botID: UUID) {
        route = .chat
        let redacted = Redactor.redactSQLLiterals([entry.severity + ": " + entry.message, entry.detail ?? ""].joined(separator: "\n"))
        send("""
        Explain this PostgreSQL log entry: what happened, likely causes, the supporting evidence from your findings, \
        recommended actions, and whether more investigation is needed.

        \(redacted)
        """, to: botID)
    }

    private func engineCall(_ id: UUID, retry: QuickAction?, _ body: @escaping (DiagnosticEngine, EngineTarget) async throws -> ChatMessage) {
        guard let bot = bot(id), let engine else { post(errorMessage(id, engineProblem ?? .engineMissing(""), retry: nil)); return }
        let t = target(for: bot)
        generating[id] = Task {
            defer { generating[id] = nil }
            do { let m = try await body(engine, t); if !Task.isCancelled { post(m) } }
            catch is CancellationError {}
            catch { post(errorMessage(id, error as? EngineError ?? .failed(error.localizedDescription), retry: retry)) }
        }
    }

    private func errorMessage(_ id: UUID, _ e: EngineError, retry: QuickAction?) -> ChatMessage {
        ChatMessage(botID: id, role: .assistant, payload: .error(title: e.title, detail: e.detail, hint: e.hint))
    }

    // MARK: Notifications

    var unreadNotifications: Int { notifications.filter { !$0.read }.count }

    private func prefs(for bot: DatabaseBot) -> NotificationPlanner.Preferences {
        .init(minimum: max(bot.schedule.notifyMinimum, settings.minimumSeverity),
              notifyResolved: bot.schedule.notifyResolved, disabled: settings.disabledCategories)
    }

    private func ledgerFor(_ id: UUID) -> AnnouncementLedger { store.value(AnnouncementLedger.self, for: "ledger.\(id)") ?? AnnouncementLedger() }
    private func saveLedger(_ l: AnnouncementLedger, for id: UUID) { try? store.setValue(l, for: "ledger.\(id)") }

    private func deliver(_ p: PlannedNotification, bot: DatabaseBot, messageID: UUID?) {
        let n = AppNotification(botID: bot.id, category: p.category, severity: p.severity, title: p.title, body: p.body,
                                messageID: messageID, dedupKey: p.dedupKey)
        notifications.insert(n, at: 0)
        try? store.save(n)
        updateDockBadge()
        // The in-app center always records it; the macOS banner respects the switches,
        // the database's mute, and quiet hours (which critical alerts may break through).
        guard settings.notificationsEnabled, settings.systemBanners, !bot.muted else { return }
        let urgent = n.severity == .critical && settings.criticalBreaksQuietHours
        if settings.quietHours.contains(Date()) && !urgent { return }
        SystemNotifications.post(n, botName: bot.displayName, sound: settings.notificationSounds, preview: settings.showPreviews)
    }

    /// Unread count on the Dock icon (Settings → Notifications → Badge).
    func updateDockBadge() {
        let unread = unreadNotifications
        NSApp.dockTile.badgeLabel = settings.dockBadge && settings.notificationsEnabled && unread > 0 ? "\(unread)" : nil
    }

    func markRead(_ n: AppNotification) {
        guard let i = notifications.firstIndex(where: { $0.id == n.id }) else { return }
        notifications[i].read = true
        try? store.save(notifications[i])
        updateDockBadge()
    }

    func markAllRead() {
        for i in notifications.indices where !notifications[i].read { notifications[i].read = true; try? store.save(notifications[i]) }
        updateDockBadge()
    }

    func open(_ n: AppNotification) {
        markRead(n)
        selection = n.botID
        route = .chat
        showingNotifications = false
        scrollTarget = n.messageID
    }

    func open(notificationID: String) {
        if let n = notifications.first(where: { $0.id.uuidString == notificationID }) { open(n) }
    }

    func requestNotificationPermission() { SystemNotifications.requestAuthorization() }

    /// "Send test": a real system notification, so the user can check banners,
    /// sounds and Focus settings. Returns a reason when it can't be delivered.
    @discardableResult
    func sendTestNotification() -> String? {
        guard SystemNotifications.available else { return "System notifications need the bundled PGBot.app." }
        guard settings.notificationsEnabled else { return "Notifications are turned off in PGBot." }
        SystemNotifications.requestAuthorization()
        SystemNotifications.postTest(sound: settings.notificationSounds)
        return nil
    }

    // MARK: Settings & system

    private func persistSettings(_ old: AppSettings) {
        Theme.apply(settings)
        if scenario == nil { try? realStore.setValue(settings, for: "settings") }
        if old.enginePathOverride != settings.enginePathOverride {
            let support = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0].appendingPathComponent("PGBot")
            configureEngine(stateDirectory: support.appendingPathComponent("engine-state"))
        }
        if old.launchAtLogin != settings.launchAtLogin { LoginItem.set(settings.launchAtLogin) }
        if old.dockBadge != settings.dockBadge || old.notificationsEnabled != settings.notificationsEnabled { updateDockBadge() }
        if old.backgroundMonitoring != settings.backgroundMonitoring {
            Task { await scheduler.setGloballyPaused(!settings.backgroundMonitoring) }
        }
    }

    func saveAPIKey(_ key: String, for provider: AIProviderKind) {
        if key.isEmpty { try? secrets.delete(SecretAccount.aiKey(provider)) } else { try? secrets.set(key, for: SecretAccount.aiKey(provider)) }
    }

    func hasAPIKey(_ provider: AIProviderKind) -> Bool { ((try? secrets.get(SecretAccount.aiKey(provider))) ?? nil)?.isEmpty == false }

    func deleteAllLocalData() {
        for b in bots { try? secrets.delete(b.keychainAccount) }
        for p in AIProviderKind.allCases { try? secrets.delete(SecretAccount.aiKey(p)) }
        try? realStore.wipe()
        settings = AppSettings()
        bots = []; conversations = [:]; latest = [:]; previous = [:]; notifications = []; selection = nil
    }

    private func observeSystem() {
        // Capture the scheduler actor, not self: these run on the notification queue.
        let scheduler: InspectionScheduler = self.scheduler
        let ws = NSWorkspace.shared.notificationCenter
        observers.append(ws.addObserver(forName: NSWorkspace.willSleepNotification, object: nil, queue: .main) { _ in
            Task { await scheduler.willSleep() }
        })
        observers.append(ws.addObserver(forName: NSWorkspace.didWakeNotification, object: nil, queue: .main) { _ in
            Task { await scheduler.didWake() }
        })
    }

    // MARK: Export

    func exportConversation(_ id: UUID) {
        guard let bot = bot(id) else { return }
        let f = DateFormatter(); f.dateStyle = .medium; f.timeStyle = .short
        let text = messages(for: id).map { "[\(f.string(from: $0.createdAt))] \($0.role == .user ? "You" : "PGBot"): \($0.plainText)" }
            .joined(separator: "\n\n")
        FileExport.save(Data(text.utf8), suggestedName: "\(bot.displayName) conversation.txt")
    }

    func exportInspection(_ id: UUID) {
        guard let bot = bot(id), let rec = latest[id], let data = try? EngineJSON.encoder().encode(rec.context) else { return }
        FileExport.save(data, suggestedName: "\(bot.displayName) inspection.json")
    }
}

// MARK: - Platform helpers

enum FileExport {
    @MainActor static func save(_ data: Data, suggestedName: String) {
        let panel = NSSavePanel()
        panel.nameFieldStringValue = suggestedName
        panel.canCreateDirectories = true
        if panel.runModal() == .OK, let url = panel.url { try? data.write(to: url, options: .atomic) }
    }
}

enum LoginItem {
    static func set(_ on: Bool) {
        guard Bundle.main.bundleIdentifier != nil else { return }
        do { if on { try SMAppService.mainApp.register() } else { try SMAppService.mainApp.unregister() } } catch {}
    }
}

/// Network path monitoring, used to tell "you're offline" apart from "the database is down".
final class NetworkStatus: @unchecked Sendable {
    static let shared = NetworkStatus()
    private let monitor = NWPathMonitor()
    private let lock = NSLock()
    private var satisfied = true
    var isReachable: Bool { lock.withLock { satisfied } }
    private init() {
        monitor.pathUpdateHandler = { [weak self] p in self?.lock.withLock { self?.satisfied = p.status == .satisfied } }
        monitor.start(queue: DispatchQueue(label: "dev.pgbot.desktop.network"))
    }
}

/// Native Notification Center delivery. Only available when running as a
/// bundled .app (UNUserNotificationCenter requires a bundle identifier).
enum SystemNotifications {
    static var available: Bool { Bundle.main.bundleIdentifier != nil }

    static func requestAuthorization() {
        guard available else { return }
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound, .badge]) { _, _ in }
    }

    static func postTest(sound: Bool) {
        guard available else { return }
        let c = UNMutableNotificationContent()
        c.title = "🐘 PGBot · Test notification"
        c.body = "Notifications are working. Findings, regressions and recoveries will appear like this."
        if sound { c.sound = .default }
        UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: "test-\(UUID().uuidString)", content: c, trigger: nil))
    }

    enum Permission { case unknown, allowed, denied, notAsked }

    static func permission() async -> Permission {
        guard available else { return .unknown }
        switch await UNUserNotificationCenter.current().notificationSettings().authorizationStatus {
        case .authorized, .provisional: return .allowed
        case .denied: return .denied
        case .notDetermined: return .notAsked
        default: return .unknown
        }
    }

    static func openSystemSettings() {
        let id = Bundle.main.bundleIdentifier ?? "dev.pgbot.desktop"
        if let url = URL(string: "x-apple.systempreferences:com.apple.Notifications-Settings.extension?id=\(id)") {
            NSWorkspace.shared.open(url)
        }
    }

    static func post(_ n: AppNotification, botName: String, sound: Bool, preview: Bool = true) {
        guard available else { return }
        let c = UNMutableNotificationContent()
        // Without previews, the banner says only which database needs a look.
        c.title = preview ? n.title : "🐘 \(botName)"
        c.body = preview ? n.body : (n.category == .resolved ? "An issue was resolved." : "New activity — open PGBot to see details.")
        c.threadIdentifier = n.botID.uuidString
        c.userInfo = ["notificationID": n.id.uuidString]
        if sound && n.severity != .info { c.sound = .default }
        c.interruptionLevel = n.severity == .critical ? .timeSensitive : .active
        UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: n.id.uuidString, content: c, trigger: nil))
    }
}
