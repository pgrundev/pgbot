import PGBotKit
import SwiftUI

/// The native Settings window (⌘,).
@MainActor struct AppSettingsView: View {
    @AppStorage("settingsTab") private var tab = "account"

    private static let sections: [(id: String, title: String, symbol: String)] = [
        ("account", "Account", "person.crop.circle"),
        ("appearance", "Appearance", "paintbrush"),
        ("defaults", "New Databases", "cylinder.split.1x2"),
        ("monitoring", "Monitoring", "waveform.path.ecg"),
        ("ai", "AI Providers", "sparkles"),
        ("notifications", "Notifications", "bell"),
        ("integrations", "Integrations", "puzzlepiece.extension"),
        ("privacy", "Privacy & Security", "lock.shield"),
        ("about", "About", "info.circle"),
    ]

    var body: some View {
        HStack(spacing: 0) {
            List(selection: Binding(get: { tab }, set: { tab = $0 ?? "account" })) {
                ForEach(Self.sections, id: \.id) { s in
                    Label(s.title, systemImage: s.symbol).tag(s.id)
                }
            }
            .listStyle(.sidebar)
            .frame(width: 200)
            Divider()
            VStack(alignment: .leading, spacing: 0) {
                Text(Self.sections.first { $0.id == tab }?.title ?? "")
                    .font(.system(size: 20, weight: .semibold))
                    .padding(.horizontal, 22).padding(.top, 18)
                detail.frame(maxWidth: .infinity, maxHeight: .infinity)
            }
            .background(Palette.canvas)
        }
        .frame(width: 800, height: 600)
    }

    @ViewBuilder private var detail: some View {
        switch tab {
        case "appearance": AppearanceSettings()
        case "defaults": NewDatabaseSettings()
        case "monitoring": MonitoringSettings()
        case "ai": AIProviderSettings()
        case "notifications": NotificationSettings()
        case "integrations": IntegrationSettings()
        case "privacy": PrivacySettings()
        case "about": AboutSettings()
        default: AccountSettings()
        }
    }
}

@MainActor private struct AccountSettings: View {
    @Environment(AppModel.self) private var model
    var body: some View {
        Form {
            if let s = model.session {
                HStack(spacing: Space.m) {
                    ProfileAvatar(name: s.user.name, size: 48)
                    VStack(alignment: .leading) {
                        Text(s.user.name).font(.headline)
                        Text(s.user.email ?? (s.user.provider == .local ? "Local profile — no account" : "")).foregroundStyle(.secondary)
                    }
                }
                LabeledContent("Signed in with", value: s.user.provider == .local ? "Not signed in (local only)" : s.user.provider.rawValue.capitalized)
                LabeledContent("Plan", value: s.subscription.plan == .pro ? "Pro" : "Free")
                Section {
                    Text("Free includes unlimited local database bots, diagnostics, monitoring, metrics, logs and bring-your-own AI. Pro will add always-on cloud monitoring, Slack/email/webhooks and team features.")
                        .font(.callout).foregroundStyle(.secondary)
                    Link("Manage subscription", destination: URL(string: "https://pgbot.dev/pricing")!)
                }
                Section {
                    Button("Sign Out") { Task { await model.signOut() } }
                }
            }
        }
        .formStyle(.grouped).scrollContentBackground(.hidden)
    }
}

