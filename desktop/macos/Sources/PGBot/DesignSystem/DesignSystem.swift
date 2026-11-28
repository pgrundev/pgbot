import AppKit
import PGBotKit
import SwiftUI

// MARK: - Color tokens
//
// A quiet, two-surface palette: a white canvas for the conversation and a soft
// gray sidebar. Color is reserved for the bots themselves and for severity.

extension Color {
    /// A color that resolves per appearance.
    init(light: NSColor, dark: NSColor) {
        self.init(nsColor: NSColor(name: nil) { $0.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua ? dark : light })
    }
}

/// sRGB hex → NSColor.
func hex(_ v: UInt32, _ alpha: CGFloat = 1) -> NSColor {
    NSColor(srgbRed: CGFloat((v >> 16) & 0xff) / 255, green: CGFloat((v >> 8) & 0xff) / 255, blue: CGFloat(v & 0xff) / 255, alpha: alpha)
}

/// Dark mode follows Grok: near-black, neutral (no blue tint), low-contrast
/// gray surfaces, light text on dark-gray user bubbles, white primary buttons.
enum Palette {
    static let canvas = Color(light: .white, dark: hex(0x151515))
    static let sidebar = Color(light: hex(0xf6f6f5), dark: hex(0x0d0d0d))
    /// Bot message bubbles.
    static let bubble = Color(light: hex(0xf1f1f0), dark: hex(0x1f1f1f))
    /// The user's own messages.
    static let userBubble = Color(light: hex(0xe9e9e7), dark: hex(0x2c2c2c))
    static let userText = Color(light: hex(0x141414), dark: hex(0xececec))
    /// Pills, the input, round buttons.
    static let floating = Color(light: hex(0xf3f3f2), dark: hex(0x1c1c1c))
    static let floatingHover = Color(light: hex(0xe9e9e7), dark: hex(0x262626))
    /// Round controls inside the input ("+").
    static let control = Color(light: hex(0xe6e6e4), dark: hex(0x262626))
    /// Primary buttons and the send button.
    static let primaryFill = Color(light: hex(0x141414), dark: hex(0xf5f5f5))
    static let onPrimary = Color(light: .white, dark: hex(0x0d0d0d))
    static let selection = Color(light: hex(0xebebea), dark: hex(0x1f1f1f))
    static let hover = Color(light: hex(0xf0f0ef), dark: hex(0x171717))
    static let hairline = Color(light: NSColor(white: 0, alpha: 0.08), dark: NSColor(white: 1, alpha: 0.07))
    static let strongHairline = Color(light: NSColor(white: 0, alpha: 0.14), dark: NSColor(white: 1, alpha: 0.12))
    /// Lighter secondary text.
    static let muted = Color(light: hex(0x5f5f5f), dark: hex(0xa8a8a8))
    static let link = Color(light: hex(0x2f6fdb), dark: hex(0x6aa5ff))
    static let healthy = Color(nsColor: hex(0x22c55e))
    static let card = Color.primary.opacity(0.04)
    static let subtleFill = Color.primary.opacity(0.06)
    /// Cards and code inside a message: set off from the bubble (or from the canvas when messages are plain).
    static var inset: Color {
        Theme.plainMessages ? bubble : Color(light: .white, dark: hex(0x171717))
    }
}

extension BotColor {
    var color: Color {
        switch self {
        case .blue: Color(nsColor: hex(0x3b6cf5))
        case .green: Color(nsColor: hex(0x22b45a))
        case .orange: Color(nsColor: hex(0xf26a1b))
        case .purple: Color(red: 0.56, green: 0.40, blue: 0.95)
        case .pink: Color(red: 0.93, green: 0.33, blue: 0.62)
        case .yellow: Color(red: 0.95, green: 0.72, blue: 0.10)
        case .red: Color(red: 0.92, green: 0.30, blue: 0.30)
        case .teal: Color(red: 0.30, green: 0.72, blue: 0.62)
        case .gray: Color(red: 0.55, green: 0.57, blue: 0.61)
        // The spec's neutral avatar; it carries a database icon instead of the elephant.
        case .black: Color(light: hex(0x2a2a2a), dark: hex(0x242424))
        }
    }
}

