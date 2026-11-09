import AppKit
import SwiftUI

/// The Archives window (panel-tasarim §29.6): an archive dropped on it (or chosen) is
/// checked as the panel's import checks an upload, then opened into a folder or loaded
/// into the Mac's MongoDB; an archive is made of a database of the Mac's MongoDB and a
/// folder; the panel's import page opens for it; what was opened on this Mac. Drawn of
/// plain views and AppKit's cheapest controls, as the Backups window (no popover, no
/// switch: §29.13).
public struct ArchivesWindow: View {
    @Bindable var model: ArchivesModel
    let actions: ArchivesActions
    /// The clock for "3 gün önce" (fixed in the image tests).
    let fixedNow: Date?

    public static let size = NSSize(width: 900, height: 780)

    public init(model: ArchivesModel, actions: ArchivesActions, now: Date? = nil) {
        self.model = model
        self.actions = actions
        fixedNow = now
    }

    public var body: some View {
        VStack(spacing: 0) {
            ArchivesHeader(model: model)
            Divider()
            ScrollView {
                Group {
                    switch model.tab {
                    case .open:
                        OpenTab(model: model, actions: actions)
                    case .make:
                        MakeTab(model: model, actions: actions)
                    }
                }
                .padding(16)
                .frame(maxWidth: .infinity, alignment: .topLeading)
            }
            Divider()
            OpenedSection(list: model.opened, reveal: actions.reveal, now: fixedNow ?? Date())
        }
        .background(Color(nsColor: .windowBackgroundColor))
    }
}

/// What the window's buttons do outside it; the app gives them (the image tests none).
public struct ArchivesActions {
    /// Shows a file or a folder in the Finder.
    public var reveal: (String) -> Void
    /// Opens a page in the browser (a panel's import page).
    public var open: (URL) -> Void
    /// Asks for an archive, or a folder, to the model.
    public var chooseFile: () -> Void
    public var chooseFolder: () -> Void

    public init(reveal: @escaping (String) -> Void, open: @escaping (URL) -> Void, chooseFile: @escaping () -> Void,
                chooseFolder: @escaping () -> Void) {
        self.reveal = reveal
        self.open = open
        self.chooseFile = chooseFile
        self.chooseFolder = chooseFolder
    }

    public static var none: ArchivesActions { ArchivesActions(reveal: { _ in }, open: { _ in }, chooseFile: {}, chooseFolder: {}) }
}

// MARK: - the head

private struct ArchivesHeader: View {
    @Bindable var model: ArchivesModel

    var body: some View {
        HStack(spacing: 4) {
            tab("Arşivi aç ya da doğrula", .open)
            tab("Arşiv oluştur", .make)
            Spacer()
            Text("Arşiv dosyası yalnız okunur; panele hiçbir şey gönderilmez.")
                .font(.caption)
                .foregroundStyle(.secondary)
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 10)
    }

    private func tab(_ title: String, _ tab: ArchivesModel.Tab) -> some View {
        Button {
            model.tab = tab
        } label: {
            Text(title)
                .font(.headline)
                .padding(.horizontal, 12)
                .padding(.vertical, 4)
                .background(Capsule().fill(model.tab == tab ? Color.accentColor.opacity(0.22) : Color.primary.opacity(0.06)))
                .contentShape(Capsule())
        }
        .buttonStyle(.plain)
    }
}

// MARK: - opening an archive

