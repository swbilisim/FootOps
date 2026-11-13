import Foundation

/// footops-mac, run for each thing the app shows (panel-tasarim §29.2): a short command
/// writes one JSON object, yokla a JSON line per step. No port, no service: each run
/// cleans up after itself. Stopping one sends SIGTERM; the core ends what it was doing
/// and says "durduruldu".
public struct Core: Sendable {
    public let executable: URL

    public init(executable: URL) {
        self.executable = executable
    }

    /// The core beside the app (Contents/MacOS/footops-mac); FOOTOPS_MAC_CORE names
    /// another, to try a build out.
    public static func bundled(_ bundle: Bundle = .main,
                               environment: [String: String] = ProcessInfo.processInfo.environment) -> Core? {
        if let path = environment["FOOTOPS_MAC_CORE"], !path.isEmpty {
            return Core(executable: URL(fileURLWithPath: path))
        }
        return bundle.url(forAuxiliaryExecutable: "footops-mac").map(Core.init(executable:))
    }

    /// What a run wrote, and its exit code; stopped: it was stopped from the app (only
    /// when the run was asked to keep what was written then).
    public struct Output: Sendable {
        public var stdout: Data
        public var stderr: String
        public var status: Int32
        public var stopped = false
    }

    public enum Failure: Error, Equatable, LocalizedError {
        case cannotStart(String)
        case timedOut(seconds: Int)
        /// Stopped from the app (the task cancelled).
        case stopped

        public var errorDescription: String? {
            switch self {
            case .cannotStart(let why): "footops-mac çalıştırılamadı: \(why)"
            case .timedOut(let s): "footops-mac \(s) sn içinde bitmedi, durduruldu"
            case .stopped: "durduruldu"
            }
        }
    }

    /// The most kept of what a run writes (the core's answers are a few KB).
    static let outputMax = 8 << 20

    /// Runs a command to its end: what it wrote and its exit code.
    public func run(_ args: [String], input: Data? = nil, timeout: Duration = .seconds(60)) async throws -> Output {
        try await execute(args, input: input, timeout: timeout, grace: .seconds(5), onLine: nil)
    }

    /// Runs a command, handing each line it writes over as it comes (from a background
    /// thread). Stopped, it has grace to clean up before SIGKILL (opening a backup:
    /// removing half a folder can take a while).
    /// keepOnStop: stopped, what it wrote is still the answer (a database pull may finish
    /// what it began, its result the truth), marked stopped — not Failure.stopped.
    public func stream(_ args: [String], timeout: Duration, grace: Duration = .seconds(5), keepOnStop: Bool = false,
                       onLine: @escaping @Sendable (Data) -> Void) async throws -> Output {
        try await execute(args, input: nil, timeout: timeout, grace: grace, keepOnStop: keepOnStop, onLine: onLine)
    }

    private func execute(_ args: [String], input: Data?, timeout: Duration, grace: Duration, keepOnStop: Bool = false,
                         onLine: (@Sendable (Data) -> Void)?) async throws -> Output {
        _ = Self.brokenPipesIgnored
        let process = Process()
        process.executableURL = executable
        process.arguments = args
        let out = Pipe()
        let err = Pipe()
        let inPipe = input.map { _ in Pipe() }
        process.standardOutput = out
        process.standardError = err
        process.standardInput = inPipe ?? FileHandle.nullDevice
        let run = Running(process, grace: Double(grace.components.seconds))
        let seconds = Double(timeout.components.seconds) + Double(timeout.components.attoseconds) / 1e18

        return try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Output, Error>) in
                DispatchQueue.global(qos: .utility).async {
                    let stdout = Collected(onLine: onLine)
                    let stderr = Collected(onLine: nil)
                    stdout.follow(out.fileHandleForReading)
                    stderr.follow(err.fileHandleForReading)
                    do {
                        try process.run()
                    } catch {
                        stdout.stop()
                        stderr.stop()
                        cont.resume(throwing: Failure.cannotStart(error.localizedDescription))
                        return
                    }
                    run.started()
                    DispatchQueue.global().asyncAfter(deadline: .now() + seconds) { run.stop(timedOut: true) }
                    if let input, let inPipe {
                        DispatchQueue.global().async {
                            // A core that ends before reading it all: EPIPE, not SIGPIPE.
                            let w = inPipe.fileHandleForWriting
                            try? w.write(contentsOf: input)
                            try? w.close()
                        }
                    }
                    process.waitUntilExit()
                    // What it wrote last is still in the pipes; a process of its own
                    // holding them open must not hold the app as well.
                    let grace = DispatchTime.now() + 2
                    stdout.waitForEnd(until: grace)
                    stderr.waitForEnd(until: grace)
                    stdout.stop()
                    stderr.stop()
                    run.finished()
                    let text = String(decoding: stderr.data.prefix(4096), as: UTF8.self)
                    if run.timedOut {
                        cont.resume(throwing: Failure.timedOut(seconds: Int(seconds)))
                    } else if run.cancelled && keepOnStop {
                        cont.resume(returning: Output(stdout: stdout.data, stderr: text, status: process.terminationStatus, stopped: true))
                    } else if run.cancelled {
                        cont.resume(throwing: Failure.stopped)
                    } else {
                        cont.resume(returning: Output(stdout: stdout.data, stderr: text, status: process.terminationStatus))
                    }
                }
            }
        } onCancel: {
            run.stop(timedOut: false)
        }
    }

    /// Writing to a core that has ended is an error to the writer, not the end of the app.
    private static let brokenPipesIgnored: Void = {
        signal(SIGPIPE, SIG_IGN)
    }()
}

