import Foundation
import Virtualization

// This helper only inspects a local restore image and validates account options.
// It never creates, installs, starts, or enumerates a virtual machine.
@main struct MacOSPreflight {
    static func output(_ value: [String: Any]) {
        if let data = try? JSONSerialization.data(withJSONObject: value, options: [.sortedKeys]) {
            print(String(decoding: data, as: UTF8.self))
        }
    }
    static func main() async {
        #if arch(arm64)
        guard #available(macOS 27, *) else { output(["error": "host-version"]); return }
        guard CommandLine.arguments.count == 3 else { output(["error": "input"]); return }
        guard VZVirtualMachine.isSupported else { output(["error": "virtualization-unavailable"]); return }
        guard let data = try? FileHandle.standardInput.readToEnd(),
              let password = String(data: data, encoding: .utf8) else { output(["error": "input"]); return }
        let account = VZMacGuestProvisioningOptions()
        account.username = CommandLine.arguments[2]
        account.fullName = CommandLine.arguments[2]
        account.password = password
        account.logsInAutomatically = false
        account.enablesRemoteLogin = true
        do { try account.validate() } catch {
            // SDK errors can contain credential-bearing values. Return a closed category.
            output(["error": "account-invalid"]); return
        }
        do {
            let image = try await VZMacOSRestoreImage.image(from: URL(fileURLWithPath: CommandLine.arguments[1]))
            let guest = image.operatingSystemVersion
            let host = ProcessInfo.processInfo.operatingSystemVersion
            guard guest.majorVersion >= 27 else { output(["error": "guest-version"]); return }
            guard ![host.majorVersion, host.minorVersion, host.patchVersion].lexicographicallyPrecedes([guest.majorVersion, guest.minorVersion, guest.patchVersion]) else { output(["error": "guest-newer-than-host"]); return }
            guard image.isSupported, let requirements = image.mostFeaturefulSupportedConfiguration,
                  requirements.hardwareModel.isSupported else { output(["error": "image-incompatible"]); return }
            let cpus = max(2, requirements.minimumSupportedCPUCount)
            let memory = max(UInt64(8 * 1024 * 1024 * 1024), requirements.minimumSupportedMemorySize)
            let mib = (memory + 1048575) / 1048576
            guard cpus <= VZVirtualMachineConfiguration.maximumAllowedCPUCount,
                  mib * 1048576 <= VZVirtualMachineConfiguration.maximumAllowedMemorySize else { output(["error": "resources-unavailable"]); return }
            output(["guestVersion": "\(guest.majorVersion).\(guest.minorVersion).\(guest.patchVersion)",
                    "guestBuild": image.buildVersion,
                    "hostVersion": "\(host.majorVersion).\(host.minorVersion).\(host.patchVersion)",
                    "cpuCount": cpus, "memoryMiB": mib,
                    "supported": true, "accountValidated": true])
        } catch { output(["error": "image-unreadable"]); return }
        #else
        output(["error": "host-architecture"])
        #endif
    }
}
