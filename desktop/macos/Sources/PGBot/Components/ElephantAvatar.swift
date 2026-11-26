import PGBotKit
import SwiftUI

/// A bot's avatar. Default style (design spec): the bot's color as a circle or a
/// softly rounded square tilted −8°, with 🐘 on it; the neutral color carries a
/// database icon instead. The flat "Slonic" mascot is the alternative style
/// (Settings → Appearance → Avatars).
@MainActor struct ElephantAvatar: View {
    var color: BotColor
    var shape: AvatarShape = .roundedSquare
    var size: CGFloat = 36

    @State private var hovering = false
    @State private var wiggles = 0
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    private struct Motion { var angle: Double = 0; var lift: CGFloat = 0 }

    var body: some View {
        Group {
            if Theme.avatarStyle == .emoji { emojiAvatar } else { mascot }
        }
        .frame(width: size, height: size)
        // A little "hello" when the pointer arrives: hop, wiggle, settle.
        .keyframeAnimator(initialValue: Motion(), trigger: wiggles) { content, m in
            content.rotationEffect(.degrees(m.angle)).offset(y: m.lift)
        } keyframes: { _ in
            KeyframeTrack(\.angle) {
                SpringKeyframe(-9, duration: 0.12)
                SpringKeyframe(7, duration: 0.14)
                SpringKeyframe(-3, duration: 0.12)
                SpringKeyframe(0, duration: 0.22)
            }
            KeyframeTrack(\.lift) {
                SpringKeyframe(-max(2, size * 0.08), duration: 0.14)
                SpringKeyframe(0, duration: 0.4, spring: .bouncy)
            }
        }
        .scaleEffect(hovering && animates ? 1.06 : 1)
        .animation(.spring(response: 0.3, dampingFraction: 0.6), value: hovering)
        .onHover { inside in
            hovering = inside
            if inside && animates { wiggles += 1 }
        }
        .accessibilityHidden(true)
    }

    private var animates: Bool { Theme.animateAvatars && !reduceMotion && size >= 20 }

    private var emojiAvatar: some View {
        let fill = color.color
        let glyph = Group {
            if color == .black {
                Image(systemName: "cylinder.split.1x2").font(.system(size: size * 0.40, weight: .regular)).foregroundStyle(Color(nsColor: hex(0xd0d0d0)))
            } else {
                Text("🐘").font(.system(size: size * 0.46))
            }
        }
        return ZStack {
            if shape == .circle {
                Circle().fill(fill)
                    .overlay(Circle().strokeBorder(Color.white.opacity(color == .black ? 0.08 : 0)))
                glyph
            } else {
                ZStack {
                    RoundedRectangle(cornerRadius: size * 0.31, style: .continuous).fill(fill)
                        .overlay(RoundedRectangle(cornerRadius: size * 0.31, style: .continuous).strokeBorder(Color.white.opacity(color == .black ? 0.08 : 0)))
                    glyph
                }
                .rotationEffect(.degrees(-8))
                .scaleEffect(0.94) // keep the tilted corners inside the frame
            }
        }
    }

    private var mascot: some View {
        let c = color.color
        let s = size
        return ZStack {
            ear(c).frame(width: s * 0.34, height: s * 0.44).offset(x: -s * 0.30, y: -s * 0.04)
            ear(c).frame(width: s * 0.34, height: s * 0.44).offset(x: s * 0.30, y: -s * 0.04)
            head(c).frame(width: s * 0.74, height: s * 0.70).offset(y: s * 0.04)
            HStack(spacing: s * 0.085) { eye(s); eye(s) }
                .rotationEffect(.degrees(10))
                .offset(x: s * 0.06, y: -s * 0.01)
        }
        .rotationEffect(.degrees(-9))
    }

    @ViewBuilder private func head(_ c: Color) -> some View {
        if shape == .circle {
            Ellipse().fill(c)
        } else {
            RoundedRectangle(cornerRadius: size * 0.24, style: .continuous).fill(c)
        }
    }

    private func ear(_ c: Color) -> some View {
        ZStack {
            Ellipse().fill(c)
            Ellipse().fill(Color.black.opacity(0.14))
        }
    }

    private func eye(_ s: CGFloat) -> some View {
        Capsule().fill(Color.white).frame(width: s * 0.072, height: s * 0.16)
    }
}

/// The product mark.
@MainActor struct PGBotLogo: View {
    var size: CGFloat = 72
    var body: some View { ElephantAvatar(color: .blue, shape: .roundedSquare, size: size) }
}

@MainActor struct BotAvatarView: View {
    let bot: DatabaseBot
    var size: CGFloat = 32
    var showsStatus = false
    var body: some View {
        ElephantAvatar(color: bot.avatar.color, shape: bot.avatar.shape, size: size)
            .overlay(alignment: .bottomTrailing) {
                // Only problems earn a dot; healthy bots stay clean.
                if showsStatus && [.degraded, .critical, .unreachable].contains(bot.status) {
                    StatusDot(status: bot.status, size: max(9, size * 0.24)).offset(x: 1, y: 1)
                }
            }
    }
}
