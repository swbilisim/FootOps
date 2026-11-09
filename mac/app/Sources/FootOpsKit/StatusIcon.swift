import AppKit

/// The menu bar's icon (panel-tasarim §29.3): footprints — FootOps — in the whole's
/// colour. Gray is the system's own template image, drawn dimmed (the status item's
/// appearsDisabled), as an icon with nothing to say.
@MainActor
public enum StatusIcon {
    static let symbol = "shoeprints.fill"

    public static func image(_ light: Light) -> NSImage {
        let config = NSImage.SymbolConfiguration(pointSize: 14, weight: .semibold)
        let base = NSImage(systemSymbolName: symbol, accessibilityDescription: "FootOps")?.withSymbolConfiguration(config)
            ?? NSImage(size: NSSize(width: 18, height: 18))
        if light == .gray {
            let template = base.copy() as! NSImage
            template.isTemplate = true
            return template
        }
        return tinted(base, color(light))
    }

    /// The symbol filled with a colour, resolved when drawn: the menu bar's appearance
    /// (light or dark, from the wallpaper) picks the yellow.
    static func tinted(_ base: NSImage, _ color: NSColor) -> NSImage {
        let image = NSImage(size: base.size, flipped: false) { rect in
            base.draw(in: rect)
            color.set()
            rect.fill(using: .sourceAtop)
            return true
        }
        image.isTemplate = false
        image.cacheMode = .never
        image.accessibilityDescription = "FootOps"
        return image
    }

    static func color(_ light: Light) -> NSColor {
        switch light {
        case .green:
            .systemGreen
        case .yellow:
            // The system's yellow is pale on a light menu bar: a deeper one there.
            NSColor(name: nil) { appearance in
                appearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua
                    ? .systemYellow : NSColor(srgbRed: 0.80, green: 0.58, blue: 0.0, alpha: 1)
            }
        case .red:
            .systemRed
        case .gray:
            .secondaryLabelColor
        }
    }
}