@MainActor private struct AppearanceSettings: View {
    @Environment(AppModel.self) private var model
    var body: some View {
        @Bindable var model = model
        Form {
            Section("Look") {
                Picker("Theme", selection: $model.settings.appearance) {
                    Text("System").tag(AppSettings.Appearance.system)
                    Text("Light").tag(AppSettings.Appearance.light)
                    Text("Dark").tag(AppSettings.Appearance.dark)
                }
                .pickerStyle(.segmented)
                LabeledContent("Accent color") { AccentSwatches(selection: $model.settings.accent) }
                Picker("Avatars", selection: $model.settings.avatarStyle) {
                    ForEach(AvatarStyle.allCases) { Text($0.label).tag($0) }
                }
                .pickerStyle(.segmented)
                Toggle("Animate avatars on hover", isOn: $model.settings.animateAvatars)
                Picker("Text size", selection: $model.settings.textSize) {
                    ForEach(TextSize.allCases) { Text($0.label).tag($0) }
                }
                .pickerStyle(.segmented)
                Picker("Density", selection: $model.settings.density) {
                    ForEach(Density.allCases) { Text($0.label).tag($0) }
                }
                .pickerStyle(.segmented)
            }
            Section("Conversation") {
                Picker("Message style", selection: $model.settings.bubbleStyle) {
                    ForEach(BubbleStyle.allCases) { Text($0.label).tag($0) }
                }
                .pickerStyle(.segmented)
                Toggle("Show timestamps between messages", isOn: $model.settings.showTimestamps)
                Picker("Send messages with", selection: $model.settings.sendWithReturn) {
                    Text("Return").tag(true)
                    Text("⌘ Return").tag(false)
                }
                .pickerStyle(.segmented)
                Text(model.settings.sendWithReturn ? "Shift-Return adds a new line." : "Return adds a new line.")
                    .font(.caption).foregroundStyle(.secondary)
            }
            Section("Sidebar") {
                Toggle("Show pinned databases", isOn: $model.settings.showPinnedBots)
                Toggle("Group databases by environment", isOn: $model.settings.groupByEnvironment)
                Toggle("Show latest finding under each database", isOn: $model.settings.showSidebarPreviews)
                Toggle("Reopen the last database at launch", isOn: $model.settings.restoreLastSelection)
            }
            Section("Menu bar") {
                Toggle("Show menu bar icon", isOn: $model.settings.showMenuBarIcon)
                Text("With the menu bar icon on, PGBot keeps monitoring after you close the window.").font(.caption).foregroundStyle(.secondary)
            }
            Section {
                HStack { Spacer(); Button("Restore Default Appearance") { model.settings.resetAppearance() } }
            }
        }
        .formStyle(.grouped).scrollContentBackground(.hidden)
    }
}

/// Accent color picker in the style of System Settings.
@MainActor private struct AccentSwatches: View {
    @Binding var selection: AccentChoice
    var body: some View {
        HStack(spacing: 7) {
            ForEach(AccentChoice.allCases) { a in
                Button { selection = a } label: {
                    Circle()
                        .fill(a == .system
                              ? AnyShapeStyle(AngularGradient(colors: [.red, .orange, .yellow, .green, .blue, .purple, .red], center: .center))
                              : AnyShapeStyle(a.color))
                        .frame(width: 18, height: 18)
                        .overlay(Circle().fill(.white).frame(width: 6, height: 6).opacity(selection == a ? 1 : 0))
                        .overlay(Circle().strokeBorder(Color.primary.opacity(0.12)))
                }
                .buttonStyle(.plain)
                .help(a.label)
                .accessibilityLabel(a.label)
                .accessibilityAddTraits(selection == a ? .isSelected : [])
            }
        }
    }
}

