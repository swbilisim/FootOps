import Foundation
import Observation
import os

// What footops-mac veritabani … writes with --json (panel-tasarim §33.4), and the
// Databases window's state: a database of the Mac's MongoDB paired once with a site,
// compared with the site's latest backup, sent to its panel's import page, or pulled in
// its place. The core keeps the rules and does the work; the app asks and shows.

/// The site a database of the Mac's is paired with; serverName as the Mac calls it.
public struct DBPair: Decodable, Sendable, Equatable {
    public var server: String
    public var serverName: String
    public var site: String

    public init(server: String, serverName: String, site: String) {
        self.server = server
        self.serverName = serverName
        self.site = site
    }

    enum CodingKeys: String, CodingKey { case server, serverName, site }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        server = try c.decode(String.self, forKey: .server)
        serverName = try c.or(.serverName, server.uppercased())
        site = try c.decode(String.self, forKey: .site)
    }
}

/// A database of the Mac's MongoDB (opened: one footops-mac loaded or copied), or a
/// paired one not there (missing).
public struct DBEntry: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { name }
    public var name: String
    public var bytes: Int64
    public var opened: Bool
    public var missing: Bool
    public var pair: DBPair?

    public init(name: String, bytes: Int64 = 0, opened: Bool = false, missing: Bool = false, pair: DBPair? = nil) {
        self.name = name
        self.bytes = bytes
        self.opened = opened
        self.missing = missing
        self.pair = pair
    }

    enum CodingKeys: String, CodingKey { case name, bytes, opened, missing, pair }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decode(String.self, forKey: .name)
        bytes = try c.or(.bytes, 0)
        opened = try c.or(.opened, false)
        missing = try c.or(.missing, false)
        pair = try c.decodeIfPresent(DBPair.self, forKey: .pair)
    }
}

/// veritabani liste.
public struct DBList: Decodable, Sendable, Equatable {
    public var uri: String
    public var databases: [DBEntry]
    /// The Mac's MongoDB could not be asked; the pairings could not be read.
    public var mongoError: String?
    public var pairsError: String?

    public init(uri: String = "", databases: [DBEntry], mongoError: String? = nil, pairsError: String? = nil) {
        self.uri = uri
        self.databases = databases
        self.mongoError = mongoError
        self.pairsError = pairsError
    }

    enum CodingKeys: String, CodingKey { case uri, databases, mongoError, pairsError }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        uri = try c.or(.uri, "")
        databases = try c.or(.databases, [])
        mongoError = try c.decodeIfPresent(String.self, forKey: .mongoError)
        pairsError = try c.decodeIfPresent(String.self, forKey: .pairsError)
    }

    /// The owner's own databases: not footops-mac's loads and copies.
    public var own: [DBEntry] { databases.filter { !$0.opened } }
}

/// A side's newest record: the latest of its documents' times and where.
public struct Newest: Decodable, Sendable, Equatable {
    public var time: Date
    public var collection: String
    public var field: String

    public init(time: Date, collection: String = "", field: String) {
        self.time = time
        self.collection = collection
        self.field = field
    }

    enum CodingKeys: String, CodingKey { case time, collection, field }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        time = try c.decode(Date.self, forKey: .time)
        collection = try c.or(.collection, "")
        field = try c.or(.field, "")
    }

    /// "orders.updatedAt"
    public var place: String { collection.isEmpty ? field : collection + "." + field }
}

/// A few _ids of each kind, to look them up.
public struct DiffExamples: Decodable, Sendable, Equatable {
    public var different: [String]
    public var onlyLocal: [String]
    public var onlyServer: [String]

    public init(different: [String] = [], onlyLocal: [String] = [], onlyServer: [String] = []) {
        self.different = different
        self.onlyLocal = onlyLocal
        self.onlyServer = onlyServer
    }

    enum CodingKeys: String, CodingKey { case different, onlyLocal, onlyServer }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        different = try c.or(.different, [])
        onlyLocal = try c.or(.onlyLocal, [])
        onlyServer = try c.or(.onlyServer, [])
    }
}

