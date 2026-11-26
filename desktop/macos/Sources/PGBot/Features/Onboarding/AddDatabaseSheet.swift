import PGBotKit
import SwiftUI

@MainActor struct AddDatabaseSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    enum Mode: String, CaseIterable { case url = "Connection URL", manual = "Manual" }
    enum TestState { case idle, testing, success(EngineContext), failure(EngineError) }

    @State private var mode: Mode = .url
    @State private var url = ""
    @State private var urlError: String?
    @State private var conn = DatabaseConnection()
    @State private var password = ""
    @State private var name = ""
    @State private var description = ""
    @State private var color: BotColor = BotColor.allCases.randomElement() ?? .blue
    @State private var shape: AvatarShape = .roundedSquare
    @State private var environment: DatabaseEnvironment = .production
    @State private var test: TestState = .idle
    @State private var testTask: Task<Void, Never>?
    @State private var setupSQL: String?

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: Space.m) {
                ElephantAvatar(color: color, shape: shape, size: 44)
                VStack(alignment: .leading) {
                    Text("Add database").font(.title2.weight(.semibold))
                    Text("PGBot connects read-only and never changes your data.").foregroundStyle(.secondary)
                }
                Spacer()
            }
            .padding(Space.xl)
            Divider()
            ScrollView {
                VStack(alignment: .leading, spacing: Space.xl) {
                    connectionSection
                    readOnlyAdvice
                    personalizeSection
                }
                .padding(Space.xl)
            }
            Divider()
            footer.padding(Space.l)
        }
        .frame(width: 620, height: 720)
        .sheet(isPresented: Binding(get: { setupSQL != nil }, set: { if !$0 { setupSQL = nil } })) {
            SetupSQLSheet(sql: setupSQL ?? "")
        }
        .onAppear(perform: applyDefaults)
        .onDisappear { testTask?.cancel() }
    }

    // MARK: Sections

    private var connectionSection: some View {
        VStack(alignment: .leading, spacing: Space.m) {
            Picker("", selection: $mode) { ForEach(Mode.allCases, id: \.self) { Text($0.rawValue) } }
                .pickerStyle(.segmented).labelsHidden().frame(width: 280)
                .onChange(of: mode) { _, new in if new == .manual { applyURL() } }
            if mode == .url {
                VStack(alignment: .leading, spacing: 6) {
                    TextField("postgresql://username:password@hostname:5432/database?sslmode=require", text: $url, axis: .vertical)
                        .font(.system(.body, design: .monospaced)).lineLimit(1...3)
                        .textFieldStyle(.roundedBorder)
                        .onChange(of: url) { _, _ in applyURL(); test = .idle }
                    if let urlError { Text(urlError).font(.caption).foregroundStyle(.red) }
                    else if !url.isEmpty {
                        Text("\(conn.safeSummary) · SSL \(conn.sslMode.rawValue)\(password.isEmpty ? "" : " · password saved to Keychain")")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                    Text("Supabase, Neon, RDS, Aurora, Cloud SQL, Azure, Render, Railway, PGBox or self-hosted — paste the URL your provider shows.")
                        .font(.caption).foregroundStyle(.tertiary)
                }
            } else {
                Form {
                    TextField("Host", text: $conn.host)
                    TextField("Port", value: $conn.port, format: .number.grouping(.never))
                    TextField("Database", text: $conn.database)
                    TextField("Username", text: $conn.username)
                    SecureField("Password", text: $password)
                    Picker("SSL mode", selection: $conn.sslMode) { ForEach(SSLMode.allCases) { Text($0.rawValue).tag($0) } }
                    Stepper("Connection timeout: \(conn.connectTimeoutSeconds)s", value: $conn.connectTimeoutSeconds, in: 2...120)
                }
                .formStyle(.columns)
                .onChange(of: conn) { _, _ in test = .idle }
            }
            testResult
        }
    }

    @ViewBuilder private var testResult: some View {
        switch test {
        case .idle: EmptyView()
        case .testing:
            HStack(spacing: Space.s) {
                ProgressView().controlSize(.small)
                Text("Connecting and running a read-only inspection…").foregroundStyle(.secondary)
            }
        case .success(let ctx):
            VStack(alignment: .leading, spacing: 4) {
                Label("Connected to \(ctx.server.displayVersion)", systemImage: "checkmark.circle.fill").foregroundStyle(.green).font(.headline)
                capability("pg_monitor role", ctx.server.hasPgMonitor ?? false, fix: "Grant pg_monitor to see sessions, locks and settings.")
                capability("pg_stat_statements", ctx.queries?.isAvailable ?? false, fix: "Enable it for query analysis.")
                capability("Read-only session", true, fix: "")
                if let p = ctx.server.provider, !p.isEmpty, p != "unknown" { Text("Provider detected: \(p.uppercased())").font(.caption).foregroundStyle(.secondary) }
            }
            .card(padding: Space.m)
        case .failure(let e):
            VStack(alignment: .leading, spacing: 4) {
                Label(e.title, systemImage: "xmark.octagon.fill").foregroundStyle(.red).font(.headline)
                Text(e.hint)
                if !e.detail.isEmpty { Text(e.detail).font(.system(.caption, design: .monospaced)).foregroundStyle(.secondary).textSelection(.enabled) }
            }
            .card(padding: Space.m)
        }
    }

    private func capability(_ name: String, _ ok: Bool, fix: String) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            Image(systemName: ok ? "checkmark" : "exclamationmark.triangle.fill").foregroundStyle(ok ? .green : .orange).font(.caption)
            Text(name).font(.callout)
            if !ok { Text("— \(fix)").font(.caption).foregroundStyle(.secondary) }
        }
    }

    private var readOnlyAdvice: some View {
        HStack(alignment: .top, spacing: Space.m) {
            Image(systemName: "lock.shield").font(.title2).foregroundStyle(.secondary)
            VStack(alignment: .leading, spacing: 4) {
                Text("Use a dedicated read-only role").font(.headline)
                Text("PGBot only needs pg_monitor. A separate role keeps it away from your data and is easy to revoke.")
                    .foregroundStyle(.secondary)
                Button("Show setup SQL") { Task { await loadSetupSQL() } }.buttonStyle(.link).disabled(model.engine == nil)
            }
        }
    }

    private var personalizeSection: some View {
        VStack(alignment: .leading, spacing: Space.m) {
            Text("Personalize").font(.headline)
            Form {
                TextField("Display name", text: $name, prompt: Text(conn.database.isEmpty ? "My database" : conn.database))
                TextField("Description", text: $description, prompt: Text("Optional"))
                Picker("Environment", selection: $environment) { ForEach(DatabaseEnvironment.allCases) { Text($0.label).tag($0) } }
                Picker("Avatar shape", selection: $shape) {
                    Text("Rounded square").tag(AvatarShape.roundedSquare)
                    Text("Circle").tag(AvatarShape.circle)
                }
                .pickerStyle(.segmented)
                LabeledContent("Color") { ColorSwatches(selection: $color, shape: shape) }
            }
            .formStyle(.columns)
        }
    }

    private var footer: some View {
        HStack {
            Button("Cancel", role: .cancel) { dismiss() }.keyboardShortcut(.cancelAction)
            Spacer()
            Button("Test Connection") { runTest() }.disabled(!canConnect || isTesting)
            Button("Add Database") { add() }.keyboardShortcut(.defaultAction).buttonStyle(.borderedProminent)
                .disabled(!canConnect || isTesting)
        }
    }

    // MARK: Actions

    private var isTesting: Bool { if case .testing = test { return true }; return false }
    private var canConnect: Bool { urlError == nil && !conn.host.isEmpty && (mode == .manual || !url.isEmpty) }

    /// Settings → New Databases.
    private func applyDefaults() {
        let d = model.settings
        conn.sslMode = d.defaultSSLMode
        conn.connectTimeoutSeconds = d.defaultConnectTimeout
        environment = d.defaultEnvironment
        shape = d.defaultAvatarShape
        if let c = d.defaultAvatarColor { color = c }
    }

    private func applyURL() {
        guard !url.trimmingCharacters(in: .whitespaces).isEmpty else { urlError = nil; return }
        do {
            let p = try ConnectionString.parse(url)
            conn = p.connection
            password = p.password ?? ""
            urlError = nil
            if conn.host.contains("supabase") { color = .green } else if conn.host.contains("neon") { color = .teal }
            if ["localhost", "127.0.0.1", "::1"].contains(conn.host) { environment = .local }
        } catch {
            urlError = (error as? LocalizedError)?.errorDescription ?? "Couldn't read that URL."
        }
    }

    private func runTest() {
        guard let engine = model.engine else { test = .failure(model.engineProblem ?? .engineMissing("")); return }
        test = .testing
        let target = EngineTarget(connection: conn, password: password.isEmpty ? nil : password)
        testTask = Task {
            do { let ctx = try await engine.inspect(target); if !Task.isCancelled { test = .success(ctx) } }
            catch { if !Task.isCancelled { test = .failure(error as? EngineError ?? .failed(error.localizedDescription)) } }
        }
    }

    private func add() {
        if model.demoMode { model.setDemoMode(false) }
        let displayName = name.trimmingCharacters(in: .whitespaces).isEmpty ? conn.database : name
        let bot = DatabaseBot(displayName: displayName, description: description,
                              avatar: BotAvatar(color: color, shape: shape), environment: environment,
                              connection: conn, pinned: model.bots.filter(\.pinned).count < 3,
                              schedule: {
                                  var s = MonitoringSchedule()
                                  s.intervalSeconds = model.settings.defaultIntervalSeconds
                                  s.dailyReportHour = model.settings.defaultDailyReportHour
                                  s.retentionDays = model.settings.defaultRetentionDays
                                  return s
                              }())
        var first: EngineContext?
        if case .success(let ctx) = test { first = ctx }
        model.addBot(bot, password: password.isEmpty ? nil : password, firstInspection: first)
        dismiss()
    }

    private func loadSetupSQL() async {
        guard let engine = model.engine else { return }
        setupSQL = (try? await engine.setupSQL(role: "pgbot_ro", database: conn.database.isEmpty ? "postgres" : conn.database, includeLogs: false))
            ?? "-- Couldn't generate setup SQL. Run `pgbot init` in a terminal."
    }
}

