import AppKit
import Foundation
import Observation
import os

// What footops-mac yedek … writes with --json (panel-tasarim §29.14), and the Backups
// window's state. The core keeps the rules (which backup is which, where it opens, what
// may be deleted); the app only asks and shows. Fields left out are read as empty, so a
// newer core does not break an older app.

extension KeyedDecodingContainer {
    /// A field, or the fallback when it is left out.
    func or<T: Decodable>(_ key: Key, _ fallback: T) throws -> T {
        try decodeIfPresent(T.self, forKey: key) ?? fallback
    }
}

/// A server whose backups the Mac can read (yedek sunucular).
public struct BackupServer: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { code }
    public var code: String
    public var bucket: String
    /// Its repository's password is in the Keychain: without it nothing can be opened.
    public var password: Bool

    public init(code: String, bucket: String, password: Bool = true) {
        self.code = code
        self.bucket = bucket
        self.password = password
    }

    enum CodingKeys: String, CodingKey { case code, bucket, password }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        code = try c.decode(String.self, forKey: .code)
        bucket = try c.or(.bucket, "footops-\(code)")
        password = try c.or(.password, false)
    }
}

struct BackupServerList: Decodable {
    var servers: [BackupServer]

    enum CodingKeys: String, CodingKey { case servers }

    init(from decoder: Decoder) throws {
        servers = try decoder.container(keyedBy: CodingKeys.self).or(.servers, [])
    }
}

/// One part of a backup (db, files, def): its snapshot.
public struct BackupPart: Decodable, Sendable, Equatable {
    public var id: String
    public var time: Date
    public var files: Int64
    public var bytes: Int64

    public init(id: String = "", time: Date, files: Int64 = 0, bytes: Int64 = 0) {
        self.id = id
        self.time = time
        self.files = files
        self.bytes = bytes
    }

    enum CodingKeys: String, CodingKey { case id, time, files, bytes }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.or(.id, "")
        time = try c.decode(Date.self, forKey: .time)
        files = try c.or(.files, 0)
        bytes = try c.or(.bytes, 0)
    }
}

/// A site's backup: the parts one run took that the repository still keeps.
public struct BackupPoint: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { run }
    public var run: String
    public var time: Date
    public var kind: String
    public var parts: [String: BackupPart]

    public init(run: String, time: Date, kind: String, parts: [String: BackupPart]) {
        self.run = run
        self.time = time
        self.kind = kind
        self.parts = parts
    }

    enum CodingKeys: String, CodingKey { case run, time, kind, parts }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        run = try c.decode(String.self, forKey: .run)
        time = try c.decode(Date.self, forKey: .time)
        kind = try c.or(.kind, "")
        parts = try c.or(.parts, [:])
    }

    /// The run's kind as the panel names it.
    public var kindText: String {
        switch kind {
        case "daily": "gece"
        case "hourly": "saatlik"
        case "manual": "elle"
        case "": "—"
        default: kind
        }
    }
}

public struct SiteBackups: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { name }
    public var name: String
    public var points: [BackupPoint]

    public init(name: String, points: [BackupPoint]) {
        self.name = name
        self.points = points
    }

    enum CodingKeys: String, CodingKey { case name, points }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decode(String.self, forKey: .name)
        points = try c.or(.points, [])
    }
}

/// yedek liste: a server's sites and their backups, newest first.
public struct BackupList: Decodable, Sendable, Equatable {
    public var server: String
    public var bucket: String
    public var time: Date
    public var tookMs: Int
    public var snapshots: Int
    public var serverPart: Date?
    public var sites: [SiteBackups]

    public init(server: String, bucket: String, time: Date, tookMs: Int = 0, snapshots: Int, serverPart: Date? = nil,
                sites: [SiteBackups]) {
        self.server = server
        self.bucket = bucket
        self.time = time
        self.tookMs = tookMs
        self.snapshots = snapshots
        self.serverPart = serverPart
        self.sites = sites
    }

