import AppKit
import SwiftUI
import XCTest
@testable import FootOpsKit

/// The app's views drawn from the samples, light and dark, into PNGs to look at
/// (panel-tasarim §29.9): FOOTOPS_SNAPSHOTS names the folder; without it they are drawn
/// only, so a view that cannot be drawn still fails.
@MainActor
final class SnapshotTests: XCTestCase {
    private var folder: URL? {
        ProcessInfo.processInfo.environment["FOOTOPS_SNAPSHOTS"].map { URL(fileURLWithPath: $0, isDirectory: true) }
    }

    func testMenu() throws {
        let cases: [(String, Monitor, Set<String>, Bool)] = [
            ("menu-green", Monitor(status: Samples.green, readAt: Samples.ago(1)), [], false),
            ("menu-green-open", Monitor(status: Samples.green, readAt: Samples.ago(1)), ["h1"], false),
            ("menu-troubled", Monitor(status: Samples.troubled, readAt: Samples.ago(1)), ["a"], false),
            ("menu-gray", Monitor(status: Samples.gray, readAt: Samples.ago(1)), [], false),
            ("menu-offline", Monitor(status: Samples.offline, readAt: Samples.ago(1)), [], false),
            ("menu-trouble", Monitor(status: Samples.green, readAt: Samples.ago(7), trouble: "footops-mac 60 sn içinde bitmedi, durduruldu"), [], false),
            ("menu-first", Monitor(status: nil, readAt: nil, reading: true), [], false),
            ("menu-probing", Monitor(status: Samples.green, readAt: Samples.ago(1),
                                     probe: .running(done: 7, total: 12, failed: [Samples.probeRun.domains[2]])), [], false),
            ("menu-probed", Monitor(status: Samples.troubled, readAt: Samples.ago(1), probe: .finished(Samples.probeRun)), [], true),
        ]
        for (name, monitor, expanded, all) in cases {
            let view = MenuView(monitor: monitor, actions: .none, now: Samples.now, maxHeight: nil, expanded: expanded, allProbes: all)
            try draw(name, view)
        }
    }

    func testSettings() throws {
        let path = "/Users/kisi/Library/Application Support/FootOps/ayarlar.json"
        let servers = [
            ServerSetting(code: "a", name: "Contabo", panel: "https://panel-a.example", ssh: "kisi@192.0.2.10", defaultBucket: "footops-a"),
            ServerSetting(code: "h1", name: "Hetzner – ana", panel: "https://panel.example", ssh: "kisi@sunucu.example", defaultBucket: "footops-h1"),
        ]
        let model = SettingsModel(view: SettingsView(path: path, servers: servers), login: .enabled, version: "abc1234",
                                  r2: R2Access(account: "0123…cdef", accountSet: true, keySet: true, secretSet: true))
        model.tries["h1"] = .done(ServerTry(code: "h1", name: "Hetzner – ana", bucket: "footops-h1", checks: [
            TryCheck(part: "status", state: "ok", text: "Durum dosyası okundu: FootOps abc1234 · 9 / 9 site çalışıyor · son yedek 09:00 (2 dk önce yazılmış)"),
            TryCheck(part: "backups", state: "ok", text: "Yedek deposu açıldı (salt okunur): 154 anlık görüntü"),
        ], ok: true))
        model.tries["a"] = .running
        try drawWindow("settings", SettingsWindow(model: model), size: NSSize(width: SettingsWindow.width, height: 1020))

        // A server being added, one that failed its try (Cloudflare's step), a password
        // being typed, one asked about before removal, the R2 access being typed.
        let forms = SettingsModel(view: SettingsView(path: path, servers: [
            ServerSetting(code: "a", name: "Contabo", defaultBucket: "footops-a"),
            ServerSetting(code: "deneme", defaultBucket: "footops-test", keychain: false),
        ]), login: .enabled, version: "abc1234", r2: R2Access(accountSet: false, keySet: false, secretSet: false))
        forms.tries["deneme"] = .done(ServerTry(code: "deneme", name: "DENEME", bucket: "footops-test", checks: [
            TryCheck(part: "status", state: "fail", text: "Kova bu Mac'in salt okunur anahtarıyla okunamıyor (R2: 403). Cloudflare → R2 → API token'ları → bu Mac'in salt okunur anahtarı → footops-test kovasını ekle."),
            TryCheck(part: "backups", state: "skip", text: "Yedek deposu şifresi Anahtar Zinciri'nde yok: yedekler bu Mac'te açılamaz (Ayarlar → şifre)"),
        ], ok: false))
        forms.passwordFor = "a"
        forms.password = "gizli"
        forms.removing = "deneme"
        forms.adding = NewServer(code: "h2", name: "Yeni sunucu", panel: "https://panel-h2.example")
        forms.r2Form = NewR2()
        try drawWindow("settings-forms", SettingsWindow(model: forms), size: NSSize(width: SettingsWindow.width, height: 1400))

        let empty = SettingsModel(view: SettingsView(path: path, servers: []), login: .disabled, version: "abc1234")
        try drawWindow("settings-empty", SettingsWindow(model: empty), size: NSSize(width: SettingsWindow.width, height: 560))
        let failing = SettingsModel(view: SettingsView(path: path, servers: [
            ServerSetting(code: "h1", panel: "panel.example", defaultBucket: "footops-h1/kasa"),
        ]), login: .requiresApproval, version: "abc1234")
        failing.servers[0].ssh = "kisi@sunucu.example"
        failing.message = .failure("ayarlar dosyası: h1: panel adresi geçersiz: \"panel.example\" (https://… biçiminde olmalı)")
        try drawWindow("settings-error", SettingsWindow(model: failing), size: NSSize(width: SettingsWindow.width, height: 560))
    }

