import Foundation

/// AI providers the engine's `internal/ai` already speaks. The app only chooses
/// and configures one; every model call is made by the engine (`pgbot ask`,
/// `pgbot sql explain --ai`), grounded on deterministic findings.
public enum AIProviderKind: String, Codable, Sendable, CaseIterable, Identifiable {
    case anthropic, openai, gemini, xai, openaiCompatible, ollama
    public var id: String { rawValue }

    public var label: String {
        switch self {
        case .anthropic: "Anthropic Claude"
        case .openai: "OpenAI"
        case .gemini: "Google Gemini"
        case .xai: "xAI Grok"
        case .openaiCompatible: "OpenAI-compatible endpoint"
        case .ollama: "Ollama (local)"
        }
    }

    /// PGBOT_AI_PROVIDER value (internal/ai/resolve.go).
    public var engineName: String {
        switch self {
        case .anthropic: "anthropic"
        case .openai: "openai"
        case .gemini: "gemini"
        case .xai: "xai"
        case .openaiCompatible: "openai-compatible"
        case .ollama: "ollama"
        }
    }

    public var needsKey: Bool { self != .ollama }
    public var needsBaseURL: Bool { self == .openaiCompatible }
    public var defaultBaseURL: String? { self == .ollama ? "http://localhost:11434/v1" : nil }
    public var suggestedModels: [String] {
        switch self {
        case .anthropic: ["claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"]
        case .openai: ["gpt-5", "gpt-5-mini"]
        case .gemini: ["gemini-2.5-pro", "gemini-2.5-flash"]
        case .xai: ["grok-4"]
        case .openaiCompatible: []
        case .ollama: ["llama3.1", "qwen2.5"]
        }
    }
}

public struct AIConfiguration: Codable, Sendable, Equatable {
    public var provider: AIProviderKind
    public var model: String
    public var baseURL: String
    public init(provider: AIProviderKind = .anthropic, model: String = "", baseURL: String = "") {
        self.provider = provider; self.model = model; self.baseURL = baseURL
    }

    public var effectiveBaseURL: String? {
        let b = baseURL.trimmingCharacters(in: .whitespaces)
        return b.isEmpty ? provider.defaultBaseURL : b
    }

    /// True when the request never leaves this Mac — matches ai.Local's notion
    /// closely enough to choose consent wording (the engine re-checks).
    public var isLocal: Bool {
        guard let b = effectiveBaseURL, let host = URL(string: b)?.host?.lowercased() else { return false }
        return host == "localhost" || host == "127.0.0.1" || host == "::1" || host.hasSuffix(".local")
    }

    /// Environment for the engine. The key is passed only here, in-process, from the Keychain.
    public func environment(apiKey: String?) -> [String: String] {
        var env = ["PGBOT_AI_PROVIDER": provider.engineName]
        if let k = apiKey, !k.isEmpty { env["PGBOT_AI_API_KEY"] = k }
        if !model.trimmingCharacters(in: .whitespaces).isEmpty { env["PGBOT_AI_MODEL"] = model }
        if let b = effectiveBaseURL { env["PGBOT_AI_BASE_URL"] = b }
        return env
    }
}