    enum CodingKeys: String, CodingKey { case server, bucket, time, tookMs, snapshots, serverPart, sites }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        server = try c.decode(String.self, forKey: .server)
        bucket = try c.or(.bucket, "")
        time = try c.decode(Date.self, forKey: .time)
        tookMs = try c.or(.tookMs, 0)
        snapshots = try c.or(.snapshots, 0)
        serverPart = try c.decodeIfPresent(Date.self, forKey: .serverPart)
        sites = try c.or(.sites, [])
    }
}

/// One part as opened: its snapshot, and what was written of it.
public struct OpenedPart: Decodable, Sendable, Equatable {
    public var time: Date
    public var kind: String
    public var bytes: Int64
    public var written: Int64
    /// Files restored, the environment's variables.
    public var count: Int64

    enum CodingKeys: String, CodingKey { case time, kind, bytes, written, count }

    public init(time: Date, kind: String = "", bytes: Int64 = 0, written: Int64 = 0, count: Int64 = 0) {
        self.time = time
        self.kind = kind
        self.bytes = bytes
        self.written = written
        self.count = count
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        time = try c.decode(Date.self, forKey: .time)
        kind = try c.or(.kind, "")
        bytes = try c.or(.bytes, 0)
        written = try c.or(.written, 0)
        count = try c.or(.count, 0)
    }
}

/// A database loaded into the Mac's MongoDB.
public struct LoadedDB: Decodable, Sendable, Equatable {
    public var name: String
    public var documents: Int64
    public var failed: Int64
    public var replaced: Bool
    /// The documents an archive's manifest counted (0: no count to compare with).
    public var expected: Int64

    public init(name: String, documents: Int64 = 0, failed: Int64 = 0, replaced: Bool = false, expected: Int64 = 0) {
        self.name = name
        self.documents = documents
        self.failed = failed
        self.replaced = replaced
        self.expected = expected
    }

    enum CodingKeys: String, CodingKey { case name, documents, failed, replaced, expected }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decode(String.self, forKey: .name)
        documents = try c.or(.documents, 0)
        failed = try c.or(.failed, 0)
        replaced = try c.or(.replaced, false)
        expected = try c.or(.expected, 0)
    }
}

/// yedek ac / yedek mongo (and arsiv ac / arsiv mongo): what is opened (its start), then
/// what was. archive: the archive's file name (server: the backup's it was made from).
public struct OpenResult: Decodable, Sendable, Equatable {
    public var server: String
    public var site: String
    public var archive: String?
    /// The folder (yedek ac); none when only the database was loaded.
    public var target: String?
    public var time: Date
    public var parts: [String: OpenedPart]
    public var database: LoadedDB?
    public var warnings: [String]
    public var tookMs: Int

    public init(server: String, site: String, archive: String? = nil, target: String? = nil, time: Date, parts: [String: OpenedPart] = [:],
                database: LoadedDB? = nil, warnings: [String] = [], tookMs: Int = 0) {
        self.server = server
        self.site = site
        self.archive = archive
        self.target = target
        self.time = time
        self.parts = parts
        self.database = database
        self.warnings = warnings
        self.tookMs = tookMs
    }

    enum CodingKeys: String, CodingKey { case server, site, archive, target, time, parts, database, warnings, tookMs }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        server = try c.or(.server, "")
        site = try c.or(.site, "")
        archive = try c.decodeIfPresent(String.self, forKey: .archive)
        target = try c.decodeIfPresent(String.self, forKey: .target)
        time = try c.decode(Date.self, forKey: .time)
        parts = try c.or(.parts, [:])
        database = try c.decodeIfPresent(LoadedDB.self, forKey: .database)
        warnings = try c.or(.warnings, [])
        tookMs = try c.or(.tookMs, 0)
    }
}

/// One line of yedek ac --json / yedek mongo --json.
public enum OpenEvent: Sendable, Equatable {
    case start(OpenResult)
    case progress(part: String, done: Int64, total: Int64)
    case result(OpenResult)
    /// code "db_exists": the database's name is taken (database names it).
    case error(message: String, code: String?, database: String?)

