import Foundation

// What footops-mac writes with --json (panel-tasarim §29.12): durum's one object, yokla's
// event lines, ayar's settings. The core keeps the rules (the colours, the checks); the
// app only reads. Fields the core leaves out when empty are read as empty here, and an
// unknown colour as gray, so a newer core does not break an older app.

/// A colour as the core judges it, from the best: all is well, nothing known, worth a
/// look, visitors hit (or a server gone quiet).
public enum Light: String, Sendable, Codable, CaseIterable {
    case green, gray, yellow, red

    public init(from decoder: Decoder) throws {
        self = Light(rawValue: try decoder.singleValueContainer().decode(String.self)) ?? .gray
    }

    var rank: Int {
        switch self {
        case .green: 0
        case .gray: 1
        case .yellow: 2
        case .red: 3
        }
    }

    /// The worse of the two.
    public func worse(_ other: Light) -> Light { other.rank > rank ? other : self }
}

/// footops-mac durum: the colour of it all and each server's.
public struct Status: Decodable, Sendable, Equatable {
    public var time: Date
    public var color: Light
    /// Nothing could be read at all (the colour is gray): why.
    public var error: String?
    public var servers: [ServerStatus]
    /// Backups opened on this Mac over a week ago and still there: the menu's reminder.
    public var openedOld: Int

    public init(time: Date, color: Light, error: String? = nil, servers: [ServerStatus] = [], openedOld: Int = 0) {
        self.time = time
        self.color = color
        self.error = error
        self.servers = servers
        self.openedOld = openedOld
    }

    enum CodingKeys: String, CodingKey { case time, color, error, servers, openedOld }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        time = try c.decode(Date.self, forKey: .time)
        color = try c.decode(Light.self, forKey: .color)
        error = try c.decodeIfPresent(String.self, forKey: .error)
        servers = try c.decodeIfPresent([ServerStatus].self, forKey: .servers) ?? []
        openedOld = try c.decodeIfPresent(Int.self, forKey: .openedOld) ?? 0
    }
}

/// One server: its colour, why it is not green, and what its two files in R2 say.
public struct ServerStatus: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { code }
    public var code: String
    public var name: String
    public var bucket: String
    public var panel: String?
    public var ssh: String?
    public var color: Light
    /// Why it is not green, the worst first.
    public var problems: [Problem]
    /// The server's status file as read, when R2 got it; or why it could not be read.
    /// Silent: too old to tell how things are now.
    public var file: StatusFile?
    public var fileAt: Date?
    public var fileError: String?
    public var silent: Bool
    /// The outside watcher's heartbeat likewise; stale: too old to go by.
    public var watcher: WatcherBeat?
    public var watcherAt: Date?
    public var watcherError: String?
    public var watcherStale: Bool
    public var running: Int
    public var total: Int
    public var sites: [SiteStatus]

    public init(code: String, name: String, bucket: String, panel: String? = nil, ssh: String? = nil, color: Light,
                problems: [Problem] = [], file: StatusFile? = nil, fileAt: Date? = nil, fileError: String? = nil,
                silent: Bool = false, watcher: WatcherBeat? = nil, watcherAt: Date? = nil, watcherError: String? = nil,
                watcherStale: Bool = false, running: Int = 0, total: Int = 0, sites: [SiteStatus] = []) {
        self.code = code
        self.name = name
        self.bucket = bucket
        self.panel = panel
        self.ssh = ssh
        self.color = color
        self.problems = problems
        self.file = file
        self.fileAt = fileAt
        self.fileError = fileError
        self.silent = silent
        self.watcher = watcher
        self.watcherAt = watcherAt
        self.watcherError = watcherError
        self.watcherStale = watcherStale
        self.running = running
        self.total = total
        self.sites = sites
    }

    enum CodingKeys: String, CodingKey {
        case code, name, bucket, panel, ssh, color, problems, file, fileAt, fileError, silent, watcher, watcherAt,
             watcherError, watcherStale, running, total, sites
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        code = try c.decode(String.self, forKey: .code)
        name = try c.decodeIfPresent(String.self, forKey: .name) ?? code.uppercased()
        bucket = try c.decodeIfPresent(String.self, forKey: .bucket) ?? ""
        panel = try c.decodeIfPresent(String.self, forKey: .panel)
        ssh = try c.decodeIfPresent(String.self, forKey: .ssh)
        color = try c.decode(Light.self, forKey: .color)
        problems = try c.decodeIfPresent([Problem].self, forKey: .problems) ?? []
        file = try c.decodeIfPresent(StatusFile.self, forKey: .file)
        fileAt = try c.decodeIfPresent(Date.self, forKey: .fileAt)
        fileError = try c.decodeIfPresent(String.self, forKey: .fileError)
        silent = try c.decodeIfPresent(Bool.self, forKey: .silent) ?? false
        watcher = try c.decodeIfPresent(WatcherBeat.self, forKey: .watcher)
        watcherAt = try c.decodeIfPresent(Date.self, forKey: .watcherAt)
        watcherError = try c.decodeIfPresent(String.self, forKey: .watcherError)
        watcherStale = try c.decodeIfPresent(Bool.self, forKey: .watcherStale) ?? false
        running = try c.decodeIfPresent(Int.self, forKey: .running) ?? 0
        total = try c.decodeIfPresent(Int.self, forKey: .total) ?? 0
        sites = try c.decodeIfPresent([SiteStatus].self, forKey: .sites) ?? []
    }

    /// The watcher's heartbeat when it is fresh enough to go by.
    public var freshWatcher: WatcherBeat? { watcherStale ? nil : watcher }
}

