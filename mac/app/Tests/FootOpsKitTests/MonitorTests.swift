import Foundation
import XCTest
@testable import FootOpsKit

/// When the app reads, and what it keeps of a read (panel-tasarim §29.3), with a stand-in
/// core that counts its runs.
@MainActor
final class MonitorTests: XCTestCase {
    private var folder: URL!

    override func setUp() async throws {
        folder = FileManager.default.temporaryDirectory.appendingPathComponent("footops-monitor-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
    }

    override func tearDown() async throws {
        try? FileManager.default.removeItem(at: folder)
    }

    private func core(_ body: String) throws -> Core {
        let url = folder.appendingPathComponent("footops-mac")
        try ("#!/bin/sh\necho \"$*\" >> \"\(folder.path)/runs\"\n" + body + "\n").write(to: url, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: url.path)
        return Core(executable: url)
    }

    private var runs: [String] {
        ((try? String(contentsOf: folder.appendingPathComponent("runs"), encoding: .utf8)) ?? "")
            .split(separator: "\n").map(String.init)
    }

    private func until(_ what: String, _ ok: () -> Bool) async throws {
        for _ in 0..<250 where !ok() {
            try await Task.sleep(for: .milliseconds(20))
        }
        XCTAssertTrue(ok(), what)
    }

    /// Read at once; the menu opening on a read younger than a minute reads nothing, on an
    /// older one reads again.
    func testReads() async throws {
        let m = Monitor(core: try core(#"echo '{"time":"2026-11-09T10:00:00Z","color":"green","servers":[]}'"#))
        m.start()
        defer { m.stop() }
        try await until("first read") { m.readAt != nil && !m.reading }
        XCTAssertEqual(runs, ["durum --json"])
        XCTAssertEqual(m.light, .green)
        XCTAssertNil(m.trouble)
        m.menuOpened()
        try await Task.sleep(for: .milliseconds(300))
        XCTAssertEqual(runs.count, 1, "a fresh read is not repeated")
        m.menuOpened(now: Date().addingTimeInterval(61))
        try await until("second read") { runs.count == 2 && !m.reading }
    }

    /// Asked to read while a read is under way (new settings saved): one more read once it
    /// ends, which sees them.
    func testRefreshWhileReading() async throws {
        let m = Monitor(core: try core(#"sleep 0.4; echo '{"time":"2026-11-09T10:00:00Z","color":"green","servers":[]}'"#))
        m.start()
        defer { m.stop() }
        try await until("reading") { m.reading }
        m.refresh()
        try await until("read again") { runs.count == 2 }
        try await until("done") { !m.reading && m.readAt != nil }
        try await Task.sleep(for: .milliseconds(600))
        XCTAssertEqual(runs.count, 2, "once more, not again and again")
    }

    /// A core that cannot be understood: gray, with why; the answer before stays.
    func testTrouble() async throws {
        let m = Monitor(core: try core("echo panic >&2; exit 2"))
        m.start()
        defer { m.stop() }
        try await until("read") { m.readAt != nil && !m.reading }
        XCTAssertEqual(m.light, .gray)
        XCTAssertEqual(m.trouble, "footops-mac'in yanıtı anlaşılamadı (çıkış kodu 2): panic")
        XCTAssertNil(m.status)
    }

    /// After a gray read, three sooner tries (the network may be just back), then the
    /// usual 5 minutes; a known colour starts the count again.
    func testGrayRetries() {
        let gray = Monitor(status: Samples.offline, readAt: Date())
        XCTAssertEqual((0..<5).map { _ in gray.nextWait() }, [Monitor.retry, Monitor.retry, Monitor.retry, Monitor.every, Monitor.every])
        XCTAssertEqual(Monitor(status: Samples.green, readAt: Date()).nextWait(), Monitor.every)
    }

    /// yokla to its end: its result, the lines between moving the progress.
    func testProbe() async throws {
        let m = Monitor(core: try core("""
        echo '{"event":"start","total":1}'
        sleep 0.2
        echo '{"event":"progress","done":1,"total":1,"probe":{"domain":"blog.example","server":"h1","server_name":"H1","site":"blog","ok":false,"code":"502","reason":"502 — site yanıt vermiyor","ms":120,"cloudflare":true,"watcher":"down"}}'
        sleep 0.2
        echo '{"event":"result","time":"2026-11-09T10:05:00Z","took_ms":400,"total":1,"open":0,"domains":[{"domain":"blog.example","server":"h1","server_name":"H1","site":"blog","ok":false,"code":"502","ms":120,"cloudflare":true,"watcher":"down"}]}'
        """))
        m.startProbe()
        try await until("progress") {
            if case .running(1, 1, let failed) = m.probe {
                return failed.map(\.domain) == ["blog.example"]
            }
            return false
        }
        try await until("result") {
            if case .finished(let run) = m.probe {
                return run.total == 1 && run.open == 0
            }
            return false
        }
        XCTAssertEqual(runs, ["yokla --json"])
    }

    /// Stopped half-way: back where it started, the core ended.
    func testProbeStop() async throws {
        let m = Monitor(core: try core(#"trap 'exit 1' TERM; echo '{"event":"start","total":9}'; while :; do sleep 0.1; done"#))
        m.startProbe()
        try await until("started") {
            if case .running(0, 9, _) = m.probe {
                return true
            }
            return false
        }
        m.stopProbe()
        try await until("stopped") { m.probe == .idle }
    }
}
