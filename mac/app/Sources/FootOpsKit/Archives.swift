import AppKit
import Foundation
import Observation
import os

// What footops-mac arsiv … writes with --json (panel-tasarim §29.15), and the Archives
// window's state. The core keeps the rules (what an archive may hold, where it opens, how
// one is made); the app only asks and shows. Fields left out are read as empty.

/// arsiv dogrula: what checking an archive found.
public struct ArchiveInfo: Decodable, Sendable, Equatable {
    public var path: String
    public var name: String
    /// footops, mongodump, files.
    public var kind: String
    public var size: Int64
    public var sha256: String
    /// A FootOps archive's manifest: the site it was made of, when, by which FootOps.
    public var site: String?
    public var created: Date?
    public var footops: String?
    public var backup: ArchiveBackup?
    public var database: ArchiveDB?
    public var files: ArchiveFiles?
    /// The site's environment is in it (its secrets too).
    public var env: Bool
    public var envVars: Int
    public var notes: [String]
    public var tookMs: Int

    public init(path: String, kind: String, size: Int64, sha256: String = "", site: String? = nil, created: Date? = nil,
                footops: String? = nil, backup: ArchiveBackup? = nil, database: ArchiveDB? = nil, files: ArchiveFiles? = nil,
                env: Bool = false, envVars: Int = 0, notes: [String] = [], tookMs: Int = 0) {
        self.path = path
        name = (path as NSString).lastPathComponent
        self.kind = kind
        self.size = size
        self.sha256 = sha256
        self.site = site
        self.created = created
        self.footops = footops
        self.backup = backup
        self.database = database
        self.files = files
        self.env = env
        self.envVars = envVars
        self.notes = notes
        self.tookMs = tookMs
    }

    enum CodingKeys: String, CodingKey {
        case path, name, kind, size, sha256, site, created, footops, backup, database, files, env, envVars, notes, tookMs
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        path = try c.decode(String.self, forKey: .path)
        name = try c.or(.name, (path as NSString).lastPathComponent)
        kind = try c.or(.kind, "")
        size = try c.or(.size, 0)
        sha256 = try c.or(.sha256, "")
        site = try c.decodeIfPresent(String.self, forKey: .site)
        created = try c.decodeIfPresent(Date.self, forKey: .created)
        footops = try c.decodeIfPresent(String.self, forKey: .footops)
        backup = try c.decodeIfPresent(ArchiveBackup.self, forKey: .backup)
        database = try c.decodeIfPresent(ArchiveDB.self, forKey: .database)
        files = try c.decodeIfPresent(ArchiveFiles.self, forKey: .files)
        env = try c.or(.env, false)
        envVars = try c.or(.envVars, 0)
        notes = try c.or(.notes, [])
        tookMs = try c.or(.tookMs, 0)
    }

    public var kindText: String {
        switch kind {
        case "footops": "FootOps arşivi"
        case "mongodump": "mongodump arşivi"
        case "files": "dosya arşivi"
        default: kind
        }
    }

    /// The parts it can be opened into a folder as (db, files, def); a FootOps archive's
    /// only.
    public var parts: [String] {
        guard kind == "footops" else { return [] }
        return [database != nil ? "db" : nil, files != nil ? "files" : nil, env ? "def" : nil].compactMap { $0 }
    }
}

/// The backup a FootOps archive was made from (the panel's İndir).
public struct ArchiveBackup: Decodable, Sendable, Equatable {
    public var server: String
    public var kind: String
    public var time: Date

    public init(server: String, kind: String, time: Date) {
        self.server = server
        self.kind = kind
        self.time = time
    }

    enum CodingKeys: String, CodingKey { case server, kind, time }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        server = try c.or(.server, "")
        kind = try c.or(.kind, "")
        time = try c.decode(Date.self, forKey: .time)
    }
}

/// An archive's database: what the dump says of itself and, in a FootOps archive, the
/// manifest's counts (documents -1: not known).
public struct ArchiveDB: Decodable, Sendable, Equatable {
    /// The database loaded; nil: the dump holds several, one is chosen.
    public var name: String?
    /// The dump's databases (the first hundred; moreDatabases the rest: the Terminal's
    /// --kaynak takes any of them).
    public var databases: [String]
    public var moreDatabases: Int
    public var collections: [ArchiveCollection]
    public var more: Int
    public var documents: Int64
    public var server: String
    public var bytes: Int64
    public var compressed: Int64