/// One reason a server is not green.
public struct Problem: Codable, Sendable, Equatable, Hashable {
    public var color: Light
    public var text: String

    public init(_ color: Light, _ text: String) {
        self.color = color
        self.text = text
    }
}

/// A site: its container's state (and its words for it), and its domains as the watcher
/// last saw them.
public struct SiteStatus: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { name }
    public var name: String
    public var state: String
    public var label: String
    public var color: Light
    public var domains: [DomainStatus]

    public init(name: String, state: String, label: String, color: Light, domains: [DomainStatus] = []) {
        self.name = name
        self.state = state
        self.label = label
        self.color = color
        self.domains = domains
    }

    enum CodingKeys: String, CodingKey { case name, state, label, color, domains }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decode(String.self, forKey: .name)
        state = try c.decodeIfPresent(String.self, forKey: .state) ?? ""
        label = try c.decodeIfPresent(String.self, forKey: .label) ?? state
        color = try c.decode(Light.self, forKey: .color)
        domains = try c.decodeIfPresent([DomainStatus].self, forKey: .domains) ?? []
    }

    /// Switched off on purpose (parked), or frozen for a backup's moment: no colour of
    /// its own.
    public var isOff: Bool { state == "parked" || state == "paused" }
}

/// A domain as the watcher last judged it: ok, blind (a Cloudflare challenge answered for
/// the site), failing (its last visit unanswered), down (the chat was told) or unknown.
public struct DomainStatus: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { name }
    public var name: String
    public var watcher: String
    public var code: String?
    public var reason: String?
    public var since: Date?

    public init(name: String, watcher: String, code: String? = nil, reason: String? = nil, since: Date? = nil) {
        self.name = name
        self.watcher = watcher
        self.code = code
        self.reason = reason
        self.since = since
    }

    /// The watcher sees it answer.
    public var isOpen: Bool { watcher == "ok" || watcher == "blind" }
    /// The watcher could not reach it.
    public var isFailing: Bool { watcher == "failing" || watcher == "down" }
}

/// A server's izleme/durum.json, as its FootOps wrote it (internal/monitor).
public struct StatusFile: Decodable, Sendable, Equatable {
    public var server: String
    public var footops: String
    public var time: Date
    public var every: Int
    public var sitesError: String?
    public var backup: Backup
    public var sys: Sys?

    public init(server: String, footops: String, time: Date, every: Int = 300, sitesError: String? = nil,
                backup: Backup, sys: Sys? = nil) {
        self.server = server
        self.footops = footops
        self.time = time
        self.every = every
        self.sitesError = sitesError
        self.backup = backup
        self.sys = sys
    }

