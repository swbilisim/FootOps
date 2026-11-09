import Foundation
@testable import FootOpsKit

// Made-up servers and sites throughout: no real domain, address or name.
enum Samples {
    /// 2026-11-09 13:02 in Istanbul.
    static let now = Date(timeIntervalSince1970: 1_794_218_520)

    static func ago(_ minutes: Double) -> Date { now.addingTimeInterval(-minutes * 60) }

    static func site(_ name: String, _ domains: [DomainStatus], state: String = "running", label: String = "çalışıyor",
                     color: Light = .green) -> SiteStatus {
        SiteStatus(name: name, state: state, label: label, color: color, domains: domains)
    }

    static func ok(_ names: String...) -> [DomainStatus] {
        names.map { DomainStatus(name: $0, watcher: "ok") }
    }

    static func beat(_ domains: [(String, Bool)], at: Date) -> WatcherBeat {
        WatcherBeat(time: at, domains: domains.map { WatcherDomain(name: $0.0, ok: $0.1) })
    }

    static func file(_ name: String, cpu: Double, mem: (Double, Double), disk: (Double, Double), backup: Backup) -> StatusFile {
        let gb = { (v: Double) in UInt64(v * Double(1 << 30)) }
        return StatusFile(server: name, footops: "abc1234", time: ago(3), backup: backup,
                          sys: Sys(time: ago(3), cpuPercent: cpu, load1: 0.12, memUsed: gb(mem.0), memTotal: gb(mem.1),
                                   swapTotal: gb(2), diskUsed: gb(disk.0), diskTotal: gb(disk.1), uptime: 118_000))
    }

    static var h1Sites: [SiteStatus] {
        [site("blog", ok("blog.example", "www.blog.example")),
         site("notlar", ok("notlar.example")),
         site("magaza", ok("magaza.example", "www.magaza.example")),
         site("dernek", ok("dernek.example", "www.dernek.example")),
         site("eski-site", [DomainStatus(name: "eski.example", watcher: "unknown")], state: "parked", label: "parkta", color: .gray)]
    }

    static var aSites: [SiteStatus] {
        [site("dukkan", ok("dukkan.example")),
         site("uygulama", ok("app.example", "panel.app.example", "api.app.example")),
         site("katalog", ok("katalog.example"))]
    }

    static var green: Status {
        let h1Domains = h1Sites.flatMap(\.domains).filter { $0.watcher == "ok" }.map { ($0.name, true) }
        let aDomains = aSites.flatMap(\.domains).map { ($0.name, true) }
        return Status(time: now, color: .green, servers: [
            ServerStatus(code: "a", name: "A", bucket: "footops-a", color: .green,
                         file: file("A", cpu: 4.8, mem: (2.2, 7.8), disk: (11.9, 95.8),
                                    backup: Backup(ready: true, lastOk: ago(62), nightDone: "2026-11-09")),
                         fileAt: ago(3), watcher: beat(aDomains, at: ago(2)), watcherAt: ago(2),
                         running: 3, total: 3, sites: aSites),
            ServerStatus(code: "h1", name: "H1", bucket: "footops-h1", panel: "https://panel.example", ssh: "kisi@sunucu.example",
                         color: .green,
                         file: file("H1", cpu: 2.1, mem: (1.5, 3.7), disk: (8.7, 35.7),
                                    backup: Backup(ready: true, lastOk: ago(62), nightDone: "2026-11-09")),
                         fileAt: ago(1), watcher: beat(h1Domains, at: ago(2)), watcherAt: ago(2),
                         running: 4, total: 5, sites: h1Sites),
        ])
    }

    static var aDomainsDown: [(String, Bool)] {
        aSites.flatMap(\.domains).map { ($0.name, $0.name != "katalog.example") }
    }

    /// H1 worth a look (a backup late, a site unhealthy), A with a site down.
    static var troubled: Status {
        var st = green
        st.color = .red
        st.servers[0].color = .red
        st.servers[0].problems = [
            Problem(.red, "Çalışmıyor (container durmuş): katalog"),
            Problem(.red, "Nöbetçiye göre açılmıyor: katalog.example (502 — site yanıt vermiyor, 12:41'dan beri)"),
        ]
        st.servers[0].running = 2
        st.servers[0].sites[2] = site("katalog", [DomainStatus(name: "katalog.example", watcher: "down", code: "502",
                                                              reason: "502 — site yanıt vermiyor", since: ago(21))],
                                      state: "stopped", label: "durduruldu", color: .red)
        st.servers[0].watcher = beat(aDomainsDown, at: ago(2))
        st.servers[1].color = .yellow
        st.servers[1].problems = [
            Problem(.yellow, "Sağlıksız: blog"),
            Problem(.yellow, "Yedek: Gece yedeği gecikti (son başarılı yedek 26 saat önce)"),
        ]
        st.servers[1].sites[0] = site("blog", ok("blog.example", "www.blog.example"), state: "unhealthy", label: "sağlıksız",
                                      color: .yellow)
        return st
    }

    /// Nothing could be read at all.
    static var gray: Status {
        Status(time: now, color: .gray, error: "Anahtar Zinciri'nde footops-yedek / r2-mac-gizli yok (salt okunur anahtarın gizlisi)")
    }

    /// R2 out of reach: each server gray, its files unread.
    static var offline: Status {
        let why = "R2'ye ulaşılamadı: dial tcp: lookup <hesap>.r2.cloudflarestorage.com: no such host"
        return Status(time: now, color: .gray, servers: [
            ServerStatus(code: "a", name: "A", bucket: "footops-a", color: .gray, problems: [Problem(.gray, "Durum dosyası okunamadı: " + why)],
                         fileError: why, watcherError: why),
            ServerStatus(code: "h1", name: "H1", bucket: "footops-h1", color: .gray, problems: [Problem(.gray, "Durum dosyası okunamadı: " + why)],
                         fileError: why, watcherError: why),
        ])
    }

    static func probe(_ domain: String, _ server: String, ok: Bool = true, code: String = "200", ms: Int = 240,
                      reason: String? = nil, watcher: String = "ok", verdict: String? = nil) -> Probe {
        Probe(domain: domain, server: server.lowercased(), serverName: server, site: "", ok: ok, code: code, reason: reason,
              ms: ms, watcher: watcher, verdict: verdict)
    }

    static var probeRun: ProbeRun {
        let list = [
            probe("app.example", "A", ms: 412), probe("api.app.example", "A", code: "401", ms: 380),
            probe("katalog.example", "A", ok: false, code: "502", ms: 220, reason: "502 — site yanıt vermiyor", watcher: "down",
                  verdict: "sunucu ya da site sorunu: nöbetçi de ulaşamıyor"),
            probe("panel.app.example", "A", code: "302", ms: 301), probe("dukkan.example", "A", ms: 655),
            probe("blog.example", "H1", ms: 190), probe("notlar.example", "H1", code: "302", ms: 205),
            probe("magaza.example", "H1", ms: 233), probe("dernek.example", "H1", ms: 181),
            probe("www.blog.example", "H1", code: "301", ms: 176), probe("www.magaza.example", "H1", code: "301", ms: 199),
            probe("www.dernek.example", "H1", code: "301", ms: 188),
        ]
        return ProbeRun(time: ago(1), tookMs: 2840, total: list.count, open: list.count - 1, domains: list,
                        skipped: [Skipped(serverName: "H1", site: "eski-site", label: "parkta")], slowest: "dukkan.example", slowestMs: 655)
    }
}