private struct OpenTab: View {
    @Bindable var model: ArchivesModel
    let actions: ArchivesActions

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            DropZone(title: model.info == nil ? "Bir arşivi buraya bırak" : "Başka bir arşiv: buraya bırak",
                     detail: "FootOps arşivi (.footops.tar: panelin Dışa aktar'ı, yedekten İndir), mongodump arşivi (.archive, .gz) ya da dosya arşivi (zip, tar.gz)",
                     button: "Dosya seç…", icon: "doc.zipper", compact: model.info != nil, enabled: !model.isBusy,
                     drop: { model.choose(file: $0.path) }, choose: actions.chooseFile)
            switch model.check {
            case .none:
                VStack(alignment: .leading, spacing: 6) {
                    Text("Arşiv önce panelin İçe aktar'ının kurallarıyla denetlenir (tür içerikten, sağlamalar, yollar, bağlantılar, bomba), sonra açılır.")
                    Text("Yeni bir site için arşiv: Arşiv oluştur — Mac'teki veritabanından ve sitenin klasöründen.")
                }
                .font(.callout)
                .foregroundStyle(.secondary)
            case .checking(let path, let done, let total):
                VStack(alignment: .leading, spacing: 6) {
                    HStack {
                        Text("Denetleniyor: \((path as NSString).lastPathComponent)" + (total > 0 ? " · \(Fmt.bytes(done)) / \(Fmt.bytes(total))" : ""))
                            .lineLimit(1)
                            .truncationMode(.middle)
                        Spacer()
                        Button("Durdur") {
                            model.stop()
                        }
                        .buttonStyle(SmallButtonStyle())
                    }
                    Bar(fraction: total > 0 ? Double(done) / Double(total) : 0)
                }
            case .failed(let path, let message):
                VStack(alignment: .leading, spacing: 4) {
                    Text((path as NSString).lastPathComponent)
                        .font(.headline)
                    Label(message, systemImage: "xmark.octagon.fill")
                        .foregroundStyle(.red)
                        .fixedSize(horizontal: false, vertical: true)
                    Text("Bu arşiv panelin İçe aktar'ından da geçmez.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            case .checked(let info):
                ArchiveSummary(info: info, model: model)
                ArchiveActions(info: info, model: model, actions: actions)
                PanelImport(model: model, actions: actions, path: info.path)
            }
        }
    }
}

/// What checking found, as the panel's preview would show it before the trial restore.
private struct ArchiveSummary: View {
    let info: ArchiveInfo
    @Bindable var model: ArchivesModel

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                Image(systemName: info.kind == "files" ? "doc.zipper" : info.kind == "mongodump" ? "cylinder.split.1x2" : "shippingbox")
                    .foregroundStyle(.secondary)
                Text(info.name)
                    .font(.title3.weight(.semibold))
                    .lineLimit(1)
                    .truncationMode(.middle)
                Text("\(info.kindText) · \(Fmt.bytes(info.size))")
                    .foregroundStyle(.secondary)
            }
            Grid(alignment: .leadingFirstTextBaseline, horizontalSpacing: 12, verticalSpacing: 6) {
                if info.kind == "footops" {
                    GridRow {
                        label("Site")
                        Text(siteLine)
                    }
                }
                if let db = info.database {
                    GridRow {
                        label("Veritabanı")
                        VStack(alignment: .leading, spacing: 4) {
                            Text(dbLine(db))
                            if db.databases.count > 1 {
                                Text("Hangisi:" + (db.moreDatabases > 0 ? " (ilk \(db.databases.count); \(db.moreDatabases) tanesi daha Terminal'den: --kaynak)" : ""))
                                    .foregroundStyle(.secondary)
                                LazyVGrid(columns: [GridItem(.adaptive(minimum: 120), spacing: 4, alignment: .leading)], alignment: .leading, spacing: 4) {
                                    ForEach(db.databases, id: \.self) { name in
                                        Button {
                                            model.source = name
                                        } label: {
                                            Text(name)
                                                .padding(.horizontal, 10)
                                                .padding(.vertical, 2)
                                                .background(Capsule().fill(model.source == name ? Color.accentColor.opacity(0.22) : Color.primary.opacity(0.06)))
                                                .contentShape(Capsule())
                                        }
                                        .buttonStyle(.plain)
                                        .disabled(model.isBusy)
                                    }
                                }
                            }
                            Text(collectionsLine(db))
                                .font(.callout)
                                .foregroundStyle(.secondary)
                                .lineLimit(3)
                                .fixedSize(horizontal: false, vertical: true)
                        }
                    }
                }
                if let f = info.files {
                    GridRow {
                        label("Dosyalar")
                        Text(filesLine(f))
                    }
                }
                if info.env {
                    GridRow {
                        label("Ortam değişkenleri")
                        Text("\(info.envVars) değişken — sırlar içerir; içe aktarma onlara dokunmaz")
                    }
                }
                GridRow {
                    label("Bütünlük")
                    Label("tamam · sha256 \(info.sha256.prefix(12))…\(info.sha256.suffix(4)) · \(Fmt.duration(ms: info.tookMs))",
                          systemImage: "checkmark.seal.fill")
                        .foregroundStyle(.green)
                        .help("sha256 \(info.sha256)")
                }
            }
            ForEach(info.notes, id: \.self) { n in
                Notice(light: .yellow, text: n, font: .callout)
            }
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 10).fill(Color.primary.opacity(0.04)))
    }

    private func label(_ s: String) -> some View {
        Text(s)
            .foregroundStyle(.secondary)
            .gridColumnAlignment(.trailing)
    }

    private var siteLine: String {
        var t = info.site ?? "—"
        if let c = info.created {
            t += " · \(Fmt.dateTime(c))"
        }
        if let b = info.backup {
            let kind = BackupPoint(run: "", time: b.time, kind: b.kind, parts: [:]).kindText
            t += " · \(b.server.isEmpty ? "" : b.server.uppercased() + " ")\(kind) yedeğinden"
        }
        if let v = info.footops, !v.isEmpty {
            t += " · FootOps \(v)"
        }
        return t
    }

    private func dbLine(_ db: ArchiveDB) -> String {
        let name = db.name ?? model.source
        let shown = db.collections.filter { name == nil || $0.db == name }
        let views = shown.filter(\.view).count
        var t = "\(name ?? "\(db.databases.count + db.moreDatabases) veritabanı") — \(shown.count - views) koleksiyon"
        if views > 0 {
            t += " + \(views) görünüm"
        }
        t += db.documents >= 0 && db.name == name ? ", \(db.documents) belge (manifestte)" : ", belge sayısı yüklenince belli olur"
        t += " · döküm \(Fmt.bytes(db.bytes))"
        if db.compressed > 0 {
            t += " (sıkıştırılmış \(Fmt.bytes(db.compressed)))"
        }
        if !db.server.isEmpty {
            t += " · MongoDB \(db.server)"
        }
        return t
    }

    private func collectionsLine(_ db: ArchiveDB) -> String {
        let name = db.name ?? model.source
        let shown = db.collections.filter { name == nil || $0.db == name }
        var parts = shown.prefix(40).map { c in
            let label = name == nil ? "\(c.db).\(c.name)" : c.name
            return c.view ? "\(label) (görünüm)" : c.documents >= 0 ? "\(label) \(c.documents)" : label
        }
        let more = shown.count - parts.count + db.more
        if more > 0 {
            parts.append("… ve \(more) daha")
        }
        return parts.joined(separator: " · ")
    }

    private func filesLine(_ f: ArchiveFiles) -> String {
        var t = "\(f.files - f.hidden) dosya, \(f.dirs) klasör, \(Fmt.bytes(f.bytes - f.hiddenBytes))"
        if f.compressed > 0, info.kind == "footops" {
            t += " (sıkıştırılmış \(Fmt.bytes(f.compressed)))"
        }
        if f.hidden > 0 {
            t += " — \(f.hidden) gizli dosya alınmaz"
        }
        return t
    }
}

