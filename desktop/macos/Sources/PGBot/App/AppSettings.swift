import PGBotKit
import SwiftUI

/// Global preferences, persisted in the local store (no secrets here).
struct AppSettings: Codable, Equatable {
    enum Appearance: String, Codable, CaseIterable { case system, light, dark }

    var appearance: Appearance = .dark
    var avatarStyle: AvatarStyle = .emoji
    var accent: AccentChoice = .system
    var textSize: TextSize = .standard
    var density: Density = .comfortable
    var bubbleStyle: BubbleStyle = .bubbles
    var showTimestamps = true
    var sendWithReturn = true
    var showPinnedBots = true
    var groupByEnvironment = true
    var showSidebarPreviews = true
    var restoreLastSelection = true
    // Defaults for newly added databases
    var defaultEnvironment: DatabaseEnvironment = .production
    var defaultSSLMode: SSLMode = .prefer
    var defaultConnectTimeout = 10
    var defaultAvatarShape: AvatarShape = .roundedSquare
    /// nil = pick a random color for each new bot.
    var defaultAvatarColor: BotColor? = nil
    var showMenuBarIcon = true
    var launchAtLogin = false
    var backgroundMonitoring = true
    var defaultIntervalSeconds = 3600
    var defaultDailyReportHour: Int? = 9
    var defaultRetentionDays = 30
    var notificationsEnabled = true
    var notificationSounds = true
    /// macOS banners in Notification Center (off = in-app notification center only).
    var systemBanners = true
    /// Show the finding in the banner; off shows only "New finding on <database>".
    var showPreviews = true
    /// Unread count on the Dock icon.
    var dockBadge = true
    /// Critical findings and outages still alert during quiet hours.
    var criticalBreaksQuietHours = true
    var animateAvatars = true
    var quietHours = QuietHours()
    var minimumSeverity: Severity = .warn
    var disabledCategories: Set<NotificationCategory> = []
    var ai = AIConfiguration()
    /// Providers (by engine name + host) the user has agreed to send findings to.
    var aiConsents: Set<String> = []
    var enginePathOverride = ""
    var telemetryOptIn = false
    var bands = HealthBands()
    var pinnedSectionCollapsed = false
    var collapsedGroups: Set<String> = []

    var colorScheme: ColorScheme? {
        switch appearance { case .system: nil; case .light: .light; case .dark: .dark }
    }

    func consentKey(for ai: AIConfiguration) -> String { "\(ai.provider.engineName)@\(ai.effectiveBaseURL ?? "default")" }

    /// Restores only the look-and-feel options, keeping everything else.
    mutating func resetAppearance() {
        let d = AppSettings()
        appearance = d.appearance; avatarStyle = d.avatarStyle; accent = d.accent; textSize = d.textSize; density = d.density
        bubbleStyle = d.bubbleStyle; showTimestamps = d.showTimestamps; sendWithReturn = d.sendWithReturn
        animateAvatars = d.animateAvatars; showPinnedBots = d.showPinnedBots; groupByEnvironment = d.groupByEnvironment; showSidebarPreviews = d.showSidebarPreviews
    }

    /// Decodes saved settings on top of the current defaults, so options added
    /// in a newer version get their default instead of discarding the file.
    /// These settings with some keys replaced from a JSON object (launch scenarios).
    func overlaid(with json: Data) -> AppSettings {
        guard let current = try? JSONEncoder().encode(self),
              let base = try? JSONSerialization.jsonObject(with: current) as? [String: Any],
              let extra = try? JSONSerialization.jsonObject(with: json) as? [String: Any],
              let merged = try? JSONSerialization.data(withJSONObject: base.merging(extra) { $1 }),
              let out = try? JSONDecoder().decode(AppSettings.self, from: merged) else { return self }
        return out
    }