/// Starting values for the Add Database sheet. Monitoring defaults live under Monitoring.
@MainActor private struct NewDatabaseSettings: View {
    @Environment(AppModel.self) private var model
    var body: some View {
        @Bindable var model = model
        Form {
            Section("Connection") {
                Picker("Environment", selection: $model.settings.defaultEnvironment) {
                    ForEach(DatabaseEnvironment.allCases) { Text($0.label).tag($0) }
                }
                Picker("SSL mode", selection: $model.settings.defaultSSLMode) {
                    ForEach(SSLMode.allCases) { Text($0.rawValue).tag($0) }
                }
                Stepper("Connection timeout: \(model.settings.defaultConnectTimeout)s",
                        value: $model.settings.defaultConnectTimeout, in: 2...120)
                Text("Use “require” or “verify-full” for databases outside this Mac. A pasted connection URL overrides these.")
                    .font(.caption).foregroundStyle(.secondary)
            }
            Section("Bot") {
                Picker("Avatar shape", selection: $model.settings.defaultAvatarShape) {
                    Text("Rounded square").tag(AvatarShape.roundedSquare)
                    Text("Circle").tag(AvatarShape.circle)
                }
                .pickerStyle(.segmented)
                Picker("Avatar color", selection: $model.settings.defaultAvatarColor) {
                    Text("Random").tag(BotColor?.none)
                    Divider()
                    ForEach(BotColor.allCases) { Text($0.label).tag(BotColor?.some($0)) }
                }
                HStack(spacing: 10) {
                    Spacer()
                    ElephantAvatar(color: model.settings.defaultAvatarColor ?? .blue, shape: model.settings.defaultAvatarShape, size: 44)
                    Spacer()
                }
            }
            Section("Monitoring") {
                LabeledContent("Inspection interval, daily report, retention") {
                    Text("Settings → Monitoring").foregroundStyle(.secondary)
                }
            }
        }
        .formStyle(.grouped).scrollContentBackground(.hidden)
    }
}

@MainActor private struct MonitoringSettings: View {
    @Environment(AppModel.self) private var model
    var body: some View {
        @Bindable var model = model
        Form {
            Toggle("Background monitoring", isOn: $model.settings.backgroundMonitoring)
            Picker("Default inspection interval", selection: $model.settings.defaultIntervalSeconds) {
                ForEach(MonitoringSchedule.intervalChoices, id: \.1) { Text($0.0).tag($0.1) }
            }
            Picker("Default daily report", selection: $model.settings.defaultDailyReportHour) {
                Text("Off").tag(Int?.none)
                ForEach([7, 8, 9, 10], id: \.self) { Text("\($0):00").tag(Int?.some($0)) }
            }
            Picker("Default data retention", selection: $model.settings.defaultRetentionDays) {
                ForEach([7, 30, 90, 365], id: \.self) { Text("\($0) days").tag($0) }
            }
            Toggle("Launch at login", isOn: $model.settings.launchAtLogin)
            Section {
                Text("Monitoring is local: it runs while PGBot is open or in the menu bar and this Mac is awake and online. It does not cover time the Mac is asleep or off. Always-on cloud monitoring is planned for Pro.")
                    .font(.callout).foregroundStyle(.secondary)
            }
        }
        .formStyle(.grouped).scrollContentBackground(.hidden)
    }
}

@MainActor private struct AIProviderSettings: View {
    @Environment(AppModel.self) private var model
    @State private var key = ""
    @State private var saved = false

    var body: some View {
        @Bindable var model = model
        Form {
            Picker("Provider", selection: $model.settings.ai.provider) {
                ForEach(AIProviderKind.allCases) { Text($0.label).tag($0) }
            }
            .onChange(of: model.settings.ai.provider) { _, _ in key = ""; saved = false; model.settings.ai.model = "" }
            Picker("Model", selection: $model.settings.ai.model) {
                Text("Provider default").tag("")
                ForEach(model.settings.ai.provider.suggestedModels, id: \.self) { Text($0).tag($0) }
                if !model.settings.ai.model.isEmpty && !model.settings.ai.provider.suggestedModels.contains(model.settings.ai.model) {
                    Text(model.settings.ai.model).tag(model.settings.ai.model)
                }
            }
            TextField("Custom model id", text: $model.settings.ai.model, prompt: Text("Optional"))
            if model.settings.ai.provider.needsBaseURL || model.settings.ai.provider == .ollama {
                TextField("Endpoint", text: $model.settings.ai.baseURL,
                          prompt: Text(model.settings.ai.provider.defaultBaseURL ?? "https://…/v1"))
            }
            if model.settings.ai.provider.needsKey {
                HStack {
                    SecureField("API key", text: $key, prompt: Text(model.hasAPIKey(model.settings.ai.provider) ? "Saved in Keychain" : "Paste your key"))
                    Button("Save") { model.saveAPIKey(key, for: model.settings.ai.provider); key = ""; saved = true }.disabled(key.isEmpty)
                    if model.hasAPIKey(model.settings.ai.provider) {
                        Button("Remove", role: .destructive) { model.saveAPIKey("", for: model.settings.ai.provider); saved = false }
                    }
                }
                if saved { Label("Saved to your Keychain", systemImage: "checkmark").foregroundStyle(.green).font(.caption) }
            }
            Section("Privacy") {
                Text("The engine sends only PII-free findings and your question — never credentials, connection strings, or table contents. Local models (Ollama) keep everything on this Mac.")
                    .font(.callout).foregroundStyle(.secondary)
                if !model.settings.aiConsents.isEmpty {
                    Button("Revoke data-sharing consent (\(model.settings.aiConsents.count))") { model.settings.aiConsents = [] }
                }
            }
        }
        .formStyle(.grouped).scrollContentBackground(.hidden)
    }
}

