import Foundation
import Combine
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

public struct ProjectionRequest: Equatable, Sendable { public let threadID:String; public let after:UInt64; public let limit:Int; public init(threadID:String,after:UInt64,limit:Int){self.threadID=threadID;self.after=after;self.limit=limit} }
public protocol ProjectionTransport: Sendable { func fetchProjection(_ request:ProjectionRequest) async throws -> Data }
public struct ThreadCatalogRequest:Equatable,Sendable { public let projectID,search:String; public let archive:ArchiveFilter; public let limit:Int; public let cursor:String?; public init(projectID:String,search:String = "",archive:ArchiveFilter = .active,limit:Int = 100,cursor:String? = nil){self.projectID=projectID;self.search=search;self.archive=archive;self.limit=limit;self.cursor=cursor} }
public protocol CatalogTransport:Sendable { func fetchProjects()async throws->[ProjectIdentity]; func fetchCatalog(_ request:ThreadCatalogRequest)async throws->ThreadCatalogPage }

public struct HTTPClientConfiguration:Equatable,Sendable {
    public let baseURL:URL;public let bearerToken:String?
    public init(baseURL:URL,bearerToken:String?=nil){self.baseURL=baseURL;self.bearerToken=bearerToken}
    func request(url:URL)->URLRequest{var request=URLRequest(url:url);if let bearerToken{request.setValue("Bearer \(bearerToken)",forHTTPHeaderField:"Authorization")};return request}
}

@MainActor public final class ProjectionCursor: ObservableObject {
    public let threadID:String; @Published public private(set) var sequence:UInt64=0; @Published public private(set) var events:[AgoEvent]=[]
    @Published public private(set) var thread:ThreadMetadata?; @Published public private(set) var mailbox:Mailbox?; @Published public private(set) var dialogs:[PluginDialog]=[]; @Published public private(set) var diff:GitDiffProjection?; @Published public private(set) var plugins:PluginProjection?; @Published public private(set) var executor:ExecutorProjection?
    public init(threadID:String){self.threadID=threadID}
    fileprivate func apply(_ page:ThreadProjection) throws {
        guard page.thread.threadID == threadID, page.mailbox.threadID == threadID, page.dialogs.allSatisfy({$0.threadID == threadID}), page.events.allSatisfy({$0.threadID == threadID}) else { throw ProjectionError.inconsistent("thread identity changed") }
        guard page.requestedAfterSequence == sequence, page.nextAfterSequence <= page.snapshotSequence, page.thread.lastSequence == page.snapshotSequence, page.mailbox.lastSequence == page.snapshotSequence else { throw ProjectionError.inconsistent("projection cursor/snapshot mismatch") }
        var expected=sequence+1; for event in page.events { guard event.sequence == expected else { throw ProjectionError.inconsistent("events are not contiguous"); }; expected += 1 }
        let computed=page.events.last?.sequence ?? sequence; guard computed == page.nextAfterSequence, !page.hasMore || !page.events.isEmpty else { throw ProjectionError.inconsistent("next cursor does not match events") }
        events.append(contentsOf:page.events); sequence=page.nextAfterSequence; thread=page.thread; mailbox=page.mailbox; dialogs=page.dialogs; diff=page.diff; plugins=page.plugins; executor=page.executor
    }
}

@MainActor public final class ReconnectController {
    private let cursor:ProjectionCursor; private let transport:any ProjectionTransport; private let pageLimit:Int
    public init(cursor:ProjectionCursor,transport:any ProjectionTransport,pageLimit:Int=200){self.cursor=cursor;self.transport=transport;self.pageLimit=pageLimit}
    public func refresh() async throws { repeat { let request=ProjectionRequest(threadID:cursor.threadID,after:cursor.sequence,limit:pageLimit); let page=try ProjectionDecoder.decode(await transport.fetchProjection(request)); try cursor.apply(page); if !page.hasMore{return} } while true }
}

