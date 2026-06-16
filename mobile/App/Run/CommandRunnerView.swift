//
//  CommandRunnerView.swift
//  Crabbox
//
//  Native command runner for iOS. The preferred path calls the compiled Go
//  CrabboxMobile core in-process. The coordinator workspace terminal remains a
//  fallback for builds where the Go core is intentionally omitted.
//

import Foundation
import SwiftUI
import CrabboxKit

struct CommandRunnerView: View {
    @EnvironmentObject private var settings: AppSettings
    @EnvironmentObject private var sandboxStore: SandboxStore

    @State private var repo = "openclaw/crabbox"
    @State private var branch = "main"
    @State private var commandLine = "crabbox run --provider islo --no-sync -- uname -a"
    @State private var output = "$ ready\n"
    @State private var status = "Idle"
    @State private var exitCode: Int?
    @State private var workspace: WorkspaceSession?
    @State private var runningMode: RunMode?
    @State private var isRunning = false
    @State private var isStopping = false
    @State private var terminal: WorkspaceTerminalStream?
    @State private var showingProviderSettings = false

    // Distribution / targeting (the command center part).
    // When sandboxes are available (islo key or coordinator token), user can pick
    // one or many remote sandboxes and the app will distribute the command across
    // them in parallel using the embedded Go core (CrabboxMobile). This is how the
    // iOS app becomes the orchestrator for fleets of islo.dev sandboxes.
    @State private var selectedTargetIDs: Set<String> = []
    @State private var distributeResults: [String: String] = [:] // id -> combined output
    @State private var isDistributing = false
    @State private var isMapReducing = false

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(spacing: 16) {
                    headerCard
                    if canShowRunner {
                        commandCard
                        terminalCard
                    } else {
                        connectCard
                    }
                }
                .padding(20)
            }
            .background(Theme.background.ignoresSafeArea())
            .navigationTitle("Run")
            .toolbarColorScheme(.dark, for: .navigationBar)
            .toolbar {
                ToolbarItem(placement: .topBarLeading) {
                    Button {
                        Task { await mapReduceDemo() }
                    } label: {
                        HStack(spacing: 5) {
                            if isMapReducing { ProgressView().tint(Theme.accent) }
                            else { Image(systemName: "sum") }
                            Text("Map-Reduce").font(.subheadline.weight(.semibold))
                        }
                        .foregroundStyle(Theme.accent)
                    }
                    .disabled(isMapReducing)
                    .accessibilityLabel("Map-Reduce over islo")
                }
                ToolbarItem(placement: .topBarTrailing) {
                    Button {
                        showingProviderSettings = true
                    } label: {
                        Image(systemName: "slider.horizontal.3")
                            .foregroundStyle(Theme.accent)
                    }
                    .accessibilityLabel("Provider settings")
                }
            }
            .sheet(isPresented: $showingProviderSettings) {
                ProviderSettingsView()
                    .environmentObject(settings)
            }
            .task {
                await sandboxStore.refresh(using: settings)
            }
            .onChange(of: settings.providerLabel) {
                Task { await sandboxStore.refresh(using: settings) }
            }
        }
    }

    private enum RunMode {
        case nativeCore
        case workspaceTerminal
    }

    private var canShowRunner: Bool {
        CrabboxBinaryEngine.isAvailable || settings.hasCrabboxToken
    }

    private var headerCard: some View {
        Card {
            HStack(spacing: 14) {
                Image(systemName: "terminal.fill")
                    .font(.system(size: 22, weight: .semibold))
                    .foregroundStyle(Theme.accent)
                    .frame(width: 44, height: 44)
                    .background(Theme.accent.opacity(0.12), in: RoundedRectangle(cornerRadius: 12, style: .continuous))

                VStack(alignment: .leading, spacing: 3) {
                    Text("Command")
                        .font(.caption)
                        .foregroundStyle(Theme.textMuted)
                    Text(status)
                        .font(.headline)
                        .foregroundStyle(Theme.textPrimary)
                        .lineLimit(2)
                        .minimumScaleFactor(0.7)
                }
                Spacer()
                statusPill
            }
        }
    }

    @ViewBuilder
    private var statusPill: some View {
        if isRunning {
            Pill(text: "Running", color: Theme.accent)
        } else if let exitCode {
            Pill(text: "Exit \(exitCode)", color: exitCode == 0 ? Theme.accent : Theme.danger)
        } else {
            Pill(text: canShowRunner ? "Ready" : "Offline", color: canShowRunner ? Theme.accent : Theme.danger)
        }
    }

    private var connectCard: some View {
        Card {
            VStack(spacing: 14) {
                Image(systemName: "key.horizontal.fill")
                    .font(.system(size: 30, weight: .semibold))
                    .foregroundStyle(Theme.accent)
                Text("Connect Crabbox")
                    .font(.headline)
                    .foregroundStyle(Theme.textPrimary)
                Button {
                    showingProviderSettings = true
                } label: {
                    Text("Open provider settings")
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(PrimaryButtonStyle())
            }
            .frame(maxWidth: .infinity)
        }
    }

    private var commandCard: some View {
        Card {
            VStack(alignment: .leading, spacing: 14) {
                fieldLabel("Crabbox command")
                TextEditor(text: $commandLine)
                    .font(.system(size: 14, design: .monospaced))
                    .foregroundStyle(Theme.textPrimary)
                    .scrollContentBackground(.hidden)
                    .padding(10)
                    .frame(minHeight: 104)
                    .background(Theme.field, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
                    .overlay(
                        RoundedRectangle(cornerRadius: 12, style: .continuous)
                            .strokeBorder(Theme.hairline, lineWidth: 1)
                    )
                    .disabled(isRunning)

                if !CrabboxBinaryEngine.isAvailable {
                    fieldLabel("Workspace fallback repository")
                    TextField("owner/repo", text: $repo)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .styledRunnerField()
                        .disabled(isRunning)

                    fieldLabel("Branch")
                    TextField("main", text: $branch)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .styledRunnerField()
                        .disabled(isRunning)
                }

                // Command center targeting & distribution UI.
                // Populated live from the shared SandboxStore (refreshed by Sandboxes tab
                // or on-demand). Lets the iOS app act as orchestrator: pick one or many
                // remote islo.dev (or coordinator) sandboxes and distribute the exact
                // same command to all of them in parallel. Results are shown per-sandbox.
                if !sandboxStore.handles.isEmpty || settings.makeProvisioner() != nil {
                    VStack(alignment: .leading, spacing: 8) {
                        Text("TARGET SANDBOXES (islo.dev / coordinator)")
                            .font(.caption.bold())
                            .foregroundStyle(Theme.textMuted)

                        if sandboxStore.handles.isEmpty {
                            Button {
                                Task { await sandboxStore.refresh(using: settings) }
                            } label: {
                                Label("Refresh active sandboxes", systemImage: "arrow.clockwise")
                            }
                            .font(.subheadline)
                        } else {
                            // Multi-select for distribution. Tap to toggle. Capped so
                            // the card stays compact when the fleet is large; the
                            // Map-Reduce action auto-discovers running boxes anyway.
                            if sandboxStore.handles.count > 8 {
                                Text("showing 8 of \(sandboxStore.handles.count) — Map-Reduce auto-targets running boxes")
                                    .font(.caption2).foregroundStyle(Theme.textMuted)
                            }
                            ForEach(sandboxStore.handles.prefix(8), id: \.id) { h in
                                let isSel = selectedTargetIDs.contains(h.id)
                                Button {
                                    if isSel { selectedTargetIDs.remove(h.id) } else { selectedTargetIDs.insert(h.id) }
                                } label: {
                                    HStack {
                                        Image(systemName: isSel ? "checkmark.circle.fill" : "circle")
                                            .foregroundStyle(isSel ? Theme.accent : Theme.textMuted)
                                        VStack(alignment: .leading) {
                                            Text(h.id).font(.subheadline.monospacedDigit())
                                            Text("\(h.provider) · \(h.status)").font(.caption).foregroundStyle(Theme.textMuted)
                                        }
                                        Spacer()
                                    }
                                }
                                .buttonStyle(.plain)
                                .padding(.vertical, 4)
                            }

                            HStack {
                                Button("All") { selectedTargetIDs = Set(sandboxStore.handles.map { $0.id }) }
                                    .font(.caption)
                                Button("None") { selectedTargetIDs.removeAll() }
                                    .font(.caption)
                                Spacer()
                                if !selectedTargetIDs.isEmpty {
                                    Text("\(selectedTargetIDs.count) selected")
                                        .font(.caption)
                                        .foregroundStyle(Theme.accent)
                                }
                            }

                            // Map-reduce demo: shard sum(1...1000) across the selected
                            // islo sandboxes (MAP), then aggregate on the phone (REDUCE).
                            Button {
                                Task { await mapReduceDemo() }
                            } label: {
                                HStack(spacing: 8) {
                                    if isMapReducing { ProgressView().tint(Theme.accent) }
                                    else { Image(systemName: "sum") }
                                    Text(selectedTargetIDs.isEmpty ? "Map-Reduce  Σ(1…1000) over islo" : "Map-Reduce  Σ(1…1000) across \(selectedTargetIDs.count)")
                                        .font(.footnote.weight(.semibold))
                                }
                                .frame(maxWidth: .infinity)
                                .padding(.vertical, 10)
                                .background(Theme.accent.opacity(0.12), in: RoundedRectangle(cornerRadius: 10))
                                .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(Theme.accent.opacity(0.35), lineWidth: 1))
                                .foregroundStyle(Theme.accent)
                            }
                            .disabled(isMapReducing)
                            .padding(.top, 4)
                        }
                    }
                    .padding(8)
                    .background(Theme.field.opacity(0.6), in: RoundedRectangle(cornerRadius: 10))
                }

                HStack(spacing: 12) {
                    Button {
                        Task { await runCommand() }
                    } label: {
                        HStack {
                            if isRunning || isDistributing {
                                ProgressView().tint(.black)
                            } else {
                                Image(systemName: "play.fill")
                            }
                            Text((isRunning || isDistributing) ? "Running..." : (selectedTargetIDs.isEmpty ? "Run" : "Distribute & Run"))
                        }
                    }
                    .buttonStyle(PrimaryButtonStyle())
                    .disabled(isRunning || isStopping || isDistributing)

                    if runningMode == .workspaceTerminal || workspace != nil {
                        Button(role: .destructive) {
                            Task { await stopWorkspace() }
                        } label: {
                            if isStopping {
                                ProgressView().tint(Theme.danger)
                            } else {
                                Image(systemName: "stop.fill")
                            }
                        }
                        .buttonStyle(SecondaryButtonStyle(fullWidth: false))
                        .disabled(isStopping)
                        .accessibilityLabel("Stop workspace")
                    }

                    if !selectedTargetIDs.isEmpty {
                        Button("Clear targets") { selectedTargetIDs.removeAll() }
                            .font(.caption)
                            .foregroundStyle(Theme.textMuted)
                    }
                }
            }
        }
    }

    private var terminalCard: some View {
        Card {
            VStack(alignment: .leading, spacing: 12) {
                HStack {
                    Text(terminalTitle)
                        .font(.headline)
                        .foregroundStyle(Theme.textPrimary)
                    Spacer()
                    Button {
                        output = ""
                        exitCode = nil
                    } label: {
                        Image(systemName: "trash")
                            .foregroundStyle(Theme.textMuted)
                    }
                    .accessibilityLabel("Clear terminal")
                }

                ScrollView {
                    Text(output.isEmpty ? "$" : output)
                        .font(.system(size: 12, design: .monospaced))
                        .foregroundStyle(Theme.textPrimary)
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .topLeading)
                        .padding(12)
                }
                .frame(minHeight: 280)
                .background(Color.black, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
                .overlay(
                    RoundedRectangle(cornerRadius: 12, style: .continuous)
                        .strokeBorder(Theme.hairline, lineWidth: 1)
                )
            }
        }
    }

    private var terminalTitle: String {
        if runningMode == .nativeCore { return "CrabboxMobile" }
        return workspace?.id ?? "Terminal"
    }

    private func fieldLabel(_ text: String) -> some View {
        Text(text.uppercased())
            .font(.system(size: 11, weight: .bold))
            .foregroundStyle(Theme.textMuted)
    }

    @MainActor
    private func runCommand() async {
        guard !isRunning && !isDistributing else { return }
        let baseArgs: [String]
        do {
            baseArgs = try parseCrabboxCommandLine(commandLine)
        } catch {
            status = "Invalid command"
            output = "parse error: \(describe(error))\n"
            exitCode = 2
            return
        }

        if commandLineNeedsIsloKey(baseArgs) && !settings.hasIsloProvider {
            status = "islo key missing"
            output = "provider=islo requires an islo key in provider settings\n"
            exitCode = 2
            showingProviderSettings = true
            return
        }

        // Distribution mode (the command center feature).
        // If user selected sandboxes in the UI (populated from the live islo/coordinator
        // list), we fan the command out to all of them in parallel using the embedded
        // Go core. Each run gets `--id <sandbox>` injected so it targets that remote box.
        // This lets the iPhone act as the central dispatcher for work across a fleet of
        // islo.dev remote sandboxes.
        let targets = selectedTargetIDs.isEmpty ? [] : sandboxStore.handles.filter { selectedTargetIDs.contains($0.id) }

        if !targets.isEmpty && CrabboxBinaryEngine.isAvailable {
            await distributeToSandboxes(baseArgs: baseArgs, targets: targets)
            return
        }

        if CrabboxBinaryEngine.isAvailable {
            await runNativeCrabbox(baseArgs)
            return
        }

        await runWorkspaceCommand(baseArgs)
    }

    /// Distribute the same command (with --id injected) to multiple remote sandboxes
    /// in parallel. Results are collected per-sandbox and shown in the output area.
    /// This is the "iOS as command center" distribution path.
    @MainActor
    private func distributeToSandboxes(baseArgs: [String], targets: [SandboxHandle]) async {
        isDistributing = true
        distributeResults = [:]
        status = "Distributing to \(targets.count) sandbox(es)..."
        output = "$ distributing \(commandLine) to \(targets.map { $0.id }.joined(separator: ", "))\n"

        // Capture MainActor-isolated settings on the actor before entering concurrent tasks.
        let isloKey = settings.isloKey?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        let coordinator = settings.coordinatorURL.trimmingCharacters(in: .whitespacesAndNewlines)
        let crabboxToken = settings.crabboxToken?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        // Writable state/config dirs for the Go core's lease-claim system, shared
        // by every parallel run (the claims are keyed by repo+slug, so distinct
        // slugs below keep per-target leases separate).
        let baseEnv = CrabboxRuntimePaths.sandboxEnvironment()

        await withTaskGroup(of: (String, CrabboxBinaryResult?).self) { group in
            for h in targets {
                group.addTask {
                    // Target a distinct lease/sandbox per selection via `--slug`
                    // (a `run` flag, inserted AFTER the `run` token). `crabbox run`
                    // has no `--id` flag — that belongs to `stop`/`resolve`.
                    let args = Self.injectRunFlag(baseArgs, flag: "--slug", value: h.id)
                    var env: [String: String] = baseEnv
                    if !isloKey.isEmpty {
                        env["ISLO_API_KEY"] = isloKey
                        env["CRABBOX_ISLO_API_KEY"] = isloKey
                    }
                    if !coordinator.isEmpty { env["CRABBOX_COORDINATOR"] = coordinator }
                    if !crabboxToken.isEmpty {
                        env["CRABBOX_COORDINATOR_TOKEN"] = crabboxToken
                    }

                    do {
                        let res = try CrabboxBinaryEngine.run(args: args, env: env)
                        return (h.id, res)
                    } catch {
                        let errRes = CrabboxBinaryResult(exitCode: 1, stdout: "", stderr: "error: \(error)", error: String(describing: error))
                        return (h.id, errRes)
                    }
                }
            }

            for await (id, res) in group {
                if let res {
                    let chunk = "\n=== \(id) (exit \(res.exitCode)) ===\n\(res.stdout)\(res.stderr.isEmpty ? "" : "\n[stderr]\n\(res.stderr)")\(res.error.map { "\n[err] \($0)" } ?? "")\n"
                    distributeResults[id] = chunk
                    await MainActor.run {
                        output += chunk
                    }
                }
            }
        }

        isDistributing = false
        status = "Distributed to \(targets.count)"
        // Keep the single-run output area as aggregate; per-id also in distributeResults if UI wants tabs later.
    }

    /// Map-reduce demo: shard `Σ(1…n)` (with min/max) across the selected islo
    /// sandboxes (MAP) via `IsloClient.exec` in parallel, then aggregate on the
    /// phone (REDUCE). The iOS app as a command center over islo.dev.
    @MainActor
    private func mapReduceDemo() async {
        guard let key = settings.isloKey?.trimmingCharacters(in: .whitespacesAndNewlines), !key.isEmpty,
              let client = IsloClient(apiKey: key, baseURL: settings.isloBaseURL) else {
            status = "islo key missing"
            output = "Add your islo key in provider settings to map-reduce over islo.\n"
            showingProviderSettings = true
            return
        }

        isMapReducing = true
        defer { isMapReducing = false }
        exitCode = nil
        runningMode = .nativeCore
        let n = 1000

        // Targets: the user's explicit selection, else auto-discover running islo
        // boxes (preferring fresh `crabbox-mapreduce-*` demo boxes) so the demo is
        // a single tap.
        var targets = sandboxStore.handles.filter { selectedTargetIDs.contains($0.id) }.map { $0.id }
        if targets.isEmpty {
            status = "Discovering running islo sandboxes…"
            output = "$ map-reduce  Σ(1…\(n))  — discovering running islo sandboxes…\n\n"
            let all = (try? await client.listSandboxes()) ?? []
            let running = all.filter { $0.status == "running" }.map(\.name)
            let demo = running.filter { $0.hasPrefix("crabbox-mapreduce") }
            targets = (demo.isEmpty ? running : demo)
            targets = Array(targets.prefix(4))
        }
        guard !targets.isEmpty else {
            status = "No running sandboxes"
            output = "No running islo sandboxes to map-reduce across. Launch some in the Sandboxes tab.\n"
            return
        }

        let probeTargets = targets
        status = "Map-reduce: probing \(probeTargets.count) sandbox(es)…"
        output = "$ map-reduce  Σ(1…\(n))  over \(probeTargets.count) islo sandbox(es)\n\n"

        // PROBE: keep only sandboxes that actually respond, so the shards cover
        // 1…n exactly even if some selected boxes are stopped/failed.
        var live: [String] = []
        await withTaskGroup(of: (String, Bool).self) { group in
            for name in probeTargets {
                group.addTask {
                    let res = try? await client.exec(name: name, script: "echo ok")
                    return (name, (res?.stdout ?? "").contains("ok"))
                }
            }
            for await (name, ok) in group {
                output += ok ? "  [probe] \(name) ✓ live\n" : "  [probe] \(name) ✗ skipped\n"
                if ok { live.append(name) }
            }
        }
        guard !live.isEmpty else {
            output += "\n  no live sandboxes among the selection.\n"
            status = "No live sandboxes"; exitCode = 1; return
        }

        let s = live.count
        status = "Map-reduce across \(s) sandbox(es)…"
        output += "\n"

        // MAP: one shard per LIVE sandbox, executed in parallel over islo.
        let shards = live.enumerated().map { (idx: $0.offset, name: $0.element) }
        var lines: [(name: String, sum: Int, mn: Int, mx: Int)] = []
        await withTaskGroup(of: (String, String).self) { group in
            for shard in shards {
                let per = n / s
                let a = shard.idx * per + 1
                let b = (shard.idx == s - 1) ? n : (shard.idx + 1) * per
                let name = shard.name
                group.addTask {
                    let script = "seq \(a) \(b) | awk '{sum+=$1; if(mn==\"\"||$1<mn)mn=$1; if($1>mx)mx=$1} END{print sum, mn, mx}'"
                    let res = try? await client.exec(name: name, script: script)
                    return (name, (res?.stdout ?? "").trimmingCharacters(in: .whitespacesAndNewlines))
                }
            }
            for await (name, out) in group {
                let parts = out.split(separator: " ").compactMap { Int($0) }
                if parts.count == 3 {
                    lines.append((name, parts[0], parts[1], parts[2]))
                    output += "  [map] \(name) → sum=\(parts[0]) min=\(parts[1]) max=\(parts[2])\n"
                } else {
                    output += "  [map] \(name) → \(out.isEmpty ? "(no output)" : out)\n"
                }
            }
        }

        // REDUCE on the phone.
        let total = lines.reduce(0) { $0 + $1.sum }
        let gmin = lines.map(\.mn).min()
        let gmax = lines.map(\.mx).max()
        let expected = n * (n + 1) / 2
        output += "\n  [reduce] Σ = \(total)  ·  min = \(gmin.map(String.init) ?? "—")  ·  max = \(gmax.map(String.init) ?? "—")  across \(lines.count) sandbox(es)\n"
        let ok = total == expected && gmin == 1 && gmax == n
        output += ok ? "  ✅ matches Σ(1…\(n)) = \(expected)\n" : "  expected Σ = \(expected)\n"
        exitCode = ok ? 0 : 1
        status = ok ? "✓ Σ=\(total) · \(lines.count) boxes" : "Map-reduce done"
        runningMode = nil
    }

    @MainActor
    private func runNativeCrabbox(_ args: [String]) async {
        isRunning = true
        runningMode = .nativeCore
        exitCode = nil
        workspace = nil
        status = "Running Go core"
        output = "$ crabbox \(args.joined(separator: " "))\n"

        // Pin the Go core's state/config dirs to writable sandbox paths so the
        // islo lease-claim system (crabboxStateDir -> XDG_STATE_HOME/$HOME) works
        // inside the iOS app container.
        var env: [String: String] = CrabboxRuntimePaths.sandboxEnvironment()
        if let key = settings.isloKey?.trimmingCharacters(in: .whitespacesAndNewlines), !key.isEmpty {
            env["ISLO_API_KEY"] = key
            env["CRABBOX_ISLO_API_KEY"] = key
        }
        let coordinator = settings.coordinatorURL.trimmingCharacters(in: .whitespacesAndNewlines)
        if !coordinator.isEmpty {
            env["CRABBOX_COORDINATOR"] = coordinator
        }
        if let token = settings.crabboxToken?.trimmingCharacters(in: .whitespacesAndNewlines), !token.isEmpty {
            env["CRABBOX_COORDINATOR_TOKEN"] = token
        }

        do {
            let result = try await Task.detached(priority: .userInitiated) {
                try CrabboxBinaryEngine.run(args: args, env: env)
            }.value
            appendOutput(result.stdout)
            appendOutput(result.stderr)
            if let error = result.error, !error.isEmpty {
                appendLine("error: \(error)")
            }
            exitCode = result.exitCode
            status = result.exitCode == 0 ? "Succeeded" : "Failed"
        } catch {
            exitCode = 1
            status = "Failed"
            appendLine("error: \(describe(error))")
        }
        isRunning = false
        runningMode = nil
    }

    @MainActor
    private func runWorkspaceCommand(_ args: [String]) async {
        guard let token = settings.crabboxToken?.trimmingCharacters(in: .whitespacesAndNewlines), !token.isEmpty,
              let client = CoordinatorClient(coordinatorURL: settings.coordinatorURL, token: token) else {
            status = "Provider missing"
            showingProviderSettings = true
            return
        }

        let id = makeWorkspaceID()
        let wrappedCommand = crabboxWorkspaceCommand(workspaceCommand(from: args))
        let request = WorkspaceCreateRequest(
            id: id,
            repo: repo.trimmingCharacters(in: .whitespacesAndNewlines),
            branch: branch.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ? "main" : branch,
            command: wrappedCommand
        )

        isRunning = true
        runningMode = .workspaceTerminal
        exitCode = nil
        workspace = nil
        status = "Creating workspace"
        output = "$ crabbox \(args.joined(separator: " "))\n[workspace fallback \(id)]\n"

        do {
            var session = try await client.createWorkspace(request)
            workspace = session
            appendLine(session.message)

            var lastStatus = session.status
            for _ in 0..<120 where !session.isReady {
                if session.isTerminal {
                    throw LLMError.unavailable(session.message)
                }
                try await Task.sleep(nanoseconds: 2_000_000_000)
                session = try await client.getWorkspace(id: id)
                workspace = session
                if session.status != lastStatus {
                    lastStatus = session.status
                    appendLine(session.message)
                }
            }

            guard session.isReady else {
                throw LLMError.unavailable("workspace did not become ready")
            }
            guard let attachURL = session.attachURL, let url = URL(string: attachURL) else {
                throw LLMError.unavailable("workspace terminal unavailable")
            }

            status = "Streaming terminal"
            appendLine("workspace ready")
            let stream = WorkspaceTerminalStream(
                url: url,
                token: token,
                onOutput: { chunk in
                    Task { @MainActor in
                        appendOutput(chunk)
                    }
                },
                onEnd: { reason in
                    Task { @MainActor in
                        terminal = nil
                        isRunning = false
                        status = reason
                        readExitCode()
                    }
                }
            )
            terminal = stream
            stream.start()
        } catch {
            terminal = nil
            isRunning = false
            runningMode = nil
            status = "Failed"
            appendLine("error: \(describe(error))")
        }
    }

    @MainActor
    private func stopWorkspace() async {
        terminal?.cancel()
        terminal = nil
        guard let id = workspace?.id,
              let token = settings.crabboxToken?.trimmingCharacters(in: .whitespacesAndNewlines), !token.isEmpty,
              let client = CoordinatorClient(coordinatorURL: settings.coordinatorURL, token: token) else {
            isRunning = false
            runningMode = nil
            return
        }
        isStopping = true
        defer { isStopping = false }
        do {
            let stopped = try await client.deleteWorkspace(id: id)
            workspace = stopped
            status = stopped.status.capitalized
            appendLine(stopped.message)
        } catch {
            appendLine("stop error: \(describe(error))")
        }
        isRunning = false
        runningMode = nil
    }

    private func workspaceCommand(from args: [String]) -> String {
        if let separator = args.firstIndex(of: "--") {
            let commandArgs = args[args.index(after: separator)...]
            if !commandArgs.isEmpty {
                return commandArgs.map(shellQuote).joined(separator: " ")
            }
        }
        return commandLine
    }

    private func shellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\"'\"'") + "'"
    }

    /// Inserts a `run`-scoped flag (e.g. `--slug <id>`) immediately after the
    /// `run` subcommand token, so it lands before the `--` command separator and
    /// is parsed as a run flag (not a global flag before the subcommand). No-ops
    /// if the flag is already present.
    nonisolated private static func injectRunFlag(_ args: [String], flag: String, value: String) -> [String] {
        guard !args.contains(flag) else { return args }
        var out = args
        if let runIdx = out.firstIndex(of: "run") {
            out.insert(contentsOf: [flag, value], at: runIdx + 1)
        } else {
            out.insert(contentsOf: [flag, value], at: min(1, out.count))
        }
        return out
    }

    private func makeWorkspaceID() -> String {
        let suffix = String(Int(Date().timeIntervalSince1970), radix: 36)
        let raw = "ios-\(suffix)"
        return isValidWorkspaceID(raw) ? raw : "ios-run"
    }

    @MainActor
    private func appendLine(_ line: String) {
        appendOutput("\n[\(line)]\n")
    }

    @MainActor
    private func appendOutput(_ text: String) {
        output += Self.cleanTerminal(text)
        if output.count > 80_000 {
            output = String(output.suffix(80_000))
        }
        readExitCode()
    }

    @MainActor
    private func readExitCode() {
        guard let marker = output.range(of: crabboxWorkspaceExitMarker, options: .backwards) else { return }
        let tail = output[marker.upperBound...]
        let digits = tail.prefix { $0.isNumber }
        if let code = Int(digits) {
            exitCode = code
            if !isRunning { status = code == 0 ? "Succeeded" : "Failed" }
        }
    }

    private func describe(_ error: Error) -> String {
        if let llm = error as? LLMError { return llm.description }
        if let custom = error as? CustomStringConvertible { return custom.description }
        return (error as NSError).localizedDescription
    }

    private static func cleanTerminal(_ text: String) -> String {
        var result = ""
        var iterator = text.makeIterator()
        while let char = iterator.next() {
            if char == "\u{001B}" {
                while let next = iterator.next(), !next.isLetter {
                    continue
                }
                continue
            }
            if char == "\r" { continue }
            result.append(char)
        }
        return result
    }
}

