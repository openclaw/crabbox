// Runtime proof controller. Compile with the pinned UTM source's generated
// Scripting/UTMScripting.swift. All AppleEvents address one verified process.
import AppKit
import Foundation
import CoreFoundation
import ScriptingBridge

struct ProofError: Error, CustomStringConvertible {
    let description: String
    init(_ description: String) { self.description = description }
}

struct GuestResult {
    let exited: Bool
    let exitCode: Int?
    let stdout: Data?
    let stderr: Data?

    static func boolean(_ value: Any?) throws -> Bool {
        guard let number = value as? NSNumber,
              CFGetTypeID(number) == CFBooleanGetTypeID() else {
            throw ProofError("Guest completion state must be a typed boolean")
        }
        return number.boolValue
    }

    static func integer(_ value: Any?) throws -> Int {
        guard let number = value as? NSNumber,
              CFGetTypeID(number) != CFBooleanGetTypeID(),
              ["c", "C", "s", "S", "i", "I", "l", "L", "q", "Q"].contains(String(cString: number.objCType)),
              number.doubleValue == Double(number.intValue) else {
            throw ProofError("Guest process status must be a typed integer")
        }
        return number.intValue
    }

    static func decode(_ record: [AnyHashable: Any]) throws -> GuestResult {
        // UTM.sdef exposes Cocoa hasExited as the AppleScript property 'exited'.
        // PID-bound ScriptingBridge returns 'exited'; accept the explicit Cocoa
        // alias too, but never accept missing, loosely coerced or conflicting state.
        guard record["exited"] != nil || record["hasExited"] != nil else {
            throw ProofError("Guest process returned no completion state")
        }
        let exited = try boolean(record["exited"] ?? record["hasExited"])
        if let alias = record["hasExited"], record["exited"] != nil {
            guard try boolean(alias) == exited else { throw ProofError("Conflicting guest completion states") }
        }
        if !exited { return GuestResult(exited: false, exitCode: nil, stdout: nil, stderr: nil) }
        let code = try integer(record["exitCode"])
        let signal = try integer(record["signalCode"])
        guard (0...255).contains(code), signal == 0,
              let output = record["outputData"] as? String,
              let error = record["errorData"] as? String,
              let stdout = Data(base64Encoded: output),
              let stderr = Data(base64Encoded: error) else {
            throw ProofError("Guest process returned incomplete, abnormal or invalid results")
        }
        return GuestResult(exited: true, exitCode: code, stdout: stdout, stderr: stderr)
    }
}

#if !PROOF_IDENTITY_TESTS
final class EventErrors: NSObject, SBApplicationDelegate {
    var failure: Error?
    func eventDidFail(_ event: UnsafePointer<AppleEvent>, withError error: Error) -> Any? {
        failure = error
        return nil
    }
}

#endif

struct ProcessIdentity {
    let pid: pid_t
    let launchTime: Double?
    let executable: URL?
    let bundleIdentifier: String?
    let terminated: Bool

    static func snapshot() -> [ProcessIdentity] {
        NSWorkspace.shared.runningApplications.map { app in
            ProcessIdentity(pid: app.processIdentifier, launchTime: app.launchDate?.timeIntervalSince1970,
                            executable: app.executableURL?.resolvingSymlinksInPath(),
                            bundleIdentifier: app.bundleIdentifier, terminated: app.isTerminated)
        }
    }
}

struct Identity {
    let pid: pid_t
    let launchTime: Double
    let executable: URL

    func verify(processes: [ProcessIdentity] = ProcessIdentity.snapshot(), exclusive: Bool = true) throws {
        guard let running = processes.first(where: { $0.pid == pid }),
              !running.terminated, running.launchTime == launchTime,
              running.executable == executable else {
            throw ProofError("Built UTM process identity changed or exited; refusing AppleEvent and fallback")
        }
        if exclusive { try Self.rejectOtherCopies(expected: executable, processes: processes) }
    }

    static func rejectOtherCopies(expected: URL, processes: [ProcessIdentity]) throws {
        let others = processes.filter {
            $0.bundleIdentifier == "com.utmapp.UTM" && !$0.terminated && $0.executable != expected
        }
        guard others.isEmpty else {
            throw ProofError("Another UTM application is running; refusing ambiguous built-app proof. Preserve its sessions and inspect application paths.")
        }
    }

    static func discover(app: URL, processes: [ProcessIdentity] = ProcessIdentity.snapshot()) throws -> Identity {
        let expected = app.appendingPathComponent("Contents/MacOS/UTM").resolvingSymlinksInPath()
        try rejectOtherCopies(expected: expected, processes: processes)
        let matches = processes.filter { !$0.terminated && $0.executable == expected }
        guard matches.count == 1, let process = matches.first, let launched = process.launchTime else {
            throw ProofError("Expected exactly one running packaged UTM executable at the built path")
        }
        return Identity(pid: process.pid, launchTime: launched, executable: expected)
    }
}

#if !PROOF_IDENTITY_TESTS
final class Controller {
    let identity: Identity
    let application: SBApplication
    let errors = EventErrors()
    let exclusive: Bool

