import AppKit
import Observation
import ServiceManagement
import SwiftUI

/// Whether the app starts with the Mac (SMAppService keeps it; no file of ours).
public enum LoginState: Equatable, Sendable {
    case enabled
    case disabled
    /// Turned on, waiting for the user's yes in System Settings › Login Items.
    case requiresApproval
    case unavailable(String)
}

public enum LoginItem {
    public static var state: LoginState {
        switch SMAppService.mainApp.status {
        case .enabled: .enabled
        case .requiresApproval: .requiresApproval
        case .notRegistered: .disabled
        case .notFound: .unavailable("Uygulama sistemde kayıtlı değil (Uygulamalar klasöründen açın)")
        @unknown default: .disabled
        }
    }

    public static func set(_ on: Bool) throws {
        if on {
            try SMAppService.mainApp.register()
        } else {
            try SMAppService.mainApp.unregister()
        }
    }
}

/// The Settings window's state: each server's name, panel, SSH target and bucket, read
/// and written through footops-mac ayar (the rules are the core's); servers added, tried,
/// given their password and taken off this Mac through footops-mac sunucu; the Mac's R2
/// access; the login item.
@MainActor @Observable
public final class SettingsModel {
    public var servers: [ServerSetting] = []
    public private(set) var path = ""
    /// The file the list was read from: saving writes over that file only.
    public private(set) var revision = ""
    /// The file or the Keychain could not be read (what could be is listed).
    public private(set) var loadError: String?
    public private(set) var loaded: [ServerSetting] = []
    public private(set) var busy = false
    public var message: Message?
    public private(set) var login: LoginState
    public let version: String

    /// The Mac's R2 access as the Keychain has it; nil: not read.
    public internal(set) var r2: R2Access?
    /// What sunucu dene said of a server, by its code.
    public internal(set) var tries: [String: TryState] = [:]
    /// The server being added, as typed: its form is open.
    var adding: NewServer?
    /// The server whose password is being typed, and what is typed.
    var passwordFor: String?
    var password = ""
    /// The server asked about before it is taken off this Mac.
    var removing: String?
    /// The R2 access being typed: its form is open.
    var r2Form: NewR2?

    public enum Message: Equatable, Sendable {
        case success(String)
        case failure(String)
    }

    /// Where a server's try is: under way, its answer, or why there is none.
    public enum TryState: Equatable, Sendable {
        case running
        case done(ServerTry)
        case failed(String)
    }

    @ObservationIgnored private let core: Core?
    /// Each try's own token: an answer comes back to its try only (not to a server taken
    /// off and added again meanwhile).
    @ObservationIgnored private var tryTokens: [String: UUID] = [:]
    /// Called once new settings are written (the menu reads the status again).
    @ObservationIgnored public var onSaved: () -> Void = {}

    public init(core: Core?, version: String) {
        self.core = core
        self.version = version
        login = LoginItem.state
    }

    /// A model holding what it is given (the image tests').
    public init(view: SettingsView, login: LoginState, version: String, r2: R2Access? = nil) {
        core = nil
        self.version = version
        self.login = login
        self.r2 = r2
        apply(view)
    }

    /// Edited since read.
    public var dirty: Bool { servers != loaded }

    /// A form is open (one at a time): Return is its, not Kaydet's.
    var formOpen: Bool { adding != nil || passwordFor != nil || r2Form != nil }

    /// A server's name as the window shows it.
    func shownName(_ code: String) -> String {
        servers.first { $0.code == code }?.shownName ?? code.uppercased()
    }

    /// Reads the settings and the R2 access anew; not while a read or a save is under way
    /// (one at a time: a read begun before a save's end would show what was there before
    /// it).
    public func load() async {
        guard let core, !busy else { return }
        busy = true
        defer { busy = false }
        login = LoginItem.state
        do {
            let out = try await core.run(["ayar", "--json"], timeout: .seconds(30))
            switch Core.decode(SettingsView.self, from: out) {
            case .success(let v):
                apply(v)
            case .failure(let m):
                loadError = m.text
            }
        } catch {
            loadError = error.localizedDescription
        }
        if let out = try? await core.run(["sunucu", "r2", "--json"], timeout: .seconds(30)),
           case .success(let v) = Core.decode(R2Access.self, from: out) {
            r2 = v
        }
    }

