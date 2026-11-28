import CryptoKit
import Foundation

// OAuth 2.0 authorization-code flow with PKCE (RFC 7636) for a public native
// client. The app holds NO client secret: Google and GitHub are reached through
// a small PGBot auth broker (`PGBotAuthBrokerURL` in Info.plist) that keeps the
// provider secrets server-side, verifies PKCE, and returns PGBot session tokens.
// The browser leg runs in the system browser (ASWebAuthenticationSession, in
// the app target); everything else is here and testable.

public struct PKCE: Sendable, Equatable {
    public let verifier: String
    public let challenge: String
    public static let method = "S256"

    public init(verifier: String) {
        self.verifier = verifier
        self.challenge = Self.base64url(Data(SHA256.hash(data: Data(verifier.utf8))))
    }

    /// 32 random bytes → a 43-char verifier (RFC 7636 §4.1 allows 43–128).
    public static func generate() -> PKCE { PKCE(verifier: randomToken(bytes: 32)) }

    public static func randomToken(bytes: Int) -> String {
        var g = SystemRandomNumberGenerator()
        return base64url(Data((0..<bytes).map { _ in UInt8.random(in: 0...255, using: &g) }))
    }

    static func base64url(_ d: Data) -> String {
        d.base64EncodedString().replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: "=", with: "")
    }
}

public struct AuthConfiguration: Sendable, Equatable {
    public var brokerURL: URL?
    public var clientID: String
    public var callbackScheme: String
    public init(brokerURL: URL?, clientID: String = "pgbot-desktop", callbackScheme: String = "pgbot") {
        self.brokerURL = brokerURL; self.clientID = clientID; self.callbackScheme = callbackScheme
    }
    public var isConfigured: Bool { brokerURL != nil }
    public var redirectURI: String { "\(callbackScheme)://auth/callback" }

    public static func fromBundle(_ b: Bundle = .main) -> AuthConfiguration {
        let raw = (b.object(forInfoDictionaryKey: "PGBotAuthBrokerURL") as? String)
            ?? ProcessInfo.processInfo.environment["PGBOT_AUTH_BROKER_URL"]
        let url = raw.flatMap { $0.isEmpty ? nil : URL(string: $0) }
        // Refuse a non-TLS broker except on loopback (local development).
        if let u = url, u.scheme != "https", !(u.host == "localhost" || u.host == "127.0.0.1") {
            return AuthConfiguration(brokerURL: nil)
        }
        return AuthConfiguration(brokerURL: url)
    }
}

public struct AuthenticationSession: Codable, Sendable, Equatable {
    public var user: User
    public var accessToken: String
    public var refreshToken: String?
    public var expiresAt: Date
    public var subscription: Subscription
    public var isExpired: Bool { Date() >= expiresAt.addingTimeInterval(-60) }
    public init(user: User, accessToken: String, refreshToken: String?, expiresAt: Date, subscription: Subscription) {
        self.user = user; self.accessToken = accessToken; self.refreshToken = refreshToken
        self.expiresAt = expiresAt; self.subscription = subscription
    }
}

public enum AuthError: LocalizedError, Equatable {
    case notConfigured, cancelled, stateMismatch, missingCode, providerError(String), network(String), expired
    public var errorDescription: String? {
        switch self {
        case .notConfigured: "Sign-in isn't configured in this build."
        case .cancelled: "Sign-in was cancelled."
        case .stateMismatch: "The sign-in response didn't match this request, so it was rejected for your safety. Please try again."
        case .missingCode: "The sign-in response was incomplete. Please try again."
        case .providerError(let s): "The provider declined sign-in: \(s)"
        case .network(let s): "Couldn't reach the sign-in service: \(s)"
        case .expired: "Your session expired. Please sign in again."
        }
    }
}

/// One in-flight authorization request.
public struct AuthorizationRequest: Sendable {
    public let provider: AuthProvider
    public let pkce: PKCE
    public let state: String
    public let url: URL

    /// Validates the redirect and extracts the code. State is compared in
    /// constant time; any mismatch aborts (CSRF / mix-up defense).
    public func code(from callback: URL) throws -> String {
        let items = URLComponents(url: callback, resolvingAgainstBaseURL: false)?.queryItems ?? []
        func v(_ n: String) -> String? { items.first { $0.name == n }?.value }
        if let err = v("error") { throw AuthError.providerError(v("error_description") ?? err) }
        guard let got = v("state"), constantTimeEqual(got, state) else { throw AuthError.stateMismatch }
        guard let code = v("code"), !code.isEmpty else { throw AuthError.missingCode }
        return code
    }
}

func constantTimeEqual(_ a: String, _ b: String) -> Bool {
    let x = Array(a.utf8), y = Array(b.utf8)
    guard x.count == y.count else { return false }
    return zip(x, y).reduce(0) { $0 | ($1.0 ^ $1.1) } == 0
}

/// Token-endpoint transport, abstracted for checks.
public protocol AuthTransport: Sendable {
    func post(_ url: URL, form: [String: String]) async throws -> Data
}

