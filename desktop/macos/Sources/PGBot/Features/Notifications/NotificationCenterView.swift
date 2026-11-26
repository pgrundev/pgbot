import PGBotKit
import SwiftUI

@MainActor struct NotificationCenterView: View {
    @Environment(AppModel.self) private var model
    var initialBot: UUID? = nil
    @State private var botFilter: UUID?
    @State private var severityFilter: Severity?
    @State private var unreadOnly = false

    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Text("Notifications").font(.headline)
                Spacer()
                Button("Mark all as read") { model.markAllRead() }.buttonStyle(.link).disabled(model.unreadNotifications == 0)
            }
            .padding(Space.m)
            HStack(spacing: Space.s) {
                Picker("Database", selection: $botFilter) {
                    Text("All databases").tag(UUID?.none)
                    ForEach(model.bots) { Text($0.displayName).tag(UUID?.some($0.id)) }
                }
                .labelsHidden()
                Picker("Severity", selection: $severityFilter) {
                    Text("Any severity").tag(Severity?.none)
                    ForEach(Severity.allCases.reversed(), id: \.self) { Text($0.label).tag(Severity?.some($0)) }
                }
                .labelsHidden()
                Toggle("Unread", isOn: $unreadOnly).toggleStyle(.checkbox)
            }
            .controlSize(.small).padding(.horizontal, Space.m).padding(.bottom, Space.s)
            Divider()
            if filtered.isEmpty {
                ContentUnavailableView("You're all caught up", systemImage: "bell.badge", description: Text("New findings, regressions and resolutions appear here."))
                    .frame(height: 260)
            } else {
                ScrollView {
                    LazyVStack(spacing: 0) {
                        ForEach(filtered) { n in
                            NotificationRow(n: n).onTapGesture { model.open(n) }
                            Divider().padding(.leading, 56)
                        }
                    }
                }
                .frame(maxHeight: 440)
            }
            Divider()
            HStack {
                if let id = botFilter, let bot = model.bot(id) {
                    Button(bot.muted ? "Unmute \(bot.displayName)" : "Mute \(bot.displayName)") { model.mutate(bot.id) { $0.muted.toggle() } }
                }
                Spacer()
                Button("Notification settings…") { openAppSettings(tab: "notifications") }
            }
            .buttonStyle(.link).font(.callout).padding(Space.m)
        }
        .frame(width: 400)
        .onAppear { botFilter = initialBot }
    }

    private var filtered: [AppNotification] {
        model.notifications.filter {
            (botFilter == nil || $0.botID == botFilter) && (severityFilter == nil || $0.severity == severityFilter) && (!unreadOnly || !$0.read)
        }
    }
}

@MainActor struct NotificationRow: View {
    @Environment(AppModel.self) private var model
    let n: AppNotification
    var body: some View {
        HStack(alignment: .top, spacing: Space.m) {
            if let bot = model.bot(n.botID) { BotAvatarView(bot: bot, size: 32) } else { ElephantAvatar(color: .gray, size: 32) }
            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: 4) {
                    Image(systemName: n.category == .resolved ? "checkmark.circle.fill" : n.severity.symbol)
                        .foregroundStyle(n.category == .resolved ? .green : n.severity.color).font(.caption)
                    Text(n.title).font(.callout.weight(n.read ? .regular : .semibold)).lineLimit(1)
                }
                Text(n.body).font(.callout).foregroundStyle(.secondary).lineLimit(2)
                Text(n.createdAt.formatted(.relative(presentation: .named))).font(.caption2).foregroundStyle(.tertiary)
            }
            Spacer()
            if !n.read { Circle().fill(Theme.accent).frame(width: 7, height: 7).padding(.top, 6) }
        }
        .padding(.horizontal, Space.m).padding(.vertical, Space.s)
        .contentShape(Rectangle())
        .hoverHighlight(cornerRadius: 0)
        .contextMenu {
            Button(n.read ? "Already read" : "Mark as Read") { model.markRead(n) }.disabled(n.read)
            if let bot = model.bot(n.botID) {
                Button(bot.muted ? "Unmute \(bot.displayName)" : "Mute \(bot.displayName)") { model.mutate(bot.id) { $0.muted.toggle() } }
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityHint("Opens the conversation")
    }
}

// MARK: - Menu bar

