#if os(macOS)
import CryptoKit
import Foundation
import Security

public enum BundleLaunchError: Error, Equatable {
    case missingManifest, unsupportedSchema, malformedManifest, unsafePath(String), missingPath(String), nonExecutable(String), desktopMismatch, insecureRuntimeDirectory, randomGenerationFailed, endpointTimeout, invalidEndpoint
}

public struct BundleLaunchPlan: Equatable, Sendable {
    public let executable: URL
    public let arguments: [String]
    public let workingDirectory: URL
}

public final class BundledDaemonSession:@unchecked Sendable {
    public let configuration:HTTPClientConfiguration
    private let process:Process
    init(process:Process,configuration:HTTPClientConfiguration){self.process=process;self.configuration=configuration}
    public func terminate(){if process.isRunning{process.terminate()}}
    deinit{terminate()}
}

public enum BundleManifestLauncher {
    public static func load(bundleURL: URL = Bundle.main.bundleURL, resourceURL: URL? = Bundle.main.resourceURL) throws -> BundleLaunchPlan {
        guard let resourceURL else { throw BundleLaunchError.missingManifest }
        let manifestURL = resourceURL.appending(path: "bundle-manifest.json")
        guard FileManager.default.fileExists(atPath: manifestURL.path) else { throw BundleLaunchError.missingManifest }
        let data = try Data(contentsOf: manifestURL)
        try validateShape(data)
        let manifest = try JSONDecoder().decode(Manifest.self, from: data)
        guard manifest.schemaVersion == 1, manifest.launch.base == "bundle-root" else { throw BundleLaunchError.unsupportedSchema }
        for component in manifest.components {
            let path=try resolve(component.path,under:bundleURL,executable:component.kind=="native")
            if component.kind=="asset" { let digest=SHA256.hash(data:try Data(contentsOf:path)).map{String(format:"%02x",$0)}.joined();guard component.sha256==digest else{throw BundleLaunchError.malformedManifest} }
            else if component.kind != "native" || component.integrity != "apple-code-signature" { throw BundleLaunchError.malformedManifest }
        }
        let desktop = try resolve(manifest.launch.desktop.executable, under: bundleURL, executable: true)
        guard desktop.standardizedFileURL == Bundle.main.executableURL?.standardizedFileURL || bundleURL != Bundle.main.bundleURL else { throw BundleLaunchError.desktopMismatch }
        let daemon = try resolve(manifest.launch.daemon.executable, under: bundleURL, executable: true)
        var arguments = manifest.launch.daemon.arguments
        for item in manifest.launch.daemon.pathArguments {
            arguments.append(item.flag)
            arguments.append(try resolve(item.path, under: bundleURL, executable: item.flag == "--executor-command" || item.flag == "--supervisor-command" || item.flag == "--bun").path)
        }
        guard manifest.components.map({[$0.id,$0.kind,$0.path]}) == expectedComponents,
              manifest.launch.desktop.executable == "Contents/MacOS/AgoDesktop",
              manifest.launch.daemon.executable == "Contents/Resources/Runtime/bin/ago",
              manifest.launch.daemon.arguments == ["daemon"],
              manifest.launch.daemon.pathArguments.map({[$0.flag,$0.path]}) == expectedPathArguments else { throw BundleLaunchError.malformedManifest }
        return BundleLaunchPlan(executable: daemon, arguments: arguments, workingDirectory: bundleURL)
    }

    public static func launch(_ plan: BundleLaunchPlan) throws -> Process {
        let process = Process(); process.executableURL = plan.executable; process.arguments = plan.arguments; process.currentDirectoryURL = plan.workingDirectory
        let inherited = ProcessInfo.processInfo.environment
        process.environment = ["HOME": inherited["HOME"] ?? NSHomeDirectory(), "TMPDIR": inherited["TMPDIR"] ?? NSTemporaryDirectory(), "LANG": inherited["LANG"] ?? "en_US.UTF-8", "PATH": ""]
        try process.run(); return process
    }