/// One collection compared; local / server -1: not on that side; error: counted only.
public struct CollectionDiff: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { name }
    public var name: String
    public var local: Int64
    public var server: Int64
    public var same: Int64
    public var different: Int64
    public var onlyLocal: Int64
    public var onlyServer: Int64
    public var localNewest: Newest?
    public var serverNewest: Newest?
    public var examples: DiffExamples?
    public var error: String?

    public init(name: String, local: Int64, server: Int64, same: Int64 = 0, different: Int64 = 0, onlyLocal: Int64 = 0, onlyServer: Int64 = 0,
                localNewest: Newest? = nil, serverNewest: Newest? = nil, examples: DiffExamples? = nil, error: String? = nil) {
        self.name = name
        self.local = local
        self.server = server
        self.same = same
        self.different = different
        self.onlyLocal = onlyLocal
        self.onlyServer = onlyServer
        self.localNewest = localNewest
        self.serverNewest = serverNewest
        self.examples = examples
        self.error = error
    }

    enum CodingKeys: String, CodingKey {
        case name, local, server, same, different, onlyLocal, onlyServer, localNewest, serverNewest, examples, error
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decode(String.self, forKey: .name)
        local = try c.or(.local, -1)
        server = try c.or(.server, -1)
        same = try c.or(.same, 0)
        different = try c.or(.different, 0)
        onlyLocal = try c.or(.onlyLocal, 0)
        onlyServer = try c.or(.onlyServer, 0)
        localNewest = try c.decodeIfPresent(Newest.self, forKey: .localNewest)
        serverNewest = try c.decodeIfPresent(Newest.self, forKey: .serverNewest)
        examples = try c.decodeIfPresent(DiffExamples.self, forKey: .examples)
        error = try c.decodeIfPresent(String.self, forKey: .error)
    }

    /// Something differs (or could not be compared).
    public var differs: Bool { different + onlyLocal + onlyServer > 0 || error != nil || local < 0 || server < 0 }
}

public struct DiffSide: Decodable, Sendable, Equatable {
    public var documents: Int64
    public var collections: Int
    public var newest: Newest?

    public init(documents: Int64, collections: Int, newest: Newest? = nil) {
        self.documents = documents
        self.collections = collections
        self.newest = newest
    }

    enum CodingKeys: String, CodingKey { case documents, collections, newest }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        documents = try c.or(.documents, 0)
        collections = try c.or(.collections, 0)
        newest = try c.decodeIfPresent(Newest.self, forKey: .newest)
    }
}

/// Two databases compared: collection by collection, and the whole.
public struct DBDiff: Decodable, Sendable, Equatable {
    public var collections: [CollectionDiff]
    /// Collections past the most compared; views and time series, not compared.
    public var more: Int
    public var skipped: [String]
    public var local: DiffSide
    public var serverSide: DiffSide
    public var same: Int64
    public var different: Int64
    public var onlyLocal: Int64
    public var onlyServer: Int64
    /// Collections counted only.
    public var errors: Int
    public var identical: Bool

    public init(collections: [CollectionDiff], more: Int = 0, skipped: [String] = [], local: DiffSide, serverSide: DiffSide, same: Int64,
                different: Int64, onlyLocal: Int64, onlyServer: Int64, errors: Int = 0, identical: Bool) {
        self.collections = collections
        self.more = more
        self.skipped = skipped
        self.local = local
        self.serverSide = serverSide
        self.same = same
        self.different = different
        self.onlyLocal = onlyLocal
        self.onlyServer = onlyServer
        self.errors = errors
        self.identical = identical
    }

    enum CodingKeys: String, CodingKey {
        case collections, more, skipped, local, serverSide, same, different, onlyLocal, onlyServer, errors, identical
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        collections = try c.or(.collections, [])
        more = try c.or(.more, 0)
        skipped = try c.or(.skipped, [])
        local = try c.decode(DiffSide.self, forKey: .local)
        serverSide = try c.decode(DiffSide.self, forKey: .serverSide)
        same = try c.or(.same, 0)
        different = try c.or(.different, 0)
        onlyLocal = try c.or(.onlyLocal, 0)
        onlyServer = try c.or(.onlyServer, 0)
        errors = try c.or(.errors, 0)
        identical = try c.or(.identical, false)
    }

