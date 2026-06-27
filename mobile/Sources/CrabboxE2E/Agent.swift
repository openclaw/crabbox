import CrabboxKit
import Foundation

#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

/// The fixed action vocabulary the tiny LLM is allowed to choose from. Keeping
/// it small and explicit is what makes a 0.5B model usable as a driver.
public let agentActionVocabulary = [
    "OPEN_SETTINGS", "CLOSE_SETTINGS", "ENTER_URL", "TAP_CONNECT", "TAP_RESET",
    "TAP_RELOAD", "TAP_HOME", "TAP_BACK", "TAP_RETRY", "NAVIGATE",
]

private let argActions: Set<String> = ["ENTER_URL", "NAVIGATE"]

/// One decision returned by the driver.
public struct AgentDecision: Equatable {
    public let action: String
    public let arg: String
}

/// Renders the current app state as compact text for the model to reason over.
public func renderState(_ state: AppState) -> String {
    var lines = [
        "screen: \(state.settingsVisible ? "SETTINGS_SHEET" : "BROWSER")",
        "status: \(selectStatusLabel(state).rawValue)",
        "coordinator(home): \(state.homeURL)",
        "currentURL: \(state.currentURL)",
        "canGoBack: \(state.canGoBack)",
    ]
    if state.settingsVisible {
        lines.append("draftURL: \(state.draftURL)")
        if !state.urlError.isEmpty { lines.append("urlError: \(state.urlError)") }
    }
    return lines.joined(separator: "\n")
}

private let systemPrompt = """
You are an end-to-end test driver for a mobile app. Given the current screen as \
text and a list of allowed actions, output exactly ONE next action as JSON \
{"action":<one allowed action>,"arg":<string>}. Set "arg" to a URL ONLY for \
ENTER_URL or NAVIGATE; otherwise "arg" must be "". Choose the single most useful \
next action. Do not invent actions.
"""

/// Selects the next action via a local Ollama model, falling back to a
/// deterministic rule-based driver when Ollama is unavailable or misbehaves —
/// so a missing model never fails the run (the invariants are the judge, the
/// LLM only explores).
public func selectAction(stateText: String, actions: [String]) -> AgentDecision {
    if let decision = ollamaSelect(stateText: stateText, actions: actions) {
        return decision
    }
    return deterministicFallback(stateText: stateText, actions: actions)
}

private func ollamaSelect(stateText: String, actions: [String]) -> AgentDecision? {
    let host = ProcessInfo.processInfo.environment["OLLAMA_HOST"] ?? "http://127.0.0.1:11434"
    let model = ProcessInfo.processInfo.environment["CRABBOX_AGENT_MODEL"] ?? "qwen2.5:0.5b"
    guard let url = URL(string: "\(host)/api/chat") else { return nil }

    let body: [String: Any] = [
        "model": model,
        "stream": false,
        "keep_alive": -1,
        "format": [
            "type": "object",
            "properties": [
                "action": ["type": "string", "enum": actions],
                "arg": ["type": "string"],
            ],
            "required": ["action", "arg"],
        ],
        "options": ["temperature": 0, "seed": 0, "num_ctx": 1024, "num_predict": 24],
        "messages": [
            ["role": "system", "content": systemPrompt],
            ["role": "user", "content": "ALLOWED ACTIONS: \(actions.joined(separator: ", "))\n\nSCREEN:\n\(stateText)"],
        ],
    ]

    guard let payload = try? JSONSerialization.data(withJSONObject: body) else { return nil }
    var request = URLRequest(url: url)
    request.httpMethod = "POST"
    request.setValue("application/json", forHTTPHeaderField: "Content-Type")
    request.httpBody = payload
    request.timeoutInterval = 30

    var responseData: Data?
    let semaphore = DispatchSemaphore(value: 0)
    URLSession.shared.dataTask(with: request) { data, response, _ in
        if let http = response as? HTTPURLResponse, http.statusCode == 200 { responseData = data }
        semaphore.signal()
    }.resume()
    _ = semaphore.wait(timeout: .now() + 35)

    guard let data = responseData,
          let envelope = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
          let message = envelope["message"] as? [String: Any],
          let content = message["content"] as? String,
          let contentData = content.data(using: .utf8),
          let parsed = try? JSONSerialization.jsonObject(with: contentData) as? [String: Any],
          let action = parsed["action"] as? String, actions.contains(action)
    else { return nil }

    var arg = (parsed["arg"] as? String) ?? ""
    if !argActions.contains(action) { arg = "" }
    return AgentDecision(action: action, arg: arg)
}

