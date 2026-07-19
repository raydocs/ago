import Foundation
import Testing
@testable import AgoClientCore

@MainActor @Suite struct ReconnectControllerTests {
    @Test func opaqueJSONAndSchema() throws {
        let page = try ProjectionDecoder.decode(Data(Self.json(events: [(1, "future.event", "{\"nested\":[1,true]}")]).utf8))
        #expect(page.events[0].type == "future.event")
        #expect(page.events[0].payload.objectValue?["nested"]?.arrayValue?.count == 2)
        #expect(page.plugins.registrations[0].tools[0].inputSchema.objectValue?["type"]?.stringValue == "object")
        #expect(page.diff.snapshot == nil && page.diff.comments.isEmpty)
        #expect(throws: ProjectionError.self) { try ProjectionDecoder.decode(Data(Self.json(schema: 2).utf8)) }
    }

    @Test func decodesTypedDiffHunksAndComments() throws {
        let snapshot = """
        {"thread_id":"thread-1","environment_id":"env","executor_generation":2,"repository_id":"repo","worktree_id":"wt","revision":4,"digest":"digest","head_oid":"head","index_digest":"index","projection":{"schema_version":1,"repository_id":"repo","worktree_id":"wt","digest":"digest","head_oid":"head","index_digest":"index","serialized_index":{"exists":true,"digest":"sum","size":12},"staged":[],"unstaged":[{"id":"file","path":"Sources/A.swift","content_digest":"content","status":"modified","binary":false,"protected":false,"mutation_supported":true,"hunks":[{"id":"hunk","header":"@@ -1 +1 @@","patch":"@@ -1 +1 @@\\n-old\\n+new\\n","old_start":1,"old_lines":1,"new_start":1,"new_lines":1,"occurrence":1}]}]},"created_sequence":3,"created_at":"2026-07-19T12:00:00Z"}
        """
        let comment = """
        {"thread_id":"thread-1","comment_id":"comment","snapshot_generation":2,"snapshot_revision":4,"snapshot_digest":"digest","file_id":"file","hunk_id":"hunk","actor":"me","body":"Please change this","created_sequence":4,"created_at":"2026-07-19T12:01:00Z"}
        """
        let raw = Self.json().replacingOccurrences(of: "\"snapshot\":null,\"comments\":[]", with: "\"snapshot\":\(snapshot),\"comments\":[\(comment)]")
        let page = try ProjectionDecoder.decode(Data(raw.utf8))
        #expect(page.diff.snapshot?.projection.unstaged[0].hunks[0].patch.contains("-old") == true)
        #expect(page.diff.comments[0].hunkID == "hunk")
    }

