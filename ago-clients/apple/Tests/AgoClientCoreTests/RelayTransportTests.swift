import Foundation
import Testing
@testable import AgoClientCore

@Suite struct RelayTransportTests {
    @Test func projectionUsesExactEnqueueAndPendingResultContract()async throws {
        let server=RelayServer(payload:try fixtureProjection(),pendingOnce:true)
        let configuration=try RelayConfiguration(relayURL:URL(string:"https://relay.example")!,projectID:"project-1",bearerToken:"secret",maximumPollAttempts:2,minimumPollDelay:.zero,execute:{try await server.execute($0)})
        let data=try await RelayProjectionTransport(configuration:configuration).fetchProjection(ProjectionRequest(threadID:"thread-1",after:4,limit:200))
        #expect(try JSONSerialization.jsonObject(with:data) as? [String:Any] != nil)
        let requests=await server.requests
        #expect(requests.count == 3)
        #expect(requests.allSatisfy{$0.value(forHTTPHeaderField:"Authorization")=="Bearer secret"})
        let body=try #require(requests[0].httpBody)
        let object=try #require(try JSONSerialization.jsonObject(with:body) as? [String:Any])
        #expect(Set(object.keys)==["nonce","project_id","thread_id","action","payload"])
        #expect(object["project_id"] as? String=="project-1")
        #expect(object["thread_id"] as? String=="thread-1")
        #expect(object["action"] as? String=="thread.projection")
        let payload=try #require(object["payload"] as? [String:Any])
        #expect(Set(payload.keys)==["after_sequence","limit"])
        #expect(payload["after_sequence"] as? Int==4)
        #expect(payload["limit"] as? Int==200)
        #expect(requests[1].url?.absoluteString.contains("/v1/relay/results?sequence=7")==true)
    }

    @Test func submitAndArchiveUseOnlyPublishedActions()async throws {
        let server=RelayServer(payload:["accepted":true])
        let configuration=try RelayConfiguration(relayURL:URL(string:"https://relay.example")!,projectID:"project",bearerToken:"runtime",minimumPollDelay:.zero,execute:{try await server.execute($0)})
        let input=try MessageInput(text:"Review")
        try await RelayMessageTransport(configuration:configuration).submit(threadID:"thread",envelope:MutationEnvelope(commandID:"ignored",idempotencyKey:"ignored",actorID:"ignored",expectedSequence:8),input:input)
        try await RelayArchiveTransport(configuration:configuration).send(.archive(threadID:"thread",envelope:MutationEnvelope(commandID:"ignored",idempotencyKey:"ignored",actorID:"ignored",expectedSequence:9)))
        let bodies=try await server.enqueueBodyData().map{try JSONSerialization.jsonObject(with:$0) as! [String:Any]}
        #expect(bodies.map{$0["action"] as? String} == ["thread.submit","thread.archive"])
        #expect((bodies[0]["payload"] as? [String:Any])?["expected_sequence"] as? Int==8)
        #expect((bodies[1]["payload"] as? [String:Any])?["expected_sequence"] as? Int==9)
        await #expect(throws:RelayError.unsupported){try await RelayArchiveTransport(configuration:configuration).send(.dequeue(threadID:"thread",queueItemID:"q",envelope:MutationEnvelope(commandID:"x",idempotencyKey:"x",actorID:"x",expectedSequence:9)))}
    }

    @Test func rejectsChangedNonceAndRedactsAuthorizationFailure()async throws {
        let changed=RelayServer(payload:["ok":true],changedNonce:true)
        let first=try RelayConfiguration(relayURL:URL(string:"https://relay.example")!,projectID:"p",bearerToken:"never-print",minimumPollDelay:.zero,execute:{try await changed.execute($0)})
        await #expect(throws:RelayError.protocolError){try await RelayClient(configuration:first).archive(threadID:"t",expectedSequence:1)}
        let denied=RelayServer(payload:[:],status:401)
        let second=try RelayConfiguration(relayURL:URL(string:"https://relay.example")!,projectID:"p",bearerToken:"never-print",minimumPollDelay:.zero,execute:{try await denied.execute($0)})
        await #expect(throws:RelayError.authorizationFailed){try await RelayClient(configuration:second).archive(threadID:"t",expectedSequence:1)}
    }

