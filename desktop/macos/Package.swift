// swift-tools-version:5.10
//
// PGBot for macOS — a native SwiftUI front end over the existing pgbot Go engine.
//
//   PGBotKit        business logic: engine adapter, models, Keychain, SQLite
//                   persistence, monitoring scheduler, finding diff, redaction,
//                   OAuth/PKCE. No SwiftUI, so it is testable headless.
//   PGBot           the SwiftUI application (executable; bundled into PGBot.app
//                   by scripts/build-app.sh).
//   PGBotKitChecks  the test suite as an executable. Command Line Tools ship no
//                   XCTest, so `swift run PGBotKitChecks` is the portable runner.
import PackageDescription

let package = Package(
    name: "PGBotDesktop",
    platforms: [.macOS(.v14)],
    products: [
        .library(name: "PGBotKit", targets: ["PGBotKit"]),
    ],
    targets: [
        .target(name: "PGBotKit", path: "Sources/PGBotKit", linkerSettings: [.linkedLibrary("sqlite3")]),
        .executableTarget(name: "PGBotKitChecks", dependencies: ["PGBotKit"], path: "Tests/PGBotKitChecks"),
    ]
)
