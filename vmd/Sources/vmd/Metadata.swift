import Foundation

// Instance metadata mirrors the Go helper's Instance struct. The daemon only
// mutates the keys it owns and passes every other key through untouched so a
// newer Go helper can add fields without the daemon erasing them.
struct InstanceMetadata {
  private(set) var values: [String: Any]
  let path: String

  init(path: String) throws {
    self.path = path
    let data = try Data(contentsOf: URL(fileURLWithPath: path))
    guard let object = try JSONSerialization.jsonObject(with: data) as? [String: Any] else {
      throw VMDError("decode metadata \(path): not a JSON object")
    }
    values = object
  }

  func string(_ key: String) -> String {
    (values[key] as? String) ?? ""
  }

  func int(_ key: String) -> Int {
    if let value = values[key] as? Int { return value }
    if let value = values[key] as? NSNumber { return value.intValue }
    return 0
  }

  mutating func set(_ key: String, _ value: String) {
    // Go's omitempty drops empty strings; mirror that so files stay identical.
    if value.isEmpty && key != "status" {
      values.removeValue(forKey: key)
    } else {
      values[key] = value
    }
  }

  mutating func set(_ key: String, _ value: Int) {
    if value == 0 {
      values.removeValue(forKey: key)
    } else {
      values[key] = value
    }
  }

  mutating func setStatus(_ status: String, error: String = "") {
    set("status", status)
    set("error", error)
  }

  mutating func write() throws {
    values["updatedAt"] = rfc3339UTCNow()
    var data = try JSONSerialization.data(
      withJSONObject: values, options: [.prettyPrinted, .sortedKeys])
    data.append(0x0A)
    try writeFileAtomically(path: path, data: data, mode: 0o600)
  }
}