    static func load(_ data: Data?) -> AppSettings {
        guard let data,
              let stored = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let defaultsData = try? JSONEncoder().encode(AppSettings()),
              let defaults = try? JSONSerialization.jsonObject(with: defaultsData) as? [String: Any],
              let merged = try? JSONSerialization.data(withJSONObject: defaults.merging(stored) { _, saved in saved })
        else { return AppSettings() }
        return (try? JSONDecoder().decode(AppSettings.self, from: merged)) ?? AppSettings()
    }
}

enum AccentChoice: String, Codable, CaseIterable, Identifiable {
    case system, blue, purple, pink, red, orange, yellow, green, teal, graphite
    var id: String { rawValue }
    var label: String { self == .system ? "System" : rawValue.capitalized }
    var color: Color {
        switch self {
        case .system: .accentColor
        case .blue: Color(red: 0.20, green: 0.48, blue: 0.98)
        case .purple: Color(red: 0.56, green: 0.36, blue: 0.95)
        case .pink: Color(red: 0.93, green: 0.30, blue: 0.60)
        case .red: Color(red: 0.92, green: 0.27, blue: 0.27)
        case .orange: Color(red: 0.96, green: 0.50, blue: 0.10)
        case .yellow: Color(red: 0.90, green: 0.68, blue: 0.05)
        case .green: Color(red: 0.18, green: 0.68, blue: 0.38)
        case .teal: Color(red: 0.10, green: 0.66, blue: 0.64)
        case .graphite: Color(red: 0.45, green: 0.47, blue: 0.51)
        }
    }
}

enum TextSize: String, Codable, CaseIterable, Identifiable {
    case small, standard, large, extraLarge
    var id: String { rawValue }
    var label: String { switch self { case .small: "Small"; case .standard: "Default"; case .large: "Large"; case .extraLarge: "Extra Large" } }
    var scale: CGFloat { switch self { case .small: 0.9; case .standard: 1; case .large: 1.12; case .extraLarge: 1.25 } }
}

enum Density: String, Codable, CaseIterable, Identifiable {
    case compact, comfortable
    var id: String { rawValue }
    var label: String { rawValue.capitalized }
}

enum BubbleStyle: String, Codable, CaseIterable, Identifiable {
    case bubbles, plain
    var id: String { rawValue }
    var label: String { self == .bubbles ? "Bubbles" : "Plain text" }
}

enum DetailRoute: Equatable { case chat, metrics, logs }

enum AuthPhase: Equatable {
    case restoring
    case signedOut(error: String?)
    case signingIn(AuthProvider)
    case signedIn(AuthenticationSession)
}

/// The deterministic actions behind the composer's plus menu.
enum QuickAction: String, CaseIterable, Identifiable {
    case healthCheck, queries, indexes, connections, locks, replication, logs, explainSQL, schema, report, whatChanged
    var id: String { rawValue }
    var label: String {
        switch self {
        case .healthCheck: "Run health check"
        case .queries: "Analyze queries"
        case .indexes: "Analyze indexes"
        case .connections: "Show active connections"
        case .locks: "Show database locks"
        case .replication: "Analyze replication"
        case .logs: "Show database logs"
        case .explainSQL: "Explain SQL…"
        case .schema: "Show schema"
        case .report: "Generate database report"
        case .whatChanged: "What changed since yesterday?"
        }
    }
    var symbol: String {
        switch self {
        case .healthCheck: "stethoscope"
        case .queries: "bolt.horizontal"
        case .indexes: "list.bullet.indent"
        case .connections: "person.2"
        case .locks: "lock"
        case .replication: "arrow.triangle.2.circlepath"
        case .logs: "doc.plaintext"
        case .explainSQL: "text.magnifyingglass"
        case .schema: "tablecells"
        case .report: "doc.richtext"
        case .whatChanged: "clock.arrow.circlepath"
        }
    }
}

enum AvatarStyle: String, Codable, CaseIterable, Identifiable {
    case emoji, mascot
    var id: String { rawValue }
    var label: String { self == .emoji ? "🐘 Elephant" : "Slonic mascot" }
}
