import AppKit
import SwiftUI

/// The Backups window (panel-tasarim §29.5): a server's sites and their backups, one
/// opened into a folder or into the Mac's MongoDB, what was opened on this Mac, the
/// recovery line. Drawn of plain views and AppKit's cheapest controls (buttons,
/// checkboxes, one text field): no popover, no switch — on macOS 26 each took ~140 MB of
/// graphics memory as it was drawn (§29.13).
public struct BackupsWindow: View {
    @Bindable var model: BackupsModel
    let actions: BackupsActions
    /// The clock for "3 gün önce" (fixed in the image tests).
    let fixedNow: Date?

    public static let size = NSSize(width: 900, height: 640)

    public init(model: BackupsModel, actions: BackupsActions, now: Date? = nil) {
        self.model = model
        self.actions = actions
        fixedNow = now
    }

    public var body: some View {
        VStack(spacing: 0) {
            BackupsHeader(model: model)
            Divider()
            if let e = model.serversError, model.servers.isEmpty {
                Notice(light: .gray, text: e)
                    .padding(16)
                Spacer()
            } else {
                HStack(spacing: 0) {
                    SiteColumn(model: model)
                        .frame(width: 230)
                    Divider()
                    PointsColumn(model: model, actions: actions)
                }
            }
            Divider()
            OpenedSection(list: model.opened, reveal: actions.reveal, now: fixedNow ?? Date())
        }
        .background(Color(nsColor: .windowBackgroundColor))
    }
}

/// What the window's buttons do outside it; the app gives them (the image tests none).
public struct BackupsActions {
    /// Shows a folder in the Finder.
    public var reveal: (String) -> Void

    public init(reveal: @escaping (String) -> Void) {
        self.reveal = reveal
    }

    public static var none: BackupsActions { BackupsActions(reveal: { _ in }) }
}

// MARK: - the head: servers, the list's state, the recovery line

private struct BackupsHeader: View {
    @Bindable var model: BackupsModel

    var body: some View {
        HStack(alignment: .center, spacing: 12) {
            HStack(spacing: 4) {
                ForEach(model.servers) { s in
                    Button {
                        model.select(server: s.code)
                    } label: {
                        Text(s.name)
                            .font(.headline)
                            .lineLimit(1)
                            .padding(.horizontal, 12)
                            .padding(.vertical, 4)
                            .background(
                                Capsule().fill(model.server == s.code ? Color.accentColor.opacity(0.22) : Color.primary.opacity(0.06))
                            )
                            .contentShape(Capsule())
                    }
                    .buttonStyle(.plain)
                    .help(s.bucket)
                    .disabled(model.isBusy && model.server != s.code)
                }
            }
            VStack(alignment: .leading, spacing: 1) {
                Text(listLine)
                    .font(.callout)
                Text(model.list.map { "okundu \(Fmt.clock($0.time, now: Date())) · \(Fmt.duration(ms: $0.tookMs))" } ?? " ")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            .lineLimit(1)
            Button {
                model.refreshList()
            } label: {
                if let s = model.server, model.readingList.contains(s) {
                    ProgressView().controlSize(.small)
                        .frame(width: 16, height: 16)
                } else {
                    Image(systemName: "arrow.clockwise")
                        .frame(width: 16, height: 16)
                }
            }
            .buttonStyle(IconButtonStyle())
            .help("Yedek listesini yeniden oku")
            .disabled(model.server.map { model.readingList.contains($0) } ?? true)
            Spacer(minLength: 8)
            RecoveryButton(model: model)
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 10)
    }

    private var listLine: String {
        guard let code = model.server, let s = model.servers.first(where: { $0.code == code }) else { return "Sunucu yok" }
        if !s.password {
            return "\(s.bucket) · depo şifresi Anahtar Zinciri'nde yok (restic-\(s.code))"
        }
        if let l = model.list {
            return "\(l.bucket) · \(l.snapshots) anlık görüntü · \(l.sites.count) site"
        }
        return model.readingList.contains(code) ? "\(s.bucket) · yedek deposu okunuyor…" : s.bucket
    }
}

/// The recovery line onto the pasteboard, never shown (panel-tasarim §29.5).
private struct RecoveryButton: View {
    let model: BackupsModel

