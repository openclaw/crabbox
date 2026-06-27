import XCTest
@testable import CrabboxKit

final class WorkspaceClientTests: XCTestCase {
    func testWorkspaceIDValidationMatchesCoordinatorShape() {
        XCTAssertTrue(isValidWorkspaceID("ios-run-123"))
        XCTAssertTrue(isValidWorkspaceID("a"))
        XCTAssertFalse(isValidWorkspaceID(""))
        XCTAssertFalse(isValidWorkspaceID("-bad"))
        XCTAssertFalse(isValidWorkspaceID("bad-"))
        XCTAssertFalse(isValidWorkspaceID("Bad"))
        XCTAssertFalse(isValidWorkspaceID(String(repeating: "a", count: 64)))
    }

    func testWorkspaceCommandPrintsExitMarker() {
        let wrapped = crabboxWorkspaceCommand("echo hi")
        XCTAssertTrue(wrapped.contains("echo hi"))
        XCTAssertTrue(wrapped.contains(crabboxWorkspaceExitMarker))
        XCTAssertTrue(wrapped.contains("exit \"$status\""))
    }

    func testParsesCrabboxCommandLineAndDropsBinaryName() throws {
        let args = try parseCrabboxCommandLine("crabbox run --provider islo --no-sync -- sh -c 'echo hi'")
        XCTAssertEqual(args, ["run", "--provider", "islo", "--no-sync", "--", "sh", "-c", "echo hi"])
    }

    func testParserRejectsBrokenQuotes() {
        XCTAssertThrowsError(try parseCrabboxCommandLine("crabbox run 'oops")) { error in
            XCTAssertEqual(error as? CrabboxCommandLineError, .unterminatedQuote("'"))
        }
    }

    func testDetectsIsloProviderCommands() throws {
        XCTAssertTrue(commandLineNeedsIsloKey(try parseCrabboxCommandLine("crabbox run --provider islo -- true")))
        XCTAssertTrue(commandLineNeedsIsloKey(try parseCrabboxCommandLine("crabbox run --provider=islo -- true")))
        XCTAssertFalse(commandLineNeedsIsloKey(try parseCrabboxCommandLine("crabbox run --provider aws -- true")))
    }
}