/// Rule-based selector mirroring the scenario heuristics; always returns an
/// in-vocabulary action so the agent loop can run without any model present.
func deterministicFallback(stateText: String, actions: [String]) -> AgentDecision {
    let s = stateText.lowercased()
    func pick(_ a: String) -> Bool { actions.contains(a) }

    if s.contains("offline") || s.contains("error") || s.contains("failed") {
        if pick("TAP_RETRY") { return AgentDecision(action: "TAP_RETRY", arg: "") }
        if pick("TAP_RELOAD") { return AgentDecision(action: "TAP_RELOAD", arg: "") }
    }
    if s.contains("settings_sheet") {
        if pick("TAP_CONNECT") { return AgentDecision(action: "TAP_CONNECT", arg: "") }
    }
    if pick("OPEN_SETTINGS") { return AgentDecision(action: "OPEN_SETTINGS", arg: "") }
    if pick("TAP_HOME") { return AgentDecision(action: "TAP_HOME", arg: "") }
    return AgentDecision(action: actions.first ?? "TAP_RELOAD", arg: "")
}

/// Drives a fresh `Sim` for `steps` LLM-selected actions, modeling the web-view
/// load that each remount/navigation would trigger, and returns the sim with
/// its collected invariant violations.
@discardableResult
public func runAgentLoop(env: AppEnv, steps: Int) -> Sim {
    let sim = Sim(env: env)
    sim.dispatch(.bootLoaded(storedURL: nil))
    sim.loadCycle(url: sim.state.homeURL, title: "Crabbox")

    for _ in 0..<steps {
        let decision = selectAction(stateText: renderState(sim.state), actions: agentActionVocabulary)
        apply(decision, to: sim)
    }
    return sim
}

private func apply(_ decision: AgentDecision, to sim: Sim) {
    switch decision.action {
    case "OPEN_SETTINGS":
        sim.dispatch(.openSettings)
    case "CLOSE_SETTINGS":
        sim.dispatch(.closeSettings)
    case "ENTER_URL":
        sim.dispatch(.setDraft(url: decision.arg))
    case "TAP_CONNECT":
        let r = sim.dispatch(.saveCoordinator)
        if r.effects.contains(where: { if case .persistCoordinator = $0 { return true }; return false }) {
            sim.loadCycle(url: sim.state.homeURL, title: "Crabbox")
        }
    case "TAP_RESET":
        sim.dispatch(.resetCoordinator)
        sim.loadCycle(url: sim.state.homeURL, title: "Crabbox")
    case "TAP_RELOAD", "TAP_RETRY":
        sim.dispatch(.reload)
        sim.loadCycle(url: sim.state.currentURL, title: sim.state.pageTitle)
    case "TAP_HOME":
        sim.dispatch(.goHome)
        sim.loadCycle(url: sim.state.homeURL, title: "Crabbox")
    case "TAP_BACK":
        let r = sim.dispatch(.goBack)
        if r.effects.contains(.webViewGoBack) {
            sim.dispatch(.webViewNavState(url: sim.state.homeURL, title: "Crabbox", canGoBack: false))
        }
    case "NAVIGATE":
        let r = sim.dispatch(.shouldStartLoad(url: decision.arg))
        if r.loadDecision == .load {
            sim.dispatch(.webViewNavState(url: decision.arg, title: "Page", canGoBack: true))
        }
    default:
        break
    }
}
