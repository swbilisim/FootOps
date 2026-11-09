import Foundation
import Observation
import SwiftUI

/// What was opened on this Mac (panel-tasarim §29.7): the folders backups and archives were
/// opened into, the databases they were loaded into (yedek acilanlar), deleted one by one
/// (yedek sil; the core deletes only what footops-mac opened). The Backups and the Archives
/// windows show one list: what either opens or deletes, both see.
@MainActor @Observable
public final class OpenedList {
    public private(set) var view: OpenedView?
    public private(set) var error: String?
    public private(set) var reading = false
    /// The item a delete is being asked for (a folder's path, a database's name).
    public var confirmDelete: String?
    public private(set) var deleting: String?

    @ObservationIgnored private let core: Core?
    /// A read asked for while one was under way: once it ends.
    @ObservationIgnored private var again = false

    public init(core: Core?) {
        self.core = core
    }

    /// A list holding what it is given (the image tests').
    public init(view: OpenedView?) {
        core = nil
        self.view = view
    }

    public func refresh() {
        Task { await read() }
    }

    func read() async {
        guard let core else { return }
        guard !reading else {
            again = true
            return
        }
        reading = true
        defer {
            reading = false
            if again {
                again = false
                Task { await read() }
            }
        }
        do {
            let out = try await core.run(["yedek", "acilanlar", "--json"], timeout: .seconds(60))
            switch Core.decode(OpenedView.self, from: out) {
            case .success(let v):
                view = v
                error = nil
            case .failure(let m):
                error = m.text
            }
        } catch Core.Failure.stopped {
            return
        } catch {
            self.error = error.localizedDescription
        }
    }

    /// Deletes a folder or a database on the list (yedek sil; the core checks it is one
    /// footops-mac opened).
    public func delete(_ item: String) {
        guard let core, deleting == nil else { return }
        confirmDelete = nil
        deleting = item
        Task {
            defer { deleting = nil }
            do {
                let out = try await core.run(["yedek", "sil", item, "--json"], timeout: .seconds(120))
                if case .failure(let m) = Core.decode(DeleteAnswer.self, from: out) {
                    error = m.text
                } else {
                    error = nil
                }
            } catch {
                self.error = error.localizedDescription
            }
            await read()
        }
    }
}