    var body: some View {
        VStack(alignment: .trailing, spacing: 2) {
            Button {
                model.copyRecovery()
            } label: {
                Label("Kurtarma satırını kopyala", systemImage: "key")
            }
            .disabled(model.server == nil || model.recovery == .copying || !(model.servers.first { $0.code == model.server }?.password ?? false))
            .help("\"Yedekten kur\" için bu sunucunun kurtarma bilgisi (sır içerir): yalnız panoya, ekranda görünmez")
            Text(note)
                .font(.caption)
                .foregroundStyle(noteColor)
                .lineLimit(2)
                .multilineTextAlignment(.trailing)
                .frame(maxWidth: 300, alignment: .trailing)
        }
    }

    private var note: String {
        switch model.recovery {
        case .idle: "Yalnız panoya; 1 dk sonra silinir"
        case .copying: "Alınıyor…"
        case .copied(let server, _):
            "\(model.servers.first { $0.code == server }?.name ?? server.uppercased()) panoda — 1 dk sonra silinecek"
        case .cleared: "Pano temizlendi"
        case .kept: "Pano değişmişti: dokunulmadı"
        case .failed(let e): e
        }
    }

    private var noteColor: Color {
        switch model.recovery {
        case .failed: .red
        case .copied: .orange
        default: .secondary
        }
    }
}

// MARK: - the sites

private struct SiteColumn: View {
    @Bindable var model: BackupsModel

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Text("SİTELER")
                .font(.caption.weight(.semibold))
                .foregroundStyle(.secondary)
                .padding(.horizontal, 14)
                .padding(.top, 10)
                .padding(.bottom, 4)
            if let e = model.listError {
                Notice(light: .gray, text: e)
                    .padding(.horizontal, 14)
                    .padding(.vertical, 6)
            }
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 1) {
                    ForEach(model.list?.sites ?? []) { s in
                        Button {
                            model.select(site: s.name)
                        } label: {
                            HStack(alignment: .firstTextBaseline) {
                                VStack(alignment: .leading, spacing: 1) {
                                    Text(s.name)
                                        .lineLimit(1)
                                    if let last = s.points.first {
                                        Text("son: \(Fmt.dateTime(last.time))")
                                            .font(.caption)
                                            .foregroundStyle(.secondary)
                                    }
                                }
                                Spacer(minLength: 4)
                                Text("\(s.points.count)")
                                    .font(.caption.monospacedDigit())
                                    .foregroundStyle(.secondary)
                            }
                        }
                        .buttonStyle(SelectableRowStyle(selected: model.site == s.name))
                        .disabled(model.isBusy)
                    }
                    if let l = model.list, l.sites.isEmpty {
                        Text("Bu depoda FootOps'un site yedeği yok.")
                            .font(.callout)
                            .foregroundStyle(.secondary)
                            .padding(14)
                    }
                    if let at = model.list?.serverPart {
                        Text("Sunucu ayarları: \(Fmt.dateTime(at))")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .padding(.horizontal, 14)
                            .padding(.top, 8)
                    }
                }
                .padding(.horizontal, 6)
                .padding(.bottom, 8)
            }
        }
    }
}

// MARK: - a site's backups, and opening one

private struct PointsColumn: View {
    @Bindable var model: BackupsModel
    let actions: BackupsActions

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            if let site = model.siteBackups {
                HStack(alignment: .firstTextBaseline) {
                    Text(site.name)
                        .font(.title3.weight(.semibold))
                    Text("\(site.points.count) yedek")
                        .foregroundStyle(.secondary)
                }
                .padding(.horizontal, 16)
                .padding(.top, 10)
                .padding(.bottom, 6)
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 1) {
                        ForEach(site.points) { p in
                            Button {
                                model.select(run: p.run)
                            } label: {
                                PointRow(point: p)
                            }
                            .buttonStyle(SelectableRowStyle(selected: model.run == p.run))
                            .disabled(model.isBusy)
                        }
                    }
                    .padding(.horizontal, 8)
                    .padding(.bottom, 6)
                }
                Divider()
                if let p = model.point {
                    OpenPanel(model: model, point: p, actions: actions)
                        .padding(16)
                }
            } else {
                Spacer()
                Text(emptyText)
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
                    .frame(maxWidth: 420)
                    .frame(maxWidth: .infinity)
                Spacer()
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }
}

extension PointsColumn {
    /// What the column says before there is a site to show.
    var emptyText: String {
        guard let code = model.server, let s = model.servers.first(where: { $0.code == code }) else { return "" }
        if !s.password {
            return "Bu sunucunun depo şifresi Anahtar Zinciri'nde yok (footops-yedek / restic-\(s.code)): yedekleri açılamaz."
        }
        if model.list == nil {
            return model.readingList.contains(code)
                ? "Yedek deposu okunuyor… İlk okuma 10-20 sn sürebilir; sonrakiler önbellekten, birkaç saniye."
                : (model.listError == nil ? "" : "Yedek listesi okunamadı: yeniden dene (↻).")
        }
        return "Bir site seç: yedekleri burada listelenir."
    }
}