public struct HTTPProjectionTransport: ProjectionTransport {
    public let configuration:HTTPClientConfiguration;public var baseURL:URL{configuration.baseURL}; public init(baseURL:URL,bearerToken:String?=nil){configuration=HTTPClientConfiguration(baseURL:baseURL,bearerToken:bearerToken)};public init(configuration:HTTPClientConfiguration){self.configuration=configuration}
    public func fetchProjection(_ request:ProjectionRequest) async throws -> Data {let urlRequest=Self.makeURLRequest(configuration:configuration,request:request);let (data,response)=try await URLSession.shared.data(for:urlRequest); guard let http=response as? HTTPURLResponse,(200..<300).contains(http.statusCode) else {throw URLError(.badServerResponse)}; return data }
    static func makeURLRequest(configuration:HTTPClientConfiguration,request:ProjectionRequest)->URLRequest{var components=URLComponents(url:percentEncodedURL(baseURL:configuration.baseURL,segments:["v1","threads",request.threadID,"projection"]),resolvingAgainstBaseURL:false)!;components.queryItems=[URLQueryItem(name:"after",value:String(request.after)),URLQueryItem(name:"limit",value:String(request.limit))];return configuration.request(url:components.url!)}
}

public struct HTTPCatalogTransport:CatalogTransport {
    public let configuration:HTTPClientConfiguration;public var baseURL:URL{configuration.baseURL}; public init(baseURL:URL,bearerToken:String?=nil){configuration=HTTPClientConfiguration(baseURL:baseURL,bearerToken:bearerToken)};public init(configuration:HTTPClientConfiguration){self.configuration=configuration}
    public func fetchProjects()async throws->[ProjectIdentity] { let request=configuration.request(url:percentEncodedURL(baseURL:baseURL,segments:["v1","threads"]));let (data,response)=try await URLSession.shared.data(for:request); try Self.requireSuccess(response); let raw=try JSONSerialization.jsonObject(with:data); guard let object=raw as? [String:Any],Set(object.keys)==["threads"] else{throw ProjectionError.inconsistent("malformed project thread list")}; struct List:Decodable{let threads:[ThreadMetadata]}; let list=try JSONDecoder().decode(List.self,from:data); var seen=Set<String>(); return list.threads.compactMap{seen.insert($0.project.projectID).inserted ? $0.project:nil}.sorted{$0.projectID<$1.projectID} }
    public func fetchCatalog(_ request:ThreadCatalogRequest)async throws->ThreadCatalogPage { let urlRequest=Self.makeURLRequest(configuration:configuration,request:request); let (data,response)=try await URLSession.shared.data(for:urlRequest); try Self.requireSuccess(response); return try ThreadCatalogDecoder.decode(data) }
    static func makeURLRequest(baseURL:URL,request:ThreadCatalogRequest)->URLRequest{makeURLRequest(configuration:HTTPClientConfiguration(baseURL:baseURL),request:request)}
    static func makeURLRequest(configuration:HTTPClientConfiguration,request:ThreadCatalogRequest)->URLRequest { var components=URLComponents(url:percentEncodedURL(baseURL:configuration.baseURL,segments:["v1","threads"]),resolvingAgainstBaseURL:false)!; var query=[("project_id",request.projectID)]; if !request.search.isEmpty{query.append(("search",request.search))}; query.append(("archive",request.archive.rawValue)); query.append(("limit",String(request.limit))); if let cursor=request.cursor{query.append(("cursor",cursor))}; let allowed=CharacterSet.urlQueryAllowed.subtracting(CharacterSet(charactersIn:"&=+/?#%")); components.percentEncodedQuery=query.map{"\($0.0)=\($0.1.addingPercentEncoding(withAllowedCharacters:allowed)!)"}.joined(separator:"&"); var result=configuration.request(url:components.url!); result.httpMethod="GET"; return result }
    private static func requireSuccess(_ response:URLResponse)throws { guard let http=response as? HTTPURLResponse,(200..<300).contains(http.statusCode) else{throw URLError(.badServerResponse)} }
}

func percentEncodedURL(baseURL:URL,segments:[String]) -> URL {
    var components=URLComponents(url:baseURL,resolvingAgainstBaseURL:false)!
    let allowed=CharacterSet.urlPathAllowed.subtracting(CharacterSet(charactersIn:"/?#%"))
    let suffix=segments.map{$0.addingPercentEncoding(withAllowedCharacters:allowed)!}.joined(separator:"/")
    components.percentEncodedPath = "/" + components.percentEncodedPath.trimmingCharacters(in:CharacterSet(charactersIn:"/")) + "/" + suffix
    return components.url!
}
