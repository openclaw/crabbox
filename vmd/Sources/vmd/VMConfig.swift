import Darwin
import Foundation
import Virtualization

struct VMSpec {
  var cpus: Int
  var memoryMiB: Int
  var diskPath: String
  var seedPath: String
  var efiVariableStorePath: String
  var consoleLogPath: String
}

struct BuiltVMConfiguration {
  let configuration: VZVirtualMachineConfiguration
  let consoleSink: ConsoleLogSink?
}

func buildVMConfiguration(_ spec: VMSpec) throws -> BuiltVMConfiguration {
  let efiStore: VZEFIVariableStore
  do {
    efiStore = try VZEFIVariableStore(
      creatingVariableStoreAt: URL(fileURLWithPath: spec.efiVariableStorePath),
      options: [.allowOverwrite])
  } catch {
    throw VMDError("create EFI variable store: \(error.localizedDescription)")
  }
  if chmod(spec.efiVariableStorePath, 0o600) != 0 {
    throw VMDError.errno("secure EFI variable store")
  }
  let bootLoader = VZEFIBootLoader()
  bootLoader.variableStore = efiStore

  let configuration = VZVirtualMachineConfiguration()
  configuration.bootLoader = bootLoader
  configuration.cpuCount = spec.cpus
  configuration.memorySize = UInt64(spec.memoryMiB) * 1024 * 1024
  configuration.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]

  let network = VZVirtioNetworkDeviceConfiguration()
  network.attachment = VZNATNetworkDeviceAttachment()
  configuration.networkDevices = [network]

  configuration.socketDevices = [VZVirtioSocketDeviceConfiguration()]

  let rootAttachment: VZDiskImageStorageDeviceAttachment
  do {
    rootAttachment = try VZDiskImageStorageDeviceAttachment(
      url: URL(fileURLWithPath: spec.diskPath), readOnly: false)
  } catch {
    throw VMDError("open root disk: \(error.localizedDescription)")
  }
  let seedAttachment: VZDiskImageStorageDeviceAttachment
  do {
    seedAttachment = try VZDiskImageStorageDeviceAttachment(
      url: URL(fileURLWithPath: spec.seedPath), readOnly: true)
  } catch {
    throw VMDError("open seed disk: \(error.localizedDescription)")
  }
  configuration.storageDevices = [
    VZVirtioBlockDeviceConfiguration(attachment: rootAttachment),
    VZVirtioBlockDeviceConfiguration(attachment: seedAttachment),
  ]

  var consoleSink: ConsoleLogSink?
  if !spec.consoleLogPath.isEmpty {
    let sink = try ConsoleLogSink(path: spec.consoleLogPath)
    consoleSink = sink
    let console = VZVirtioConsoleDeviceSerialPortConfiguration()
    console.attachment = VZFileHandleSerialPortAttachment(
      fileHandleForReading: FileHandle(forReadingAtPath: "/dev/null"),
      fileHandleForWriting: sink.writeHandle)
    configuration.serialPorts = [console]
  }

  do {
    try configuration.validate()
  } catch {
    consoleSink?.closeWriteSide()
    throw VMDError("validate vm config: \(error.localizedDescription)")
  }
  return BuiltVMConfiguration(configuration: configuration, consoleSink: consoleSink)
}
