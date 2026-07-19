#if os(macOS)
import CryptoKit
import Foundation
import Testing
@testable import AgoClientCore

@Suite struct BundleLauncherTests {
    @Test func resolvesClosedManifestAgainstBundleRoot() throws {
        let fixture = try Fixture()
        let plan = try BundleManifestLauncher.load(bundleURL: fixture.root, resourceURL: fixture.resources)
        #expect(plan.executable == fixture.root.appending(path:"Contents/Resources/Runtime/bin/ago"))
        #expect(plan.workingDirectory == fixture.root)
        #expect(plan.arguments == ["daemon", "--executor-command", fixture.root.appending(path:"Contents/Resources/Runtime/bin/bun").path, "--executor-entry", fixture.root.appending(path:"Contents/Resources/Runtime/pi-adapter/main.ts").path, "--supervisor-command", fixture.root.appending(path:"Contents/Resources/Runtime/bin/ago-supervisor").path, "--bun", fixture.root.appending(path:"Contents/Resources/Runtime/bin/bun").path, "--plugin-runtime", fixture.root.appending(path:"Contents/Resources/Runtime/plugin-runtime/main.ts").path])
    }

    @Test func rejectsEscapingMissingAndUnknownManifestValues() throws {
        let fixture = try Fixture()
        var manifest = fixture.manifest
        manifest = manifest.replacingOccurrences(of:"Contents/Resources/Runtime/bin/ago\"",with:"Contents/../outside\"")
        try Data(manifest.utf8).write(to:fixture.manifestURL)
        #expect(throws: BundleLaunchError.unsafePath("Contents/../outside")){try BundleManifestLauncher.load(bundleURL:fixture.root,resourceURL:fixture.resources)}
        try fixture.writeManifest();try FileManager.default.removeItem(at:fixture.root.appending(path:"Contents/Resources/Runtime/bin/ago"))
        #expect(throws: BundleLaunchError.missingPath("Contents/Resources/Runtime/bin/ago")){try BundleManifestLauncher.load(bundleURL:fixture.root,resourceURL:fixture.resources)}
        try fixture.makeFiles();try Data(fixture.manifest.replacingOccurrences(of:"\"schemaVersion\":1",with:"\"schemaVersion\":1,\"unknown\":true").utf8).write(to:fixture.manifestURL)
        #expect(throws: BundleLaunchError.malformedManifest){try BundleManifestLauncher.load(bundleURL:fixture.root,resourceURL:fixture.resources)}
    }

    @Test func appendsPrivateDynamicRuntimeArgumentsWithoutChangingManifestPlan()throws{
        let fixture=try Fixture(),runtime=FileManager.default.temporaryDirectory.appending(path:UUID().uuidString)
        defer{try? FileManager.default.removeItem(at:runtime)}
        let original=try BundleManifestLauncher.load(bundleURL:fixture.root,resourceURL:fixture.resources),token=String(repeating:"0123456789abcdef",count:3)
        let configured=try BundleManifestLauncher.configuredPlan(original,runtimeDirectory:runtime,bearerToken:token)
        #expect(configured.arguments.suffix(12)==["--db",runtime.appending(path:"ago.db").path,"--socket",runtime.appending(path:"ago.sock").path,"--attachments-root",runtime.appending(path:"attachments").path,"--tcp-listen","127.0.0.1:0","--tcp-endpoint-file",runtime.appending(path:"tcp-endpoint.json").path,"--tcp-bearer-token",token])
        let permissions=(try FileManager.default.attributesOfItem(atPath:runtime.path)[.posixPermissions] as! NSNumber).intValue
        #expect(permissions==0o700);#expect(original.arguments.count+12==configured.arguments.count)
    }

