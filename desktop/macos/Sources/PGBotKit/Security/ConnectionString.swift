import Foundation

/// Parses a pasted `postgres://` URL into non-secret settings plus the secret,
/// and rebuilds a DSN for the engine. The DSN only ever travels to the engine
/// through its environment (`PGBOT_DATABASE_URL`), never argv, a file, or a log.
public enum ConnectionString {
    public struct Parsed: Equatable, Sendable {
        public var connection: DatabaseConnection
        public var password: String?
    }

    public enum ParseError: LocalizedError, Equatable {
        case notAURL, unsupportedScheme(String), missingHost
        public var errorDescription: String? {
            switch self {
            case .notAURL: "That doesn't look like a connection URL. It should start with postgresql://"
            case .unsupportedScheme(let s): "“\(s)://” isn't a PostgreSQL URL. Use postgresql:// or postgres://"
            case .missingHost: "The URL has no host name."
            }
        }
    }

    public static func parse(_ raw: String) throws -> Parsed {
        let s = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let comps = URLComponents(string: s), let scheme = comps.scheme?.lowercased() else { throw ParseError.notAURL }
        guard scheme == "postgres" || scheme == "postgresql" else { throw ParseError.unsupportedScheme(scheme) }
        guard let host = comps.host, !host.isEmpty else { throw ParseError.missingHost }
        // URLComponents' plain accessors (host, user, password, path) are already
        // percent-decoded — decoding again would corrupt a password containing "%".
        let path = String(comps.path.dropFirst())
        var conn = DatabaseConnection(
            host: host,
            port: comps.port ?? 5432,
            database: path.isEmpty ? "postgres" : path,
            username: comps.user.flatMap { $0.isEmpty ? nil : $0 } ?? "postgres")
        for item in comps.queryItems ?? [] {
            guard let v = item.value else { continue }
            switch item.name {
            case "sslmode": conn.sslMode = SSLMode(rawValue: v) ?? conn.sslMode
            case "connect_timeout": conn.connectTimeoutSeconds = Int(v) ?? conn.connectTimeoutSeconds
            case "password": continue // a secret in the query string goes to the Keychain, not settings
            default: conn.extraParameters[item.name] = v
            }
        }
        var password = comps.password
        if password == nil, let q = comps.queryItems?.first(where: { $0.name == "password" })?.value { password = q }
        return Parsed(connection: conn, password: password)
    }

    /// Builds the DSN handed to the engine. Callers must treat the result as a secret.
    public static func build(_ c: DatabaseConnection, password: String?) -> String {
        var comps = URLComponents()
        comps.scheme = "postgresql"
        comps.host = c.host
        comps.port = c.port
        comps.percentEncodedUser = encode(c.username)
        if let p = password, !p.isEmpty { comps.percentEncodedPassword = encode(p) }
        comps.percentEncodedPath = "/" + encode(c.database)
        var items = [URLQueryItem(name: "sslmode", value: c.sslMode.rawValue),
                     URLQueryItem(name: "connect_timeout", value: String(c.connectTimeoutSeconds))]
        items += c.extraParameters.sorted(by: { $0.key < $1.key }).map { URLQueryItem(name: $0.key, value: $0.value) }
        comps.queryItems = items
        return comps.string ?? ""
    }

    /// RFC 3986 unreserved characters only — everything else is escaped so
    /// `@`, `:`, `/`, `?`, `#` in passwords can't break the URL.
    private static func encode(_ s: String) -> String {
        var allowed = CharacterSet.alphanumerics
        allowed.insert(charactersIn: "-._~")
        return s.addingPercentEncoding(withAllowedCharacters: allowed) ?? s
    }
}
