import AppKit
import FootOpsKit

// FootOps.app (panel-tasarim §29): an item in the menu bar, no Dock icon until a window
// opens. `FootOps --simge <dizin>` writes the icon for `make mac` and exits; `--yedekler`
// and `--arsivler [dosya]` open the Backups or the Archives window at once, the latter
// checking the archive given (to try them, to measure them).

let args = CommandLine.arguments.dropFirst()
if args.first == "--simge" {
    guard let folder = args.dropFirst().first else {
        FileHandle.standardError.write(Data("kullanım: FootOps --simge <dizin>\n".utf8))
        exit(2)
    }
    do {
        try AppIcon.writeIconset(to: URL(fileURLWithPath: folder, isDirectory: true))
        exit(0)
    } catch {
        FileHandle.standardError.write(Data("simge yazılamadı: \(error.localizedDescription)\n".utf8))
        exit(1)
    }
}

let app = NSApplication.shared
let controller = AppController()
controller.openBackupsAtLaunch = args.contains("--yedekler")
if let i = args.firstIndex(of: "--arsivler") {
    controller.openArchivesAtLaunch = true
    if let file = args.dropFirst(i - args.startIndex + 1).first, !file.hasPrefix("--") {
        controller.archiveAtLaunch = file
    }
}
app.delegate = controller
app.setActivationPolicy(.accessory)
app.run()