    public init(name: String?, databases: [String], moreDatabases: Int = 0, collections: [ArchiveCollection], more: Int = 0,
                documents: Int64 = -1, server: String = "", bytes: Int64 = 0, compressed: Int64 = 0) {
        self.name = name
        self.databases = databases
        self.moreDatabases = moreDatabases
        self.collections = collections
        self.more = more
        self.documents = documents
        self.server = server
        self.bytes = bytes
        self.compressed = compressed
    }

    enum CodingKeys: String, CodingKey { case name, databases, moreDatabases, collections, more, documents, server, bytes, compressed }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decodeIfPresent(String.self, forKey: .name)
        databases = try c.or(.databases, [])
        moreDatabases = try c.or(.moreDatabases, 0)
        collections = try c.or(.collections, [])
        more = try c.or(.more, 0)
        documents = try c.or(.documents, -1)
        server = try c.or(.server, "")
        bytes = try c.or(.bytes, 0)
        compressed = try c.or(.compressed, 0)
    }
}

public struct ArchiveCollection: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { db + "." + name }
    public var db: String
    public var name: String
    public var view: Bool
    /// -1: not known.
    public var documents: Int64

    public init(db: String, name: String, view: Bool = false, documents: Int64 = -1) {
        self.db = db
        self.name = name
        self.view = view
        self.documents = documents
    }

    enum CodingKeys: String, CodingKey { case db, name, view, documents }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        db = try c.or(.db, "")
        name = try c.decode(String.self, forKey: .name)
        view = try c.or(.view, false)
        documents = try c.or(.documents, -1)
    }
}

/// An archive's files: all of them, hidden ones (never imported) included.
public struct ArchiveFiles: Decodable, Sendable, Equatable {
    public var files: Int
    public var dirs: Int
    public var bytes: Int64
    public var hidden: Int
    public var hiddenBytes: Int64
    public var compressed: Int64

    public init(files: Int, dirs: Int, bytes: Int64, hidden: Int = 0, hiddenBytes: Int64 = 0, compressed: Int64 = 0) {
        self.files = files
        self.dirs = dirs
        self.bytes = bytes
        self.hidden = hidden
        self.hiddenBytes = hiddenBytes
        self.compressed = compressed
    }

    enum CodingKeys: String, CodingKey { case files, dirs, bytes, hidden, hiddenBytes, compressed }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        files = try c.or(.files, 0)
        dirs = try c.or(.dirs, 0)
        bytes = try c.or(.bytes, 0)
        hidden = try c.or(.hidden, 0)
        hiddenBytes = try c.or(.hiddenBytes, 0)
        compressed = try c.or(.compressed, 0)
    }
}

/// arsiv olustur: the archive made (archive: what checking it found), and its start.
public struct CreateResult: Decodable, Sendable, Equatable {
    public var path: String
    public var site: String
    public var db: String?
    public var folder: String?
    public var archive: ArchiveInfo?
    /// The folder's entries left out: hidden files, links, names an import refuses.
    public var skipped: Int
    public var warnings: [String]
    public var tookMs: Int
    /// The import page on the panel it was made for (veritabani gonder).
    public var importURL: String?

    public init(path: String, site: String, db: String? = nil, folder: String? = nil, archive: ArchiveInfo? = nil, skipped: Int = 0,
                warnings: [String] = [], tookMs: Int = 0, importURL: String? = nil) {
        self.path = path
        self.site = site
        self.db = db
        self.folder = folder
        self.archive = archive
        self.skipped = skipped
        self.warnings = warnings
        self.tookMs = tookMs
        self.importURL = importURL
    }

    enum CodingKeys: String, CodingKey {
        case path, site, db, folder, archive, skipped, warnings, tookMs
        case importURL = "import"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        path = try c.or(.path, "")
        site = try c.or(.site, "")
        db = try c.decodeIfPresent(String.self, forKey: .db)
        folder = try c.decodeIfPresent(String.self, forKey: .folder)
        archive = try c.decodeIfPresent(ArchiveInfo.self, forKey: .archive)
        skipped = try c.or(.skipped, 0)
        warnings = try c.or(.warnings, [])
        tookMs = try c.or(.tookMs, 0)
        importURL = try c.decodeIfPresent(String.self, forKey: .importURL)
    }
}