@MainActor struct MenuBarContent: View {
    @Environment(AppModel.self) private var model
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        if model.session == nil {
            VStack(alignment: .leading, spacing: Space.s) {
                Text("PGBot").font(.headline)
                Button("Open PGBot to sign in") { NSApp.activate(ignoringOtherApps: true); openWindow(id: "main") }
                Button("Quit PGBot") { NSApp.terminate(nil) }.buttonStyle(.link).font(.caption)
            }
            .padding(Space.m).frame(width: 240)
        } else {
            signedIn
        }
    }

    private var signedIn: some View {
        VStack(alignment: .leading, spacing: Space.s) {
            HStack {
                PGBotLogo(size: 22)
                VStack(alignment: .leading, spacing: 0) {
                    Text("PGBot").font(.headline)
                    Text("\(model.bots.count) connected database\(model.bots.count == 1 ? "" : "s")").font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                let crit = model.latest.values.reduce(0) { $0 + $1.score.critical }
                let warn = model.latest.values.reduce(0) { $0 + $1.score.warnings }
                VStack(alignment: .trailing, spacing: 0) {
                    Text("\(crit) critical").font(.caption).foregroundStyle(crit > 0 ? .red : .secondary)
                    Text("\(warn) warnings").font(.caption).foregroundStyle(warn > 0 ? .orange : .secondary)
                }
            }
            Divider()
            ForEach(model.bots) { bot in
                Button { open(bot.id) } label: {
                    HStack(spacing: Space.s) {
                        StatusDot(status: bot.status)
                        Text(bot.displayName).lineLimit(1)
                        Spacer()
                        Text(bot.lastSummary.map { $0.components(separatedBy: " · ").first ?? $0 } ?? bot.status.label)
                            .font(.caption).foregroundStyle(.secondary).lineLimit(1)
                    }
                    .contentShape(Rectangle())
                }
                .buttonStyle(.plain).padding(.vertical, 2)
            }
            if !model.notifications.isEmpty {
                Divider()
                Text("Recent").font(.caption.weight(.semibold)).foregroundStyle(.secondary)
                ForEach(model.notifications.prefix(3)) { n in
                    Button { NSApp.activate(ignoringOtherApps: true); openWindow(id: "main"); model.open(n) } label: {
                        Text(n.body).font(.caption).lineLimit(1).frame(maxWidth: .infinity, alignment: .leading)
                    }.buttonStyle(.plain)
                }
            }
            Divider()
            Text(model.monitoringPaused ? "Monitoring paused" : "Monitoring while this Mac is awake")
                .font(.caption).foregroundStyle(.secondary)
            HStack {
                Button("Open PGBot") { NSApp.activate(ignoringOtherApps: true); openWindow(id: "main") }
                Button("Run all") { model.runAllInspections() }
                Button("Send test") { model.sendTestNotification() }
                Button(model.monitoringPaused ? "Resume" : "Pause") { model.setMonitoringPaused(!model.monitoringPaused) }
            }
            .controlSize(.small)
            HStack {
                Button("Settings…") { NSApp.activate(ignoringOtherApps: true); openAppSettings() }
                Spacer()
                Button("Quit PGBot") { NSApp.terminate(nil) }
            }
            .buttonStyle(.link).font(.caption)
        }
        .padding(Space.m)
        .frame(width: 320)
    }

    private func open(_ id: UUID) {
        NSApp.activate(ignoringOtherApps: true)
        openWindow(id: "main")
        model.selection = id
        model.route = .chat
    }
}

// MARK: - Sheets

/// Explicit consent before findings are sent to an external AI provider.
@MainActor struct AIConsentSheet: View {
    @Environment(AppModel.self) private var model
    let consent: AppModel.PendingConsent
    var body: some View {
        VStack(alignment: .leading, spacing: Space.m) {
            Label("Send findings to \(consent.ai.provider.label)?", systemImage: "hand.raised").font(.title3.weight(.semibold))
            Text("To answer, PGBot sends this database's **findings** — the same PII-free report as `pgbot inspect --json` — and your question to \(consent.ai.provider.label)\(consent.ai.effectiveBaseURL.flatMap { URL(string: $0)?.host }.map { " at \($0)" } ?? "").")
            VStack(alignment: .leading, spacing: 4) {
                Label("Never sent: passwords, connection strings, or table contents", systemImage: "checkmark.shield")
                Label("SQL literals are replaced with placeholders by the engine", systemImage: "checkmark.shield")
                Label("The model can't reach your database; it only reads the findings", systemImage: "checkmark.shield")
            }
            .font(.callout).foregroundStyle(.secondary)
            Text("You can revoke this in Settings → AI providers, or use Ollama to keep everything on this Mac.")
                .font(.caption).foregroundStyle(.tertiary)
            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { model.pendingConsent = nil }.keyboardShortcut(.cancelAction)
                Button("Allow and Ask") { model.grantConsent(consent) }.keyboardShortcut(.defaultAction).buttonStyle(.borderedProminent)
            }
        }
        .padding(Space.xl).frame(width: 480)
    }
}

@MainActor struct ExplainSQLSheet: View {
    @Environment(AppModel.self) private var model
    @State private var sql = ""
    var body: some View {
        VStack(alignment: .leading, spacing: Space.m) {
            Text("Explain SQL").font(.title3.weight(.semibold))
            Text("PGBot plans the query inside a read-only transaction and never executes it. Only a single SELECT is accepted.")
                .foregroundStyle(.secondary)
            TextEditor(text: $sql).font(.system(.body, design: .monospaced)).frame(height: 180)
                .overlay(RoundedRectangle(cornerRadius: 6).strokeBorder(Palette.hairline))
            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { model.explainSQLDraft = nil }.keyboardShortcut(.cancelAction)
                Button("Explain") { if let id = model.selection { model.explain(sql: sql, on: id) } }
                    .keyboardShortcut(.defaultAction).buttonStyle(.borderedProminent)
                    .disabled(sql.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            }
        }
        .padding(Space.xl).frame(width: 560)
        .onAppear { sql = model.explainSQLDraft ?? "" }
    }
}