    /// Collections only on the server: a full import of the Mac's drops them there.
    public var onlyServerCollections: [String] { collections.filter { $0.local < 0 && $0.server >= 0 }.map(\.name) }
    /// Collections only here: a pull drops them here (the copy keeps them).
    public var onlyLocalCollections: [String] { collections.filter { $0.server < 0 && $0.local >= 0 }.map(\.name) }
}

/// The backup a database was compared with or pulled from.
public struct DBBackup: Decodable, Sendable, Equatable {
    public var id: String
    public var kind: String
    public var time: Date
    public var bytes: Int64

    public init(id: String = "", kind: String, time: Date, bytes: Int64 = 0) {
        self.id = id
        self.kind = kind
        self.time = time
        self.bytes = bytes
    }

    enum CodingKeys: String, CodingKey { case id, kind, time, bytes }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.or(.id, "")
        kind = try c.or(.kind, "")
        time = try c.decode(Date.self, forKey: .time)
        bytes = try c.or(.bytes, 0)
    }

    /// "gece", "saatlik", "elle"
    public var kindText: String {
        switch kind {
        case "daily": "gece"
        case "hourly": "saatlik"
        case "manual": "elle"
        default: kind
        }
    }
}

/// veritabani karsilastir / cek: their start, then what was found (diff) or done (a
/// pull: the copy of what was there, the backup loaded in its place).
public struct DBSyncResult: Decodable, Sendable, Equatable {
    public var db: String
    public var server: String
    public var serverName: String
    public var site: String
    public var backup: DBBackup
    public var diff: DBDiff?
    public var copy: String?
    public var loaded: LoadedDB?
    public var warnings: [String]
    public var tookMs: Int

    public init(db: String, server: String, serverName: String, site: String, backup: DBBackup, diff: DBDiff? = nil, copy: String? = nil,
                loaded: LoadedDB? = nil, warnings: [String] = [], tookMs: Int = 0) {
        self.db = db
        self.server = server
        self.serverName = serverName
        self.site = site
        self.backup = backup
        self.diff = diff
        self.copy = copy
        self.loaded = loaded
        self.warnings = warnings
        self.tookMs = tookMs
    }

    enum CodingKeys: String, CodingKey { case db, server, serverName, site, backup, diff, copy, loaded, warnings, tookMs }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        db = try c.decode(String.self, forKey: .db)
        server = try c.or(.server, "")
        serverName = try c.or(.serverName, server.uppercased())
        site = try c.or(.site, "")
        backup = try c.decode(DBBackup.self, forKey: .backup)
        diff = try c.decodeIfPresent(DBDiff.self, forKey: .diff)
        copy = try c.decodeIfPresent(String.self, forKey: .copy)
        loaded = try c.decodeIfPresent(LoadedDB.self, forKey: .loaded)
        warnings = try c.or(.warnings, [])
        tookMs = try c.or(.tookMs, 0)
    }
}

/// A line of veritabani karsilastir --json or cek --json.
enum DBEvent: Decodable {
    case start(DBSyncResult)
    case progress(part: String, done: Int64, total: Int64)
    case result(DBSyncResult)
    case error(String)

    enum CodingKeys: String, CodingKey { case event, part, done, total, message }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        switch try c.decode(String.self, forKey: .event) {
        case "start":
            self = .start(try DBSyncResult(from: decoder))
        case "progress":
            self = .progress(part: try c.or(.part, ""), done: try c.or(.done, 0), total: try c.or(.total, 0))
        case "result":
            self = .result(try DBSyncResult(from: decoder))
        case "error":
            self = .error(try c.or(.message, "bilinmeyen hata"))
        case let other:
            throw DecodingError.dataCorruptedError(forKey: .event, in: c, debugDescription: "bilinmeyen olay: \(other)")
        }
    }
}

/// A server and its sites as the menu last read them (its status file), and its panel's
/// address when the settings have one: what a database can be paired with.
public struct ServerSites: Sendable, Equatable, Identifiable {
    public var id: String { code }
    public var code: String
    public var name: String
    public var panel: String?
    public var sites: [String]

    public init(code: String, name: String, panel: String? = nil, sites: [String]) {
        self.code = code
        self.name = name
        self.panel = panel
        self.sites = sites
    }