    /// The Backups window in its states: a backup chosen, being opened, the database's
    /// name taken, opened (with a warning), failed; what was opened (a week old among
    /// them); a server without its password; the list being read.
    func testBackups() throws {
        let t = { (h: Double) in Samples.now.addingTimeInterval(-h * 3600) }
        let part = { (h: Double, files: Int64, bytes: Int64) in BackupPart(id: "x", time: t(h), files: files, bytes: bytes) }
        let points = [
            BackupPoint(run: "Hour1", time: t(1), kind: "hourly", parts: ["db": part(1, 1, 38_211)]),
            BackupPoint(run: "Night1", time: t(10), kind: "daily", parts: ["db": part(10, 1, 38_000), "files": part(10, 48, 3_355_443),
                                                                          "def": part(10, 2, 914)]),
            BackupPoint(run: "Night0", time: t(34), kind: "daily", parts: ["db": part(34, 1, 37_500), "files": part(34, 47, 3_300_000),
                                                                          "def": part(34, 2, 914)]),
            BackupPoint(run: "Manual", time: t(50), kind: "manual", parts: ["db": part(50, 1, 37_000)]),
        ]
        let list = BackupList(server: "h1", bucket: "footops-h1", time: Samples.ago(2), tookMs: 1934, snapshots: 156, serverPart: t(10),
                              sites: [SiteBackups(name: "blog", points: Array(points.prefix(2))),
                                      SiteBackups(name: "magaza", points: points),
                                      SiteBackups(name: "uzun-bir-site-adi-burada", points: [points[1]])])
        let servers = [BackupServer(code: "a", name: "Contabo", bucket: "footops-a"), BackupServer(code: "h1", name: "Hetzner – ana", bucket: "footops-h1")]
        let opened = OpenedView(folders: [
            OpenedFolder(path: "/Users/kisi/Downloads/magaza-20261109-0300", server: "h1", site: "magaza", time: t(10), at: Samples.ago(5),
                         files: 50, bytes: 3_400_000),
            OpenedFolder(path: "/Users/kisi/Downloads/blog-20261101-0300", server: "h1", site: "blog", time: t(200), at: t(190),
                         files: 12, bytes: 220_000, old: true),
            OpenedFolder(path: "/Users/kisi/Downloads/magaza-20261108-0300", server: "h1", site: "magaza", time: t(34), at: t(20),
                         files: 3, bytes: 90_000, partial: true),
            OpenedFolder(path: "/Volumes/Disk/magaza", server: "h1", site: "magaza", time: t(34), at: t(30), files: 0, bytes: 0,
                         error: "bakılamadı: lstat /Volumes/Disk/magaza: no such file or directory"),
        ], databases: [
            OpenedDatabase(name: "magaza_yedek_20261109_0300", server: "h1", site: "magaza", time: t(10), at: Samples.ago(3), bytes: 618_496),
            OpenedDatabase(name: "eski_yedek_20261020", bytes: 1_200_000, registered: false),
        ])
        let res = OpenResult(server: "h1", site: "magaza", target: "/Users/kisi/Downloads/magaza-20261109-0300", time: t(10),
                             parts: ["db": OpenedPart(time: t(10), written: 38_000), "files": OpenedPart(time: t(10), written: 3_355_443, count: 48),
                                     "def": OpenedPart(time: t(10), count: 7)],
                             warnings: ["1 dosya açılamadı: /kilitli/c.txt: izin yok"], tookMs: 5021)
        let cases: [(String, BackupsModel)] = [
            ("backups", BackupsModel(servers: servers, server: "h1", lists: ["h1": .loaded(list)], site: "magaza", run: "Night1",
                                     opened: opened, recovery: .copied(server: "h1", at: Samples.ago(0.2)))),
            ("backups-running", BackupsModel(servers: servers, server: "h1", lists: ["h1": .loaded(list)], site: "magaza", run: "Night1",
                                             operation: .running(.init(mongo: false, site: "magaza", target: "/x", part: "files",
                                                                       done: 1_200_000, total: 3_355_443)), opened: OpenedView())),
            ("backups-asking", BackupsModel(servers: servers, server: "h1", lists: ["h1": .loaded(list)], site: "magaza", run: "Night1",
                                            operation: .asking(name: "magaza_yedek_20261109_0300"), opened: opened)),
            ("backups-done", BackupsModel(servers: servers, server: "h1", lists: ["h1": .loaded(list)], site: "magaza", run: "Night1",
                                          operation: .finished(res), opened: opened, recovery: .kept)),
            ("backups-failed", BackupsModel(servers: servers, server: "h1", lists: ["h1": .loaded(list)], site: "magaza", run: "Hour1",
                                            operation: .failed("macOS /Users/kisi/Downloads klasörüne yazma izni vermedi: Sistem Ayarları › Gizlilik ve Güvenlik › Dosyalar ve Klasörler › FootOps (ya da --hedef ile başka bir klasör)"),
                                            opened: opened, recovery: .failed("Anahtar Zinciri'nde footops-yedek / restic-h1 yok"))),
            ("backups-nopassword", BackupsModel(servers: [BackupServer(code: "a", bucket: "footops-a", password: false), servers[1]],
                                                server: "a", opened: OpenedView(mongoError: "Mac'teki MongoDB'ye bağlanılamadı (mongodb://127.0.0.1:27017): çalışıyor mu?"))),
            ("backups-reading", BackupsModel(servers: servers, server: "h1", readingList: ["h1"])),
        ]
        for (name, model) in cases {
            if model.run != nil {
                model.parts = Set(model.point?.parts.keys.map { $0 } ?? [])
            }
            if name == "backups" {
                model.opened.confirmDelete = "eski_yedek_20261020"
            }
            try drawWindow(name, BackupsWindow(model: model, actions: .none, now: Samples.now), size: BackupsWindow.size)
        }
        let menu = Monitor(status: Status(time: Samples.now, color: .green, servers: Samples.green.servers, openedOld: 2), readAt: Samples.ago(1))
        try draw("menu-reminder", MenuView(monitor: menu, actions: .none, now: Samples.now, maxHeight: nil))
    }

