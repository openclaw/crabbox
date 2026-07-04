import Darwin
import Foundation
import Virtualization

private let startupAuthorizationByte: UInt8 = 0xA5

func logLine(_ message: String) {
  FileHandle.standardError.write(Data((message + "\n").utf8))
}

// Blocks until the parent helper writes the authorization byte on the
// inherited descriptor, proving the daemon's identity was recorded before
// the VM starts.
func waitForStartupAuthorization(fd: Int32) throws {
  var marker: UInt8 = 0
  while true {
    let n = read(fd, &marker, 1)
    if n == 1 { break }
    if n < 0 && Darwin.errno == EINTR { continue }
    throw VMDError("startup authorization closed before approval")
  }
  close(fd)
  if marker != startupAuthorizationByte {
    throw VMDError("invalid startup authorization marker")
  }
}

final class ServeCommand {
  private let stateRoot: String
  private let name: String
  private var metadata: InstanceMetadata
  private let vmQueue = DispatchQueue(label: "crabbox.apple-vm.vmd")
  private var vm: VZVirtualMachine!
  private var stateObservation: NSKeyValueObservation?
  private var vsockListener: VZVirtioSocketListener?
  private var vsockDelegate: VSOCKListenerDelegate?
  private var proxy: LocalSSHProxy?
  private var consoleSink: ConsoleLogSink?
  private var signalSources: [DispatchSourceSignal] = []
  private var finished = false

  init(stateRoot: String, name: String) throws {
    self.stateRoot = stateRoot
    self.name = name
    metadata = try InstanceMetadata(
      path: StateLayout.metadataPath(stateRoot: stateRoot, name: name))
  }

  func run() -> Never {
    do {
      try start()
    } catch {
      fail("\(error)")
    }
    dispatchMain()
  }

  private func start() throws {
    metadata.set("pid", Int(getpid()))
    metadata.set("pidStartedAt", try processStartIdentity(pid: getpid()))
    metadata.setStatus("starting")
    try metadata.write()

    let spec = VMSpec(
      cpus: metadata.int("cpus"),
      memoryMiB: metadata.int("memoryMiB"),
      diskPath: metadata.string("diskPath"),
      seedPath: metadata.string("seedPath"),
      efiVariableStorePath: metadata.string("efiVariableStorePath"),
      consoleLogPath: metadata.string("consoleLogPath"))
    let built = try buildVMConfiguration(spec)
    consoleSink = built.consoleSink

    let pool = GuestChannelPool()
    let proxy = try LocalSSHProxy(pool: pool) { [weak self] message in
      self?.vmQueue.async {
        self?.fail("local SSH proxy failed: \(message)", requestVMStop: true)
      }
    }
    self.proxy = proxy

    try vmQueue.sync {
      vm = VZVirtualMachine(configuration: built.configuration, queue: vmQueue)
      guard let socketDevice = vm.socketDevices.first as? VZVirtioSocketDevice else {
        throw VMDError("vm socket device unavailable")
      }
      let delegate = VSOCKListenerDelegate(pool: pool)
      let listener = VZVirtioSocketListener()
      listener.delegate = delegate
      vsockDelegate = delegate
      vsockListener = listener
      socketDevice.setSocketListener(listener, forPort: ProxyProtocol.hostVSOCKSSHPort)
    }

    metadata.set("sshHost", "127.0.0.1")
    metadata.set("sshPort", proxy.port)
    try metadata.write()
    proxy.start()

    installSignalHandlers()

    vmQueue.async { [self] in
      observeState()
      vm.start { [self] result in
        switch result {
        case .success:
          setStatus("running")
        case .failure(let error):
          fail("start virtual machine: \(error.localizedDescription)", requestVMStop: false)
        }
      }
    }
  }

  private func installSignalHandlers() {
    for signalNumber in [SIGINT, SIGTERM] {
      signal(signalNumber, SIG_IGN)
      let source = DispatchSource.makeSignalSource(signal: signalNumber, queue: vmQueue)
      source.setEventHandler { [self] in
        logLine("apple-vm daemon received signal \(signalNumber) for \(name)")
        setStatus("stopping")
        requestVMStop()
      }
      source.resume()
      signalSources.append(source)
    }
  }

  // Runs on vmQueue.
  private func observeState() {
    stateObservation = vm.observe(\.state) { [weak self] vm, _ in
      self?.handleState(vm.state)
    }
  }

  // Runs on vmQueue.
  private func handleState(_ state: VZVirtualMachine.State) {
    if finished { return }
    switch state {
    case .running:
      setStatus("running")
    case .stopping:
      setStatus("stopping")
    case .stopped:
      setStatus("stopped")
      finish(exitCode: 0)
    case .error:
      fail("vm entered error state", requestVMStop: true)
    default:
      logLine("apple-vm daemon state=\(state.rawValue) name=\(name)")
    }
  }

  // Runs on vmQueue.
  private func requestVMStop() {
    guard let vm else { return }
    if vm.canRequestStop {
      if (try? vm.requestStop()) != nil {
        return
      }
    }
    if vm.canStop {
      vm.stop { error in
        if let error {
          logLine("apple-vm daemon stop failed: \(error.localizedDescription)")
        }
      }
    }
  }

  private func setStatus(_ status: String, error: String = "") {
    metadata.setStatus(status, error: error)
    do {
      try metadata.write()
    } catch {
      logLine("apple-vm daemon metadata write failed: \(error)")
    }
  }

  private func fail(_ message: String, requestVMStop stopVM: Bool = false) {
    // finish() closes the proxy listener, which surfaces as an accept error;
    // never let that teardown echo overwrite a terminal status.
    if finished { return }
    logLine("apple-vm daemon error: \(message)")
    setStatus("error", error: message)
    if stopVM {
      requestVMStop()
    }
    finish(exitCode: 1)
  }

  private func finish(exitCode: Int32) {
    if finished { return }
    finished = true
    proxy?.stop()
    consoleSink?.closeWriteSide()
    // Give the console sink a moment to drain buffered output.
    DispatchQueue.global().asyncAfter(deadline: .now() + 0.2) {
      exit(exitCode)
    }
  }
}