    /// A page of the site's on the panel (sitePath: "import", "backup").
    public func page(site: String, _ path: String) -> URL? {
        guard let panel, !panel.isEmpty,
              let escaped = site.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) else { return nil }
        let base = panel.hasSuffix("/") ? String(panel.dropLast()) : panel
        return URL(string: base + "/sites/" + escaped + "/" + path)
    }
}

// MARK: - the window's state

/// The Databases window's state (panel-tasarim §33.4): the Mac's databases and their
/// pairings; one chosen; compared, sent or pulled, one at a time, stoppable.
@MainActor @Observable
public final class DatabasesModel {
    public enum Job: String, Sendable {
        case compare, pull, send
    }

    public enum Operation: Equatable, Sendable {
        case idle
        /// part: the core's (download, load, compare, copy; a send's: db, check).
        case running(job: Job, db: String, part: String?, done: Int64, total: Int64)
        case pulled(DBSyncResult)
        case sent(db: String, CreateResult)
        case failed(db: String, job: Job, message: String)
    }

    /// What waits for a yes: a pull, a send.
    public enum Confirm: Equatable, Sendable {
        case pull(String)
        case send(String)
    }

    public private(set) var list: DBList?
    public private(set) var listError: String?
    public private(set) var reading = false
    public var selected: String?
    public private(set) var operation: Operation = .idle
    /// The latest comparison of each database (until it changes: a pull).
    public private(set) var compared: [String: DBSyncResult] = [:]
    public var confirm: Confirm?
    /// A pairing being chosen for the selected database.
    public var pairing = false
    public var pairServer: String?
    public var pairSite: String?
    public private(set) var pairError: String?
    public private(set) var servers: [ServerSites] = []

    public let opened: OpenedList
    /// The archive made for a panel: its import page opened, the archive shown (the app).
    @ObservationIgnored public var onSent: ((CreateResult) -> Void)?

    @ObservationIgnored private let core: Core?
    @ObservationIgnored private var task: Task<Void, Never>?
    /// A read asked for while one was under way: once it ends.
    @ObservationIgnored private var readAgain = false
    @ObservationIgnored private let log = Logger(subsystem: "io.github.swbilisim.footops", category: "veritabani")

    /// How long a compare, a pull or a send may take before the app gives up on it
    /// (stoppable before that); stopped, how long the core has to put back what it began.
    static let limit: Duration = .seconds(6 * 3600)
    static let grace: Duration = .seconds(120)

    public init(core: Core?, opened: OpenedList? = nil, servers: [ServerSites] = []) {
        self.core = core
        self.opened = opened ?? OpenedList(core: core)
        self.servers = servers
    }

    /// A model holding what it is given (the image tests').
    public init(list: DBList?, selected: String? = nil, operation: Operation = .idle, compared: [String: DBSyncResult] = [:],
                confirm: Confirm? = nil, pairing: Bool = false, pairServer: String? = nil, pairSite: String? = nil,
                servers: [ServerSites] = [], opened: OpenedView? = nil) {
        core = nil
        self.opened = OpenedList(view: opened)
        self.list = list
        listError = list?.mongoError ?? list?.pairsError
        self.selected = selected
        self.operation = operation
        self.compared = compared
        self.confirm = confirm
        self.pairing = pairing
        self.pairServer = pairServer
        self.pairSite = pairSite
        self.servers = servers
    }

    // MARK: what is shown

    public var entry: DBEntry? { list?.databases.first { $0.name == selected } }

    public var isBusy: Bool {
        if case .running = operation {
            return true
        }
        return false
    }

    /// What runs may be stopped: not a pull replacing the database (its loading, or its
    /// putting back) — the core finishes that whatever comes, a half database looks whole.
    public var canStop: Bool {
        guard case .running(let job, _, let part, _, _) = operation else { return false }
        return !(job == .pull && (part == "load" || part == "restore"))
    }

    /// The selected database's server, as the menu knows it (its panel, its sites).
    public var pairedServer: ServerSites? {
        guard let p = entry?.pair else { return nil }
        return servers.first { $0.code == p.server }
    }

    public var pairTarget: ServerSites? { servers.first { $0.code == pairServer } }

    // MARK: reading

    /// The window opens: the databases, what was opened.
    public func load() async {
        async let opened: Void = self.opened.read()
        await readList()
        await opened
    }