    /// The Archives window in its states: nothing yet, an archive checked (a backup's,
    /// with its secrets), a dump of two databases, being opened, a name taken, loaded,
    /// refused, an archive of files; making one: chosen, under way, made (the panel's
    /// import page to open).
    func testArchives() throws {
        let d = CoreJSON.decoder()
        guard case .checked(let info) = try d.decode(ArchiveEvent.self, from: Data(ArchivesTests.checked.utf8)),
              case .checked(let two) = try d.decode(ArchiveEvent.self, from: Data(ArchivesTests.twoDBs.utf8)),
              case .made(let made) = try d.decode(ArchiveEvent.self, from: Data(ArchivesTests.made.utf8))
        else { return XCTFail() }
        let files = ArchiveInfo(path: "/Users/kisi/Downloads/uploads.zip", kind: "files", size: 8_400_000, sha256: String(repeating: "ab", count: 32),
                                files: ArchiveFiles(files: 42, dirs: 3, bytes: 8_300_000, hidden: 2, hiddenBytes: 12_000),
                                notes: ["2 gizli dosya (12 KB; .htaccess, .DS_Store gibi) içe aktarılmaz, klasöre de açılmaz.",
                                        "Bütün dosyalar uploads/ klasörünün içinde: panel içe aktarırken \"üst klasörü atla\"yı önerebilir."])
        let targets = [PanelTarget(code: "a", name: "A", panel: "https://panel-a.example", sites: ["etiket", "magaza", "yag-dukkani", "kasa-pos"]),
                       PanelTarget(code: "h1", name: "H1", panel: "https://panel-h1.example", sites: ["blog", "defter", "ornek-sirket"])]
        let t = { (h: Double) in Samples.now.addingTimeInterval(-h * 3600) }
        let opened = OpenedView(folders: [
            OpenedFolder(path: "/Users/kisi/Downloads/magaza-20261110-0136", server: "h1", site: "magaza", archive: "magaza-20261110-013610.footops.tar",
                         time: t(10), at: Samples.ago(5), files: 4, bytes: 31_000),
        ], databases: [
            OpenedDatabase(name: "magaza_arsiv_20261110_0136", site: "magaza", archive: "magaza-20261110-013610.footops.tar", time: t(10),
                           at: Samples.ago(3), bytes: 524_288),
            OpenedDatabase(name: "blog_yedek_20261109_0300", server: "h1", site: "blog", time: t(30), at: t(20), bytes: 618_496),
        ])
        let loaded = OpenResult(server: "h1", site: "magaza", archive: info.name, target: "/Users/kisi/Downloads/magaza-20261110-0136", time: t(10),
                                parts: ["db": OpenedPart(time: t(10), written: 31_375), "files": OpenedPart(time: t(10), written: 18, count: 2)],
                                database: LoadedDB(name: "magaza_arsiv_20261110_0136", documents: 5, expected: 5), tookMs: 1300)
        let dbs = LocalDatabases(uri: "mongodb://127.0.0.1:27017", databases: [
            LocalDatabase(name: "dukkan", bytes: 45_178_880), LocalDatabase(name: "magaza_dev", bytes: 2_592_768),
            LocalDatabase(name: "magaza_arsiv_20261110_0136", bytes: 524_288, opened: true), LocalDatabase(name: "ornek_sirket", bytes: 1_200_000),
        ])
        let cases: [(String, ArchivesModel)] = [
            ("archives-empty", ArchivesModel(opened: OpenedView())),
            ("archives-checked", ArchivesModel(check: .checked(info), targets: targets, panelSite: "magaza", opened: opened)),
            ("archives-twodbs", ArchivesModel(check: .checked(two), targets: targets, opened: opened)),
            ("archives-checking", ArchivesModel(check: .checking(path: "/Users/kisi/Downloads/buyuk-20261110-0300.footops.tar", done: 300_000_000, total: 820_000_000),
                                                opened: opened)),
            ("archives-opening", ArchivesModel(check: .checked(info), operation: .running(.init(mongo: false, site: "magaza", target: "/x", part: "files",
                                                                                                  done: 12, total: 18)), opened: opened)),
            ("archives-asking", ArchivesModel(check: .checked(info), operation: .asking(name: "magaza_arsiv_20261110_0136"), opened: opened)),
            ("archives-loaded", ArchivesModel(check: .checked(info), operation: .finished(loaded), targets: targets, opened: opened)),
            ("archives-failed", ArchivesModel(check: .failed(path: "/Users/kisi/Downloads/bozuk.footops.tar",
                                                             message: "db.archive.gz manifest'le tutmuyor (boyut ya da sha256): dosya bozulmuş"), opened: opened)),
            ("archives-files", ArchivesModel(check: .checked(files), targets: targets, opened: OpenedView())),
            ("archives-make", ArchivesModel(tab: .make, databases: dbs, makeDB: "magaza_dev", makeFolder: "/Users/kisi/Projeler/magaza/uploads", opened: opened)),
            ("archives-make-running", ArchivesModel(tab: .make, databases: dbs, makeDB: "magaza_dev", make: .running(part: "files", done: 3_000_000, total: 8_300_000),
                                                    opened: opened)),
            ("archives-made", ArchivesModel(tab: .make, databases: dbs, makeDB: "magaza_dev", makeFolder: "/Users/kisi/Projeler/magaza/uploads",
                                            make: .finished(made), targets: targets, panelSite: "magaza", opened: opened)),
            ("archives-nomongo", ArchivesModel(tab: .make, databases: LocalDatabases(databases: [], mongoError: "Mac'teki MongoDB'ye bağlanılamadı (mongodb://127.0.0.1:27017): çalışıyor mu? brew services start mongodb-community@7.0"),
                                               opened: OpenedView())),
        ]
        for (name, model) in cases {
            if name == "archives-twodbs" {
                model.source = "magaza"
            }
            if name == "archives-made" {
                model.makeSite = "magaza"
            }
            try drawWindow(name, ArchivesWindow(model: model, actions: .none, now: Samples.now), size: ArchivesWindow.size)
        }
    }

