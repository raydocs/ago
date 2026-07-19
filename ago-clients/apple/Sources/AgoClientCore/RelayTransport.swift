import Foundation
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

public enum RelayAction:String,Codable,Sendable { case projection="thread.projection",submit="thread.submit",archive="thread.archive",authChallenge="auth.challenge",authAssertion="auth.assertion" }
public enum RelayError:Error,Equatable { case invalidConfiguration,protocolError,authorizationFailed,conflict,rejected,unavailable,timedOut,unsupported }
public typealias RelayAuthorizationProvider = @Sendable (RelayAction,String,String) async throws -> String?
public typealias RelayRequestExecutor = @Sendable (URLRequest) async throws -> (Data,HTTPURLResponse)

public struct RelayConfiguration:Sendable {
    public let relayURL:URL,projectID:String,bearerToken:String
    public let maximumPollAttempts:Int
    public let minimumPollDelay:Duration
    let authorization:RelayAuthorizationProvider?
    let execute:RelayRequestExecutor
    public init(relayURL:URL,projectID:String,bearerToken:String,maximumPollAttempts:Int=8,minimumPollDelay:Duration = .milliseconds(100),authorization:RelayAuthorizationProvider?=nil,execute:RelayRequestExecutor?=nil)throws {
        guard relayURL.scheme=="https",relayURL.user==nil,relayURL.password==nil,relayURL.query==nil,relayURL.fragment==nil,!projectID.isEmpty,!bearerToken.isEmpty,maximumPollAttempts>0 else{throw RelayError.invalidConfiguration}
        self.relayURL=relayURL;self.projectID=projectID;self.bearerToken=bearerToken;self.maximumPollAttempts=maximumPollAttempts;self.minimumPollDelay=minimumPollDelay;self.authorization=authorization
        self.execute=execute ?? { request in let(data,response)=try await URLSession.shared.data(for:request);guard let http=response as? HTTPURLResponse else{throw RelayError.protocolError};return(data,http) }
    }
}

public struct RelayClient:Sendable {
    public let configuration:RelayConfiguration
    public init(configuration:RelayConfiguration){self.configuration=configuration}
    public func projection(_ request:ProjectionRequest)async throws->Data {try await perform(action:.projection,threadID:request.threadID,payload:try encode(ProjectionPayload(afterSequence:request.after,limit:request.limit)))}
    public func submit(threadID:String,expectedSequence:UInt64,input:MessageInput)async throws { _ = try await perform(action:.submit,threadID:threadID,payload:try encode(SubmitPayload(expectedSequence:expectedSequence,content:input,class:"normal"))) }
    public func archive(threadID:String,expectedSequence:UInt64)async throws { _ = try await perform(action:.archive,threadID:threadID,payload:try encode(ArchivePayload(expectedSequence:expectedSequence))) }
    public func authenticationChallenge(threadID:String,rpID:String)async throws->RelayAuthenticationChallenge {guard !rpID.isEmpty else{throw RelayError.invalidConfiguration};let value=try decodeStrict(RelayAuthenticationChallenge.self,try await perform(action:.authChallenge,threadID:threadID,payload:try encode(AuthChallengePayload(rpID:rpID))),keys:["challenge","rp_id","expires_at"]);guard value.rpID==rpID,value.challengeData != nil,validRFC3339(value.expiresAt) else{throw RelayError.protocolError};return value}
    public func authenticationAssertion(threadID:String,assertion:RelayAuthenticationAssertion)async throws->RelayAuthorizationGrant {try assertion.validate();let value=try decodeStrict(RelayAuthorizationGrant.self,try await perform(action:.authAssertion,threadID:threadID,payload:try encode(assertion)),keys:["authorization_token","expires_at"]);guard value.authorizationToken.hasPrefix("g."),value.authorizationToken.count>2,validRFC3339(value.expiresAt) else{throw RelayError.protocolError};return value}