    public func refresh() {
        Task { await readList() }
    }

    /// The servers and their sites anew (the menu's latest status).
    public func update(servers: [ServerSites]) {
        self.servers = servers
        if let s = pairServer, !servers.contains(where: { $0.code == s }) {
            pairServer = nil
            pairSite = nil
        }
    }

    func readList() async {
        guard let core else { return }
        guard !reading else {
            readAgain = true
            return
        }
        reading = true
        defer {
            reading = false
            if readAgain {
                readAgain = false
                Task { await readList() }
            }
        }
        do {
            let out = try await core.run(["veritabani", "liste", "--json"], timeout: .seconds(30))
            switch Core.decode(DBList.self, from: out) {
            case .success(let l):
                list = l
                listError = l.mongoError ?? l.pairsError
                if selected == nil || !l.databases.contains(where: { $0.name == selected }) {
                    selected = l.own.first?.name
                }
            case .failure(let m):
                listError = m.text
            }
        } catch Core.Failure.stopped {
            return
        } catch {
            listError = error.localizedDescription
        }
    }

    // MARK: choosing, pairing

    public func select(_ db: String) {
        guard db != selected, !isBusy else { return }
        selected = db
        confirm = nil
        pairing = false
        pairError = nil
    }

    /// Starts choosing a site for the selected database: its own, or one of its name.
    public func startPairing() {
        guard let e = entry else { return }
        pairing = true
        pairError = nil
        if let p = e.pair {
            (pairServer, pairSite) = (p.server, p.site)
            return
        }
        let guesses = [e.name, e.name.replacingOccurrences(of: "_", with: "-")].map { $0.lowercased() }
        for s in servers {
            if let site = s.sites.first(where: { guesses.contains($0) }) {
                (pairServer, pairSite) = (s.code, site)
                return
            }
        }
        pairServer = pairServer ?? servers.first?.code
        pairSite = nil
    }

    public func pick(server code: String) {
        guard code != pairServer else { return }
        pairServer = code
        pairSite = nil
    }

    public func savePairing() {
        guard let db = selected, let server = pairServer, let site = pairSite else { return }
        pair(["veritabani", "eslestir", db, server, site, "--json"], db: db)
    }

    public func removePairing() {
        guard let db = selected else { return }
        pair(["veritabani", "eslestir", db, "--kaldir", "--json"], db: db)
    }

    private func pair(_ args: [String], db: String) {
        guard let core, !isBusy else { return }
        Task {
            do {
                let out = try await core.run(args, timeout: .seconds(30))
                if case .failure(let m) = Core.decode(PairAnswer.self, from: out) {
                    pairError = m.text
                    return
                }
                pairing = false
                pairError = nil
                compared[db] = nil // another site: the comparison was the old one's
                await readList()
            } catch {
                pairError = error.localizedDescription
            }
        }
    }

    // MARK: comparing, pulling, sending

    public func compare() {
        guard let db = selected, entry?.pair != nil else { return }
        confirm = nil
        start(.compare, db: db, ["veritabani", "karsilastir", db, "--json"])
    }

    public func ask(_ c: Confirm) {
        guard !isBusy else { return }
        confirm = c
    }

    /// The yes to what was asked.
    public func confirmed() {
        switch confirm {
        case .pull(let db):
            confirm = nil
            start(.pull, db: db, ["veritabani", "cek", db, "--json"])
        case .send(let db):
            confirm = nil
            start(.send, db: db, ["veritabani", "gonder", db, "--json"])
        case nil:
            return
        }
    }

    private func start(_ job: Job, db: String, _ args: [String]) {
        guard let core, task == nil else { return }
        operation = .running(job: job, db: db, part: nil, done: 0, total: 0)
        log.info("başladı: \(args.dropLast().joined(separator: " "), privacy: .public)")
        task = Task {
            defer { task = nil }
            do {
                let out = try await core.stream(args, timeout: Self.limit, grace: Self.grace, keepOnStop: true) { line in
                    guard let part = Self.progress(of: line) else { return }
                    DispatchQueue.main.async {
                        MainActor.assumeIsolated {
                            guard case .running(let j, let d, _, _, _) = self.operation else { return }
                            self.operation = .running(job: j, db: d, part: part.part, done: part.done, total: part.total)
                        }
                    }
                }
                finish(job, db: db, out)
            } catch Core.Failure.stopped {
                operation = .idle
            } catch {
                operation = .failed(db: db, job: job, message: error.localizedDescription)
            }
        }
    }

