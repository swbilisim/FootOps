import AppKit
import SwiftUI

/// The Databases window (panel-tasarim §33.4): the Mac's databases on the left, each
/// paired once with a site of a server; the chosen one compared with the site's latest
/// backup, sent to its panel's import page, or pulled in its place; what was opened on
/// this Mac below (a pull's copy among it). Plain views and AppKit's cheapest controls,
/// as the other windows (no popover, no switch: §29.13).
public struct DatabasesWindow: View {
    @Bindable var model: DatabasesModel
    let actions: DatabasesActions
    /// The clock for "3 sa önce" (fixed in the image tests).
    let fixedNow: Date?

    public static let size = NSSize(width: 1000, height: 780)

    public init(model: DatabasesModel, actions: DatabasesActions, now: Date? = nil) {
        self.model = model
        self.actions = actions
        fixedNow = now
    }

    public var body: some View {
        let now = fixedNow ?? Date()
        VStack(spacing: 0) {
            HStack(alignment: .top, spacing: 0) {
                DatabaseColumn(model: model)
                    .frame(width: 290)
                Divider()
                ScrollView {
                    DatabaseDetail(model: model, actions: actions, now: now)
                        .padding(16)
                        .frame(maxWidth: .infinity, alignment: .topLeading)
                }
            }
            Divider()
            OpenedSection(list: model.opened, reveal: actions.reveal, now: now)
        }
        .background(Color(nsColor: .windowBackgroundColor))
    }
}

/// What the window's buttons do outside it; the app gives them (the image tests none).
public struct DatabasesActions {
    /// Shows a file in the Finder.
    public var reveal: (String) -> Void
    /// Opens a page in the browser (a panel's).
    public var open: (URL) -> Void

    public init(reveal: @escaping (String) -> Void, open: @escaping (URL) -> Void) {
        self.reveal = reveal
        self.open = open
    }

    public static var none: DatabasesActions { DatabasesActions(reveal: { _ in }, open: { _ in }) }
}

// MARK: - the list

private struct DatabaseColumn: View {
    @Bindable var model: DatabasesModel

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text("BU MAC'TEKİ VERİTABANLARI")
                    .font(.caption.weight(.semibold))
                    .foregroundStyle(.secondary)
                Spacer()
                Button {
                    model.refresh()
                } label: {
                    if model.reading {
                        ProgressView().controlSize(.small)
                            .frame(width: 16, height: 16)
                    } else {
                        Image(systemName: "arrow.clockwise")
                            .frame(width: 16, height: 16)
                    }
                }
                .buttonStyle(IconButtonStyle())
                .help("Yeniden oku")
                .disabled(model.reading)
            }
            if let e = model.listError {
                Notice(light: .gray, text: e, font: .caption)
            }
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 1) {
                    ForEach(model.list?.own ?? []) { d in
                        Button {
                            model.select(d.name)
                        } label: {
                            VStack(alignment: .leading, spacing: 1) {
                                HStack {
                                    Text(d.name)
                                        .lineLimit(1)
                                        .truncationMode(.middle)
                                    Spacer(minLength: 4)
                                    Text(d.missing ? "yok" : Fmt.bytes(d.bytes))
                                        .font(.caption.monospacedDigit())
                                        .foregroundStyle(.secondary)
                                }
                                Text(d.pair.map { "→ \($0.serverName) / \($0.site)" } ?? "eşleşmedi")
                                    .font(.caption)
                                    .foregroundStyle(d.pair == nil ? .tertiary : .secondary)
                                    .lineLimit(1)
                            }
                        }
                        .buttonStyle(SelectableRowStyle(selected: model.selected == d.name))
                        .disabled(model.isBusy && model.selected != d.name)
                    }
                }
                .padding(4)
            }
            .background(RoundedRectangle(cornerRadius: 8).fill(Color.primary.opacity(0.04)))
            if let uri = model.list?.uri, !uri.isEmpty {
                Text(uri)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .padding(12)
    }
}