    public static func start(_ plan:BundleLaunchPlan,runtimeDirectory:URL?=nil,timeout:Duration = .seconds(10))async throws->BundledDaemonSession {
        let root=try privateRuntimeDirectory(runtimeDirectory)
        var random=[UInt8](repeating:0,count:32);guard SecRandomCopyBytes(kSecRandomDefault,random.count,&random)==errSecSuccess else{throw BundleLaunchError.randomGenerationFailed}
        let token=Data(random).base64EncodedString()
        let endpoint=root.appending(path:"tcp-endpoint.json"),socket=root.appending(path:"ago.sock")
        let configured=BundleLaunchPlan(executable:plan.executable,arguments:plan.arguments+dynamicArguments(root:root,endpoint:endpoint,socket:socket,token:token),workingDirectory:plan.workingDirectory)
        let process=try launch(configured)
        do{let baseURL=try await waitForEndpoint(endpoint,process:process,timeout:timeout);return BundledDaemonSession(process:process,configuration:HTTPClientConfiguration(baseURL:baseURL,bearerToken:token))}catch{if process.isRunning{process.terminate()};throw error}
    }

    static func configuredPlan(_ plan:BundleLaunchPlan,runtimeDirectory:URL,bearerToken:String)throws->BundleLaunchPlan {
        let root=try privateRuntimeDirectory(runtimeDirectory),endpoint=root.appending(path:"tcp-endpoint.json"),socket=root.appending(path:"ago.sock")
        return BundleLaunchPlan(executable:plan.executable,arguments:plan.arguments+dynamicArguments(root:root,endpoint:endpoint,socket:socket,token:bearerToken),workingDirectory:plan.workingDirectory)
    }
    private static func dynamicArguments(root:URL,endpoint:URL,socket:URL,token:String)->[String]{["--db",root.appending(path:"ago.db").path,"--socket",socket.path,"--attachments-root",root.appending(path:"attachments").path,"--tcp-listen","127.0.0.1:0","--tcp-endpoint-file",endpoint.path,"--tcp-bearer-token",token]}
    private static func privateRuntimeDirectory(_ supplied:URL?)throws->URL {
        let root=supplied ?? FileManager.default.urls(for:.applicationSupportDirectory,in:.userDomainMask)[0].appending(path:"AgoDesktop/Runtime")
        try FileManager.default.createDirectory(at:root,withIntermediateDirectories:true,attributes:[.posixPermissions:0o700]);try FileManager.default.setAttributes([.posixPermissions:0o700],ofItemAtPath:root.path)
        let canonical=root.resolvingSymlinksInPath().standardizedFileURL
        guard canonical==root.standardizedFileURL,let permissions=(try FileManager.default.attributesOfItem(atPath:root.path)[.posixPermissions] as? NSNumber)?.intValue,permissions & 0o077==0 else{throw BundleLaunchError.insecureRuntimeDirectory}
        let endpoint=root.appending(path:"tcp-endpoint.json");if FileManager.default.fileExists(atPath:endpoint.path){let attributes=try FileManager.default.attributesOfItem(atPath:endpoint.path);guard attributes[.type] as? FileAttributeType == .typeRegular,(attributes[.posixPermissions] as? NSNumber)?.intValue==0o600 else{throw BundleLaunchError.insecureRuntimeDirectory};try FileManager.default.removeItem(at:endpoint)}
        return root
    }
    private static func waitForEndpoint(_ endpoint:URL,process:Process,timeout:Duration)async throws->URL {
        let clock=ContinuousClock(),deadline=clock.now.advanced(by:timeout)
        while clock.now<deadline {if !process.isRunning{throw BundleLaunchError.invalidEndpoint};if FileManager.default.fileExists(atPath:endpoint.path){return try decodeEndpoint(endpoint)};try await Task.sleep(for:.milliseconds(25))}
        throw BundleLaunchError.endpointTimeout
    }
    static func decodeEndpoint(_ endpoint:URL)throws->URL {
        let attributes=try FileManager.default.attributesOfItem(atPath:endpoint.path)
        guard attributes[.type] as? FileAttributeType == .typeRegular,(attributes[.posixPermissions] as? NSNumber)?.intValue==0o600 else{throw BundleLaunchError.invalidEndpoint}
        let data=try Data(contentsOf:endpoint,options:.uncached);guard data.count<=1024,let raw=try JSONSerialization.jsonObject(with:data) as? [String:Any],Set(raw.keys)==["base_url"],let value=raw["base_url"] as? String,let url=URL(string:value),url.scheme=="http",url.host=="127.0.0.1",url.user==nil,url.password==nil,url.query==nil,url.fragment==nil,url.path.isEmpty else{throw BundleLaunchError.invalidEndpoint};return url
    }