extension Severity {
    var color: Color {
        switch self { case .critical: .red; case .warn: .orange; case .info: .secondary }
    }
    var symbol: String {
        switch self {
        case .critical: "exclamationmark.octagon.fill"
        case .warn: "exclamationmark.triangle.fill"
        case .info: "info.circle.fill"
        }
    }
}

extension ConnectionStatus {
    var color: Color {
        switch self {
        case .healthy: Palette.healthy
        case .degraded: .orange
        case .critical, .unreachable: .red
        case .unknown, .paused, .connecting: .gray
        }
    }
    var label: String {
        switch self {
        case .unknown: "Not inspected yet"
        case .connecting: "Inspecting…"
        case .healthy: "Healthy"
        case .degraded: "Needs attention"
        case .critical: "Critical"
        case .unreachable: "Unreachable"
        case .paused: "Monitoring paused"
        }
    }
}

extension HealthBands.Band {
    var color: Color {
        switch self { case .green: Palette.healthy; case .yellow: .yellow; case .orange: .orange; case .red: .red; case .gray: .secondary }
    }
}

// MARK: - Typography
//
// macOS's default body (13pt) is too small for a reading-first chat; messages
// use 15pt, like modern assistant apps.

enum Typo {
    static var body: Font { .sys(size: 15) }
    static var bodyEmphasis: Font { .sys(size: 15, weight: .semibold) }
    static var title: Font { .sys(size: 17, weight: .semibold) }
    static var small: Font { .sys(size: 13) }
    static var smallEmphasis: Font { .sys(size: 13, weight: .medium) }
    static var micro: Font { .sys(size: 11.5) }
}

/// User-chosen accent and text size (Settings → Appearance). Changing either
/// rebuilds the main window (RootView is keyed on them), so plain globals suffice.
enum Theme {
    nonisolated(unsafe) static var textScale: CGFloat = 1
    nonisolated(unsafe) static var accent: Color = .accentColor
    nonisolated(unsafe) static var plainMessages = false
    nonisolated(unsafe) static var avatarStyle: AvatarStyle = .emoji
    nonisolated(unsafe) static var animateAvatars = true
    static func apply(_ s: AppSettings) {
        animateAvatars = s.animateAvatars
        avatarStyle = s.avatarStyle
        textScale = s.textSize.scale
        accent = s.accent.color
        plainMessages = s.bubbleStyle == .plain
    }
}

extension Font {
    /// A fixed point size that follows the app's text-size setting.
    static func sys(size: CGFloat, weight: Font.Weight = .regular, design: Font.Design = .default) -> Font {
        .system(size: size * Theme.textScale, weight: weight, design: design)
    }
}

// MARK: - Spacing & shape

enum Space {
    static let xs: CGFloat = 4, s: CGFloat = 8, m: CGFloat = 12, l: CGFloat = 16, xl: CGFloat = 24, xxl: CGFloat = 36
}

enum Radius {
    static let card: CGFloat = 16, bubble: CGFloat = 22, composer: CGFloat = 26, chip: CGFloat = 8
}

/// The conversation column's readable width.
let readableWidth: CGFloat = 800

// MARK: - Modifiers

struct CardStyle: ViewModifier {
    var padding: CGFloat = Space.l
    func body(content: Content) -> some View {
        content.padding(padding)
            .background(Palette.card, in: RoundedRectangle(cornerRadius: Radius.card, style: .continuous))
    }
}

