import Foundation
import XCTest
@testable import FootOpsKit

/// The Settings window's side of the core (panel-tasarim §29.16): ayar's and sunucu's JSON
/// as the app reads and writes it, the window's state through a stand-in core. The real
/// footops-mac is the Go tests' (internal/mac).
@MainActor
final class SettingsTests: XCTestCase {
    private var folder: URL!

    override func setUp() async throws {
        folder = FileManager.default.temporaryDirectory.appendingPathComponent("footops-ayar-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
    }

    override func tearDown() async throws {
        try? FileManager.default.removeItem(at: folder)
    }

    /// As the core writes them, fields left out; what the app writes, in the core's keys.
    func testCoding() throws {
        let d = CoreJSON.decoder()
        let v = try d.decode(SettingsView.self, from: Data("""
        {"path":"/x/ayarlar.json","servers":[{"code":"a","default_bucket":"footops-a","keychain":true},
         {"code":"h1","name":"Hetzner – ana","panel":"https://panel.example","default_bucket":"footops-h1","keychain":true}]}
        """.utf8))
        XCTAssertEqual(v.servers[0].name, "")
        XCTAssertEqual(v.servers[0].shownName, "A")
        XCTAssertEqual(v.servers[1].shownName, "Hetzner – ana")

        let file = try JSONSerialization.jsonObject(with: JSONEncoder().encode(SettingsFile(v.servers))) as! [String: Any]
        let servers = file["servers"] as! [String: [String: String]]
        XCTAssertEqual(servers["h1"], ["name": "Hetzner – ana", "panel": "https://panel.example", "ssh": "", "bucket": ""])

        let ns = try JSONSerialization.jsonObject(with: JSONEncoder().encode(NewServer(code: "h2", password: "x"))) as! [String: String]
        XCTAssertEqual(Set(ns.keys), ["code", "name", "panel", "ssh", "bucket", "password"])
        let r2 = try JSONSerialization.jsonObject(with: JSONEncoder().encode(NewR2(account: "a", keyID: "k", secret: "s"))) as! [String: String]
        XCTAssertEqual(r2, ["account": "a", "key_id": "k", "secret": "s"])

        let t = try d.decode(ServerTry.self, from: Data("""
        {"code":"h1","name":"H1","bucket":"footops-h1","checks":[{"part":"status","state":"ok","text":"Durum dosyası okundu"},
         {"part":"backups","state":"skip","text":"şifre yok"}],"ok":true,"took_ms":4530}
        """.utf8))
        XCTAssertEqual(t.checks.map(\.state), ["ok", "skip"])
        XCTAssertEqual(t.tookMs, 4530)
        let access = try d.decode(R2Access.self, from: Data(#"{"account":"0123…cdef","account_set":true,"key_set":true,"secret_set":false}"#.utf8))
        XCTAssertEqual(access.account, "0123…cdef")
        XCTAssertFalse(access.complete)

        let bs = try d.decode(BackupServerList.self, from: Data(#"{"servers":[{"code":"a","bucket":"footops-a","password":true},{"code":"h1","name":"Ana","bucket":"footops-h1","password":true}]}"#.utf8))
        XCTAssertEqual(bs.servers.map(\.name), ["A", "Ana"])
        let folder = try d.decode(OpenedFolder.self, from: Data(#"{"path":"/x","server":"h1","server_name":"Ana","site":"m","time":"2026-11-09T00:00:00Z","at":"2026-11-09T00:00:00Z"}"#.utf8))
        XCTAssertEqual(folder.serverName, "Ana")
    }

    /// A stand-in core: ayar and sunucu as the Go one answers them, what it is given kept
    /// in the folder (one file a command); failing names a command ("sunucu ekle") that
    /// fails, with code when given; slowTry: dene answers after a while.
    private func core(failing: String = "", code: String = "", slowTry: Bool = false) throws -> Core {
        let url = folder.appendingPathComponent("footops-mac")
        let dir = folder.path
        let view = { (servers: String) in #"{"path":"/x/ayarlar.json","servers":[\#(servers)]}"# }
        let a = #"{"code":"a","name":"Contabo","default_bucket":"footops-a","keychain":true}"#
        let h1 = #"{"code":"h1","default_bucket":"footops-h1","keychain":true}"#
        let h2 = #"{"code":"h2","name":"Yeni","default_bucket":"footops-h2","keychain":true}"#
        let script = """
        #!/bin/sh
        echo "$*" >> '\(dir)/runs'
        case "$1 $2" in
        "\(failing)") cat > /dev/null; echo '{"event":"error","message":"olmadı: \(failing)"\(code.isEmpty ? "" : #","code":"\#(code)""#)}'; exit 1 ;;
        esac
        case "$1 $2" in
        "ayar --json")
          if [ -f '\(dir)/h3' ]; then echo '{"path":"/x/ayarlar.json","revision":"yeni","servers":[\(a),\(h1),{"code":"h3","keychain":true}]}'
          else echo '\(view("\(a),\(h1)"))'; fi ;;
        "sunucu r2")
          case "$3" in
          yaz) cat > '\(dir)/r2.json'; echo '{"account":"aaaa…bbbb","account_set":true,"key_set":true,"secret_set":true}' ;;
          *) echo '{"account":"0123…cdef","account_set":true,"key_set":false,"secret_set":false}' ;;
          esac ;;
        "sunucu ekle") cat > '\(dir)/ekle.json'; echo '\(view("\(a),\(h1),\(h2)"))' ;;
        "sunucu sifre") cat > '\(dir)/sifre-'"$3"; echo '\(view("\(a),\(h1),\(h2)"))' ;;
        "sunucu kaldir") echo '\(view("\(a),\(h1)"))' ;;
        "sunucu dene") \(slowTry ? "sleep 0.6;" : "") echo '{"code":"'"$3"'","name":"X","bucket":"footops-'"$3"'","checks":[{"part":"status","state":"ok","text":"okundu"}],"ok":true,"took_ms":5}'; exit 0 ;;
        esac
        """
        try script.write(to: url, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: url.path)
        return Core(executable: url)
    }

    private func kept(_ name: String) -> String {
        (try? String(contentsOf: folder.appendingPathComponent(name), encoding: .utf8)) ?? ""
    }

    private var runs: [String] { kept("runs").split(separator: "\n").map(String.init) }

    /// Read; a server added — its fields to the core on stdin, a name typed and not saved
    /// kept — and tried; its password written, tried again; taken off; the R2 access.
    func testServers() async throws {
        let m = SettingsModel(core: try core(), version: "test")
        await m.load()
        XCTAssertEqual(m.servers.map(\.shownName), ["Contabo", "H1"])
        XCTAssertEqual(m.r2, R2Access(account: "0123…cdef", accountSet: true, keySet: false, secretSet: false))

        m.servers[1].name = "Ana"
        m.startAdding()
        m.adding?.code = " H2 "
        m.adding?.name = "Yeni"
        m.adding?.password = "Abcdefghij0123456789Klmnopqrst0123456789"
        await m.add()
        XCTAssertNil(m.adding)
        XCTAssertEqual(m.servers.map(\.code), ["a", "h1", "h2"])
        XCTAssertEqual(m.servers[1].name, "Ana", "a name typed and not saved stays")
        XCTAssertTrue(m.dirty)
        XCTAssertEqual(m.message, .success("Eklendi: Yeni — deneniyor"))
        let sent = try JSONSerialization.jsonObject(with: Data(kept("ekle.json").utf8)) as! [String: String]
        XCTAssertEqual(sent["code"], " H2 ")
        XCTAssertEqual(sent["password"], "Abcdefghij0123456789Klmnopqrst0123456789")
        guard case .done(let t)? = m.tries["h2"] else { return XCTFail("\(m.tries)") }
        XCTAssertEqual(t.checks, [TryCheck(part: "status", state: "ok", text: "okundu")])
        XCTAssertEqual(runs.suffix(2), ["sunucu ekle --json", "sunucu dene h2 --json"])

        m.passwordFor = "a"
        m.password = "Zyxwvutsrq9876543210Zyxwvutsrq9876543210"
        await m.savePassword()
        XCTAssertNil(m.passwordFor)
        XCTAssertEqual(m.password, "")
        XCTAssertEqual(kept("sifre-a"), "Zyxwvutsrq9876543210Zyxwvutsrq9876543210")
        XCTAssertEqual(runs.last, "sunucu dene a --json")

        m.removing = "h2"
        await m.remove()
        XCTAssertNil(m.removing)
        XCTAssertNil(m.tries["h2"])
        XCTAssertEqual(m.servers.map(\.code), ["a", "h1"])
        XCTAssertEqual(m.message, .success("Yeni bu Mac'ten kaldırıldı (sunucuya dokunulmadı)"))

        m.startR2()
        m.r2Form?.keyID = "k"
        m.r2Form?.secret = "s"
        await m.saveR2()
        XCTAssertNil(m.r2Form)
        XCTAssertEqual(m.r2?.complete, true)
        XCTAssertEqual(try JSONSerialization.jsonObject(with: Data(kept("r2.json").utf8)) as? [String: String],
                       ["account": "", "key_id": "k", "secret": "s"])
    }

    /// The core refusing: said in the message, the form as it was, nothing tried.
    func testRefused() async throws {
        let m = SettingsModel(core: try core(failing: "sunucu ekle"), version: "test")
        await m.load()
        m.startAdding()
        m.adding?.code = "h1"
        await m.add()
        XCTAssertEqual(m.message, .failure("olmadı: sunucu ekle"))
        XCTAssertEqual(m.adding?.code, "h1")
        XCTAssertTrue(m.tries.isEmpty)
        XCTAssertFalse(m.busy)
    }

    /// A try's answer coming after its server was taken off is dropped: it would land on
    /// one added again under the same code.
    func testLateTry() async throws {
        let m = SettingsModel(core: try core(slowTry: true), version: "test")
        await m.load()
        let trying = Task { await m.tryServer("a") }
        try await Task.sleep(for: .milliseconds(150))
        XCTAssertEqual(m.tries["a"], .running)
        m.removing = "a"
        await m.remove()
        await trying.value
        XCTAssertNil(m.tries["a"])
    }

    /// A failed command reads the list anew (part of it may have been done); a save over
    /// a file changed elsewhere is refused, the list read anew with what was typed kept.
    func testReadAnew() async throws {
        let m = SettingsModel(core: try core(failing: "sunucu kaldir"), version: "test")
        await m.load()
        m.removing = "h1"
        await m.remove()
        XCTAssertEqual(m.message, .failure("olmadı: sunucu kaldir"))
        XCTAssertEqual(runs.suffix(2), ["sunucu kaldir h1 --json", "ayar --json"])

        let s = SettingsModel(core: try core(failing: "ayar yaz", code: "settings_changed"), version: "test")
        await s.load()
        s.servers[0].name = "Yeni ad"
        try Data().write(to: folder.appendingPathComponent("h3"))
        await s.save()
        guard case .failure(let text)? = s.message else { return XCTFail("\(String(describing: s.message))") }
        XCTAssertTrue(text.hasPrefix("Ayarlar bu arada başka bir yerden değişti"), text)
        XCTAssertEqual(s.servers.map(\.code), ["a", "h1", "h3"])
        XCTAssertEqual(s.servers[0].name, "Yeni ad")
        XCTAssertEqual(s.revision, "yeni")
        XCTAssertTrue(s.dirty)
    }

    /// One form at a time: Return belongs to the open one.
    func testOneForm() async throws {
        let m = SettingsModel(view: SettingsView(path: "/x", servers: [ServerSetting(code: "a")]), login: .disabled, version: "test")
        XCTAssertFalse(m.formOpen)
        m.startAdding()
        m.adding?.password = "gizli"
        m.startPassword("a")
        XCTAssertNil(m.adding)
        XCTAssertEqual(m.passwordFor, "a")
        m.password = "yazilan"
        m.startR2()
        XCTAssertNil(m.passwordFor)
        XCTAssertEqual(m.password, "")
        XCTAssertTrue(m.formOpen)
    }
}