    enum CodingKeys: String, CodingKey { case server, footops, time, every, sitesError, backup, sys }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        server = try c.decodeIfPresent(String.self, forKey: .server) ?? ""
        footops = try c.decodeIfPresent(String.self, forKey: .footops) ?? ""
        time = try c.decode(Date.self, forKey: .time)
        every = try c.decodeIfPresent(Int.self, forKey: .every) ?? 0
        sitesError = try c.decodeIfPresent(String.self, forKey: .sitesError)
        backup = try c.decodeIfPresent(Backup.self, forKey: .backup) ?? Backup(ready: false)
        sys = try c.decodeIfPresent(Sys.self, forKey: .sys)
    }
}

/// The status file's word on backups.
public struct Backup: Decodable, Sendable, Equatable {
    public var ready: Bool
    public var paused: Bool
    public var lastOk: Date?
    public var nightDone: String?
    public var problems: [String]

    public init(ready: Bool, paused: Bool = false, lastOk: Date? = nil, nightDone: String? = nil, problems: [String] = []) {
        self.ready = ready
        self.paused = paused
        self.lastOk = lastOk
        self.nightDone = nightDone
        self.problems = problems
    }

    enum CodingKeys: String, CodingKey { case ready, paused, lastOk, nightDone, problems }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        ready = try c.decodeIfPresent(Bool.self, forKey: .ready) ?? false
        paused = try c.decodeIfPresent(Bool.self, forKey: .paused) ?? false
        lastOk = try c.decodeIfPresent(Date.self, forKey: .lastOk)
        nightDone = try c.decodeIfPresent(String.self, forKey: .nightDone)
        problems = try c.decodeIfPresent([String].self, forKey: .problems) ?? []
    }
}

/// The server's own measures (the agent's last sample): bytes and seconds.
public struct Sys: Decodable, Sendable, Equatable {
    public var time: Date
    public var cpuPercent: Double
    public var load1: Double
    public var load5: Double
    public var load15: Double
    public var memUsed: UInt64
    public var memTotal: UInt64
    public var swapUsed: UInt64
    public var swapTotal: UInt64
    public var diskUsed: UInt64
    public var diskTotal: UInt64
    public var uptime: Int64

    public init(time: Date, cpuPercent: Double, load1: Double = 0, load5: Double = 0, load15: Double = 0,
                memUsed: UInt64, memTotal: UInt64, swapUsed: UInt64 = 0, swapTotal: UInt64 = 0,
                diskUsed: UInt64, diskTotal: UInt64, uptime: Int64 = 0) {
        self.time = time
        self.cpuPercent = cpuPercent
        self.load1 = load1
        self.load5 = load5
        self.load15 = load15
        self.memUsed = memUsed
        self.memTotal = memTotal
        self.swapUsed = swapUsed
        self.swapTotal = swapTotal
        self.diskUsed = diskUsed
        self.diskTotal = diskTotal
        self.uptime = uptime
    }
}

/// The outside watcher's izleme/nobetci.json (the Cloudflare Worker's heartbeat).
public struct WatcherBeat: Decodable, Sendable, Equatable {
    public var time: Date
    public var domains: [WatcherDomain]
    public var sendError: String?

    public init(time: Date, domains: [WatcherDomain] = [], sendError: String? = nil) {
        self.time = time
        self.domains = domains
        self.sendError = sendError
    }

    enum CodingKeys: String, CodingKey { case time, domains, sendError }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        time = try c.decode(Date.self, forKey: .time)
        domains = try c.decodeIfPresent([WatcherDomain].self, forKey: .domains) ?? []
        sendError = try c.decodeIfPresent(String.self, forKey: .sendError)
    }
}

public struct WatcherDomain: Decodable, Sendable, Equatable {
    public var name: String
    public var ok: Bool

    public init(name: String, ok: Bool) {
        self.name = name
        self.ok = ok
    }

    enum CodingKeys: String, CodingKey { case name, ok }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decodeIfPresent(String.self, forKey: .name) ?? ""
        ok = try c.decodeIfPresent(Bool.self, forKey: .ok) ?? false
    }
}

// MARK: - yokla