/// A white card that sits inside a gray message bubble.
struct InsetCardStyle: ViewModifier {
    var padding: CGFloat = Space.m
    func body(content: Content) -> some View {
        content.padding(padding)
            .background(Palette.inset, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
    }
}

/// Floating capsule chrome (title pill, header actions, status notes).
struct FloatingPill: ViewModifier {
    var height: CGFloat = 34
    func body(content: Content) -> some View {
        content.padding(.horizontal, 14).frame(height: height)
            .background(Palette.floating, in: Capsule())
            .overlay(Capsule().strokeBorder(Palette.hairline))
    }
}

/// Outline capsule (spec "Metrics" button): transparent, hairline border.
struct OutlinePill: ViewModifier {
    var active = false
    @State private var hovering = false
    func body(content: Content) -> some View {
        content.padding(.horizontal, 14).frame(height: 36)
            .background(active || hovering ? Palette.floatingHover : .clear, in: Capsule())
            .overlay(Capsule().strokeBorder(Palette.strongHairline))
            .contentShape(Capsule())
            .onHover { hovering = $0 }
    }
}

extension View {
    func card(padding: CGFloat = Space.l) -> some View { modifier(CardStyle(padding: padding)) }
    func insetCard(padding: CGFloat = Space.m) -> some View { modifier(InsetCardStyle(padding: padding)) }
    func floatingPill(height: CGFloat = 34) -> some View { modifier(FloatingPill(height: height)) }
    func outlinePill(active: Bool = false) -> some View { modifier(OutlinePill(active: active)) }
    func hoverHighlight(cornerRadius: CGFloat = 8) -> some View { modifier(HoverHighlight(cornerRadius: cornerRadius)) }
}

struct HoverHighlight: ViewModifier {
    var cornerRadius: CGFloat = 8
    @State private var hovering = false
    func body(content: Content) -> some View {
        content
            .background(RoundedRectangle(cornerRadius: cornerRadius, style: .continuous).fill(hovering ? Palette.subtleFill : .clear))
            .onHover { hovering = $0 }
    }
}

/// Message buttons (spec): 36pt capsules. Primary is #f0f0ec with dark text;
/// secondary is an outline that fills on hover.
struct PillButtonStyle: ButtonStyle {
    var prominent = false
    var compact = false
    func makeBody(configuration: Configuration) -> some View {
        PillBody(configuration: configuration, prominent: prominent, compact: compact)
    }

