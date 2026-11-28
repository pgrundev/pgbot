import PGBotKit
import SwiftUI

@MainActor struct ConversationView: View {
    @Environment(AppModel.self) private var model
    let bot: DatabaseBot
    @State private var search = ""
    @State private var searching = false
    @State private var atBottom = true

    var body: some View {
        let all = model.messages(for: bot.id)
        let shown = search.isEmpty ? all : all.filter { $0.plainText.localizedCaseInsensitiveContains(search) }
        ZStack(alignment: .bottom) {
            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: model.settings.density == .compact ? 6 : 10) {
                        Color.clear.frame(height: searching ? 48 : 4)
                        if all.isEmpty { emptyState }
                        ForEach(Array(shown.enumerated()), id: \.element.id) { i, m in
                            if model.settings.showTimestamps && (i == 0 || m.createdAt.timeIntervalSince(shown[i - 1].createdAt) > 15 * 60) {
                                Text(conversationStamp(m.createdAt))
                                    .font(Typo.small).foregroundStyle(.secondary)
                                    .frame(maxWidth: .infinity).padding(.vertical, 6)
                            }
                            MessageView(message: m, bot: bot).id(m.id)
                        }
                        if model.generating[bot.id] != nil || model.inspecting.contains(bot.id) {
                            ThinkingRow(bot: bot, label: model.inspecting.contains(bot.id) ? "Inspecting the database…"
                                        : (model.isPSQL(bot.id) ? "Running…" : "Thinking…"))
                        }
                        Color.clear.frame(height: 110).id("bottom")
                            .onAppear { atBottom = true }
                            .onDisappear { atBottom = false }
                    }
                    .frame(maxWidth: readableWidth)
                    .padding(.horizontal, 22)
                    .frame(maxWidth: .infinity)
                }
                .scrollIndicators(.automatic)
                .defaultScrollAnchor(.bottom)
                .onChange(of: model.scrollTarget) { _, target in
                    guard let target else { return }
                    withAnimation(.snappy) { proxy.scrollTo(target, anchor: .center) }
                }
                .onChange(of: all.count) { _, _ in withAnimation(.snappy) { proxy.scrollTo("bottom", anchor: .bottom) } }
                .onChange(of: model.generating[bot.id] != nil) { _, _ in withAnimation { proxy.scrollTo("bottom", anchor: .bottom) } }
                .onAppear { proxy.scrollTo(model.scrollTarget ?? all.last?.id, anchor: .bottom) }
                .overlay(alignment: .bottom) {
                    if !atBottom {
                        Button { withAnimation(.snappy) { proxy.scrollTo("bottom", anchor: .bottom) } } label: {
                            CircleIcon(symbol: "arrow.down", size: 34)
                        }
                        .buttonStyle(.plain).help("Scroll to latest")
                        .padding(.bottom, 96)
                        .transition(.opacity.combined(with: .scale(scale: 0.9)))
                    }
                }
                .animation(.snappy(duration: 0.2), value: atBottom)
            }
            // Fade the conversation out under the composer.
            LinearGradient(colors: [Palette.canvas.opacity(0), Palette.canvas], startPoint: .top, endPoint: .bottom)
                .frame(height: 70).allowsHitTesting(false)
            ChatComposer(bot: bot)
        }
        .overlay(alignment: .top) {
            if searching {
                HStack(spacing: 8) {
                    Image(systemName: "magnifyingglass").foregroundStyle(.secondary)
                    TextField("Search this conversation", text: $search).textFieldStyle(.plain)
                    Text("\(shown.count) of \(all.count)").font(Typo.micro).foregroundStyle(.secondary)
                    Button { searching = false; search = "" } label: { Image(systemName: "xmark.circle.fill") }
                        .buttonStyle(.plain).foregroundStyle(.secondary)
                }
                .frame(width: 380)
                .floatingPill(height: 34)
                .padding(.top, 6)
            }
        }
        .background {
            Button("") { searching.toggle(); if !searching { search = "" } }.keyboardShortcut("f").hidden()
        }
    }

    private var emptyState: some View {
        VStack(spacing: Space.m) {
            BotAvatarView(bot: bot, size: 72)
            Text(bot.displayName).font(.sys(size: 22, weight: .semibold))
            Text("Ask anything about this database, or pick an action from +.")
                .font(Typo.body).foregroundStyle(.secondary)
            Button("Run health check") { model.run(.healthCheck, on: bot.id) }.buttonStyle(.pill)
        }
        .frame(maxWidth: .infinity).padding(.top, 120)
    }
}

@MainActor struct ThinkingRow: View {
    @Environment(AppModel.self) private var model
    let bot: DatabaseBot
    let label: String
    @State private var started = Date()
    var body: some View {
        HStack(spacing: 10) {
            ProgressView().controlSize(.small)
            TimelineView(.periodic(from: started, by: 1)) { ctx in
                HStack(spacing: 6) {
                    Text(label)
                    Text("\(Int(ctx.date.timeIntervalSince(started)))s").monospacedDigit().foregroundStyle(.tertiary)
                }
            }
            if model.generating[bot.id] != nil {
                Button("Stop") { model.stopGenerating(bot.id) }.buttonStyle(.pill)
            }
        }
        .font(Typo.body).foregroundStyle(.secondary)
        .padding(.horizontal, 18).padding(.vertical, 12)
        .background(Palette.bubble, in: RoundedRectangle(cornerRadius: 20, style: .continuous))
    }
}

