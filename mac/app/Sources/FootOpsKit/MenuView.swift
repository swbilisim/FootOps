import SwiftUI

/// What the menu's items do; the app gives them (the image tests give nothing).
public struct MenuActions {
    public var open: (URL) -> Void
    public var backups: () -> Void
    public var archives: () -> Void
    public var databases: () -> Void
    public var settings: () -> Void
    public var quit: () -> Void

    public init(open: @escaping (URL) -> Void, backups: @escaping () -> Void = {}, archives: @escaping () -> Void = {},
                databases: @escaping () -> Void = {}, settings: @escaping () -> Void, quit: @escaping () -> Void) {
        self.open = open
        self.backups = backups
        self.archives = archives
        self.databases = databases
        self.settings = settings
        self.quit = quit
    }

    public static var none: MenuActions { MenuActions(open: { _ in }, settings: {}, quit: {}) }
}

extension Light {
    public var color: Color {
        switch self {
        case .green: .green
        case .yellow: .yellow
        case .red: .red
        case .gray: .gray
        }
    }

    /// The whole in a few words (the menu's head).
    public var summary: String {
        switch self {
        case .green: "Her şey yolunda"
        case .yellow: "Bakılmalı"
        case .red: "Sorun var"
        case .gray: "Bilgi yok"
        }
    }
}

/// The menu bar's window (panel-tasarim §29.3): the whole, each server (its sites on a
/// click), the watcher, the visit from the Mac, the settings.
public struct MenuView: View {
    let monitor: Monitor
    let actions: MenuActions
    /// The clock for "3 dk önce" (fixed in the image tests; else the minute's).
    let fixedNow: Date?
    /// The tallest the window grows before it scrolls; nil: never scrolls (the image tests
    /// draw it whole).
    let maxHeight: CGFloat?
    @State private var expanded: Set<String>
    @State private var allProbes: Bool

    public static let width: CGFloat = 360

    public init(monitor: Monitor, actions: MenuActions, now: Date? = nil, maxHeight: CGFloat? = 560,
                expanded: Set<String> = [], allProbes: Bool = false) {
        self.monitor = monitor
        self.actions = actions
        fixedNow = now
        self.maxHeight = maxHeight
        _expanded = State(initialValue: expanded)
        _allProbes = State(initialValue: allProbes)
    }

    public var body: some View {
        Group {
            if let fixedNow {
                content(now: fixedNow)
            } else {
                TimelineView(.everyMinute) { context in
                    content(now: context.date)
                }
            }
        }
        .frame(width: Self.width)
    }

    @ViewBuilder
    private func content(now: Date) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            Header(monitor: monitor, now: now)
            Divider()
            scrolling {
                VStack(alignment: .leading, spacing: 2) {
                    if let status = monitor.status {
                        if let error = status.error {
                            Notice(light: .gray, text: error)
                                .padding(.horizontal, 14)
                                .padding(.vertical, 8)
                        }
                        ForEach(status.servers) { server in
                            ServerSection(server: server, now: now, expanded: binding(server.code), actions: actions)
                        }
                        if !status.servers.isEmpty {
                            WatcherLine(servers: status.servers, now: now)
                        }
                    } else if monitor.trouble == nil {
                        Text("Sunucuların durumu okunuyor…")
                            .foregroundStyle(.secondary)
                            .padding(14)
                    }
                    Divider().padding(.vertical, 4)
                    ProbeSection(monitor: monitor, now: now, all: $allProbes)
                    if let n = monitor.status?.openedOld, n > 0 {
                        OpenedReminder(count: n, actions: actions)
                    }
                }
                .padding(.vertical, 6)
            }
            Divider()
            Footer(actions: actions)
        }
    }

    private func binding(_ code: String) -> Binding<Bool> {
        Binding(get: { expanded.contains(code) }, set: { on in
            if on {
                expanded.insert(code)
            } else {
                expanded.remove(code)
            }
        })
    }

    /// The middle scrolls once it is taller than maxHeight; shorter, the window fits it (a
    /// scroll view's ideal height is its content's, so the window opens at its size).
    @ViewBuilder
    private func scrolling<Content: View>(@ViewBuilder _ inner: () -> Content) -> some View {
        if let maxHeight {
            ScrollView {
                inner()
            }
            .scrollBounceBehavior(.basedOnSize)
            .frame(maxHeight: maxHeight)
        } else {
            inner()
        }
    }
}