@MainActor struct ColorSwatches: View {
    @Binding var selection: BotColor
    var shape: AvatarShape = .roundedSquare
    var body: some View {
        HStack(spacing: 6) {
            ForEach(BotColor.allCases) { c in
                Button { selection = c } label: {
                    ElephantAvatar(color: c, shape: shape, size: 26)
                        .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(selection == c ? Theme.accent : .clear, lineWidth: 2).padding(-3))
                }
                .buttonStyle(.plain).help(c.label).accessibilityLabel(c.label)
                .accessibilityAddTraits(selection == c ? .isSelected : [])
            }
        }
    }
}

@MainActor struct SetupSQLSheet: View {
    let sql: String
    @Environment(\.dismiss) private var dismiss
    var body: some View {
        VStack(alignment: .leading, spacing: Space.m) {
            Text("Create a read-only monitoring role").font(.title3.weight(.semibold))
            Text("Generated by `pgbot init`. Review it, set a real password, and run it as an administrator. PGBot never runs it for you.")
                .foregroundStyle(.secondary)
            ScrollView { SQLBlock(sql: sql) }
            HStack { Spacer(); Button("Done") { dismiss() }.keyboardShortcut(.defaultAction) }
        }
        .padding(Space.xl).frame(width: 640, height: 520)
    }
}
