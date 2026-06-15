import CrabboxE2E
import CrabboxKit
import Foundation

// crabbox-sim — the headless end-to-end runner for the Crabbox iOS app.
//
//   swift run crabbox-sim            Run all deterministic scenarios + invariants.
//   swift run crabbox-sim --agent    Additionally run the tiny-LLM (Ollama) driver.
//   swift run crabbox-sim --json     Emit a machine-readable JSON report.
//
// This is exactly what the islo provider runs inside a Linux sandbox.

let args = Set(CommandLine.arguments.dropFirst())
let runAgent = args.contains("--agent")
let jsonOut = args.contains("--json")
let agentSteps = Int(ProcessInfo.processInfo.environment["CRABBOX_AGENT_STEPS"] ?? "") ?? 24

// `--chat`: a real end-to-end LLM smoke test against a SandboxEngine (Ollama).
// Proves the engine the iOS app uses works against a live model. Points at
// OLLAMA_HOST (default local) with CRABBOX_AGENT_MODEL (default tinyllama).
if args.contains("--chat") {
    let host = ProcessInfo.processInfo.environment["OLLAMA_HOST"] ?? "http://127.0.0.1:11434"
    let model = ProcessInfo.processInfo.environment["CRABBOX_AGENT_MODEL"] ?? "tinyllama"
    guard let engine = SandboxEngine(endpoint: host, model: model) else {
        FileHandle.standardError.write(Data("bad OLLAMA_HOST\n".utf8)); exit(2)
    }
    let ready = await engine.isReady()
    print("engine: \(engine.displayName) — ready: \(ready)")
    guard ready else {
        FileHandle.standardError.write(Data("engine not ready (is Ollama running with the model?)\n".utf8)); exit(2)
    }
    let reply = try await engine.reply(
        messages: [
            ChatMessage(role: .system, content: "You are Crabbox's assistant. Answer in one short sentence."),
            ChatMessage(role: .user, content: "What is a sandbox in one sentence?"),
        ],
        options: LLMOptions(temperature: 0, seed: 0, numCtx: 1024, numPredict: 64)
    )
    print("reply: \(reply.trimmingCharacters(in: .whitespacesAndNewlines))")
    exit(0)
}

// `--islo-demo`: the live "trigger islo from the phone" e2e, exercising the
// EXACT IsloClient the app's Sandboxes tab uses. Validates token exchange,
// sandbox lifecycle, and (with CRABBOX_ISLO_LLM=1) the full provision → Ollama
// bootstrap → share → chat flow. Needs ISLO_API_KEY in the environment.
if args.contains("--islo-demo") {
    let env = ProcessInfo.processInfo.environment
    guard let key = env["ISLO_API_KEY"] ?? env["CRABBOX_ISLO_API_KEY"], !key.isEmpty else {
        FileHandle.standardError.write(Data("set ISLO_API_KEY\n".utf8)); exit(2)
    }
    let base = env["ISLO_BASE_URL"] ?? "https://api.islo.dev"
    let withLLM = env["CRABBOX_ISLO_LLM"] == "1"
    guard let islo = IsloClient(apiKey: key, baseURL: base, timeout: withLLM ? 600 : 60) else {
        FileHandle.standardError.write(Data("bad islo config\n".utf8)); exit(2)
    }
    do {
        let existing = try await islo.listSandboxes()
        print("auth OK — \(existing.count) existing sandbox(es)")
        let name = "crab-e2e-\(env["USER"] ?? "x")"
        print("creating sandbox \(name)…")
        let created = try await islo.createSandbox(name: name)
        print("created: \(created.name) [\(created.status)]")

        if withLLM {
            let model = env["CRABBOX_AGENT_MODEL"] ?? "qwen2.5:0.5b"
            print("bootstrapping Ollama + \(model) (this takes a few minutes)…")
            let boot = try await islo.exec(name: created.name, script: isloOllamaBootstrapScript(model: model))
            print("bootstrap exit=\(boot.exitCode)")
            let share = try await islo.createShare(name: created.name, port: 11434)
            print("share: \(share.url)")
            if let engine = SandboxEngine(endpoint: share.url, model: model) {
                let reply = try await engine.reply(
                    messages: [ChatMessage(role: .user, content: "Say hello from the sandbox in one sentence.")],
                    options: LLMOptions(temperature: 0, numPredict: 48)
                )
                print("LLM reply: \(reply.trimmingCharacters(in: .whitespacesAndNewlines))")
            }
        }

        print("cleaning up: deleting \(created.name)…")
        try await islo.deleteSandbox(name: created.name)
        print("DONE — islo e2e succeeded.")
        exit(0)
    } catch {
        FileHandle.standardError.write(Data("islo e2e failed: \(error)\n".utf8))
        exit(1)
    }
}

struct ScenarioResult { let name: String; let steps: Int; let violations: [String] }

var results: [ScenarioResult] = []

let unitViolations = runUnitChecks()
results.append(ScenarioResult(name: "unit_vectors", steps: 0, violations: unitViolations))

for scenario in scenarios {
    let sim = Sim(env: scenario.env)
    scenario.body(sim)
    results.append(ScenarioResult(name: scenario.name, steps: sim.trace.count, violations: sim.violations))
}

if runAgent {
    for env in [AppEnv(allowLocalHTTP: false), AppEnv(allowLocalHTTP: true)] {
        let sim = runAgentLoop(env: env, steps: agentSteps)
        let label = env.allowLocalHTTP ? "agent_explore_dev" : "agent_explore_prod"
        results.append(ScenarioResult(name: label, steps: sim.trace.count, violations: sim.violations))
    }
}

let totalViolations = results.reduce(0) { $0 + $1.violations.count }
let failed = results.filter { !$0.violations.isEmpty }

if jsonOut {
    let payload: [String: Any] = [
        "scenarios": results.count,
        "totalSteps": results.reduce(0) { $0 + $1.steps },
        "violations": totalViolations,
        "failures": failed.map { ["name": $0.name, "violations": $0.violations] },
        "ok": totalViolations == 0,
    ]
    if let data = try? JSONSerialization.data(withJSONObject: payload, options: [.prettyPrinted, .sortedKeys]),
       let text = String(data: data, encoding: .utf8) {
        print(text)
    }
} else {
    print("crabbox-sim — headless e2e for the Crabbox iOS app\n")
    for result in results {
        let mark = result.violations.isEmpty ? "PASS" : "FAIL"
        print(String(format: "  [%@] %-28@ %3d steps", mark, result.name as NSString, result.steps))
        for violation in result.violations { print("        - \(violation)") }
    }
    let stepTotal = results.reduce(0) { $0 + $1.steps }
    print("\n\(results.count) scenarios, \(stepTotal) steps, \(totalViolations) invariant violation(s)")
    print(totalViolations == 0 ? "OK — all invariants held." : "FAILED — see violations above.")
}

exit(totalViolations == 0 ? 0 : 1)
