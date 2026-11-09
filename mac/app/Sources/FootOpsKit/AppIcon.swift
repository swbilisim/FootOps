import AppKit

/// The app's icon, drawn: footprints on a green-to-teal tile. `FootOps --simge <dizin>`
/// writes it as an iconset for `make mac` (iconutil turns it into AppIcon.icns, which the
/// Dock and Finder show), so no picture is kept in the repository.
@MainActor
public enum AppIcon {
    /// The tile at a size, in points (pixels at 1x).
    public static func draw(side: CGFloat) {
        // Apple's grid: the tile is 824 of 1024, its corners 185.
        let unit = side / 1024
        let tile = NSRect(x: 100 * unit, y: 100 * unit, width: 824 * unit, height: 824 * unit)
        let path = NSBezierPath(roundedRect: tile, xRadius: 185 * unit, yRadius: 185 * unit)
        NSGraphicsContext.saveGraphicsState()
        let shadow = NSShadow()
        shadow.shadowColor = NSColor.black.withAlphaComponent(0.28)
        shadow.shadowBlurRadius = 18 * unit
        shadow.shadowOffset = NSSize(width: 0, height: -8 * unit)
        shadow.set()
        NSColor(srgbRed: 0.07, green: 0.45, blue: 0.45, alpha: 1).setFill()
        path.fill()
        NSGraphicsContext.restoreGraphicsState()
        NSGradient(colors: [NSColor(srgbRed: 0.27, green: 0.80, blue: 0.47, alpha: 1),
                            NSColor(srgbRed: 0.05, green: 0.47, blue: 0.53, alpha: 1)])?.draw(in: path, angle: -90)

        let config = NSImage.SymbolConfiguration(pointSize: 430 * unit, weight: .semibold)
        guard let symbol = NSImage(systemSymbolName: StatusIcon.symbol, accessibilityDescription: nil)?
            .withSymbolConfiguration(config) else { return }
        let white = StatusIcon.tinted(symbol, .white)
        let size = symbol.size
        white.draw(in: NSRect(x: tile.midX - size.width / 2, y: tile.midY - size.height / 2 - 6 * unit,
                              width: size.width, height: size.height))
    }

    /// The PNG of the icon at pixels × pixels.
    static func png(pixels: Int) -> Data? {
        guard let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: pixels, pixelsHigh: pixels, bitsPerSample: 8,
                                         samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB,
                                         bytesPerRow: 0, bitsPerPixel: 0) else { return nil }
        rep.size = NSSize(width: pixels, height: pixels)
        NSGraphicsContext.saveGraphicsState()
        defer { NSGraphicsContext.restoreGraphicsState() }
        NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
        draw(side: CGFloat(pixels))
        NSGraphicsContext.current?.flushGraphics()
        return rep.representation(using: .png, properties: [:])
    }

    /// An iconset folder (AppIcon.iconset) as iconutil takes it.
    public static func writeIconset(to folder: URL) throws {
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        for points in [16, 32, 128, 256, 512] {
            for scale in [1, 2] {
                guard let data = png(pixels: points * scale) else {
                    throw CocoaError(.fileWriteUnknown)
                }
                let name = scale == 1 ? "icon_\(points)x\(points).png" : "icon_\(points)x\(points)@2x.png"
                try data.write(to: folder.appendingPathComponent(name))
            }
        }
    }
}
