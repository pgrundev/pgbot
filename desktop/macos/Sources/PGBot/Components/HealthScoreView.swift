import PGBotKit
import SwiftUI

/// The reusable health score: a large number, its band color, the change since
/// the previous inspection, and finding counts. Shows "Incomplete" or
/// "Unavailable" instead of a reassuring number when the engine couldn't see enough.
@MainActor struct HealthScoreView: View {
    let score: HealthScore
    var previous: Int?
    var resolved: Int = 0
    var inspectedAt: Date?
    var bands = HealthBands()
    var compact = false

    var body: some View {
        let band = bands.band(for: score)
        HStack(alignment: .center, spacing: Space.l) {
            VStack(alignment: .leading, spacing: 2) {
                if score.completeness == .unavailable {
                    Text("—").font(.sys(size: compact ? 28 : 40, weight: .semibold, design: .rounded))
                    Text("Health score unavailable").font(.callout).foregroundStyle(.secondary)
                } else {
                    HStack(alignment: .firstTextBaseline, spacing: 2) {
                        Text("\(score.value)")
                            .font(.sys(size: compact ? 30 : 44, weight: .semibold, design: .rounded))
                            .foregroundStyle(band.color)
                            .contentTransition(.numericText())
                        Text("/100").font(.sys(size: compact ? 14 : 17, weight: .medium, design: .rounded)).foregroundStyle(.secondary)
                        if let d = delta, d != 0 {
                            Label("\(d > 0 ? "+" : "")\(d)", systemImage: d > 0 ? "arrow.up.right" : "arrow.down.right")
                                .font(.caption.weight(.semibold))
                                .foregroundStyle(d > 0 ? .green : .orange)
                                .padding(.leading, 6)
                                .help("Change since the previous inspection")
                        }
                    }
                    HStack(spacing: 6) {
                        Text("Health score").font(.callout).foregroundStyle(.secondary)
                        if score.completeness == .partial {
                            Text("Incomplete").font(.caption2.weight(.semibold))
                                .padding(.horizontal, 6).padding(.vertical, 1)
                                .background(Color.orange.opacity(0.15), in: Capsule()).foregroundStyle(.orange)
                                .help(score.gaps.joined(separator: "\n"))
                        }
                    }
                }
            }
            if !compact {
                Divider().frame(height: 44)
                HStack(spacing: Space.l) {
                    counter(score.critical, "Critical", .red)
                    counter(score.warnings, "Warnings", .orange)
                    counter(resolved, "Resolved", .green)
                }
                Spacer(minLength: 0)
                if let at = inspectedAt {
                    VStack(alignment: .trailing, spacing: 2) {
                        Text("Last inspection").font(.caption).foregroundStyle(.secondary)
                        Text(at, style: .relative).font(.caption.monospacedDigit()) + Text(" ago").font(.caption)
                    }
                    .foregroundStyle(.secondary)
                }
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel(score.completeness == .unavailable ? "Health score unavailable"
                            : "Health score \(score.value) out of 100, \(score.critical) critical, \(score.warnings) warnings")
    }

    private var delta: Int? { previous.map { score.value - $0 } }

    private func counter(_ n: Int, _ label: String, _ color: Color) -> some View {
        VStack(alignment: .leading, spacing: 1) {
            Text("\(n)").font(.title3.weight(.semibold).monospacedDigit()).foregroundStyle(n > 0 ? color : .primary)
            Text(label).font(.caption).foregroundStyle(.secondary)
        }
    }
}
