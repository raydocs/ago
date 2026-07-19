import CryptoKit
import Foundation

public enum ConformanceError: Error, Equatable { case invalidArguments, invalidProjectionURL, malformedProjection }

public struct ConformanceEndpoint: Equatable, Sendable {
    public let baseURL: URL, threadID: String
    public static func parse(_ value: String) throws -> ConformanceEndpoint {
        guard var components=URLComponents(string:value),["http","https"].contains(components.scheme),components.host != nil,components.user==nil,components.password==nil,components.query==nil,components.fragment==nil else{throw ConformanceError.invalidProjectionURL}
        let parts=components.percentEncodedPath.split(separator:"/",omittingEmptySubsequences:false)
        guard parts.count>=5,parts[parts.count-4]=="v1",parts[parts.count-3]=="threads",parts.last=="projection",let thread=String(parts[parts.count-2]).removingPercentEncoding,!thread.isEmpty else{throw ConformanceError.invalidProjectionURL}
        components.percentEncodedPath=parts.dropLast(4).joined(separator:"/");if components.percentEncodedPath.isEmpty{components.percentEncodedPath="/"}
        guard let base=components.url else{throw ConformanceError.invalidProjectionURL};return ConformanceEndpoint(baseURL:base,threadID:thread)
    }
}

public enum ProjectionConformance {
    @MainActor public static func run(projectionURL: String, transport: (any ProjectionTransport)? = nil) async throws -> String {
        let endpoint=try ConformanceEndpoint.parse(projectionURL)
        let recording=RecordingProjectionTransport(base:transport ?? HTTPProjectionTransport(baseURL:endpoint.baseURL))
        let cursor=ProjectionCursor(threadID:endpoint.threadID)
        try await ReconnectController(cursor:cursor,transport:recording,pageLimit:200).refresh()
        return try summarize(pages:await recording.pages)
    }

    static func summarize(pages:[Data])throws->String {
        guard let last=pages.last else{throw ConformanceError.malformedProjection}
        var rawEvents:[JSONValue]=[]
        for page in pages { guard let object=try JSONDecoder().decode(JSONValue.self,from:page).objectValue,let events=object["events"]?.arrayValue else{throw ConformanceError.malformedProjection};rawEvents += events }
        guard let object=try JSONDecoder().decode(JSONValue.self,from:last).objectValue,
              var mailbox=object["mailbox"]?.objectValue,let queue=mailbox.removeValue(forKey:"queue")?.arrayValue,
              let dialogs=object["dialogs"]?.arrayValue,let diff=object["diff"]?.objectValue,
              let snapshot=object["snapshot_sequence"],let sequence=snapshot.numberUInt64,
              let comments=diff["comments"]?.arrayValue else{throw ConformanceError.malformedProjection}
        let mailboxValue=JSONValue.object(mailbox),queueValue=JSONValue.array(queue),dialogsValue=JSONValue.array(dialogs),diffValue=JSONValue.object(diff),eventsValue=JSONValue.array(rawEvents)
        let whole:JSONValue = .object(["snapshot_sequence":snapshot,"mailbox":mailboxValue,"queue":queueValue,"dialogs":dialogsValue,"diff":diffValue,"events":eventsValue])
        var mailboxSummary=mailbox;mailboxSummary["digest"] = .string(digest(mailboxValue))
        let first=rawEvents.first?.objectValue?["sequence"] ?? .null,lastSequence=rawEvents.last?.objectValue?["sequence"] ?? .null
        let output:JSONValue = .object([
            "digest":.string(digest(whole)),"snapshot_sequence":.number(Double(sequence)),"mailbox":.object(mailboxSummary),
            "queue":.object(["count":.number(Double(queue.count)),"digest":.string(digest(queueValue))]),
            "dialogs":.object(["count":.number(Double(dialogs.count)),"digest":.string(digest(dialogsValue))]),
            "diff":.object(["has_snapshot":.bool(diff["snapshot"] != .null),"comment_count":.number(Double(comments.count)),"digest":.string(digest(diffValue))]),
            "events":.object(["count":.number(Double(rawEvents.count)),"first_sequence":first,"last_sequence":lastSequence,"digest":.string(digest(eventsValue))])
        ])
        return try canonical(output)
    }
    private static func digest(_ value:JSONValue)->String { let bytes=Data((try! canonical(value)).utf8);return SHA256.hash(data:bytes).map{String(format:"%02x",$0)}.joined() }
    private static func canonical(_ value:JSONValue)throws->String { let encoder=JSONEncoder();encoder.outputFormatting=[.sortedKeys,.withoutEscapingSlashes];return String(decoding:try encoder.encode(value),as:UTF8.self) }
}

private actor RecordingProjectionTransport:ProjectionTransport {
    let base:any ProjectionTransport;private(set)var pages:[Data]=[]
    init(base:any ProjectionTransport){self.base=base}
    func fetchProjection(_ request:ProjectionRequest)async throws->Data{let data=try await base.fetchProjection(request);pages.append(data);return data}
}

private extension JSONValue { var numberUInt64:UInt64?{if case .number(let value)=self,value>=0,value.rounded()==value{return UInt64(value)};return nil} }