/// The ways to open a checked archive.
private struct ArchiveActions: View {
    let info: ArchiveInfo
    @Bindable var model: ArchivesModel
    let actions: ArchivesActions

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            if !info.parts.isEmpty {
                HStack(spacing: 16) {
                    ForEach(info.parts, id: \.self) { part in
                        // A checkbox, not a switch (§29.13).
                        Toggle(isOn: Binding(get: { model.parts.contains(part) }, set: { on in
                            if on {
                                model.parts.insert(part)
                            } else {
                                model.parts.remove(part)
                            }
                        })) {
                            Text(partLabel(part))
                        }
                        .toggleStyle(.checkbox)
                    }
                }
                .disabled(model.isBusy)
            }
            HStack(spacing: 10) {
                if info.kind == "footops" {
                    Button("Klasöre aç") {
                        model.openFolder()
                    }
                    .keyboardShortcut(.defaultAction)
                    .disabled(model.isBusy || model.parts.isEmpty)
                    .help("İndirilenler'e <site>-<zaman> klasörü (yalnız senin): db.archive, files/, env, manifest.json")
                }
                if info.database != nil {
                    Button("Mac'teki MongoDB'ye yükle") {
                        model.loadDatabase()
                    }
                    .disabled(model.isBusy || (info.database?.name == nil && model.source == nil))
                    .help("Veritabanı <site>_arsiv_<tarih>_<saat> adıyla yüklenir; sayım manifestle karşılaştırılır")
                }
                Button("Finder'da göster") {
                    actions.reveal(info.path)
                }
                Spacer()
            }
            if info.kind != "files" {
                OperationView(operation: model.operation, otherName: $model.otherName,
                              idle: info.kind == "footops" ? "Arşiv dosyası değişmez; açılan klasör yalnız senin, Time Machine'e girmez."
                                  : "Arşiv dosyası değişmez; veritabanı <ad>_arsiv_<tarih>_<saat> adıyla yüklenir.",
                              stop: model.stop, retry: model.retryLoad, dismiss: model.dismiss, reveal: actions.reveal)
            }
        }
    }

    private func partLabel(_ part: String) -> String {
        switch part {
        case "db": "Veritabanı (\(Fmt.bytes(info.database?.bytes ?? 0)))"
        case "files": "Dosyalar (\((info.files?.files ?? 0) - (info.files?.hidden ?? 0)) dosya)"
        case "def": "Ayarlar (env, \(info.envVars) değişken)"
        default: part
        }
    }
}