    /// The run's end: its result, or its error.
    var isOutcome: Bool {
        switch self {
        case .result, .error: true
        default: false
        }
    }
}

extension OpenEvent: Decodable {
    enum CodingKeys: String, CodingKey { case event, part, done, total, message, code, database }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        switch try c.decode(String.self, forKey: .event) {
        case "start":
            self = .start(try OpenResult(from: decoder))
        case "progress":
            self = .progress(part: try c.or(.part, ""), done: try c.or(.done, 0), total: try c.or(.total, 0))
        case "result":
            self = .result(try OpenResult(from: decoder))
        case "error":
            self = .error(message: try c.or(.message, "bilinmeyen hata"), code: try c.decodeIfPresent(String.self, forKey: .code),
                          database: try? c.decodeIfPresent(String.self, forKey: .database))
        case let other:
            throw DecodingError.dataCorruptedError(forKey: .event, in: c, debugDescription: "bilinmeyen olay: \(other)")
        }
    }
}

/// A folder a backup (or an archive: its file's name) was opened into, still there.
public struct OpenedFolder: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { path }
    public var path: String
    public var server: String
    public var site: String
    public var archive: String?
    public var time: Date
    public var at: Date
    public var files: Int64
    public var bytes: Int64
    /// Opened over a week ago.
    public var old: Bool
    /// Its open did not end: under way (busy), or cut short (the core was killed).
    public var partial: Bool
    public var busy: Bool
    /// It could not be looked at now (macOS privacy, a disk not mounted): its line is kept.
    public var error: String?

    public init(path: String, server: String, site: String, archive: String? = nil, time: Date, at: Date, files: Int64, bytes: Int64,
                old: Bool = false, partial: Bool = false, busy: Bool = false, error: String? = nil) {
        self.path = path
        self.server = server
        self.site = site
        self.archive = archive
        self.time = time
        self.at = at
        self.files = files
        self.bytes = bytes
        self.old = old
        self.partial = partial
        self.busy = busy
        self.error = error
    }

    enum CodingKeys: String, CodingKey { case path, server, site, archive, time, at, files, bytes, old, partial, busy, error }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        path = try c.decode(String.self, forKey: .path)
        server = try c.or(.server, "")
        site = try c.or(.site, "")
        archive = try c.decodeIfPresent(String.self, forKey: .archive)
        time = try c.decode(Date.self, forKey: .time)
        at = try c.decode(Date.self, forKey: .at)
        files = try c.or(.files, 0)
        bytes = try c.or(.bytes, 0)
        old = try c.or(.old, false)
        partial = try c.or(.partial, false)
        busy = try c.or(.busy, false)
        error = try c.decodeIfPresent(String.self, forKey: .error)
    }
}

/// A database of the Mac's MongoDB a backup or an archive was loaded into; registered:
/// footops-mac loaded it (else only its name, *_yedek_* or *_arsiv_*, says so).
public struct OpenedDatabase: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { name }
    public var name: String
    public var server: String?
    public var site: String?
    public var archive: String?
    public var time: Date?
    public var at: Date?
    public var bytes: Int64
    public var registered: Bool
    public var old: Bool

    public init(name: String, server: String? = nil, site: String? = nil, archive: String? = nil, time: Date? = nil, at: Date? = nil,
                bytes: Int64 = 0, registered: Bool = true, old: Bool = false) {
        self.name = name
        self.server = server
        self.site = site
        self.archive = archive
        self.time = time
        self.at = at
        self.bytes = bytes
        self.registered = registered
        self.old = old
    }

    enum CodingKeys: String, CodingKey { case name, server, site, archive, time, at, bytes, registered, old }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decode(String.self, forKey: .name)
        server = try c.decodeIfPresent(String.self, forKey: .server)
        site = try c.decodeIfPresent(String.self, forKey: .site)
        archive = try c.decodeIfPresent(String.self, forKey: .archive)
        time = try c.decodeIfPresent(Date.self, forKey: .time)
        at = try c.decodeIfPresent(Date.self, forKey: .at)
        bytes = try c.or(.bytes, 0)
        registered = try c.or(.registered, false)
        old = try c.or(.old, false)
    }
}