/// A line of arsiv dogrula --json or arsiv olustur --json.
enum ArchiveEvent: Decodable {
    case progress(part: String, done: Int64, total: Int64)
    case checked(ArchiveInfo)
    case started(CreateResult)
    case made(CreateResult)
    case error(String)

    enum CodingKeys: String, CodingKey { case event, part, done, total, message, kind }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        switch try c.decode(String.self, forKey: .event) {
        case "progress":
            self = .progress(part: try c.or(.part, ""), done: try c.or(.done, 0), total: try c.or(.total, 0))
        case "start":
            self = .started(try CreateResult(from: decoder))
        case "result":
            // dogrula's names its kind; olustur's is the archive made.
            self = c.contains(.kind) ? .checked(try ArchiveInfo(from: decoder)) : .made(try CreateResult(from: decoder))
        case "error":
            self = .error(try c.or(.message, "bilinmeyen hata"))
        case let other:
            throw DecodingError.dataCorruptedError(forKey: .event, in: c, debugDescription: "bilinmeyen olay: \(other)")
        }
    }
}

/// A database of the Mac's MongoDB (arsiv veritabanlari); opened: a backup or an archive
/// was loaded into it.
public struct LocalDatabase: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { name }
    public var name: String
    public var bytes: Int64
    public var opened: Bool

    public init(name: String, bytes: Int64, opened: Bool = false) {
        self.name = name
        self.bytes = bytes
        self.opened = opened
    }

    enum CodingKeys: String, CodingKey { case name, bytes, opened }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decode(String.self, forKey: .name)
        bytes = try c.or(.bytes, 0)
        opened = try c.or(.opened, false)
    }
}

public struct LocalDatabases: Decodable, Sendable, Equatable {
    public var uri: String
    public var databases: [LocalDatabase]
    /// The Mac's MongoDB could not be asked.
    public var mongoError: String?

    public init(uri: String = "", databases: [LocalDatabase], mongoError: String? = nil) {
        self.uri = uri
        self.databases = databases
        self.mongoError = mongoError
    }

    enum CodingKeys: String, CodingKey { case uri, databases, mongoError }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        uri = try c.or(.uri, "")
        databases = try c.or(.databases, [])
        mongoError = try c.decodeIfPresent(String.self, forKey: .mongoError)
    }
}

/// A server whose panel the archive can be imported on: its address (the settings') and
/// its sites (its status file's).
public struct PanelTarget: Sendable, Equatable, Identifiable {
    public var id: String { code }
    public var code: String
    public var name: String
    public var panel: String
    public var sites: [String]

    public init(code: String, name: String, panel: String, sites: [String]) {
        self.code = code
        self.name = name
        self.panel = panel
        self.sites = sites
    }

    /// The site's import page.
    public func importURL(site: String) -> URL? {
        let base = panel.hasSuffix("/") ? String(panel.dropLast()) : panel
        guard let escaped = site.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) else { return nil }
        return URL(string: base + "/sites/" + escaped + "/import")
    }
}

// MARK: - the window's state

/// The Archives window's state (panel-tasarim §29.6): an archive checked (arsiv dogrula),
/// then opened into a folder (arsiv ac) or its database loaded into the Mac's MongoDB
/// (arsiv mongo), one at a time, stoppable; an archive made of a database of the Mac's
/// MongoDB and a folder (arsiv olustur); the panel's import page for it; what was opened
/// on this Mac (shared with the Backups window).
@MainActor @Observable
public final class ArchivesModel {
    public enum Tab: Sendable {
        case open, make
    }

    public enum Check: Equatable, Sendable {
        case none
        case checking(path: String, done: Int64, total: Int64)
        case checked(ArchiveInfo)
        case failed(path: String, message: String)
    }

    public enum Make: Equatable, Sendable {
        case idle
        case running(part: String?, done: Int64, total: Int64)
        case finished(CreateResult)
        case failed(String)
    }

    public var tab: Tab = .open
    public private(set) var check: Check = .none
    /// The parts to open into a folder (db, files, def).
    public var parts: Set<String> = []
    /// The dump's database to load, when it holds several.
    public var source: String?
    public private(set) var operation: BackupsModel.Operation = .idle
    /// The name to load the database under when the default one is taken.
    public var otherName = ""

