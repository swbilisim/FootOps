import Foundation
import XCTest
@testable import FootOpsKit

/// Running the core: stand-in scripts in its place (a short answer, lines, stdin, a stop,
/// a clock running out). The real footops-mac is the Go tests' (internal/mac).
final class CoreTests: XCTestCase {
    private var folder: URL!

    override func setUpWithError() throws {
        folder = FileManager.default.temporaryDirectory.appendingPathComponent("footops-core-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: folder)
    }

    private func script(_ body: String) throws -> Core {
        let url = folder.appendingPathComponent("footops-mac-\(UUID().uuidString.prefix(8))")
        try ("#!/bin/sh\n" + body + "\n").write(to: url, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: url.path)
        return Core(executable: url)
    }

    /// One object and the exit code (1: the core said what went wrong in it); the
    /// arguments reach it.
    func testRun() async throws {
        let core = try script(#"echo "{\"path\":\"$1-$2\",\"servers\":[]}"; echo uyarı >&2; exit 1"#)
        let out = try await core.run(["ayar", "--json"])
        XCTAssertEqual(out.status, 1)
        XCTAssertEqual(out.stderr, "uyarı\n")
        XCTAssertEqual(try Core.decode(SettingsView.self, from: out).get().path, "ayar---json")
    }

    /// What is given goes in on stdin (ayar yaz).
    func testInput() async throws {
        let core = try script("cat")
        let input = Data(#"{"format":1,"servers":{}}"#.utf8)
        let out = try await core.run(["ayar", "yaz"], input: input)
        XCTAssertEqual(out.stdout, input)
        XCTAssertEqual(out.status, 0)
    }

    /// Lines as they come, in order; a last one without its newline too.
    func testStream() async throws {
        let core = try script(#"printf '{"n":1}\n'; sleep 0.2; printf '{"n":2}\n\n{"n":'; sleep 0.1; printf '3}'"#)
        let lines = Lines()
        let out = try await core.stream(["yokla", "--json"], timeout: .seconds(10)) { lines.add($0) }
        XCTAssertEqual(lines.all, [#"{"n":1}"#, #"{"n":2}"#, #"{"n":3}"#])
        XCTAssertEqual(String(decoding: out.stdout, as: UTF8.self), "{\"n\":1}\n{\"n\":2}\n\n{\"n\":3}")
    }

    /// A stop ends the core with SIGTERM: the run says "stopped", at once.
    func testStop() async throws {
        let core = try script(#"trap 'echo "{\"event\":\"error\",\"message\":\"durduruldu\"}"; exit 1' TERM; sleep 30 & wait"#)
        let task = Task { try await core.run(["durum", "--json"]) }
        try await Task.sleep(for: .milliseconds(300))
        let start = Date()
        task.cancel()
        do {
            _ = try await task.value
            XCTFail("not stopped")
        } catch let f as Core.Failure {
            XCTAssertEqual(f, .stopped)
        }
        XCTAssertLessThan(Date().timeIntervalSince(start), 3)
    }

    /// A core that will not end on its own is stopped by the clock; one deaf to SIGTERM
    /// gets SIGKILL 5 seconds on. (The clock gives perl time to start and turn deaf.)
    func testTimeout() async throws {
        let core = try script(#"exec perl -e '$SIG{TERM} = "IGNORE"; sleep 30'"#)
        let start = Date()
        do {
            _ = try await core.run(["durum", "--json"], timeout: .milliseconds(1500))
            XCTFail("not stopped")
        } catch let f as Core.Failure {
            XCTAssertEqual(f, .timedOut(seconds: 1))
        }
        let took = Date().timeIntervalSince(start)
        XCTAssertGreaterThan(took, 6)
        XCTAssertLessThan(took, 10)
    }

    /// A process of the core's own that keeps its pipes open (it has none: Go closes them
    /// on exec) does not hold the run past the core's end.
    func testChildHoldingPipes() async throws {
        let core = try script(#"echo '{"path":"/x","servers":[]}'; sleep 30 &"#)
        let start = Date()
        let out = try await core.run(["ayar", "--json"])
        XCTAssertLessThan(Date().timeIntervalSince(start), 4)
        XCTAssertEqual(try Core.decode(SettingsView.self, from: out).get().path, "/x")
    }

    /// A core that ends before reading what it is given: the app goes on (no SIGPIPE).
    func testInputUnread() async throws {
        let core = try script("exit 3")
        let out = try await core.run(["ayar", "yaz"], input: Data(repeating: 0x20, count: 1 << 20))
        XCTAssertEqual(out.status, 3)
    }

    func testCannotStart() async {
        let core = Core(executable: folder.appendingPathComponent("yok"))
        do {
            _ = try await core.run(["durum"])
            XCTFail("started")
        } catch let f as Core.Failure {
            guard case .cannotStart = f else {
                return XCTFail("\(f)")
            }
        } catch {
            XCTFail("\(error)")
        }
    }

    /// The core's place: beside the app, or where FOOTOPS_MAC_CORE says.
    func testBundled() {
        XCTAssertEqual(Core.bundled(environment: ["FOOTOPS_MAC_CORE": "/tmp/footops-mac"])?.executable.path, "/tmp/footops-mac")
        XCTAssertNil(Core.bundled(Bundle(for: CoreTests.self), environment: [:]))
    }
}

private final class Lines: @unchecked Sendable {
    private let lock = NSLock()
    private var lines: [String] = []

    func add(_ d: Data) {
        lock.lock()
        lines.append(String(decoding: d, as: UTF8.self))
        lock.unlock()
    }

    var all: [String] {
        lock.lock()
        defer { lock.unlock() }
        return lines
    }
}
