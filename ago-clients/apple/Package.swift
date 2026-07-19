// swift-tools-version: 6.0
import PackageDescription
let package = Package(name: "AgoAppleClients", platforms: [.macOS(.v15), .iOS(.v18)], products: [.library(name: "AgoClientCore", targets: ["AgoClientCore"]), .executable(name: "AgoDesktop", targets: ["AgoDesktop"]), .executable(name: "AgoMobile", targets: ["AgoMobile"])], targets: [.target(name: "AgoClientCore"), .executableTarget(name: "AgoDesktop", dependencies: ["AgoClientCore"]), .executableTarget(name: "AgoMobile", dependencies: ["AgoClientCore"]), .testTarget(name: "AgoClientCoreTests", dependencies: ["AgoClientCore"])])