// MARK: - head and foot

private struct Header: View {
    let monitor: Monitor
    let now: Date

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            Circle()
                .fill(monitor.light.color.gradient)
                .frame(width: 14, height: 14)
                .padding(.top, 3)
            VStack(alignment: .leading, spacing: 2) {
                Text("FootOps · \(monitor.light.summary)")
                    .font(.headline)
                Text(subtitle)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            Spacer(minLength: 8)
            Button {
                monitor.refresh()
            } label: {
                if monitor.reading {
                    ProgressView().controlSize(.small)
                        .frame(width: 16, height: 16)
                } else {
                    Image(systemName: "arrow.clockwise")
                        .frame(width: 16, height: 16)
                }
            }
            .buttonStyle(IconButtonStyle())
            .help("Durumu şimdi oku")
            .disabled(monitor.reading)
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
    }

    private var subtitle: String {
        if let trouble = monitor.trouble {
            var text = "Okunamadı: \(trouble)"
            if let at = monitor.status?.time {
                text += " · gösterilen \(Fmt.clock(at, now: now))'deki durum"
            }
            return text
        }
        guard let at = monitor.readAt ?? monitor.status?.time else {
            return monitor.reading ? "Okunuyor…" : "Henüz okunmadı"
        }
        var text = "Son okuma \(Fmt.clock(at, now: now)) · \(Fmt.ago(at, now: now))"
        if let st = monitor.status, st.color == .yellow || st.color == .red {
            let n = st.servers.reduce(0) { $0 + $1.problems.count }
            if n > 0 {
                text = "\(n) konu · " + text
            }
        }
        return text
    }
}

/// Backups opened on this Mac a week ago and more, still there (panel-tasarim §29.7):
/// they may hold a site's secrets.
private struct OpenedReminder: View {
    let count: Int
    let actions: MenuActions

    var body: some View {
        Button {
            actions.backups()
        } label: {
            Notice(light: .yellow, text: "Bu Mac'te bir haftadan eski \(count) açılmış yedek ya da arşiv var (sırlar içerebilir): Yedekler'den sil",
                   font: .subheadline)
        }
        .buttonStyle(MenuRowStyle())
        .padding(.horizontal, 6)
    }
}

private struct Footer: View {
    let actions: MenuActions

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 2) {
                item("Yedekler…", "externaldrive", "⌘Y", actions.backups)
                    .keyboardShortcut("y", modifiers: .command)
                item("Arşivler…", "shippingbox", "⌘R", actions.archives)
                    .keyboardShortcut("r", modifiers: .command)
            }
            HStack(spacing: 2) {
                item("Veritabanları…", "cylinder.split.1x2", "⌘D", actions.databases)
                    .keyboardShortcut("d", modifiers: .command)
                item("Ayarlar…", "gearshape", "⌘,", actions.settings)
                    .keyboardShortcut(",", modifiers: .command)
            }
            item("Çık", nil, "⌘Q", actions.quit)
                .keyboardShortcut("q", modifiers: .command)
        }
        .padding(6)
    }

    private func item(_ title: String, _ icon: String?, _ key: String, _ action: @escaping () -> Void) -> some View {
        Button(action: action) {
            HStack {
                if let icon {
                    Label(title, systemImage: icon)
                } else {
                    Text(title)
                }
                Spacer(minLength: 4)
                Text(key).foregroundStyle(.tertiary)
            }
            .lineLimit(1)
        }
        .buttonStyle(MenuRowStyle())
        .frame(maxWidth: .infinity)
    }
}

/// A line of text with its colour's dot.
struct Notice: View {
    let light: Light
    let text: String
    var font: Font = .callout

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            Image(systemName: "circle.fill")
                .font(.system(size: 7))
                .foregroundStyle(light.color)
            Text(text)
                .font(font)
                .fixedSize(horizontal: false, vertical: true)
        }
    }
}

// MARK: - a server