/// One domain visited from the Mac, next to what the watcher last said of it.
public struct Probe: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { server + "/" + domain }
    public var domain: String
    public var server: String
    public var serverName: String
    public var site: String
    /// Answered: anything but a 5xx, a time-out or no connection.
    public var ok: Bool
    /// The HTTP status, "timeout" or "error".
    public var code: String
    public var reason: String?
    public var note: String?
    /// Time to the answer's first byte.
    public var ms: Int
    public var cloudflare: Bool
    public var certExpires: Date?
    public var watcher: String
    /// What the Mac's visit and the watcher's together say, when not all is well.
    public var verdict: String?

    public init(domain: String, server: String, serverName: String, site: String, ok: Bool, code: String,
                reason: String? = nil, note: String? = nil, ms: Int, cloudflare: Bool = true, certExpires: Date? = nil,
                watcher: String = "ok", verdict: String? = nil) {
        self.domain = domain
        self.server = server
        self.serverName = serverName
        self.site = site
        self.ok = ok
        self.code = code
        self.reason = reason
        self.note = note
        self.ms = ms
        self.cloudflare = cloudflare
        self.certExpires = certExpires
        self.watcher = watcher
        self.verdict = verdict
    }

    enum CodingKeys: String, CodingKey {
        case domain, server, serverName, site, ok, code, reason, note, ms, cloudflare, certExpires, watcher, verdict
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        domain = try c.decode(String.self, forKey: .domain)
        server = try c.decodeIfPresent(String.self, forKey: .server) ?? ""
        serverName = try c.decodeIfPresent(String.self, forKey: .serverName) ?? server.uppercased()
        site = try c.decodeIfPresent(String.self, forKey: .site) ?? ""
        ok = try c.decodeIfPresent(Bool.self, forKey: .ok) ?? false
        code = try c.decodeIfPresent(String.self, forKey: .code) ?? ""
        reason = try c.decodeIfPresent(String.self, forKey: .reason)
        note = try c.decodeIfPresent(String.self, forKey: .note)
        ms = try c.decodeIfPresent(Int.self, forKey: .ms) ?? 0
        cloudflare = try c.decodeIfPresent(Bool.self, forKey: .cloudflare) ?? false
        certExpires = try c.decodeIfPresent(Date.self, forKey: .certExpires)
        watcher = try c.decodeIfPresent(String.self, forKey: .watcher) ?? "unknown"
        verdict = try c.decodeIfPresent(String.self, forKey: .verdict)
    }
}

/// A site not visited: parked on purpose.
public struct Skipped: Decodable, Sendable, Equatable {
    public var serverName: String
    public var site: String
    public var label: String

    public init(serverName: String, site: String, label: String) {
        self.serverName = serverName
        self.site = site
        self.label = label
    }
}

/// footops-mac yokla's result.
public struct ProbeRun: Decodable, Sendable, Equatable {
    public var time: Date
    public var tookMs: Int
    public var total: Int
    public var open: Int
    public var domains: [Probe]
    public var skipped: [Skipped]
    /// What the run could not go by.
    public var notes: [String]
    public var slowest: String?
    public var slowestMs: Int?

    public init(time: Date, tookMs: Int, total: Int, open: Int, domains: [Probe], skipped: [Skipped] = [],
                notes: [String] = [], slowest: String? = nil, slowestMs: Int? = nil) {
        self.time = time
        self.tookMs = tookMs
        self.total = total
        self.open = open
        self.domains = domains
        self.skipped = skipped
        self.notes = notes
        self.slowest = slowest
        self.slowestMs = slowestMs
    }

    enum CodingKeys: String, CodingKey { case time, tookMs, total, open, domains, skipped, notes, slowest, slowestMs }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        time = try c.decode(Date.self, forKey: .time)
        tookMs = try c.decodeIfPresent(Int.self, forKey: .tookMs) ?? 0
        total = try c.decodeIfPresent(Int.self, forKey: .total) ?? 0
        open = try c.decodeIfPresent(Int.self, forKey: .open) ?? 0
        domains = try c.decodeIfPresent([Probe].self, forKey: .domains) ?? []
        skipped = try c.decodeIfPresent([Skipped].self, forKey: .skipped) ?? []
        notes = try c.decodeIfPresent([String].self, forKey: .notes) ?? []
        slowest = try c.decodeIfPresent(String.self, forKey: .slowest)
        slowestMs = try c.decodeIfPresent(Int.self, forKey: .slowestMs)
    }
}