private struct PointRow: View {
    let point: BackupPoint

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            Text(Fmt.dateTime(point.time))
                .monospacedDigit()
                .frame(width: 128, alignment: .leading)
            Text(point.kindText)
                .foregroundStyle(.secondary)
                .frame(width: 56, alignment: .leading)
            Text(PartText.summary(point.parts))
                .foregroundStyle(.secondary)
                .lineLimit(1)
                .truncationMode(.tail)
            Spacer(minLength: 0)
        }
        .font(.callout)
    }
}

enum PartText {
    static let order = ["db", "files", "def"]

    static func name(_ part: String) -> String {
        switch part {
        case "db": "Veritabanı"
        case "files": "Dosyalar"
        case "def": "Ayarlar"
        case "mongo": "MongoDB'ye yükleme"
        case "check": "Denetim"
        default: part
        }
    }

    static func detail(_ part: String, _ p: BackupPart) -> String {
        switch part {
        case "db": Fmt.bytes(p.bytes)
        case "files": "\(p.files) dosya, \(Fmt.bytes(p.bytes))"
        case "def": "env, footops.json"
        default: ""
        }
    }

    static func summary(_ parts: [String: BackupPart]) -> String {
        order.compactMap { k in parts[k].map { "\(name(k).lowercased(with: Locale(identifier: "tr_TR"))) \(detail(k, $0))" } }
            .joined(separator: " · ")
    }
}

/// The chosen backup: its parts to tick, the two ways to open it, and how it goes.
private struct OpenPanel: View {
    @Bindable var model: BackupsModel
    let point: BackupPoint
    let actions: BackupsActions

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("\(Fmt.dateTime(point.time)) · \(point.kindText) yedeği")
                .font(.headline)
            HStack(spacing: 16) {
                ForEach(PartText.order.filter { point.parts[$0] != nil }, id: \.self) { part in
                    // A checkbox, not a switch (§29.13).
                    Toggle(isOn: Binding(get: { model.parts.contains(part) }, set: { on in
                        if on {
                            model.parts.insert(part)
                        } else {
                            model.parts.remove(part)
                        }
                    })) {
                        Text("\(PartText.name(part)) (\(PartText.detail(part, point.parts[part]!)))")
                    }
                    .toggleStyle(.checkbox)
                }
            }
            .disabled(model.isBusy)
            HStack(spacing: 10) {
                Button("Klasöre aç") {
                    model.openFolder()
                }
                .keyboardShortcut(.defaultAction)
                .disabled(model.isBusy || model.parts.isEmpty)
                .help("İndirilenler'e <site>-<zaman> klasörü (yalnız senin): seçili parçalar, panelin İndir'iyle aynı biçimde")
                Button("Mac'teki MongoDB'ye yükle") {
                    model.loadDatabase()
                }
                .disabled(model.isBusy || point.parts["db"] == nil)
                .help("Veritabanı <site>_yedek_<tarih>_<saat> adıyla yüklenir; klasör bırakmaz")
                Spacer()
            }
            OperationView(operation: model.operation, otherName: $model.otherName, idle: "Yedekler yalnız okunur: hiçbir yedek değişmez ya da silinmez.",
                          stop: model.stop, retry: model.retryLoad, dismiss: model.dismiss, reveal: actions.reveal)
        }
    }
}

/// An open's state (a backup's or an archive's): under way, the database's name taken,
/// done, failed.
struct OperationView: View {
    let operation: BackupsModel.Operation
    @Binding var otherName: String
    /// What is said while nothing runs.
    let idle: String
    let stop: () -> Void
    let retry: (_ name: String, _ overwrite: Bool) -> Void
    let dismiss: () -> Void
    let reveal: (String) -> Void

