import AuthenticationServices
import PGBotKit
import SwiftUI

/// Runs the browser leg of OAuth in the user's system browser session.
protocol WebAuthenticator {
    @MainActor func authenticate(url: URL, callbackScheme: String) async throws -> URL
}

final class SystemBrowserAuthenticator: NSObject, WebAuthenticator, ASWebAuthenticationPresentationContextProviding {
    private var session: ASWebAuthenticationSession?

    @MainActor func authenticate(url: URL, callbackScheme: String) async throws -> URL {
        try await withCheckedThrowingContinuation { cont in
            let s = ASWebAuthenticationSession(url: url, callbackURLScheme: callbackScheme) { callback, error in
                if let callback { cont.resume(returning: callback); return }
                if let e = error as? ASWebAuthenticationSessionError, e.code == .canceledLogin { cont.resume(throwing: AuthError.cancelled); return }
                cont.resume(throwing: AuthError.network(error?.localizedDescription ?? "unknown"))
            }
            s.presentationContextProvider = self
            // Share cookies with the user's browser so existing Google/GitHub sessions work.
            s.prefersEphemeralWebBrowserSession = false
            session = s
            if !s.start() { cont.resume(throwing: AuthError.network("Couldn't open the browser")) }
        }
    }

    func presentationAnchor(for session: ASWebAuthenticationSession) -> ASPresentationAnchor {
        MainActor.assumeIsolated { NSApp.keyWindow ?? NSApp.windows.first ?? ASPresentationAnchor() }
    }
}

@MainActor struct SignInView: View {
    @Environment(AppModel.self) private var model
    @State private var authenticator = SystemBrowserAuthenticator()

    var body: some View {
        VStack(spacing: 0) {
            Spacer()
            VStack(spacing: Space.l) {
                PGBotLogo(size: 84)
                Text("PGBot").font(.sys(size: 34, weight: .semibold, design: .rounded))
                Text("An AI agent for every Postgres database.")
                    .font(.title2.weight(.medium))
                    .multilineTextAlignment(.center)
                Text("Connect your databases. Understand what's happening. Know what to fix.")
                    .font(.title3).foregroundStyle(.secondary).multilineTextAlignment(.center)
            }
            .padding(.bottom, Space.xxl)

            VStack(spacing: Space.m) {
                providerButton(.google, title: "Continue with Google", symbol: "g.circle.fill")
                providerButton(.github, title: "Continue with GitHub", symbol: "chevron.left.forwardslash.chevron.right")
                if !model.authService.config.isConfigured {
                    VStack(spacing: Space.s) {
                        Text("Account sign-in isn't configured in this build.")
                            .font(.callout).foregroundStyle(.secondary)
                        Button("Continue without an account") { Task { await model.continueLocally() } }
                            .buttonStyle(.link)
                        Text("Everything runs locally. Your databases and history stay on this Mac.")
                            .font(.caption).foregroundStyle(.tertiary).multilineTextAlignment(.center)
                    }
                    .padding(.top, Space.s)
                }
            }
            .frame(width: 320)

            if case .signedOut(let error?) = model.auth {
                Label(error, systemImage: "exclamationmark.triangle.fill")
                    .font(.callout).foregroundStyle(.orange)
                    .padding(.top, Space.l).frame(maxWidth: 420)
            }
            Spacer()
            HStack(spacing: Space.l) {
                Link("Terms of Service", destination: URL(string: "https://pgbot.dev/terms")!)
                Text("·").foregroundStyle(.tertiary)
                Link("Privacy Policy", destination: URL(string: "https://pgbot.dev/privacy")!)
            }
            .font(.footnote).foregroundStyle(.secondary)
            .padding(.bottom, Space.xl)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(.background)
    }

    private func providerButton(_ p: AuthProvider, title: String, symbol: String) -> some View {
        let busy = model.auth == .signingIn(p)
        return Button {
            Task { await model.signIn(with: p, using: authenticator) }
        } label: {
            HStack(spacing: Space.s) {
                if busy { ProgressView().controlSize(.small) } else { Image(systemName: symbol) }
                Text(busy ? "Waiting for your browser…" : title).fontWeight(.medium)
            }
            .frame(maxWidth: .infinity).frame(height: 40)
        }
        .buttonStyle(.plain)
        .background(Palette.subtleFill, in: RoundedRectangle(cornerRadius: 10, style: .continuous))
        .overlay(RoundedRectangle(cornerRadius: 10, style: .continuous).strokeBorder(Palette.hairline))
        .disabled(!model.authService.config.isConfigured || model.auth == .signingIn(.google) || model.auth == .signingIn(.github))
        .opacity(model.authService.config.isConfigured ? 1 : 0.45)
    }
}

@MainActor struct RootView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.openWindow) private var openWindow
    var body: some View {
        Group {
            switch model.auth {
            case .restoring:
                VStack(spacing: Space.l) { PGBotLogo(size: 64); ProgressView().controlSize(.small) }
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            case .signedOut, .signingIn:
                SignInView()
            case .signedIn:
                MainView()
            }
        }
        .frame(minWidth: 860, minHeight: 560)
        .onAppear { WindowOpener.open = openWindow }
    }
}