@MainActor private struct NotificationSettings: View {
    @Environment(AppModel.self) private var model
    @State private var testResult: String?
    @State private var permission: SystemNotifications.Permission = .unknown

    var body: some View {
        @Bindable var model = model
        Form {
            Section {
                Toggle("Allow notifications", isOn: $model.settings.notificationsEnabled)
                    .onChange(of: model.settings.notificationsEnabled) { _, on in
                        if on { model.requestNotificationPermission() }
                        Task { permission = await SystemNotifications.permission() }
                    }
                permissionRow
            } footer: {
                Text("PGBot notifies you when findings appear, get worse, or are resolved — once per change, never repeated for something you've already seen.")
                    .font(.caption).foregroundStyle(.secondary)
            }

            Section("How") {
                Toggle("Show macOS banners", isOn: $model.settings.systemBanners)
                Toggle("Play sound", isOn: $model.settings.notificationSounds)
                    .disabled(!model.settings.systemBanners)
                Toggle("Show details in banners", isOn: $model.settings.showPreviews)
                    .disabled(!model.settings.systemBanners)
                Text(model.settings.showPreviews ? "Banners name the finding, e.g. “Three unused indexes occupy 4.2 GB”."
                     : "Banners only say which database needs a look — useful when sharing your screen.")
                    .font(.caption).foregroundStyle(.secondary)
                Toggle("Badge the Dock icon with unread count", isOn: $model.settings.dockBadge)
                HStack {
                    Button("Send Test Notification") { testResult = model.sendTestNotification() ?? "Sent — look at the top-right corner of your screen." }
                    if let testResult { Text(testResult).font(.caption).foregroundStyle(.secondary) }
                }
            }
            .disabled(!model.settings.notificationsEnabled)

            Section("What") {
                Picker("Notify me about", selection: $model.settings.minimumSeverity) {
                    Text("Critical only").tag(Severity.critical)
                    Text("Warnings and critical").tag(Severity.warn)
                    Text("Everything").tag(Severity.info)
                }
                ForEach(NotificationCategory.allCases, id: \.self) { c in
                    Toggle(isOn: Binding(get: { !model.settings.disabledCategories.contains(c) },
                                         set: { on in if on { model.settings.disabledCategories.remove(c) } else { model.settings.disabledCategories.insert(c) } })) {
                        VStack(alignment: .leading, spacing: 2) {
                            Label(c.label, systemImage: c.symbol)
                            Text(c.detail).font(.caption).foregroundStyle(.secondary)
                        }
                    }
                }
            }
            .disabled(!model.settings.notificationsEnabled)

            Section("When") {
                Toggle("Quiet hours", isOn: $model.settings.quietHours.enabled)
                if model.settings.quietHours.enabled {
                    Picker("From", selection: $model.settings.quietHours.startHour) { ForEach(0..<24, id: \.self) { Text(hourLabel($0)).tag($0) } }
                    Picker("Until", selection: $model.settings.quietHours.endHour) { ForEach(0..<24, id: \.self) { Text(hourLabel($0)).tag($0) } }
                    Toggle("Critical alerts still come through", isOn: $model.settings.criticalBreaksQuietHours)
                }
                Text("Monitoring runs while this Mac is awake. Nothing is checked — or notified — while it sleeps.")
                    .font(.caption).foregroundStyle(.secondary)
            }
            .disabled(!model.settings.notificationsEnabled)

            Section("Per database") {
                if model.bots.isEmpty {
                    Text("Add a database to choose its notifications.").foregroundStyle(.secondary)
                }
                ForEach(model.bots) { bot in
                    Picker(selection: level(bot)) {
                        Text("Everything").tag(Level.everything)
                        Text("Warnings and critical").tag(Level.warnings)
                        Text("Critical only").tag(Level.critical)
                        Divider()
                        Text("Off").tag(Level.off)
                    } label: {
                        HStack(spacing: 10) {
                            BotAvatarView(bot: bot, size: 24)
                            Text(bot.displayName)
                        }
                    }
                }
                if !model.bots.isEmpty {
                    Text("A database's level applies together with “Notify me about” above — the stricter of the two wins.")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            .disabled(!model.settings.notificationsEnabled)
        }
        .formStyle(.grouped).scrollContentBackground(.hidden)
        .task { permission = await SystemNotifications.permission() }
    }

    @ViewBuilder private var permissionRow: some View {
        if !SystemNotifications.available {
            Label("macOS notifications need the installed PGBot.app; this build shows them in PGBot only.", systemImage: "info.circle")
                .font(.callout).foregroundStyle(.secondary)
        } else {
            switch permission {
            case .allowed:
                Label("macOS: allowed", systemImage: "checkmark.circle.fill").foregroundStyle(Palette.healthy)
            case .denied:
                HStack {
                    Label("macOS: blocked in System Settings", systemImage: "xmark.octagon.fill").foregroundStyle(.red)
                    Spacer()
                    Button("Open System Settings") { SystemNotifications.openSystemSettings() }
                }
            case .notAsked:
                HStack {
                    Label("macOS hasn't asked yet", systemImage: "questionmark.circle").foregroundStyle(.secondary)
                    Spacer()
                    Button("Ask Now") {
                        model.requestNotificationPermission()
                        Task { try? await Task.sleep(for: .seconds(1)); permission = await SystemNotifications.permission() }
                    }
                }
            case .unknown:
                EmptyView()
            }
        }
    }

    enum Level: Hashable { case everything, warnings, critical, off }

    private func level(_ bot: DatabaseBot) -> Binding<Level> {
        Binding(
            get: {
                guard let b = model.bot(bot.id) else { return .warnings }
                if b.muted { return .off }
                switch b.schedule.notifyMinimum { case .info: return .everything; case .warn: return .warnings; case .critical: return .critical }
            },
            set: { new in
                model.mutate(bot.id) { b in
                    b.muted = new == .off
                    switch new {
                    case .everything: b.schedule.notifyMinimum = .info
                    case .warnings: b.schedule.notifyMinimum = .warn
                    case .critical: b.schedule.notifyMinimum = .critical
                    case .off: break
                    }
                }
            })
    }

    private func hourLabel(_ h: Int) -> String {
        Calendar.current.date(bySettingHour: h, minute: 0, second: 0, of: Date())!.formatted(date: .omitted, time: .shortened)
    }
}

extension NotificationCategory {
    var symbol: String {
        switch self {
        case .criticalFinding: "exclamationmark.octagon"
        case .warning: "exclamationmark.triangle"
        case .scoreChange: "gauge.with.dots.needle.33percent"
        case .regression: "tortoise"
        case .connectivity: "bolt.horizontal.circle"
        case .resolved: "checkmark.circle"
        case .scheduledReport: "sun.horizon"
        }
    }
    var detail: String {
        switch self {
        case .criticalFinding: "Something needs action now, like wraparound risk or a full disk."
        case .warning: "A new warning, like unused indexes or autovacuum falling behind."
        case .scoreChange: "The health score dropped by 10 points or more."
        case .regression: "A query or workload got noticeably slower."
        case .connectivity: "PGBot couldn't reach the database — and when it's back."
        case .resolved: "A finding you were told about has gone away."
        case .scheduledReport: "The morning health report for each database."
        }
    }
}

@MainActor private struct IntegrationSettings: View {
    var body: some View {
        Form {
            ForEach(["Slack", "Discord", "Email", "GitHub", "Webhooks", "Prometheus", "OpenTelemetry"], id: \.self) { name in
                LabeledContent(name) { Text("Coming soon").foregroundStyle(.tertiary) }
            }
            Text("Integrations deliver the same deduplicated notifications to your team's channels. Prometheus is already available from the CLI: `pgbot inspect --format=prometheus`.")
                .font(.caption).foregroundStyle(.secondary)
        }
        .formStyle(.grouped).scrollContentBackground(.hidden)
    }
}

@MainActor private struct PrivacySettings: View {
    @Environment(AppModel.self) private var model
    var body: some View {
        @Bindable var model = model
        Form {
            Section("Local data") {
                LabeledContent("Conversations & history", value: "~/Library/Application Support/PGBot")
                LabeledContent("Passwords & API keys", value: "macOS Keychain (this Mac only)")
                Button("Show in Finder") {
                    let url = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0].appendingPathComponent("PGBot")
                    NSWorkspace.shared.activateFileViewerSelecting([url])
                }
            }
            Section {
                Toggle("Share anonymous usage statistics", isOn: $model.settings.telemetryOptIn)
                Text("Off by default. Database contents, names, queries and findings are never collected.").font(.caption).foregroundStyle(.secondary)
                LabeledContent("Cloud sync", value: "Off — everything stays on this Mac")
            }
            Section {
                Button("Delete All Local Data…", role: .destructive) {
                    let a = NSAlert()
                    a.messageText = "Delete all local PGBot data?"
                    a.informativeText = "Removes every database bot, conversation, history, setting, saved password and API key from this Mac. Your databases are not touched."
                    a.addButton(withTitle: "Delete"); a.addButton(withTitle: "Cancel")
                    if a.runModal() == .alertFirstButtonReturn { model.deleteAllLocalData() }
                }
            }
        }
        .formStyle(.grouped).scrollContentBackground(.hidden)
    }
}

@MainActor private struct AboutSettings: View {
    @Environment(AppModel.self) private var model
    var body: some View {
        @Bindable var model = model
        Form {
            HStack(spacing: Space.m) {
                PGBotLogo(size: 56)
                VStack(alignment: .leading) {
                    Text("PGBot").font(.title2.weight(.semibold))
                    Text("Every PostgreSQL database deserves its own intelligent assistant.").foregroundStyle(.secondary)
                }
            }
            LabeledContent("App version", value: Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "development")
            LabeledContent("Engine version", value: model.engineVersion ?? (model.engineProblem?.title ?? "…"))
            TextField("Engine path override", text: $model.settings.enginePathOverride, prompt: Text("Bundled engine"))
            Section {
                Link("Documentation", destination: URL(string: "https://pgbot.dev")!)
                Link("GitHub repository", destination: URL(string: "https://github.com/pgrundev/pgbot")!)
                Link("Report an issue", destination: URL(string: "https://github.com/pgrundev/pgbot/issues/new")!)
                Link("License (Apache-2.0)", destination: URL(string: "https://github.com/pgrundev/pgbot/blob/main/LICENSE")!)
            }
        }
        .formStyle(.grouped).scrollContentBackground(.hidden)
    }
}
