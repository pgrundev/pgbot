import PGBotKit
import SwiftUI

/// Per-bot settings. Edits a draft and saves on "Done", so Cancel really cancels.
@MainActor struct DatabaseSettingsView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let botID: UUID
    @State private var draft: DatabaseBot?
    @State private var newPassword = ""
    @State private var testing = false
    @State private var testResult: String?

    var body: some View {
        VStack(spacing: 0) {
            if let binding = Binding($draft) {
                TabView {
                    general(binding).tabItem { Text("General") }
                    connection(binding).tabItem { Text("Connection") }
                    monitoring(binding).tabItem { Text("Monitoring") }
                    notifications(binding).tabItem { Text("Notifications") }
                    ai.tabItem { Text("AI") }
                    advanced(binding).tabItem { Text("Advanced") }
                }
                .padding(Space.l)
            }
            Divider()
            HStack {
                Button("Cancel", role: .cancel) { dismiss() }.keyboardShortcut(.cancelAction)
                Spacer()
                Button("Done") { save() }.keyboardShortcut(.defaultAction).buttonStyle(.borderedProminent)
            }
            .padding(Space.l)
        }
        .frame(width: 600, height: 560)
        .onAppear { draft = model.bot(botID) }
    }

    private func save() {
        guard let d = draft else { return }
        if !newPassword.isEmpty { model.setPassword(newPassword, for: d) }
        model.update(d)
        dismiss()
    }

    private func general(_ b: Binding<DatabaseBot>) -> some View {
        Form {
            LabeledContent("Avatar") { ElephantAvatar(color: b.wrappedValue.avatar.color, shape: b.wrappedValue.avatar.shape, size: 48) }
            TextField("Display name", text: b.displayName)
            TextField("Description", text: b.description)
            LabeledContent("Color") { ColorSwatches(selection: b.avatar.color, shape: b.wrappedValue.avatar.shape) }
            Picker("Shape", selection: b.avatar.shape) {
                Text("Rounded square").tag(AvatarShape.roundedSquare); Text("Circle").tag(AvatarShape.circle)
            }.pickerStyle(.segmented)
            Picker("Environment", selection: b.environment) { ForEach(DatabaseEnvironment.allCases) { Text($0.label).tag($0) } }
            TextField("Custom group", text: Binding(get: { b.wrappedValue.customGroup ?? "" },
                                                     set: { b.wrappedValue.customGroup = $0.isEmpty ? nil : $0 }), prompt: Text("Uses the environment"))
            Toggle("Pinned", isOn: b.pinned)
        }
        .formStyle(.grouped)
    }

    private func connection(_ b: Binding<DatabaseBot>) -> some View {
        Form {
            TextField("Host", text: b.connection.host)
            TextField("Port", value: b.connection.port, format: .number.grouping(.never))
            TextField("Database", text: b.connection.database)
            TextField("Username", text: b.connection.username)
            SecureField("New password", text: $newPassword, prompt: Text("Unchanged (stored in Keychain)"))
            Picker("SSL mode", selection: b.connection.sslMode) { ForEach(SSLMode.allCases) { Text($0.rawValue).tag($0) } }
            Stepper("Connection timeout: \(b.wrappedValue.connection.connectTimeoutSeconds)s", value: b.connection.connectTimeoutSeconds, in: 2...120)
            Section {
                HStack {
                    Button("Test Connection") { Task { await test(b.wrappedValue) } }.disabled(testing || b.wrappedValue.isDemo)
                    if testing { ProgressView().controlSize(.small) }
                    if let r = testResult { Text(r).font(.callout).foregroundStyle(.secondary) }
                }
                if let ctx = model.latest[botID]?.context {
                    LabeledContent("Monitoring permissions", value: ctx.server.hasPgMonitor == true ? "pg_monitor granted" : "pg_monitor missing")
                    LabeledContent("pg_stat_statements", value: ctx.queries?.isAvailable == true ? "Enabled" : "Not available")
                }
            }
        }
        .formStyle(.grouped)
    }

    private func monitoring(_ b: Binding<DatabaseBot>) -> some View {
        Form {
            Toggle("Monitor this database", isOn: b.schedule.enabled)
            Picker("Inspect", selection: b.schedule.intervalSeconds) {
                ForEach(MonitoringSchedule.intervalChoices, id: \.1) { Text($0.0).tag($0.1) }
            }
            Picker("Daily report", selection: b.schedule.dailyReportHour) {
                Text("Off").tag(Int?.none)
                ForEach([6, 7, 8, 9, 10, 12, 17], id: \.self) { h in
                    Text(Calendar.current.date(bySettingHour: h, minute: 0, second: 0, of: Date())!.formatted(date: .omitted, time: .shortened)).tag(Int?.some(h))
                }
            }
            Picker("Keep local history", selection: b.schedule.retentionDays) {
                ForEach([7, 30, 90, 365], id: \.self) { Text("\($0) days").tag($0) }
            }
            Section("Ignored findings") {
                if b.wrappedValue.schedule.ignoredFindingKeys.isEmpty {
                    Text("None. Use “Ignore” on a finding to hide it for this database.").foregroundStyle(.secondary)
                }
                ForEach(Array(b.wrappedValue.schedule.ignoredFindingKeys).sorted(), id: \.self) { k in
                    HStack {
                        Text(k).font(.system(.callout, design: .monospaced))
                        Spacer()
                        Button("Restore") { b.wrappedValue.schedule.ignoredFindingKeys.remove(k) }.buttonStyle(.link)
                    }
                }
            }
            Text("Local monitoring runs while PGBot is open (or in the menu bar) and this Mac is awake. It isn't 24/7 coverage.")
                .font(.caption).foregroundStyle(.secondary)
        }
        .formStyle(.grouped)
    }

    private func notifications(_ b: Binding<DatabaseBot>) -> some View {
        Form {
            Toggle("Mute this database", isOn: b.muted)
            Picker("Notify for", selection: b.schedule.notifyMinimum) {
                Text("Critical findings only").tag(Severity.critical)
                Text("Warnings and critical").tag(Severity.warn)
                Text("Everything").tag(Severity.info)
            }
            Toggle("Recovery notifications", isOn: b.schedule.notifyResolved)
            Text("Quiet hours, sounds and categories are global: Settings → Notifications.").font(.caption).foregroundStyle(.secondary)
        }
        .formStyle(.grouped)
    }

    private var ai: some View {
        Form {
            LabeledContent("Provider", value: model.settings.ai.provider.label)
            LabeledContent("Model", value: model.settings.ai.model.isEmpty ? "Provider default" : model.settings.ai.model)
            Text("AI answers explain PGBot's deterministic findings; the model never connects to the database. Configure providers and keys in Settings → AI providers.")
                .font(.callout).foregroundStyle(.secondary)
            Button("Open AI settings…") { dismiss(); openAppSettings(tab: "ai") }
        }
        .formStyle(.grouped)
    }

    private func advanced(_ b: Binding<DatabaseBot>) -> some View {
        Form {
            Section {
                Button("Export Inspection Report…") { model.exportInspection(botID) }.disabled(model.latest[botID] == nil)
                Button("Export Conversation History…") { model.exportConversation(botID) }
                Button("Clear Local History", role: .destructive) { model.clearConversation(botID) }
            }
            Section {
                Button("Remove Database…", role: .destructive) {
                    let alert = NSAlert()
                    alert.messageText = "Remove “\(b.wrappedValue.displayName)”?"
                    alert.informativeText = "This deletes its conversation, history and saved password from this Mac. The database itself is not touched."
                    alert.addButton(withTitle: "Remove"); alert.addButton(withTitle: "Cancel")
                    if alert.runModal() == .alertFirstButtonReturn { dismiss(); model.remove(botID) }
                }
            }
            Text("Statement timeouts and per-database engine options use the engine's defaults (read-only session, bounded statement time).")
                .font(.caption).foregroundStyle(.secondary)
        }
        .formStyle(.grouped)
    }

    private func test(_ bot: DatabaseBot) async {
        guard let engine = model.engine else { testResult = model.engineProblem?.title; return }
        testing = true; testResult = nil
        defer { testing = false }
        let pw = newPassword.isEmpty ? model.password(for: bot) : newPassword
        do {
            let ctx = try await engine.inspect(EngineTarget(connection: bot.connection, password: pw))
            testResult = "Connected — \(ctx.server.displayVersion)"
        } catch {
            let e = error as? EngineError ?? .failed(error.localizedDescription)
            testResult = "\(e.title). \(e.hint)"
        }
    }
}
