import Darwin
import Foundation
import Virtualization

// Reverse SSH proxy over VSOCK.
//
// The guest pre-connects a pool of idle VSOCK channels to the host (port
// 2222). When a local TCP client connects to the loopback SSH endpoint, the
// daemon takes an idle guest channel, activates it (host sends 0x01, guest
// answers 0x02 once it has dialed sshd), and then bridges the two streams.
// This mirrors the protocol the Go helper previously implemented and the
// guest-side systemd proxy service expects.
enum ProxyProtocol {
  static let hostVSOCKSSHPort: UInt32 = 2222
  static let maxChannels = 32
  static let acquireWaitSeconds = 5.0
  static let activateWaitSeconds = 2.0
  static let activateByte: UInt8 = 0x01
  static let readyByte: UInt8 = 0x02
}

final class GuestChannelPool {
  private let lock = NSCondition()
  private var idle: [Int32] = []
  private var closed = false

  // Mirrors the Go enqueue behavior: keep the newest channels, dropping the
  // oldest idle one when the pool is full.
  func enqueue(_ fd: Int32) {
    lock.lock()
    defer { lock.unlock() }
    if closed {
      close(fd)
      return
    }
    while idle.count >= ProxyProtocol.maxChannels {
      close(idle.removeFirst())
    }
    idle.append(fd)
    lock.signal()
  }

  func acquire(deadline: Date) -> Int32? {
    lock.lock()
    defer { lock.unlock() }
    while !closed {
      if !idle.isEmpty {
        return idle.removeFirst()
      }
      if !lock.wait(until: deadline) {
        return nil
      }
    }
    return nil
  }

  func shutdown() {
    lock.lock()
    defer { lock.unlock() }
    closed = true
    for fd in idle {
      close(fd)
    }
    idle.removeAll()
    lock.broadcast()
  }
}

final class VSOCKListenerDelegate: NSObject, VZVirtioSocketListenerDelegate {
  private let pool: GuestChannelPool

  init(pool: GuestChannelPool) {
    self.pool = pool
  }

  func listener(
    _ listener: VZVirtioSocketListener,
    shouldAcceptNewConnection connection: VZVirtioSocketConnection,
    from socketDevice: VZVirtioSocketDevice
  ) -> Bool {
    // Duplicate the descriptor so the channel outlives the connection object.
    let fd = dup(connection.fileDescriptor)
    connection.close()
    if fd < 0 {
      return false
    }
    pool.enqueue(fd)
    return true
  }
}

final class LocalSSHProxy {
  private let listenFD: Int32
  let port: Int
  private let pool: GuestChannelPool
  private let active = DispatchSemaphore(value: ProxyProtocol.maxChannels)
  private let onFatalError: (String) -> Void

  init(pool: GuestChannelPool, onFatalError: @escaping (String) -> Void) throws {
    self.pool = pool
    self.onFatalError = onFatalError
    (listenFD, port) = try LocalSSHProxy.openLoopbackListener()
  }

  private static func openLoopbackListener() throws -> (Int32, Int) {
    let fd = socket(AF_INET, SOCK_STREAM, 0)
    if fd < 0 {
      throw VMDError.errno("open local ssh proxy socket")
    }
    var address = sockaddr_in()
    address.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
    address.sin_family = sa_family_t(AF_INET)
    address.sin_port = 0
    address.sin_addr.s_addr = inet_addr("127.0.0.1")
    let bindResult = withUnsafePointer(to: &address) { pointer in
      pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) { sa in
        bind(fd, sa, socklen_t(MemoryLayout<sockaddr_in>.size))
      }
    }
    if bindResult != 0 {
      close(fd)
      throw VMDError.errno("bind local ssh proxy")
    }
    if listen(fd, 64) != 0 {
      close(fd)
      throw VMDError.errno("listen local ssh proxy")
    }
    var bound = sockaddr_in()
    var length = socklen_t(MemoryLayout<sockaddr_in>.size)
    let nameResult = withUnsafeMutablePointer(to: &bound) { pointer in
      pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) { sa in
        getsockname(fd, sa, &length)
      }
    }
    if nameResult != 0 {
      close(fd)
      throw VMDError.errno("resolve local ssh proxy port")
    }
    return (fd, Int(UInt16(bigEndian: bound.sin_port)))
  }

  func start() {
    let thread = Thread { [self] in
      acceptLoop()
    }
    thread.name = "ssh-proxy-accept"
    thread.start()
  }

  func stop() {
    close(listenFD)
    pool.shutdown()
  }

  private func acceptLoop() {
    while true {
      let clientFD = accept(listenFD, nil, nil)
      if clientFD < 0 {
        if Darwin.errno == EINTR { continue }
        onFatalError("accept local ssh connection: \(String(cString: strerror(Darwin.errno)))")
        return
      }
      if active.wait(timeout: .now()) != .success {
        close(clientFD)
        continue
      }
      guard let guestFD = acquireActivatedChannel() else {
        active.signal()
        close(clientFD)
        continue
      }
      let thread = Thread { [self] in
        bridge(clientFD, guestFD)
        active.signal()
      }
      thread.name = "ssh-proxy-bridge"
      thread.start()
    }
  }

  private func acquireActivatedChannel() -> Int32? {
    let deadline = Date().addingTimeInterval(ProxyProtocol.acquireWaitSeconds)
    while let fd = pool.acquire(deadline: deadline) {
      if activateChannel(fd) {
        return fd
      }
      close(fd)
    }
    return nil
  }

  private func activateChannel(_ fd: Int32) -> Bool {
    guard setChannelTimeout(fd, seconds: ProxyProtocol.activateWaitSeconds) else { return false }
    var activate = ProxyProtocol.activateByte
    if write(fd, &activate, 1) != 1 {
      return false
    }
    var ready: UInt8 = 0
    if read(fd, &ready, 1) != 1 || ready != ProxyProtocol.readyByte {
      return false
    }
    return setChannelTimeout(fd, seconds: 0)
  }

  private func setChannelTimeout(_ fd: Int32, seconds: Double) -> Bool {
    var timeout = timeval(
      tv_sec: Int(seconds),
      tv_usec: Int32((seconds - Double(Int(seconds))) * 1_000_000))
    let size = socklen_t(MemoryLayout<timeval>.size)
    return setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, size) == 0
      && setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, size) == 0
  }

  private func bridge(_ left: Int32, _ right: Int32) {
    let group = DispatchGroup()
    for (source, target) in [(left, right), (right, left)] {
      group.enter()
      let thread = Thread {
        pump(from: source, to: target)
        // Unblock the opposite pump the way the Go helper's deadline trick
        // did: once one direction ends, tear down the destination socket.
        shutdown(target, SHUT_RDWR)
        group.leave()
      }
      thread.name = "ssh-proxy-pump"
      thread.start()
    }
    group.wait()
    close(left)
    close(right)
  }
}

private func pump(from source: Int32, to target: Int32) {
  var buffer = [UInt8](repeating: 0, count: 32 * 1024)
  while true {
    let n = read(source, &buffer, buffer.count)
    if n == 0 { return }
    if n < 0 {
      if Darwin.errno == EINTR { continue }
      return
    }
    var offset = 0
    while offset < n {
      let written = buffer.withUnsafeBytes { raw in
        write(target, raw.baseAddress!.advanced(by: offset), n - offset)
      }
      if written < 0 {
        if Darwin.errno == EINTR { continue }
        return
      }
      offset += written
    }
  }
}
