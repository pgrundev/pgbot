import AppKit
import PGBotKit
import SwiftUI
import UserNotifications

@main
struct PGBotApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    @State private var model = AppModel()

    var body: some Scene {
        Window("PGBot", id: "main") {
            RootView()
                // Text size and accent are read from Theme globals; rebuilding on change applies them everywhere.
                .id("\(model.settings.textSize.rawValue)-\(model.settings.accent.rawValue)-\(model.settings.bubbleStyle.rawValue)-\(model.settings.avatarStyle.rawValue)")
                .environment(model)
                .tint(model.settings.accent.color)
                .preferredColorScheme(model.settings.colorScheme)
                .task {
                    delegate.model = model
                    await model.start()
                }
        }
        .defaultSize(width: 1180, height: 780)
        .windowStyle(.hiddenTitleBar)
        .commands { AppCommands(model: model) }

        Window("PGBot Settings", id: "settings") {
            AppSettingsView()
                .environment(model)
                .tint(model.settings.accent.color)
                .preferredColorScheme(model.settings.colorScheme)
        }
        .windowResizability(.contentSize)
        .defaultPosition(.center)

        // Read-only binding: SwiftUI calls the setter while inserting the item, and
        // writing settings from there re-evaluates this body in a loop.
        MenuBarExtra(isInserted: Binding(get: { model.settings.showMenuBarIcon }, set: { _ in })) {
            MenuBarContent().environment(model)
        } label: {
            Image(systemName: menuBarSymbol)
        }
        .menuBarExtraStyle(.window)
    }

    private var menuBarSymbol: String {
        if model.bots.contains(where: { $0.status == .critical || $0.status == .unreachable }) { return "exclamationmark.circle.fill" }
        if model.bots.contains(where: { $0.status == .degraded }) { return "exclamationmark.circle" }
        return "circle.hexagonpath"
    }
}

/// `Commands.body` is nonisolated in the macOS 14 SDK, but menu actions and
/// their state reads always happen on the main thread — so each touch of the
/// model asserts that instead of hopping.
struct AppCommands: Commands {
    let model: AppModel
    @Environment(\.openWindow) private var openWindow

    private func onMain<T: Sendable>(_ body: @MainActor () -> T) -> T { MainActor.assumeIsolated(body) }

    var body: some Commands {
        let signedIn = onMain { model.session != nil }
        let hasSelection = onMain { model.selection != nil }
        let paused = onMain { model.monitoringPaused }
        CommandGroup(replacing: .appSettings) {
            Button("Settings…") { onMain { openWindow(id: "settings") } }.keyboardShortcut(",")
        }
        CommandGroup(replacing: .newItem) {
            Button("Add Database…") { onMain { openWindow(id: "main"); model.showingAddDatabase = true } }
                .keyboardShortcut("n")
                .disabled(!signedIn)
        }
        CommandMenu("Database") {
            Button("Search Databases") { onMain { model.showingSearch = true } }.keyboardShortcut("k")
            Divider()
            Button("Run Inspection") { onMain { if let id = model.selection { Task { await model.inspect(id, reason: .manual) } } } }
                .keyboardShortcut("r").disabled(!hasSelection)
            Button("Run All Inspections") { onMain { model.runAllInspections() } }.keyboardShortcut("r", modifiers: [.command, .shift])
            Divider()
            Button("Conversation") { onMain { model.route = .chat } }.keyboardShortcut("1")
            Button("Metrics") { onMain { model.route = .metrics } }.keyboardShortcut("2")
            Button("Logs") { onMain { model.route = .logs } }.keyboardShortcut("3")
            Button("PSQL Mode") { onMain { if let id = model.selection { model.route = .chat; model.setPSQLMode(!model.isPSQL(id), for: id) } } }
                .keyboardShortcut("p", modifiers: [.command, .shift]).disabled(!hasSelection)
            Divider()
            Button("Notifications") { onMain { model.showingNotifications.toggle() } }.keyboardShortcut("n", modifiers: [.command, .shift])
            Button(paused ? "Resume Monitoring" : "Pause Monitoring") { onMain { model.setMonitoringPaused(!model.monitoringPaused) } }
        }
    }
}

/// Keeps monitoring alive with the window closed (when the menu bar icon is on)
/// and routes notification clicks back into the app.
final class AppDelegate: NSObject, NSApplicationDelegate, UNUserNotificationCenterDelegate {
    weak var model: AppModel?

    func applicationDidFinishLaunching(_ notification: Notification) {
        if SystemNotifications.available { UNUserNotificationCenter.current().delegate = self }
        // Started from `swift run` (no bundle) the process is not a regular app yet.
        if Bundle.main.bundleIdentifier == nil { NSApp.setActivationPolicy(.regular); NSApp.activate(ignoringOtherApps: true) }
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        MainActor.assumeIsolated { !(model?.settings.showMenuBarIcon ?? false) }
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse) async {
        let id = response.notification.request.content.userInfo["notificationID"] as? String
        await MainActor.run {
            NSApp.activate(ignoringOtherApps: true)
            if let id { model?.open(notificationID: id) }
        }
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification) async -> UNNotificationPresentationOptions {
        [.banner, .list, .sound]
    }
}