    private static func resolve(_ relative: String, under root: URL, executable: Bool) throws -> URL {
        guard relative.hasPrefix("Contents/"), !relative.hasPrefix("/"), !relative.split(separator: "/", omittingEmptySubsequences: false).contains(where: { $0.isEmpty || $0 == "." || $0 == ".." }) else { throw BundleLaunchError.unsafePath(relative) }
        let canonicalRoot = root.resolvingSymlinksInPath().standardizedFileURL
        let candidate = root.appending(path: relative).resolvingSymlinksInPath().standardizedFileURL
        guard candidate.path.hasPrefix(canonicalRoot.path + "/") else { throw BundleLaunchError.unsafePath(relative) }
        var directory: ObjCBool = false
        guard FileManager.default.fileExists(atPath: candidate.path, isDirectory: &directory), !directory.boolValue else { throw BundleLaunchError.missingPath(relative) }
        guard !executable || FileManager.default.isExecutableFile(atPath: candidate.path) else { throw BundleLaunchError.nonExecutable(relative) }
        return candidate
    }

    private static func validateShape(_ data: Data) throws {
        guard let root = try JSONSerialization.jsonObject(with: data) as? [String: Any], Set(root.keys) == ["schemaVersion", "components", "launch"],
              let launch = root["launch"] as? [String: Any], Set(launch.keys) == ["base", "desktop", "daemon"],
              let desktop = launch["desktop"] as? [String: Any], Set(desktop.keys) == ["executable"],
              let daemon = launch["daemon"] as? [String: Any], Set(daemon.keys) == ["arguments", "executable", "pathArguments"],
              let pathArguments = daemon["pathArguments"] as? [[String: Any]], pathArguments.allSatisfy({ Set($0.keys) == ["flag", "path"] }),
              let components = root["components"] as? [[String: Any]], components.allSatisfy({ Set($0.keys) == ($0["kind"] as? String == "native" ? ["id", "kind", "path", "integrity"] : ["id", "kind", "path", "sha256"]) }) else { throw BundleLaunchError.malformedManifest }
    }

    private static let expectedComponents = [
        ["desktop","native","Contents/MacOS/AgoDesktop"],
        ["ago-daemon","native","Contents/Resources/Runtime/bin/ago"],
        ["ago-supervisor","native","Contents/Resources/Runtime/bin/ago-supervisor"],
        ["bun-runtime","native","Contents/Resources/Runtime/bin/bun"],
        ["pi-sidecar","asset","Contents/Resources/Runtime/pi-adapter/main.ts"],
        ["pi-provider","asset","Contents/Resources/Runtime/pi-adapter/provider-process.ts"],
        ["pi-photon-wasm","asset","Contents/Resources/Runtime/pi-adapter/photon-node/photon_rs_bg.wasm"],
        ["plugin-runtime","asset","Contents/Resources/Runtime/plugin-runtime/main.ts"]
    ]
    private static let expectedPathArguments = [
        ["--executor-command","Contents/Resources/Runtime/bin/bun"],
        ["--executor-entry","Contents/Resources/Runtime/pi-adapter/main.ts"],
        ["--supervisor-command","Contents/Resources/Runtime/bin/ago-supervisor"],
        ["--bun","Contents/Resources/Runtime/bin/bun"],
        ["--plugin-runtime","Contents/Resources/Runtime/plugin-runtime/main.ts"]
    ]

    private struct Manifest: Decodable {
        let schemaVersion: Int; let components: [Component]; let launch: Launch
        struct Component: Decodable { let id, kind, path: String; let integrity, sha256: String? }
        struct Launch: Decodable { let base: String; let desktop: Desktop; let daemon: Daemon }
        struct Desktop: Decodable { let executable: String }
        struct Daemon: Decodable { let arguments: [String]; let executable: String; let pathArguments: [PathArgument] }
        struct PathArgument: Decodable { let flag, path: String }
    }
}
#endif