/// One line of yokla --json.
public enum ProbeEvent: Sendable, Equatable {
    case start(total: Int)
    case progress(done: Int, total: Int, probe: Probe)
    case result(ProbeRun)
    case error(String)
}

extension ProbeEvent: Decodable {
    enum CodingKeys: String, CodingKey { case event, total, done, probe, message }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        switch try c.decode(String.self, forKey: .event) {
        case "start":
            self = .start(total: try c.decodeIfPresent(Int.self, forKey: .total) ?? 0)
        case "progress":
            self = .progress(done: try c.decode(Int.self, forKey: .done), total: try c.decode(Int.self, forKey: .total),
                             probe: try c.decode(Probe.self, forKey: .probe))
        case "result":
            self = .result(try ProbeRun(from: decoder))
        case "error":
            self = .error(try c.decodeIfPresent(String.self, forKey: .message) ?? "bilinmeyen hata")
        case let other:
            throw DecodingError.dataCorruptedError(forKey: .event, in: c, debugDescription: "bilinmeyen olay: \(other)")
        }
    }
}

// MARK: - ayar

/// footops-mac ayar: where the settings file is and every server the Mac knows.
public struct SettingsView: Decodable, Sendable, Equatable {
    public var path: String
    /// The file as read: ayar yaz given it writes over that file only.
    public var revision: String
    /// The file or the Keychain could not be read (what could be is still listed).
    public var error: String?
    public var servers: [ServerSetting]

    public init(path: String, revision: String = "", error: String? = nil, servers: [ServerSetting]) {
        self.path = path
        self.revision = revision
        self.error = error
        self.servers = servers
    }

    enum CodingKeys: String, CodingKey { case path, revision, error, servers }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        path = try c.decodeIfPresent(String.self, forKey: .path) ?? ""
        revision = try c.decodeIfPresent(String.self, forKey: .revision) ?? ""
        error = try c.decodeIfPresent(String.self, forKey: .error)
        servers = try c.decodeIfPresent([ServerSetting].self, forKey: .servers) ?? []
    }
}

/// One server's settings: what the Mac calls it, what the app opens (the panel, SSH) and
/// where its backups are.
public struct ServerSetting: Codable, Sendable, Equatable, Identifiable {
    public var id: String { code }
    public var code: String
    /// Empty: its code in capitals (and the menu, its status file's name).
    public var name: String
    public var panel: String
    public var ssh: String
    public var bucket: String
    /// Where its backups are read when bucket is empty.
    public var defaultBucket: String
    /// Its repository password is in the Keychain (a server footops-yedek knows).
    public var keychain: Bool

    public init(code: String, name: String = "", panel: String = "", ssh: String = "", bucket: String = "",
                defaultBucket: String = "", keychain: Bool = true) {
        self.code = code
        self.name = name
        self.panel = panel
        self.ssh = ssh
        self.bucket = bucket
        self.defaultBucket = defaultBucket
        self.keychain = keychain
    }

    enum CodingKeys: String, CodingKey { case code, name, panel, ssh, bucket, defaultBucket, keychain }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        code = try c.decode(String.self, forKey: .code)
        name = try c.decodeIfPresent(String.self, forKey: .name) ?? ""
        panel = try c.decodeIfPresent(String.self, forKey: .panel) ?? ""
        ssh = try c.decodeIfPresent(String.self, forKey: .ssh) ?? ""
        bucket = try c.decodeIfPresent(String.self, forKey: .bucket) ?? ""
        defaultBucket = try c.decodeIfPresent(String.self, forKey: .defaultBucket) ?? "footops-\(code)"
        keychain = try c.decodeIfPresent(Bool.self, forKey: .keychain) ?? false
    }

    /// The name as the windows show it.
    public var shownName: String {
        let n = name.trimmingCharacters(in: .whitespaces)
        return n.isEmpty ? code.uppercased() : n
    }
}

/// The settings to write (ayar yaz's stdin): the file's own form.
struct SettingsFile: Encodable {
    struct Server: Encodable {
        var name: String
        var panel: String
        var ssh: String
        var bucket: String
    }

    var format = 1
    /// The file the list was read from (none: written over whatever is there).
    var revision: String?
    var servers: [String: Server]

