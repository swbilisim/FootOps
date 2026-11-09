import Foundation
import Observation
import os

/// What the app knows and when it reads again (panel-tasarim §29.3): the core's durum every
/// 5 minutes while the Mac is awake, a few seconds after it wakes, and when the menu opens
/// on a read older than a minute; yokla only when asked. Nothing runs while the Mac sleeps.
@MainActor @Observable
public final class Monitor {
    /// The core's last answer.
    public private(set) var status: Status?
    /// When the last read ended.
    public private(set) var readAt: Date?
    public private(set) var reading = false
    /// The core could not be run or understood: why. The light is gray; the last answer
    /// stays, as of readAt's read before.
    public private(set) var trouble: String?
    public private(set) var probe: ProbeState = .idle

    /// The menu bar's colour.
    public var light: Light { trouble != nil ? .gray : (status?.color ?? .gray) }

    @ObservationIgnored private let core: Core?
    @ObservationIgnored private var loop: Task<Void, Never>?
    @ObservationIgnored private var probeTask: Task<Void, Never>?
    @ObservationIgnored private var grayRetries = 0
    /// The newest read's number: a read stopped for a newer one (its core slow to end)
    /// says nothing when it ends.
    @ObservationIgnored private var readNumber = 0
    /// A read asked for while one was under way (new settings): once it ends.
    @ObservationIgnored private var readAgain = false
    @ObservationIgnored private let log = Logger(subsystem: "io.github.swbilisim.footops", category: "monitor")

    /// The time between reads while all is known; after a gray one, a few sooner tries
    /// (the network may be just back).
    static let every: Duration = .seconds(300)
    static let retry: Duration = .seconds(30)
    static let retries = 3
    /// A read younger than this is not repeated when the menu opens.
    static let freshFor: TimeInterval = 60

    public init(core: Core?) {
        self.core = core
    }

    /// A monitor holding what it is given (the image tests' and previews').
    public init(status: Status?, readAt: Date?, trouble: String? = nil, probe: ProbeState = .idle, reading: Bool = false) {
        core = nil
        self.status = status
        self.readAt = readAt
        self.trouble = trouble
        self.probe = probe
        self.reading = reading
    }

    public func start() {
        schedule(after: .zero)
    }

    public func stop() {
        loop?.cancel()
        loop = nil
        probeTask?.cancel()
    }

    /// The Mac woke: read again once the network is back, the read before sleep is old.
    public func didWake() {
        schedule(after: .seconds(5))
    }

    /// The Mac goes to sleep: nothing is left half-way (a visit cut by sleep would read as
    /// a site down).
    public func willSleep() {
        loop?.cancel()
        loop = nil
        stopProbe()
    }

    /// The menu opened: read if the last read is older than a minute.
    public func menuOpened(now: Date = Date()) {
        guard !reading, core != nil else { return }
        if let at = readAt, now.timeIntervalSince(at) < Self.freshFor {
            return
        }
        schedule(after: .zero)
    }

    /// Read now (the menu's button, new settings); a read under way is followed by one
    /// more, which sees what changed since it began.
    public func refresh() {
        guard !reading else {
            readAgain = true
            return
        }
        schedule(after: .zero)
    }

    private func schedule(after delay: Duration) {
        loop?.cancel()
        loop = Task {
            if delay > .zero {
                try? await Task.sleep(for: delay)
            }
            while !Task.isCancelled {
                await read()
                if readAgain {
                    readAgain = false
                    continue
                }
                try? await Task.sleep(for: nextWait(), tolerance: .seconds(10))
            }
        }
    }

    func nextWait() -> Duration {
        guard light == .gray, grayRetries < Self.retries else {
            if light != .gray {
                grayRetries = 0
            }
            return Self.every
        }
        grayRetries += 1
        return Self.retry
    }

    private func read() async {
        guard let core else {
            trouble = "footops-mac uygulamanın içinde bulunamadı (Contents/MacOS/footops-mac)"
            readAt = Date()
            return
        }
        readNumber += 1
        let number = readNumber
        reading = true
        defer {
            if number == readNumber {
                reading = false
            }
        }
        let clock = ContinuousClock()
        let start = clock.now
        do {
            let out = try await core.run(["durum", "--json"], timeout: .seconds(60))
            guard number == readNumber else { return }
            switch Core.decode(Status.self, from: out) {
            case .success(let st):
                status = st
                trouble = nil
            case .failure(let m):
                trouble = m.text
            }
        } catch Core.Failure.stopped {
            return // stopped for a newer read, or for sleep: it says
        } catch {
            guard number == readNumber else { return }
            trouble = error.localizedDescription
        }
        guard number == readNumber else { return }
        readAt = Date()
        let took = clock.now - start
        log.info("durum: \(self.light.rawValue, privacy: .public), \(took.components.seconds)s \(took.components.attoseconds / 1_000_000_000_000_000)ms")
    }

    // MARK: yokla

    public func startProbe() {
        guard probeTask == nil, let core else { return }
        probe = .running(done: 0, total: 0, failed: [])
        probeTask = Task {
            defer { probeTask = nil }
            do {
                let out = try await core.stream(["yokla", "--json"], timeout: .seconds(180)) { line in
                    guard let ev = try? CoreJSON.decoder().decode(ProbeEvent.self, from: line) else { return }
                    DispatchQueue.main.async {
                        MainActor.assumeIsolated { self.progress(ev) }
                    }
                }
                // The end from the output itself: the lines handed over above only move
                // the progress.
                probe = Self.outcome(of: out)
            } catch Core.Failure.stopped {
                probe = .idle
            } catch {
                probe = .failed(error.localizedDescription, at: Date())
            }
            if case .finished(let run) = probe {
                log.info("yokla: \(run.open)/\(run.total) açık, \(run.tookMs) ms")
            }
        }
    }

    public func stopProbe() {
        probeTask?.cancel()
    }

    private func progress(_ ev: ProbeEvent) {
        guard case .running(_, _, var failed) = probe else { return }
        switch ev {
        case .start(let total):
            probe = .running(done: 0, total: total, failed: failed)
        case .progress(let done, let total, let p):
            if !p.ok {
                failed.append(p)
            }
            probe = .running(done: done, total: total, failed: failed)
        case .result, .error:
            break
        }
    }

    /// What yokla's output comes to: its result line, or its error.
    nonisolated static func outcome(of out: Core.Output) -> ProbeState {
        let decoder = CoreJSON.decoder()
        var last: ProbeEvent?
        for line in out.stdout.split(separator: 0x0A) {
            if let ev = try? decoder.decode(ProbeEvent.self, from: Data(line)) {
                last = ev
            }
        }
        switch last {
        case .result(let run):
            return .finished(run)
        case .error(let message):
            return .failed(message, at: Date())
        default:
            let said = out.stderr.split(separator: "\n").first.map(String.init) ?? ""
            return .failed("footops-mac'in yanıtı anlaşılamadı (çıkış kodu \(out.status))" + (said.isEmpty ? "" : ": \(said.prefix(300))"),
                           at: Date())
        }
    }
}

/// Where a visit of the sites from the Mac stands.
public enum ProbeState: Equatable, Sendable {
    case idle
    /// Under way: how far, and the domains that did not answer so far.
    case running(done: Int, total: Int, failed: [Probe])
    case finished(ProbeRun)
    case failed(String, at: Date)
}
