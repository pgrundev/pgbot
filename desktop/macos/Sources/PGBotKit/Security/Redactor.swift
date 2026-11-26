import Foundation

/// Last-line redaction for anything the app displays, persists, or sends to an
/// AI provider that did not already go through the engine's own scrubbing.
/// The engine scrubs SQL literals in its JSON (`--json` is the scrubbed
/// contract); this covers engine *stderr* (which can echo a DSN), pasted text,
/// and log lines the user hands to "Explain with PGBot".
public enum Redactor {
    private static let rules: [(NSRegularExpression, String)] = [
        // user:password@ inside any URL-ish DSN
        (re(#"(?i)\b(postgres(?:ql)?|mysql|redis|https?)://([^:/@\s]+):([^@\s]+)@"#), "$1://$2:•••@"),
        // key=value secrets in libpq keyword DSNs and env dumps
        (re(#"(?i)\b(password|passwd|pwd|sslpassword|api[_-]?key|secret|token)\s*=\s*('[^']*'|"[^"]*"|\S+)"#), "$1=•••"),
        // Authorization headers / bearer tokens
        (re(#"(?i)\b(bearer)\s+[A-Za-z0-9\-._~+/]{12,}=*"#), "$1 •••"),
        // Common provider key shapes (sk-…, xai-…, AIza…, ghp_…)
        (re(#"\b(sk-(?:ant-)?[A-Za-z0-9\-_]{16,}|xai-[A-Za-z0-9]{16,}|AIza[0-9A-Za-z\-_]{30,}|gh[pousr]_[A-Za-z0-9]{30,})\b"#), "•••"),
        // E-mail addresses
        (re(#"[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}"#), "<email>"),
    ]

    /// SQL string literals and numeric literals → placeholders, as the engine does.
    private static let sqlLiteral = re(#"'(?:[^']|'')*'"#)
    private static let sqlNumber = re(#"(?<![A-Za-z_$0-9.])-?\d+(?:\.\d+)?(?![A-Za-z_0-9])"#)

    public static func redact(_ s: String) -> String {
        rules.reduce(s) { acc, rule in
            rule.0.stringByReplacingMatches(in: acc, range: NSRange(acc.startIndex..., in: acc), withTemplate: rule.1)
        }
    }

    /// Redacts secrets and also SQL literals — for log lines and SQL before AI.
    public static func redactSQLLiterals(_ s: String) -> String {
        var out = redact(s)
        out = sqlLiteral.stringByReplacingMatches(in: out, range: NSRange(out.startIndex..., in: out), withTemplate: "'?'")
        out = sqlNumber.stringByReplacingMatches(in: out, range: NSRange(out.startIndex..., in: out), withTemplate: "?")
        return out
    }

    /// Removes one specific secret wherever it appears (e.g. the bot's own
    /// password echoed back by a driver error).
    public static func scrub(_ s: String, secrets: [String]) -> String {
        secrets.filter { $0.count >= 3 }.reduce(redact(s)) { $0.replacingOccurrences(of: $1, with: "•••") }
    }

    private static func re(_ p: String) -> NSRegularExpression { try! NSRegularExpression(pattern: p) }
}
