// Pure identity records only: this executable makes no application or AppleEvent calls.
import Foundation

@main struct IdentityChecks {
    static func rejected(_ body: () throws -> Void) {
        do {
            try body()
            fatalError("An invalid process identity was accepted")
        } catch { /* Expected fail-closed result. */ }
    }

    static func main() throws {
        let app = URL(fileURLWithPath: "/tmp/fixture-built/UTM.app")
        let executable = app.appendingPathComponent("Contents/MacOS/UTM").resolvingSymlinksInPath()
        let installed = URL(fileURLWithPath: "/Applications/UTM.app/Contents/MacOS/UTM")
        func record(_ pid: pid_t = 1234, _ time: Double? = 100.25,
                    _ path: URL? = executable, _ terminated: Bool = false) -> ProcessIdentity {
            ProcessIdentity(pid: pid, launchTime: time, executable: path,
                            bundleIdentifier: "com.utmapp.UTM", terminated: terminated)
        }
        let identity = try Identity.discover(app: app, processes: [record()])
        precondition(identity.pid == 1234 && identity.launchTime == 100.25)
        try identity.verify(processes: [record()])
        rejected { try identity.verify(processes: []) }
        rejected { try identity.verify(processes: [record(1234, 100.25, executable, true)]) }
        rejected { try identity.verify(processes: [record(1234, 100.25, installed)]) }
        rejected { try identity.verify(processes: [record(1234, 999.0)]) }
        rejected { try identity.verify(processes: [record(9999)]) }
        rejected { try identity.verify(processes: [record(1234, nil)]) }
        rejected { _ = try Identity.discover(app: app, processes: [record(1234, 100.25, installed)]) }
        rejected { _ = try Identity.discover(app: app, processes: [record(), record(5678)]) }
        let competing = [record(), record(5678, 200.0, installed)]
        rejected { try identity.verify(processes: competing) }
        // Cleanup may address the same verified built PID despite a competing copy;
        // it must still reject a reused PID, changed launch time or wrong executable.
        try identity.verify(processes: competing, exclusive: false)
        rejected { try identity.verify(processes: [record(1234, 100.25, installed)], exclusive: false) }
        rejected { try identity.verify(processes: [record(1234, 999.0)], exclusive: false) }
        // Exact observed PID-bound ScriptingBridge shape from an own-nonce exec.
        let output = "CRABBOX_TEST_READY\nLinux\n"
        let actual: [AnyHashable: Any] = ["exited": NSNumber(value: true), "exitCode": NSNumber(value: 0),
            "signalCode": NSNumber(value: 0), "outputData": Data(output.utf8).base64EncodedString(),
            "outputText": output, "errorData": "", "errorText": ""]
        let decoded = try GuestResult.decode(actual)
        precondition(decoded.exited && decoded.exitCode == 0 && decoded.stdout == Data(output.utf8) && decoded.stderr == Data())
        var alias = actual
        alias["hasExited"] = alias.removeValue(forKey: "exited")
        let aliasResult = try GuestResult.decode(alias)
        precondition(aliasResult.exited)
        let pending = try GuestResult.decode(["exited": NSNumber(value: false)])
        precondition(!pending.exited && pending.exitCode == nil)
        func invalid(_ key: String, _ value: Any?) {
            var changed = actual
            changed[key] = value
            rejected { _ = try GuestResult.decode(changed) }
        }
        invalid("exited", nil)
        invalid("exited", NSNumber(value: 1))
        invalid("exited", "true")
        invalid("hasExited", NSNumber(value: false))
        invalid("exitCode", nil)
        invalid("exitCode", NSNumber(value: true))
        invalid("exitCode", NSNumber(value: 0.5))
        invalid("exitCode", NSNumber(value: 256))
        invalid("signalCode", nil)
        invalid("signalCode", NSNumber(value: 15))
        invalid("outputData", nil)
        invalid("outputData", "%%% invalid base64 %%%")
        invalid("errorData", nil)
        invalid("errorData", NSNumber(value: 0))
        print("invalid process identities rejected; exact PID cleanup identity preserved; observed guest result shape validated")
    }
}
