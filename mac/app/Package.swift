// swift-tools-version: 6.0
// FootOps.app (panel-tasarim §29): the menu bar's view of the servers. The window and the
// menu are SwiftUI, the shell AppKit; what it shows comes from footops-mac (the Go core
// beside it in Contents/MacOS), run for each read. `make mac` builds the app bundle.
import PackageDescription

let package = Package(
    name: "FootOps",
    platforms: [.macOS(.v14)],
    products: [.executable(name: "FootOps", targets: ["FootOps"])],
    targets: [
        .target(name: "FootOpsKit"),
        .executableTarget(name: "FootOps", dependencies: ["FootOpsKit"]),
        .testTarget(name: "FootOpsKitTests", dependencies: ["FootOpsKit"]),
    ]
)
