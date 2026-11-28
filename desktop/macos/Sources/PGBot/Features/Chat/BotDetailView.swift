import PGBotKit
import SwiftUI

/// No header bar: content runs edge to edge and the bot's name floats on top
/// in a pill, with a couple of quiet actions on the right.
@MainActor struct BotDetailView: View {
    @Environment(AppModel.self) private var model
    let bot: DatabaseBot

    var body: some View {
        VStack(spacing: 0) {
            BotHeader(bot: bot)
            StatusNotes()
            Group {
                switch model.route {
                case .chat: ConversationView(bot: bot)
                case .metrics: MetricsView(bot: bot)
                case .logs: LogsView(bot: bot)
                }
            }
            .frame(maxHeight: .infinity)
        }
        .navigationTitle(bot.displayName)
    }
}

/// 62pt header: the bot in a centered pill, quiet outline actions on the right.
@MainActor struct BotHeader: View {
    @Environment(AppModel.self) private var model
    let bot: DatabaseBot
    @State private var showingInfo = false
    @State private var showingNotifications = false

    var body: some View {
        ZStack {
            Button { showingInfo.toggle() } label: {
                HStack(spacing: 8) {
                    BotAvatarView(bot: bot, size: 22)
                    Text(bot.displayName).font(.sys(size: 15, weight: .medium))
                    StatusDot(status: model.inspecting.contains(bot.id) ? .connecting : bot.status, size: 7)
                    if model.isPSQL(bot.id) {
                        Text("PSQL").font(.sys(size: 10, weight: .bold, design: .monospaced))
                            .padding(.horizontal, 5).padding(.vertical, 1.5)
                            .background(Palette.healthy.opacity(0.16), in: Capsule()).foregroundStyle(Palette.healthy)
                    }
                    if bot.isDemo { DemoBadge() }
                }
                .padding(.leading, 8).padding(.trailing, 14).padding(.vertical, 6)
                .background(Palette.floating, in: RoundedRectangle(cornerRadius: 20, style: .continuous))
                .contentShape(RoundedRectangle(cornerRadius: 20))
            }
            .buttonStyle(.plain)
            .popover(isPresented: $showingInfo, arrowEdge: .bottom) { DatabaseInfoPopover(bot: bot) }
            .accessibilityLabel("\(bot.displayName), \(bot.status.label). Show database details")

            HStack(spacing: 8) {
                if model.route != .chat {
                    Button { withAnimation(.snappy) { model.route = .chat } } label: {
                        Label("Chat", systemImage: "chevron.left").font(.sys(size: 14, weight: .medium)).outlinePill()
                    }
                    .buttonStyle(.plain)
                    .keyboardShortcut(.escape, modifiers: [])
                }
                Spacer()
                Button { withAnimation(.snappy) { model.route = model.route == .metrics ? .chat : .metrics } } label: {
                    Text("Metrics").font(.sys(size: 14, weight: .medium)).outlinePill(active: model.route == .metrics)
                }
                .buttonStyle(.plain)
                .help(model.route == .metrics ? "Back to chat" : "Metrics")
                Button { withAnimation(.snappy) { model.route = model.route == .logs ? .chat : .logs } } label: {
                    CircleIcon(symbol: "doc.plaintext", size: 36, outlined: true)
                }
                .buttonStyle(.plain).help("Logs")
                Button { showingNotifications.toggle() } label: {
                    CircleIcon(symbol: "bell", size: 36, badge: model.notifications.contains { $0.botID == bot.id && !$0.read }, outlined: true)
                }
                .buttonStyle(.plain).help("Notifications")
                .popover(isPresented: $showingNotifications, arrowEdge: .bottom) { NotificationCenterView(initialBot: bot.id) }
                Menu {
                    Button("Run Inspection") { Task { await model.inspect(bot.id, reason: .manual) } }
                        .disabled(model.inspecting.contains(bot.id))
                    Button("Database Settings…") { model.settingsForBot = bot }
                    Divider()
                    Button(bot.muted ? "Unmute Notifications" : "Mute Notifications") { model.mutate(bot.id) { $0.muted.toggle() } }
                    Button(bot.schedule.enabled ? "Pause Monitoring" : "Resume Monitoring") { model.mutate(bot.id) { $0.schedule.enabled.toggle() } }
                    Divider()
                    Button("Export Conversation…") { model.exportConversation(bot.id) }
                    Button("Export Inspection Report…") { model.exportInspection(bot.id) }.disabled(model.latest[bot.id] == nil)
                    if model.demoMode { Divider(); Button("Exit Demo Mode") { model.setDemoMode(false) } }
                } label: {
                    CircleIcon(symbol: "ellipsis", size: 36, outlined: true)
                }
                .menuStyle(.button).buttonStyle(.plain).menuIndicator(.hidden).fixedSize()
                .help("More")
            }
        }
        .padding(.horizontal, 14)
        .frame(height: 62)
    }
}

/// Facts about the connection — never the password or the full connection string.
@MainActor struct DatabaseInfoPopover: View {
    @Environment(AppModel.self) private var model
    let bot: DatabaseBot
    var body: some View {
        let ctx = model.latest[bot.id]?.context
        VStack(alignment: .leading, spacing: Space.m) {
            HStack(spacing: Space.m) {
                BotAvatarView(bot: bot, size: 40)
                VStack(alignment: .leading) {
                    Text(bot.displayName).font(Typo.bodyEmphasis)
                    if !bot.description.isEmpty { Text(bot.description).font(Typo.small).foregroundStyle(.secondary) }
                }
            }
            Grid(alignment: .leading, horizontalSpacing: Space.l, verticalSpacing: 6) {
                row("PostgreSQL", ctx?.server.displayVersion ?? "Unknown until the first inspection")
                row("Host", "\(bot.connection.host):\(bot.connection.port)")
                row("Database", bot.connection.database)
                row("User", bot.connection.username)
                if let p = ctx?.server.provider, !p.isEmpty, p != "unknown" { row("Provider", p.uppercased()) }
                row("Environment", bot.environment.label)
                row("Status", model.inspecting.contains(bot.id) ? "Inspecting…" : bot.status.label)
                row("Last inspection", bot.lastInspectedAt.map { $0.formatted(.relative(presentation: .named)) } ?? "Never")
                row("Monitoring", bot.isDemo ? "Demo data" : (!bot.schedule.enabled || model.monitoringPaused ? "Paused"
                    : "Every \(Duration.seconds(bot.schedule.intervalSeconds).formatted(.units(allowed: [.hours, .minutes], width: .wide))) while this Mac is awake"))
                if let has = ctx?.server.hasPgMonitor { row("pg_monitor", has ? "Granted" : "Missing — some statistics hidden") }
            }
            .font(Typo.small)
        }
        .padding(Space.l)
        .frame(width: 340)
    }

    private func row(_ k: String, _ v: String) -> some View {
        GridRow {
            Text(k).foregroundStyle(.secondary)
            Text(v).textSelection(.enabled)
        }
    }
}