    @Test func projectsOnlyAuthoritativeTimelineAndPanelEvents() throws {
        let events: [AgoEvent] = [
            try Self.event(1, "assistant.completed", "{\"turn_id\":\"turn\",\"event\":{\"message\":{\"content\":[{\"type\":\"thinking\",\"thinking\":\"checked\"},{\"type\":\"text\",\"text\":\"done\"}]}}}"),
            try Self.event(2, "tool.requested", "{\"event\":{\"callId\":\"call\",\"name\":\"read\",\"input\":{\"path\":\"README.md\"}}}"),
            try Self.event(3, "tool.completed", "{\"call_id\":\"call\",\"name\":\"read\",\"output\":\"contents\",\"error\":false}"),
            try Self.event(4, "assistant.completed", "{\"usage\":{\"input\":99},\"title\":\"ignore\",\"body\":\"ignore\"}"),
            try Self.event(5, "provider.usage-recorded", "{\"record_id\":\"U-1\",\"created_sequence\":5,\"thread_id\":\"thread-1\",\"idempotency_key\":\"usage-1\",\"provider\":\"provider\",\"model\":\"model\",\"request_id\":\"request\",\"status\":\"final\",\"usage\":{\"input_tokens\":11,\"output_tokens\":7,\"cache_read_tokens\":3,\"cache_write_tokens\":4,\"total_tokens\":25},\"cost\":{\"input\":0.1,\"output\":0.2,\"cache_read\":0.01,\"cache_write\":0.02,\"total\":0.33}}"),
            try Self.event(6, "verification.check-recorded", "{\"record_id\":\"V-1\",\"created_sequence\":6,\"thread_id\":\"thread-1\",\"idempotency_key\":\"verify-1\",\"check_id\":\"tests\",\"command\":\"swift test\",\"status\":\"passed\",\"output_summary\":\"12 tests passed\"}"),
            try Self.event(7, "plugin.dialog.requested", "{\"dialog_id\":\"D-1\",\"thread_id\":\"thread-1\",\"turn_id\":\"turn\",\"plugin_id\":\"plugin\",\"generation\":1,\"invocation_id\":\"invoke\",\"deadline\":\"2026-07-19T12:00:00Z\",\"request_type\":\"notify\",\"request\":{\"kind\":\"notify\",\"title\":\"Input needed\",\"message\":\"Confirm\"},\"state\":\"pending\",\"revision\":1,\"requested_sequence\":7}")
        ]
        #expect(EventProjection.timeline(events).map(\.kind) == [.thinking, .text, .toolCall, .toolResult])
        #expect(EventProjection.providerUsage(events)?.usage.inputTokens == 11)
        #expect(EventProjection.verification(events)?.command == "swift test")
        #expect(EventProjection.notification(events[3]) == nil)
        #expect(EventProjection.notification(events[6])?.title == "Input needed")
    }

    @Test func strictCatalogDecodingRejectsUnknownFields() throws {
        let raw = Data("{\"schema_version\":1,\"threads\":[{\"thread_id\":\"t\",\"project_id\":\"p\",\"title\":\"Thread\",\"workspace\":\"/x\",\"last_sequence\":2,\"activity\":\"running\",\"created_at\":\"a\",\"updated_at\":\"b\",\"archived\":false,\"archived_at\":\"\"}]}".utf8)
        let page = try ThreadCatalogDecoder.decode(raw)
        #expect(page.threads[0].activity == .running)
        let invalid = Data(String(decoding: raw, as: UTF8.self).replacingOccurrences(of: "{\"schema_version\"", with: "{\"unknown\":true,\"schema_version\"").utf8)
        #expect(throws: ProjectionError.self) { try ThreadCatalogDecoder.decode(invalid) }
    }

    @Test func rejectsUnknownTopLevelField() {
        let raw = Self.json().replacingOccurrences(of: "{\"schema_version\"", with: "{\"surprise\":true,\"schema_version\"")
        #expect(throws: ProjectionError.self) { try ProjectionDecoder.decode(Data(raw.utf8)) }
    }

    @Test func decodesRFC3339NanoDeadline() throws {
        let raw = Self.json().replacingOccurrences(of: "2026-07-19T12:00:00Z", with: "2026-07-19T12:00:00.123456789Z")
        let page = try ProjectionDecoder.decode(Data(raw.utf8))
        let baseline = ISO8601DateFormatter().date(from: "2026-07-19T12:00:00Z")!
        #expect(abs(page.dialogs[0].deadline.timeIntervalSince(baseline) - 0.123456789) < 0.000_001)
    }

    @Test func paginationAndSameThreadSequence() async throws {
        let transport = FauxTransport([Self.json(next: 1, snapshot: 2, more: true, events: [(1,"x","{}")]), Self.json(after: 1, next: 2, snapshot: 2, events: [(2,"y","{}")])])
        let cursor = ProjectionCursor(threadID: "thread-1")
        try await ReconnectController(cursor: cursor, transport: transport, pageLimit: 1).refresh()
        #expect(cursor.events.map(\.sequence) == [1,2])
        #expect(await transport.requests.map(\.after) == [0,1])
    }