    @Test func passkeyChallengeAssertionAndSingleMutationGrantUseExactDTOs()async throws {
        let server=AuthRelayServer()
        let base=try RelayConfiguration(relayURL:URL(string:"https://relay.example")!,projectID:"project",bearerToken:"session",minimumPollDelay:.zero,execute:{try await server.execute($0)})
        let client=RelayClient(configuration:base),challenge=try await client.authenticationChallenge(threadID:"thread",rpID:"example.com")
        #expect(challenge == RelayAuthenticationChallenge(challenge:"AQID",rpID:"example.com",expiresAt:"2026-07-19T07:00:00Z"))
        #expect(challenge.challengeData == Data([1,2,3]))
        let assertion=RelayAuthenticationAssertion(credentialID:Data("credential".utf8).base64URLEncodedString(),rpID:"example.com",clientDataJSON:Data("client".utf8).base64URLEncodedString(),authenticatorData:Data("authenticator".utf8).base64URLEncodedString(),signature:Data("signature".utf8).base64URLEncodedString())
        let grant=try await client.authenticationAssertion(threadID:"thread",assertion:assertion)
        #expect(grant.authorizationToken=="g.one-use-grant")
        let authorized=try RelayConfiguration(relayURL:base.relayURL,projectID:base.projectID,bearerToken:base.bearerToken,minimumPollDelay:.zero,authorization:{_,_,_ in grant.authorizationToken},execute:{try await server.execute($0)})
        try await RelayClient(configuration:authorized).submit(threadID:"thread",expectedSequence:4,input:try MessageInput(text:"go"))
        let bodies=try await server.enqueueBodyData().map{try JSONSerialization.jsonObject(with:$0) as! [String:Any]}
        #expect(bodies.map{$0["action"] as? String}==["auth.challenge","auth.assertion","thread.submit"])
        #expect(bodies[0]["authorization_token"]==nil)
        #expect(bodies[1]["authorization_token"]==nil)
        #expect(bodies[2]["authorization_token"] as? String=="g.one-use-grant")
        #expect(bodies[0]["payload"] as? [String:String]==["rp_id":"example.com"])
        let assertionPayload=try #require(bodies[1]["payload"] as? [String:Any])
        #expect(Set(assertionPayload.keys)==["credential_id","rp_id","client_data_json","authenticator_data","signature"])
        #expect(assertionPayload["credential_id"] as? String==assertion.credentialID)
        #expect(assertionPayload.values.compactMap{$0 as? String}.allSatisfy{!$0.contains("=")})
        let padded=RelayAuthenticationAssertion(credentialID:"YQ==",rpID:"example.com",clientDataJSON:"YQ",authenticatorData:"YQ",signature:"YQ")
        #expect(throws:RelayError.invalidConfiguration){try padded.validate()}
    }

    private func fixtureProjection()throws->[String:Any]{["schema_version":1,"thread":["thread_id":"thread-1","last_sequence":4,"title":"Relay","workspace":"remote","mode":"default","executor":["type":"remote"],"project":["project_id":"project-1"],"agent":["definition_id":"agent","version":"1","display_name":"Agent","default_mode":"default"]],"mailbox":["thread_id":"thread-1","last_sequence":4,"activity":"idle","cancel_requested":false,"queue":[]],"events":[],"dialogs":[],"diff":["comments":[]],"requested_after_sequence":4,"next_after_sequence":4,"snapshot_sequence":4,"has_more":false,"plugins":["available":false,"generation":0,"registrations":[]],"executor":["target":["type":"remote"],"activity":"idle"]]}
}

private actor AuthRelayServer {
    var posts:[Data]=[]
    func execute(_ request:URLRequest)throws->(Data,HTTPURLResponse){let url=request.url!;if request.httpMethod=="POST"{posts.append(request.httpBody!);let body=try JSONSerialization.jsonObject(with:request.httpBody!) as! [String:Any];return(try JSONSerialization.data(withJSONObject:["sequence":posts.count,"nonce":body["nonce"]!]),HTTPURLResponse(url:url,statusCode:202,httpVersion:nil,headerFields:nil)!)};let sequence=Int(URLComponents(url:url,resolvingAgainstBaseURL:false)!.queryItems!.first!.value!)!,body=try JSONSerialization.jsonObject(with:posts[sequence-1]) as! [String:Any],action=body["action"] as! String;let payload:Any=switch action{case "auth.challenge":["challenge":"AQID","rp_id":"example.com","expires_at":"2026-07-19T07:00:00Z"];case "auth.assertion":["authorization_token":"g.one-use-grant","expires_at":"2026-07-19T07:01:00Z"];default:["accepted":true]};return(try JSONSerialization.data(withJSONObject:["sequence":sequence,"nonce":body["nonce"]!,"account_id":"account","device_id":"device","payload":payload]),HTTPURLResponse(url:url,statusCode:200,httpVersion:nil,headerFields:nil)!)}
    func enqueueBodyData()->[Data]{posts}
}

private actor RelayServer {
    var requests:[URLRequest]=[];let payload:Any,pendingOnce:Bool,changedNonce:Bool,status:Int;var polls=0,varSequence=6
    init(payload:Any,pendingOnce:Bool=false,changedNonce:Bool=false,status:Int=200){self.payload=payload;self.pendingOnce=pendingOnce;self.changedNonce=changedNonce;self.status=status}
    func execute(_ request:URLRequest)throws->(Data,HTTPURLResponse){requests.append(request);let url=request.url!;if status != 200{return(Data(),HTTPURLResponse(url:url,statusCode:status,httpVersion:nil,headerFields:nil)!)};if request.httpMethod=="POST"{varSequence+=1;let body=try JSONSerialization.jsonObject(with:request.httpBody!) as! [String:Any];let nonce=body["nonce"] as! String;return(try JSONSerialization.data(withJSONObject:["sequence":varSequence,"nonce":changedNonce ? "changed":nonce]),HTTPURLResponse(url:url,statusCode:202,httpVersion:nil,headerFields:nil)!)};polls+=1;let sequence=Int(URLComponents(url:url,resolvingAgainstBaseURL:false)!.queryItems!.first!.value!)!;if pendingOnce&&polls==1{return(try JSONSerialization.data(withJSONObject:["sequence":sequence,"pending":true]),HTTPURLResponse(url:url,statusCode:202,httpVersion:nil,headerFields:nil)!)};let enqueue=try JSONSerialization.jsonObject(with:requests.last(where:{$0.httpMethod=="POST"})!.httpBody!) as! [String:Any];return(try JSONSerialization.data(withJSONObject:["sequence":sequence,"nonce":enqueue["nonce"]!,"account_id":"account","device_id":"device","payload":payload]),HTTPURLResponse(url:url,statusCode:200,httpVersion:nil,headerFields:nil)!)}
    func enqueueBodyData()->[Data]{requests.filter{$0.httpMethod=="POST"}.compactMap(\.httpBody)}
}
