import CryptoKit
import Foundation
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

public enum MessageInputLimits {
    public static let textBytes = 256 << 10
    public static let attachmentCount = 16
    public static let fileMentionCount = 64
    public static let attachmentBytes: UInt64 = 4 << 20
    public static let aggregateAttachmentBytes: UInt64 = 16 << 20
    public static let repositoryPathBytes = 4096
}

public struct AttachmentRef: Codable, Equatable, Sendable {
    public let attachmentID, sha256: String
    public let sizeBytes: UInt64
    public let mediaType, filename: String
    enum CodingKeys: String, CodingKey { case attachmentID = "attachment_id", sha256, sizeBytes = "size_bytes", mediaType = "media_type", filename }
    public init(attachmentID: String, sha256: String, sizeBytes: UInt64, mediaType: String, filename: String) {
        self.attachmentID = attachmentID; self.sha256 = sha256; self.sizeBytes = sizeBytes; self.mediaType = mediaType; self.filename = filename
    }
}

public struct RepositoryFileMention: Codable, Equatable, Sendable {
    public let path: String
    public init(path: String) throws { try Self.validate(path); self.path = path }
    private static func validate(_ path: String) throws {
        guard !path.isEmpty, path.lengthOfBytes(using: .utf8) <= MessageInputLimits.repositoryPathBytes,
              !path.contains("\0"), !path.contains("\\"), !path.hasPrefix("/"), !path.hasSuffix("/") else { throw MessageCompositionError.invalidFileMention }
        let parts = path.split(separator: "/", omittingEmptySubsequences: false)
        guard !parts.isEmpty, parts.allSatisfy({ !$0.isEmpty && $0 != "." && $0 != ".." && $0.lowercased() != ".git" }) else { throw MessageCompositionError.invalidFileMention }
    }
}

public struct MessageInput: Codable, Equatable, Sendable {
    public let text: String
    public let attachments: [AttachmentRef]
    public let fileMentions: [RepositoryFileMention]
    enum CodingKeys: String, CodingKey { case text, attachments, fileMentions = "file_mentions" }
    public init(text: String, attachments: [AttachmentRef] = [], fileMentions: [RepositoryFileMention] = []) throws {
        guard text.lengthOfBytes(using: .utf8) <= MessageInputLimits.textBytes, !text.contains("\0"),
              !text.isEmpty || !attachments.isEmpty || !fileMentions.isEmpty,
              attachments.count <= MessageInputLimits.attachmentCount,
              fileMentions.count <= MessageInputLimits.fileMentionCount,
              attachments.reduce(UInt64(0), { $0 + $1.sizeBytes }) <= MessageInputLimits.aggregateAttachmentBytes,
              Set(attachments.map(\.attachmentID)).count == attachments.count,
              Set(fileMentions.map(\.path)).count == fileMentions.count else { throw MessageCompositionError.invalidMessage }
        self.text = text; self.attachments = attachments; self.fileMentions = fileMentions
    }
    public func encode(to encoder: Encoder) throws { var values=encoder.container(keyedBy:CodingKeys.self);if !text.isEmpty{try values.encode(text,forKey:.text)};if !attachments.isEmpty{try values.encode(attachments,forKey:.attachments)};if !fileMentions.isEmpty{try values.encode(fileMentions,forKey:.fileMentions)} }
    public init(from decoder: Decoder) throws { let values=try decoder.container(keyedBy:CodingKeys.self);try self.init(text:values.decodeIfPresent(String.self,forKey:.text) ?? "",attachments:values.decodeIfPresent([AttachmentRef].self,forKey:.attachments) ?? [],fileMentions:values.decodeIfPresent([RepositoryFileMention].self,forKey:.fileMentions) ?? []) }
}

public struct ImmutableAttachment: Equatable, Sendable, Identifiable {
    public let sourceURL: URL
    public let ref: AttachmentRef
    private let fingerprint: FileFingerprint
    public var id: String { ref.attachmentID }

