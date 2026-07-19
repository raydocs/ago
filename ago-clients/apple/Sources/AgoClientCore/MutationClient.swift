import Foundation
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

public struct MutationEnvelope: Equatable, Sendable {
    public let commandID,idempotencyKey,actorID:String; public let expectedSequence:UInt64
    public init(commandID:String,idempotencyKey:String,actorID:String,expectedSequence:UInt64){self.commandID=commandID;self.idempotencyKey=idempotencyKey;self.actorID=actorID;self.expectedSequence=expectedSequence}
}
public enum GitMutationKind:String, Equatable, Sendable { case stage, unstage }
public enum MutationRequest: Equatable, Sendable {
    case queueEdit(threadID:String,queueItemID:String,envelope:MutationEnvelope,content:JSONValue)
    case dequeue(threadID:String,queueItemID:String,envelope:MutationEnvelope)
    case steer(threadID:String,queueItemID:String,envelope:MutationEnvelope,expectedTurnID:String)
    case interrupt(threadID:String,turnID:String,envelope:MutationEnvelope,content:JSONValue)
    case git(threadID:String,kind:GitMutationKind,envelope:MutationEnvelope,snapshotRevision:UInt64,snapshotDigest:String,unitIDs:[String])
    case revert(threadID:String,envelope:MutationEnvelope,snapshotRevision:UInt64,snapshotDigest:String,receiptID:String)
    case archive(threadID:String,envelope:MutationEnvelope)
    case diffComment(threadID:String,commentID:String,actorID:String,expectedSequence:UInt64,snapshotRevision:UInt64,snapshotDigest:String,fileID:String,hunkID:String?,body:String)
    case resolveDialog(threadID:String,dialogID:String,resolverID:String,expectedRevision:UInt64,expectedSequence:UInt64,response:UIResult)
}
public enum MutationError: Error, Equatable { case conflict; case httpStatus(Int) }
public protocol MutationTransport:Sendable { func send(_ request:MutationRequest) async throws }

public struct HTTPMutationTransport:MutationTransport {
    public let configuration:HTTPClientConfiguration;public var baseURL:URL{configuration.baseURL}
    public init(baseURL:URL,bearerToken:String?=nil){configuration=HTTPClientConfiguration(baseURL:baseURL,bearerToken:bearerToken)};public init(configuration:HTTPClientConfiguration){self.configuration=configuration}
    public func send(_ mutation:MutationRequest) async throws { let request=try Self.makeURLRequest(configuration:configuration,mutation:mutation); let (_,response)=try await URLSession.shared.data(for:request); guard let http=response as? HTTPURLResponse else {throw URLError(.badServerResponse)}; if http.statusCode==409 {throw MutationError.conflict}; guard (200..<300).contains(http.statusCode) else {throw MutationError.httpStatus(http.statusCode)} }
    static func makeURLRequest(baseURL:URL,mutation:MutationRequest)throws->URLRequest{try makeURLRequest(configuration:HTTPClientConfiguration(baseURL:baseURL),mutation:mutation)}
    static func makeURLRequest(configuration:HTTPClientConfiguration,mutation:MutationRequest) throws -> URLRequest {
        var method:String; var segments:[String]; var body:[String:JSONValue]
        func envelope(_ value:MutationEnvelope)->[String:JSONValue] {["command_id":.string(value.commandID),"idempotency_key":.string(value.idempotencyKey),"actor_id":.string(value.actorID),"expected_sequence":.number(Double(value.expectedSequence))]}
        switch mutation {
        case let .queueEdit(thread,item,e,content): method="PATCH"; segments=["v1","threads",thread,"queue",item]; body=envelope(e); body["content"]=content
        case let .dequeue(thread,item,e): method="DELETE"; segments=["v1","threads",thread,"queue",item]; body=envelope(e)
        case let .steer(thread,item,e,turn): method="POST"; segments=["v1","threads",thread,"queue",item,"steer"]; body=envelope(e); body["expected_turn_id"] = .string(turn)
        case let .interrupt(thread,turn,e,content): method="POST"; segments=["v1","threads",thread,"turns",turn,"interrupt"]; body=envelope(e); body["content"]=content
        case let .git(thread,kind,e,revision,digest,unitIDs): method="POST"; segments=["v1","threads",thread,"diff",kind.rawValue]; body=envelope(e); body["expected_snapshot_revision"] = .number(Double(revision)); body["expected_snapshot_digest"] = .string(digest); body["selected_unit_ids"] = .array(unitIDs.map(JSONValue.string))
        case let .revert(thread,e,revision,digest,receiptID): method="POST"; segments=["v1","threads",thread,"diff","revert"]; body=envelope(e); body["expected_snapshot_revision"] = .number(Double(revision)); body["expected_snapshot_digest"] = .string(digest); body["receipt_id"] = .string(receiptID)
        case let .archive(thread,e): method="POST"; segments=["v1","threads",thread,"archive"]; body=envelope(e)
        case let .diffComment(thread,comment,actor,sequence,revision,digest,file,hunk,text): method="POST"; segments=["v1","threads",thread,"diff","comments"]; body=["comment_id":.string(comment),"actor_id":.string(actor),"expected_sequence":.number(Double(sequence)),"snapshot_revision":.number(Double(revision)),"snapshot_digest":.string(digest),"file_id":.string(file),"body":.string(text)]; if let hunk{body["hunk_id"] = .string(hunk)}
        case let .resolveDialog(thread,dialog,resolver,revision,sequence,response): method="POST"; segments=["v1","threads",thread,"dialogs",dialog,"resolve"]; body=["resolver_id":.string(resolver),"expected_revision":.number(Double(revision)),"expected_sequence":.number(Double(sequence)),"response":response.jsonValue]
        }
        var request=configuration.request(url:percentEncodedURL(baseURL:configuration.baseURL,segments:segments)); request.httpMethod=method; request.setValue("application/json",forHTTPHeaderField:"Content-Type"); request.httpBody=try JSONEncoder().encode(body); return request
    }
}

