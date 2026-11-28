import PGBotKit
import SwiftUI

/// Two surfaces, no toolbar: a soft gray sidebar and a white conversation
/// canvas. The window's title bar is hidden; the traffic lights sit in the sidebar.
@MainActor struct MainView: View {
    @Environment(AppModel.self) private var model
    @AppStorage("sidebarWidth") private var sidebarWidth: Double = 290
    @State private var dragOrigin: Double?

    var body: some View {
        @Bindable var model = model
        HStack(spacing: 0) {
            SidebarView()
                .frame(width: sidebarWidth)
                .background(Palette.sidebar)
            resizeHandle
            detail
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .background(Palette.canvas)
        }
        .ignoresSafeArea()
        .sheet(isPresented: $model.showingAddDatabase) { AddDatabaseSheet() }
        .sheet(item: $model.settingsForBot) { bot in DatabaseSettingsView(botID: bot.id) }
        .sheet(item: $model.pendingConsent) { c in AIConsentSheet(consent: c) }
        .sheet(isPresented: Binding(get: { model.explainSQLDraft != nil }, set: { if !$0 { model.explainSQLDraft = nil } })) {
            ExplainSQLSheet()
        }
        .sheet(isPresented: $model.showingSearch) { SearchPalette() }
    }

    @ViewBuilder private var detail: some View {
        if model.bots.isEmpty {
            ZStack(alignment: .top) {
                OnboardingView()
                StatusNotes().padding(.top, 14)
            }
        } else if let bot = model.selectedBot {
            BotDetailView(bot: bot).id(bot.id)
        } else {
            ContentUnavailableView("Select a database", systemImage: "cylinder.split.1x2",
                                   description: Text("Choose a database bot in the sidebar."))
        }
    }

    /// An invisible edge you can drag to resize the sidebar (no divider line:
    /// the two surface colors already separate the columns).
    private var resizeHandle: some View {
        Color.clear.frame(width: 0)
            .overlay {
                Color.clear.frame(width: 9).contentShape(Rectangle())
                    .onHover { inside in if inside { NSCursor.resizeLeftRight.push() } else { NSCursor.pop() } }
                    .gesture(DragGesture(minimumDistance: 1, coordinateSpace: .global)
                        .onChanged { v in
                            if dragOrigin == nil { dragOrigin = sidebarWidth }
                            sidebarWidth = min(420, max(240, (dragOrigin ?? sidebarWidth) + v.translation.width))
                        }
                        .onEnded { _ in dragOrigin = nil })
            }
    }
}

/// Small floating notes for rare app-wide states (engine missing, offline, paused).
/// Demo mode is marked by DEMO badges on the title pill and every row instead.
@MainActor struct StatusNotes: View {
    @Environment(AppModel.self) private var model
    var body: some View {
        VStack(spacing: 6) {
            if let p = model.engineProblem { note("exclamationmark.triangle.fill", .orange, p.title, help: p.hint) { EmptyView() } }
            if model.isOffline { note("wifi.slash", .secondary, "Offline — monitoring resumes when you're back") { EmptyView() } }
            if model.monitoringPaused && !model.bots.isEmpty && !model.demoMode {
                note("pause.circle.fill", .secondary, "Monitoring paused") { Button("Resume") { model.setMonitoringPaused(false) }.buttonStyle(.link) }
            }
        }
        .font(Typo.small)
    }

    private func note<A: View>(_ symbol: String, _ color: Color, _ text: String, help: String? = nil, @ViewBuilder action: () -> A) -> some View {
        HStack(spacing: 6) {
            Image(systemName: symbol).foregroundStyle(color)
            Text(text).foregroundStyle(.secondary)
            action()
        }
        .floatingPill(height: 28)
        .help(help ?? text)
    }
}

// MARK: - Sidebar

@MainActor struct SidebarView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        VStack(spacing: 0) {
            // Same line as the traffic lights.
            HStack(spacing: 8) {
                Spacer()
                CircleButton(symbol: "magnifyingglass", help: "Search (⌘K)") { model.showingSearch = true }
                CircleButton(symbol: "plus", help: "Add database (⌘N)") { model.showingAddDatabase = true }
            }
            .padding(.horizontal, 12)
            .frame(height: 48)

