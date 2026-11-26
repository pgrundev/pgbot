import PGBotKit
import SwiftUI

@MainActor struct LogsView: View {
    @Environment(AppModel.self) private var model
    let bot: DatabaseBot

    enum Level: String, CaseIterable, Identifiable {
        case all = "All", error = "Errors", warn = "Warnings", info = "Info", query = "Queries"
        var id: String { rawValue }
    }
    enum TimeWindow: String, CaseIterable, Identifiable {
        case all = "Any time", hour = "Last hour", day = "Last 24 hours"
        var id: String { rawValue }
        var seconds: TimeInterval? { switch self { case .all: nil; case .hour: 3600; case .day: 86400 } }
    }

    @State private var entries: [LogEntry] = []
    @State private var loading = false
    @State private var error: EngineError?
    @State private var level: Level = .all
    @State private var window: TimeWindow = .all
    @State private var search = ""
    @State private var expanded: Set<String> = []
    @State private var live = false
    @State private var liveTask: Task<Void, Never>?

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: Space.m) {
                Picker("Level", selection: $level) { ForEach(Level.allCases) { Text($0.rawValue).tag($0) } }
                    .pickerStyle(.segmented).labelsHidden().frame(width: 340)
                Picker("Time", selection: $window) { ForEach(TimeWindow.allCases) { Text($0.rawValue).tag($0) } }
                    .labelsHidden().frame(width: 130)
                TextField("Search logs", text: $search).textFieldStyle(.roundedBorder).frame(maxWidth: 240)
                Spacer()
                Toggle(isOn: $live) { Label("Live", systemImage: "dot.radiowaves.left.and.right") }
                    .toggleStyle(.button).disabled(bot.isDemo)
                    .help("Refresh every 10 seconds while this view is open")
                Button { Task { await load() } } label: { Image(systemName: "arrow.clockwise") }.disabled(loading).help("Refresh")
            }
            .padding(.horizontal, Space.xl).padding(.top, 4).padding(.bottom, Space.m)
            content
        }
        .task { await load() }
        .onChange(of: live) { _, on in
            liveTask?.cancel()
            guard on else { return }
            liveTask = Task { while !Task.isCancelled { try? await Task.sleep(for: .seconds(10)); await load() } }
        }
        .onDisappear { liveTask?.cancel() }
    }

    @ViewBuilder private var content: some View {
        if loading && entries.isEmpty {
            ProgressView("Reading the server log…").frame(maxWidth: .infinity, maxHeight: .infinity)
        } else if let error, entries.isEmpty {
            ContentUnavailableView {
                Label("Logs aren't available", systemImage: "doc.questionmark")
            } description: {
                VStack(spacing: 6) {
                    Text(error.hint)
                    Text("PGBot reads the log over SQL (pg_read_binary_file), which needs one extra grant. Managed providers such as Supabase, Neon and RDS expose logs through their own APIs instead.")
                        .foregroundStyle(.secondary)
                    if !error.detail.isEmpty { Text(error.detail).font(.caption.monospaced()).foregroundStyle(.tertiary) }
                }
            } actions: {
                Button("Try again") { Task { await load() } }
            }
        } else if filtered.isEmpty {
            ContentUnavailableView("No matching log entries", systemImage: "line.3.horizontal.decrease.circle",
                                   description: Text(entries.isEmpty ? "The server log is empty." : "Try a different filter."))
        } else {
            List(filtered) { e in
                LogRow(entry: e, expanded: expanded.contains(e.id)) {
                    if expanded.contains(e.id) { expanded.remove(e.id) } else { expanded.insert(e.id) }
                } explain: {
                    model.explainLog(e, botID: bot.id)
                }
            }
            .listStyle(.plain)
            .scrollContentBackground(.hidden)
        }
    }

    private var filtered: [LogEntry] {
        entries.filter { e in
            let lvl: Bool = switch level {
            case .all: true
            case .error: e.level == "error"
            case .warn: e.level == "warn"
            case .info: e.level == "info"
            case .query: e.level == "query"
            }
            let t = window.seconds.map { e.time >= Date().addingTimeInterval(-$0) } ?? true
            let s = search.isEmpty || e.message.localizedCaseInsensitiveContains(search) || (e.detail ?? "").localizedCaseInsensitiveContains(search)
            return lvl && t && s
        }
        .sorted { $0.time > $1.time }
    }

    private func load() async {
        if bot.isDemo { entries = model.demoLogs(for: bot.id) ?? []; return }
        guard let engine = model.engine else { error = model.engineProblem; return }
        loading = true
        defer { loading = false }
        do { entries = try await engine.logs(model.target(for: bot), last: 300); error = nil }
        catch { self.error = error as? EngineError ?? .failed(error.localizedDescription) }
    }
}

@MainActor struct LogRow: View {
    let entry: LogEntry
    let expanded: Bool
    let toggle: () -> Void
    let explain: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(alignment: .firstTextBaseline, spacing: Space.s) {
                Text(entry.severity).font(.caption.weight(.bold).monospaced())
                    .foregroundStyle(color).frame(width: 64, alignment: .leading)
                Text(entry.time.formatted(date: .omitted, time: .standard)).font(.caption.monospacedDigit()).foregroundStyle(.secondary)
                Text(entry.message).font(.system(.callout, design: .monospaced)).lineLimit(expanded ? nil : 1).textSelection(.enabled)
                Spacer()
            }
            if expanded {
                VStack(alignment: .leading, spacing: 4) {
                    if let d = entry.detail, !d.isEmpty { Text(d).font(.system(.caption, design: .monospaced)).foregroundStyle(.secondary).textSelection(.enabled) }
                    Text([entry.user.map { "user \($0)" }, entry.database.map { "db \($0)" }, entry.app.map { "app \($0)" }, entry.pid.map { "pid \($0)" }]
                        .compactMap { $0 }.joined(separator: " · ")).font(.caption2).foregroundStyle(.tertiary)
                    HStack {
                        Button("Explain with PGBot", action: explain).buttonStyle(.borderedProminent)
                        Button("Copy") { Pasteboard.copy([entry.severity, entry.message, entry.detail ?? ""].joined(separator: "\n")) }
                    }
                    .controlSize(.small)
                }
                .padding(.leading, 72)
            }
        }
        .padding(.vertical, 3)
        .contentShape(Rectangle())
        .onTapGesture(perform: toggle)
        .listRowBackground(entry.level == "error" ? Color.red.opacity(0.06) : (entry.level == "warn" ? Color.orange.opacity(0.05) : Color.clear))
        .contextMenu {
            Button("Copy Message") { Pasteboard.copy(entry.message) }
            Button("Explain with PGBot", action: explain)
        }
    }

    private var color: Color {
        switch entry.severity.uppercased() {
        case "PANIC", "FATAL", "ERROR": .red
        case "WARNING": .orange
        case "NOTICE", "INFO": .blue
        case "DEBUG", "DEBUG1", "DEBUG2", "DEBUG3", "DEBUG4", "DEBUG5": .secondary
        default: .secondary
        }
    }
}