// MARK: - making an archive

private struct MakeTab: View {
    @Bindable var model: ArchivesModel
    let actions: ArchivesActions

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Mac'teki MongoDB'nin bir veritabanından ve / veya sitenin kalıcı klasörü olacak bir klasörden panelin İçe aktar'ına uygun FootOps arşivi üretilir (yeni site: veritabanı Mac'ten).")
                .font(.callout)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            HStack(alignment: .top, spacing: 18) {
                DatabaseList(model: model)
                    .frame(width: 330)
                VStack(alignment: .leading, spacing: 12) {
                    VStack(alignment: .leading, spacing: 6) {
                        Text("KALICI KLASÖR (isteğe bağlı)")
                            .font(.caption.weight(.semibold))
                            .foregroundStyle(.secondary)
                        if let folder = model.makeFolder {
                            HStack(spacing: 8) {
                                Image(systemName: "folder")
                                    .foregroundStyle(.secondary)
                                Text(Fmt.home(folder))
                                    .lineLimit(1)
                                    .truncationMode(.middle)
                                    .help(folder)
                                Spacer()
                                Button("Kaldır") {
                                    model.makeFolder = nil
                                }
                                .buttonStyle(SmallButtonStyle())
                                .disabled(model.isBusy)
                            }
                        }
                        DropZone(title: model.makeFolder == nil ? "Bir klasörü buraya bırak" : "Başka bir klasör: buraya bırak",
                                 detail: "Gizli dosyalar ve bağlantılar alınmaz (panelin Dışa aktar'ı gibi)", button: "Klasör seç…",
                                 icon: "folder.badge.plus", compact: true, enabled: !model.isBusy,
                                 drop: { model.makeFolder = $0.path }, choose: actions.chooseFolder)
                    }
                    VStack(alignment: .leading, spacing: 6) {
                        Text("SİTE ADI")
                            .font(.caption.weight(.semibold))
                            .foregroundStyle(.secondary)
                        TextField("boşsa veritabanının (ya da klasörün) adından", text: $model.makeSite)
                            .textFieldStyle(.roundedBorder)
                            .frame(width: 260)
                            .disabled(model.isBusy)
                        Text("Arşivin adında ve manifestte; içe aktarılan site, panelde açtığın sitedir.")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                    HStack {
                        Button("Arşivi oluştur") {
                            model.makeArchive()
                        }
                        .keyboardShortcut(.defaultAction)
                        .disabled(model.isBusy || (model.makeDB == nil && model.makeFolder == nil))
                        Text(makeWhat)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                    MakeStatus(model: model, actions: actions)
                }
            }
            if case .finished(let res) = model.make {
                PanelImport(model: model, actions: actions, path: res.path)
            }
        }
    }

    private var makeWhat: String {
        switch (model.makeDB, model.makeFolder) {
        case (let db?, _?): "\(db) + klasör → İndirilenler"
        case (let db?, nil): "\(db) (dosyasız) → İndirilenler"
        case (nil, _?): "yalnız dosyalar → İndirilenler"
        default: "bir veritabanı ya da klasör seç"
        }
    }
}