private struct ServerSection: View {
    let server: ServerStatus
    let now: Date
    @Binding var expanded: Bool
    let actions: MenuActions

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 0) {
                Button {
                    withAnimation(.snappy(duration: 0.2)) {
                        expanded.toggle()
                    }
                } label: {
                    HStack(spacing: 8) {
                        Circle().fill(server.color.color.gradient)
                            .frame(width: 10, height: 10)
                        Text(server.name)
                            .font(.body.weight(.semibold))
                        Spacer(minLength: 4)
                        if server.file != nil {
                            Text("\(server.running) / \(server.total) site")
                                .monospacedDigit()
                                .foregroundStyle(.secondary)
                        }
                        Image(systemName: "chevron.right")
                            .font(.caption.weight(.semibold))
                            .foregroundStyle(.tertiary)
                            .rotationEffect(.degrees(expanded ? 90 : 0))
                    }
                }
                .buttonStyle(MenuRowStyle())
                .help(expanded ? "Siteleri gizle" : "Siteleri göster")
                if let url = panelURL {
                    Button {
                        actions.open(url)
                    } label: {
                        Image(systemName: "globe")
                    }
                    .buttonStyle(IconButtonStyle())
                    .help("\(server.name) panelini tarayıcıda aç")
                }
                if let url = sshURL {
                    Button {
                        actions.open(url)
                    } label: {
                        Image(systemName: "terminal")
                    }
                    .buttonStyle(IconButtonStyle())
                    .help("\(server.name) sunucusuna SSH ile bağlan (Terminal)")
                }
            }
            .padding(.horizontal, 6)
            VStack(alignment: .leading, spacing: 3) {
                ForEach(details, id: \.self) { line in
                    Text(line)
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                        .monospacedDigit()
                }
                ForEach(Array(server.problems.enumerated()), id: \.offset) { _, p in
                    Notice(light: p.color, text: p.text)
                }
            }
            .padding(.leading, 32)
            .padding(.trailing, 14)
            .padding(.bottom, 6)
            if expanded {
                SiteList(server: server, now: now)
                    .padding(.leading, 26)
                    .padding(.trailing, 10)
                    .padding(.bottom, 6)
                if panelURL == nil || sshURL == nil {
                    Button {
                        actions.settings()
                    } label: {
                        Text(panelURL == nil && sshURL == nil ? "Panel ve SSH adresi ekle…" :
                            panelURL == nil ? "Panel adresi ekle…" : "SSH hedefi ekle…")
                            .font(.subheadline)
                            .foregroundStyle(.secondary)
                    }
                    .buttonStyle(MenuRowStyle())
                    .padding(.leading, 24)
                    .padding(.trailing, 6)
                    .padding(.bottom, 4)
                }
            }
        }
    }

    private var panelURL: URL? {
        guard let p = server.panel, !p.isEmpty, let url = URL(string: p), ["https", "http"].contains(url.scheme?.lowercased()) else {
            return nil
        }
        return url
    }

    private var sshURL: URL? {
        guard let s = server.ssh, !s.isEmpty else { return nil }
        return URL(string: "ssh://" + s)
    }

    /// The grey lines under the name: version and backup, then the machine.
    private var details: [String] {
        guard let f = server.file else { return [] }
        var lines: [String] = []
        var first = "FootOps \(f.footops)"
        if !f.backup.ready {
            first += " · yedek hazır değil"
        } else if let last = f.backup.lastOk {
            first += " · son yedek \(Fmt.clock(last, now: now))"
        } else {
            first += " · yedek henüz yok"
        }
        lines.append(first)
        if let y = f.sys, !server.silent {
            lines.append("İşlemci %\(Fmt.number(y.cpuPercent, decimals: 0)) · bellek %\(Fmt.percent(y.memUsed, of: y.memTotal)) · disk %\(Fmt.percent(y.diskUsed, of: y.diskTotal))")
        }
        let off = server.sites.filter(\.isOff)
        if !off.isEmpty {
            lines.append(off.map { "\($0.name) \($0.label)" }.joined(separator: " · "))
        }
        return lines
    }
}