    /// A progress line of either kind of command (veritabani's, arsiv olustur's).
    nonisolated static func progress(of line: Data) -> (part: String, done: Int64, total: Int64)? {
        switch try? CoreJSON.decoder().decode(DBEvent.self, from: line) {
        case .progress(let part, let done, let total)?:
            (part, done, total)
        default:
            nil
        }
    }

    private func finish(_ job: Job, db: String, _ out: Core.Output) {
        // Stopped and gone without a word (killed after its grace): nothing to tell but
        // what is there now.
        if out.stopped && !Self.answered(out) {
            operation = .idle
            Task {
                await readList()
                await opened.read()
            }
            return
        }
        if job == .send {
            switch ArchivesModel.outcome(of: out) {
            case .made(let res)?:
                operation = .sent(db: db, res)
                log.info("gönderilecek arşiv: \(res.path, privacy: .public)")
                onSent?(res)
            case .error(let m)?:
                operation = m == "durduruldu" ? .idle : .failed(db: db, job: job, message: m)
            default:
                operation = .failed(db: db, job: job, message: "footops-mac'in yanıtı anlaşılamadı")
            }
            return
        }
        switch Self.outcome(of: out) {
        case .result(let res)?:
            if job == .compare {
                compared[db] = res
                operation = .idle
                log.info("karşılaştırıldı: \(db, privacy: .public), \(res.tookMs) ms")
            } else {
                compared[db] = nil // the database is the backup now
                operation = .pulled(res)
                log.info("çekildi: \(db, privacy: .public), kopya \(res.copy ?? "yok", privacy: .public)")
                Task {
                    await readList()
                    await opened.read()
                }
            }
        case .error(let m)?:
            operation = m == "durduruldu" ? .idle : .failed(db: db, job: job, message: m)
            if job == .pull {
                // What became of it: put back, or (said in the message) its copy kept.
                Task {
                    await readList()
                    await opened.read()
                }
            }
        default:
            operation = .failed(db: db, job: job, message: "footops-mac'in yanıtı anlaşılamadı")
        }
    }

    /// The command said how it ended: a result or an error line.
    nonisolated static func answered(_ out: Core.Output) -> Bool {
        let decoder = CoreJSON.decoder()
        return out.stdout.split(separator: 0x0A).contains { line in
            switch try? decoder.decode(DBEvent.self, from: Data(line)) {
            case .result?, .error?: true
            default: (try? decoder.decode(ArchiveEvent.self, from: Data(line))).map { if case .made = $0 { true } else { false } } ?? false
            }
        }
    }

    /// The last result or error of a command's output.
    nonisolated static func outcome(of out: Core.Output) -> DBEvent? {
        let decoder = CoreJSON.decoder()
        var last: DBEvent?
        for line in out.stdout.split(separator: 0x0A) {
            switch try? decoder.decode(DBEvent.self, from: Data(line)) {
            case .result(let r)?:
                last = .result(r)
            case .error(let m)?:
                last = .error(m)
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

    /// Lets go of what was shown of the last pull, send or failure.
    public func dismiss() {
        if case .running = operation {
            return
        }
        operation = .idle
    }

    /// Stops what runs (the core removes what it began) — unless it is a pull replacing the
    /// database: that the core finishes, and its answer is still shown.
    public func stop() {
        guard canStop else { return }
        task?.cancel()
    }

    /// Stops what runs and waits for the core to have removed what it began (the app
    /// quits), at most limit; a pull replacing the database is only waited for (past the
    /// limit the core finishes it on its own).
    public func stopAndWait(limit: Duration) async {
        guard let task else { return }
        if canStop {
            task.cancel()
        }
        let clock = ContinuousClock()
        let deadline = clock.now + limit
        while self.task != nil, clock.now < deadline {
            try? await Task.sleep(for: .milliseconds(100))
        }
    }
}

/// veritabani eslestir's answer.
struct PairAnswer: Decodable {
    var db: String
}