public struct URLSessionAuthTransport: AuthTransport {
    public init() {}
    public func post(_ url: URL, form: [String: String]) async throws -> Data {
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/x-www-form-urlencoded", forHTTPHeaderField: "Content-Type")
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        var c = URLComponents(); c.queryItems = form.map { URLQueryItem(name: $0.key, value: $0.value) }
        req.httpBody = Data((c.percentEncodedQuery ?? "").utf8)
        let (data, resp) = try await URLSession.shared.data(for: req)
        guard let http = resp as? HTTPURLResponse else { throw AuthError.network("no response") }
        guard (200..<300).contains(http.statusCode) else {
            if http.statusCode == 400 || http.statusCode == 401 { throw AuthError.expired }
            throw AuthError.network("HTTP \(http.statusCode)")
        }
        return data
    }
}

public final class AuthService: @unchecked Sendable {
    public let config: AuthConfiguration
    private let secrets: SecretStore
    private let transport: AuthTransport

    public init(config: AuthConfiguration, secrets: SecretStore, transport: AuthTransport = URLSessionAuthTransport()) {
        self.config = config; self.secrets = secrets; self.transport = transport
    }

    public func makeRequest(for provider: AuthProvider) throws -> AuthorizationRequest {
        guard let broker = config.brokerURL else { throw AuthError.notConfigured }
        let pkce = PKCE.generate(), state = PKCE.randomToken(bytes: 24)
        var c = URLComponents(url: broker.appendingPathComponent("oauth/\(provider.rawValue)/authorize"), resolvingAgainstBaseURL: false)!
        c.queryItems = [
            .init(name: "response_type", value: "code"), .init(name: "client_id", value: config.clientID),
            .init(name: "redirect_uri", value: config.redirectURI), .init(name: "code_challenge", value: pkce.challenge),
            .init(name: "code_challenge_method", value: PKCE.method), .init(name: "state", value: state),
            .init(name: "scope", value: "openid email profile"),
        ]
        return AuthorizationRequest(provider: provider, pkce: pkce, state: state, url: c.url!)
    }

    struct TokenResponse: Decodable {
        let access_token: String
        let refresh_token: String?
        let expires_in: Double
        let user: UserDTO?
        let plan: String?
        struct UserDTO: Decodable { let id: String; let name: String; let email: String?; let avatar_url: String? }
    }

    public func complete(_ request: AuthorizationRequest, callback: URL) async throws -> AuthenticationSession {
        let code = try request.code(from: callback)
        let data = try await transport.post(tokenURL(), form: [
            "grant_type": "authorization_code", "code": code, "code_verifier": request.pkce.verifier,
            "redirect_uri": config.redirectURI, "client_id": config.clientID,
        ])
        let session = try makeSession(data, provider: request.provider, previous: nil)
        try persist(session)
        return session
    }

    /// Restores the saved session, refreshing it if it has expired.
    public func restore() async throws -> AuthenticationSession? {
        guard let raw = try secrets.get(SecretAccount.authSession),
              let s = try? JSONDecoder().decode(AuthenticationSession.self, from: Data(raw.utf8)) else { return nil }
        if s.user.provider == .local || !s.isExpired { return s }
        return try await refresh(s)
    }

    public func refresh(_ s: AuthenticationSession) async throws -> AuthenticationSession {
        guard let rt = s.refreshToken else { try signOut(); throw AuthError.expired }
        do {
            let data = try await transport.post(tokenURL(), form: ["grant_type": "refresh_token", "refresh_token": rt, "client_id": config.clientID])
            let ns = try makeSession(data, provider: s.user.provider, previous: s)
            try persist(ns)
            return ns
        } catch AuthError.expired {
            try signOut(); throw AuthError.expired
        }
    }

    /// A local-only profile for builds without a configured broker. Nothing is
    /// sent anywhere; Pro features stay unavailable.
    public func continueLocally(name: String) throws -> AuthenticationSession {
        let s = AuthenticationSession(user: User(id: "local", name: name, email: nil, avatarURL: nil, provider: .local),
                                      accessToken: "", refreshToken: nil, expiresAt: .distantFuture, subscription: .init())
        try persist(s)
        return s
    }

    public func signOut() throws { try secrets.delete(SecretAccount.authSession) }

    private func tokenURL() throws -> URL {
        guard let b = config.brokerURL else { throw AuthError.notConfigured }
        return b.appendingPathComponent("oauth/token")
    }

    private func makeSession(_ data: Data, provider: AuthProvider, previous: AuthenticationSession?) throws -> AuthenticationSession {
        let t = try JSONDecoder().decode(TokenResponse.self, from: data)
        let user = t.user.map { User(id: $0.id, name: $0.name, email: $0.email, avatarURL: $0.avatar_url.flatMap(URL.init), provider: provider) }
            ?? previous?.user ?? User(id: "unknown", name: "PGBot user", email: nil, avatarURL: nil, provider: provider)
        var sub = previous?.subscription ?? Subscription()
        if let p = t.plan, let plan = SubscriptionPlan(rawValue: p) { sub.plan = plan }
        return AuthenticationSession(user: user, accessToken: t.access_token, refreshToken: t.refresh_token ?? previous?.refreshToken,
                                     expiresAt: Date().addingTimeInterval(t.expires_in), subscription: sub)
    }

    private func persist(_ s: AuthenticationSession) throws {
        try secrets.set(String(decoding: try JSONEncoder().encode(s), as: UTF8.self), for: SecretAccount.authSession)
    }
}