/// A server's sites, each with the watcher's view of its domains.
private struct SiteList: View {
    let server: ServerStatus
    let now: Date

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            if server.sites.isEmpty {
                Text(server.file == nil ? "Site listesi yok (durum dosyası okunamadı)" : "Site yok")
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }
            // Keyed by place: names are a server's word, two alike would draw wrongly.
            ForEach(Array(server.sites.enumerated()), id: \.offset) { _, site in
                VStack(alignment: .leading, spacing: 1) {
                    HStack(alignment: .firstTextBaseline, spacing: 6) {
                        Image(systemName: site.isOff ? "circle" : "circle.fill")
                            .font(.system(size: 7))
                            .foregroundStyle(site.color.color)
                        Text(site.name)
                            .font(.callout)
                            .lineLimit(1)
                        Spacer(minLength: 6)
                        Text(site.label)
                            .font(.subheadline)
                            .foregroundStyle(site.state == "running" ? .secondary : site.color == .gray ? .secondary : .primary)
                        Text(domainsText(site))
                            .font(.subheadline)
                            .foregroundStyle(.secondary)
                            .monospacedDigit()
                            .frame(minWidth: 58, alignment: .trailing)
                    }
                    ForEach(Array(site.domains.filter(\.isFailing).enumerated()), id: \.offset) { _, d in
                        Text(failingText(d))
                            .font(.subheadline)
                            .foregroundStyle(d.watcher == "down" ? Color.red : Color.secondary)
                            .padding(.leading, 13)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
                .help(site.domains.map { "\($0.name): \(watcherWords[$0.watcher] ?? $0.watcher)" }.joined(separator: "\n"))
            }
        }
    }

    private func domainsText(_ site: SiteStatus) -> String {
        let n = site.domains.count
        let failing = site.domains.filter(\.isFailing).count
        if n == 0 {
            return "alan adı yok"
        }
        if failing > 0 {
            return "✗ \(failing) / \(n)"
        }
        if site.domains.allSatisfy(\.isOpen) {
            return "✓ \(n) alan adı"
        }
        return "\(n) alan adı"
    }

    private func failingText(_ d: DomainStatus) -> String {
        var text = "\(d.name): " + (d.watcher == "down" ? "nöbetçiye göre açılmıyor" : "nöbetçi son yoklamada ulaşamadı")
        if let r = d.reason, !r.isEmpty {
            text += " (\(r))"
        }
        if let since = d.since {
            text += ", \(Fmt.clock(since, now: now))'dan beri"
        }
        return text
    }
}

let watcherWords = ["ok": "nöbetçi açık görüyor", "blind": "açık (Cloudflare doğrulaması)", "failing": "nöbetçi ulaşamadı",
                    "down": "nöbetçiye göre açılmıyor", "unknown": "nöbetçinin bilgisi yok"]

/// The watcher in one line: the domains it sees open, its last visit.
private struct WatcherLine: View {
    let servers: [ServerStatus]
    let now: Date

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Image(systemName: "binoculars")
                .font(.subheadline)
                .foregroundStyle(.secondary)
                .frame(width: 10)
            Text(text)
                .font(.subheadline)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 6)
        .help("Dış nöbetçi (Cloudflare Worker) alan adlarını dışarıdan birkaç dakikada bir yoklar.")
    }

    private var text: String {
        let fresh = servers.compactMap { s in s.freshWatcher.map { (s, $0) } }
        let quiet = servers.filter(\.watcherStale).map(\.name)
        let unread = servers.filter { $0.watcher == nil }.map(\.name)
        var parts: [String] = []
        if !fresh.isEmpty {
            let open = fresh.reduce(0) { $0 + $1.1.domains.filter(\.ok).count }
            let total = fresh.reduce(0) { $0 + $1.1.domains.count }
            var line = "Nöbetçi: \(open) / \(total) alan adı açık"
            let times = fresh.map { $0.0.watcherAt ?? $0.1.time }
            if let last = times.max() {
                line += " · son yoklama \(Fmt.clock(last, now: now))"
            }
            parts.append(line)
        }
        if !quiet.isEmpty {
            parts.append("\(quiet.joined(separator: ", ")) için nöbetçiden haber yok")
        }
        if !unread.isEmpty {
            parts.append(unread.count == servers.count ? "Nöbetçinin dosyası okunamadı"
                : "\(unread.joined(separator: ", ")) için nöbetçinin dosyası okunamadı")
        }
        return parts.joined(separator: " · ")
    }
}