/// yedek acilanlar: what was opened on this Mac.
public struct OpenedView: Decodable, Sendable, Equatable {
    public var folders: [OpenedFolder]
    public var databases: [OpenedDatabase]
    /// The Mac's MongoDB could not be asked (its databases are not listed).
    public var mongoError: String?

    public init(folders: [OpenedFolder] = [], databases: [OpenedDatabase] = [], mongoError: String? = nil) {
        self.folders = folders
        self.databases = databases
        self.mongoError = mongoError
    }

    enum CodingKeys: String, CodingKey { case folders, databases, mongoError }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        folders = try c.or(.folders, [])
        databases = try c.or(.databases, [])
        mongoError = try c.decodeIfPresent(String.self, forKey: .mongoError)
    }

    public var isEmpty: Bool { folders.isEmpty && databases.isEmpty }
}

struct DeleteAnswer: Decodable {
    var done: String
}

// MARK: - the pasteboard

/// The recovery line on the pasteboard (panel-tasarim §29.5): marked concealed and
/// transient (clipboard managers keep no copy of it), never shown, and taken off again
/// after a minute — unless something else was copied since, which is left alone. It
/// outlives the Backups window: the minute runs out with the window closed too.
@MainActor
public final class RecoveryClipboard {
    static let concealed = NSPasteboard.PasteboardType("org.nspasteboard.ConcealedType")
    static let transient = NSPasteboard.PasteboardType("org.nspasteboard.TransientType")

    private let pasteboard: NSPasteboard
    private var placed: Int?
    private var timer: Task<Void, Never>?

    public init(pasteboard: NSPasteboard = .general) {
        self.pasteboard = pasteboard
    }

    /// Puts the line on the pasteboard; after `keep`, takes it off if it is still the
    /// one there, and says whether it did.
    public func put(_ line: String, keep: Duration = .seconds(60), then: @escaping @MainActor (Bool) -> Void = { _ in }) {
        timer?.cancel()
        // This Mac's pasteboard only: not handed to the owner's other devices (Universal
        // Clipboard), where the minute's clearing would not reach it.
        pasteboard.prepareForNewContents(with: .currentHostOnly)
        // No declareTypes: it would begin new contents, the option above lost.
        pasteboard.setString(line, forType: .string)
        pasteboard.setData(Data(), forType: Self.concealed)
        pasteboard.setData(Data(), forType: Self.transient)
        let placed = pasteboard.changeCount
        self.placed = placed
        timer = Task { [weak self] in
            try? await Task.sleep(for: keep)
            guard !Task.isCancelled, let self else { return }
            then(self.takeOff())
        }
    }

    /// Takes the line off now if it is still the one there (the app quits).
    @discardableResult
    public func takeOff() -> Bool {
        timer?.cancel()
        timer = nil
        guard let placed else { return false }
        self.placed = nil
        guard pasteboard.changeCount == placed else { return false }
        pasteboard.clearContents()
        return true
    }
}

// MARK: - the window's state

/// The Backups window's state (panel-tasarim §29.5): the servers, a server's backups (yedek
/// liste, read once and on asking), one opened — into a folder (yedek ac) or into the
/// Mac's MongoDB (yedek mongo), one at a time, stoppable —, what was opened on this Mac
/// (yedek acilanlar, deleted with yedek sil), the recovery line onto the pasteboard.
@MainActor @Observable
public final class BackupsModel {
    public private(set) var servers: [BackupServer] = []
    public private(set) var serversError: String?
    public private(set) var server: String?
    /// Each server's list as last read, or why it could not be; readingList: under way.
    public private(set) var lists: [String: ListState] = [:]
    public private(set) var readingList: Set<String> = []
    public private(set) var site: String?
    public private(set) var run: String?
    /// The parts to open of the backup chosen (db, files, def).
    public var parts: Set<String> = ["db", "files", "def"]
    public private(set) var operation: Operation = .idle
    /// The name to load the database under when the default one is taken.
    public var otherName = ""
    /// What was opened on this Mac (shared with the Archives window).
    public let opened: OpenedList
    public private(set) var recovery: RecoveryState = .idle

