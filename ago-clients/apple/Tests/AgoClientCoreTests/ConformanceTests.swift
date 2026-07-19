import Foundation
import Testing
@testable import AgoClientCore

@MainActor @Suite struct ConformanceTests {
    @Test func parsesOnlyCredentialFreeProjectionEndpoint()throws{
        let endpoint=try ConformanceEndpoint.parse("https://daemon.test/api/v1/threads/thread%2Fone/projection")
        #expect(endpoint.baseURL.absoluteString=="https://daemon.test/api");#expect(endpoint.threadID=="thread/one")
        for value in ["file:///v1/threads/t/projection","https://user:secret@daemon.test/v1/threads/t/projection","https://daemon.test/v1/threads/t/projection?after=2","https://daemon.test/v1/threads/t/messages"]{#expect(throws:ConformanceError.invalidProjectionURL){try ConformanceEndpoint.parse(value)}}
    }
    @Test func productionReconnectParserEmitsStableWebFieldsAndDigests()async throws{
        let page=ReconnectControllerTests.json(next:2,snapshot:2,queue:"queue-1",events:[(1,"message.accepted","{\"content\":{\"text\":\"hello\"}}"),(2,"provider.usage-recorded","{\"input_tokens\":4}")])
        let output=try await ProjectionConformance.run(projectionURL:"https://daemon.test/v1/threads/thread-1/projection",transport:ConformanceFixtureTransport(page:Data(page.utf8)))
        let value=try JSONDecoder().decode(JSONValue.self,from:Data(output.utf8));let object=value.objectValue!
        #expect(Set(object.keys)==["digest","snapshot_sequence","mailbox","queue","dialogs","diff","events"])
        #expect(object["snapshot_sequence"] == JSONValue.number(2));#expect(object["queue"]?.objectValue?["count"] == JSONValue.number(1));#expect(object["dialogs"]?.objectValue?["count"] == JSONValue.number(1));#expect(object["diff"]?.objectValue?["has_snapshot"] == JSONValue.bool(false));#expect(object["events"]?.objectValue?["first_sequence"] == JSONValue.number(1));#expect(object["events"]?.objectValue?["last_sequence"] == JSONValue.number(2))
        for key in ["digest"]{#expect(object[key]?.stringValue?.range(of:"^[a-f0-9]{64}$",options:.regularExpression) != nil)}
        let repeated=try await ProjectionConformance.run(projectionURL:"https://daemon.test/v1/threads/thread-1/projection",transport:ConformanceFixtureTransport(page:Data(page.utf8)))
        #expect(output==repeated)
    }
}
private actor ConformanceFixtureTransport:ProjectionTransport{let page:Data;init(page:Data){self.page=page};func fetchProjection(_ request:ProjectionRequest)async throws->Data{#expect(request.after==0);#expect(request.limit==200);return page}}
