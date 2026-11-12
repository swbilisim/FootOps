import Foundation
import XCTest
@testable import FootOpsKit

/// The core's JSON as the app reads it (panel-tasarim §29.12).
final class ModelsTests: XCTestCase {
    /// durum --json as the core writes it: times with nanoseconds (the servers'), with
    /// milliseconds (the watcher's), with an offset (the Mac's own); fields left out when
    /// empty; one the app does not know.
    func testStatus() throws {
        let json = """
        {"time":"2026-11-09T13:02:15+03:00","color":"yellow","servers":[
         {"code":"h1","name":"H1","bucket":"footops-h1/kasa","panel":"https://panel.example","ssh":"kisi@sunucu.example",
          "color":"yellow","problems":[{"color":"yellow","text":"Sağlıksız: blog"}],
          "file":{"format":1,"server":"H1","footops":"abc1234","time":"2026-11-09T10:00:00Z","every":300,
                  "sites":[{"name":"blog","domains":["blog.example"],"state":"unhealthy"}],
                  "backup":{"ready":true,"last_ok":"2026-11-09T08:00:00.002327991Z","night_done":"2026-11-09"},
                  "sys":{"time":"2026-11-09T10:01:12Z","cpu_percent":2.1,"load1":0,"load5":0,"load15":0,"mem_used":1552867328,
                         "mem_total":4005429248,"swap_used":0,"swap_total":2147479552,"disk_used":9332498432,
                         "disk_total":38300921856,"uptime":118866},"yeni_bir_alan":true},
          "file_at":"2026-11-09T10:00:01Z",
          "watcher":{"format":1,"time":"2026-11-09T09:59:30.123Z","every":300,"interval":120,
                     "domains":[{"name":"blog.example","site":"blog","ok":true,"note":"cloudflare-challenge"}]},
          "watcher_at":"2026-11-09T09:59:31Z","running":1,"total":1,
          "sites":[{"name":"blog","state":"unhealthy","label":"sağlıksız","color":"yellow",
                    "domains":[{"name":"blog.example","watcher":"blind"}]}]},
         {"code":"a","name":"A","bucket":"footops-a","color":"mor","file_error":"durum dosyası yok","silent":true,
          "watcher_stale":true,"running":0,"total":0,"sites":[]}]}
        """
        let st = try CoreJSON.decoder().decode(Status.self, from: Data(json.utf8))
        XCTAssertEqual(st.color, .yellow)
        XCTAssertEqual(st.time, date("2026-11-09T10:02:15Z"))
        XCTAssertNil(st.error)
        XCTAssertEqual(st.servers.count, 2)
        let h1 = st.servers[0]
        XCTAssertEqual(h1.panel, "https://panel.example")
        XCTAssertEqual(h1.problems, [Problem(.yellow, "Sağlıksız: blog")])
        XCTAssertEqual(h1.file?.footops, "abc1234")
        XCTAssertEqual(h1.file?.backup.lastOk?.timeIntervalSince(date("2026-11-09T08:00:00Z")) ?? 0, 0.0023, accuracy: 0.0001)
        XCTAssertEqual(h1.file?.sys?.memTotal, 4_005_429_248)
        XCTAssertEqual(h1.file?.sys?.cpuPercent, 2.1)
        XCTAssertEqual(h1.watcher?.time.timeIntervalSince(date("2026-11-09T09:59:30Z")) ?? 0, 0.123, accuracy: 0.0001)
        XCTAssertEqual(h1.watcher?.domains.first?.ok, true)
        XCTAssertEqual(h1.freshWatcher?.domains.count, 1)
        XCTAssertEqual(h1.sites.first?.domains.first?.isOpen, true)
        let a = st.servers[1]
        XCTAssertEqual(a.color, .gray, "an unknown colour reads as gray")
        XCTAssertNil(a.file)
        XCTAssertTrue(a.silent)
        XCTAssertNil(a.freshWatcher)
        XCTAssertEqual(a.problems, [])
        XCTAssertNil(a.panel)

        let gray = try CoreJSON.decoder().decode(Status.self, from: Data("""
        {"time":"2026-11-09T13:02:15+03:00","color":"gray","error":"sunucu yok","servers":[]}
        """.utf8))
        XCTAssertEqual(gray.error, "sunucu yok")
        XCTAssertEqual(gray.servers, [])
    }

