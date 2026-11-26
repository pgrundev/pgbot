import Foundation
import Security

/// Secrets storage. Production uses the macOS Keychain; checks use the in-memory store.
public protocol SecretStore: Sendable {
    func set(_ secret: String, for account: String) throws
    func get(_ account: String) throws -> String?
    func delete(_ account: String) throws
}

public enum KeychainError: LocalizedError, Equatable {
    case unexpected(OSStatus)
    public var errorDescription: String? {
        switch self {
        case .unexpected(let s):
            let msg = SecCopyErrorMessageString(s, nil) as String? ?? "status \(s)"
            return "Keychain error: \(msg)"
        }
    }
}

/// Generic-password items under one service. Items are device-only
/// (`…ThisDeviceOnly`) so database credentials never sync to iCloud Keychain,
/// and are readable only after first unlock — background monitoring needs them
/// while the screen is locked, but never before the user has logged in.
public struct KeychainStore: SecretStore {
    public let service: String
    public init(service: String = "dev.pgbot.desktop") { self.service = service }

    private func base(_ account: String) -> [String: Any] {
        [kSecClass as String: kSecClassGenericPassword,
         kSecAttrService as String: service,
         kSecAttrAccount as String: account]
    }

    public func set(_ secret: String, for account: String) throws {
        let data = Data(secret.utf8)
        let update: [String: Any] = [kSecValueData as String: data,
                                     kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly]
        var status = SecItemUpdate(base(account) as CFDictionary, update as CFDictionary)
        if status == errSecItemNotFound {
            var add = base(account)
            add.merge(update) { $1 }
            add[kSecAttrLabel as String] = "PGBot — \(account.split(separator: ":").first ?? "secret")"
            status = SecItemAdd(add as CFDictionary, nil)
        }
        guard status == errSecSuccess else { throw KeychainError.unexpected(status) }
    }

    public func get(_ account: String) throws -> String? {
        var q = base(account)
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        let status = SecItemCopyMatching(q as CFDictionary, &out)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess, let data = out as? Data else { throw KeychainError.unexpected(status) }
        return String(data: data, encoding: .utf8)
    }

    public func delete(_ account: String) throws {
        let status = SecItemDelete(base(account) as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else { throw KeychainError.unexpected(status) }
    }
}

public final class InMemorySecretStore: SecretStore, @unchecked Sendable {
    private var items: [String: String] = [:]
    private let lock = NSLock()
    public init() {}
    public func set(_ secret: String, for account: String) throws { lock.withLock { items[account] = secret } }
    public func get(_ account: String) throws -> String? { lock.withLock { items[account] } }
    public func delete(_ account: String) throws { _ = lock.withLock { items.removeValue(forKey: account) } }
    public var count: Int { lock.withLock { items.count } }
}

/// Well-known Keychain account names, so every caller agrees.
public enum SecretAccount {
    public static func database(_ id: UUID) -> String { "db:\(id.uuidString)" }
    public static func aiKey(_ provider: AIProviderKind) -> String { "ai:\(provider.rawValue)" }
    public static let authSession = "auth:session"
}