    init(identity: Identity, exclusive: Bool) throws {
        self.identity = identity
        self.exclusive = exclusive
        try identity.verify(exclusive: exclusive)
        // PID targeting never resolves a URL or bundle identifier through LaunchServices.
        guard let application = SBApplication(processIdentifier: identity.pid) else {
            throw ProofError("Cannot address the verified UTM process")
        }
        self.application = application
        application.delegate = errors
    }

    func event<T>(_ body: () -> T) throws -> T {
        try identity.verify(exclusive: exclusive)
        errors.failure = nil
        let result = body()
        if let failure = errors.failure {
            let native = failure as NSError
            throw ProofError("AppleEvent failed (\(native.domain), \(native.code)); approve the actual controller context in macOS Automation. No fallback was attempted.")
        }
        try identity.verify(exclusive: exclusive)
        return result
    }

    func machine(_ identifier: String) throws -> UTMScriptingVirtualMachine {
        guard UUID(uuidString: identifier) != nil else { throw ProofError("Expected the exact task VM UUID") }
        let list = try event { (application as UTMScriptingApplication).virtualMachines!() }
        guard let candidate = try event({ list.object(withID: identifier) as? UTMScriptingVirtualMachine }) else {
            throw ProofError("Task VM object is missing from the verified built app")
        }
        let actual = try event { candidate.id!() }
        guard actual == identifier else { throw ProofError("Task VM UUID is not registered in the verified built app") }
        return candidate
    }

    func execute(_ args: [String]) throws -> Int32 {
        guard let command = args.first else { throw ProofError("Missing command") }
        let rest = Array(args.dropFirst())
        if command == "identity" {
            try identity.verify(exclusive: exclusive)
            print("verified")
            return 0
        }
        if command == "open" {
            guard rest.count == 1 else { throw ProofError("open requires the copied task bundle path") }
            _ = try event { (application as UTMScriptingApplication).open!([URL(fileURLWithPath: rest[0])]) }
            return 0
        }
        let identifier: String
        if command == "stop" {
            guard rest.count == 2 && rest[0] == "--force" else { throw ProofError("Only task stop --force UUID is supported") }
            identifier = rest[1]
        } else {
            guard let first = rest.first else { throw ProofError("Missing task VM UUID") }
            identifier = first
        }
        let vm = try machine(identifier)
        switch command {
        case "status":
            let status = try event { vm.status! }
            switch status {
            case .stopped: print("stopped")
            case .started: print("started")
            case .starting: print("starting")
            case .stopping: print("stopping")
            case .paused: print("paused")
            case .pausing: print("pausing")
            case .resuming: print("resuming")
            @unknown default: throw ProofError("Unknown VM state")
            }
        case "start":
            try event { vm.startSaving!(true, recovery: false, provisioning: nil) }
        case "stop":
            try event { vm.stopBy!(.force) }
        case "delete":
            guard try event({ vm.status! }) == .stopped else { throw ProofError("Refusing to delete a task VM that is not stopped") }
            try event { vm.delete!() }
        case "exec":
            guard rest.count >= 4 && rest[1] == "--cmd" else { throw ProofError("exec requires UUID --cmd PROGRAM ARG...") }
            let process = try event {
                vm.executeAt!(rest[2], withArguments: Array(rest.dropFirst(3)), withEnvironment: [],
                              usingInput: "", base64Encoding: true, outputCapturing: true)
            }
            while true {
                let result = try GuestResult.decode(event { process.getResult!() })
                if !result.exited {
                    Thread.sleep(forTimeInterval: 0.2)
                    continue
                }
                guard let code = result.exitCode, let stdout = result.stdout, let stderr = result.stderr else {
                    throw ProofError("Completed guest result lacks validated fields")
                }
                try identity.verify(exclusive: exclusive)
                FileHandle.standardOutput.write(stdout)
                FileHandle.standardError.write(stderr)
                return Int32(code)
            }
        default:
            throw ProofError("Unsupported proof command")
        }
        return 0
    }
}

@main struct UTMProofController {
    static func main() {
        do {
            let args = Array(CommandLine.arguments.dropFirst())
            guard args.count >= 2 else { throw ProofError("Usage: controller APP discover | APP PID LAUNCH_TIME COMMAND ...") }
            let app = URL(fileURLWithPath: args[0]).resolvingSymlinksInPath()
            if args[1] == "discover" {
                let identity = try Identity.discover(app: app)
                let data = try JSONSerialization.data(withJSONObject: ["pid": Int(identity.pid), "launchTime": identity.launchTime, "executable": identity.executable.path], options: [.sortedKeys])
                FileHandle.standardOutput.write(data)
                print("")
                return
            }
            guard args.count >= 4, let pid = Int32(args[1]), pid > 0,
                  let launchTime = Double(args[2]), launchTime.isFinite else { throw ProofError("Invalid pinned process identity") }
            let identity = Identity(pid: pid, launchTime: launchTime,
                                    executable: app.appendingPathComponent("Contents/MacOS/UTM").resolvingSymlinksInPath())
            let controller = try Controller(identity: identity, exclusive: !["status", "stop", "delete"].contains(args[3]))
            exit(try controller.execute(Array(args.dropFirst(3))))
        } catch {
            FileHandle.standardError.write(Data("\(error)\n".utf8))
            exit(1)
        }
    }
}

#endif