// MARK: - the visit from the Mac

private struct ProbeSection: View {
    let monitor: Monitor
    let now: Date
    @Binding var all: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            switch monitor.probe {
            case .idle:
                startButton(title: "Siteleri şimdi yokla", note: "Bu Mac'ten alan adı başına bir ziyaret")
            case .running(let done, let total, let failed):
                running(done: done, total: total, failed: failed)
            case .finished(let run):
                finished(run)
            case .failed(let message, let at):
                startButton(title: "Yeniden yokla", note: nil)
                Notice(light: .gray, text: "Yoklama \(Fmt.clock(at, now: now)): \(message)", font: .subheadline)
                    .padding(.horizontal, 14)
            }
        }
    }

    private func startButton(title: String, note: String?) -> some View {
        Button {
            monitor.startProbe()
        } label: {
            HStack(spacing: 8) {
                Image(systemName: "dot.radiowaves.left.and.right")
                    .frame(width: 18)
                VStack(alignment: .leading, spacing: 1) {
                    Text(title)
                    if let note {
                        Text(note)
                            .font(.subheadline)
                            .foregroundStyle(.secondary)
                    }
                }
            }
        }
        .buttonStyle(MenuRowStyle())
        .padding(.horizontal, 6)
    }

    private func running(done: Int, total: Int, failed: [Probe]) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text(total > 0 ? "Yoklanıyor… \(done) / \(total)" : "Yoklama başlıyor…")
                    .monospacedDigit()
                Spacer()
                Button("Durdur") {
                    monitor.stopProbe()
                }
                .buttonStyle(SmallButtonStyle())
            }
            Bar(fraction: total > 0 ? Double(done) / Double(total) : 0)
            ForEach(failed) { p in
                ProbeLine(probe: p)
            }
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 4)
    }

    private func finished(_ run: ProbeRun) -> some View {
        let failed = run.domains.filter { !$0.ok }
        let light: Light = run.total == 0 ? .gray : failed.isEmpty ? .green : .red
        return VStack(alignment: .leading, spacing: 5) {
            HStack(alignment: .firstTextBaseline) {
                Notice(light: light, text: summary(run), font: .body)
                Spacer(minLength: 6)
                Button("Yeniden") {
                    monitor.startProbe()
                }
                .buttonStyle(SmallButtonStyle())
            }
            ForEach(failed) { p in
                ProbeLine(probe: p)
            }
            ForEach(run.notes, id: \.self) { n in
                Text("⚠︎ " + n)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if !run.skipped.isEmpty {
                Text("Atlanan: " + run.skipped.map { "\($0.site) (\($0.serverName), \($0.label))" }.joined(separator: ", "))
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if !run.domains.isEmpty {
                Button {
                    withAnimation(.snappy(duration: 0.2)) {
                        all.toggle()
                    }
                } label: {
                    HStack(spacing: 4) {
                        Image(systemName: "chevron.right")
                            .font(.caption.weight(.semibold))
                            .rotationEffect(.degrees(all ? 90 : 0))
                        Text(all ? "Sonuçları gizle" : "Tüm sonuçlar (\(run.domains.count))")
                    }
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                }
                .buttonStyle(.plain)
                if all {
                    AllProbes(run: run)
                }
            }
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 4)
    }

    private func summary(_ run: ProbeRun) -> String {
        var text = "Yoklama \(Fmt.clock(run.time, now: now)) · \(run.open) / \(run.total) açık · \(Fmt.duration(ms: run.tookMs))"
        if run.total == 0 {
            text = "Yoklama \(Fmt.clock(run.time, now: now)) · yoklanacak alan adı yok"
        }
        return text
    }
}

/// A domain the Mac could not reach: why, and what the watcher's view adds.
private struct ProbeLine: View {
    let probe: Probe

    var body: some View {
        VStack(alignment: .leading, spacing: 1) {
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Image(systemName: "xmark")
                    .font(.system(size: 9, weight: .bold))
                    .foregroundStyle(.red)
                Text(probe.domain)
                    .font(.callout)
                Text("· \(probe.serverName)")
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }
            Text(probe.reason ?? probe.code)
                .font(.subheadline)
                .padding(.leading, 15)
                .fixedSize(horizontal: false, vertical: true)
            if let v = probe.verdict {
                Text("→ " + v)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                    .padding(.leading, 15)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }
}

/// Every domain visited, a server at a time.
private struct AllProbes: View {
    let run: ProbeRun

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            ForEach(Array(groups.enumerated()), id: \.offset) { _, group in
                let (name, list) = group
                Text(name)
                    .font(.subheadline.weight(.semibold))
                    .padding(.top, 4)
                ForEach(list) { p in
                    HStack(spacing: 6) {
                        Image(systemName: p.ok ? "checkmark" : "xmark")
                            .font(.system(size: 9, weight: .bold))
                            .foregroundStyle(p.ok ? Color.green : Color.red)
                            .frame(width: 10)
                        Text(p.domain)
                            .lineLimit(1)
                            .truncationMode(.middle)
                        Spacer(minLength: 6)
                        Text(p.ok ? "\(p.code) · \(Fmt.duration(ms: p.ms))" : p.code)
                            .monospacedDigit()
                            .foregroundStyle(.secondary)
                        Image(systemName: watcherSymbol(p.watcher))
                            .font(.system(size: 9))
                            .foregroundStyle(.secondary)
                            .frame(width: 12)
                            .help("Nöbetçi: " + (watcherWords[p.watcher] ?? p.watcher))
                    }
                    .font(.subheadline)
                }
            }
        }
    }

    private var groups: [(String, [Probe])] {
        var order: [String] = []
        var by: [String: [Probe]] = [:]
        for p in run.domains {
            if by[p.serverName] == nil {
                order.append(p.serverName)
            }
            by[p.serverName, default: []].append(p)
        }
        return order.map { ($0, by[$0] ?? []) }
    }

    private func watcherSymbol(_ w: String) -> String {
        switch w {
        case "ok", "blind": "binoculars.fill"
        case "failing", "down": "exclamationmark.triangle"
        default: "minus"
        }
    }
}