private struct DatabaseList: View {
    @Bindable var model: ArchivesModel

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text("VERİTABANI (Mac'teki MongoDB)")
                    .font(.caption.weight(.semibold))
                    .foregroundStyle(.secondary)
                Spacer()
                Button {
                    model.refreshDatabases()
                } label: {
                    if model.readingDatabases {
                        ProgressView().controlSize(.small)
                            .frame(width: 16, height: 16)
                    } else {
                        Image(systemName: "arrow.clockwise")
                            .frame(width: 16, height: 16)
                    }
                }
                .buttonStyle(IconButtonStyle())
                .help("Yeniden oku")
                .disabled(model.readingDatabases)
            }
            if let e = model.databasesError {
                Notice(light: .gray, text: e, font: .caption)
            }
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 1) {
                    Button {
                        model.makeDB = nil
                    } label: {
                        Text("— Veritabanı yok (yalnız dosyalar)")
                            .foregroundStyle(.secondary)
                    }
                    .buttonStyle(SelectableRowStyle(selected: model.makeDB == nil))
                    ForEach(model.databases?.databases ?? []) { d in
                        Button {
                            model.makeDB = d.name
                        } label: {
                            HStack {
                                Text(d.name)
                                    .lineLimit(1)
                                if d.opened {
                                    Text("yedekten / arşivden")
                                        .font(.caption)
                                        .foregroundStyle(.orange)
                                }
                                Spacer(minLength: 4)
                                Text(Fmt.bytes(d.bytes))
                                    .font(.caption.monospacedDigit())
                                    .foregroundStyle(.secondary)
                            }
                        }
                        .buttonStyle(SelectableRowStyle(selected: model.makeDB == d.name))
                    }
                }
                .padding(4)
            }
            .frame(height: 230)
            .background(RoundedRectangle(cornerRadius: 8).fill(Color.primary.opacity(0.04)))
            .disabled(model.isBusy)
            if let uri = model.databases?.uri, !uri.isEmpty {
                Text(uri)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
    }
}

private struct MakeStatus: View {
    @Bindable var model: ArchivesModel
    let actions: ArchivesActions

