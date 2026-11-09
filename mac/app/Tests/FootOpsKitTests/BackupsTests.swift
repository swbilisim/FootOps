import AppKit
import Foundation
import XCTest
@testable import FootOpsKit

/// The Backups window's side of the core (panel-tasarim §29.14): yedek's JSON as the app
/// reads it, the window's state through a stand-in core, the recovery line on the
/// pasteboard. The real footops-mac is the Go tests' (internal/mac).
@MainActor
final class BackupsTests: XCTestCase {
    private var folder: URL!

    override func setUp() async throws {
        folder = FileManager.default.temporaryDirectory.appendingPathComponent("footops-yedek-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
    }

    override func tearDown() async throws {
        try? FileManager.default.removeItem(at: folder)
    }

    private func date(_ s: String) -> Date { CoreJSON.parseDate(s)! }

    /// yedek liste, ac's lines, acilanlar, durum's reminder: as the core writes them, times
    /// with nanoseconds, fields left out.
    func testDecode() throws {
        let d = CoreJSON.decoder()
        let list = try d.decode(BackupList.self, from: Data("""
        {"server":"h1","bucket":"footops-h1","time":"2026-11-09T20:10:00Z","took_ms":1934,"snapshots":156,
         "server_part":"2026-11-09T00:00:41.123456789Z",
         "sites":[{"name":"magaza","points":[
           {"run":"HourRun1","time":"2026-11-09T11:00:03.5Z","kind":"hourly","parts":{"db":{"id":"aa","time":"2026-11-09T11:00:03.5Z","files":1,"bytes":38211}}},
           {"run":"NightRun","time":"2026-11-09T00:00:05Z","kind":"daily","parts":{
              "db":{"id":"bb","time":"2026-11-09T00:00:05Z","bytes":38000},
              "files":{"id":"cc","time":"2026-11-09T00:00:40Z","files":48,"bytes":3355443},
              "def":{"id":"dd","time":"2026-11-09T00:01:10Z","files":2,"bytes":914}}}]},
          {"name":"bos","points":[]}]}
        """.utf8))
        XCTAssertEqual(list.snapshots, 156)
        XCTAssertEqual(list.tookMs, 1934)
        XCTAssertEqual(list.serverPart?.timeIntervalSince(date("2026-11-09T00:00:41Z")) ?? 0, 0.123, accuracy: 0.001)
        XCTAssertEqual(list.sites[0].points[1].parts["files"]?.files, 48)
        XCTAssertEqual(list.sites[0].points[0].kindText, "saatlik")
        XCTAssertEqual(PartText.summary(list.sites[0].points[1].parts), "veritabanı 37 KB · dosyalar 48 dosya, 3,2 MB · ayarlar env, footops.json")

        let lines = [
            #"{"event":"start","server":"h1","bucket":"footops-h1","site":"magaza","target":"/Users/kisi/Downloads/magaza-20261109-0301","time":"2026-11-09T00:01:10Z","parts":{"db":{"id":"bb","time":"2026-11-09T00:00:05Z","bytes":38000,"kind":"daily","run":"NightRun"}},"took_ms":0}"#,
            #"{"done":12000,"event":"progress","part":"db","total":38000}"#,
            #"{"event":"result","server":"h1","bucket":"footops-h1","site":"magaza","time":"2026-11-09T00:00:05Z","parts":{"db":{"id":"bb","time":"2026-11-09T00:00:05Z","bytes":38000,"written":38000}},"database":{"name":"magaza_yedek_20261109_0300","documents":89},"warnings":["1 dosya açılamadı: /a: izin yok"],"took_ms":5021}"#,
            #"{"code":"db_exists","database":"magaza_yedek_20261109_0300","event":"error","message":"… var"}"#,
            #"{"event":"error","message":"durduruldu"}"#,
        ]
        let events = try lines.map { try d.decode(OpenEvent.self, from: Data($0.utf8)) }
        guard case .start(let s) = events[0], case .progress("db", 12000, 38000) = events[1], case .result(let r) = events[2],
              case .error(_, "db_exists"?, "magaza_yedek_20261109_0300"?) = events[3], case .error("durduruldu", nil, nil) = events[4]
        else {
            return XCTFail("\(events)")
        }
        XCTAssertEqual(s.target, "/Users/kisi/Downloads/magaza-20261109-0301")
        XCTAssertEqual(r.database, LoadedDB(name: "magaza_yedek_20261109_0300", documents: 89))
        XCTAssertNil(r.target)
        XCTAssertEqual(r.warnings.count, 1)

        let opened = try d.decode(OpenedView.self, from: Data("""
        {"time":"2026-11-09T20:10:00Z","folders":[{"path":"/Users/kisi/Downloads/magaza-20261101-0300","server":"h1","site":"magaza",
          "time":"2026-11-01T00:00:05Z","at":"2026-11-01T09:00:00Z","files":50,"bytes":4000000,"old":true}],
         "databases":[{"name":"magaza_yedek_20261109_0300","bytes":618496},{"name":"x_yedek_1","server":"a","site":"x","time":"2026-11-08T00:00:00Z",
          "at":"2026-11-09T09:00:00Z","bytes":1,"registered":true}],"mongo_error":"bağlanılamadı"}
        """.utf8))
        XCTAssertTrue(opened.folders[0].old)
        XCTAssertFalse(opened.databases[0].registered)
        XCTAssertEqual(opened.databases[1].server, "a")
        XCTAssertEqual(opened.mongoError, "bağlanılamadı")

        let st = try d.decode(Status.self, from: Data(#"{"time":"2026-11-09T10:00:00Z","color":"green","servers":[],"opened_old":2}"#.utf8))
        XCTAssertEqual(st.openedOld, 2)
    }

    /// A stand-in core: each yedek command answers as the real one would; ac and mongo
    /// with their lines. It notes its runs.
    private func core(mongoTaken: Bool = false, slow: Bool = false, twoRuns: Bool = false) throws -> Core {
        let url = folder.appendingPathComponent("footops-mac")
        let newer = twoRuns ? #"{"run":"NewRun","time":"2026-11-09T10:00:05Z","kind":"hourly","parts":{"db":{"id":"aa","time":"2026-11-09T10:00:05Z","bytes":38000}}},"# : ""
        let list = #"{"server":"h1","bucket":"footops-h1","time":"2026-11-09T20:10:00Z","snapshots":4,"sites":[{"name":"magaza","points":["# + newer + #"{"run":"NightRun","time":"2026-11-09T00:00:05Z","kind":"daily","parts":{"db":{"id":"bb","time":"2026-11-09T00:00:05Z","bytes":38000},"files":{"id":"cc","time":"2026-11-09T00:00:40Z","files":3,"bytes":3000}}}]}]}"#
        let start = #"{"event":"start","server":"h1","site":"magaza","target":"/tmp/x/magaza-20261109-0300","time":"2026-11-09T00:00:40Z","parts":{}}"#
        let result = #"{"event":"result","server":"h1","site":"magaza","target":"/tmp/x/magaza-20261109-0300","time":"2026-11-09T00:00:40Z","parts":{"db":{"time":"2026-11-09T00:00:05Z","written":38000}},"took_ms":120}"#
        let mongoResult = #"{"event":"result","server":"h1","site":"magaza","time":"2026-11-09T00:00:05Z","parts":{},"database":{"name":"magaza_yedek_20261109_0300","documents":89,"replaced":true},"took_ms":90}"#
        let script = """
        #!/bin/sh
        echo "$*" >> "\(folder.path)/runs"
        case "$1 $2" in
        "yedek sunucular") echo '{"servers":[{"code":"a","bucket":"footops-a","password":false},{"code":"h1","bucket":"footops-h1","password":true}]}' ;;
        "yedek liste") echo '\(list)' ;;
        "yedek acilanlar") echo '{"time":"2026-11-09T20:10:00Z","folders":[],"databases":[]}' ;;
        "yedek sil") echo '{"done":"Silindi"}' ;;
        "yedek kurtarma") case "$*" in *--json*) exit 3 ;; esac; echo 'FOOTOPS-KURTARMA-1:ZmFrZQ=='; echo 'Kurtarma bilgisi yazıldı' >&2 ;;
        "yedek ac")
          echo '\(start)'
          \(slow ? "trap 'echo \"{\\\"event\\\":\\\"error\\\",\\\"message\\\":\\\"durduruldu\\\"}\"; exit 1' TERM; sleep 30 & wait" : "")
          echo '{"done":1000,"event":"progress","part":"db","total":38000}'
          echo '\(result)' ;;
        "yedek mongo")
          case "$*" in
          *--uzerine*|*--ad*) echo '\(mongoResult)' ;;
          *) \(mongoTaken ? #"echo '{"code":"db_exists","database":"magaza_yedek_20261109_0300","event":"error","message":"var"}'; exit 1"# : "echo '\(mongoResult)'") ;;
          esac ;;
        esac
        """
        try script.write(to: url, atomically: true, encoding: .utf8)
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

    /// The window opens: the servers (the first with a password chosen), its list, what
    /// was opened; a site and its newest backup chosen with all its parts; opened into a
    /// folder with the ticked parts.
    func testOpenFolder() async throws {
        let m = BackupsModel(core: try core(), clipboard: nil)
        await m.load()
        XCTAssertEqual(m.server, "h1")
        XCTAssertEqual(m.list?.snapshots, 4)
        XCTAssertNotNil(m.opened)
        XCTAssertEqual(Set(runs), ["yedek sunucular --json", "yedek liste h1 --json", "yedek acilanlar --json"])
        m.select(site: "magaza")
        XCTAssertEqual(m.run, "NightRun")
        XCTAssertEqual(m.parts, ["db", "files"])
        m.parts.remove("files")
        m.openFolder()
        try await until("opened") { if case .finished = m.operation { true } else { false } }
        XCTAssertTrue(runs.contains("yedek ac h1 magaza NightRun --db --json"), "\(runs)")
        guard case .finished(let res) = m.operation else { return XCTFail() }
        XCTAssertEqual(res.target, "/tmp/x/magaza-20261109-0300")
        try await until("the list read again") { runs.filter { $0 == "yedek acilanlar --json" }.count == 2 }
        // A server without its password is not read.
        m.select(server: "a")
        try await Task.sleep(for: .milliseconds(200))
        XCTAssertFalse(runs.contains("yedek liste a --json"))
    }

    /// The database's name taken: asked, then written over (the same backup, whatever
    /// was chosen since), or loaded under another name.
    func testMongoTaken() async throws {
        let m = BackupsModel(core: try core(mongoTaken: true), clipboard: nil)
        await m.load()
        m.select(site: "magaza")
        m.loadDatabase()
        try await until("asked") { m.operation == .asking(name: "magaza_yedek_20261109_0300") }
        XCTAssertEqual(m.otherName, "magaza_yedek_20261109_0300_2")
        m.retryLoad(name: "magaza_yedek_20261109_0300", overwrite: true)
        try await until("loaded") { if case .finished = m.operation { true } else { false } }
        XCTAssertTrue(runs.contains("yedek mongo h1 magaza NightRun --ad magaza_yedek_20261109_0300 --uzerine --json"), "\(runs)")
        m.loadDatabase()
        try await until("asked again") { if case .asking = m.operation { true } else { false } }
        m.dismiss()
        XCTAssertEqual(m.operation, .idle)
    }

    /// The question about a taken name is about the backup it was asked for: choosing
    /// another drops it, and the main button loads the one chosen now.
    func testAskingGoesWithTheBackup() async throws {
        let m = BackupsModel(core: try core(mongoTaken: true, twoRuns: true), clipboard: nil)
        await m.load()
        m.select(site: "magaza")
        XCTAssertEqual(m.run, "NewRun")
        m.loadDatabase()
        try await until("asked") { if case .asking = m.operation { true } else { false } }
        m.select(run: "NightRun")
        XCTAssertEqual(m.operation, .idle)
        m.retryLoad(name: "x", overwrite: false)
        XCTAssertEqual(m.operation, .idle, "no question, no answer")
        m.loadDatabase()
        try await until("asked about the chosen one") { if case .asking = m.operation { true } else { false } }
        XCTAssertEqual(runs.filter { $0.hasPrefix("yedek mongo") }, ["yedek mongo h1 magaza NewRun --json", "yedek mongo h1 magaza NightRun --json"])
    }

    /// While a backup is being opened, nothing else is chosen; quitting waits for the
    /// core to end (it removes what it began).
    func testBusy() async throws {
        let m = BackupsModel(core: try core(slow: true, twoRuns: true), clipboard: nil)
        await m.load()
        m.select(site: "magaza")
        m.openFolder()
        try await until("running") { if case .running(let r) = m.operation { r.target != nil } else { false } }
        m.select(run: "NightRun")
        m.select(server: "a")
        XCTAssertEqual(m.run, "NewRun")
        XCTAssertEqual(m.server, "h1")
        let start = Date()
        await m.stopAndWait(limit: .seconds(10))
        XCTAssertEqual(m.operation, .idle)
        XCTAssertLessThan(Date().timeIntervalSince(start), 5)
    }

    /// The core's heartbeat before any part has spoken names none: the window still says
    /// the repository is being read. Quitting waits for the core at most its limit.
    func testHeartbeatAndLimit() async throws {
        let url = folder.appendingPathComponent("footops-mac")
        let script = """
        #!/bin/sh
        case "$1 $2" in
        "yedek sunucular") echo '{"servers":[{"code":"h1","bucket":"footops-h1","password":true}]}' ;;
        "yedek liste") echo '{"server":"h1","bucket":"footops-h1","time":"2026-11-09T20:10:00Z","snapshots":1,"sites":[{"name":"magaza","points":[{"run":"R1","time":"2026-11-09T00:00:05Z","kind":"daily","parts":{"db":{"id":"b","time":"2026-11-09T00:00:05Z","bytes":10}}}]}]}' ;;
        "yedek acilanlar") echo '{"time":"2026-11-09T20:10:00Z","folders":[],"databases":[]}' ;;
        "yedek ac")
          trap '' TERM
          echo '{"event":"start","server":"h1","site":"magaza","target":"/tmp/x","time":"2026-11-09T00:00:05Z","parts":{}}'
          echo '{"done":0,"event":"progress","part":"","total":0}'
          sleep 4 ;;
        esac
        """
        try script.write(to: url, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: url.path)
        let m = BackupsModel(core: Core(executable: url), clipboard: nil)
        await m.load()
        m.select(site: "magaza")
        m.openFolder()
        try await until("heartbeat read") {
            if case .running(let r) = m.operation { r.target != nil } else { false }
        }
        try await Task.sleep(for: .milliseconds(200))
        guard case .running(let r) = m.operation else { return XCTFail("\(m.operation)") }
        XCTAssertNil(r.part)
        let start = Date()
        await m.stopAndWait(limit: .seconds(1))
        XCTAssertLessThan(Date().timeIntervalSince(start), 2.5, "the limit holds though the core does not end")
        XCTAssertTrue(m.isBusy)
    }

    /// The outcome is the last result or error, whatever line came after it.
    func testOutcome() {
        let out = Core.Output(stdout: Data("""
        {"event":"start","server":"h1","site":"m","time":"2026-11-09T00:00:05Z","parts":{}}
        {"event":"result","server":"h1","site":"m","time":"2026-11-09T00:00:05Z","parts":{},"took_ms":5}
        {"done":5,"event":"progress","part":"db","total":5}
        """.utf8), stderr: "", status: 0)
        guard case .result(let r)? = BackupsModel.outcome(of: out) else { return XCTFail() }
        XCTAssertEqual(r.tookMs, 5)
    }

    /// Stopped: the core gets SIGTERM, the window goes back to the backup chosen.
    func testStop() async throws {
        let m = BackupsModel(core: try core(slow: true), clipboard: nil)
        await m.load()
        m.select(site: "magaza")
        m.openFolder()
        try await until("running with its folder") {
            if case .running(let r) = m.operation { r.target != nil } else { false }
        }
        let start = Date()
        m.stop()
        try await until("stopped") { m.operation == .idle }
        XCTAssertLessThan(Date().timeIntervalSince(start), 3)
    }

    /// The recovery line: asked of the core without --json, onto the pasteboard alone,
    /// marked concealed, taken off after its time — not when something else was copied.
    func testRecovery() async throws {
        let pb = NSPasteboard(name: NSPasteboard.Name("footops-test-\(UUID().uuidString)"))
        defer { pb.releaseGlobally() }
        let clip = RecoveryClipboard(pasteboard: pb)
        var cleared: Bool?
        let before = pb.changeCount
        clip.put("FOOTOPS-KURTARMA-1:abc", keep: .milliseconds(200)) { cleared = $0 }
        XCTAssertEqual(pb.changeCount, before + 1, "one content: this Mac only holds for all of it")
        XCTAssertEqual(pb.string(forType: .string), "FOOTOPS-KURTARMA-1:abc")
        XCTAssertNotNil(pb.data(forType: RecoveryClipboard.concealed))
        XCTAssertNotNil(pb.data(forType: RecoveryClipboard.transient))
        try await until("taken off") { cleared != nil }
        XCTAssertEqual(cleared, true)
        XCTAssertNil(pb.string(forType: .string))

        clip.put("FOOTOPS-KURTARMA-1:def", keep: .milliseconds(200)) { cleared = $0 }
        cleared = nil
        pb.clearContents()
        pb.setString("başka bir şey", forType: .string)
        try await until("left alone") { cleared != nil }
        XCTAssertEqual(cleared, false)
        XCTAssertEqual(pb.string(forType: .string), "başka bir şey")

        let m = BackupsModel(core: try core(), clipboard: RecoveryClipboard(pasteboard: pb))
        await m.load()
        m.copyRecovery()
        try await until("copied") { if case .copied("h1", _) = m.recovery { true } else { false } }
        XCTAssertEqual(pb.string(forType: .string), "FOOTOPS-KURTARMA-1:ZmFrZQ==")
        XCTAssertTrue(runs.contains("yedek kurtarma h1"), "\(runs)")
        XCTAssertTrue(clip.takeOff() == false, "another clipboard's line is not this one's")
    }
}