// MARK: - Composer

@MainActor struct ChatComposer: View {
    @Environment(AppModel.self) private var model
    let bot: DatabaseBot
    @State private var text = ""
    @State private var historyIndex: Int?
    @FocusState private var focused: Bool

    var body: some View {
        let psql = model.isPSQL(bot.id)
        VStack(spacing: 8) {
            if psql { psqlBar }
            field(psql: psql)
        }
        .frame(maxWidth: readableWidth + 44)
        .padding(.horizontal, 22).padding(.bottom, 18)
        .onAppear { focused = true }
        .onChange(of: psql) { _, _ in focused = true; historyIndex = nil }
    }

    /// "PSQL · read-only" bar above the console input.
    private var psqlBar: some View {
        HStack(spacing: 10) {
            Label("PSQL", systemImage: "terminal").font(.sys(size: 12, weight: .semibold, design: .monospaced))
                .foregroundStyle(Palette.healthy)
            Text(verbatim: "read-only · end with ; · ↑↓ history · \\? help").font(Typo.micro).foregroundStyle(.secondary)
            Spacer()
            Button("Exit") { model.setPSQLMode(false, for: bot.id) }.buttonStyle(.pillSmall)
        }
        .padding(.horizontal, 10)
    }

    private func field(psql: Bool) -> some View {
        let busy = model.generating[bot.id] != nil
        return HStack(alignment: .bottom, spacing: 10) {
            Menu {
                Button { model.setPSQLMode(!psql, for: bot.id) } label: {
                    Label(psql ? "Exit PSQL Mode" : "PSQL Mode", systemImage: "terminal")
                }
                Divider()
                ForEach(QuickAction.allCases) { a in
                    Button { model.run(a, on: bot.id) } label: { Label(a.label, systemImage: a.symbol) }
                }
                Divider()
                AIModelMenu()
            } label: {
                Image(systemName: "plus").font(.system(size: 16, weight: .medium)).foregroundStyle(.primary.opacity(0.85))
                    .frame(width: 38, height: 38)
                    .background(Palette.control, in: Circle())
                    .contentShape(Circle())
            }
            .menuStyle(.button).buttonStyle(.plain).menuIndicator(.hidden).fixedSize()
            .help("Actions and model")
            .accessibilityLabel("Actions")

            ComposerModeSwitch(psql: psql) { model.setPSQLMode($0, for: bot.id) }

            if psql {
                Text("\(bot.connection.database)=>")
                    .font(.sys(size: 14, weight: .medium, design: .monospaced))
                    .foregroundStyle(Palette.healthy)
                    .padding(.vertical, 9)
            }
            TextField(psql ? "SELECT … ;   or   \\dt" : "Ask your database…", text: $text, axis: .vertical)
                .textFieldStyle(.plain)
                .font(psql ? .sys(size: 14, design: .monospaced) : Typo.body)
                .lineLimit(1...12)
                .padding(.vertical, 9)
                .focused($focused)
                .onKeyPress(.return, phases: .down) { press in
                    if press.modifiers.contains(.shift) || press.modifiers.contains(.option) { text += "\n"; return .handled }
                    if press.modifiers.contains(.command) { submit(); return .handled }
                    // PSQL mode follows psql: Return runs a meta-command or a statement ending in ";".
                    if psql { if PSQL.isComplete(text) { submit() } else { text += "\n" }; return .handled }
                    if model.settings.sendWithReturn { submit(); return .handled }
                    text += "\n"; return .handled
                }
                .onKeyPress(.upArrow) { psql ? browseHistory(-1) : .ignored }
                .onKeyPress(.downArrow) { psql ? browseHistory(1) : .ignored }
                .onKeyPress(.escape) { if busy { model.stopGenerating(bot.id); return .handled }; return .ignored }
                .accessibilityLabel("Message \(bot.displayName)")

            if busy {
                Button { model.stopGenerating(bot.id) } label: {
                    Image(systemName: "stop.fill").font(.system(size: 12)).foregroundStyle(Palette.onPrimary)
                        .frame(width: 38, height: 38).background(Palette.primaryFill, in: Circle())
                }
                .buttonStyle(.plain).help("Stop (Esc)").accessibilityLabel("Stop generating")
            } else {
                Button(action: submit) {
                    Image(systemName: psql ? "return" : "arrow.up").font(.system(size: 15, weight: .semibold))
                        .foregroundStyle(Palette.onPrimary)
                        .frame(width: 38, height: 38)
                        .background(Palette.primaryFill, in: Circle())
                        .opacity(canSend ? 1 : 0.35)
                }
                .buttonStyle(.plain).disabled(!canSend)
                .help(psql ? "Run (Return after ;  or ⌘Return)" : (model.settings.sendWithReturn ? "Send (Return)" : "Send (⌘Return)")).accessibilityLabel("Send")
            }
        }
        .padding(8)
        .frame(minHeight: 54)
        .background(Palette.floating, in: RoundedRectangle(cornerRadius: 27, style: .continuous))
        .overlay(RoundedRectangle(cornerRadius: 27, style: .continuous)
            .strokeBorder(psql ? Palette.healthy.opacity(0.45) : Palette.hairline.opacity(1.25)))
    }

