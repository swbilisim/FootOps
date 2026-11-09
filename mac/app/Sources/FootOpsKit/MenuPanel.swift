import AppKit
import SwiftUI

/// The menu's window: a panel hanging from the menu bar under the icon, as the system's
/// own menu bar windows do, with the popover's blur behind it. Not an NSPopover: on
/// macOS 26 each popover took ~130 MB of graphics memory for 2-3 seconds as it opened,
/// one with a single word in it too, and kept ~10 MB more after (measured 2026-11-09;
/// this panel, with the same blur: neither).
///
/// It takes the keys (Esc closes it, the menu's ⌘Q and ⌘,) without making the app the
/// active one, and closes once something else is clicked.
final class MenuPanel: NSPanel {
    /// Called once it has closed, however it came to.
    var onClose: () -> Void = {}
    private var sizeWatch: NSKeyValueObservation?
    private let anchor: () -> NSRect?

    static let radius: CGFloat = 12

    /// A panel showing host's view, placed under the rectangle anchor gives (the status
    /// item's, in screen coordinates) and following the view's preferred size (a hosting
    /// controller's, with sizingOptions .preferredContentSize).
    init(host: NSViewController, anchor: @escaping () -> NSRect?) {
        self.anchor = anchor
        super.init(contentRect: NSRect(x: 0, y: 0, width: 360, height: 200), styleMask: [.borderless, .nonactivatingPanel],
                   backing: .buffered, defer: true)
        isFloatingPanel = true
        level = .popUpMenu
        hidesOnDeactivate = false
        isReleasedWhenClosed = false
        isOpaque = false
        backgroundColor = .clear
        hasShadow = true
        animationBehavior = .none
        collectionBehavior = [.moveToActiveSpace, .fullScreenAuxiliary, .transient, .ignoresCycle]

        let blur = NSVisualEffectView()
        blur.material = .popover
        blur.blendingMode = .behindWindow
        blur.state = .active
        blur.maskImage = Self.mask
        host.view.frame = blur.bounds
        host.view.autoresizingMask = [.width, .height]
        blur.addSubview(host.view)
        contentView = blur
        // The view controller is kept with the panel: its view is in it.
        contentViewController = nil
        hostController = host

        sizeWatch = host.observe(\.preferredContentSize, options: [.new]) { [weak self] host, _ in
            MainActor.assumeIsolated {
                self?.fit(host.preferredContentSize)
            }
        }
        fit(host.preferredContentSize == .zero ? host.view.fittingSize : host.preferredContentSize)
    }

    private var hostController: NSViewController?

    override var canBecomeKey: Bool { true }

    /// Esc.
    override func cancelOperation(_ sender: Any?) {
        close()
    }

    /// Something else was clicked.
    override func resignKey() {
        super.resignKey()
        close()
    }

    override func close() {
        guard isVisible else { return }
        super.close()
        sizeWatch = nil
        onClose()
    }

    /// Sized to the view, its top edge just under the menu bar (under the icon when the
    /// bar hides itself), its left one under the icon's, kept on the screen.
    private func fit(_ size: NSSize) {
        guard size.width > 0, size.height > 0, let at = anchor() else { return }
        let screen = NSScreen.screens.first { $0.frame.contains(NSPoint(x: at.midX, y: at.midY)) } ?? NSScreen.main
        let visible = screen?.visibleFrame ?? NSRect(x: 0, y: 0, width: 1440, height: 900)
        let x = min(max(at.minX, visible.minX + 6), visible.maxX - size.width - 6)
        let top = min(at.minY, visible.maxY) - 4
        setFrame(NSRect(x: x, y: top - size.height, width: size.width, height: size.height), display: true)
        invalidateShadow()
    }

    /// The rounded corners: a mask the blur stretches to its size.
    private static let mask: NSImage = {
        let edge = radius * 2 + 1
        let image = NSImage(size: NSSize(width: edge, height: edge), flipped: false) { rect in
            NSColor.black.setFill()
            NSBezierPath(roundedRect: rect, xRadius: radius, yRadius: radius).fill()
            return true
        }
        image.capInsets = NSEdgeInsets(top: radius, left: radius, bottom: radius, right: radius)
        image.resizingMode = .stretch
        return image
    }()
}