    public private(set) var databases: LocalDatabases?
    public private(set) var databasesError: String?
    public private(set) var readingDatabases = false
    /// What to make the archive of: a database of the Mac's MongoDB, a folder, or both.
    public var makeDB: String?
    public var makeFolder: String?
    /// The site's name for the archive ("": the core makes one of the database's).
    public var makeSite = ""
    public private(set) var make: Make = .idle

    /// Where to import it: a server's panel, a site of it.
    public var panelServer: String?
    public var panelSite: String?
    public private(set) var targets: [PanelTarget] = []

    public let opened: OpenedList

    @ObservationIgnored private let core: Core?
    @ObservationIgnored private var task: Task<Void, Never>?
    @ObservationIgnored private var asked: (path: String, source: String?)?
    @ObservationIgnored private let log = Logger(subsystem: "io.github.swbilisim.footops", category: "arsiv")

    /// How long a check, an open or a make may take before the app gives up on it
    /// (stoppable before that); stopped, how long the core has to remove what it began.
    static let limit: Duration = .seconds(6 * 3600)
    static let grace: Duration = .seconds(60)

    public init(core: Core?, opened: OpenedList? = nil, targets: [PanelTarget] = []) {
        self.core = core
        self.opened = opened ?? OpenedList(core: core)
        self.targets = targets
        panelServer = targets.first?.code
    }

    /// A model holding what it is given (the image tests').
    public init(tab: Tab = .open, check: Check = .none, operation: BackupsModel.Operation = .idle, databases: LocalDatabases? = nil,
                makeDB: String? = nil, makeFolder: String? = nil, make: Make = .idle, targets: [PanelTarget] = [],
                panelSite: String? = nil, opened: OpenedView? = nil) {
        core = nil
        self.opened = OpenedList(view: opened)
        self.tab = tab
        self.check = check
        self.operation = operation
        self.databases = databases
        self.makeDB = makeDB
        self.makeFolder = makeFolder
        self.make = make
        self.targets = targets
        panelServer = targets.first?.code
        self.panelSite = panelSite
        if case .checked(let info) = check {
            parts = Set(info.parts)
            source = info.database?.name
        }
    }

    // MARK: what is shown

    public var info: ArchiveInfo? {
        if case .checked(let i) = check {
            return i
        }
        return nil
    }

    /// Something runs: a check, an open, a make.
    public var isBusy: Bool {
        if case .checking = check {
            return true
        }
        if case .running = operation {
            return true
        }
        if case .running = make {
            return true
        }
        return false
    }

    /// The archive to import on a panel: the one checked, or the one made.
    public var importable: String? {
        if tab == .make, case .finished(let r) = make {
            return r.path
        }
        if tab == .open, let info {
            return info.path
        }
        return nil
    }

    public var target: PanelTarget? { targets.first { $0.code == panelServer } }

    // MARK: reading

    /// The window opens: what was opened, the Mac's databases.
    public func load() async {
        async let opened: Void = self.opened.read()
        await readDatabases()
        await opened
    }

    /// The panels and their sites anew (the menu's latest status).
    public func update(targets: [PanelTarget]) {
        self.targets = targets
        if panelServer == nil || !targets.contains(where: { $0.code == panelServer }) {
            panelServer = targets.first?.code
        }
        if let s = panelSite, target?.sites.contains(s) != true {
            panelSite = nil
        }
    }

    public func select(server code: String) {
        guard code != panelServer else { return }
        panelServer = code
        panelSite = nil
    }

    public func refreshDatabases() {
        Task { await readDatabases() }
    }

    func readDatabases() async {
        guard let core, !readingDatabases else { return }
        readingDatabases = true
        defer { readingDatabases = false }
        do {
            let out = try await core.run(["arsiv", "veritabanlari", "--json"], timeout: .seconds(30))
            switch Core.decode(LocalDatabases.self, from: out) {
            case .success(let d):
                databases = d
                databasesError = d.mongoError
                if let db = makeDB, !d.databases.contains(where: { $0.name == db }) {
                    makeDB = nil
                }
            case .failure(let m):
                databasesError = m.text
            }
        } catch Core.Failure.stopped {
            return
        } catch {
            databasesError = error.localizedDescription
        }
    }