@MainActor public final class MutationClient {
    private let cursor:ProjectionCursor; private let mutationTransport:any MutationTransport; private let projectionTransport:any ProjectionTransport; private let actorID:String
    public init(cursor:ProjectionCursor,mutationTransport:any MutationTransport,projectionTransport:any ProjectionTransport,actorID:String){self.cursor=cursor;self.mutationTransport=mutationTransport;self.projectionTransport=projectionTransport;self.actorID=actorID}
    private func envelope()->MutationEnvelope { let key=UUID().uuidString; return MutationEnvelope(commandID:key,idempotencyKey:key,actorID:actorID,expectedSequence:cursor.sequence) }
    private func gitEnvelope()->MutationEnvelope { let key="git:\(UUID().uuidString)"; return MutationEnvelope(commandID:key,idempotencyKey:key,actorID:actorID,expectedSequence:cursor.sequence) }
    private func perform(_ request:MutationRequest) async throws { try await mutationTransport.send(request); try await ReconnectController(cursor:cursor,transport:projectionTransport).refresh() }
    public func edit(queueItemID:String,content:JSONValue) async throws {try await perform(.queueEdit(threadID:cursor.threadID,queueItemID:queueItemID,envelope:envelope(),content:content))}
    public func dequeue(queueItemID:String) async throws {try await perform(.dequeue(threadID:cursor.threadID,queueItemID:queueItemID,envelope:envelope()))}
    public func steer(queueItemID:String) async throws {guard let turn=cursor.mailbox?.activeTurnID else {throw ProjectionError.inconsistent("no active turn")}; try await perform(.steer(threadID:cursor.threadID,queueItemID:queueItemID,envelope:envelope(),expectedTurnID:turn))}
    public func interrupt(content:JSONValue) async throws {guard let turn=cursor.mailbox?.activeTurnID else {throw ProjectionError.inconsistent("no active turn")}; try await perform(.interrupt(threadID:cursor.threadID,turnID:turn,envelope:envelope(),content:content))}
    public func stage(snapshotRevision:UInt64,snapshotDigest:String,unitIDs:[String]) async throws {try await perform(.git(threadID:cursor.threadID,kind:.stage,envelope:gitEnvelope(),snapshotRevision:snapshotRevision,snapshotDigest:snapshotDigest,unitIDs:unitIDs))}
    public func unstage(snapshotRevision:UInt64,snapshotDigest:String,unitIDs:[String]) async throws {try await perform(.git(threadID:cursor.threadID,kind:.unstage,envelope:gitEnvelope(),snapshotRevision:snapshotRevision,snapshotDigest:snapshotDigest,unitIDs:unitIDs))}
    public func revert(snapshotRevision:UInt64,snapshotDigest:String,receiptID:String) async throws {try await perform(.revert(threadID:cursor.threadID,envelope:gitEnvelope(),snapshotRevision:snapshotRevision,snapshotDigest:snapshotDigest,receiptID:receiptID))}
    public func archive() async throws {try await perform(.archive(threadID:cursor.threadID,envelope:envelope()))}
    public func requestChange(snapshotRevision:UInt64,snapshotDigest:String,fileID:String,hunkID:String?,body:String) async throws {try await perform(.diffComment(threadID:cursor.threadID,commentID:UUID().uuidString,actorID:actorID,expectedSequence:cursor.sequence,snapshotRevision:snapshotRevision,snapshotDigest:snapshotDigest,fileID:fileID,hunkID:hunkID,body:body))}
    public func resolve(_ dialog:PluginDialog,response:UIResult) async throws {try await perform(.resolveDialog(threadID:cursor.threadID,dialogID:dialog.dialogID,resolverID:actorID,expectedRevision:dialog.revision,expectedSequence:cursor.sequence,response:response))}
}