    public enum ListState: Equatable, Sendable {
        case loaded(BackupList)
        case failed(String)
    }

    public enum Operation: Equatable, Sendable {
        case idle
        case running(Running)
        /// The database's name is taken in the Mac's MongoDB: write over it, or another.
        case asking(name: String)
        case finished(OpenResult)
        case failed(String)
    }

    public struct Running: Equatable, Sendable {
        public var mongo: Bool
        public var site: String
        public var target: String?
        public var part: String?
        public var done: Int64
        public var total: Int64

        public init(mongo: Bool, site: String, target: String? = nil, part: String? = nil, done: Int64 = 0, total: Int64 = 0) {
            self.mongo = mongo
            self.site = site
            self.target = target
            self.part = part
            self.done = done
            self.total = total
        }
    }

    public enum RecoveryState: Equatable, Sendable {
        case idle
        case copying
        case copied(server: String, at: Date)
        /// Taken off the pasteboard after its minute; kept: something else was copied
        /// since and was left alone.
        case cleared
        case kept
        case failed(String)
    }

    @ObservationIgnored private let core: Core?
    @ObservationIgnored private let clipboard: RecoveryClipboard?
    @ObservationIgnored private var task: Task<Void, Never>?
    /// What the running open was asked: to ask again with another name or over it.
    @ObservationIgnored private var asked: (server: String, site: String, run: String)?
    @ObservationIgnored private let log = Logger(subsystem: "io.github.swbilisim.footops", category: "yedek")

    /// How long a backup may take to open before the app gives up on it (stoppable
    /// before that); stopped, how long the core has to remove what it began before it is
    /// killed (half a folder of many files takes seconds).
    static let openLimit: Duration = .seconds(6 * 3600)
    static let openGrace: Duration = .seconds(60)

    public init(core: Core?, clipboard: RecoveryClipboard?, opened: OpenedList? = nil) {
        self.core = core
        self.clipboard = clipboard
        self.opened = opened ?? OpenedList(core: core)
    }

    /// A model holding what it is given (the image tests').
    public init(servers: [BackupServer], server: String?, lists: [String: ListState] = [:], site: String? = nil,
                run: String? = nil, operation: Operation = .idle, opened: OpenedView? = nil, recovery: RecoveryState = .idle,
                readingList: Set<String> = []) {
        core = nil
        clipboard = nil
        self.servers = servers
        self.server = server
        self.lists = lists
        self.site = site
        self.run = run
        self.operation = operation
        self.opened = OpenedList(view: opened)
        self.recovery = recovery
        self.readingList = readingList
    }

    // MARK: what is shown

    public var list: BackupList? {
        guard let server, case .loaded(let l)? = lists[server] else { return nil }
        return l
    }

    public var listError: String? {
        guard let server, case .failed(let e)? = lists[server] else { return nil }
        return e
    }

    public var siteBackups: SiteBackups? { list?.sites.first { $0.name == site } }

    public var point: BackupPoint? { siteBackups?.points.first { $0.run == run } }

    public var isBusy: Bool {
        if case .running = operation {
            return true
        }
        return false
    }

    // MARK: reading

    /// Reads the servers, the chosen one's backups and what was opened (the window
    /// opens).
    public func load() async {
        async let opened: Void = self.opened.read()
        await readServers()
        await opened
    }