    // MARK: checking

    /// Checks an archive (dropped on the window, or chosen): what it is, what is in it.
    public func choose(file path: String) {
        guard let core, !isBusy else { return }
        tab = .open
        check = .checking(path: path, done: 0, total: 0)
        operation = .idle
        asked = nil
        log.info("denetleniyor: \(path, privacy: .public)")
        run(core, ["arsiv", "dogrula", path, "--json"]) { [weak self] ev in
            guard let self, case .checking(let p, _, _) = self.check, case .progress(_, let done, let total) = ev else { return }
            self.check = .checking(path: p, done: done, total: total)
        } done: { [weak self] outcome in
            guard let self else { return }
            switch outcome {
            case .checked(let info)?:
                self.check = .checked(info)
                self.parts = Set(info.parts)
                self.source = info.database?.name
            case .error(let m)?:
                self.check = m == "durduruldu" ? .none : .failed(path: path, message: m)
            default:
                self.check = .failed(path: path, message: "footops-mac'in yanıtı anlaşılamadı")
            }
        }
    }

    // MARK: opening

    /// Opens the checked archive's ticked parts into a folder (~/Downloads/<site>-<time>).
    public func openFolder() {
        guard let info, info.kind == "footops", !parts.isEmpty else { return }
        var args = ["arsiv", "ac", info.path]
        for (part, flag) in [("db", "--db"), ("files", "--dosyalar"), ("def", "--ayarlar")] where parts.contains(part) {
            args.append(flag)
        }
        start(args + ["--json"], mongo: false, path: info.path, source: nil)
    }

    /// Loads the checked archive's database into the Mac's MongoDB under its default name;
    /// of a dump holding several, the one chosen.
    public func loadDatabase() {
        guard let info, let db = info.database else { return }
        let from = db.name == nil ? source : nil
        guard db.name != nil || from != nil else { return }
        load(path: info.path, source: from, name: nil, overwrite: false)
    }

    /// Answers the question about a taken name, for the archive it was asked about.
    public func retryLoad(name: String, overwrite: Bool) {
        guard case .asking = operation, let asked else { return }
        load(path: asked.path, source: asked.source, name: name, overwrite: overwrite)
    }

    private func load(path: String, source: String?, name: String?, overwrite: Bool) {
        var args = ["arsiv", "mongo", path]
        if let source {
            args += ["--kaynak", source]
        }
        if let name, !name.isEmpty {
            args += ["--ad", name]
        }
        if overwrite {
            args.append("--uzerine")
        }
        start(args + ["--json"], mongo: true, path: path, source: source)
    }

    /// Gives up asking about the taken name.
    public func dismiss() {
        asked = nil
        operation = .idle
    }

    private func start(_ args: [String], mongo: Bool, path: String, source: String?) {
        guard let core, task == nil else { return }
        asked = (path, source)
        let site = info?.site ?? info?.name ?? ""
        operation = .running(BackupsModel.Running(mongo: mongo, site: site))
        log.info("açılıyor: \(args.dropLast().joined(separator: " "), privacy: .public)")
        task = Task {
            defer { task = nil }
            do {
                let out = try await core.stream(args, timeout: Self.limit, grace: Self.grace) { line in
                    guard let ev = try? CoreJSON.decoder().decode(OpenEvent.self, from: line) else { return }
                    DispatchQueue.main.async {
                        MainActor.assumeIsolated { self.progress(ev) }
                    }
                }
                finish(BackupsModel.outcome(of: out))
            } catch Core.Failure.stopped {
                operation = .idle
                asked = nil
            } catch {
                operation = .failed(error.localizedDescription)
            }
        }
    }

    private func progress(_ ev: OpenEvent) {
        guard case .running(var r) = operation else { return }
        switch ev {
        case .start(let res):
            r.target = res.target
        case .progress(let part, let done, let total):
            r.part = part.isEmpty ? nil : part
            r.done = done
            r.total = total
        case .result, .error:
            return
        }
        operation = .running(r)
    }