            ScrollView {
                VStack(alignment: .leading, spacing: 2) {
                    if model.settings.showPinnedBots && !pinned.isEmpty {
                        PinnedStrip(bots: pinned).padding(.top, 18).padding(.horizontal, 2).padding(.bottom, 10)
                    }
                    if model.settings.groupByEnvironment {
                        ForEach(model.groups, id: \.name) { group in GroupSection(name: group.name, bots: group.bots) }
                    } else {
                        ForEach(model.bots.sorted { $0.sortOrder < $1.sortOrder }) { bot in row(bot) }
                            .padding(.top, 4)
                    }
                }
                .padding(.horizontal, 10)
                .padding(.bottom, 12)
            }
            .scrollIndicators(.never)

            SidebarFooter()
        }
    }

    private var pinned: [DatabaseBot] { model.bots.filter(\.pinned).sorted { $0.sortOrder < $1.sortOrder } }

    private func row(_ bot: DatabaseBot) -> some View {
        BotRow(bot: bot, selected: model.selection == bot.id, inspecting: model.inspecting.contains(bot.id),
               compact: model.settings.density == .compact, showsPreview: model.settings.showSidebarPreviews)
            .onTapGesture { model.selection = bot.id; model.route = .chat }
            .contextMenu { BotContextMenu(bot: bot) }
    }
}

@MainActor struct GroupSection: View {
    @Environment(AppModel.self) private var model
    let name: String
    let bots: [DatabaseBot]

    var body: some View {
        let collapsed = model.settings.collapsedGroups.contains(name)
        Button {
            withAnimation(.snappy(duration: 0.2)) {
                if collapsed { model.settings.collapsedGroups.remove(name) } else { model.settings.collapsedGroups.insert(name) }
            }
        } label: {
            HStack {
                Text(name)
                Spacer()
                if collapsed { Text("\(bots.count)").monospacedDigit() }
            }
            .font(Typo.small)
            .foregroundStyle(.secondary)
            .padding(.horizontal, 10)
            .padding(.top, 14).padding(.bottom, 4)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel("\(name), \(collapsed ? "collapsed" : "expanded")")

        if !collapsed {
            ForEach(bots) { bot in
                BotRow(bot: bot, selected: model.selection == bot.id, inspecting: model.inspecting.contains(bot.id),
                       compact: model.settings.density == .compact, showsPreview: model.settings.showSidebarPreviews)
                    .onTapGesture { model.selection = bot.id; model.route = .chat }
                    .contextMenu { BotContextMenu(bot: bot) }
            }
        }
    }
}

/// Pinned bots: a 3-column grid of tiles (58pt avatar over a 13pt label).
@MainActor struct PinnedStrip: View {
    @Environment(AppModel.self) private var model
    let bots: [DatabaseBot]

    var body: some View {
        LazyVGrid(columns: Array(repeating: GridItem(.flexible(), spacing: 6), count: 3), spacing: 6) {
            ForEach(bots) { tile($0) }
        }
    }

    private func tile(_ bot: DatabaseBot) -> some View {
        let selected = model.selection == bot.id
        return Button { model.selection = bot.id; model.route = .chat } label: {
            VStack(spacing: 8) {
                BotAvatarView(bot: bot, size: 58, showsStatus: true)
                    .overlay(alignment: .topTrailing) {
                        if bot.unreadCount > 0 {
                            Circle().fill(Theme.accent).frame(width: 12, height: 12)
                                .overlay(Circle().strokeBorder(selected ? Palette.selection : Palette.sidebar, lineWidth: 2.5))
                                .offset(x: 2, y: -2)
                        }
                    }
                Text(bot.displayName)
                    .font(.sys(size: 13))
                    .foregroundStyle(selected ? Color.primary : Color.primary.opacity(0.85))
                    .lineLimit(1).truncationMode(.tail)
            }
            .padding(.top, 10).padding(.horizontal, 4).padding(.bottom, 12)
            .frame(maxWidth: .infinity)
            .background(selected ? Palette.selection : .clear, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
            .contentShape(RoundedRectangle(cornerRadius: 14))
        }
        .buttonStyle(.plain)
        .contextMenu { BotContextMenu(bot: bot) }
        .accessibilityLabel("\(bot.displayName), \(bot.status.label)\(bot.unreadCount > 0 ? ", unread" : "")")
    }
}

@MainActor struct BotRow: View {
    let bot: DatabaseBot
    var selected = false
    var inspecting = false
    var compact = false
    var showsPreview = true
    @State private var hovering = false

