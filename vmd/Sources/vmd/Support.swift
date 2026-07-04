import Darwin
import Foundation

struct VMDError: Error, CustomStringConvertible {
  let description: String

  init(_ message: String) {
    description = message
  }

  static func errno(_ label: String) -> VMDError {
    VMDError("\(label): \(String(cString: strerror(Darwin.errno)))")
  }
}

enum StateLayout {
  static let metadataFileName = "instance.json"
  static let consoleLogFileName = "console.log"

  static func instanceDir(stateRoot: String, name: String) -> String {
    (stateRoot as NSString).appendingPathComponent("instances/\(name)")
  }

  static func metadataPath(stateRoot: String, name: String) -> String {
    (instanceDir(stateRoot: stateRoot, name: name) as NSString)
      .appendingPathComponent(metadataFileName)
  }
}

func validateInstanceName(_ name: String) throws {
  let trimmed = name.trimmingCharacters(in: .whitespacesAndNewlines)
  if trimmed.isEmpty {
    throw VMDError("instance name is required")
  }
  if trimmed == "." || trimmed == ".." || trimmed.contains("/") {
    throw VMDError("invalid instance name \"\(name)\"")
  }
}

func normalizeStateRoot(_ root: String) throws -> String {
  let trimmed = root.trimmingCharacters(in: .whitespacesAndNewlines)
  if trimmed.isEmpty {
    throw VMDError("state root is required")
  }
  var absolute = trimmed
  if !absolute.hasPrefix("/") {
    absolute = FileManager.default.currentDirectoryPath + "/" + absolute
  }
  var isDirectory: ObjCBool = false
  if !FileManager.default.fileExists(atPath: absolute, isDirectory: &isDirectory)
    || !isDirectory.boolValue
  {
    throw VMDError("state root \(absolute) does not exist")
  }
  return absolute
}

// Matches the Go helper's readProcessStartTime: "darwin-kinfo:<sec>.<usec06>".
func processStartIdentity(pid: pid_t) throws -> String {
  var info = kinfo_proc()
  var size = MemoryLayout<kinfo_proc>.stride
  var mib: [Int32] = [CTL_KERN, KERN_PROC, KERN_PROC_PID, pid]
  if sysctl(&mib, UInt32(mib.count), &info, &size, nil, 0) != 0 {
    throw VMDError.errno("read process identity")
  }
  guard info.kp_proc.p_pid == pid else {
    throw VMDError("process \(pid) identity unavailable")
  }
  let started = info.kp_proc.p_starttime
  guard started.tv_sec > 0, started.tv_usec >= 0 else {
    throw VMDError("process \(pid) start time unavailable")
  }
  return "darwin-kinfo:\(started.tv_sec)." + String(format: "%06d", started.tv_usec)
}

// Matches Go time.Time RFC3339Nano encoding closely enough for the Go reader:
// UTC with microsecond precision and a trailing "Z".
func rfc3339UTCNow() -> String {
  let formatter = DateFormatter()
  formatter.locale = Locale(identifier: "en_US_POSIX")
  formatter.timeZone = TimeZone(identifier: "UTC")
  formatter.dateFormat = "yyyy-MM-dd'T'HH:mm:ss.SSSSSS'Z'"
  return formatter.string(from: Date())
}

func writeFileAtomically(path: String, data: Data, mode: mode_t) throws {
  let tmp = path + ".tmp"
  let fd = open(tmp, O_WRONLY | O_CREAT | O_TRUNC, mode)
  if fd < 0 {
    throw VMDError.errno("create \(tmp)")
  }
  defer { close(fd) }
  if fchmod(fd, mode) != 0 {
    throw VMDError.errno("chmod \(tmp)")
  }
  var written = 0
  try data.withUnsafeBytes { (raw: UnsafeRawBufferPointer) in
    while written < raw.count {
      let n = write(fd, raw.baseAddress!.advanced(by: written), raw.count - written)
      if n < 0 {
        if Darwin.errno == EINTR { continue }
        throw VMDError.errno("write \(tmp)")
      }
      written += n
    }
  }
  if rename(tmp, path) != 0 {
    throw VMDError.errno("commit \(path)")
  }
}
