import Foundation
import Testing
@testable import AgoClientCore

@MainActor @Suite struct MutationClientTests {
    @Test func exactRoutesBodiesAndEncoding() throws {
        let base = URL(string: "https://example.test/api/")!
        let envelope = MutationEnvelope(commandID: "cmd", idempotencyKey: "idem", actorID: "actor", expectedSequence: 7)
        let cases: [(MutationRequest, String, String, [String: JSONValue])] = [
            (.queueEdit(threadID: "a/b ?", queueItemID: "q/1", envelope: envelope, content: .string("new")), "PATCH", "/api/v1/threads/a%2Fb%20%3F/queue/q%2F1", ["command_id":.string("cmd"),"idempotency_key":.string("idem"),"actor_id":.string("actor"),"expected_sequence":.number(7),"content":.string("new")]),
            (.dequeue(threadID: "a/b ?", queueItemID: "q/1", envelope: envelope), "DELETE", "/api/v1/threads/a%2Fb%20%3F/queue/q%2F1", ["command_id":.string("cmd"),"idempotency_key":.string("idem"),"actor_id":.string("actor"),"expected_sequence":.number(7)]),
            (.steer(threadID: "a/b ?", queueItemID: "q/1", envelope: envelope, expectedTurnID: "turn"), "POST", "/api/v1/threads/a%2Fb%20%3F/queue/q%2F1/steer", ["command_id":.string("cmd"),"idempotency_key":.string("idem"),"actor_id":.string("actor"),"expected_sequence":.number(7),"expected_turn_id":.string("turn")]),
            (.interrupt(threadID: "a/b ?", turnID: "t/1", envelope: envelope, content: .string("stop")), "POST", "/api/v1/threads/a%2Fb%20%3F/turns/t%2F1/interrupt", ["command_id":.string("cmd"),"idempotency_key":.string("idem"),"actor_id":.string("actor"),"expected_sequence":.number(7),"content":.string("stop")]),
            (.git(threadID: "a/b ?", kind: .stage, envelope: envelope, snapshotRevision: 4, snapshotDigest: "digest", unitIDs: ["u1", "u2"]), "POST", "/api/v1/threads/a%2Fb%20%3F/diff/stage", ["command_id":.string("cmd"),"idempotency_key":.string("idem"),"actor_id":.string("actor"),"expected_sequence":.number(7),"expected_snapshot_revision":.number(4),"expected_snapshot_digest":.string("digest"),"selected_unit_ids":.array([.string("u1"),.string("u2")])]),
            (.revert(threadID: "a/b ?", envelope: envelope, snapshotRevision: 4, snapshotDigest: "digest", receiptID: "R-one"), "POST", "/api/v1/threads/a%2Fb%20%3F/diff/revert", ["command_id":.string("cmd"),"idempotency_key":.string("idem"),"actor_id":.string("actor"),"expected_sequence":.number(7),"expected_snapshot_revision":.number(4),"expected_snapshot_digest":.string("digest"),"receipt_id":.string("R-one")]),
            (.archive(threadID: "a/b ?", envelope: envelope), "POST", "/api/v1/threads/a%2Fb%20%3F/archive", ["command_id":.string("cmd"),"idempotency_key":.string("idem"),"actor_id":.string("actor"),"expected_sequence":.number(7)]),
            (.diffComment(threadID: "a/b ?", commentID: "comment", actorID: "r", expectedSequence: 7, snapshotRevision: 4, snapshotDigest: "digest", fileID: "file", hunkID: "hunk", body: "Please change this"), "POST", "/api/v1/threads/a%2Fb%20%3F/diff/comments", ["comment_id":.string("comment"),"actor_id":.string("r"),"expected_sequence":.number(7),"snapshot_revision":.number(4),"snapshot_digest":.string("digest"),"file_id":.string("file"),"hunk_id":.string("hunk"),"body":.string("Please change this")]),
            (.resolveDialog(threadID: "a/b ?", dialogID: "d/1", resolverID: "r", expectedRevision: 3, expectedSequence: 7, response: .ok(.bool(true))), "POST", "/api/v1/threads/a%2Fb%20%3F/dialogs/d%2F1/resolve", ["resolver_id":.string("r"),"expected_revision":.number(3),"expected_sequence":.number(7),"response":.object(["status":.string("ok"),"value":.bool(true)])]),
            (.resolveDialog(threadID: "a/b ?", dialogID: "d/1", resolverID: "r", expectedRevision: 3, expectedSequence: 7, response: .cancelled), "POST", "/api/v1/threads/a%2Fb%20%3F/dialogs/d%2F1/resolve", ["resolver_id":.string("r"),"expected_revision":.number(3),"expected_sequence":.number(7),"response":.object(["status":.string("cancelled")])])
        ]
        for (mutation, method, path, body) in cases {
            let request = try HTTPMutationTransport.makeURLRequest(baseURL: base, mutation: mutation)
            #expect(request.httpMethod == method); #expect(URLComponents(url:request.url!,resolvingAgainstBaseURL:false)?.percentEncodedPath == path)
            #expect(try JSONDecoder().decode([String:JSONValue].self, from: request.httpBody!) == body)
        }
    }