    @Test func reconnectDeduplicatesAndReplacesState() async throws {
        let cursor = ProjectionCursor(threadID: "thread-1")
        try await ReconnectController(cursor: cursor, transport: FauxTransport([Self.json(next: 1, snapshot: 1, queue: "old", revision: 1, events: [(1,"x","{}")])])).refresh()
        try await ReconnectController(cursor: cursor, transport: FauxTransport([Self.json(after: 1, next: 2, snapshot: 2, queue: "new", revision: 2, events: [(2,"y","{}")])])).refresh()
        #expect(cursor.events.map(\.sequence) == [1,2]); #expect(cursor.mailbox?.queue[0].queueItemID == "new"); #expect(cursor.dialogs[0].revision == 2)
        _ = ReconnectController(cursor: cursor, transport: FauxTransport([]))
        #expect(cursor.sequence == 2)
    }

    @Test func rejectsInconsistentPagesWithoutMutation() async {
        for raw in [Self.json(threadID:"other"), Self.json(after:1), Self.json(next:1,snapshot:1,events:[(2,"x","{}")]), Self.json(next:2,snapshot:1,events:[(2,"x","{}")])] {
            let cursor = ProjectionCursor(threadID: "thread-1")
            await #expect(throws: ProjectionError.self) { try await ReconnectController(cursor: cursor, transport: FauxTransport([raw])).refresh() }
            #expect(cursor.sequence == 0)
        }
    }

    static func json(schema:Int=1, threadID:String="thread-1", after:UInt64=0, next:UInt64=0, snapshot:UInt64=0, more:Bool=false, queue:String="q", revision:UInt64=1, events:[(UInt64,String,String)]=[]) -> String {
        let es=events.map{"{\"schema_version\":1,\"event_id\":\"e-\($0.0)\",\"thread_id\":\"\(threadID)\",\"sequence\":\($0.0),\"type\":\"\($0.1)\",\"visibility\":\"user\",\"payload\":\($0.2)}"}.joined(separator:",")
        return """
        {"schema_version":\(schema),"thread":{"thread_id":"\(threadID)","last_sequence":\(snapshot),"title":"Demo","workspace":"/tmp","mode":"high","executor":{"type":"local"},"project":{"project_id":"p"},"agent":{"definition_id":"a","version":"1","display_name":"Agent","default_mode":"high"},"provenance":{}},"mailbox":{"thread_id":"\(threadID)","last_sequence":\(snapshot),"activity":"idle","cancel_requested":false,"queue":[{"queue_item_id":"\(queue)","position":1,"class":"normal","state":"pending","content":{"text":"queued"}}]},"events":[\(es)],"dialogs":[{"dialog_id":"d","thread_id":"\(threadID)","turn_id":"t","plugin_id":"plug","generation":3,"invocation_id":"i","deadline":"2026-07-19T12:00:00Z","request_type":"confirm","request":{"title":"Continue?"},"state":"pending","revision":\(revision),"requested_sequence":1}],"diff":{"snapshot":null,"comments":[]},"requested_after_sequence":\(after),"next_after_sequence":\(next),"snapshot_sequence":\(snapshot),"has_more":\(more),"plugins":{"available":true,"generation":3,"registrations":[{"pluginId":"plug","tools":[{"name":"search","description":"Search","inputSchema":{"type":"object"}}],"commands":[{"id":"run","title":"Run"}],"hooks":[]}]},"executor":{"target":{"type":"local"},"activity":"idle"}}
        """
    }

    static func event(_ sequence: UInt64, _ type: String, _ payload: String) throws -> AgoEvent {
        try JSONDecoder().decode(AgoEvent.self, from: Data("{\"schema_version\":1,\"event_id\":\"e-\(sequence)\",\"thread_id\":\"thread-1\",\"sequence\":\(sequence),\"type\":\"\(type)\",\"visibility\":\"user\",\"payload\":\(payload)}".utf8))
    }
}
actor FauxTransport: ProjectionTransport {
    var pages:[String]; private(set) var requests:[ProjectionRequest]=[]
    init(_ pages:[String]) { self.pages=pages }
    func fetchProjection(_ request:ProjectionRequest) async throws -> Data { requests.append(request); guard !pages.isEmpty else { throw URLError(.resourceUnavailable) }; return Data(pages.removeFirst().utf8) }
}