    private func readServers() async {
        guard let core else { return }
        do {
            let out = try await core.run(["yedek", "sunucular", "--json"], timeout: .seconds(30))
            switch Core.decode(BackupServerList.self, from: out) {
            case .success(let l):
                servers = l.servers
                serversError = l.servers.isEmpty ? "Anahtar Zinciri'nde footops-yedek / restic-<sunucu> kaydı yok: yedekler okunamaz." : nil
            case .failure(let m):
                serversError = m.text
            }
        } catch {
            serversError = error.localizedDescription
        }
        if server == nil || !servers.contains(where: { $0.code == server }) {
            server = servers.first(where: \.password)?.code ?? servers.first?.code
        }
        if let s = server, lists[s] == nil, servers.first(where: { $0.code == s })?.password == true {
            await readList(s)
        }
    }

    public func select(server code: String) {
        guard code != server, !isBusy else { return }
        dismissAsking()
        server = code
        site = nil
        run = nil
        if lists[code] == nil, servers.first(where: { $0.code == code })?.password == true {
            Task { await readList(code) }
        }
    }

    public func select(site name: String) {
        guard name != site, !isBusy else { return }
        dismissAsking()
        site = name
        run = siteBackups?.points.first?.run
        resetParts()
    }

    public func select(run id: String) {
        guard id != run, !isBusy else { return }
        dismissAsking()
        run = id
        resetParts()
    }

    /// The question about a taken name was about the backup chosen before: it goes.
    private func dismissAsking() {
        if case .asking = operation {
            dismiss()
        }
    }

    /// Every part the chosen backup has, ticked.
    private func resetParts() {
        parts = Set(point?.parts.keys.map { $0 } ?? [])
        if case .finished = operation {
            operation = .idle
        }
        if case .failed = operation {
            operation = .idle
        }
    }

    /// Reads the chosen server's backups anew.
    public func refreshList() {
        guard let server else { return }
        Task { await readList(server) }
    }

    func readList(_ code: String) async {
        guard let core, !readingList.contains(code) else { return }
        readingList.insert(code)
        defer { readingList.remove(code) }
        do {
            let out = try await core.run(["yedek", "liste", code, "--json"], timeout: .seconds(240))
            switch Core.decode(BackupList.self, from: out) {
            case .success(let l):
                lists[code] = .loaded(l)
                log.info("liste \(code, privacy: .public): \(l.snapshots) anlık görüntü, \(l.tookMs) ms")
                if code == server, site == nil || !l.sites.contains(where: { $0.name == site }) {
                    site = nil
                    run = nil
                } else if code == server, let s = l.sites.first(where: { $0.name == site }), !s.points.contains(where: { $0.run == run }) {
                    run = s.points.first?.run
                    resetParts()
                }
            case .failure(let m):
                lists[code] = .failed(m.text)
            }
        } catch Core.Failure.stopped {
            return
        } catch {
            lists[code] = .failed(error.localizedDescription)
        }
    }

    // MARK: opening

    /// Opens the chosen backup's ticked parts into a folder (~/Downloads/<site>-<time>).
    public func openFolder() {
        guard let server, let site, let run, !parts.isEmpty else { return }
        var args = ["yedek", "ac", server, site, run]
        for (part, flag) in [("db", "--db"), ("files", "--dosyalar"), ("def", "--ayarlar")] where parts.contains(part) {
            args.append(flag)
        }
        start(args + ["--json"], mongo: false, server: server, site: site, run: run)
    }

    /// Loads the chosen backup's database into the Mac's MongoDB under its default name
    /// (the core's).
    public func loadDatabase() {
        guard let server, let site, let run else { return }
        load(target: (server, site, run), name: nil, overwrite: false)
    }

    /// Answers the question about a taken name, for the backup it was asked about: over
    /// the database of that name, or under another.
    public func retryLoad(name: String, overwrite: Bool) {
        guard case .asking = operation, let asked else { return }
        load(target: asked, name: name, overwrite: overwrite)
    }