    public func save() async {
        guard let core, !busy else { return }
        busy = true
        defer { busy = false }
        message = nil
        do {
            let input = try JSONEncoder().encode(SettingsFile(servers, revision: revision.isEmpty ? nil : revision))
            let out = try await core.run(["ayar", "yaz", "--json"], input: input, timeout: .seconds(30))
            switch Core.decode(SettingsView.self, from: out) {
            case .success(let v):
                apply(v)
                message = .success("Kaydedildi")
                onSaved()
            case .failure(let m) where m.code == "settings_changed":
                // Written elsewhere since it was read (the Terminal): read anew, what is
                // being typed kept, for another look before it is saved over.
                await reload(core)
                message = .failure("Ayarlar bu arada başka bir yerden değişti: yeniden okundu, yazdıkların duruyor — bakıp yeniden Kaydet")
            case .failure(let m):
                message = .failure(m.text)
            }
        } catch {
            message = .failure(error.localizedDescription)
        }
    }

    /// Back to what was read.
    public func revert() {
        servers = loaded
        message = nil
    }

    public func setLogin(_ on: Bool) {
        do {
            try LoginItem.set(on)
            message = nil
        } catch {
            message = .failure("Açılışta başlatma \(on ? "açılamadı" : "kapatılamadı"): \(error.localizedDescription)")
        }
        login = LoginItem.state
    }

    // MARK: servers

    func startAdding() {
        closeForms()
        adding = NewServer()
        message = nil
    }

    func startPassword(_ code: String) {
        closeForms()
        passwordFor = code
        message = nil
    }

    /// One form at a time: opening one closes the others (and what was typed in them).
    private func closeForms() {
        adding = nil
        cancelPassword()
        r2Form = nil
    }

    /// Adds the server as typed — its password into the Keychain, the rest into the
    /// settings — then tries it.
    public func add() async {
        guard let core, let draft = adding, !busy else { return }
        busy = true
        message = nil
        var added: String?
        if let v = await answer(core, ["sunucu", "ekle", "--json"], input: try? JSONEncoder().encode(draft)) {
            apply(v, keepEdits: true)
            adding = nil
            added = draft.code.trimmingCharacters(in: .whitespaces).lowercased()
            message = .success("Eklendi: \(shownName(added ?? "")) — deneniyor")
            onSaved()
        } else {
            await reload(core)
        }
        busy = false
        if let added {
            await tryServer(added)
        }
    }

    /// Writes the password typed for a server into the Keychain, then tries the server.
    public func savePassword() async {
        guard let core, let code = passwordFor, !busy else { return }
        busy = true
        message = nil
        var saved = false
        // The core tries it on the repository first (restic: seconds, minutes when R2 is
        // slow).
        if let v = await answer(core, ["sunucu", "sifre", code, "--json"], input: Data(password.utf8), timeout: .seconds(240)) {
            apply(v, keepEdits: true)
            passwordFor = nil
            password = ""
            saved = true
            message = .success("\(shownName(code)) yedek şifresi Anahtar Zinciri'ne yazıldı — deneniyor")
            onSaved()
        } else {
            await reload(core)
        }
        busy = false
        if saved {
            await tryServer(code)
        }
    }

    func cancelPassword() {
        passwordFor = nil
        password = ""
    }

    /// Takes the server asked about off this Mac: its settings and Keychain items.
    public func remove() async {
        guard let core, let code = removing, !busy else { return }
        busy = true
        defer { busy = false }
        message = nil
        let shown = shownName(code)
        if let v = await answer(core, ["sunucu", "kaldir", code, "--json"], input: nil) {
            apply(v, keepEdits: true)
            removing = nil
            tries[code] = nil
            tryTokens[code] = nil
            if passwordFor == code {
                cancelPassword()
            }
            message = .success("\(shown) bu Mac'ten kaldırıldı (sunucuya dokunulmadı)")
            onSaved()
        } else {
            // Cut short (one Keychain item taken off, not the other): as it is now.
            await reload(core)
        }
    }

