import Foundation

public struct MapReduceShard: Equatable, Sendable {
    public let sandboxID: String
    public let lowerBound: Int
    public let upperBound: Int

    public init(sandboxID: String, lowerBound: Int, upperBound: Int) {
        self.sandboxID = sandboxID
        self.lowerBound = lowerBound
        self.upperBound = upperBound
    }
}

public struct MapReduceMapResult: Equatable, Sendable {
    public let sandboxID: String
    public let sum: Int
    public let min: Int
    public let max: Int

    public init(sandboxID: String, sum: Int, min: Int, max: Int) {
        self.sandboxID = sandboxID
        self.sum = sum
        self.min = min
        self.max = max
    }
}

public struct MapReduceSummary: Equatable, Sendable {
    public let total: Int
    public let min: Int?
    public let max: Int?
    public let expected: Int
    public let resultCount: Int

    public var ok: Bool {
        total == expected && min == 1 && max == expectedRangeMax
    }

    private let expectedRangeMax: Int

    public init(total: Int, min: Int?, max: Int?, expected: Int, expectedRangeMax: Int, resultCount: Int) {
        self.total = total
        self.min = min
        self.max = max
        self.expected = expected
        self.expectedRangeMax = expectedRangeMax
        self.resultCount = resultCount
    }
}

public func planMapReduceShards(total: Int, sandboxIDs: [String]) -> [MapReduceShard] {
    let ids = sandboxIDs
        .map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }
        .filter { !$0.isEmpty }
    guard total > 0, !ids.isEmpty else { return [] }

    let selected = Array(ids.prefix(total))
    let count = selected.count
    return selected.enumerated().map { index, sandboxID in
        let per = total / count
        let lower = index * per + 1
        let upper = index == count - 1 ? total : (index + 1) * per
        return MapReduceShard(sandboxID: sandboxID, lowerBound: lower, upperBound: upper)
    }
}

public func parseMapReduceOutput(_ output: String, sandboxID: String) -> MapReduceMapResult? {
    let values = output
        .split(whereSeparator: { $0 == " " || $0 == "\n" || $0 == "\t" })
        .compactMap { Int($0) }
    guard values.count == 3 else { return nil }
    return MapReduceMapResult(sandboxID: sandboxID, sum: values[0], min: values[1], max: values[2])
}

public func reduceMapResults(total: Int, results: [MapReduceMapResult]) -> MapReduceSummary {
    let expected = total * (total + 1) / 2
    return MapReduceSummary(
        total: results.reduce(0) { $0 + $1.sum },
        min: results.map(\.min).min(),
        max: results.map(\.max).max(),
        expected: expected,
        expectedRangeMax: total,
        resultCount: results.count
    )
}