/// The list at a window's foot.
struct OpenedSection: View {
    @Bindable var list: OpenedList
    let reveal: (String) -> Void
    let now: Date

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text("BU MAC'TE AÇILANLAR")
                    .font(.caption.weight(.semibold))
                    .foregroundStyle(.secondary)
                if let v = list.view, v.folders.contains(where: \.old) || v.databases.contains(where: \.old) {
                    Text("bir haftadan eskiler var: gerekmiyorsa sil")
                        .font(.caption)
                        .foregroundStyle(.orange)
                }
                Spacer()
                Button {
                    list.refresh()
                } label: {
                    if list.reading {
                        ProgressView().controlSize(.small)
                            .frame(width: 16, height: 16)
                    } else {
                        Image(systemName: "arrow.clockwise")
                            .frame(width: 16, height: 16)
                    }
                }
                .buttonStyle(IconButtonStyle())
                .help("Yeniden oku")
                .disabled(list.reading)
            }
            if let e = list.error {
                Notice(light: .red, text: e, font: .caption)
            }
            if let v = list.view {
                if v.isEmpty {
                    Text("Açılmış yedek ya da arşiv yok. Açılanlar burada listelenir; işin bitince sil (sırlar içerebilir).")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                }
                ScrollView {
                    VStack(alignment: .leading, spacing: 1) {
                        ForEach(v.folders) { f in
                            OpenedRow(icon: "folder", title: (f.path as NSString).lastPathComponent, detail: folderDetail(f),
                                      help: f.path, old: f.old, note: folderNote(f), item: f.path, busy: f.busy, list: list) {
                                Button("Göster") {
                                    reveal(f.path)
                                }
                                .buttonStyle(SmallButtonStyle())
                            }
                        }
                        ForEach(v.databases) { d in
                            OpenedRow(icon: "cylinder.split.1x2", title: d.name, detail: dbDetail(d), help: "Mac'teki MongoDB",
                                      old: d.old, note: d.old ? "bir haftadan eski" : nil, item: d.name, busy: false, list: list) {
                                EmptyView()
                            }
                        }
                        if let e = v.mongoError {
                            Notice(light: .gray, text: "MongoDB: \(e)", font: .caption)
                                .padding(.horizontal, 8)
                        }
                    }
                }
                .frame(maxHeight: 150)
                .fixedSize(horizontal: false, vertical: true)
            } else if list.reading {
                Text("Okunuyor…")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 10)
    }

    /// Where it came from: a server's backup ("H1 / magaza"), or an archive.
    private func origin(server: String?, site: String?, archive: String?) -> String {
        if let archive {
            return "arşiv \(archive)" + (site.map { $0.isEmpty ? "" : " (\($0))" } ?? "")
        }
        return "\((server ?? "").uppercased()) / \(site ?? "")"
    }

    private func taken(_ time: Date, archive: String?) -> String {
        archive == nil ? "yedek \(Fmt.dateTime(time))" : Fmt.dateTime(time)
    }

    private func folderDetail(_ f: OpenedFolder) -> String {
        let from = origin(server: f.server, site: f.site, archive: f.archive)
        if let e = f.error {
            return "\(from) · \(e)"
        }
        return "\(from) · \(taken(f.time, archive: f.archive)) · \(f.files) dosya, \(Fmt.bytes(f.bytes)) · açıldı \(Fmt.ago(f.at, now: now))"
    }

    private func folderNote(_ f: OpenedFolder) -> String? {
        if f.busy {
            return "açılıyor…"
        }
        if f.partial {
            return "yarım kaldı (açma kesildi)"
        }
        return f.old ? "bir haftadan eski" : nil
    }

    private func dbDetail(_ d: OpenedDatabase) -> String {
        var t = "MongoDB · \(Fmt.bytes(d.bytes))"
        if d.registered, d.server != nil || d.archive != nil {
            t += " · " + origin(server: d.server, site: d.site, archive: d.archive)
            if let time = d.time {
                t += ", " + taken(time, archive: d.archive)
            }
            if let at = d.at {
                t += " · yüklendi \(Fmt.ago(at, now: now))"
            }
        } else {
            t += " · footops-mac'in listesinde değil"
        }
        return t
    }
}

private struct OpenedRow<Extra: View>: View {
    let icon: String
    let title: String
    let detail: String
    let help: String
    let old: Bool
    /// A word beside the title: a week old, cut short, under way.
    let note: String?
    let item: String
    /// Under way elsewhere (the Terminal): not deleted.
    let busy: Bool
    let list: OpenedList
    @ViewBuilder let extra: () -> Extra

    var body: some View {
        HStack(alignment: .center, spacing: 8) {
            Image(systemName: icon)
                .foregroundStyle(.secondary)
                .frame(width: 18)
            VStack(alignment: .leading, spacing: 1) {
                HStack(spacing: 6) {
                    Text(title)
                        .lineLimit(1)
                        .truncationMode(.middle)
                    if let note {
                        Text(note)
                            .font(.caption)
                            .foregroundStyle(.orange)
                    }
                }
                Text(detail)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                    .truncationMode(.tail)
            }
            .help(help)
            Spacer(minLength: 8)
            if list.deleting == item {
                ProgressView().controlSize(.small)
            } else if list.confirmDelete == item {
                Text("Silinsin mi?")
                    .font(.callout)
                Button("Evet, sil") {
                    list.delete(item)
                }
                .buttonStyle(SmallButtonStyle())
                .foregroundStyle(.red)
                Button("Vazgeç") {
                    list.confirmDelete = nil
                }
                .buttonStyle(SmallButtonStyle())
            } else {
                extra()
                Button("Sil") {
                    list.confirmDelete = item
                }
                .buttonStyle(SmallButtonStyle())
                .disabled(list.deleting != nil || busy)
            }
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 4)
        .background(RoundedRectangle(cornerRadius: 6).fill(old ? Color.orange.opacity(0.08) : Color.clear))
    }
}
