import Darwin
import Foundation

// crabbox-apple-vm-vmd is the privileged Virtualization.framework daemon for
// the apple-vm provider. The Go helper (crabbox-apple-vm-helper) prepares all
// instance assets and spawns this binary to host the VM ("serve") or to
// validate the runtime ("probe"). It must be codesigned with the
// com.apple.security.virtualization entitlement; the helper installs and
// signs a managed copy automatically.

func usage() -> Never {
  logLine("usage: crabbox-apple-vm-vmd <serve|probe|version> [flags]")
  exit(2)
}

func parseFlags(_ arguments: [String]) throws -> [String: String] {
  var flags: [String: String] = [:]
  var index = 0
  while index < arguments.count {
    let argument = arguments[index]
    guard argument.hasPrefix("--") else {
      throw VMDError("unexpected argument \(argument)")
    }
    let key = String(argument.dropFirst(2))
    guard index + 1 < arguments.count else {
      throw VMDError("flag --\(key) requires a value")
    }
    flags[key] = arguments[index + 1]
    index += 2
  }
  return flags
}

let arguments = Array(CommandLine.arguments.dropFirst())
guard let subcommand = arguments.first else {
  usage()
}

do {
  switch subcommand {
  case "serve":
    let flags = try parseFlags(Array(arguments.dropFirst()))
    let stateRoot = try normalizeStateRoot(flags["state-root"] ?? "")
    let name = flags["name"] ?? ""
    try validateInstanceName(name)
    if let rawFD = flags["startup-fd"] {
      guard let fd = Int32(rawFD), fd >= 0 else {
        throw VMDError("invalid startup fd \(rawFD)")
      }
      try waitForStartupAuthorization(fd: fd)
    }
    let command = try ServeCommand(
      stateRoot: stateRoot, name: name.trimmingCharacters(in: .whitespacesAndNewlines))
    command.run()
  case "probe":
    try runProbe()
    exit(0)
  case "version":
    print("crabbox-apple-vm-vmd")
    exit(0)
  default:
    usage()
  }
} catch {
  logLine("crabbox-apple-vm-vmd \(subcommand): \(error)")
  exit(1)
}
