import AppKit
import os
import SwiftUI

/// The app's shell (panel-tasarim §29.2): the menu bar's item and the window it opens,
/// the Settings window, sleep and wake, starting with the Mac. AppKit's own status item
/// rather than SwiftUI's MenuBarExtra: only it can colour the icon, tell when the menu
/// opens and close it from an item; the menu is a panel of ours (MenuPanel).
@MainActor
public final class AppController: NSObject, NSApplicationDelegate, NSWindowDelegate {
    private let core: Core?
    private let monitor: Monitor
    private let version: String
    private var item: NSStatusItem?
    private var menu: MenuPanel?
    /// When the menu last closed: the click on the icon that closed it (the panel lets go
    /// of the keys on the mouse's way down) does not open it again on the way up.
    private var menuClosedAt = Date.distantPast
    private var menuHeight: CGFloat = 560
    private var settingsWindow: NSWindow?
    private var settingsModel: SettingsModel?
    private var backupsWindow: NSWindow?
    private var backupsModel: BackupsModel?
    /// The recovery line's minute on the pasteboard runs out with the window closed too.
    private let clipboard = RecoveryClipboard()
    /// The Backups window opens at launch (FootOps --yedekler).
    public var openBackupsAtLaunch = false
    private var observers: [NSObjectProtocol] = []
    private var terminate: DispatchSourceSignal?
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

        menuHeight = min(720, (NSScreen.main?.visibleFrame.height ?? 900) - 140)

        let center = NSWorkspace.shared.notificationCenter
        observers.append(center.addObserver(forName: NSWorkspace.didWakeNotification, object: nil, queue: .main) { [weak self] _ in
            MainActor.assumeIsolated { self?.monitor.didWake() }
        })
        observers.append(center.addObserver(forName: NSWorkspace.willSleepNotification, object: nil, queue: .main) { [weak self] _ in
            MainActor.assumeIsolated { self?.monitor.willSleep() }
        })

        // SIGTERM (make mac-install's pkill) quits as the menu's Çık does: the core runs
        // under way are stopped, not left behind. From the run loop, not inside the main
        // queue's block: quitting may wait for an open's cleanup (terminateLater), and
        // the task that answers runs on that queue.
        signal(SIGTERM, SIG_IGN)
        let source = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
        source.setEventHandler {
            RunLoop.main.perform {
                MainActor.assumeIsolated { NSApp.terminate(nil) }
            }
        }
        source.resume()
        terminate = source

        startWithTheMacOnce()
        monitor.start()
        if openBackupsAtLaunch {
            showBackups()
        }
        log.info("açıldı: \(self.version, privacy: .public), çekirdek \(self.core?.executable.path ?? "yok", privacy: .public)")
    }

    /// Quitting (the menu's Çık, ⌘Q, make mac-install's SIGTERM) with a backup being
    /// opened: it is stopped and the core given time to remove what it began (at most
    /// 30 seconds) before the app goes.
    public func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        guard let model = backupsModel, model.isBusy else { return .terminateNow }
        log.info("kapanıyor: süren yedek açma durduruluyor")
        Task { @MainActor in
            await model.stopAndWait(limit: .seconds(30))
            NSApp.reply(toApplicationShouldTerminate: true)
        }
        return .terminateLater
    }

    public func applicationWillTerminate(_ notification: Notification) {
        monitor.stop()
        backupsModel?.stop()
        // A recovery line still on the pasteboard goes with the app.
        clipboard.takeOff()
        log.info("kapandı")
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
                self?.menu?.close()
                NSWorkspace.shared.open(url)
            },
            backups: { [weak self] in
                self?.menu?.close()
                self?.showBackups()
            },
            settings: { [weak self] in
                self?.menu?.close()
                self?.showSettings()
            },
            quit: {
                NSApp.terminate(nil)
            })
    }

    @objc private func toggleMenu(_ sender: Any?) {
        if let menu {
            menu.close()
        } else if Date().timeIntervalSince(menuClosedAt) > 0.3 {
            showMenu()
        }
    }

    /// The menu is made anew each time it opens and let go once it closes: its window,
    /// views and layers are not kept between openings; making it takes a few
    /// milliseconds.
    private func showMenu() {
        guard menu == nil, let button = item?.button else { return }
        monitor.menuOpened()
        let host = NSHostingController(rootView: MenuView(monitor: monitor, actions: actions, maxHeight: menuHeight))
        host.sizingOptions = [.preferredContentSize]
        let panel = MenuPanel(host: host) { [weak button] in
            guard let button, let window = button.window else { return nil }
            return window.convertToScreen(button.convert(button.bounds, to: nil))
        }
        panel.onClose = { [weak self, weak panel] in
            guard let self, let panel, self.menu === panel else { return }
            self.menu = nil
            self.menuClosedAt = Date()
            self.item?.button?.highlight(false)
        }
        menu = panel
        button.highlight(true)
        panel.makeKeyAndOrderFront(nil)
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

    // MARK: Backups

    private func showBackups() {
        if backupsWindow == nil {
            let model = BackupsModel(core: core, clipboard: clipboard)
            let actions = BackupsActions(reveal: { path in
                NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: path)])
            })
            let window = NSWindow(contentViewController: NSHostingController(rootView: BackupsWindow(model: model, actions: actions)))
            window.title = "FootOps Yedekleri"
            window.styleMask = [.titled, .closable, .miniaturizable, .resizable]
            window.setContentSize(BackupsWindow.size)
            window.contentMinSize = NSSize(width: 760, height: 520)
            window.isReleasedWhenClosed = false
            window.delegate = self
            window.center()
            backupsWindow = window
            backupsModel = model
            Task { await model.load() }
        }
        NSApp.setActivationPolicy(.regular)
        backupsWindow?.makeKeyAndOrderFront(nil)
        NSApp.activate()
    }

    public func windowWillClose(_ notification: Notification) {
        let window = notification.object as? NSWindow
        if window === backupsWindow {
            // An open under way stops with it (the core removes what it began).
            backupsModel?.stop()
        }
        guard window === settingsWindow || window === backupsWindow else { return }
        // Opened seldom, read anew each time: nothing of them is kept once they close
        // (after the close has run its course); the Dock icon goes with the last.
        DispatchQueue.main.async { [weak self] in
            guard let self else { return }
            if self.settingsWindow?.isVisible != true {
                self.settingsWindow = nil
                self.settingsModel = nil
            }
            if self.backupsWindow?.isVisible != true {
                self.backupsWindow = nil
                self.backupsModel = nil
            }
            if self.settingsWindow == nil && self.backupsWindow == nil {
                NSApp.setActivationPolicy(.accessory)
            }
            if window === self.backupsWindow || self.backupsWindow == nil {
                self.monitor.refresh() // the menu's reminder of what was opened
            }
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