    /// Tries a server from this Mac (sunucu dene): its status file, its repository. Read
    /// only, so not one at a time with the rest.
    public func tryServer(_ code: String) async {
        guard let core, tries[code] != .running else { return }
        let token = UUID()
        tryTokens[code] = token
        tries[code] = .running
        let state: TryState
        do {
            let out = try await core.run(["sunucu", "dene", code, "--json"], timeout: .seconds(240))
            switch Core.decode(ServerTry.self, from: out) {
            case .success(let t):
                state = .done(t)
            case .failure(let m):
                state = .failed(m.text)
            }
        } catch {
            state = .failed(error.localizedDescription)
        }
        if tryTokens[code] == token {
            tries[code] = state
            tryTokens[code] = nil
        }
    }

    // MARK: R2

    func startR2() {
        closeForms()
        r2Form = NewR2()
        message = nil
    }

    public func saveR2() async {
        guard let core, let form = r2Form, !busy else { return }
        busy = true
        defer { busy = false }
        message = nil
        do {
            let out = try await core.run(["sunucu", "r2", "yaz", "--json"], input: try JSONEncoder().encode(form), timeout: .seconds(30))
            switch Core.decode(R2Access.self, from: out) {
            case .success(let v):
                r2 = v
                r2Form = nil
                message = .success("R2 erişimi Anahtar Zinciri'ne yazıldı")
                onSaved()
            case .failure(let m):
                message = .failure(m.text)
            }
        } catch {
            message = .failure(error.localizedDescription)
        }
    }

    /// Runs a command answered with the settings as they are now; its failure goes to the
    /// message.
    private func answer(_ core: Core, _ args: [String], input: Data?, timeout: Duration = .seconds(30)) async -> SettingsView? {
        do {
            let out = try await core.run(args, input: input, timeout: timeout)
            switch Core.decode(SettingsView.self, from: out) {
            case .success(let v):
                return v
            case .failure(let m):
                message = .failure(m.text)
            }
        } catch {
            message = .failure(error.localizedDescription)
        }
        return nil
    }

    /// The settings read anew after a command that failed (part of it may have been
    /// done), what is being typed kept.
    private func reload(_ core: Core) async {
        if let out = try? await core.run(["ayar", "--json"], timeout: .seconds(30)),
           case .success(let v) = Core.decode(SettingsView.self, from: out) {
            apply(v, keepEdits: true)
        }
    }

    /// Takes what the core says; keepEdits: fields typed and not saved stay as typed (a
    /// server added or taken off in the meantime).
    private func apply(_ v: SettingsView, keepEdits: Bool = false) {
        var edited: [String: ServerSetting] = [:]
        if keepEdits {
            for s in servers where !loaded.contains(s) {
                edited[s.code] = s
            }
        }
        path = v.path
        revision = v.revision
        loadError = v.error
        loaded = v.servers
        servers = v.servers.map { s in
            guard var mine = edited[s.code] else { return s }
            mine.keychain = s.keychain
            mine.defaultBucket = s.defaultBucket
            return mine
        }
    }
}

/// The Settings window (panel-tasarim §29.2, §29.16): per server its name, the panel
/// (opened in the browser), the SSH target (opened in the Terminal), the bucket and the
/// backups' password, tried from here, taken off this Mac; servers added; the Mac's R2
/// access; starting with the Mac.
public struct SettingsWindow: View {
    @Bindable var model: SettingsModel

    public static let width: CGFloat = 560

    public init(model: SettingsModel) {
        self.model = model
    }