/// A thin progress bar, drawn (no AppKit control: the image tests draw it too).
struct Bar: View {
    let fraction: Double

    var body: some View {
        GeometryReader { g in
            ZStack(alignment: .leading) {
                Capsule().fill(Color.primary.opacity(0.1))
                Capsule().fill(Color.accentColor)
                    .frame(width: max(4, g.size.width * min(max(fraction, 0), 1)))
            }
        }
        .frame(height: 4)
    }
}

// MARK: - styles

/// A row of the menu: highlighted under the pointer, as the system's menus.
struct MenuRowStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        Row(configuration: configuration)
    }

    private struct Row: View {
        let configuration: Configuration
        @State private var hover = false
        @Environment(\.isEnabled) private var enabled

        var body: some View {
            configuration.label
                .padding(.horizontal, 8)
                .padding(.vertical, 5)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(
                    RoundedRectangle(cornerRadius: 6, style: .continuous)
                        .fill(Color.primary.opacity(configuration.isPressed ? 0.14 : hover && enabled ? 0.07 : 0))
                )
                .contentShape(Rectangle())
                .onHover { hover = $0 }
                .opacity(enabled ? 1 : 0.5)
        }
    }
}

/// A small round button with an icon (refresh, panel, SSH).
struct IconButtonStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        Icon(configuration: configuration)
    }

    private struct Icon: View {
        let configuration: Configuration
        @State private var hover = false

        var body: some View {
            configuration.label
                .font(.system(size: 12, weight: .medium))
                .foregroundStyle(.secondary)
                .frame(width: 26, height: 26)
                .background(
                    Circle().fill(Color.primary.opacity(configuration.isPressed ? 0.14 : hover ? 0.08 : 0))
                )
                .contentShape(Circle())
                .onHover { hover = $0 }
        }
    }
}

/// A small bordered button (Durdur, Yeniden).
struct SmallButtonStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.subheadline)
            .padding(.horizontal, 9)
            .padding(.vertical, 3)
            .background(
                Capsule().fill(Color.primary.opacity(configuration.isPressed ? 0.16 : 0.08))
            )
            .contentShape(Capsule())
    }
}
