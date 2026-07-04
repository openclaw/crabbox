import Darwin
import Foundation
import Virtualization

// Probe validates that this host can build and instantiate a Linux VM
// configuration (including the virtualization entitlement) without booting
// anything. It mirrors the runtime checks the Go helper previously ran
// through the vz library.
func runProbe() throws {
  let tmpDir = NSTemporaryDirectory() + "crabbox-apple-vm-probe-" + UUID().uuidString
  try FileManager.default.createDirectory(
    atPath: tmpDir, withIntermediateDirectories: true,
    attributes: [.posixPermissions: 0o700])
  defer { try? FileManager.default.removeItem(atPath: tmpDir) }

  let diskPath = tmpDir + "/disk.raw"
  let seedPath = tmpDir + "/seed.raw"
  try createEmptyDiskImage(path: diskPath, bytes: 64 * 1024 * 1024)
  try createEmptyDiskImage(path: seedPath, bytes: 8 * 1024 * 1024)

  let spec = VMSpec(
    cpus: 2,
    memoryMiB: 2048,
    diskPath: diskPath,
    seedPath: seedPath,
    efiVariableStorePath: tmpDir + "/efi.bin",
    consoleLogPath: tmpDir + "/console.log")
  let built = try buildVMConfiguration(spec)
  defer { built.consoleSink?.closeWriteSide() }

  let queue = DispatchQueue(label: "crabbox.apple-vm.probe")
  try queue.sync {
    let vm = VZVirtualMachine(configuration: built.configuration, queue: queue)
    if vm.state != .stopped {
      throw VMDError("probe vm reported unexpected initial state \(vm.state.rawValue)")
    }
  }
}

private func createEmptyDiskImage(path: String, bytes: Int64) throws {
  let fd = open(path, O_WRONLY | O_CREAT | O_EXCL, 0o600)
  if fd < 0 {
    throw VMDError.errno("create disk image \(path)")
  }
  defer { close(fd) }
  if ftruncate(fd, bytes) != 0 {
    throw VMDError.errno("size disk image \(path)")
  }
}