    var body: some View {
        HStack(spacing: 12) {
            BotAvatarView(bot: bot, size: compact || !showsPreview ? 28 : 36, showsStatus: true)
            VStack(alignment: .leading, spacing: 1) {
                HStack(spacing: 5) {
                    Text(bot.displayName)
                        .font(.sys(size: 15, weight: bot.unreadCount > 0 ? .semibold : .regular))
                        .lineLimit(1)
                    if bot.isDemo { DemoBadge() }
                    if bot.muted { Image(systemName: "bell.slash").font(.sys(size: 10)).foregroundStyle(.tertiary) }
                }
                if showsPreview {
                    Text(subtitle).font(.sys(size: 13)).foregroundStyle(.secondary).lineLimit(1).truncationMode(.tail)
                }
            }
            Spacer(minLength: 4)
            if inspecting { ProgressView().controlSize(.mini) }
            else if bot.unreadCount > 0 { Circle().fill(Theme.accent).frame(width: 8, height: 8) }
        }
        .padding(compact ? 5 : 8)
        .background(RoundedRectangle(cornerRadius: 12, style: .continuous)
            .fill(selected ? Palette.selection : (hovering ? Palette.hover : .clear)))
        .contentShape(RoundedRectangle(cornerRadius: 12))
        .onHover { hovering = $0 }
        .accessibilityElement(children: .combine)
        .accessibilityAddTraits(selected ? [.isSelected, .isButton] : .isButton)
    }

    private var subtitle: String {
        if inspecting { return "Inspecting…" }
        if !bot.schedule.enabled { return "Monitoring paused" }
        return bot.lastSummary ?? "Not inspected yet"
    }
}

@MainActor struct BotContextMenu: View {
    @Environment(AppModel.self) private var model
    let bot: DatabaseBot
    var body: some View {
        Button("Run Inspection") { Task { await model.inspect(bot.id, reason: .manual) } }.disabled(bot.isDemo)
        Divider()
        Button(bot.pinned ? "Unpin" : "Pin to Top") { model.mutate(bot.id) { $0.pinned.toggle() } }
        let ids = model.groups.first { $0.bots.contains { $0.id == bot.id } }?.bots.map(\.id) ?? []
        if let i = ids.firstIndex(of: bot.id) {
            Button("Move Up") { model.move(from: IndexSet(integer: i), to: i - 1, within: ids) }.disabled(i == 0)
            Button("Move Down") { model.move(from: IndexSet(integer: i), to: i + 2, within: ids) }.disabled(i == ids.count - 1)
        }
        Menu("Color") {
            ForEach(BotColor.allCases) { c in
                Button { model.mutate(bot.id) { $0.avatar.color = c } } label: { Label(c.label, systemImage: bot.avatar.color == c ? "checkmark" : "circle.fill") }
            }
        }
        Menu("Move to Group") {
            ForEach(DatabaseEnvironment.allCases.filter { $0 != .custom }) { e in
                Button(e.label) { model.mutate(bot.id) { $0.environment = e; $0.customGroup = nil } }
            }
        }
        Button(bot.muted ? "Unmute Notifications" : "Mute Notifications") { model.mutate(bot.id) { $0.muted.toggle() } }
        Button(bot.schedule.enabled ? "Pause Monitoring" : "Resume Monitoring") {
            model.mutate(bot.id) { $0.schedule.enabled.toggle(); $0.status = $0.schedule.enabled ? .unknown : .paused }
        }
        Divider()
        Button("Settings…") { model.settingsForBot = bot }
        Divider()
        Button("Remove Database…", role: .destructive) { confirmRemove(bot) }
    }

    private func confirmRemove(_ bot: DatabaseBot) {
        let alert = NSAlert()
        alert.messageText = "Remove “\(bot.displayName)”?"
        alert.informativeText = "This deletes its conversation, history and saved password from this Mac. The database itself is not touched."
        alert.addButton(withTitle: "Remove")
        alert.addButton(withTitle: "Cancel")
        alert.buttons.first?.hasDestructiveAction = true
        if alert.runModal() == .alertFirstButtonReturn { model.remove(bot.id) }
    }
}

@MainActor struct SidebarFooter: View {
    @Environment(AppModel.self) private var model
    @State private var hoveringAdd = false