    public static func capture(url: URL, mediaType: String, attachmentID: String = "att-\(UUID().uuidString)") throws -> ImmutableAttachment {
        let before = try FileFingerprint.read(url)
        let bytes = try boundedRead(url)
        let after = try FileFingerprint.read(url)
        guard before == after else { throw MessageCompositionError.attachmentChanged }
        let filename = url.lastPathComponent
        guard validAttachmentID(attachmentID), validFilename(filename), validMediaType(mediaType) else { throw MessageCompositionError.invalidAttachment }
        let digest = SHA256.hash(data: bytes).map { String(format: "%02x", $0) }.joined()
        return ImmutableAttachment(sourceURL: url, ref: AttachmentRef(attachmentID: attachmentID, sha256: digest, sizeBytes: UInt64(bytes.count), mediaType: mediaType, filename: filename), fingerprint: after)
    }

    public func verifiedBytes() throws -> Data {
        guard try FileFingerprint.read(sourceURL) == fingerprint else { throw MessageCompositionError.attachmentChanged }
        let bytes = try Self.boundedRead(sourceURL)
        guard try FileFingerprint.read(sourceURL) == fingerprint,
              UInt64(bytes.count) == ref.sizeBytes,
              SHA256.hash(data: bytes).map({ String(format: "%02x", $0) }).joined() == ref.sha256 else { throw MessageCompositionError.attachmentChanged }
        return bytes
    }

    private static func boundedRead(_ url: URL) throws -> Data {
        let handle: FileHandle
        do { handle = try FileHandle(forReadingFrom: url) } catch CocoaError.fileReadNoSuchFile { throw MessageCompositionError.attachmentMissing } catch { throw error }
        defer { try? handle.close() }
        let data = try handle.read(upToCount: Int(MessageInputLimits.attachmentBytes) + 1) ?? Data()
        guard data.count <= MessageInputLimits.attachmentBytes else { throw MessageCompositionError.attachmentTooLarge }
        return data
    }
    private static func validAttachmentID(_ value: String) -> Bool { !value.isEmpty && value.utf8.count <= 128 && value.range(of: "^[A-Za-z0-9][A-Za-z0-9._:-]*$", options: .regularExpression) != nil }
    private static func validFilename(_ value: String) -> Bool { !value.isEmpty && value.utf8.count <= 255 && value != "." && value != ".." && !value.contains("/") && !value.contains("\\") && !value.unicodeScalars.contains(where: { $0.value < 0x20 || $0.value == 0x7f }) }
    private static func validMediaType(_ value: String) -> Bool { !value.isEmpty && value.utf8.count <= 127 && value == value.lowercased() && value.contains("/") && !value.contains(";") && !value.contains(" ") }
}

private struct FileFingerprint: Equatable, Sendable {
    let size, fileNumber: UInt64
    let modificationDate: Date
    static func read(_ url: URL) throws -> FileFingerprint {
        let attributes: [FileAttributeKey: Any]
        do { attributes = try FileManager.default.attributesOfItem(atPath: url.path) } catch CocoaError.fileReadNoSuchFile { throw MessageCompositionError.attachmentMissing } catch { throw error }
        guard attributes[.type] as? FileAttributeType == .typeRegular,
              let size = attributes[.size] as? NSNumber,
              let fileNumber = attributes[.systemFileNumber] as? NSNumber,
              let modificationDate = attributes[.modificationDate] as? Date else { throw MessageCompositionError.invalidAttachment }
        return FileFingerprint(size: size.uint64Value, fileNumber: fileNumber.uint64Value, modificationDate: modificationDate)
    }
}

public enum MessageCompositionError: Error, Equatable {
    case attachmentMissing, attachmentChanged, attachmentTooLarge, invalidAttachment, invalidFileMention, invalidMessage
}

public protocol MessageTransport: Sendable {
    func upload(threadID: String, attachment: AttachmentRef, bytes: Data) async throws
    func submit(threadID: String, envelope: MutationEnvelope, input: MessageInput) async throws
}