    /// The Databases window in its states: nothing paired yet, a site being chosen,
    /// compared (differing: a collection counted only, a view left out; and the same),
    /// being compared, a pull and a send asked about, pulled, sent, failed, no MongoDB.
    func testDatabases() throws {
        let t = { (h: Double) in Samples.now.addingTimeInterval(-h * 3600) }
        let list = DBList(uri: "mongodb://127.0.0.1:27017", databases: [
            DBEntry(name: "blog_dev", bytes: 1_298_432),
            DBEntry(name: "dukkan", bytes: 45_178_880, pair: DBPair(server: "a", serverName: "A", site: "yag-dukkani")),
            DBEntry(name: "eski_proje", missing: true, pair: DBPair(server: "a", serverName: "A", site: "eski-proje")),
            DBEntry(name: "magaza_dev", bytes: 2_592_768, pair: DBPair(server: "h1", serverName: "H1", site: "magaza")),
            DBEntry(name: "ornek_sirket", bytes: 1_200_000),
            DBEntry(name: "magaza_dev_yedek_20261110_1200", bytes: 2_500_000, opened: true),
        ])
        let servers = [ServerSites(code: "a", name: "A", panel: "https://panel-a.example", sites: ["etiket", "kasa-pos", "magaza", "yag-dukkani"]),
                       ServerSites(code: "h1", name: "H1", panel: "https://panel-h1.example", sites: ["blog", "defter", "magaza", "ornek-sirket"])]
        let urunler = CollectionDiff(name: "urunler", local: 431, server: 431, same: 427, different: 4,
                                     localNewest: Newest(time: t(30), collection: "urunler", field: "updatedAt"),
                                     serverNewest: Newest(time: t(52), collection: "urunler", field: "updatedAt"),
                                     examples: DiffExamples(different: [#"{"$oid": "6a08d947286b694863255add"}"#]))
        let siparisler = CollectionDiff(name: "siparisler", local: 6_150, server: 6_192, same: 6_150, onlyServer: 42,
                                        localNewest: Newest(time: t(70), collection: "siparisler", field: "createdAt"),
                                        serverNewest: Newest(time: t(4), collection: "siparisler", field: "createdAt"))
        let diff = DBDiff(collections: [
            CollectionDiff(name: "ayarlar", local: 1, server: 1, same: 1),
            CollectionDiff(name: "denemeler", local: 12, server: -1, onlyLocal: 12),
            CollectionDiff(name: "kodlar", local: 40, server: 40, error: "belgeleri tek tek karşılaştırılamadı: _id'leri MongoDB'nin sırasıyla gelmedi (koleksiyonun kendi harf sıralaması ya da karışık _id türleri); yalnız belge sayıları"),
            siparisler, urunler,
        ], skipped: ["ucuz_urunler (görünüm, karşılaştırılmaz)"],
                          local: DiffSide(documents: 6_634, collections: 5, newest: urunler.localNewest),
                          serverSide: DiffSide(documents: 6_664, collections: 4, newest: siparisler.serverNewest),
                          same: 6_578, different: 4, onlyLocal: 12, onlyServer: 42, errors: 1, identical: false)
        let backup = DBBackup(kind: "hourly", time: t(3), bytes: 1_800_000)
        let compared = DBSyncResult(db: "magaza_dev", server: "h1", serverName: "H1", site: "magaza", backup: backup, diff: diff, tookMs: 9_800)
        let same = DBSyncResult(db: "magaza_dev", server: "h1", serverName: "H1", site: "magaza", backup: backup, diff: DBDiff(
            collections: [CollectionDiff(name: "siparisler", local: 6_192, server: 6_192, same: 6_192), CollectionDiff(name: "urunler", local: 431, server: 431, same: 431)],
            local: DiffSide(documents: 6_623, collections: 2, newest: siparisler.serverNewest), serverSide: DiffSide(documents: 6_623, collections: 2, newest: siparisler.serverNewest),
            same: 6_623, different: 0, onlyLocal: 0, onlyServer: 0, identical: true), tookMs: 7_100)
        let pulled = DBSyncResult(db: "magaza_dev", server: "h1", serverName: "H1", site: "magaza", backup: backup, copy: "magaza_dev_yedek_20261110_1200",
                                  loaded: LoadedDB(name: "magaza_dev", documents: 6_664, replaced: true), tookMs: 6_000)
        let sent = CreateResult(path: "/Users/kisi/Downloads/magaza-20261110-120000.footops.tar", site: "magaza", db: "magaza_dev", tookMs: 900,
                                importURL: "https://panel-h1.example/sites/magaza/import")
        let opened = OpenedView(folders: [], databases: [
            OpenedDatabase(name: "magaza_dev_yedek_20261110_1200", time: Samples.ago(30), at: Samples.ago(30), bytes: 2_500_000, copy: "magaza_dev"),
            OpenedDatabase(name: "blog_yedek_20261109_0300", server: "h1", serverName: "H1", site: "blog", time: t(30), at: t(200), bytes: 618_496, old: true),
        ])
        let one = ["magaza_dev": compared]
        let cases: [(String, DatabasesModel)] = [
            ("databases-unpaired", DatabasesModel(list: list, selected: "ornek_sirket", servers: servers, opened: opened)),
            ("databases-pairing", DatabasesModel(list: list, selected: "ornek_sirket", pairing: true, pairServer: "h1", pairSite: "ornek-sirket",
                                                 servers: servers, opened: opened)),
            ("databases-paired", DatabasesModel(list: list, selected: "magaza_dev", servers: servers, opened: OpenedView())),
            ("databases-compared", DatabasesModel(list: list, selected: "magaza_dev", compared: one, servers: servers, opened: opened)),
            ("databases-same", DatabasesModel(list: list, selected: "magaza_dev", compared: ["magaza_dev": same], servers: servers, opened: opened)),
            ("databases-comparing", DatabasesModel(list: list, selected: "magaza_dev",
                                                   operation: .running(job: .compare, db: "magaza_dev", part: "compare", done: 5_200, total: 13_298),
                                                   servers: servers, opened: opened)),
            ("databases-confirm-pull", DatabasesModel(list: list, selected: "magaza_dev", compared: one, confirm: .pull("magaza_dev"), servers: servers, opened: opened)),
            ("databases-confirm-send", DatabasesModel(list: list, selected: "magaza_dev", compared: one, confirm: .send("magaza_dev"), servers: servers, opened: opened)),
            ("databases-confirm-send-nopanel", DatabasesModel(list: list, selected: "magaza_dev", confirm: .send("magaza_dev"),
                                                              servers: [ServerSites(code: "h1", name: "H1", sites: ["magaza"])], opened: opened)),
            ("databases-pulling", DatabasesModel(list: list, selected: "magaza_dev",
                                                 operation: .running(job: .pull, db: "magaza_dev", part: "copy", done: 1_200_000, total: 2_400_000),
                                                 servers: servers, opened: opened)),
            ("databases-replacing", DatabasesModel(list: list, selected: "magaza_dev",
                                                   operation: .running(job: .pull, db: "magaza_dev", part: "load", done: 900_000, total: 1_800_000),
                                                   servers: servers, opened: opened)),
            ("databases-pulled", DatabasesModel(list: list, selected: "magaza_dev", operation: .pulled(pulled), servers: servers, opened: opened)),
            ("databases-sent", DatabasesModel(list: list, selected: "magaza_dev", operation: .sent(db: "magaza_dev", sent), compared: one, servers: servers, opened: opened)),
            ("databases-failed", DatabasesModel(list: list, selected: "magaza_dev", operation: .failed(db: "magaza_dev", job: .pull,
                message: "mongorestore başarısız: stream or file does not appear to be a mongodump archive — yereldeki magaza_dev olduğu gibi kaldı"),
                                                servers: servers, opened: opened)),
            ("databases-none", DatabasesModel(list: list, servers: servers, opened: opened)),
            ("databases-nomongo", DatabasesModel(list: DBList(databases: [], mongoError: "Mac'teki MongoDB'ye bağlanılamadı (mongodb://127.0.0.1:27017): çalışıyor mu? brew services start mongodb-community@7.0"),
                                                 servers: servers, opened: OpenedView())),
        ]
        for (name, model) in cases {
            try drawWindow(name, DatabasesWindow(model: model, actions: .none, now: Samples.now), size: DatabasesWindow.size)
        }
    }

    /// The four icons on a light and a dark menu bar, in one strip.
    func testIcons() throws {
        let cell = NSSize(width: 44, height: 24)
        let scale: CGFloat = 4
        let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: Int(cell.width * 4 * scale), pixelsHigh: Int(cell.height * 2 * scale),
                                   bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB,
                                   bytesPerRow: 0, bitsPerPixel: 0)!
        rep.size = NSSize(width: cell.width * 4, height: cell.height * 2)
        NSGraphicsContext.saveGraphicsState()
        NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
        for (row, dark) in [false, true].enumerated() {
            let y = CGFloat(1 - row) * cell.height
            (dark ? NSColor(white: 0.16, alpha: 1) : NSColor(white: 0.92, alpha: 1)).setFill()
            NSRect(x: 0, y: y, width: cell.width * 4, height: cell.height).fill()
            for (col, light) in Light.allCases.enumerated() {
                let image = StatusIcon.image(light)
                XCTAssertGreaterThan(image.size.width, 10, "\(light)")
                NSAppearance(named: dark ? .darkAqua : .aqua)!.performAsCurrentDrawingAppearance {
                    // A template is the menu bar's to colour: drawn as it would, dimmed.
                    let shown = image.isTemplate ? StatusIcon.tinted(image, dark ? .white : .black) : image
                    shown.draw(in: NSRect(x: CGFloat(col) * cell.width + (cell.width - image.size.width) / 2,
                                          y: y + (cell.height - image.size.height) / 2, width: image.size.width, height: image.size.height),
                               from: .zero, operation: .sourceOver, fraction: image.isTemplate ? 0.35 : 1)
                }
            }
        }
        NSGraphicsContext.restoreGraphicsState()
        if let folder {
            try rep.representation(using: .png, properties: [:])!.write(to: folder.appendingPathComponent("icons.png"))
        }
    }

    /// A view with AppKit's controls in it (a form, text fields) in a window of its own,
    /// drawn the way the window server would: ImageRenderer leaves such controls out.
    private func drawWindow<V: View>(_ name: String, _ view: V, size: NSSize) throws {
        for dark in [false, true] {
            let appearance = NSAppearance(named: dark ? .darkAqua : .aqua)!
            let host = NSHostingView(rootView: view.frame(width: size.width, height: size.height))
            host.frame = NSRect(origin: .zero, size: size)
            _ = NSApplication.shared
            let window = NSWindow(contentRect: host.frame, styleMask: [.borderless], backing: .buffered, defer: false)
            window.isReleasedWhenClosed = false
            window.appearance = appearance
            window.contentView = host
            host.layoutSubtreeIfNeeded()
            RunLoop.current.run(until: Date().addingTimeInterval(0.3))
            guard let rep = host.bitmapImageRepForCachingDisplay(in: host.bounds) else {
                XCTFail("\(name): çizilemedi")
                return
            }
            host.cacheDisplay(in: host.bounds, to: rep)
            window.close()
            guard let folder else { continue }
            try rep.representation(using: .png, properties: [:])!
                .write(to: folder.appendingPathComponent("\(name)-\(dark ? "dark" : "light").png"))
        }
    }

    private func draw<V: View>(_ name: String, _ view: V, background: Bool = false) throws {
        for dark in [false, true] {
            let scheme: ColorScheme = dark ? .dark : .light
            let content = view
                .background(background ? Color(nsColor: .windowBackgroundColor) : Color(nsColor: .controlBackgroundColor))
                .environment(\.colorScheme, scheme)
            let renderer = ImageRenderer(content: content)
            renderer.scale = 2
            guard let image = renderer.cgImage else {
                XCTFail("\(name): çizilemedi")
                return
            }
            XCTAssertGreaterThan(image.height, 100, name)
            guard let folder else { continue }
            let rep = NSBitmapImageRep(cgImage: image)
            try rep.representation(using: .png, properties: [:])!
                .write(to: folder.appendingPathComponent("\(name)-\(dark ? "dark" : "light").png"))
        }
    }
}