    /// ↑/↓ walk the database's PSQL history when the input is a single line.
    private func browseHistory(_ step: Int) -> KeyPress.Result {
        let history = model.psqlHistory[bot.id] ?? []
        guard !history.isEmpty, !text.contains("\n") || historyIndex != nil else { return .ignored }
        let current = historyIndex ?? history.count
        let next = current + step
        if next < 0 { return .handled }
        if next >= history.count { historyIndex = nil; text = ""; return .handled }
        historyIndex = next
        text = history[next]
        return .handled
    }

    private var canSend: Bool { !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty }

    private func submit() {
        guard canSend, model.generating[bot.id] == nil else { return }
        if model.isPSQL(bot.id) { model.runPSQL(text, on: bot.id) } else { model.send(text, to: bot.id) }
        text = ""
        historyIndex = nil
    }
}

/// Chat | PSQL, next to +: shows which mode the composer is in and switches it.
/// The active segment is a raised pill (green for PSQL); the other stays quiet.
@MainActor struct ComposerModeSwitch: View {
    let psql: Bool
    let set: (Bool) -> Void
    @Namespace private var pill

    var body: some View {
        HStack(spacing: 2) {
            segment("Chat", symbol: "bubble.left", active: !psql, tint: .primary) { set(false) }
                .help("Ask in plain language")
            segment("PSQL", symbol: "terminal", active: psql, tint: Palette.healthy, mono: true) { set(true) }
                .help("Run read-only SQL and \\d commands (⌘⇧P)")
        }
        .padding(3)
        .frame(height: 38)
        .background(Palette.control, in: Capsule())
        .animation(.snappy(duration: 0.22), value: psql)
        .accessibilityElement(children: .contain)
        .accessibilityLabel("Composer mode")
    }

    private func segment(_ title: String, symbol: String, active: Bool, tint: Color, mono: Bool = false,
                         action: @escaping () -> Void) -> some View {
        ModeSegment(title: title, symbol: symbol, active: active, tint: tint, mono: mono, pill: pill, action: action)
    }
}

@MainActor private struct ModeSegment: View {
    let title: String, symbol: String, active: Bool, tint: Color, mono: Bool
    let pill: Namespace.ID
    let action: () -> Void
    @State private var hover = false

    var body: some View {
        Button(action: action) {
            HStack(spacing: 5) {
                Image(systemName: symbol).font(.system(size: 11.5, weight: .semibold))
                Text(title).font(.sys(size: 12, weight: .semibold, design: mono ? .monospaced : .default))
            }
            .foregroundStyle(active ? AnyShapeStyle(tint) : AnyShapeStyle(.secondary.opacity(hover ? 1 : 0.8)))
            .padding(.horizontal, 10)
            .frame(maxHeight: .infinity)
            .background {
                if active {
                    Capsule().fill(tint == .primary ? Palette.floating : Palette.healthy.opacity(0.16))
                        .overlay(Capsule().strokeBorder(tint == .primary ? Palette.hairline.opacity(1.7) : Palette.healthy.opacity(0.45)))
                        .shadow(color: .black.opacity(0.18), radius: 2, y: 1)
                        .matchedGeometryEffect(id: "pill", in: pill)
                }
            }
            .contentShape(Capsule())
        }
        .buttonStyle(.plain)
        .onHover { hover = $0 }
        .accessibilityLabel(title)
        .accessibilityAddTraits(active ? .isSelected : [])
    }
}

/// The model choice lives in the + menu instead of cluttering the composer.
@MainActor struct AIModelMenu: View {
    @Environment(AppModel.self) private var model
    var body: some View {
        Menu("Model: \(model.settings.ai.model.isEmpty ? model.settings.ai.provider.label : model.settings.ai.model)") {
            ForEach(AIProviderKind.allCases) { p in
                Menu(p.label) {
                    ForEach(p.suggestedModels.isEmpty ? ["Default"] : p.suggestedModels, id: \.self) { m in
                        Button {
                            model.settings.ai.provider = p
                            model.settings.ai.model = m == "Default" ? "" : m
                        } label: {
                            if model.settings.ai.provider == p && (model.settings.ai.model == m || (m == "Default" && model.settings.ai.model.isEmpty)) {
                                Label(m, systemImage: "checkmark")
                            } else { Text(m) }
                        }
                    }
                }
            }
            Divider()
            Button("AI Settings…") { openAppSettings(tab: "ai") }
        }
    }
}
