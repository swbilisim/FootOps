import AppKit
import SwiftUI
import XCTest
@testable import FootOpsKit

/// The app's views drawn from the samples, light and dark, into PNGs to look at
/// (panel-tasarim §29.9): FOOTOPS_SNAPSHOTS names the folder; without it they are drawn
/// only, so a view that cannot be drawn still fails.
@MainActor
final class SnapshotTests: XCTestCase {
    private var folder: URL? {
        ProcessInfo.processInfo.environment["FOOTOPS_SNAPSHOTS"].map { URL(fileURLWithPath: $0, isDirectory: true) }
    }

    func testMenu() throws {
        let cases: [(String, Monitor, Set<String>, Bool)] = [
            ("menu-green", Monitor(status: Samples.green, readAt: Samples.ago(1)), [], false),
            ("menu-green-open", Monitor(status: Samples.green, readAt: Samples.ago(1)), ["h1"], false),
            ("menu-troubled", Monitor(status: Samples.troubled, readAt: Samples.ago(1)), ["a"], false),
            ("menu-gray", Monitor(status: Samples.gray, readAt: Samples.ago(1)), [], false),
            ("menu-offline", Monitor(status: Samples.offline, readAt: Samples.ago(1)), [], false),
            ("menu-trouble", Monitor(status: Samples.green, readAt: Samples.ago(7), trouble: "footops-mac 60 sn içinde bitmedi, durduruldu"), [], false),
            ("menu-first", Monitor(status: nil, readAt: nil, reading: true), [], false),
            ("menu-probing", Monitor(status: Samples.green, readAt: Samples.ago(1),
                                     probe: .running(done: 7, total: 12, failed: [Samples.probeRun.domains[2]])), [], false),
            ("menu-probed", Monitor(status: Samples.troubled, readAt: Samples.ago(1), probe: .finished(Samples.probeRun)), [], true),
        ]
        for (name, monitor, expanded, all) in cases {
            let view = MenuView(monitor: monitor, actions: .none, now: Samples.now, maxHeight: nil, expanded: expanded, allProbes: all)
            try draw(name, view)
        }
    }

    func testSettings() throws {
        let path = "/Users/kisi/Library/Application Support/FootOps/ayarlar.json"
        let model = SettingsModel(view: SettingsView(path: path, servers: [
            ServerSetting(code: "a", defaultBucket: "footops-a"),
            ServerSetting(code: "h1", panel: "https://panel.example", ssh: "kisi@sunucu.example", defaultBucket: "footops-h1"),
        ]), login: .enabled, version: "abc1234")
        try drawWindow("settings", SettingsWindow(model: model), size: NSSize(width: SettingsWindow.width, height: 640))
        let failing = SettingsModel(view: SettingsView(path: path, servers: [
            ServerSetting(code: "h1", panel: "panel.example", defaultBucket: "footops-h1/kasa"),
        ]), login: .requiresApproval, version: "abc1234")
        failing.servers[0].ssh = "kisi@sunucu.example"
        failing.message = .failure("ayarlar dosyası: h1: panel adresi geçersiz: \"panel.example\" (https://… biçiminde olmalı)")
        try drawWindow("settings-error", SettingsWindow(model: failing), size: NSSize(width: SettingsWindow.width, height: 560))
    }

    /// The four icons on a light and a dark menu bar, in one strip.
    func testIcons() throws {
        let cell = NSSize(width: 44, height: 24)
        let scale: CGFloat = 4
        let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: Int(cell.width * 4 * scale), pixelsHigh: Int(cell.height * 2 * scale),
                                   bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB,
                                   bytesPerRow: 0, bitsPerPixel: 0)!
        rep.size = NSSize(width: cell.width * 4, height: cell.height * 2)
        NSGraphicsContext.saveGraphicsState()
        NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
        for (row, dark) in [false, true].enumerated() {
            let y = CGFloat(1 - row) * cell.height
            (dark ? NSColor(white: 0.16, alpha: 1) : NSColor(white: 0.92, alpha: 1)).setFill()
            NSRect(x: 0, y: y, width: cell.width * 4, height: cell.height).fill()
            for (col, light) in Light.allCases.enumerated() {
                let image = StatusIcon.image(light)
                XCTAssertGreaterThan(image.size.width, 10, "\(light)")
                NSAppearance(named: dark ? .darkAqua : .aqua)!.performAsCurrentDrawingAppearance {
                    // A template is the menu bar's to colour: drawn as it would, dimmed.
                    let shown = image.isTemplate ? StatusIcon.tinted(image, dark ? .white : .black) : image
                    shown.draw(in: NSRect(x: CGFloat(col) * cell.width + (cell.width - image.size.width) / 2,
                                          y: y + (cell.height - image.size.height) / 2, width: image.size.width, height: image.size.height),
                               from: .zero, operation: .sourceOver, fraction: image.isTemplate ? 0.35 : 1)
                }
            }
        }
        NSGraphicsContext.restoreGraphicsState()
        if let folder {
            try rep.representation(using: .png, properties: [:])!.write(to: folder.appendingPathComponent("icons.png"))
        }
    }

    /// A view with AppKit's controls in it (a form, text fields) in a window of its own,
    /// drawn the way the window server would: ImageRenderer leaves such controls out.
    private func drawWindow<V: View>(_ name: String, _ view: V, size: NSSize) throws {
        for dark in [false, true] {
            let appearance = NSAppearance(named: dark ? .darkAqua : .aqua)!
            let host = NSHostingView(rootView: view.frame(width: size.width, height: size.height))
            host.frame = NSRect(origin: .zero, size: size)
            _ = NSApplication.shared
            let window = NSWindow(contentRect: host.frame, styleMask: [.borderless], backing: .buffered, defer: false)
            window.isReleasedWhenClosed = false
            window.appearance = appearance
            window.contentView = host
            host.layoutSubtreeIfNeeded()
            RunLoop.current.run(until: Date().addingTimeInterval(0.3))
            guard let rep = host.bitmapImageRepForCachingDisplay(in: host.bounds) else {
                XCTFail("\(name): çizilemedi")
                return
            }
            host.cacheDisplay(in: host.bounds, to: rep)
            window.close()
            guard let folder else { continue }
            try rep.representation(using: .png, properties: [:])!
                .write(to: folder.appendingPathComponent("\(name)-\(dark ? "dark" : "light").png"))
        }
    }

    private func draw<V: View>(_ name: String, _ view: V, background: Bool = false) throws {
        for dark in [false, true] {
            let scheme: ColorScheme = dark ? .dark : .light
            let content = view
                .background(background ? Color(nsColor: .windowBackgroundColor) : Color(nsColor: .controlBackgroundColor))
                .environment(\.colorScheme, scheme)
            let renderer = ImageRenderer(content: content)
            renderer.scale = 2
            guard let image = renderer.cgImage else {
                XCTFail("\(name): çizilemedi")
                return
            }
            XCTAssertGreaterThan(image.height, 100, name)
            guard let folder else { continue }
            let rep = NSBitmapImageRep(cgImage: image)
            try rep.representation(using: .png, properties: [:])!
                .write(to: folder.appendingPathComponent("\(name)-\(dark ? "dark" : "light").png"))
        }
    }
}