// MARK: - the chosen one

private struct DatabaseDetail: View {
    @Bindable var model: DatabasesModel
    let actions: DatabasesActions
    let now: Date

    var body: some View {
        if let e = model.entry {
            VStack(alignment: .leading, spacing: 14) {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    Image(systemName: "cylinder.split.1x2")
                        .foregroundStyle(.secondary)
                    Text(e.name)
                        .font(.title3.weight(.semibold))
                        .lineLimit(1)
                        .truncationMode(.middle)
                    Text(e.missing ? "Mac'teki MongoDB'de yok (Yerele çek oluşturur)" : Fmt.bytes(e.bytes))
                        .foregroundStyle(.secondary)
                }
                PairingBox(model: model, entry: e)
                if e.pair != nil && !model.pairing {
                    ActionRow(model: model, entry: e)
                    ConfirmBox(model: model, entry: e)
                    OperationBox(model: model, entry: e, actions: actions, now: now)
                    if let res = model.compared[e.name] {
                        ComparisonView(res: res, server: model.pairedServer, actions: actions, now: now)
                    }
                }
            }
        } else {
            VStack(alignment: .leading, spacing: 8) {
                Text("Soldan bir veritabanı seç.")
                    .font(.headline)
                Text("Her veritabanı bir kez bir sunucudaki siteyle eşleştirilir. Sonra:")
                VStack(alignment: .leading, spacing: 4) {
                    Text("• Karşılaştır: sitenin son yedeğiyle, belge belge (sunucuya dokunulmaz).")
                    Text("• Sunucuya gönder: panelin İçe aktar'ı için arşiv; önizleme ve onay panelde.")
                    Text("• Yerele çek: son yedeği buraya koyar; önce buradakinin kopyası alınır.")
                }
                Text("Birleştirme yok: hangi tarafın doğru olduğuna sen karar verirsin.")
                    .foregroundStyle(.secondary)
            }
            .font(.callout)
        }
    }
}

/// The site the database is paired with, or the choice of one: a server (the menu's),
/// one of its sites.
private struct PairingBox: View {
    @Bindable var model: DatabasesModel
    let entry: DBEntry