    var body: some View {
        @Bindable var model = model
        HStack(spacing: 8) {
            Button { model.showingAddDatabase = true } label: {
                HStack(spacing: 10) {
                    Image(systemName: "plus").font(.system(size: 13, weight: .medium))
                        .frame(width: 30, height: 30)
                        .background(Palette.control, in: Circle())
                    Text("Add database").font(.sys(size: 15))
                }
                .padding(.vertical, 4).padding(.horizontal, 6).padding(.trailing, 6)
                .background(hoveringAdd ? Palette.hover : .clear, in: Capsule())
                .contentShape(Capsule())
                .onHover { hoveringAdd = $0 }
            }
            .buttonStyle(.plain)

            Spacer()
            CircleButton(symbol: "bell", help: "Notifications", size: 30, badge: model.unreadNotifications > 0) {
                model.showingNotifications.toggle()
            }
            .popover(isPresented: $model.showingNotifications, arrowEdge: .top) { NotificationCenterView() }
            Menu {
                if let s = model.session {
                    Text(s.user.name)
                    if let e = s.user.email { Text(e) }
                    Divider()
                }
                Button("Settings…") { openAppSettings() }
                Button(model.monitoringPaused ? "Resume Monitoring" : "Pause Monitoring") { model.setMonitoringPaused(!model.monitoringPaused) }
                Button(model.demoMode ? "Exit Demo Mode" : "Explore Demo Mode") { model.setDemoMode(!model.demoMode) }
                Divider()
                Button("Switch Account…") { Task { await model.signOut() } }
                Button("Sign Out") { Task { await model.signOut() } }
            } label: {
                ProfileAvatar(name: model.session?.user.name ?? "?", size: 30)
            }
            .menuStyle(.button).buttonStyle(.plain).menuIndicator(.hidden).fixedSize()
            .help(model.session?.user.name ?? "Account")
        }
        .padding(.horizontal, 10).padding(.vertical, 12)
    }
}

@MainActor struct ProfileAvatar: View {
    let name: String
    var size: CGFloat = 26
    var body: some View {
        Text(String(name.split(separator: " ").compactMap(\.first).prefix(2)).uppercased())
            .font(.sys(size: size * 0.38, weight: .semibold))
            .foregroundStyle(.white)
            .frame(width: size, height: size)
            .background(Circle().fill(LinearGradient(colors: [Color(red: 0.45, green: 0.6, blue: 0.98), Color(red: 0.3, green: 0.45, blue: 0.9)],
                                                     startPoint: .top, endPoint: .bottom)))
    }
}

/// ⌘K: jump to any database or into a past message.
@MainActor struct SearchPalette: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    @State private var query = ""
    @FocusState private var focused: Bool

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 10) {
                Image(systemName: "magnifyingglass").foregroundStyle(.secondary)
                TextField("Search databases and conversations", text: $query)
                    .textFieldStyle(.plain).font(.sys(size: 17)).focused($focused)
                    .onSubmit { if let first = results.first { go(first.bot, first.message) } }
            }
            .padding(18)
            Divider().opacity(0.5)
            ScrollView {
                LazyVStack(spacing: 2) {
                    ForEach(results, id: \.key) { r in
                        Button { go(r.bot, r.message) } label: {
                            HStack(spacing: 12) {
                                BotAvatarView(bot: r.bot, size: 30)
                                VStack(alignment: .leading, spacing: 1) {
                                    Text(r.bot.displayName).font(.sys(size: 14))
                                    Text(r.message.map { Format.sqlPreview($0.plainText, limit: 90) } ?? r.bot.connection.safeSummary)
                                        .font(Typo.small).foregroundStyle(.secondary).lineLimit(1)
                                }
                                Spacer()
                            }
                            .padding(.horizontal, 10).padding(.vertical, 7)
                            .contentShape(Rectangle())
                        }
                        .buttonStyle(.plain)
                        .hoverHighlight(cornerRadius: 10)
                    }
                }
                .padding(8)
            }
        }
        .frame(width: 580, height: 420)
        .background(Palette.canvas)
        .onAppear { focused = true }
        .onExitCommand { dismiss() }
    }

    struct Result { let key: String; let bot: DatabaseBot; let message: ChatMessage? }

    private var results: [Result] {
        let q = query.trimmingCharacters(in: .whitespaces)
        let bots = model.bots.filter { q.isEmpty || $0.displayName.localizedCaseInsensitiveContains(q) || $0.connection.host.localizedCaseInsensitiveContains(q) }
            .map { Result(key: $0.id.uuidString, bot: $0, message: nil) }
        guard q.count >= 2 else { return bots }
        let msgs = model.bots.flatMap { b in
            model.messages(for: b.id).filter { $0.plainText.localizedCaseInsensitiveContains(q) }.suffix(5)
                .map { Result(key: $0.id.uuidString, bot: b, message: $0) }
        }
        return bots + msgs
    }

    private func go(_ bot: DatabaseBot, _ m: ChatMessage?) {
        model.selection = bot.id; model.route = .chat; model.scrollTarget = m?.id
        dismiss()
    }
}