    private func perform(action:RelayAction,threadID:String,payload:Data)async throws->Data {
        guard !threadID.isEmpty else{throw RelayError.invalidConfiguration}
        let nonce=UUID().uuidString,authorizationToken=(action == .submit || action == .archive) ? try await configuration.authorization?(action,configuration.projectID,threadID):nil
        let body=try encode(Enqueue(nonce:nonce,projectID:configuration.projectID,threadID:threadID,action:action,authorizationToken:authorizationToken,payload:try JSONDecoder().decode(JSONValue.self,from:payload)))
        var accepted:Accepted?
        for attempt in 0..<3 { do {let(data,response)=try await request(path:["v1","relay","requests"],method:"POST",body:body);try status(response);guard response.statusCode==202 else{throw RelayError.protocolError};let value=try strict(Accepted.self,data,keys:["sequence","nonce"]);guard value.sequence>0,value.nonce==nonce else{throw RelayError.protocolError};accepted=value;break}catch let error as RelayError{guard error == .unavailable,attempt<2 else{throw error};try await sleep(attempt)}catch{if attempt==2{throw RelayError.unavailable};try await sleep(attempt)} }
        guard let accepted else{throw RelayError.unavailable}
        for attempt in 0..<configuration.maximumPollAttempts {
            do {
                let(data,response)=try await request(path:["v1","relay","results"],query:[URLQueryItem(name:"sequence",value:String(accepted.sequence))]);try status(response)
                if response.statusCode==202 {let pending=try strict(Pending.self,data,keys:["sequence","pending"]);guard pending.sequence==accepted.sequence,pending.pending else{throw RelayError.protocolError};if attempt+1==configuration.maximumPollAttempts{throw RelayError.timedOut};try await sleep(attempt);continue}
                guard response.statusCode==200 else{throw RelayError.protocolError}
                let object=try JSONSerialization.jsonObject(with:data);guard let raw=object as? [String:Any],Set(raw.keys)==["sequence","nonce","account_id","device_id","payload"] || Set(raw.keys)==["sequence","nonce","account_id","device_id","error"],raw["sequence"] as? UInt64==accepted.sequence || (raw["sequence"] as? NSNumber)?.uint64Value==accepted.sequence,raw["nonce"] as? String==nonce,let account=raw["account_id"] as? String,!account.isEmpty,let device=raw["device_id"] as? String,!device.isEmpty else{throw RelayError.protocolError}
                if let error=raw["error"] as? [String:Any] {guard Set(error.keys)==["code","message"],let code=error["code"] as? String,error["message"] is String else{throw RelayError.protocolError};if code=="conflict" || code=="replay"{throw RelayError.conflict};if code=="unauthorized" || code=="authorization_required"{throw RelayError.authorizationFailed};throw RelayError.rejected}
                guard let result=raw["payload"],JSONSerialization.isValidJSONObject(result)else{throw RelayError.protocolError};return try JSONSerialization.data(withJSONObject:result)
            } catch let error as RelayError {if error != .unavailable{throw error};if attempt+1==configuration.maximumPollAttempts{throw RelayError.timedOut};try await sleep(attempt)} catch {if attempt+1==configuration.maximumPollAttempts{throw RelayError.timedOut};try await sleep(attempt)}
        }
        throw RelayError.timedOut
    }
    private func request(path:[String],method:String="GET",query:[URLQueryItem]=[],body:Data?=nil)async throws->(Data,HTTPURLResponse){var components=URLComponents(url:percentEncodedURL(baseURL:configuration.relayURL,segments:path),resolvingAgainstBaseURL:false)!;components.queryItems=query.isEmpty ? nil:query;var request=URLRequest(url:components.url!);request.httpMethod=method;request.httpBody=body;request.setValue("Bearer \(configuration.bearerToken)",forHTTPHeaderField:"Authorization");request.setValue("no-store",forHTTPHeaderField:"Cache-Control");if body != nil{request.setValue("application/json",forHTTPHeaderField:"Content-Type")};return try await configuration.execute(request)}
    private func status(_ response:HTTPURLResponse)throws{switch response.statusCode{case 200,202:return;case 401,403:throw RelayError.authorizationFailed;case 409:throw RelayError.conflict;case 500...599:throw RelayError.unavailable;default:throw RelayError.rejected}}
    private func sleep(_ attempt:Int)async throws{let multiplier=1 << min(attempt,4);try await Task.sleep(for:configuration.minimumPollDelay*multiplier)}
    private func encode<T:Encodable>(_ value:T)throws->Data{try JSONEncoder().encode(value)}
    private func strict<T:Decodable>(_ type:T.Type,_ data:Data,keys:Set<String>)throws->T{let raw=try JSONSerialization.jsonObject(with:data);guard let object=raw as? [String:Any],Set(object.keys)==keys else{throw RelayError.protocolError};return try JSONDecoder().decode(type,from:data)}
    private func decodeStrict<T:Decodable>(_ type:T.Type,_ data:Data,keys:Set<String>)throws->T{try strict(type,data,keys:keys)}
}