    private func finish(_ ev: OpenEvent?) {
        switch ev {
        case .result(let res):
            operation = .finished(res)
            asked = nil
            log.info("açıldı: \(res.archive ?? "", privacy: .public), \(res.tookMs) ms")
            Task { await opened.read() }
        case .error(_, "db_exists", let name?):
            otherName = name + "_2"
            operation = .asking(name: name)
        case .error(let message, _, _):
            operation = message == "durduruldu" ? .idle : .failed(message)
            asked = nil
        default:
            operation = .failed("footops-mac'in yanıtı anlaşılamadı")
            asked = nil
        }
    }

    // MARK: making

    /// Makes an archive of the database and / or the folder chosen (~/Downloads/<site>-<time>.footops.tar).
    public func makeArchive() {
        guard let core, !isBusy, makeDB != nil || makeFolder != nil else { return }
        // A question about a taken name, left open, could not be answered while this runs.
        if case .asking = operation {
            dismiss()
        }
        var args = ["arsiv", "olustur"]
        if let db = makeDB {
            args += ["--db", db]
        }
        if let folder = makeFolder {
            args += ["--dosyalar", folder]
        }
        let site = makeSite.trimmingCharacters(in: .whitespaces)
        if !site.isEmpty {
            args += ["--site", site]
        }
        make = .running(part: nil, done: 0, total: 0)
        log.info("üretiliyor: \(args.joined(separator: " "), privacy: .public)")
        run(core, args + ["--json"]) { [weak self] ev in
            guard let self, case .running = self.make, case .progress(let part, let done, let total) = ev else { return }
            self.make = .running(part: part.isEmpty ? nil : part, done: done, total: total)
        } done: { [weak self] outcome in
            guard let self else { return }
            switch outcome {
            case .made(let res)?:
                self.make = .finished(res)
                if self.panelSite == nil, self.target?.sites.contains(res.site) == true {
                    self.panelSite = res.site
                }
                Task { await self.readDatabases() }
            case .error(let m)?:
                self.make = m == "durduruldu" ? .idle : .failed(m)
            default:
                self.make = .failed("footops-mac'in yanıtı anlaşılamadı")
            }
        }
    }

    // MARK: running

    /// Runs a command whose lines are ArchiveEvents: each progress to onEvent, the outcome
    /// (its last result or error) to done.
    private func run(_ core: Core, _ args: [String], onEvent: @escaping @MainActor (ArchiveEvent) -> Void,
                     done: @escaping @MainActor (ArchiveEvent?) -> Void) {
        guard task == nil else { return }
        task = Task {
            defer { task = nil }
            do {
                let out = try await core.stream(args, timeout: Self.limit, grace: Self.grace) { line in
                    guard let ev = try? CoreJSON.decoder().decode(ArchiveEvent.self, from: line) else { return }
                    DispatchQueue.main.async {
                        MainActor.assumeIsolated { onEvent(ev) }
                    }
                }
                done(Self.outcome(of: out))
            } catch Core.Failure.stopped {
                done(.error("durduruldu"))
            } catch {
                done(.error(error.localizedDescription))
            }
        }
    }

    /// The last result or error of a command's output.
    nonisolated static func outcome(of out: Core.Output) -> ArchiveEvent? {
        let decoder = CoreJSON.decoder()
        var last: ArchiveEvent?
        for line in out.stdout.split(separator: 0x0A) {
            switch try? decoder.decode(ArchiveEvent.self, from: Data(line)) {
            case let ev? where !ev.isProgress:
                if case .started = ev {
                    continue
                }
                last = ev
            default:
                break
            }
        }
        if last == nil {
            let said = out.stderr.split(separator: "\n").first.map(String.init) ?? ""
            return .error("footops-mac'in yanıtı anlaşılamadı (çıkış kodu \(out.status))" + (said.isEmpty ? "" : ": \(said.prefix(300))"))
        }
        return last
    }

    /// Stops what runs (the core removes what it began).
    public func stop() {
        task?.cancel()
    }

    /// Stops what runs and waits for the core to have removed what it began (the app
    /// quits), at most limit.
    public func stopAndWait(limit: Duration) async {
        guard let task else { return }
        task.cancel()
        let clock = ContinuousClock()
        let deadline = clock.now + limit
        while self.task != nil, clock.now < deadline {
            try? await Task.sleep(for: .milliseconds(100))
        }
    }
}

extension ArchiveEvent {
    var isProgress: Bool {
        if case .progress = self {
            return true
        }
        return false
    }
}