    var body: some View {
        switch operation {
        case .idle:
            Text(idle)
                .font(.caption)
                .foregroundStyle(.secondary)
        case .running(let r):
            VStack(alignment: .leading, spacing: 6) {
                HStack {
                    Text(runningText(r))
                        .font(.callout)
                        .lineLimit(1)
                        .truncationMode(.middle)
                    Spacer()
                    Button("Durdur") {
                        stop()
                    }
                    .buttonStyle(SmallButtonStyle())
                }
                Bar(fraction: r.total > 0 ? Double(r.done) / Double(r.total) : 0)
                Text("Pencere kapanırsa durdurulur; yarım kalan silinir.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        case .asking(let name):
            VStack(alignment: .leading, spacing: 8) {
                Notice(light: .yellow, text: "Mac'teki MongoDB'de \(name) adlı veritabanı var.")
                HStack(spacing: 8) {
                    Button("Üzerine yaz") {
                        retry(name, true)
                    }
                    Text("ya da")
                        .foregroundStyle(.secondary)
                    TextField("Başka ad", text: $otherName)
                        .textFieldStyle(.roundedBorder)
                        .frame(width: 240)
                        .onSubmit { retry(otherName, false) }
                    Button("Bu adla yükle") {
                        retry(otherName, false)
                    }
                    .disabled(otherName.isEmpty)
                    Button("Vazgeç") {
                        dismiss()
                    }
                }
            }
            .padding(10)
            .background(RoundedRectangle(cornerRadius: 8).fill(Color.yellow.opacity(0.12)))
        case .finished(let res):
            VStack(alignment: .leading, spacing: 6) {
                if let target = res.target {
                    HStack(spacing: 8) {
                        Label("Açıldı: \(Fmt.home(target))", systemImage: "checkmark.circle.fill")
                            .foregroundStyle(.green)
                            .lineLimit(1)
                            .truncationMode(.middle)
                        Button("Finder'da göster") {
                            reveal(target)
                        }
                        .buttonStyle(SmallButtonStyle())
                    }
                }
                if let db = res.database {
                    Label("MongoDB'ye yüklendi: \(db.name) — \(db.documents) belge" + (db.failed > 0 ? ", \(db.failed) belge yüklenemedi" : "")
                        + (db.replaced ? " (eskisinin yerine)" : "") + manifestNote(db), systemImage: "checkmark.circle.fill")
                        .foregroundStyle(db.failed > 0 || mismatch(db) ? .orange : .green)
                }
                Text(openedText(res))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                ForEach(res.warnings, id: \.self) { w in
                    Notice(light: .yellow, text: w, font: .caption)
                }
            }
        case .failed(let e):
            Label(e, systemImage: "exclamationmark.triangle.fill")
                .foregroundStyle(.red)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    /// An archive's manifest counted the documents: the same, or not.
    private func manifestNote(_ db: LoadedDB) -> String {
        guard db.expected > 0 else { return "" }
        return mismatch(db) ? " (manifestte \(db.expected))" : ", manifestle aynı"
    }

    private func mismatch(_ db: LoadedDB) -> Bool { db.expected > 0 && db.documents + db.failed != db.expected }

    private func runningText(_ r: BackupsModel.Running) -> String {
        guard let part = r.part else {
            return "\(r.site): yedek deposu okunuyor…"
        }
        var text = "\(r.site) · \(PartText.name(part))"
        if r.total > 0 {
            text += " · \(Fmt.bytes(r.done)) / \(Fmt.bytes(r.total))"
        }
        return text
    }

    /// What was written of each part (the backup's time is the panel's head).
    private func openedText(_ res: OpenResult) -> String {
        var parts: [String] = []
        for k in PartText.order {
            guard let p = res.parts[k] else { continue }
            switch k {
            case "files": parts.append("\(PartText.name(k)) \(p.count) dosya, \(Fmt.bytes(p.written))")
            case "def": parts.append("\(PartText.name(k)) \(p.count) değişken")
            default: parts.append(p.written > 0 ? "\(PartText.name(k)) \(Fmt.bytes(p.written))" : PartText.name(k))
            }
        }
        return (parts + [Fmt.duration(ms: res.tookMs)]).joined(separator: " · ")
    }
}

// MARK: - styles

/// A row to choose: highlighted while chosen, and under the pointer.
struct SelectableRowStyle: ButtonStyle {
    let selected: Bool

    func makeBody(configuration: Configuration) -> some View {
        Row(configuration: configuration, selected: selected)
    }

    private struct Row: View {
        let configuration: Configuration
        let selected: Bool
        @State private var hover = false
        @Environment(\.isEnabled) private var enabled

        var body: some View {
            configuration.label
                .padding(.horizontal, 8)
                .padding(.vertical, 5)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(
                    RoundedRectangle(cornerRadius: 6, style: .continuous)
                        .fill(selected ? Color.accentColor.opacity(0.2)
                            : Color.primary.opacity(configuration.isPressed ? 0.12 : hover && enabled ? 0.06 : 0))
                )
                .contentShape(Rectangle())
                .onHover { hover = $0 }
                .opacity(enabled || selected ? 1 : 0.6)
        }
    }
}