public struct RelayProjectionTransport:ProjectionTransport { let client:RelayClient;public init(configuration:RelayConfiguration){client=RelayClient(configuration:configuration)};public func fetchProjection(_ request:ProjectionRequest)async throws->Data{try await client.projection(request)} }
public struct RelayMessageTransport:MessageTransport { let client:RelayClient;public init(configuration:RelayConfiguration){client=RelayClient(configuration:configuration)};public func upload(threadID:String,attachment:AttachmentRef,bytes:Data)async throws{throw RelayError.unsupported};public func submit(threadID:String,envelope:MutationEnvelope,input:MessageInput)async throws{try await client.submit(threadID:threadID,expectedSequence:envelope.expectedSequence,input:input)} }
public struct RelayArchiveTransport:MutationTransport { let client:RelayClient;public init(configuration:RelayConfiguration){client=RelayClient(configuration:configuration)};public func send(_ request:MutationRequest)async throws{guard case let .archive(threadID,envelope)=request else{throw RelayError.unsupported};try await client.archive(threadID:threadID,expectedSequence:envelope.expectedSequence)} }

private struct ProjectionPayload:Codable {let afterSequence:UInt64,limit:Int;enum CodingKeys:String,CodingKey{case afterSequence="after_sequence",limit}}
private struct SubmitPayload:Codable {let expectedSequence:UInt64,content:MessageInput,`class`:String;enum CodingKeys:String,CodingKey{case expectedSequence="expected_sequence",content,`class`}}
private struct ArchivePayload:Codable {let expectedSequence:UInt64;enum CodingKeys:String,CodingKey{case expectedSequence="expected_sequence"}}
private struct AuthChallengePayload:Codable {let rpID:String;enum CodingKeys:String,CodingKey{case rpID="rp_id"}}
private struct Enqueue:Codable {let nonce,projectID,threadID:String;let action:RelayAction;let authorizationToken:String?;let payload:JSONValue;enum CodingKeys:String,CodingKey{case nonce,projectID="project_id",threadID="thread_id",action,authorizationToken="authorization_token",payload}}
private struct Accepted:Codable {let sequence:UInt64,nonce:String}
private struct Pending:Codable {let sequence:UInt64,pending:Bool}

public struct RelayAuthenticationChallenge:Codable,Equatable,Sendable {public let challenge,rpID,expiresAt:String;enum CodingKeys:String,CodingKey{case challenge,rpID="rp_id",expiresAt="expires_at"};public var challengeData:Data?{Data(base64URLEncoded:challenge)}}
public struct RelayAuthenticationAssertion:Codable,Equatable,Sendable {
    public let credentialID,rpID,clientDataJSON,authenticatorData,signature:String
    enum CodingKeys:String,CodingKey{case credentialID="credential_id",rpID="rp_id",clientDataJSON="client_data_json",authenticatorData="authenticator_data",signature}
    public init(credentialID:String,rpID:String,clientDataJSON:String,authenticatorData:String,signature:String){self.credentialID=credentialID;self.rpID=rpID;self.clientDataJSON=clientDataJSON;self.authenticatorData=authenticatorData;self.signature=signature}
    func validate()throws{guard !rpID.isEmpty,[credentialID,clientDataJSON,authenticatorData,signature].allSatisfy({Data(base64URLEncoded:$0) != nil})else{throw RelayError.invalidConfiguration}}
}
public struct RelayAuthorizationGrant:Codable,Equatable,Sendable {public let authorizationToken,expiresAt:String;enum CodingKeys:String,CodingKey{case authorizationToken="authorization_token",expiresAt="expires_at"}}
public extension Data {
    init?(base64URLEncoded value:String){guard !value.isEmpty,!value.contains("="),value.range(of:"^[A-Za-z0-9_-]+$",options:.regularExpression) != nil else{return nil};let padding=String(repeating:"=",count:(4-value.count%4)%4);guard let decoded=Data(base64Encoded:value.replacingOccurrences(of:"-",with:"+").replacingOccurrences(of:"_",with:"/")+padding),decoded.base64URLEncodedString()==value else{return nil};self=decoded}
    func base64URLEncodedString()->String{base64EncodedString().replacingOccurrences(of:"+",with:"-").replacingOccurrences(of:"/",with:"_").replacingOccurrences(of:"=",with:"")}
}
private func validRFC3339(_ value:String)->Bool{let formatter=ISO8601DateFormatter();formatter.formatOptions=[.withInternetDateTime,.withFractionalSeconds];if formatter.date(from:value) != nil{return true};formatter.formatOptions=[.withInternetDateTime];return formatter.date(from:value) != nil}