    public var body: some View {
        VStack(spacing: 0) {
            Form {
                Section {
                    if model.servers.isEmpty && model.adding == nil {
                        Text(model.busy ? "Okunuyor…" : "Henüz sunucu yok: aşağıdaki Sunucu ekle ile ekleyin.")
                            .foregroundStyle(.secondary)
                    }
                    ForEach($model.servers) { $server in
                        ServerFields(server: $server, model: model)
                    }
                    if model.adding != nil {
                        NewServerFields(model: model)
                    } else {
                        HStack {
                            Button {
                                model.startAdding()
                            } label: {
                                Label("Sunucu ekle…", systemImage: "plus")
                            }
                            .disabled(model.busy)
                            Spacer()
                        }
                    }
                } header: {
                    Text("Sunucular")
                } footer: {
                    Text("Panel tarayıcıda, SSH Terminal'de açılır; kova boşsa varsayılanı okunur. Ad, panel, SSH ve kova Kaydet'le yazılır; eklemek, şifre ve kaldırmak hemen. Adresler yalnız bu Mac'te (ayar dosyası), şifreler Anahtar Zinciri'nde; kaldırmak sunucuyu yalnız bu Mac'ten siler.")
                        .foregroundStyle(.secondary)
                }
                Section {
                    R2Fields(model: model)
                } header: {
                    Text("R2 erişimi")
                } footer: {
                    Text("Sunucuların durumu ve yedekleri R2'deki kovalarından, bu Mac'in salt okunur anahtarıyla okunur.")
                        .foregroundStyle(.secondary)
                }
                Section("Genel") {
                    // A checkbox: macOS 26's switch took ~140 MB of graphics memory for a
                    // few seconds each time it was drawn (measured; a checkbox, none).
                    Toggle("Mac açılınca FootOps'u başlat", isOn: Binding(
                        get: { model.login == .enabled || model.login == .requiresApproval },
                        set: { model.setLogin($0) }
                    ))
                    .toggleStyle(.checkbox)
                    .disabled(isUnavailable)
                    if let note = loginNote {
                        Text(note)
                            .font(.callout)
                            .foregroundStyle(.secondary)
                    }
                }
                Section("Bilgi") {
                    LabeledContent("Sırlar") {
                        Text("Anahtar Zinciri, footops-yedek hizmeti")
                    }
                    LabeledContent("Ayar dosyası") {
                        Text(model.path.replacingOccurrences(of: NSHomeDirectory(), with: "~"))
                            .textSelection(.enabled)
                            .lineLimit(2)
                            .truncationMode(.middle)
                    }
                    LabeledContent("Sürüm") {
                        Text(model.version)
                            .textSelection(.enabled)
                    }
                }
            }
            .formStyle(.grouped)
            Divider()
            HStack(spacing: 10) {
                switch model.message {
                case .success(let text):
                    Label(text, systemImage: "checkmark.circle.fill")
                        .foregroundStyle(.green)
                        .fixedSize(horizontal: false, vertical: true)
                case .failure(let text):
                    Label(text, systemImage: "exclamationmark.triangle.fill")
                        .foregroundStyle(.red)
                        .fixedSize(horizontal: false, vertical: true)
                case nil:
                    if let e = model.loadError {
                        Label(e, systemImage: "exclamationmark.triangle")
                            .foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
                Spacer(minLength: 10)
                Button("Geri al") {
                    model.revert()
                }
                .disabled(!model.dirty || model.busy)
                Button("Kaydet") {
                    Task { await model.save() }
                }
                .keyboardShortcut(model.formOpen ? nil : .defaultAction)
                .disabled(!model.dirty || model.busy)
            }
            .font(.callout)
            .padding(14)
            .background(Color(nsColor: .windowBackgroundColor))
        }
    }

    private var isUnavailable: Bool {
        if case .unavailable = model.login {
            return true
        }
        return false
    }

    private var loginNote: String? {
        switch model.login {
        case .requiresApproval:
            "Sistem Ayarları › Genel › Giriş Öğeleri'nde FootOps'a izin verilmesi bekleniyor."
        case .unavailable(let why):
            why
        default:
            nil
        }
    }
}

/// A server: its name and code, what it is tried with, its fields, its password; asked
/// before it is taken off.
private struct ServerFields: View {
    @Binding var server: ServerSetting
    @Bindable var model: SettingsModel

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Text(server.shownName)
                    .font(.headline)
                    .lineLimit(1)
                Text(server.code)
                    .font(.caption.monospaced())
                    .foregroundStyle(.secondary)
                Spacer(minLength: 8)
                Button("Dene") {
                    Task { await model.tryServer(server.code) }
                }
                .disabled(model.tries[server.code] == .running)
                .help("Bu Mac'ten dener: kovadaki durum dosyası ve yedek deposu (yalnız okur)")
                Button("Kaldır…") {
                    model.removing = server.code
                }
                .disabled(model.busy)
                .help("Yalnız bu Mac'ten kaldırır: sunucuya ve yedeklerine bir şey olmaz")
            }
            .controlSize(.small)
            if model.removing == server.code {
                RemoveConfirm(name: server.shownName, model: model)
            }
            TextField("Ad", text: $server.name, prompt: Text(server.code.uppercased()))
            TextField("Panel", text: $server.panel, prompt: Text("https://panel.example.com"))
            TextField("SSH", text: $server.ssh, prompt: Text("kullanıcı@sunucu"))
            TextField("Kova", text: $server.bucket, prompt: Text(server.defaultBucket))
            LabeledContent("Yedek şifresi") {
                if model.passwordFor == server.code {
                    HStack(spacing: 6) {
                        SecureField("Yedek şifresi", text: $model.password, prompt: Text("40 harf ve rakam"))
                            .labelsHidden()
                        Button("Vazgeç") {
                            model.cancelPassword()
                        }
                        Button("Yaz") {
                            Task { await model.savePassword() }
                        }
                        .keyboardShortcut(.defaultAction)
                        .disabled(model.password.isEmpty || model.busy)
                    }
                } else {
                    HStack(spacing: 6) {
                        Text(server.keychain ? "Anahtar Zinciri'nde" : "yok: yedekler açılamaz")
                            .foregroundStyle(.secondary)
                        Button(server.keychain ? "Değiştir…" : "Gir…") {
                            model.startPassword(server.code)
                        }
                        .controlSize(.small)
                        .disabled(model.busy)
                    }
                }
            }
            if let state = model.tries[server.code] {
                TryView(state: state)
            }
        }
        .textFieldStyle(.roundedBorder)
        .autocorrectionDisabled()
        .padding(.vertical, 4)
    }
}

