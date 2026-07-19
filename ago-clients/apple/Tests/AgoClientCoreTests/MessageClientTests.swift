import Foundation
import Testing
@testable import AgoClientCore

@Suite struct MessageClientTests {
    @Test func mentionDTOIsCanonicalAndRejectsUnsafePaths() throws {
        let mention = try RepositoryFileMention(path: "Sources/App/Main.swift")
        let encoded = try JSONEncoder().encode(mention)
        #expect(try JSONDecoder().decode(JSONValue.self, from: encoded) == .object(["path": .string("Sources/App/Main.swift")]))
        for path in ["", "/tmp/a", "../a", "a/../b", "a\\b", ".git/config", "a//b", "a/"] { #expect(throws: MessageCompositionError.invalidFileMention) { try RepositoryFileMention(path: path) } }
    }

    @Test func immutableAttachmentFailsWhenMissingOrChanged() throws {
        let directory = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString); try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let missingURL = directory.appending(path: "missing.txt")
        #expect(throws: MessageCompositionError.attachmentMissing) { try ImmutableAttachment.capture(url: missingURL, mediaType: "text/plain", attachmentID: "att-missing") }
        let changedURL = directory.appending(path: "changed.txt"); try Data("before".utf8).write(to: changedURL)
        let attachment = try ImmutableAttachment.capture(url: changedURL, mediaType: "text/plain", attachmentID: "att-changed")
        try Data("changed bytes".utf8).write(to: changedURL)
        #expect(throws: MessageCompositionError.attachmentChanged) { try attachment.verifiedBytes() }
        try FileManager.default.removeItem(at: changedURL)
        #expect(throws: MessageCompositionError.attachmentMissing) { try attachment.verifiedBytes() }
    }

    @Test func readIsBoundedToDaemonAttachmentLimit() throws {
        let url = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: url) }
        try Data(repeating: 1, count: Int(MessageInputLimits.attachmentBytes) + 1).write(to: url)
        #expect(throws: MessageCompositionError.attachmentTooLarge) { try ImmutableAttachment.capture(url: url, mediaType: "application/octet-stream") }
    }

    @Test func exactUploadAndSubmitContracts() throws {
        let ref = AttachmentRef(attachmentID: "att-1", sha256: String(repeating: "a", count: 64), sizeBytes: 3, mediaType: "text/plain", filename: "a.txt")
        let upload = try HTTPMessageTransport.uploadRequest(baseURL: URL(string: "https://example.test/api/")!, threadID: "t/1", attachment: ref, bytes: Data("abc".utf8))
        #expect(upload.httpMethod == "POST"); #expect(upload.url?.path(percentEncoded: true) == "/api/v1/threads/t%2F1/attachments"); #expect(upload.httpBody == Data("abc".utf8)); #expect(upload.value(forHTTPHeaderField: "X-Ago-Attachment-Ref") != nil)
        let input = try MessageInput(text: "Review", attachments: [ref], fileMentions: [try RepositoryFileMention(path: "Sources/A.swift")])
        let envelope = MutationEnvelope(commandID: "cmd", idempotencyKey: "idem", actorID: "mac", expectedSequence: 9)
        let submit = try HTTPMessageTransport.submitRequest(baseURL: URL(string: "https://example.test/api/")!, threadID: "t/1", envelope: envelope, input: input)
        #expect(submit.httpMethod == "POST"); #expect(submit.url?.path(percentEncoded: true) == "/api/v1/threads/t%2F1/messages")
        let body = try JSONDecoder().decode(JSONValue.self, from: submit.httpBody!)
        #expect(body.objectValue?["class"] == .string("normal")); #expect(body.objectValue?["expected_sequence"] == .number(9)); #expect(body.objectValue?["content"]?.objectValue?["file_mentions"] == .array([.object(["path": .string("Sources/A.swift")])]))
        let authenticated=HTTPClientConfiguration(baseURL:URL(string:"http://127.0.0.1:1")!,bearerToken:"token")
        #expect(try HTTPMessageTransport.uploadRequest(configuration:authenticated,threadID:"t",attachment:ref,bytes:Data()).value(forHTTPHeaderField:"Authorization")=="Bearer token")
        #expect(try HTTPMessageTransport.submitRequest(configuration:authenticated,threadID:"t",envelope:envelope,input:input).value(forHTTPHeaderField:"Authorization")=="Bearer token")
    }

    @MainActor @Test func uploadsAllAttachmentsBeforeCanonicalSubmit() async throws {
        let directory=FileManager.default.temporaryDirectory.appending(path:UUID().uuidString);try FileManager.default.createDirectory(at:directory,withIntermediateDirectories:true);defer{try? FileManager.default.removeItem(at:directory)}
        let url=directory.appending(path:"note.txt");try Data("note".utf8).write(to:url)
        let attachment=try ImmutableAttachment.capture(url:url,mediaType:"text/plain",attachmentID:"att-order")
        let transport=OrderedMessageTransport();let client=MessageClient(cursor:ProjectionCursor(threadID:"thread"),transport:transport,projectionTransport:FailingProjectionTransport(),actorID:"mac")
        await #expect(throws:URLError.self){try await client.submit(text:"Review",attachments:[attachment],fileMentions:[try RepositoryFileMention(path:"Sources/A.swift")])}
        let operations=await transport.operations
        #expect(operations == ["upload:att-order","submit:att-order:Sources/A.swift"])
    }
}

private actor OrderedMessageTransport:MessageTransport{private(set)var operations:[String]=[];func upload(threadID:String,attachment:AttachmentRef,bytes:Data)async throws{operations.append("upload:\(attachment.attachmentID)")};func submit(threadID:String,envelope:MutationEnvelope,input:MessageInput)async throws{operations.append("submit:\(input.attachments[0].attachmentID):\(input.fileMentions[0].path)")}}
private actor FailingProjectionTransport:ProjectionTransport{func fetchProjection(_ request:ProjectionRequest)async throws->Data{throw URLError(.badServerResponse)}}