    var body: some View {
        if let p = entry.pair, !model.pairing {
            HStack(spacing: 8) {
                Text("Site:")
                    .foregroundStyle(.secondary)
                Text(verbatim: "\(p.serverName) / \(p.site)")
                    .font(.headline)
                Button("Değiştir") {
                    model.startPairing()
                }
                .buttonStyle(SmallButtonStyle())
                .disabled(model.isBusy)
                Button("Eşleştirmeyi kaldır") {
                    model.removePairing()
                }
                .buttonStyle(SmallButtonStyle())
                .disabled(model.isBusy)
            }
        } else if !model.pairing {
            VStack(alignment: .leading, spacing: 6) {
                Text("Bu veritabanı henüz bir siteyle eşleşmedi: karşılaştırmak, göndermek ya da çekmek için önce hangi sitenin veritabanı olduğunu seç.")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                Button("Siteyle eşleştir…") {
                    model.startPairing()
                }
            }
        } else {
            VStack(alignment: .leading, spacing: 8) {
                Text("HANGİ SİTENİN VERİTABANI?")
                    .font(.caption.weight(.semibold))
                    .foregroundStyle(.secondary)
                if model.servers.isEmpty {
                    Text("Sunucuların durumu henüz okunmadı (menü okuyunca burada görünür).")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                } else {
                    HStack(spacing: 4) {
                        ForEach(model.servers) { s in
                            Button {
                                model.pick(server: s.code)
                            } label: {
                                Text(s.name)
                                    .font(.headline)
                                    .lineLimit(1)
                                    .padding(.horizontal, 12)
                                    .padding(.vertical, 3)
                                    .background(Capsule().fill(model.pairServer == s.code ? Color.accentColor.opacity(0.22) : Color.primary.opacity(0.06)))
                                    .contentShape(Capsule())
                            }
                            .buttonStyle(.plain)
                        }
                    }
                    if let t = model.pairTarget {
                        LazyVGrid(columns: [GridItem(.adaptive(minimum: 150), spacing: 4, alignment: .leading)], alignment: .leading, spacing: 4) {
                            ForEach(t.sites, id: \.self) { s in
                                Button {
                                    model.pairSite = s
                                } label: {
                                    Text(s)
                                        .lineLimit(1)
                                }
                                .buttonStyle(SelectableRowStyle(selected: model.pairSite == s))
                            }
                        }
                        if t.sites.isEmpty {
                            Text("Bu sunucunun durum dosyasında site yok.")
                                .font(.callout)
                                .foregroundStyle(.secondary)
                        }
                    }
                }
                if let e = model.pairError {
                    Label(e, systemImage: "exclamationmark.triangle.fill")
                        .foregroundStyle(.red)
                        .fixedSize(horizontal: false, vertical: true)
                }
                HStack(spacing: 8) {
                    Button("Eşleştir") {
                        model.savePairing()
                    }
                    .keyboardShortcut(.defaultAction)
                    .disabled(model.pairSite == nil)
                    Button("Vazgeç") {
                        model.pairing = false
                    }
                    Text("Bu Mac'te kalır; sunucuya bir şey gitmez.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
            .padding(12)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(RoundedRectangle(cornerRadius: 10).strokeBorder(Color.primary.opacity(0.1)))
        }
    }
}

private struct ActionRow: View {
    @Bindable var model: DatabasesModel
    let entry: DBEntry

    var body: some View {
        HStack(spacing: 8) {
            Button {
                model.compare()
            } label: {
                Label("Karşılaştır", systemImage: "arrow.left.arrow.right")
            }
            .disabled(model.isBusy || entry.missing)
            .help("Sitenin son yedeğiyle belge belge (sunucuya dokunulmaz)")
            Button {
                model.ask(.send(entry.name))
            } label: {
                Label("Sunucuya gönder…", systemImage: "arrow.up.circle")
            }
            .disabled(model.isBusy || entry.missing)
            .help("Panelin İçe aktar'ı için arşiv; önizleme ve onay panelde")
            Button {
                model.ask(.pull(entry.name))
            } label: {
                Label("Yerele çek…", systemImage: "arrow.down.circle")
            }
            .disabled(model.isBusy)
            .help("Sitenin son yedeği bu veritabanının yerine (önce buradakinin kopyası)")
        }
    }
}

/// The yes asked before a pull or a send, with what the latest comparison says it does.
private struct ConfirmBox: View {
    @Bindable var model: DatabasesModel
    let entry: DBEntry

    var body: some View {
        if let c = model.confirm, let p = entry.pair {
            let diff = model.compared[entry.name]?.diff
            VStack(alignment: .leading, spacing: 8) {
                switch c {
                case .pull:
                    Text(verbatim: "Yerele çek: \(entry.name), \(p.serverName) / \(p.site) sitesinin son yedeğiyle değiştirilecek.")
                        .font(.headline)
                    Text(verbatim: entry.missing ? "Bu Mac'te öyle bir veritabanı yok: yedekten oluşturulur."
                         : "Önce buradakinin kopyası alınır (\(entry.name)_yedek_<tarih>_<saat>; Bu Mac'te açılanlar'da durur). Yükleme başarısız olursa buradaki eski hâline döner.")
                    Text(verbatim: "Bu veritabanını kullanan bir uygulama (geliştirme sunucusu) açıksa önce onu durdur: çekme sırasında yazdıkları kaybolur.")
                        .foregroundStyle(.secondary)
                    if let d = diff {
                        Text(pullEffect(d))
                            .foregroundStyle(d.different + d.onlyLocal > 0 ? .orange : .secondary)
                    } else if !entry.missing {
                        Text("Önce karşılaştırırsan neyin değişeceğini görürsün.")
                            .foregroundStyle(.secondary)
                    }
                    buttons("Evet, yerele çek")
                case .send:
                    Text(verbatim: "Sunucuya gönder: \(entry.name) → \(p.serverName) / \(p.site)")
                        .font(.headline)
                    Text("Arşiv üretilir (İndirilenler), sonra panelde sitenin İçe aktar sayfası açılır: arşivi sayfaya sürükle. Panel önce önizleme gösterir ve onayını ister; uygularken sunucudakinin güvenlik kopyasını alır (geri alınabilir).")
                    if let d = diff {
                        Text(sendEffect(d))
                            .foregroundStyle(d.different + d.onlyServer > 0 ? .orange : .secondary)
                    } else {
                        Text("Önce karşılaştırırsan sunucuda neyin değişeceğini görürsün.")
                            .foregroundStyle(.secondary)
                    }
                    if model.pairedServer?.panel == nil {
                        Notice(light: .yellow, text: "Ayarlar'da bu sunucunun panel adresi yok: arşiv üretilir, sayfayı panelde kendin açarsın (site › İçe aktar).", font: .caption)
                    }
                    buttons("Arşivi üret ve paneli aç")
                }
            }
            .font(.callout)
            .fixedSize(horizontal: false, vertical: true)
            .padding(12)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(RoundedRectangle(cornerRadius: 10).fill(Color.accentColor.opacity(0.07)))
        }
    }

    private func buttons(_ yes: String) -> some View {
        HStack(spacing: 8) {
            Button(yes) {
                model.confirmed()
            }
            .keyboardShortcut(.defaultAction)
            Button("Vazgeç") {
                model.confirm = nil
            }
        }
    }

    private func pullEffect(_ d: DBDiff) -> String {
        if d.identical {
            return "Karşılaştırmaya göre iki taraf aynı: çekmek bir şey değiştirmez."
        }
        var t = "Karşılaştırmaya göre burada: \(Fmt.count(d.different)) belge sunucudakiyle değişir, \(Fmt.count(d.onlyLocal)) belge kaybolur (yalnız burada), \(Fmt.count(d.onlyServer)) belge gelir."
        if !d.onlyLocalCollections.isEmpty {
            t += " Yalnız burada olan koleksiyonlar da gider: " + d.onlyLocalCollections.joined(separator: ", ") + "."
        }
        return t + " Kaybolanlar kopyada kalır."
    }

    private func sendEffect(_ d: DBDiff) -> String {
        if d.identical {
            return "Karşılaştırmaya göre iki taraf aynı: göndermek bir şey değiştirmez."
        }
        var t = "Karşılaştırmaya göre, panelde Tamamen değiştir seçilirse sunucuda: \(Fmt.count(d.onlyServer)) belge silinir (yalnız orada), \(Fmt.count(d.different)) belge buradakiyle değişir, \(Fmt.count(d.onlyLocal)) belge eklenir."
        if !d.onlyServerCollections.isEmpty {
            t += " Yalnız orada olan koleksiyonlar da gider: " + d.onlyServerCollections.joined(separator: ", ") + "."
        }
        return t + " Yalnız olmayanları ekle seçilirse yalnız \(Fmt.count(d.onlyLocal)) belge eklenir, gerisi olduğu gibi kalır."
    }
}

/// What runs, or how the last pull, send or failure ended.
private struct OperationBox: View {
    @Bindable var model: DatabasesModel
    let entry: DBEntry
    let actions: DatabasesActions
    let now: Date

    var body: some View {
        switch model.operation {
        case .running(let job, let db, let part, let done, let total) where db == entry.name:
            VStack(alignment: .leading, spacing: 6) {
                HStack {
                    Text(verbatim: runningText(job, part, done, total))
                        .lineLimit(1)
                    Spacer()
                    if model.canStop {
                        Button("Durdur") {
                            model.stop()
                        }
                        .buttonStyle(SmallButtonStyle())
                    }
                }
                Bar(fraction: total > 0 ? Double(done) / Double(total) : 0)
                Text(verbatim: footnote(job, part))
                    .font(.caption)
                    .foregroundStyle(model.canStop ? Color.secondary : Color.orange)
                    .fixedSize(horizontal: false, vertical: true)
            }
        case .pulled(let res) where res.db == entry.name:
            VStack(alignment: .leading, spacing: 6) {
                Label(String("\(res.db) artık \(res.serverName) / \(res.site) sitesinin \(Fmt.dateTime(res.backup.time)) yedeği (\(Fmt.count(res.loaded?.documents ?? 0)) belge)."),
                      systemImage: "checkmark.circle.fill")
                    .foregroundStyle(.green)
                    .fixedSize(horizontal: false, vertical: true)
                if let copy = res.copy {
                    Text(verbatim: "Önceki hâli: \(copy) (aşağıda, Bu Mac'te açılanlar'da; gerekmeyince sil).")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                ForEach(res.warnings, id: \.self) { w in
                    Notice(light: .yellow, text: w, font: .caption)
                }
            }
        case .sent(let db, let res) where db == entry.name:
            VStack(alignment: .leading, spacing: 6) {
                Label(String("Arşiv hazır: \(Fmt.home(res.path))"), systemImage: "checkmark.circle.fill")
                    .foregroundStyle(.green)
                    .lineLimit(1)
                    .truncationMode(.middle)
                Text(res.importURL == nil ? "Panelde sitenin İçe aktar sayfasını aç ve arşivi sayfaya sürükle." :
                     "Panelde İçe aktar sayfası açıldı: arşivi Finder'dan sayfaya sürükle; önizlemeye bakıp onayla. İşin bitince arşivi sil (veritabanı içinde).")
                    .font(.callout)
                    .fixedSize(horizontal: false, vertical: true)
                HStack(spacing: 8) {
                    Button("Finder'da göster") {
                        actions.reveal(res.path)
                    }
                    .buttonStyle(SmallButtonStyle())
                    if let u = res.importURL.flatMap(URL.init(string:)) {
                        Button("İçe aktar sayfasını yeniden aç") {
                            actions.open(u)
                        }
                        .buttonStyle(SmallButtonStyle())
                    }
                }
                ForEach(res.warnings, id: \.self) { w in
                    Notice(light: .yellow, text: w, font: .caption)
                }
            }
        case .failed(let db, let job, let message) where db == entry.name:
            VStack(alignment: .leading, spacing: 4) {
                Label(message, systemImage: "exclamationmark.triangle.fill")
                    .foregroundStyle(.red)
                    .fixedSize(horizontal: false, vertical: true)
                Text(job == .pull ? "Yerele çek tamamlanmadı." : job == .send ? "Arşiv üretilemedi; sunucuya bir şey gitmedi." : "Karşılaştırılamadı.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        default:
            EmptyView()
        }
    }

    private func footnote(_ job: DatabasesModel.Job, _ part: String?) -> String {
        switch (job, part) {
        case (.pull, "load"), (.pull, "restore"):
            "Bu adım durdurulamaz: yarıda kalsa veritabanı yarım kalırdı. Pencereyi kapatsan ya da uygulamadan çıksan da biter."
        case (.pull, _):
            "Durdurulursa ya da pencere kapanırsa hiçbir şey değişmez (alınan kopya silinir)."
        default:
            "Pencere kapanırsa durdurulur; yarım kalan silinir."
        }
    }

    private func runningText(_ job: DatabasesModel.Job, _ part: String?, _ done: Int64, _ total: Int64) -> String {
        let of = total > 0 ? " · \(Fmt.bytes(done)) / \(Fmt.bytes(total))" : ""
        switch part {
        case "download": return "Sitenin yedeği indiriliyor (R2, salt okunur)" + of
        case "restore": return "Yüklenemedi; buradaki eski hâline getiriliyor" + of
        case "load": return (job == .pull ? "Yedek buradakinin yerine yükleniyor" : "Yedek geçici olarak Mac'teki MongoDB'ye yükleniyor") + of
        case "compare": return "Karşılaştırılıyor · \(Fmt.count(done)) / \(Fmt.count(total)) belge"
        case "copy": return "Buradakinin kopyası alınıyor" + of
        case "db": return "Veritabanı dökülüyor (mongodump) · \(Fmt.bytes(done))"
        case "check": return "Üretilen arşiv denetleniyor" + of
        default:
            switch job {
            case .compare: return "Sitenin yedekleri okunuyor…"
            case .pull: return "Sitenin yedekleri okunuyor…"
            case .send: return "Arşiv hazırlanıyor…"
            }
        }
    }
}

// MARK: - a comparison

private struct ComparisonView: View {
    let res: DBSyncResult
    let server: ServerSites?
    let actions: DatabasesActions
    let now: Date

    var body: some View {
        if let d = res.diff {
            VStack(alignment: .leading, spacing: 10) {
                Divider()
                HStack(alignment: .firstTextBaseline) {
                    Text("KARŞILAŞTIRMA")
                        .font(.caption.weight(.semibold))
                        .foregroundStyle(.secondary)
                    Text(verbatim: "\(Fmt.duration(ms: res.tookMs))")
                        .font(.caption)
                        .foregroundStyle(.tertiary)
                }
                summary(d)
                Text(verbatim: "Sunucu tarafı: \(res.serverName) / \(res.site), \(Fmt.dateTime(res.backup.time)) yedeği (\(res.backup.kindText), \(Fmt.ago(res.backup.time, now: now))).")
                    .font(.callout)
                HStack(spacing: 8) {
                    Text("Daha tazesi için panelde elle yedek al, sonra yeniden karşılaştır.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    if let u = server?.page(site: res.site, "backup") {
                        Button("Panelde yedekle…") {
                            actions.open(u)
                        }
                        .buttonStyle(SmallButtonStyle())
                    }
                }
                newest(d)
                CollectionTable(diff: d)
                if d.more > 0 {
                    Notice(light: .yellow, text: "\(d.more) koleksiyon daha var: karşılaştırılmadı.", font: .caption)
                }
                ForEach(d.skipped, id: \.self) { s in
                    Notice(light: .gray, text: s, font: .caption)
                }
                ForEach(d.collections.filter { $0.error != nil }) { c in
                    Notice(light: .yellow, text: "\(c.name): \(c.error ?? "")", font: .caption)
                }
                ForEach(res.warnings, id: \.self) { w in
                    Notice(light: .yellow, text: w, font: .caption)
                }
            }
        }
    }

    @ViewBuilder
    private func summary(_ d: DBDiff) -> some View {
        if d.identical {
            Label("Belgeler birebir aynı (\(Fmt.count(d.same)) belge, \(d.local.collections) koleksiyon).", systemImage: "checkmark.circle.fill")
                .font(.headline)
                .foregroundStyle(.green)
        } else {
            Label("Aynı değil: \(Fmt.count(d.different)) farklı · \(Fmt.count(d.onlyLocal)) yalnız bu Mac'te · \(Fmt.count(d.onlyServer)) yalnız sunucuda" +
                  (d.errors > 0 ? " · \(d.errors) koleksiyon yalnız sayıldı" : ""), systemImage: "arrow.left.arrow.right.circle.fill")
                .font(.headline)
                .foregroundStyle(.orange)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    @ViewBuilder
    private func newest(_ d: DBDiff) -> some View {
        let l = d.local.newest, s = d.serverSide.newest
        VStack(alignment: .leading, spacing: 2) {
            Text(verbatim: "En yeni kayıt — bu Mac: \(newestText(l)) · sunucu: \(newestText(s))")
            if let l, let s, !d.identical {
                Text(l.time > s.time ? "Bu Mac'teki daha yeni görünüyor." : l.time < s.time ? "Sunucudaki daha yeni görünüyor." : "İkisinin en yeni kaydı aynı anda.")
                    .foregroundStyle(.secondary)
            }
        }
        .font(.callout)
    }

    private func newestText(_ n: Newest?) -> String {
        guard let n else { return "—" }
        return "\(Fmt.dateTime(n.time)) (\(n.place))"
    }
}

/// The collections, a row each: the counts on both sides and what differs, the rows that
/// differ marked; the _ids of a few differing documents in the row's help.
private struct CollectionTable: View {
    let diff: DBDiff

    var body: some View {
        Grid(alignment: .trailing, horizontalSpacing: 14, verticalSpacing: 3) {
            GridRow {
                head("Koleksiyon").gridColumnAlignment(.leading)
                head("Bu Mac")
                head("Sunucu")
                head("Aynı")
                head("Farklı")
                head("Yalnız bu Mac'te")
                head("Yalnız sunucuda")
            }
            Divider()
            ForEach(diff.collections) { c in
                GridRow {
                    HStack(spacing: 6) {
                        Circle()
                            .fill(c.error != nil ? Color.yellow : c.differs ? Color.orange : Color.clear)
                            .frame(width: 6, height: 6)
                        Text(verbatim: c.name)
                            .lineLimit(1)
                            .truncationMode(.middle)
                    }
                    .frame(maxWidth: 240, alignment: .leading)
                    .help(examples(c))
                    Text(verbatim: count(c.local))
                    Text(verbatim: count(c.server))
                    if c.error != nil {
                        Text(verbatim: "?")
                        Text(verbatim: "?")
                        Text(verbatim: "?")
                        Text(verbatim: "?")
                    } else {
                        Text(verbatim: Fmt.count(c.same))
                            .foregroundStyle(.secondary)
                        marked(c.different)
                        marked(c.onlyLocal)
                        marked(c.onlyServer)
                    }
                }
                .font(.callout.monospacedDigit())
            }
            Divider()
            GridRow {
                Text("Toplam").fontWeight(.semibold)
                    .padding(.leading, 12)
                Text(verbatim: Fmt.count(diff.local.documents))
                Text(verbatim: Fmt.count(diff.serverSide.documents))
                Text(verbatim: Fmt.count(diff.same))
                marked(diff.different)
                marked(diff.onlyLocal)
                marked(diff.onlyServer)
            }
            .font(.callout.monospacedDigit())
        }
        .padding(10)
        .background(RoundedRectangle(cornerRadius: 8).fill(Color.primary.opacity(0.04)))
    }

    private func head(_ s: String) -> some View {
        Text(s)
            .font(.caption.weight(.semibold))
            .foregroundStyle(.secondary)
    }

    private func count(_ n: Int64) -> String { n < 0 ? "—" : Fmt.count(n) }

    private func marked(_ n: Int64) -> some View {
        Text(verbatim: Fmt.count(n))
            .foregroundStyle(n > 0 ? Color.orange : Color.secondary)
            .fontWeight(n > 0 ? .semibold : .regular)
    }

    private func examples(_ c: CollectionDiff) -> String {
        guard let x = c.examples else { return c.name }
        var lines = [c.name]
        for (what, ids) in [("farklı", x.different), ("yalnız bu Mac'te", x.onlyLocal), ("yalnız sunucuda", x.onlyServer)] where !ids.isEmpty {
            lines.append("\(what): " + ids.joined(separator: ", "))
        }
        return lines.joined(separator: "\n")
    }
}