private struct RemoveConfirm: View {
    let name: String
    @Bindable var model: SettingsModel

    var body: some View {
        HStack(alignment: .center, spacing: 8) {
            Text("\(name) bu Mac'ten kaldırılsın mı? Ayarları ve Anahtar Zinciri'ndeki yedek şifresi silinir (Vaultwarden'daki kaydı durmalı: yedekleri açmak için gerekir); sunucuya ve yedeklerine bir şey olmaz.")
                .font(.callout)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 4)
            Button("Vazgeç") {
                model.removing = nil
            }
            Button("Kaldır", role: .destructive) {
                Task { await model.remove() }
            }
            .disabled(model.busy)
        }
        .controlSize(.small)
        .padding(8)
        .background(RoundedRectangle(cornerRadius: 6).fill(Color.red.opacity(0.08)))
    }
}

/// What a try said, a line each: ✓ worked, ! worked with a word of care, ✗ failed, –
/// not tried.
private struct TryView: View {
    let state: SettingsModel.TryState

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            switch state {
            case .running:
                HStack(spacing: 6) {
                    ProgressView()
                        .controlSize(.small)
                    Text("Deneniyor: kovadaki durum dosyası, yedek deposu…")
                        .foregroundStyle(.secondary)
                }
            case .failed(let text):
                Label(text, systemImage: "exclamationmark.triangle.fill")
                    .foregroundStyle(.red)
                    .fixedSize(horizontal: false, vertical: true)
            case .done(let t):
                ForEach(t.checks, id: \.self) { c in
                    HStack(alignment: .firstTextBaseline, spacing: 6) {
                        Image(systemName: Self.symbol(c.state))
                            .foregroundStyle(Self.color(c.state))
                        Text(c.text)
                            .fixedSize(horizontal: false, vertical: true)
                            .textSelection(.enabled)
                    }
                }
            }
        }
        .font(.callout)
    }

    static func symbol(_ state: String) -> String {
        switch state {
        case "ok": "checkmark.circle.fill"
        case "warn": "exclamationmark.triangle.fill"
        case "fail": "xmark.circle.fill"
        default: "minus.circle"
        }
    }

    static func color(_ state: String) -> Color {
        switch state {
        case "ok": .green
        case "warn": .orange
        case "fail": .red
        default: .secondary
        }
    }
}