    @Test func conflictDoesNotRefreshOrMutateCursor() async {
        let cursor = ProjectionCursor(threadID: "thread-1")
        let mutations = ConflictMutationTransport()
        let projections = CountingProjectionTransport()
        let client = MutationClient(cursor: cursor, mutationTransport: mutations, projectionTransport: projections, actorID: "me")
        await #expect(throws: MutationError.conflict) { try await client.dequeue(queueItemID: "q") }
        #expect(cursor.sequence == 0)
        #expect(await projections.count == 0)
        let sent = await mutations.requests
        #expect(sent.count == 1)
        guard case .dequeue(_, _, let envelope) = sent[0] else { Issue.record("wrong mutation"); return }
        #expect(envelope.commandID == envelope.idempotencyKey)
        #expect(envelope.expectedSequence == 0)
    }

    @Test func catalogRequestUsesExplicitProjectScope() throws {
        let request = HTTPCatalogTransport.makeURLRequest(
            baseURL: URL(string: "https://example.test/api/")!,
            request: ThreadCatalogRequest(projectID: "project/one", search: "needle", archive: .active, limit: 100)
        )
        #expect(request.httpMethod == "GET")
        #expect(request.url?.absoluteString == "https://example.test/api/v1/threads?project_id=project%2Fone&search=needle&archive=active&limit=100")
    }

    @Test func bearerConfigurationAuthenticatesCatalogProjectionAndMutationRequests()throws{
        let configuration=HTTPClientConfiguration(baseURL:URL(string:"http://127.0.0.1:43210")!,bearerToken:"secret-token")
        let catalog=HTTPCatalogTransport.makeURLRequest(configuration:configuration,request:ThreadCatalogRequest(projectID:"p"))
        let projection=HTTPProjectionTransport.makeURLRequest(configuration:configuration,request:ProjectionRequest(threadID:"t",after:0,limit:200))
        let mutation=try HTTPMutationTransport.makeURLRequest(configuration:configuration,mutation:.archive(threadID:"t",envelope:MutationEnvelope(commandID:"c",idempotencyKey:"i",actorID:"a",expectedSequence:1)))
        for request in [catalog,projection,mutation]{#expect(request.value(forHTTPHeaderField:"Authorization")=="Bearer secret-token")}
        #expect(HTTPCatalogTransport.makeURLRequest(baseURL:configuration.baseURL,request:ThreadCatalogRequest(projectID:"p")).value(forHTTPHeaderField:"Authorization")==nil)
    }

    @Test func queueJSONEditorPreservesEveryJSONType() throws {
        let values:[JSONValue]=[.object(["text":.string("hello")]),.array([.number(1),.bool(true)]),.number(42),.bool(false),.null,.string("plain string")]
        for value in values { #expect(try JSONValue.parse(value.formatted)==value) }
        #expect(throws:Error.self){try JSONValue.parse("{not json}")}
    }
}

actor ConflictMutationTransport: MutationTransport {
    private(set) var requests:[MutationRequest] = []
    func send(_ request:MutationRequest) async throws { requests.append(request); throw MutationError.conflict }
}
actor CountingProjectionTransport: ProjectionTransport {
    private(set) var count=0
    func fetchProjection(_ request:ProjectionRequest) async throws -> Data { count += 1; throw URLError(.badServerResponse) }
}
