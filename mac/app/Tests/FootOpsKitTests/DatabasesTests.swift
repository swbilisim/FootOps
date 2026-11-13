import Foundation
import XCTest
@testable import FootOpsKit

/// The Databases window's side of the core (panel-tasarim §33.4): veritabani's JSON as
/// the app reads it (the core's own output — internal/mac's tests printed it — with
/// made-up names), the window's state through a stand-in core. The real footops-mac is
/// the Go tests'.
@MainActor
final class DatabasesTests: XCTestCase {
    private var folder: URL!

    override func setUp() async throws {
        folder = FileManager.default.temporaryDirectory.appendingPathComponent("footops-vt-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
    }

    override func tearDown() async throws {
        try? FileManager.default.removeItem(at: folder)
    }

    static let list = #"{"uri":"mongodb://127.0.0.1:27017","databases":[{"name":"eski","missing":true,"pair":{"server":"a","server_name":"Contabo","site":"eski"}},{"name":"magaza_dev","bytes":24576,"pair":{"server":"h1","server_name":"H1","site":"magaza"}},{"name":"magaza_dev_yedek_20261109_0900","bytes":122880,"opened":true},{"name":"yeni_site","bytes":4096}]}"#
    static let compared = #"{"event":"result","db":"magaza_dev","server":"h1","server_name":"H1","site":"magaza","backup":{"id":"c5c6d825236a3bbbfb7aba36aa07e6a9a53490f27417860eaf0477a7c6cbb0cc","run":"NightRun0000000000000001","kind":"daily","time":"2026-11-07T03:00:05Z","bytes":1119},"temporary":"magaza_yedek_20261107_0300","diff":{"collections":[{"name":"notlar","local":1,"server":-1,"same":0,"different":0,"only_local":1,"only_server":0,"examples":{"only_local":["1"]}},{"name":"siparisler","local":2,"server":2,"same":2,"different":0,"only_local":0,"only_server":0},{"name":"urunler","local":3,"server":3,"same":1,"different":1,"only_local":1,"only_server":1,"local_newest":{"time":"2026-11-08T12:00:00Z","collection":"urunler","field":"createdAt"},"server_newest":{"time":"2026-11-07T02:00:00Z","collection":"urunler","field":"updatedAt"},"examples":{"different":["2"],"only_local":["4"],"only_server":["3"]}}],"local":{"documents":6,"collections":3,"newest":{"time":"2026-11-08T12:00:00Z","collection":"urunler","field":"createdAt"}},"server_side":{"documents":5,"collections":2,"newest":{"time":"2026-11-07T02:00:00Z","collection":"urunler","field":"updatedAt"}},"same":3,"different":1,"only_local":2,"only_server":1,"identical":false},"took_ms":1495}"#
    static let pulled = #"{"event":"result","db":"magaza_dev","server":"h1","server_name":"H1","site":"magaza","backup":{"id":"c5c6d825236a3bbbfb7aba36aa07e6a9a53490f27417860eaf0477a7c6cbb0cc","run":"NightRun0000000000000001","kind":"daily","time":"2026-11-07T03:00:05Z","bytes":1119},"copy":"magaza_dev_yedek_20261109_0900","loaded":{"name":"magaza_dev","documents":5,"replaced":true},"took_ms":1320}"#
    static let sent = #"{"event":"result","path":"/tmp/x/Downloads/magaza-20261109-090000.footops.tar","site":"magaza","db":"magaza_dev","archive":{"path":"/tmp/x/Downloads/magaza-20261109-090000.footops.tar","name":"magaza-20261109-090000.footops.tar","kind":"footops","size":3072,"sha256":"b484606893829e6e14196f3996e79ac2bafda32c04e406a39f9db8b6ac02f420","site":"magaza","created":"2026-11-09T09:00:00Z","footops":"dev","database":{"name":"magaza_dev","databases":["magaza_dev"],"collections":[{"db":"magaza_dev","name":"siparisler","documents":2},{"db":"magaza_dev","name":"urunler","documents":3}],"documents":5,"server":"7.0.26","tool":"100.12.2","bytes":1155,"compressed":490},"took_ms":0},"took_ms":46,"import":"https://panel.example/sites/magaza/import"}"#
    static let pullFailed = #"{"event":"error","message":"mongorestore başarısız: stream or file does not appear to be a mongodump archive — yereldeki magaza_dev olduğu gibi kaldı"}"#

    func testDecode() throws {
        let d = CoreJSON.decoder()
        let l = try d.decode(DBList.self, from: Data(Self.list.utf8))
        XCTAssertEqual(l.databases.count, 4)
        XCTAssertEqual(l.own.map(\.name), ["eski", "magaza_dev", "yeni_site"], "footops-mac's copies are not the owner's own")
        XCTAssertEqual(l.databases[0].missing, true)
        XCTAssertEqual(l.databases[1].pair, DBPair(server: "h1", serverName: "H1", site: "magaza"))
        XCTAssertNil(l.databases[3].pair)

        guard case .result(let c) = try d.decode(DBEvent.self, from: Data(Self.compared.utf8)), let diff = c.diff else { return XCTFail() }
        XCTAssertEqual(c.backup.kindText, "gece")
        XCTAssertEqual(diff.serverSide.documents, 5)
        XCTAssertEqual(diff.local.newest?.place, "urunler.createdAt")
        XCTAssertEqual(diff.collections.map(\.name), ["notlar", "siparisler", "urunler"])
        XCTAssertEqual(diff.collections[0].server, -1)
        XCTAssertEqual(diff.collections[2].examples?.onlyServer, ["3"])
        XCTAssertEqual(diff.collections.map(\.differs), [true, false, true])
        XCTAssertEqual(diff.onlyLocalCollections, ["notlar"])
        XCTAssertEqual(diff.onlyServerCollections, [])
        XCTAssertFalse(diff.identical)
        XCTAssertEqual(c.tookMs, 1495)

        guard case .result(let p) = try d.decode(DBEvent.self, from: Data(Self.pulled.utf8)) else { return XCTFail() }
        XCTAssertEqual(p.copy, "magaza_dev_yedek_20261109_0900")
        XCTAssertEqual(p.loaded?.documents, 5)
        XCTAssertNil(p.diff)

        guard case .made(let s) = try d.decode(ArchiveEvent.self, from: Data(Self.sent.utf8)) else { return XCTFail() }
        XCTAssertEqual(s.importURL, "https://panel.example/sites/magaza/import")
        guard case .error(let m) = try d.decode(DBEvent.self, from: Data(Self.pullFailed.utf8)) else { return XCTFail() }
        XCTAssertTrue(m.contains("olduğu gibi kaldı"))
        if case .progress("compare", 1200, 2488) = try d.decode(DBEvent.self, from: Data(#"{"done":1200,"event":"progress","part":"compare","total":2488}"#.utf8)) {} else {
            XCTFail("progress")
        }

        // What was opened: a pull's copy says whose.
        let o = try d.decode(OpenedView.self, from: Data(#"{"time":"2026-11-09T09:00:00Z","folders":[],"databases":[{"name":"magaza_dev_yedek_20261109_0900","time":"2026-11-09T09:00:00Z","at":"2026-11-09T09:00:00Z","bytes":122880,"registered":true,"copy":"magaza_dev"}]}"#.utf8))
        XCTAssertEqual(o.databases[0].copy, "magaza_dev")
    }

    func testCount() {
        XCTAssertEqual(Fmt.count(0), "0")
        XCTAssertEqual(Fmt.count(999), "999")
        XCTAssertEqual(Fmt.count(6150), "6.150")
        XCTAssertEqual(Fmt.count(1_234_567), "1.234.567")
        XCTAssertEqual(Fmt.count(-1200), "-1.200")
    }

    func testPages() {
        let s = ServerSites(code: "h1", name: "H1", panel: "https://panel.example/", sites: ["magaza"])
        XCTAssertEqual(s.page(site: "magaza", "backup")?.absoluteString, "https://panel.example/sites/magaza/backup")
        XCTAssertNil(ServerSites(code: "a", name: "A", sites: []).page(site: "x", "import"))
    }

    /// A stand-in core answering as the real one would; it notes its runs.
    /// pull: "ok", "fails", "critical" (in the step that is not stopped: it ignores
    /// SIGTERM and finishes), "late" (stopped while copying, but past the point of no
    /// return already: it finishes all the same).
    private func core(slow: Bool = false, pullFails: Bool = false, pull: String = "ok") throws -> Core {
        let url = folder.appendingPathComponent("footops-mac")
        let script = """
        #!/bin/sh
        echo "$*" >> "\(folder.path)/runs"
        case "$1 $2" in
        "veritabani liste") echo '\(Self.list)' ;;
        "veritabani eslestir") echo "{\\"db\\":\\"$3\\",\\"pair\\":null}" ;;
        "veritabani karsilastir")
          echo '{"event":"start","db":"magaza_dev","server":"h1","server_name":"H1","site":"magaza","backup":{"kind":"daily","time":"2026-11-07T03:00:05Z"},"took_ms":0}'
          echo '{"done":3,"event":"progress","part":"compare","total":11}'
          \(slow ? "trap 'kill $! 2>/dev/null; echo \"{\\\"event\\\":\\\"error\\\",\\\"message\\\":\\\"durduruldu\\\"}\"; exit 1' TERM; sleep 30 & wait" : "")
          echo '\(Self.compared)' ;;
        "veritabani cek")
          echo '{"done":100,"event":"progress","part":"download","total":1119}'
          \(pull == "critical" ? "trap '' TERM; echo '{\"done\":0,\"event\":\"progress\",\"part\":\"load\",\"total\":0}'; sleep 1" : "")
          \(pull == "late" ? "trap '' TERM; echo '{\"done\":0,\"event\":\"progress\",\"part\":\"copy\",\"total\":0}'; sleep 1" : "")
          \(pullFails ? "echo '\(Self.pullFailed)'; exit 1" : "echo '\(Self.pulled)'") ;;
        "veritabani gonder") echo '{"event":"start","path":"/tmp/x/a.footops.tar","site":"magaza","took_ms":0}'; echo '\(Self.sent)' ;;
        "yedek acilanlar") echo '{"time":"2026-11-10T09:00:00Z","folders":[],"databases":[]}' ;;
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

    private let servers = [ServerSites(code: "a", name: "Contabo", sites: ["eski", "yeni-site"]),
                           ServerSites(code: "h1", name: "H1", panel: "https://panel.example", sites: ["magaza", "notlar"])]

    /// The window opens (the databases, what was opened); the first of the owner's
    /// chosen; compared; a pull asked, confirmed, done — the comparison gone with it (the
    /// database changed), the list and what was opened read again.
    func testCompareAndPull() async throws {
        let m = DatabasesModel(core: try core(), servers: servers)
        await m.load()
        XCTAssertEqual(Set(runs), ["veritabani liste --json", "yedek acilanlar --json"])
        XCTAssertEqual(m.selected, "eski")
        m.select("magaza_dev")
        XCTAssertEqual(m.pairedServer?.code, "h1")
        m.compare()
        try await until("compared") { m.compared["magaza_dev"] != nil && !m.isBusy }
        XCTAssertTrue(runs.contains("veritabani karsilastir magaza_dev --json"), "\(runs)")
        XCTAssertEqual(m.compared["magaza_dev"]?.diff?.different, 1)
        XCTAssertEqual(m.operation, .idle)

        m.ask(.pull("magaza_dev"))
        XCTAssertEqual(m.confirm, .pull("magaza_dev"))
        m.confirmed()
        XCTAssertNil(m.confirm)
        try await until("pulled") { if case .pulled = m.operation { true } else { false } }
        XCTAssertTrue(runs.contains("veritabani cek magaza_dev --json"))
        XCTAssertNil(m.compared["magaza_dev"], "the database changed: the comparison is gone")
        try await until("read again") { runs.filter { $0 == "veritabani liste --json" }.count == 2 && runs.filter { $0 == "yedek acilanlar --json" }.count == 2 }
        m.dismiss()
        XCTAssertEqual(m.operation, .idle)
    }

    /// Sent: the archive made, the app told (it opens the import page, shows the archive).
    func testSend() async throws {
        let m = DatabasesModel(core: try core(), servers: servers)
        var told: CreateResult?
        m.onSent = { told = $0 }
        await m.load()
        m.select("magaza_dev")
        m.ask(.send("magaza_dev"))
        m.confirmed()
        try await until("sent") { if case .sent = m.operation { true } else { false } }
        XCTAssertTrue(runs.contains("veritabani gonder magaza_dev --json"), "\(runs)")
        XCTAssertEqual(told?.importURL, "https://panel.example/sites/magaza/import")
        guard case .sent(let db, let res) = m.operation else { return XCTFail() }
        XCTAssertEqual(db, "magaza_dev")
        XCTAssertEqual(res.path, "/tmp/x/Downloads/magaza-20261109-090000.footops.tar")
    }

    /// Pairing: a site of the database's name guessed (an underscore a dash); saved and
    /// removed through the core, the list read again after each.
    func testPairing() async throws {
        let m = DatabasesModel(core: try core(), servers: servers)
        await m.load()
        m.select("yeni_site")
        m.startPairing()
        XCTAssertTrue(m.pairing)
        XCTAssertEqual(m.pairServer, "a")
        XCTAssertEqual(m.pairSite, "yeni-site")
        m.pick(server: "h1")
        XCTAssertNil(m.pairSite, "another server: its sites")
        m.pairSite = "notlar"
        m.savePairing()
        try await until("saved") { !m.pairing }
        XCTAssertTrue(runs.contains("veritabani eslestir yeni_site h1 notlar --json"), "\(runs)")
        // Paired already: its own site to begin with.
        m.select("magaza_dev")
        m.startPairing()
        XCTAssertEqual(m.pairServer, "h1")
        XCTAssertEqual(m.pairSite, "magaza")
        m.pairing = false
        m.removePairing()
        try await until("removed") { runs.contains("veritabani eslestir magaza_dev --kaldir --json") }
        try await until("read again") { runs.filter { $0 == "veritabani liste --json" }.count == 3 }
        // The servers read anew without the one being chosen: the choice goes.
        m.startPairing()
        m.update(servers: [servers[0]])
        XCTAssertNil(m.pairServer)
        XCTAssertNil(m.pairSite)
    }

    /// A pull replacing the database is not stopped (the core finishes it): no stop
    /// button's effect, the app's quit waits; its answer is shown. One stopped while
    /// copying that finished all the same (it was past the point of no return) is shown
    /// as done, not as stopped.
    func testPullNotStoppedHalfWay() async throws {
        let m = DatabasesModel(core: try core(pull: "critical"), servers: servers)
        await m.load()
        m.select("magaza_dev")
        m.ask(.pull("magaza_dev"))
        m.confirmed()
        try await until("replacing") { if case .running(_, _, let part, _, _) = m.operation { part == "load" } else { false } }
        XCTAssertTrue(m.isBusy)
        XCTAssertFalse(m.canStop)
        m.stop()
        await m.stopAndWait(limit: .seconds(10))
        guard case .pulled(let res) = m.operation else { return XCTFail("\(m.operation)") }
        XCTAssertEqual(res.copy, "magaza_dev_yedek_20261109_0900")

        let late = DatabasesModel(core: try core(pull: "late"), servers: servers)
        await late.load()
        late.select("magaza_dev")
        late.ask(.pull("magaza_dev"))
        late.confirmed()
        try await until("copying") { if case .running(_, _, let part, _, _) = late.operation { part == "copy" } else { false } }
        XCTAssertTrue(late.canStop)
        late.stop()
        try await until("ended") { !late.isBusy }
        if case .pulled = late.operation {} else {
            XCTFail("a pull that finished was shown as \(late.operation)")
        }
    }

    /// While one runs nothing else starts, no other database is chosen; stopped, the core
    /// is told and the window is idle again. A pull that fails says why.
    func testBusyStopAndFailure() async throws {
        let m = DatabasesModel(core: try core(slow: true, pullFails: true), servers: servers)
        await m.load()
        m.select("magaza_dev")
        m.compare()
        try await until("running") { if case .running(_, _, let part, _, _) = m.operation { part == "compare" } else { false } }
        XCTAssertTrue(m.isBusy)
        m.select("yeni_site")
        XCTAssertEqual(m.selected, "magaza_dev")
        m.ask(.pull("magaza_dev"))
        XCTAssertNil(m.confirm)
        m.compare()
        XCTAssertEqual(runs.filter { $0.hasPrefix("veritabani karsilastir") }.count, 1)
        let start = Date()
        await m.stopAndWait(limit: .seconds(10))
        XCTAssertLessThan(Date().timeIntervalSince(start), 5)
        XCTAssertEqual(m.operation, .idle)
        XCTAssertNil(m.compared["magaza_dev"])

        m.ask(.pull("magaza_dev"))
        m.confirmed()
        try await until("failed") { if case .failed = m.operation { true } else { false } }
        XCTAssertEqual(m.operation, .failed(db: "magaza_dev", job: .pull,
                                            message: "mongorestore başarısız: stream or file does not appear to be a mongodump archive — yereldeki magaza_dev olduğu gibi kaldı"))
    }
}