    var body: some View {
        switch model.make {
        case .idle:
            EmptyView()
        case .running(let part, let done, let total):
            VStack(alignment: .leading, spacing: 6) {
                HStack {
                    Text(runningText(part, done, total))
                        .lineLimit(1)
                    Spacer()
                    Button("Durdur") {
                        model.stop()
                    }
                    .buttonStyle(SmallButtonStyle())
                }
                Bar(fraction: total > 0 ? Double(done) / Double(total) : 0)
                Text("Pencere kapanırsa durdurulur; yarım arşiv silinir.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        case .finished(let res):
            VStack(alignment: .leading, spacing: 6) {
                HStack(spacing: 8) {
                    Label("Hazır: \(Fmt.home(res.path))", systemImage: "checkmark.circle.fill")
                        .foregroundStyle(.green)
                        .lineLimit(1)
                        .truncationMode(.middle)
                    Button("Finder'da göster") {
                        actions.reveal(res.path)
                    }
                    .buttonStyle(SmallButtonStyle())
                }
                Text(madeText(res))
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

    private func runningText(_ part: String?, _ done: Int64, _ total: Int64) -> String {
        switch part {
        case "db": "Veritabanı dökülüyor (mongodump) · \(Fmt.bytes(done))"
        case "files": "Dosyalar paketleniyor · \(Fmt.bytes(done))" + (total > 0 ? " / \(Fmt.bytes(total))" : "")
        case "check": "Üretilen arşiv denetleniyor · \(Fmt.bytes(done))" + (total > 0 ? " / \(Fmt.bytes(total))" : "")
        default: "Hazırlanıyor…"
        }
    }

    private func madeText(_ res: CreateResult) -> String {
        var parts: [String] = []
        if let a = res.archive {
            parts.append(Fmt.bytes(a.size))
            if let d = a.database {
                parts.append("veritabanı \(d.name ?? "") — \(d.collections.filter { !$0.view }.count) koleksiyon, \(d.documents) belge")
            }
            if let f = a.files {
                parts.append("\(f.files) dosya, \(Fmt.bytes(f.bytes))")
            }
        }
        if res.skipped > 0 {
            parts.append("\(res.skipped) öğe alınmadı (gizli dosya, bağlantı)")
        }
        parts.append(Fmt.duration(ms: res.tookMs))
        if let sha = res.archive?.sha256, !sha.isEmpty {
            parts.append("sha256 \(sha.prefix(12))…")
        }
        return parts.joined(separator: " · ")
    }
}

// MARK: - shared

/// The panel's import page for the archive: a server, one of its sites, opened in the
/// browser; the archive is then dragged onto the page from the Finder.
private struct PanelImport: View {
    @Bindable var model: ArchivesModel
    let actions: ArchivesActions
    let path: String

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("PANELDE İÇE AKTAR")
                .font(.caption.weight(.semibold))
                .foregroundStyle(.secondary)
            if model.targets.isEmpty {
                Text("Ayarlar'da panel adresi olan sunucu yok (ya da sunucuların durumu henüz okunmadı): FootOps › Ayarlar… › panel adresi.")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            } else {
                HStack(spacing: 4) {
                    ForEach(model.targets) { t in
                        Button {
                            model.select(server: t.code)
                        } label: {
                            Text(t.code.uppercased())
                                .font(.headline)
                                .padding(.horizontal, 12)
                                .padding(.vertical, 3)
                                .background(Capsule().fill(model.panelServer == t.code ? Color.accentColor.opacity(0.22) : Color.primary.opacity(0.06)))
                                .contentShape(Capsule())
                        }
                        .buttonStyle(.plain)
                        .help(t.panel)
                    }
                    Text("hangi siteye?")
                        .foregroundStyle(.secondary)
                        .padding(.leading, 6)
                }
                if let t = model.target {
                    LazyVGrid(columns: [GridItem(.adaptive(minimum: 150), spacing: 4, alignment: .leading)], alignment: .leading, spacing: 4) {
                        ForEach(t.sites, id: \.self) { s in
                            Button {
                                model.panelSite = s
                            } label: {
                                Text(s)
                                    .lineLimit(1)
                            }
                            .buttonStyle(SelectableRowStyle(selected: model.panelSite == s))
                        }
                    }
                    if t.sites.isEmpty {
                        Text("Bu sunucunun durum dosyasında site yok.")
                            .font(.callout)
                            .foregroundStyle(.secondary)
                    }
                }
                HStack(spacing: 10) {
                    Button("Panelde İçe aktar'ı aç") {
                        if let s = model.panelSite, let url = model.target?.importURL(site: s) {
                            actions.open(url)
                        }
                    }
                    .disabled(model.panelSite == nil)
                    Button("Arşivi Finder'da göster") {
                        actions.reveal(path)
                    }
                    Text("Sayfa açılınca arşivi Finder'dan sayfaya sürükle; panel önce denetler (deneme yüklemesi), onayını sonra ister.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 10).strokeBorder(Color.primary.opacity(0.1)))
    }
}

/// A place to drop a file or a folder on, and the button that asks for one instead.
private struct DropZone: View {
    let title: String
    let detail: String
    let button: String
    let icon: String
    let compact: Bool
    let enabled: Bool
    let drop: (URL) -> Void
    let choose: () -> Void
    @State private var targeted = false

    var body: some View {
        Group {
            if compact {
                HStack(spacing: 12) {
                    Image(systemName: icon)
                        .font(.title3)
                        .foregroundStyle(targeted ? Color.accentColor : .secondary)
                    VStack(alignment: .leading, spacing: 2) {
                        Text(title)
                        Text(detail)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    Spacer(minLength: 8)
                    Button(button) {
                        choose()
                    }
                    .disabled(!enabled)
                }
                .padding(10)
            } else {
                VStack(spacing: 10) {
                    Image(systemName: icon)
                        .font(.system(size: 40))
                        .foregroundStyle(targeted ? Color.accentColor : .secondary)
                    Text(title)
                        .font(.title3.weight(.semibold))
                    Text(detail)
                        .font(.callout)
                        .foregroundStyle(.secondary)
                        .multilineTextAlignment(.center)
                        .fixedSize(horizontal: false, vertical: true)
                    Button(button) {
                        choose()
                    }
                    .disabled(!enabled)
                }
                .padding(24)
                .frame(minHeight: 220)
            }
        }
        .frame(maxWidth: .infinity)
        .background(
            RoundedRectangle(cornerRadius: 10)
                .strokeBorder(style: StrokeStyle(lineWidth: 1.5, dash: [6, 4]))
                .foregroundStyle(targeted ? Color.accentColor : Color.primary.opacity(0.25))
        )
        .background(RoundedRectangle(cornerRadius: 10).fill(targeted ? Color.accentColor.opacity(0.08) : Color.clear))
        .dropDestination(for: URL.self) { urls, _ in
            guard enabled, let url = urls.first, url.isFileURL else { return false }
            drop(url)
            return true
        } isTargeted: { targeted = $0 }
    }
}
