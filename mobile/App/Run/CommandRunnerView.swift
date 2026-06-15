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
                        .lineLimit(1)
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

                HStack(spacing: 12) {
                    Button {
                        Task { await runCommand() }
                    } label: {
                        HStack {
                            if isRunning {
                                ProgressView().tint(.black)
                            } else {
                                Image(systemName: "play.fill")
                            }
                            Text(isRunning ? "Running" : "Run")
                        }
                    }
                    .buttonStyle(PrimaryButtonStyle())
                    .disabled(isRunning || isStopping)

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
        guard !isRunning else { return }
        let args: [String]
        do {
            args = try parseCrabboxCommandLine(commandLine)
        } catch {
            status = "Invalid command"
            output = "parse error: \(describe(error))\n"
            exitCode = 2
            return
        }

        if commandLineNeedsIsloKey(args) && !settings.hasIsloProvider {
            status = "islo key missing"
            output = "provider=islo requires an islo key in provider settings\n"
            exitCode = 2
            showingProviderSettings = true
            return
        }

        if CrabboxBinaryEngine.isAvailable {
            await runNativeCrabbox(args)
            return
        }

        await runWorkspaceCommand(args)
    }

    @MainActor
    private func runNativeCrabbox(_ args: [String]) async {
        isRunning = true
        runningMode = .nativeCore
        exitCode = nil
        workspace = nil
        status = "Running Go core"
        output = "$ crabbox \(args.joined(separator: " "))\n"

        var env: [String: String] = [:]
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