    init(_ list: [ServerSetting], revision: String? = nil) {
        self.revision = revision
        servers = Dictionary(list.map { ($0.code, Server(name: $0.name, panel: $0.panel, ssh: $0.ssh, bucket: $0.bucket)) },
                             uniquingKeysWith: { a, _ in a })
    }
}

// MARK: - sunucu

/// A server to add (sunucu ekle's stdin); the password goes to the Keychain only.
struct NewServer: Encodable, Equatable {
    var code = ""
    var name = ""
    var panel = ""
    var ssh = ""
    var bucket = ""
    var password = ""
}

/// The Mac's R2 access to write (sunucu r2 yaz's stdin): a field left empty keeps what
/// is there.
struct NewR2: Encodable, Equatable {
    var account = ""
    var keyID = ""
    var secret = ""

    enum CodingKeys: String, CodingKey {
        case account
        case keyID = "key_id"
        case secret
    }
}

/// footops-mac sunucu dene: what was tried of a server from this Mac, a line each.
public struct ServerTry: Decodable, Sendable, Equatable {
    public var code: String
    public var name: String
    public var bucket: String
    public var checks: [TryCheck]
    /// Nothing failed (a part not tried fails nothing).
    public var ok: Bool
    public var tookMs: Int

    public init(code: String, name: String, bucket: String, checks: [TryCheck], ok: Bool, tookMs: Int = 0) {
        self.code = code
        self.name = name
        self.bucket = bucket
        self.checks = checks
        self.ok = ok
        self.tookMs = tookMs
    }

    enum CodingKeys: String, CodingKey { case code, name, bucket, checks, ok, tookMs }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        code = try c.decode(String.self, forKey: .code)
        name = try c.decodeIfPresent(String.self, forKey: .name) ?? code.uppercased()
        bucket = try c.decodeIfPresent(String.self, forKey: .bucket) ?? ""
        checks = try c.decodeIfPresent([TryCheck].self, forKey: .checks) ?? []
        ok = try c.decodeIfPresent(Bool.self, forKey: .ok) ?? false
        tookMs = try c.decodeIfPresent(Int.self, forKey: .tookMs) ?? 0
    }
}

/// One thing tried: ok, warn (it works, with a word of care), fail, or skip (not tried).
public struct TryCheck: Decodable, Sendable, Equatable, Hashable {
    public var part: String
    public var state: String
    public var text: String

    public init(part: String, state: String, text: String) {
        self.part = part
        self.state = state
        self.text = text
    }
}

/// footops-mac sunucu r2: which items of the Mac's R2 access the Keychain has; the
/// account masked.
public struct R2Access: Decodable, Sendable, Equatable {
    public var account: String?
    public var accountSet: Bool
    public var keySet: Bool
    public var secretSet: Bool

    public init(account: String? = nil, accountSet: Bool, keySet: Bool, secretSet: Bool) {
        self.account = account
        self.accountSet = accountSet
        self.keySet = keySet
        self.secretSet = secretSet
    }

    /// All three there: the servers' buckets can be read.
    public var complete: Bool { accountSet && keySet && secretSet }
}

/// The core's word that a command failed: {"event":"error","message":…}, with a code
/// for one the app answers in a way of its own (settings_changed).
struct CoreFailure: Decodable {
    var event: String
    var message: String?
    var code: String?
}

// MARK: - decoding

public enum CoreJSON {
    /// The decoder for the core's JSON: snake_case keys, RFC 3339 times with or without
    /// fractions (the servers' come with nanoseconds, the watcher's with milliseconds).
    public static func decoder() -> JSONDecoder {
        let d = JSONDecoder()
        d.keyDecodingStrategy = .convertFromSnakeCase
        d.dateDecodingStrategy = .custom { decoder in
            let c = try decoder.singleValueContainer()
            let text = try c.decode(String.self)
            guard let date = parseDate(text) else {
                throw DecodingError.dataCorruptedError(in: c, debugDescription: "tarih anlaşılamadı: \(text)")
            }
            return date
        }
        return d
    }

    static func parseDate(_ text: String) -> Date? {
        if let d = try? Date.ISO8601FormatStyle(includingFractionalSeconds: true).parse(text) {
            return d
        }
        return try? Date.ISO8601FormatStyle().parse(text)
    }
}
