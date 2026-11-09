import AppKit
import os
import SwiftUI

/// The app's shell (panel-tasarim §29.2): the menu bar's item and the window it opens,
/// the Settings window, sleep and wake, starting with the Mac. AppKit's own status item
/// rather than SwiftUI's MenuBarExtra: only it can colour the icon, tell when the menu
/// opens and close it from an item.
@MainActor
public final class AppController: NSObject, NSApplicationDelegate, NSPopoverDelegate, NSWindowDelegate {
    private let core: Core?
    private let monitor: Monitor
    private let version: String
    private var item: NSStatusItem?
    private let popover = NSPopover()
    private var settingsWindow: NSWindow?
    private var settingsModel: SettingsModel?
    private var observers: [NSObjectProtocol] = []
    private let log = Logger(subsystem: "io.github.swbilisim.footops", category: "app")

    public override init() {
        core = Core.bundled()
        monitor = Monitor(core: core)
        version = Bundle.main.object(forInfoDictionaryKey: "FootOpsVersion") as? String ?? "geliştirme"
        super.init()
    }

    public func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.mainMenu = Self.mainMenu()

        let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        item.autosaveName = "FootOps"
        item.button?.target = self
        item.button?.action = #selector(toggleMenu(_:))
        self.item = item
        showLight()

        let height = min(720, (NSScreen.main?.visibleFrame.height ?? 900) - 140)
        let host = NSHostingController(rootView: MenuView(monitor: monitor, actions: actions, maxHeight: height))
        host.sizingOptions = [.preferredContentSize]
        popover.contentViewController = host
        popover.behavior = .transient
        popover.delegate = self

        let center = NSWorkspace.shared.notificationCenter
        observers.append(center.addObserver(forName: NSWorkspace.didWakeNotification, object: nil, queue: .main) { [weak self] _ in
            MainActor.assumeIsolated { self?.monitor.didWake() }
        })
        observers.append(center.addObserver(forName: NSWorkspace.willSleepNotification, object: nil, queue: .main) { [weak self] _ in
            MainActor.assumeIsolated { self?.monitor.willSleep() }
        })

        startWithTheMacOnce()
        monitor.start()
        log.info("açıldı: \(self.version, privacy: .public), çekirdek \(self.core?.executable.path ?? "yok", privacy: .public)")
    }

    public func applicationWillTerminate(_ notification: Notification) {
        monitor.stop()
    }

    /// The app opened again while running (from Finder, Spotlight): the menu.
    public func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        if !flag {
            showMenu()
        }
        return false
    }

    // MARK: the menu

    private var actions: MenuActions {
        MenuActions(
            open: { [weak self] url in
                self?.popover.performClose(nil)
                NSWorkspace.shared.open(url)
            },
            settings: { [weak self] in
                self?.popover.performClose(nil)
                self?.showSettings()
            },
            quit: {
                NSApp.terminate(nil)
            })
    }

    @objc private func toggleMenu(_ sender: Any?) {
        if popover.isShown {
            popover.performClose(sender)
        } else {
            showMenu()
        }
    }

    private func showMenu() {
        guard let button = item?.button else { return }
        monitor.menuOpened()
        popover.show(relativeTo: button.bounds, of: button, preferredEdge: .minY)
        // Keys (Esc, ⌘Q, ⌘,) reach the menu only when it is the key window.
        NSApp.activate()
        popover.contentViewController?.view.window?.makeKey()
    }

    /// The icon follows the whole's colour.
    private func showLight() {
        let light = withObservationTracking {
            monitor.light
        } onChange: { [weak self] in
            Task { @MainActor in self?.showLight() }
        }
        guard let button = item?.button else { return }
        button.image = StatusIcon.image(light)
        button.appearsDisabled = light == .gray
        button.toolTip = "FootOps: " + light.summary
        button.setAccessibilityLabel("FootOps: " + light.summary)
    }

    // MARK: Settings

    private func showSettings() {
        if settingsWindow == nil {
            let model = SettingsModel(core: core, version: version)
            model.onSaved = { [weak self] in self?.monitor.refresh() }
            let window = NSWindow(contentViewController: NSHostingController(rootView: SettingsWindow(model: model)))
            window.title = "FootOps Ayarları"
            window.styleMask = [.titled, .closable, .miniaturizable, .resizable]
            window.setContentSize(NSSize(width: SettingsWindow.width, height: 620))
            window.contentMinSize = NSSize(width: 460, height: 360)
            window.isReleasedWhenClosed = false
            window.delegate = self
            window.center()
            settingsWindow = window
            settingsModel = model
        }
        // Read anew when it opens; open already, what is being typed in it stays.
        if let model = settingsModel, settingsWindow?.isVisible != true {
            model.message = nil
            Task { await model.load() }
        }
        // A window of its own: the app shows in the Dock while it is open.
        NSApp.setActivationPolicy(.regular)
        settingsWindow?.makeKeyAndOrderFront(nil)
        NSApp.activate()
    }

    public func windowWillClose(_ notification: Notification) {
        if (notification.object as? NSWindow) === settingsWindow {
            NSApp.setActivationPolicy(.accessory)
        }
    }

    /// The first run from Applications turns starting with the Mac on (panel-tasarim
    /// §29.1); the Settings window turns it off. Never from a build folder.
    private func startWithTheMacOnce() {
        let key = "acilistaBaslatKuruldu"
        guard !UserDefaults.standard.bool(forKey: key), Bundle.main.bundlePath.hasPrefix("/Applications/") else { return }
        UserDefaults.standard.set(true, forKey: key)
        do {
            try LoginItem.set(true)
        } catch {
            log.error("açılışta başlatma kurulamadı: \(error.localizedDescription, privacy: .public)")
        }
    }

    /// The menu bar's menus while a window is open: what text fields need (copy, paste,
    /// undo) and closing it.
    static func mainMenu() -> NSMenu {
        let main = NSMenu()
        let appItem = NSMenuItem()
        let app = NSMenu()
        app.addItem(withTitle: "FootOps'tan Çık", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appItem.submenu = app
        main.addItem(appItem)

        let editItem = NSMenuItem()
        let edit = NSMenu(title: "Düzen")
        edit.addItem(withTitle: "Geri Al", action: Selector(("undo:")), keyEquivalent: "z")
        let redo = edit.addItem(withTitle: "Yinele", action: Selector(("redo:")), keyEquivalent: "z")
        redo.keyEquivalentModifierMask = [.command, .shift]
        edit.addItem(.separator())
        edit.addItem(withTitle: "Kes", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        edit.addItem(withTitle: "Kopyala", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "Yapıştır", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        edit.addItem(withTitle: "Tümünü Seç", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editItem.submenu = edit
        main.addItem(editItem)

        let windowItem = NSMenuItem()
        let window = NSMenu(title: "Pencere")
        window.addItem(withTitle: "Kapat", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
        window.addItem(withTitle: "Küçült", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
        windowItem.submenu = window
        main.addItem(windowItem)
        return main
    }
}
