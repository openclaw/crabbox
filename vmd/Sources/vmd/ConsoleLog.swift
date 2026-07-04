import Darwin
import Foundation

// Bounded console log sink: the VM's serial output is piped through the
// daemon and copied into console.log until the byte budget is exhausted,
// after which a truncation marker is written once and further output is
// discarded. Mirrors the Go helper's previous behavior byte for byte.
final class ConsoleLogSink {
  static let maxBytes = 8 << 20
  static let truncatedMarker = "\n[crabbox: console log truncated at 8 MiB]\n"

  let writeHandle: FileHandle
  private let readFD: Int32
  private let thread: Thread

  init(path: String) throws {
    let logFD = open(path, O_WRONLY | O_CREAT | O_TRUNC, 0o600)
    if logFD < 0 {
      throw VMDError.errno("open console log")
    }
    if fchmod(logFD, 0o600) != 0 {
      close(logFD)
      throw VMDError.errno("secure console log")
    }
    var fds: [Int32] = [0, 0]
    if pipe(&fds) != 0 {
      close(logFD)
      throw VMDError.errno("create console log pipe")
    }
    readFD = fds[0]
    writeHandle = FileHandle(fileDescriptor: fds[1], closeOnDealloc: true)
    let source = readFD
    thread = Thread {
      ConsoleLogSink.copyBounded(from: source, to: logFD)
      close(source)
      close(logFD)
    }
    thread.name = "console-log"
    thread.start()
  }

  func closeWriteSide() {
    try? writeHandle.close()
  }

  private static func copyBounded(from source: Int32, to target: Int32) {
    let marker = Array(truncatedMarker.utf8)
    var remaining = maxBytes - marker.count
    var sealed = false
    var buffer = [UInt8](repeating: 0, count: 64 * 1024)
    while true {
      let n = read(source, &buffer, buffer.count)
      if n == 0 { return }
      if n < 0 {
        if Darwin.errno == EINTR { continue }
        return
      }
      if sealed { continue }
      let writeBytes = min(n, remaining)
      if writeBytes > 0 {
        if !writeFully(target, buffer, writeBytes) {
          sealed = true
          continue
        }
        remaining -= writeBytes
      }
      if writeBytes < n {
        _ = writeFully(target, marker, marker.count)
        sealed = true
      }
    }
  }

  private static func writeFully(_ fd: Int32, _ bytes: [UInt8], _ count: Int) -> Bool {
    var offset = 0
    while offset < count {
      let n = bytes.withUnsafeBytes { raw in
        write(fd, raw.baseAddress!.advanced(by: offset), count - offset)
      }
      if n < 0 {
        if Darwin.errno == EINTR { continue }
        return false
      }
      offset += n
    }
    return true
  }
}