    private func load(target: (server: String, site: String, run: String), name: String?, overwrite: Bool) {
        var args = ["yedek", "mongo", target.server, target.site, target.run]
        if let name, !name.isEmpty {
            args += ["--ad", name]
        }
        if overwrite {
            args.append("--uzerine")
        }
        start(args + ["--json"], mongo: true, server: target.server, site: target.site, run: target.run)
    }

    /// Gives up asking about the taken name.
    public func dismiss() {
        asked = nil
        operation = .idle
    }

    private func start(_ args: [String], mongo: Bool, server: String, site: String, run: String) {
        guard let core, task == nil else { return }
        asked = (server, site, run)
        operation = .running(Running(mongo: mongo, site: site))
        log.info("açılıyor: \(args.dropLast().joined(separator: " "), privacy: .public)")
        task = Task {
            defer { task = nil }
            do {
                let out = try await core.stream(args, timeout: Self.openLimit, grace: Self.openGrace) { line in
                    guard let ev = try? CoreJSON.decoder().decode(OpenEvent.self, from: line) else { return }
                    DispatchQueue.main.async {
                        MainActor.assumeIsolated { self.progress(ev) }
                    }
                }
                finish(Self.outcome(of: out))
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
            // The core's heartbeat before a part has spoken names none: still reading the
            // repository.
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
            log.info("açıldı: \(res.site, privacy: .public), \(res.tookMs) ms")
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

    /// The last event of an open's output: its result or its error.
    nonisolated static func outcome(of out: Core.Output) -> OpenEvent? {
        let decoder = CoreJSON.decoder()
        var last: OpenEvent?
        for line in out.stdout.split(separator: 0x0A) {
            switch try? decoder.decode(OpenEvent.self, from: Data(line)) {
            case let ev? where ev.isOutcome:
                last = ev
            default:
                break
            }
        }
        switch last {
        case .result, .error:
            return last
        default:
            let said = out.stderr.split(separator: "\n").first.map(String.init) ?? ""
            return .error(message: "footops-mac'in yanıtı anlaşılamadı (çıkış kodu \(out.status))" + (said.isEmpty ? "" : ": \(said.prefix(300))"),
                          code: nil, database: nil)
        }
    }

    /// Stops the open under way (the core removes what it began).
    public func stop() {
        task?.cancel()
    }

    /// Stops the open under way and waits for the core to have removed what it began
    /// (the app quits), at most limit.
    public func stopAndWait(limit: Duration) async {
        guard let task else { return }
        task.cancel()
        // The open's task lets go of itself once the core has ended; a task's value would
        // be waited for whole, whatever the limit.
        let clock = ContinuousClock()
        let deadline = clock.now + limit
        while self.task != nil, clock.now < deadline {
            try? await Task.sleep(for: .milliseconds(100))
        }
    }

    // MARK: the recovery line

    /// Puts the chosen server's recovery line on the pasteboard: read from the core's
    /// output (never shown), taken off a minute later.
    public func copyRecovery() {
        guard let core, let clipboard, let server, recovery != .copying else { return }
        recovery = .copying
        Task {
            do {
                // No --json: the line is a secret, kept out of the core's JSON.
                let out = try await core.run(["yedek", "kurtarma", server], timeout: .seconds(30))
                let line = String(decoding: out.stdout, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
                guard out.status == 0, line.hasPrefix("FOOTOPS-KURTARMA-1:"), !line.contains("\n") else {
                    let said = out.stderr.split(separator: "\n").first.map { String($0.drop(while: { $0 == "✗" || $0 == " " })) } ?? ""
                    recovery = .failed(said.isEmpty ? "kurtarma satırı alınamadı (çıkış kodu \(out.status))" : String(said.prefix(300)))
                    return
                }
                clipboard.put(line) { [weak self] cleared in
                    guard let self, case .copied = self.recovery else { return }
                    self.recovery = cleared ? .cleared : .kept
                }
                recovery = .copied(server: server, at: Date())
            } catch {
                recovery = .failed(error.localizedDescription)
            }
        }
    }
}