    @Test func acceptsOnlyPrivateStrictLoopbackEndpointFile()throws{
        let root=FileManager.default.temporaryDirectory.appending(path:UUID().uuidString);try FileManager.default.createDirectory(at:root,withIntermediateDirectories:true);defer{try? FileManager.default.removeItem(at:root)}
        let endpoint=root.appending(path:"endpoint.json");try Data("{\"base_url\":\"http://127.0.0.1:43123\"}\n".utf8).write(to:endpoint);try FileManager.default.setAttributes([.posixPermissions:0o600],ofItemAtPath:endpoint.path)
        #expect(try BundleManifestLauncher.decodeEndpoint(endpoint)==URL(string:"http://127.0.0.1:43123")!)
        try Data("{\"base_url\":\"http://127.0.0.1:43123\",\"token\":\"forbidden\"}".utf8).write(to:endpoint);#expect(throws:BundleLaunchError.invalidEndpoint){try BundleManifestLauncher.decodeEndpoint(endpoint)}
        try Data("{\"base_url\":\"http://192.0.2.1:43123\"}".utf8).write(to:endpoint);#expect(throws:BundleLaunchError.invalidEndpoint){try BundleManifestLauncher.decodeEndpoint(endpoint)}
    }
}

private struct Fixture {
    private let content=Data("fixture".utf8)
    let root=FileManager.default.temporaryDirectory.appending(path:UUID().uuidString+".app")
    var resources:URL{root.appending(path:"Contents/Resources")};var manifestURL:URL{resources.appending(path:"bundle-manifest.json")}
    var digest:String{SHA256.hash(data:content).map{String(format:"%02x",$0)}.joined()}
    var manifest:String{Self.template.replacingOccurrences(of:"DIGEST",with:digest)}
    private static let template=#"{"components":[{"id":"desktop","integrity":"apple-code-signature","kind":"native","path":"Contents/MacOS/AgoDesktop"},{"id":"ago-daemon","integrity":"apple-code-signature","kind":"native","path":"Contents/Resources/Runtime/bin/ago"},{"id":"ago-supervisor","integrity":"apple-code-signature","kind":"native","path":"Contents/Resources/Runtime/bin/ago-supervisor"},{"id":"bun-runtime","integrity":"apple-code-signature","kind":"native","path":"Contents/Resources/Runtime/bin/bun"},{"id":"pi-sidecar","kind":"asset","path":"Contents/Resources/Runtime/pi-adapter/main.ts","sha256":"DIGEST"},{"id":"pi-provider","kind":"asset","path":"Contents/Resources/Runtime/pi-adapter/provider-process.ts","sha256":"DIGEST"},{"id":"pi-photon-wasm","kind":"asset","path":"Contents/Resources/Runtime/pi-adapter/photon-node/photon_rs_bg.wasm","sha256":"DIGEST"},{"id":"plugin-runtime","kind":"asset","path":"Contents/Resources/Runtime/plugin-runtime/main.ts","sha256":"DIGEST"}],"launch":{"base":"bundle-root","daemon":{"arguments":["daemon"],"executable":"Contents/Resources/Runtime/bin/ago","pathArguments":[{"flag":"--executor-command","path":"Contents/Resources/Runtime/bin/bun"},{"flag":"--executor-entry","path":"Contents/Resources/Runtime/pi-adapter/main.ts"},{"flag":"--supervisor-command","path":"Contents/Resources/Runtime/bin/ago-supervisor"},{"flag":"--bun","path":"Contents/Resources/Runtime/bin/bun"},{"flag":"--plugin-runtime","path":"Contents/Resources/Runtime/plugin-runtime/main.ts"}]},"desktop":{"executable":"Contents/MacOS/AgoDesktop"}},"schemaVersion":1}"#
    init()throws{try makeFiles();try writeManifest()}
    func makeFiles()throws{for path in ["Contents/MacOS/AgoDesktop","Contents/Resources/Runtime/bin/ago","Contents/Resources/Runtime/bin/ago-supervisor","Contents/Resources/Runtime/bin/bun","Contents/Resources/Runtime/pi-adapter/main.ts","Contents/Resources/Runtime/pi-adapter/provider-process.ts","Contents/Resources/Runtime/pi-adapter/photon-node/photon_rs_bg.wasm","Contents/Resources/Runtime/plugin-runtime/main.ts"]{let url=root.appending(path:path);try FileManager.default.createDirectory(at:url.deletingLastPathComponent(),withIntermediateDirectories:true);FileManager.default.createFile(atPath:url.path,contents:content);try FileManager.default.setAttributes([.posixPermissions:0o755],ofItemAtPath:url.path)}}
    func writeManifest()throws{try Data(manifest.utf8).write(to:manifestURL)}
}
#endif