/// A run's process, stopped from whichever thread asks: SIGTERM first (the core cleans
/// up and ends), SIGKILL if it is still there grace seconds later.
private final class Running: @unchecked Sendable {
    private let lock = NSLock()
    private let process: Process
    private let grace: Double
    private var isStarted = false
    private var isFinished = false
    private var stopWanted = false
    private(set) var cancelled = false
    private(set) var timedOut = false

    init(_ process: Process, grace: Double) {
        self.process = process
        self.grace = grace
    }

    func started() {
        lock.lock()
        isStarted = true
        let stop = stopWanted
        lock.unlock()
        if stop {
            terminate()
        }
    }

    func finished() {
        lock.lock()
        isFinished = true
        lock.unlock()
    }

    func stop(timedOut byClock: Bool) {
        lock.lock()
        if isFinished {
            lock.unlock()
            return
        }
        if byClock {
            timedOut = true
        } else {
            cancelled = true
        }
        stopWanted = true
        let now = isStarted
        lock.unlock()
        if now {
            terminate()
        }
    }

    private func terminate() {
        process.terminate()
        let pid = process.processIdentifier
        DispatchQueue.global().asyncAfter(deadline: .now() + grace) { [self] in
            lock.lock()
            let done = isFinished
            lock.unlock()
            if !done && process.isRunning {
                kill(pid, SIGKILL)
            }
        }
    }
}

/// What a run writes on one of its pipes, gathered as it comes (and handed over line by
/// line when asked), read once the run is over.
private final class Collected: @unchecked Sendable {
    private let lock = NSLock()
    private var bytes = Data()
    private var line = Data()
    private let onLine: (@Sendable (Data) -> Void)?
    private let ended = DispatchSemaphore(value: 0)
    private var handle: FileHandle?

    init(onLine: (@Sendable (Data) -> Void)?) {
        self.onLine = onLine
    }

    func follow(_ h: FileHandle) {
        handle = h
        h.readabilityHandler = { [self] h in
            let d = h.availableData
            if d.isEmpty {
                h.readabilityHandler = nil
                ended.signal()
            } else {
                add(d)
            }
        }
    }

    func waitForEnd(until deadline: DispatchTime) {
        _ = ended.wait(timeout: deadline)
    }

    /// No more reading; a last line without its newline is a line too.
    func stop() {
        handle?.readabilityHandler = nil
        lock.lock()
        let rest = line
        line = Data()
        lock.unlock()
        if !rest.isEmpty {
            onLine?(rest)
        }
    }

    private func add(_ d: Data) {
        var lines: [Data] = []
        lock.lock()
        if bytes.count < Core.outputMax {
            bytes.append(d.prefix(Core.outputMax - bytes.count))
        }
        if onLine != nil {
            line.append(d)
            while let nl = line.firstIndex(of: 0x0A) {
                let one = line[line.startIndex..<nl]
                line = Data(line[line.index(after: nl)...])
                if !one.isEmpty {
                    lines.append(Data(one))
                }
            }
            if line.count > 1 << 20 { // a line that never ends is no line of the core's
                line = Data()
            }
        }
        lock.unlock()
        for l in lines {
            onLine?(l)
        }
    }

    var data: Data {
        lock.lock()
        defer { lock.unlock() }
        return bytes
    }
}

extension Core {
    /// A short command's one JSON object: the answer, or the core's own words on why there
    /// is none ({"event":"error"}), or what little can be told.
    public static func decode<T: Decodable>(_ type: T.Type, from out: Output) -> Result<T, CoreMessage> {
        let decoder = CoreJSON.decoder()
        // The error first: an answer whose fields may all be left out (ayar's) would
        // read an error object as an empty answer.
        if let f = try? decoder.decode(CoreFailure.self, from: out.stdout), f.event == "error" {
            return .failure(CoreMessage(f.message ?? "bilinmeyen hata", code: f.code))
        }
        if let v = try? decoder.decode(T.self, from: out.stdout) {
            return .success(v)
        }
        let said = out.stderr.split(separator: "\n").first.map(String.init) ?? ""
        var text = "footops-mac'in yanıtı anlaşılamadı (çıkış kodu \(out.status))"
        if !said.isEmpty {
            text += ": " + said.prefix(300)
        }
        return .failure(CoreMessage(text))
    }
}

/// Why the core gave no answer, in its words; code: its error's (settings_changed).
public struct CoreMessage: Error, Equatable, Sendable {
    public var text: String
    public var code: String?

    public init(_ text: String, code: String? = nil) {
        self.text = text
        self.code = code
    }
}