public struct HTTPMessageTransport: MessageTransport {
    public let configuration:HTTPClientConfiguration;public var baseURL:URL{configuration.baseURL}
    public init(baseURL:URL,bearerToken:String?=nil){configuration=HTTPClientConfiguration(baseURL:baseURL,bearerToken:bearerToken)};public init(configuration:HTTPClientConfiguration){self.configuration=configuration}
    public func upload(threadID: String, attachment: AttachmentRef, bytes: Data) async throws {
        let request = try Self.uploadRequest(configuration:configuration, threadID: threadID, attachment: attachment, bytes: bytes)
        let (data, response) = try await URLSession.shared.data(for: request)
        try Self.require(response, status: 201)
        guard try Self.decodeAttachment(data) == attachment else { throw ProjectionError.inconsistent("uploaded attachment identity changed") }
    }
    public func submit(threadID: String, envelope: MutationEnvelope, input: MessageInput) async throws {
        let request = try Self.submitRequest(configuration:configuration, threadID: threadID, envelope: envelope, input: input)
        let (_, response) = try await URLSession.shared.data(for: request); try Self.require(response, status: 202)
    }
    static func uploadRequest(baseURL: URL, threadID: String, attachment: AttachmentRef, bytes: Data) throws -> URLRequest {
        try uploadRequest(configuration:HTTPClientConfiguration(baseURL:baseURL),threadID:threadID,attachment:attachment,bytes:bytes)
    }
    static func uploadRequest(configuration:HTTPClientConfiguration, threadID: String, attachment: AttachmentRef, bytes: Data) throws -> URLRequest {
        var request = configuration.request(url: percentEncodedURL(baseURL: configuration.baseURL, segments: ["v1", "threads", threadID, "attachments"]))
        request.httpMethod = "POST"; request.httpBody = bytes; request.setValue("application/octet-stream", forHTTPHeaderField: "Content-Type")
        request.setValue(String(decoding: try JSONEncoder().encode(attachment), as: UTF8.self), forHTTPHeaderField: "X-Ago-Attachment-Ref")
        return request
    }
    static func submitRequest(baseURL: URL, threadID: String, envelope: MutationEnvelope, input: MessageInput) throws -> URLRequest {
        try submitRequest(configuration:HTTPClientConfiguration(baseURL:baseURL),threadID:threadID,envelope:envelope,input:input)
    }
    static func submitRequest(configuration:HTTPClientConfiguration, threadID: String, envelope: MutationEnvelope, input: MessageInput) throws -> URLRequest {
        struct Body: Encodable { let commandID, idempotencyKey, actorID: String; let expectedSequence: UInt64; let content: MessageInput; let `class`: String; enum CodingKeys: String, CodingKey { case commandID = "command_id", idempotencyKey = "idempotency_key", actorID = "actor_id", expectedSequence = "expected_sequence", content, `class` } }
        var request = configuration.request(url: percentEncodedURL(baseURL: configuration.baseURL, segments: ["v1", "threads", threadID, "messages"])); request.httpMethod = "POST"; request.setValue("application/json", forHTTPHeaderField: "Content-Type"); request.httpBody = try JSONEncoder().encode(Body(commandID: envelope.commandID, idempotencyKey: envelope.idempotencyKey, actorID: envelope.actorID, expectedSequence: envelope.expectedSequence, content: input, class: "normal")); return request
    }
    private static func decodeAttachment(_ data: Data) throws -> AttachmentRef { let raw = try JSONSerialization.jsonObject(with: data); guard let object = raw as? [String: Any], Set(object.keys) == ["attachment_id", "sha256", "size_bytes", "media_type", "filename"] else { throw ProjectionError.inconsistent("malformed attachment response") }; return try JSONDecoder().decode(AttachmentRef.self, from: data) }
    private static func require(_ response: URLResponse, status: Int) throws { guard let http = response as? HTTPURLResponse else { throw URLError(.badServerResponse) }; if http.statusCode == 409 { throw MutationError.conflict }; guard http.statusCode == status else { throw MutationError.httpStatus(http.statusCode) } }
}

@MainActor public final class MessageClient {
    private let cursor: ProjectionCursor, transport: any MessageTransport, projectionTransport: any ProjectionTransport, actorID: String
    public init(cursor: ProjectionCursor, transport: any MessageTransport, projectionTransport: any ProjectionTransport, actorID: String) { self.cursor = cursor; self.transport = transport; self.projectionTransport = projectionTransport; self.actorID = actorID }
    public func submit(text: String, attachments: [ImmutableAttachment], fileMentions: [RepositoryFileMention]) async throws {
        let input = try MessageInput(text: text, attachments: attachments.map(\.ref), fileMentions: fileMentions)
        for attachment in attachments { try await transport.upload(threadID: cursor.threadID, attachment: attachment.ref, bytes: attachment.verifiedBytes()) }
        let key = UUID().uuidString
        try await transport.submit(threadID: cursor.threadID, envelope: MutationEnvelope(commandID: key, idempotencyKey: key, actorID: actorID, expectedSequence: cursor.sequence), input: input)
        try await ReconnectController(cursor: cursor, transport: projectionTransport).refresh()
    }
}
