import PGBotKit
import SwiftUI

@MainActor struct OnboardingView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        VStack(spacing: Space.xl) {
            HStack(spacing: -10) {
                ElephantAvatar(color: .blue, size: 56).rotationEffect(.degrees(-8))
                ElephantAvatar(color: .green, size: 72).zIndex(1)
                ElephantAvatar(color: .orange, size: 56).rotationEffect(.degrees(8))
            }
            VStack(spacing: Space.s) {
                Text("Meet your first database bot.").font(.sys(size: 28, weight: .semibold))
                Text("Connect a PostgreSQL database and PGBot will analyze its health, identify potential issues, and help you understand what to improve.")
                    .font(.title3).foregroundStyle(.secondary).multilineTextAlignment(.center).frame(maxWidth: 520)
            }
            Button { model.showingAddDatabase = true } label: {
                Label("Add database", systemImage: "plus").font(.headline).padding(.horizontal, Space.l).frame(height: 36)
            }
            .buttonStyle(.borderedProminent).controlSize(.large)
            .keyboardShortcut(.defaultAction)

            Text("Works with local PostgreSQL, Supabase, Neon, AWS RDS & Aurora, Google Cloud SQL, Azure, Render, Railway, PGBox and self-hosted servers.")
                .font(.callout).foregroundStyle(.tertiary).multilineTextAlignment(.center).frame(maxWidth: 520)
            Button("Explore with sample data") { model.setDemoMode(true) }.buttonStyle(.link).font(.callout)
        }
        .padding(Space.xxl)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}
