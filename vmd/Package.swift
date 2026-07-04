// swift-tools-version:6.0
import PackageDescription

let package = Package(
  name: "crabbox-apple-vm-vmd",
  platforms: [
    .macOS(.v13)
  ],
  targets: [
    .executableTarget(
      name: "crabbox-apple-vm-vmd",
      path: "Sources/vmd",
      swiftSettings: [
        .swiftLanguageMode(.v5)
      ]
    )
  ]
)