private final class WorkspaceTerminalStream {
    private let task: URLSessionWebSocketTask
    private let onOutput: (String) -> Void
    private let onEnd: (String) -> Void
    private var stopped = false

    init(url: URL, token: String, onOutput: @escaping (String) -> Void, onEnd: @escaping (String) -> Void) {
        var request = URLRequest(url: url)
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        self.task = URLSession.shared.webSocketTask(with: request)
        self.onOutput = onOutput
        self.onEnd = onEnd
    }

    func start() {
        task.resume()
        sendResize(cols: 100, rows: 32)
        receive()
    }

    func cancel() {
        stopped = true
        task.cancel(with: .goingAway, reason: nil)
    }

    private func receive() {
        task.receive { [weak self] result in
            guard let self else { return }
            switch result {
            case .success(let message):
                let bytes: Int
                switch message {
                case .data(let data):
                    bytes = data.count
                    self.onOutput(String(data: data, encoding: .utf8) ?? "")
                case .string(let text):
                    bytes = Data(text.utf8).count
                    self.onOutput(text)
                @unknown default:
                    bytes = 0
                }
                if bytes > 0 {
                    self.sendAck(bytes: bytes)
                }
                self.receive()
            case .failure:
                if !self.stopped {
                    self.onEnd(self.task.closeCode == .normalClosure ? "Finished" : "Disconnected")
                }
            }
        }
    }

    private func sendResize(cols: Int, rows: Int) {
        task.send(.string(#"{"type":"resize","cols":\#(cols),"rows":\#(rows)}"#)) { _ in }
    }

    private func sendAck(bytes: Int) {
        task.send(.string(#"{"type":"ack","bytes":\#(bytes)}"#)) { _ in }
    }
}

private extension View {
    func styledRunnerField() -> some View {
        self
            .foregroundStyle(Theme.textPrimary)
            .padding(.horizontal, 14)
            .padding(.vertical, 12)
            .background(Theme.field, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
            .overlay(
                RoundedRectangle(cornerRadius: 12, style: .continuous)
                    .strokeBorder(Theme.hairline, lineWidth: 1)
            )
    }
}