    private struct PillBody: View {
        let configuration: ButtonStyleConfiguration
        let prominent: Bool
        let compact: Bool
        @State private var hovering = false
        @Environment(\.isEnabled) private var enabled
        var body: some View {
            configuration.label
                .font(.sys(size: compact ? 13 : 14, weight: .medium))
                .padding(.horizontal, compact ? 12 : 16)
                .frame(height: compact ? 30 : 36)
                .foregroundStyle(prominent ? Palette.onPrimary : Color.primary)
                .background(prominent ? Palette.primaryFill : (hovering ? Palette.floatingHover : Color.clear), in: Capsule())
                .overlay(Capsule().strokeBorder(prominent ? Color.clear : Palette.strongHairline))
                .opacity(configuration.isPressed ? 0.75 : (enabled ? 1 : 0.45))
                .contentShape(Capsule())
                .onHover { hovering = $0 }
        }
    }
}

extension ButtonStyle where Self == PillButtonStyle {
    static var pill: PillButtonStyle { PillButtonStyle() }
    static var pillProminent: PillButtonStyle { PillButtonStyle(prominent: true) }
    static var pillSmall: PillButtonStyle { PillButtonStyle(compact: true) }
}

// MARK: - Controls

/// A white round icon button, as in the sidebar's top corner.
@MainActor struct CircleButton: View {
    let symbol: String
    let help: String
    var size: CGFloat = 34
    var badge = false
    let action: () -> Void
    var body: some View {
        Button(action: action) { CircleIcon(symbol: symbol, size: size, badge: badge) }
            .buttonStyle(.plain)
            .help(help)
            .accessibilityLabel(help)
    }
}

@MainActor struct CircleIcon: View {
    let symbol: String
    var size: CGFloat = 34
    var badge = false
    var outlined = false
    @State private var hovering = false
    var body: some View {
        Image(systemName: symbol)
            .font(.system(size: size * 0.41, weight: .medium))
            .foregroundStyle(.primary.opacity(0.85))
            .frame(width: size, height: size)
            .background(outlined ? (hovering ? Palette.floatingHover : .clear) : (hovering ? Palette.floatingHover : Palette.floating), in: Circle())
            .overlay(Circle().strokeBorder(outlined ? Palette.strongHairline : .clear))
            .overlay(alignment: .topTrailing) {
                if badge {
                    Circle().fill(Theme.accent).frame(width: 9, height: 9)
                        .overlay(Circle().strokeBorder(Palette.canvas, lineWidth: 2))
                        .offset(x: 1, y: -1)
                }
            }
            .contentShape(Circle())
            .onHover { hovering = $0 }
    }
}

/// A borderless icon button (used in dense places like tables and toolbars).
@MainActor struct IconButton: View {
    let symbol: String
    let help: String
    var size: CGFloat = 15
    var badge: Int = 0
    let action: () -> Void
    var body: some View {
        Button(action: action) {
            Image(systemName: symbol).font(.sys(size: size)).frame(width: 30, height: 28).contentShape(Rectangle())
                .overlay(alignment: .topTrailing) {
                    if badge > 0 { Circle().fill(Theme.accent).frame(width: 7, height: 7).offset(x: -5, y: 5) }
                }
        }
        .buttonStyle(.plain).foregroundStyle(.secondary).hoverHighlight().help(help).accessibilityLabel(help)
    }
}

@MainActor struct DemoBadge: View {
    var body: some View {
        Text("DEMO")
            .font(.sys(size: 9, weight: .bold, design: .rounded))
            .padding(.horizontal, 5).padding(.vertical, 1.5)
            .background(Color.purple.opacity(0.14), in: Capsule())
            .foregroundStyle(.purple)
            .accessibilityLabel("Demo data")
    }
}

@MainActor struct StatusDot: View {
    let status: ConnectionStatus
    var size: CGFloat = 8
    var body: some View {
        Circle().fill(status.color).frame(width: size, height: size)
            .overlay(Circle().strokeBorder(Palette.sidebar, lineWidth: size > 8 ? 2 : 0))
            .accessibilityLabel(status.label)
    }
}

/// Settings is a regular window scene ("settings"): on macOS 14 the Settings
/// scene can only be opened through SettingsLink, not from code, menus or the
/// menu bar. RootView hands us its OpenWindowAction at launch.
@MainActor enum WindowOpener { static var open: OpenWindowAction? }

@MainActor func openAppSettings(tab: String? = nil) {
    if let tab { UserDefaults.standard.set(tab, forKey: "settingsTab") }
    NSApp.activate(ignoringOtherApps: true)
    WindowOpener.open?(id: "settings")
    // When PGBot isn't frontmost, SwiftUI can create the window without ordering
    // it in; bring it forward explicitly.
    for delay in [0.0, 0.15, 0.4, 0.9] {
        DispatchQueue.main.asyncAfter(deadline: .now() + delay) {
            guard let w = NSApp.windows.first(where: { $0.title == "PGBot Settings" }) else {
                WindowOpener.open?(id: "settings") // not created yet (or dropped): ask again
                return
            }
            guard !w.isVisible || !w.isKeyWindow else { return }
            NSApp.activate(ignoringOtherApps: true)
            w.makeKeyAndOrderFront(nil)
        }
    }
}

/// "Today 2:04 PM", "Yesterday 9:00 AM", "Mon, Nov 23 2:04 AM".
func conversationStamp(_ d: Date) -> String {
    let cal = Calendar.current, time = d.formatted(date: .omitted, time: .shortened)
    if cal.isDateInToday(d) { return "Today \(time)" }
    if cal.isDateInYesterday(d) { return "Yesterday \(time)" }
    return d.formatted(.dateTime.weekday(.abbreviated).month(.abbreviated).day()) + " " + time
}