    /// yokla --json's lines, one at a time.
    func testProbeEvents() throws {
        let d = CoreJSON.decoder()
        XCTAssertEqual(try d.decode(ProbeEvent.self, from: Data(#"{"event":"start","total":35}"#.utf8)), .start(total: 35))
        let progress = try d.decode(ProbeEvent.self, from: Data("""
        {"event":"progress","done":3,"total":35,"probe":{"domain":"blog.example","server":"h1","server_name":"H1","site":"blog",
         "ok":false,"code":"timeout","reason":"zaman aşımı (10 sn içinde yanıt yok)","ms":10001,"cloudflare":false,
         "watcher":"ok","verdict":"yalnız bu Mac'ten açılmıyor: yerel ağ ya da DNS olabilir"}}
        """.utf8))
        guard case .progress(3, 35, let p) = progress else {
            return XCTFail("\(progress)")
        }
        XCTAssertEqual(p.domain, "blog.example")
        XCTAssertFalse(p.ok)
        XCTAssertEqual(p.ms, 10001)
        XCTAssertNil(p.certExpires)
        let result = try d.decode(ProbeEvent.self, from: Data("""
        {"event":"result","time":"2026-11-09T13:05:00+03:00","took_ms":2840,"total":1,"open":1,
         "domains":[{"domain":"blog.example","server":"h1","server_name":"H1","site":"blog","ok":true,"code":"200","ms":180,
                     "cloudflare":true,"cert_expires":"2027-01-30T23:59:59Z","watcher":"ok"}],
         "skipped":[{"server":"h1","server_name":"H1","site":"eski","state":"parked","label":"parkta","domains":["eski.example"]}],
         "slowest":"blog.example","slowest_ms":180}
        """.utf8))
        guard case .result(let run) = result else {
            return XCTFail("\(result)")
        }
        XCTAssertEqual(run.tookMs, 2840)
        XCTAssertEqual(run.domains.first?.certExpires, date("2027-01-30T23:59:59Z"))
        XCTAssertEqual(run.skipped, [Skipped(serverName: "H1", site: "eski", label: "parkta")])
        XCTAssertEqual(run.notes, [])
        XCTAssertEqual(try d.decode(ProbeEvent.self, from: Data(#"{"event":"error","message":"durduruldu"}"#.utf8)), .error("durduruldu"))
        XCTAssertThrowsError(try d.decode(ProbeEvent.self, from: Data(#"{"event":"baska"}"#.utf8)))
    }

    /// The last line of yokla's output decides how it ended.
    func testProbeOutcome() {
        let lines = [#"{"event":"start","total":1}"#,
                     #"{"event":"result","time":"2026-11-09T10:05:00Z","took_ms":5,"total":0,"open":0,"domains":[]}"#]
        let done = Monitor.outcome(of: Core.Output(stdout: Data(lines.joined(separator: "\n").utf8), stderr: "", status: 0))
        guard case .finished(let run) = done else {
            return XCTFail("\(done)")
        }
        XCTAssertEqual(run.total, 0)
        let failed = Monitor.outcome(of: Core.Output(stdout: Data(#"{"event":"error","message":"sunucu yok"}"#.utf8), stderr: "", status: 1))
        guard case .failed("sunucu yok", _) = failed else {
            return XCTFail("\(failed)")
        }
        let garbled = Monitor.outcome(of: Core.Output(stdout: Data("panic: ...".utf8), stderr: "goroutine 1\nmore", status: 2))
        guard case .failed(let why, _) = garbled else {
            return XCTFail("\(garbled)")
        }
        XCTAssertEqual(why, "footops-mac'in yanıtı anlaşılamadı (çıkış kodu 2): goroutine 1")
    }

    /// ayar --json in, ayar yaz's stdin out: the file's own form.
    func testSettings() throws {
        let v = try CoreJSON.decoder().decode(SettingsView.self, from: Data("""
        {"path":"/Users/kisi/Library/Application Support/FootOps/ayarlar.json","error":"kilitli",
         "servers":[{"code":"a","default_bucket":"footops-a","keychain":true},
                    {"code":"b","ssh":"kisi@sunucu.example","bucket":"kova-b","default_bucket":"footops-b"}]}
        """.utf8))
        XCTAssertEqual(v.error, "kilitli")
        XCTAssertEqual(v.servers, [ServerSetting(code: "a", defaultBucket: "footops-a"),
                                   ServerSetting(code: "b", ssh: "kisi@sunucu.example", bucket: "kova-b", defaultBucket: "footops-b",
                                                 keychain: false)])
        let data = try JSONEncoder().encode(SettingsFile(v.servers))
        let back = try JSONSerialization.jsonObject(with: data) as? [String: Any]
        XCTAssertEqual(back?["format"] as? Int, 1)
        let servers = back?["servers"] as? [String: [String: String]]
        XCTAssertEqual(servers?["a"], ["name": "", "panel": "", "ssh": "", "bucket": ""])
        XCTAssertEqual(servers?["b"], ["name": "", "panel": "", "ssh": "kisi@sunucu.example", "bucket": "kova-b"])
    }

    /// The core's answer, its own error, or what can be told of neither.
    func testDecodeAnswer() {
        let ok = Core.decode(SettingsView.self, from: Core.Output(stdout: Data(#"{"path":"/x","servers":[]}"#.utf8), stderr: "", status: 0))
        XCTAssertEqual(try ok.get().path, "/x")
        let refused = Core.decode(SettingsView.self, from: Core.Output(
            stdout: Data(#"{"event":"error","message":"h1: SSH hedefi geçersiz"}"#.utf8), stderr: "", status: 1))
        XCTAssertEqual(refused, .failure(CoreMessage("h1: SSH hedefi geçersiz")))
        let nothing = Core.decode(Status.self, from: Core.Output(stdout: Data(), stderr: "", status: 137))
        XCTAssertEqual(nothing, .failure(CoreMessage("footops-mac'in yanıtı anlaşılamadı (çıkış kodu 137)")))
    }

    func testFormat() {
        XCTAssertEqual(Fmt.size(1_552_867_328), "1,4 GB")
        XCTAssertEqual(Fmt.size(512 << 20), "512 MB")
        XCTAssertEqual(Fmt.percent(1_552_867_328, of: 4_005_429_248), 39)
        XCTAssertEqual(Fmt.percent(1, of: 0), 0)
        XCTAssertEqual(Fmt.percent(UInt64.max, of: 1), 999) // a server's figures, not trapped
        XCTAssertEqual(Fmt.duration(ms: 665), "665 ms")
        XCTAssertEqual(Fmt.duration(ms: 2840), "2,8 sn")
        XCTAssertEqual(Fmt.span(20), "1 dk")
        XCTAssertEqual(Fmt.span(125 * 60), "2 sa 5 dk")
        XCTAssertEqual(Fmt.span(120 * 60), "2 sa")
        XCTAssertEqual(Fmt.span((3 * 24 + 4) * 3600), "3 gün 4 sa")
        let now = date("2026-11-09T10:02:00Z")
        XCTAssertEqual(Fmt.ago(now.addingTimeInterval(-30), now: now), "az önce")
        XCTAssertEqual(Fmt.ago(now.addingTimeInterval(-180), now: now), "3 dk önce")
        let istanbul = TimeZone(identifier: "Europe/Istanbul")!
        XCTAssertEqual(Fmt.clock(date("2026-11-09T08:00:00.002Z"), now: now, zone: istanbul), "11:00")
        XCTAssertEqual(Fmt.clock(date("2026-11-08T20:59:00Z"), now: now, zone: istanbul), "08.11 23:59")
        XCTAssertEqual(Fmt.clock(date("2026-11-08T21:00:00Z"), now: now, zone: istanbul), "00:00", "Istanbul's day")
        XCTAssertEqual(Fmt.upperFirst("işlemci"), "İşlemci")
        XCTAssertEqual(Fmt.number(0.41, decimals: 2), "0,41")
    }

    func testLight() {
        XCTAssertEqual(Light.green.worse(.gray), .gray)
        XCTAssertEqual(Light.red.worse(.yellow), .red)
        XCTAssertEqual(Light.yellow.worse(.red), .red)
    }

    private func date(_ s: String) -> Date {
        CoreJSON.parseDate(s)!
    }
}
