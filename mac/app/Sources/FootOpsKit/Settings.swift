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

/// The Settings window's state: each server's panel, SSH target and bucket, read and
/// written through footops-mac ayar (the rules are the core's), and the login item.
@MainActor @Observable
public final class SettingsModel {
    public var servers: [ServerSetting] = []
    public private(set) var path = ""
    /// The file or the Keychain could not be read (what could be is listed).
    public private(set) var loadError: String?
    public private(set) var loaded: [ServerSetting] = []
    public private(set) var busy = false
    public var message: Message?
    public private(set) var login: LoginState
    public let version: String

    public enum Message: Equatable, Sendable {
        case success(String)
        case failure(String)
    }

    @ObservationIgnored private let core: Core?
    /// Called once new settings are written (the menu reads the status again).
    @ObservationIgnored public var onSaved: () -> Void = {}

    public init(core: Core?, version: String) {
        self.core = core
        self.version = version
        login = LoginItem.state
    }

    /// A model holding what it is given (the image tests').
    public init(view: SettingsView, login: LoginState, version: String) {
        core = nil
        self.version = version
        self.login = login
        apply(view)
    }

    /// Edited since read.
    public var dirty: Bool { servers != loaded }

    /// Reads the settings anew; not while a read or a save is under way (one at a time:
    /// a read begun before a save's end would show what was there before it).
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
    }

    public func save() async {
        guard let core, !busy else { return }
        busy = true
        defer { busy = false }
        message = nil
        do {
            let input = try JSONEncoder().encode(SettingsFile(servers))
            let out = try await core.run(["ayar", "yaz", "--json"], input: input, timeout: .seconds(30))
            switch Core.decode(SettingsView.self, from: out) {
            case .success(let v):
                apply(v)
                message = .success("Kaydedildi")
                onSaved()
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

    private func apply(_ v: SettingsView) {
        path = v.path
        loadError = v.error
        servers = v.servers
        loaded = v.servers
    }
}

/// The Settings window (panel-tasarim §29.2): per server the panel (opened in the
/// browser), the SSH target (opened in the Terminal), the bucket; starting with the Mac.
public struct SettingsWindow: View {
    @Bindable var model: SettingsModel

    public static let width: CGFloat = 540

    public init(model: SettingsModel) {
        self.model = model
    }

    public var body: some View {
        VStack(spacing: 0) {
            Form {
                Section {
                    if model.servers.isEmpty {
                        Text(model.busy ? "Okunuyor…" : "Bilinen sunucu yok: Anahtar Zinciri'nde footops-yedek / restic-<sunucu> kaydı gerekir.")
                            .foregroundStyle(.secondary)
                    }
                    ForEach($model.servers) { $server in
                        ServerFields(server: $server)
                    }
                } header: {
                    Text("Sunucular")
                } footer: {
                    Text("Panel tarayıcıda, SSH Terminal'de açılır. Kova boşsa varsayılanı okunur. Adresler yalnız bu Mac'te, ayar dosyasında durur.")
                        .foregroundStyle(.secondary)
                }
                Section("Genel") {
                    Toggle("Mac açılınca FootOps'u başlat", isOn: Binding(
                        get: { model.login == .enabled || model.login == .requiresApproval },
                        set: { model.setLogin($0) }
                    ))
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
                .keyboardShortcut(.defaultAction)
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

private struct ServerFields: View {
    @Binding var server: ServerSetting

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text(server.code.uppercased())
                    .font(.headline)
                if !server.keychain {
                    Text("yalnız ayarlarda")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
            TextField("Panel", text: $server.panel, prompt: Text("https://panel.example.com"))
            TextField("SSH", text: $server.ssh, prompt: Text("kullanıcı@sunucu"))
            TextField("Kova", text: $server.bucket, prompt: Text(server.defaultBucket))
        }
        .textFieldStyle(.roundedBorder)
        .autocorrectionDisabled()
        .padding(.vertical, 4)
    }
}
