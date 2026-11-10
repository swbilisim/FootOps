import Foundation
import XCTest
@testable import FootOpsKit

/// The Archives window's side of the core (panel-tasarim §29.15): arsiv's JSON as the app
/// reads it (the core's own output, made-up names), the window's state through a stand-in
/// core. The real footops-mac is the Go tests' (internal/mac).
@MainActor
final class ArchivesTests: XCTestCase {
    private var folder: URL!

    override func setUp() async throws {
        folder = FileManager.default.temporaryDirectory.appendingPathComponent("footops-arsiv-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
    }

    override func tearDown() async throws {
        try? FileManager.default.removeItem(at: folder)
    }

    static let checked = #"{"event":"result","path":"/tmp/x/magaza-20261110-013610.footops.tar","name":"magaza-20261110-013610.footops.tar","kind":"footops","size":12288,"sha256":"702416bbb163e9074d6ed6031398b9a73be1f277de4ec8902788ee200a11a025","site":"magaza","created":"2026-11-09T22:36:10Z","footops":"abc1234","backup":{"server":"h1","kind":"daily","time":"2026-11-09T00:00:05.25Z"},"database":{"name":"magaza","databases":["magaza"],"collections":[{"db":"magaza","name":"sayfalar","documents":2},{"db":"magaza","name":"ucuz","view":true,"documents":-1},{"db":"magaza","name":"urunler","documents":3}],"documents":5,"server":"7.0.45","tool":"100.12.2","bytes":31375,"compressed":7811},"files":{"files":3,"dirs":1,"bytes":22,"hidden":1,"hidden_bytes":4,"compressed":259},"env":true,"env_vars":2,"notes":["1 gizli dosya (4 B; .htaccess, .DS_Store gibi) içe aktarılmaz, klasöre de açılmaz."],"took_ms":12}"#
    static let twoDBs = #"{"event":"result","path":"/tmp/x/sunucu.archive.gz","name":"sunucu.archive.gz","kind":"mongodump","size":818,"sha256":"28f0f99ce1f6bf99839704bbd7d7bd25c5a4bc2bbcb59b6b97b0d26cf3e4fe8b","database":{"databases":["blog","magaza"],"collections":[{"db":"blog","name":"yazilar","documents":-1},{"db":"magaza","name":"urunler","documents":-1}],"documents":-1,"server":"7.0.45","tool":"100.19.1","bytes":2433,"compressed":818},"notes":["Dökümde 2 veritabanı var (blog, magaza): yüklerken ya da içe aktarırken biri seçilir."],"took_ms":1}"#
    static let made = #"{"event":"result","path":"/tmp/x/magaza-20261110-090000.footops.tar","site":"magaza","db":"magaza_dev","folder":"/tmp/x/uploads","archive":{"path":"/tmp/x/magaza-20261110-090000.footops.tar","name":"magaza-20261110-090000.footops.tar","kind":"footops","size":46080,"sha256":"ab","site":"magaza","created":"2026-11-10T09:00:00Z","database":{"name":"magaza_dev","databases":["magaza_dev"],"collections":[{"db":"magaza_dev","name":"urunler","documents":7}],"documents":7,"bytes":900,"compressed":300},"files":{"files":2,"dirs":1,"bytes":40000,"compressed":40100},"took_ms":3},"skipped":2,"warnings":["1 dosya ya da klasör adında ayrışık harf var"],"took_ms":700}"#

    func testDecode() throws {
        let d = CoreJSON.decoder()
        guard case .checked(let info) = try d.decode(ArchiveEvent.self, from: Data(Self.checked.utf8)) else { return XCTFail() }
        XCTAssertEqual(info.kind, "footops")
        XCTAssertEqual(info.kindText, "FootOps arşivi")
        XCTAssertEqual(info.parts, ["db", "files", "def"])
        XCTAssertEqual(info.backup?.server, "h1")
        XCTAssertEqual(info.database?.collections.filter(\.view).map(\.name), ["ucuz"])
        XCTAssertEqual(info.database?.documents, 5)
        XCTAssertEqual(info.files?.hiddenBytes, 4)
        XCTAssertEqual(info.envVars, 2)
        XCTAssertEqual(info.tookMs, 12)
        guard case .checked(let two) = try d.decode(ArchiveEvent.self, from: Data(Self.twoDBs.utf8)) else { return XCTFail() }
        XCTAssertNil(two.database?.name)
        XCTAssertEqual(two.database?.moreDatabases, 0)
        let many = try d.decode(ArchiveDB.self, from: Data(#"{"databases":["a","b"],"more_databases":19998,"collections":[],"documents":-1}"#.utf8))
        XCTAssertEqual(many.moreDatabases, 19998)
        XCTAssertEqual(two.database?.documents, -1)
        XCTAssertEqual(two.parts, [], "a dump opens into no folder")
        guard case .made(let res) = try d.decode(ArchiveEvent.self, from: Data(Self.made.utf8)) else { return XCTFail() }
        XCTAssertEqual(res.skipped, 2)
        XCTAssertEqual(res.archive?.database?.documents, 7)
        guard case .started(let st) = try d.decode(ArchiveEvent.self, from: Data(#"{"event":"start","path":"/tmp/x/a.footops.tar","site":"magaza","took_ms":0}"#.utf8)),
              case .progress("check", 512, 12288) = try d.decode(ArchiveEvent.self, from: Data(#"{"done":512,"event":"progress","part":"check","total":12288}"#.utf8)),
              case .error("durduruldu") = try d.decode(ArchiveEvent.self, from: Data(#"{"event":"error","message":"durduruldu"}"#.utf8))
        else { return XCTFail() }
        XCTAssertNil(st.archive)

        // arsiv ac / mongo: yedek's lines, the archive's name and the manifest's count in them.
        let ev = try d.decode(OpenEvent.self, from: Data(#"{"event":"result","server":"h1","bucket":"","site":"magaza","archive":"magaza.footops.tar","time":"2026-11-09T22:36:10Z","parts":{"db":{"id":"","time":"2026-11-09T22:36:10Z","bytes":31375}},"database":{"name":"magaza_arsiv_20261110_0136","documents":5,"expected":5},"took_ms":900}"#.utf8))
        guard case .result(let opened) = ev else { return XCTFail() }
        XCTAssertEqual(opened.archive, "magaza.footops.tar")
        XCTAssertEqual(opened.database?.expected, 5)

        let dbs = try d.decode(LocalDatabases.self, from: Data(#"{"uri":"mongodb://127.0.0.1:27017","databases":[{"name":"magaza_dev","bytes":2592768},{"name":"magaza_arsiv_x","bytes":1,"opened":true}]}"#.utf8))
        XCTAssertEqual(dbs.databases.map(\.opened), [false, true])
        let down = try d.decode(LocalDatabases.self, from: Data(#"{"databases":[],"mongo_error":"bağlanılamadı"}"#.utf8))
        XCTAssertEqual(down.mongoError, "bağlanılamadı")

        let folders = try d.decode(OpenedView.self, from: Data(#"{"folders":[{"path":"/x/magaza-20261110-0136","server":"","site":"magaza","archive":"magaza.footops.tar","time":"2026-11-09T22:36:10Z","at":"2026-11-10T09:00:00Z","files":4,"bytes":31000}],"databases":[{"name":"magaza_arsiv_x","site":"magaza","archive":"magaza.footops.tar","bytes":1,"registered":true}]}"#.utf8))
        XCTAssertEqual(folders.folders[0].archive, "magaza.footops.tar")
        XCTAssertEqual(folders.databases[0].archive, "magaza.footops.tar")
    }

    func testImportURL() {
        let t = PanelTarget(code: "h1", name: "H1", panel: "https://panel.example/", sites: ["magaza"])
        XCTAssertEqual(t.importURL(site: "magaza")?.absoluteString, "https://panel.example/sites/magaza/import")
        let u = PanelTarget(code: "a", name: "A", panel: "http://localhost:7701", sites: [])
        XCTAssertEqual(u.importURL(site: "yeni-site")?.absoluteString, "http://localhost:7701/sites/yeni-site/import")
    }

    /// A stand-in core answering as the real one would; it notes its runs.
    private func core(mongoTaken: Bool = false, slow: Bool = false) throws -> Core {
        let url = folder.appendingPathComponent("footops-mac")
        let openResult = #"{"event":"result","server":"h1","site":"magaza","archive":"magaza-20261110-013610.footops.tar","target":"/tmp/x/magaza-20261110-0136","time":"2026-11-09T22:36:10Z","parts":{"db":{"time":"2026-11-09T22:36:10Z","written":31375}},"took_ms":30}"#
        let mongoResult = #"{"event":"result","server":"h1","site":"magaza","archive":"magaza-20261110-013610.footops.tar","time":"2026-11-09T22:36:10Z","parts":{},"database":{"name":"magaza_arsiv_20261110_0136","documents":5,"expected":5,"replaced":true},"took_ms":90}"#
        let script = """
        #!/bin/sh
        echo "$*" >> "\(folder.path)/runs"
        case "$1 $2" in
        "arsiv dogrula")
          echo '{"done":512,"event":"progress","part":"check","total":12288}'
          case "$3" in
          *.gz) echo '\(Self.twoDBs)' ;;
          *bozuk*) echo '{"event":"error","message":"db.archive.gz: gzip bozuk"}'; exit 1 ;;
          *) echo '\(Self.checked)' ;;
          esac ;;
        "arsiv ac")
          echo '{"event":"start","server":"h1","site":"magaza","target":"/tmp/x/magaza-20261110-0136","time":"2026-11-09T22:36:10Z","parts":{}}'
          \(slow ? "trap 'kill $! 2>/dev/null; echo \"{\\\"event\\\":\\\"error\\\",\\\"message\\\":\\\"durduruldu\\\"}\"; exit 1' TERM; sleep 30 & wait" : "")
          echo '\(openResult)' ;;
        "arsiv mongo")
          case "$*" in
          *--uzerine*|*--ad*|*--kaynak*) echo '\(mongoResult)' ;;
          *) \(mongoTaken ? #"echo '{"code":"db_exists","database":"magaza_arsiv_20261110_0136","event":"error","message":"var"}'; exit 1"# : "echo '\(mongoResult)'") ;;
          esac ;;
        "arsiv olustur") echo '{"event":"start","path":"/tmp/x/magaza-20261110-090000.footops.tar","site":"magaza","took_ms":0}'; echo '{"done":1048576,"event":"progress","part":"db","total":0}'; echo '\(Self.made)' ;;
        "arsiv veritabanlari") echo '{"uri":"mongodb://127.0.0.1:27017","databases":[{"name":"magaza_dev","bytes":2592768}]}' ;;
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

    /// The window opens (what was opened, the Mac's databases); an archive dropped is
    /// checked, its parts ticked; opened into a folder with them.
    func testCheckAndOpen() async throws {
        let m = ArchivesModel(core: try core(), targets: [PanelTarget(code: "h1", name: "H1", panel: "https://p.example", sites: ["magaza"])])
        await m.load()
        XCTAssertEqual(Set(runs), ["yedek acilanlar --json", "arsiv veritabanlari --json"])
        XCTAssertEqual(m.databases?.databases.map(\.name), ["magaza_dev"])
        XCTAssertEqual(m.panelServer, "h1")
        m.choose(file: "/tmp/x/magaza.footops.tar")
        try await until("checked") { m.info != nil }
        XCTAssertEqual(m.parts, ["db", "files", "def"])
        XCTAssertEqual(m.source, "magaza")
        XCTAssertEqual(m.importable, "/tmp/x/magaza-20261110-013610.footops.tar")
        m.parts.remove("def")
        m.openFolder()
        try await until("opened") { if case .finished = m.operation { true } else { false } }
        XCTAssertTrue(runs.contains("arsiv ac /tmp/x/magaza-20261110-013610.footops.tar --db --dosyalar --json"), "\(runs)")
        try await until("the list read again") { runs.filter { $0 == "yedek acilanlar --json" }.count == 2 }
        // One that does not pass.
        m.choose(file: "/tmp/x/bozuk.footops.tar")
        try await until("failed") { if case .failed = m.check { true } else { false } }
        XCTAssertEqual(m.check, .failed(path: "/tmp/x/bozuk.footops.tar", message: "db.archive.gz: gzip bozuk"))
        XCTAssertEqual(m.operation, .idle, "the last archive's result goes with it")
    }

    /// The database's name taken: asked, then written over; a dump of several databases
    /// loads only once one is chosen.
    func testLoad() async throws {
        let m = ArchivesModel(core: try core(mongoTaken: true))
        m.choose(file: "/tmp/x/magaza.footops.tar")
        try await until("checked") { m.info != nil }
        m.loadDatabase()
        try await until("asked") { m.operation == .asking(name: "magaza_arsiv_20261110_0136") }
        XCTAssertEqual(m.otherName, "magaza_arsiv_20261110_0136_2")
        m.retryLoad(name: "magaza_arsiv_20261110_0136", overwrite: true)
        try await until("loaded") { if case .finished = m.operation { true } else { false } }
        // Asked again, then an archive made meanwhile: the question goes (it could not be
        // answered while the make runs).
        m.loadDatabase()
        try await until("asked again") { if case .asking = m.operation { true } else { false } }
        m.makeDB = "magaza_dev"
        m.makeArchive()
        XCTAssertEqual(m.operation, .idle)
        try await until("made") { if case .finished = m.make { true } else { false } }
        XCTAssertEqual(runs.filter { $0.hasPrefix("arsiv mongo") }, ["arsiv mongo /tmp/x/magaza-20261110-013610.footops.tar --json",
                                                                     "arsiv mongo /tmp/x/magaza-20261110-013610.footops.tar --ad magaza_arsiv_20261110_0136 --uzerine --json",
                                                                     "arsiv mongo /tmp/x/magaza-20261110-013610.footops.tar --json"])

        m.choose(file: "/tmp/x/sunucu.archive.gz")
        try await until("checked") { m.info?.kind == "mongodump" }
        XCTAssertNil(m.source)
        let before = runs.count
        m.loadDatabase()
        try await Task.sleep(for: .milliseconds(100))
        XCTAssertEqual(runs.count, before, "none chosen, nothing run")
        m.source = "blog"
        m.loadDatabase()
        try await until("loaded") { runs.contains("arsiv mongo /tmp/x/sunucu.archive.gz --kaynak blog --json") }
    }

    /// Made of a database and a folder; the panel's site picked for it when it has one of
    /// that name.
    func testMake() async throws {
        let m = ArchivesModel(core: try core(), targets: [PanelTarget(code: "h1", name: "H1", panel: "https://p.example", sites: ["blog", "magaza"])])
        m.tab = .make
        XCTAssertNil(m.importable)
        m.makeDB = "magaza_dev"
        m.makeFolder = "/tmp/x/uploads"
        m.makeSite = " magaza "
        m.makeArchive()
        try await until("made") { if case .finished = m.make { true } else { false } }
        XCTAssertTrue(runs.contains("arsiv olustur --db magaza_dev --dosyalar /tmp/x/uploads --site magaza --json"), "\(runs)")
        XCTAssertEqual(m.panelSite, "magaza")
        XCTAssertEqual(m.importable, "/tmp/x/magaza-20261110-090000.footops.tar")
        XCTAssertEqual(m.target?.importURL(site: m.panelSite!)?.absoluteString, "https://p.example/sites/magaza/import")
        m.select(server: "h1")
        XCTAssertEqual(m.panelSite, "magaza", "the same server: the site stays")
        m.update(targets: [PanelTarget(code: "a", name: "A", panel: "https://a.example", sites: [])])
        XCTAssertEqual(m.panelServer, "a")
        XCTAssertNil(m.panelSite)
    }

    /// While one runs nothing else starts; stopped, the core is told and the window is
    /// idle again.
    func testBusy() async throws {
        let m = ArchivesModel(core: try core(slow: true))
        m.choose(file: "/tmp/x/magaza.footops.tar")
        try await until("checked") { m.info != nil }
        m.openFolder()
        try await until("running") { if case .running(let r) = m.operation { r.target != nil } else { false } }
        XCTAssertTrue(m.isBusy)
        m.choose(file: "/tmp/x/baska.footops.tar")
        m.makeDB = "magaza_dev"
        m.makeArchive()
        XCTAssertEqual(runs.filter { $0.hasPrefix("arsiv dogrula") }.count, 1)
        XCTAssertFalse(runs.contains { $0.hasPrefix("arsiv olustur") })
        let start = Date()
        await m.stopAndWait(limit: .seconds(10))
        XCTAssertLessThan(Date().timeIntervalSince(start), 5)
        XCTAssertEqual(m.operation, .idle)
        XCTAssertFalse(m.isBusy)
    }
}