/// The new server's form: its code, name, panel, SSH, bucket and password.
private struct NewServerFields: View {
    @Bindable var model: SettingsModel

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Yeni sunucu")
                .font(.headline)
            TextField("Kod", text: field(\.code), prompt: Text("kurulumda verilen, ör. h2"))
            TextField("Ad", text: field(\.name), prompt: Text("menüde görünen, ör. Hetzner – ana"))
            TextField("Panel", text: field(\.panel), prompt: Text("https://panel.example.com"))
            TextField("SSH", text: field(\.ssh), prompt: Text("kullanıcı@sunucu"))
            TextField("Kova", text: field(\.bucket), prompt: Text("footops-\(code.isEmpty ? "<kod>" : code)"))
            SecureField("Yedek şifresi", text: field(\.password), prompt: Text("panelin Yedekleme sayfası bir kez gösterir"))
            Text("Kod, sunucuya FootOps kurulurken verilen koddur. Şifre yalnız Anahtar Zinciri'ne yazılır; boş bırakılırsa sonra girilir. Yeni kova, Cloudflare'de bu Mac'in salt okunur anahtarına eklenince okunur. Ekle'den sonra sunucu denenir.")
                .font(.callout)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            HStack {
                Spacer()
                Button("Vazgeç") {
                    model.adding = nil
                }
                Button("Ekle") {
                    Task { await model.add() }
                }
                .keyboardShortcut(.defaultAction)
                .disabled(code.isEmpty || model.busy)
            }
        }
        .textFieldStyle(.roundedBorder)
        .autocorrectionDisabled()
        .padding(.vertical, 4)
    }

    private var code: String {
        (model.adding?.code ?? "").trimmingCharacters(in: .whitespaces).lowercased()
    }

    private func field(_ key: WritableKeyPath<NewServer, String>) -> Binding<String> {
        Binding(get: { model.adding?[keyPath: key] ?? "" }, set: { model.adding?[keyPath: key] = $0 })
    }
}

/// The Mac's R2 access: what the Keychain has (the account masked), or its form.
private struct R2Fields: View {
    @Bindable var model: SettingsModel

    var body: some View {
        if model.r2Form != nil {
            TextField("Hesap kimliği", text: field(\.account), prompt: Text(model.r2?.account ?? "32 onaltılık rakam"))
                .textFieldStyle(.roundedBorder)
                .autocorrectionDisabled()
            TextField("Anahtar kimliği", text: field(\.keyID), prompt: Text("Access Key ID"))
                .textFieldStyle(.roundedBorder)
                .autocorrectionDisabled()
            SecureField("Gizli anahtar", text: field(\.secret), prompt: Text("Secret Access Key"))
                .textFieldStyle(.roundedBorder)
            VStack(alignment: .leading, spacing: 8) {
                Text("Boş bırakılan alan olduğu gibi kalır; anahtar kimliği ile gizlisi birlikte verilir. Yalnız okuma izni olan bir anahtar (Cloudflare › R2 › API token'ları).")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                HStack {
                    Spacer()
                    Button("Vazgeç") {
                        model.r2Form = nil
                    }
                    Button("Yaz") {
                        Task { await model.saveR2() }
                    }
                    .keyboardShortcut(.defaultAction)
                    .disabled(model.r2Form == NewR2() || model.busy)
                }
            }
        } else {
            LabeledContent("Hesap") {
                Text(model.r2.map { $0.account ?? "yok" } ?? "…")
                    .textSelection(.enabled)
            }
            LabeledContent("Salt okunur anahtar") {
                HStack(spacing: 6) {
                    Text(keyText)
                        .foregroundStyle(.secondary)
                    Button(model.r2?.complete == true ? "Değiştir…" : "Gir…") {
                        model.startR2()
                    }
                    .controlSize(.small)
                    .disabled(model.busy)
                }
            }
        }
    }

    private var keyText: String {
        guard let r2 = model.r2 else { return "…" }
        switch (r2.keySet, r2.secretSet) {
        case (true, true): return "Anahtar Zinciri'nde"
        case (false, false): return "yok"
        default: return "eksik"
        }
    }

    private func field(_ key: WritableKeyPath<NewR2, String>) -> Binding<String> {
        Binding(get: { model.r2Form?[keyPath: key] ?? "" }, set: { model.r2Form?[keyPath: key] = $0 })
    }
}
