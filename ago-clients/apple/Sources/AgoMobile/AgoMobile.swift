#if os(iOS)
import AgoClientCore
import AuthenticationServices
import SwiftUI
import UIKit

@main struct AgoMobileApp:App {
    @StateObject private var model=MobileModel()
    var body:some Scene{WindowGroup{MobileRoot(model:model)}}
}

enum MobileTransportMode:String,CaseIterable,Identifiable {case relay="Remote relay",direct="Direct daemon (local/dev)";var id:String{rawValue}}

@MainActor final class MobileModel:ObservableObject {
    @Published var mode:MobileTransportMode = .relay
    @Published var endpoint="https://"
    @Published var relayToken=""
    @Published var relayThreadID=""
    @Published var relyingPartyID=""
    @Published var actorID="ago-mobile"
    @Published var projects:[ProjectIdentity]=[]
    @Published var threads:[ThreadCatalogEntry]=[]
    @Published var projectID=""
    @Published var cursor:ProjectionCursor?
    @Published var status="Configure Ago Relay"
    @Published var error:String?
    @Published var draft=""
    private var mutations:MutationClient?
    private var messages:MessageClient?
    private let passkeys=PasskeyAssertionCoordinator()

    var isDirect:Bool{mode == .direct}
    var canAuthorizeMutation:Bool{isDirect || !relyingPartyID.isEmpty}
    var baseURL:URL?{guard let url=URL(string:endpoint),mode == .direct || url.scheme=="https" else{return nil};return url}
    var relayConfiguration:RelayConfiguration?{guard mode == .relay,let url=baseURL else{return nil};return try? RelayConfiguration(relayURL:url,projectID:projectID,bearerToken:relayToken)}
    func connect()async {
        guard let baseURL else{fail("Relay requires credential-free HTTPS; direct mode requires an absolute URL.");return}
        if mode == .relay {guard relayConfiguration != nil,!relayThreadID.isEmpty else{fail("Relay URL, in-memory bearer token, project ID, and thread ID are required.");return};projects=[];threads=[];await open(relayThreadID);return}
        error=nil;status="Loading projects…"
        do{projects=try await HTTPCatalogTransport(baseURL:baseURL).fetchProjects();if !projects.contains(where:{$0.projectID==projectID}){projectID=projects.first?.projectID ?? ""};try await refreshThreads();status="Connected"}catch{fail(error)}
    }
    func refreshThreads()async throws {
        guard let baseURL,!projectID.isEmpty else{threads=[];return};var result:[ThreadCatalogEntry]=[],next:String?
        repeat{let page=try await HTTPCatalogTransport(baseURL:baseURL).fetchCatalog(ThreadCatalogRequest(projectID:projectID,cursor:next));result += page.threads;next=page.nextCursor}while next != nil
        threads=result
    }
    func open(_ threadID:String)async {
        guard let baseURL else{return};let cursor=ProjectionCursor(threadID:threadID);self.cursor=cursor
        if let relayConfiguration {let relayProjection=RelayProjectionTransport(configuration:relayConfiguration);mutations=MutationClient(cursor:cursor,mutationTransport:RelayArchiveTransport(configuration:relayConfiguration),projectionTransport:relayProjection,actorID:actorID);messages=MessageClient(cursor:cursor,transport:RelayMessageTransport(configuration:relayConfiguration),projectionTransport:relayProjection,actorID:actorID)}
        else {let direct=HTTPProjectionTransport(baseURL:baseURL);mutations=MutationClient(cursor:cursor,mutationTransport:HTTPMutationTransport(baseURL:baseURL),projectionTransport:direct,actorID:actorID);messages=MessageClient(cursor:cursor,transport:HTTPMessageTransport(baseURL:baseURL),projectionTransport:direct,actorID:actorID)}
        await refreshProjection()
    }
    func refreshProjection()async{guard let cursor,let baseURL else{return};let transport:any ProjectionTransport=relayConfiguration.map{RelayProjectionTransport(configuration:$0)} ?? HTTPProjectionTransport(baseURL:baseURL);do{try await ReconnectController(cursor:cursor,transport:transport).refresh();status="Sequence \(cursor.sequence)"}catch{fail(error)}}
    func submit()async{guard !draft.isEmpty else{return};do{if let cursor,relayConfiguration != nil{let configuration=try await authenticatedRelayConfiguration(threadID:cursor.threadID);let projection=RelayProjectionTransport(configuration:configuration);try await MessageClient(cursor:cursor,transport:RelayMessageTransport(configuration:configuration),projectionTransport:projection,actorID:actorID).submit(text:draft,attachments:[],fileMentions:[])}else{guard let messages else{return};try await messages.submit(text:draft,attachments:[],fileMentions:[])};draft="";status="Sequence \(cursor?.sequence ?? 0)"}catch MutationError.conflict{await relayConflict()}catch RelayError.conflict{await relayConflict()}catch{fail(error)}}
    func mutate(_ action:@escaping(MutationClient)async throws->Void)async{guard let mutations else{return};do{try await action(mutations);status="Sequence \(cursor?.sequence ?? 0)"}catch MutationError.conflict{await relayConflict()}catch RelayError.conflict{await relayConflict()}catch{fail(error)}}
    func archive()async{if let cursor,relayConfiguration != nil{do{let configuration=try await authenticatedRelayConfiguration(threadID:cursor.threadID);let projection=RelayProjectionTransport(configuration:configuration);try await MutationClient(cursor:cursor,mutationTransport:RelayArchiveTransport(configuration:configuration),projectionTransport:projection,actorID:actorID).archive();self.cursor=nil}catch RelayError.conflict{await relayConflict()}catch{fail(error)}}else{await mutate{try await $0.archive()};cursor=nil;try? await refreshThreads()}}
    private func fail(_ value:any Error){fail(String(describing:value))}
    private func fail(_ message:String){error=message;status="Unavailable"}
    private func relayConflict()async{await refreshProjection();error="Authoritative state changed; projection refreshed."}
    private func authenticatedRelayConfiguration(threadID:String)async throws->RelayConfiguration {
        guard let configuration=relayConfiguration,!relyingPartyID.isEmpty else{throw RelayError.invalidConfiguration}
        let client=RelayClient(configuration:configuration),challenge=try await client.authenticationChallenge(threadID:threadID,rpID:relyingPartyID)
        guard let bytes=challenge.challengeData else{throw RelayError.protocolError}
        let assertion=try await passkeys.assert(relyingPartyID:challenge.rpID,challenge:bytes)
        let grant=try await client.authenticationAssertion(threadID:threadID,assertion:assertion)
        let token=grant.authorizationToken
        return try RelayConfiguration(relayURL:configuration.relayURL,projectID:configuration.projectID,bearerToken:configuration.bearerToken,authorization:{_,_,_ in token})
    }
}

@MainActor final class PasskeyAssertionCoordinator:NSObject,ASAuthorizationControllerDelegate,ASAuthorizationControllerPresentationContextProviding {
    private var continuation:CheckedContinuation<RelayAuthenticationAssertion,Error>?
    private var relyingPartyID=""
    private var controller:ASAuthorizationController?
    func assert(relyingPartyID:String,challenge:Data)async throws->RelayAuthenticationAssertion {
        guard continuation==nil else{throw RelayError.rejected}
        self.relyingPartyID=relyingPartyID
        let request=ASAuthorizationPlatformPublicKeyCredentialProvider(relyingPartyIdentifier:relyingPartyID).createCredentialAssertionRequest(challenge:challenge)
        return try await withCheckedThrowingContinuation{continuation in self.continuation=continuation;let controller=ASAuthorizationController(authorizationRequests:[request]);self.controller=controller;controller.delegate=self;controller.presentationContextProvider=self;controller.performRequests()}
    }
    func authorizationController(controller:ASAuthorizationController,didCompleteWithAuthorization authorization:ASAuthorization){guard let credential=authorization.credential as? ASAuthorizationPlatformPublicKeyCredentialAssertion else{finish(.failure(RelayError.protocolError));return};finish(.success(RelayAuthenticationAssertion(credentialID:credential.credentialID.base64URLEncodedString(),rpID:relyingPartyID,clientDataJSON:credential.rawClientDataJSON.base64URLEncodedString(),authenticatorData:credential.rawAuthenticatorData.base64URLEncodedString(),signature:credential.signature.base64URLEncodedString())))}
    func authorizationController(controller:ASAuthorizationController,didCompleteWithError error:Error){finish(.failure(error))}
    func presentationAnchor(for controller:ASAuthorizationController)->ASPresentationAnchor{UIApplication.shared.connectedScenes.compactMap{$0 as? UIWindowScene}.flatMap(\.windows).first(where:\.isKeyWindow) ?? ASPresentationAnchor()}
    private func finish(_ result:Result<RelayAuthenticationAssertion,Error>){let value=continuation;continuation=nil;controller=nil;relyingPartyID="";value?.resume(with:result)}
}

struct MobileRoot:View {
    @ObservedObject var model:MobileModel
    var body:some View{NavigationStack{List{
        Section("Transport"){
            Picker("Mode",selection:$model.mode){ForEach(MobileTransportMode.allCases){Text($0.rawValue).tag($0)}}
            TextField(model.isDirect ? "http://127.0.0.1:8080":"https://relay.example",text:$model.endpoint).textInputAutocapitalization(.never).keyboardType(.URL)
            if model.isDirect {TextField("Actor ID",text:$model.actorID).textInputAutocapitalization(.never)}
            else {
                SecureField("In-memory relay bearer",text:$model.relayToken).textInputAutocapitalization(.never)
                TextField("Project ID",text:$model.projectID).textInputAutocapitalization(.never)
                TextField("Thread ID",text:$model.relayThreadID).textInputAutocapitalization(.never)
                TextField("Passkey relying party ID",text:$model.relyingPartyID).textInputAutocapitalization(.never)
                Text("Relay credentials are held in memory only. Submit and archive request a passkey assertion and use its scope-bound grant for that mutation only. Registration is not exposed.").font(.caption).foregroundStyle(.secondary)
            }
            Button("Connect"){Task{await model.connect()}}.disabled(model.baseURL==nil)
            if !model.isDirect{Text("Passkey registration is intentionally unavailable.").font(.caption).foregroundStyle(.secondary)}
        }
        if model.isDirect,!model.projects.isEmpty{Section("Project"){Picker("Project",selection:$model.projectID){ForEach(model.projects,id:\.projectID){Text($0.displayName ?? $0.projectID).tag($0.projectID)}}.onChange(of:model.projectID){_,_ in Task{try? await model.refreshThreads()}}}}
        if model.isDirect {Section("Threads"){ForEach(model.threads){thread in NavigationLink{MobileThread(model:model,threadID:thread.threadID)}label:{VStack(alignment:.leading){Text(thread.title.isEmpty ? thread.threadID:thread.title);Text("\(thread.activity.rawValue) · #\(thread.lastSequence)").font(.caption).foregroundStyle(.secondary)}}}}}
        else if let cursor=model.cursor {Section("Relay thread"){NavigationLink(cursor.thread?.title ?? cursor.threadID){MobileThread(model:model,threadID:cursor.threadID)}}}
        Section{Text(model.status).foregroundStyle(.secondary);if let error=model.error{Text(error).foregroundStyle(.red).textSelection(.enabled)}}
    }.navigationTitle("Ago")}}
}

struct MobileThread:View {
    @ObservedObject var model:MobileModel;let threadID:String
    var body:some View{Group{if let cursor=model.cursor,cursor.threadID==threadID{MobileProjection(model:model,cursor:cursor)}else{ProgressView("Loading thread…")}}.navigationTitle(model.cursor?.thread?.title ?? threadID).navigationBarTitleDisplayMode(.inline).task{if model.cursor?.threadID != threadID{await model.open(threadID)}}.refreshable{await model.refreshProjection()}.toolbar{ToolbarItem(placement:.topBarTrailing){Button("Archive",role:.destructive){Task{await model.archive()}}.disabled(!model.canAuthorizeMutation)}}}
}

struct MobileProjection:View {
    @ObservedObject var model:MobileModel;@ObservedObject var cursor:ProjectionCursor
    @State private var queueEdit:MobileQueueEdit?
    @State private var dialogText=""
    @State private var commentTarget:MobileCommentTarget?
    var pendingDialog:PluginDialog?{cursor.dialogs.first{$0.state=="pending"}}
    var body:some View{List{
        Section("Message"){TextEditor(text:$model.draft).frame(minHeight:70);Button("Send"){Task{await model.submit()}}.disabled(model.draft.isEmpty || !model.canAuthorizeMutation)}
        Section("Transcript"){ForEach(EventProjection.timeline(cursor.events)){item in VStack(alignment:.leading){Text(item.title ?? item.kind.rawValue).font(.caption).foregroundStyle(.secondary);Text(item.text ?? item.detail?.formatted ?? "").textSelection(.enabled)}}}
        if model.isDirect {Section("Queue"){ForEach(cursor.mailbox?.queue ?? [],id:\.queueItemID){item in VStack(alignment:.leading){Text(item.content.formatted).font(.caption.monospaced());HStack{Button("Edit"){queueEdit=MobileQueueEdit(item:item)};Button("Remove",role:.destructive){Task{await model.mutate{try await $0.dequeue(queueItemID:item.queueItemID)}}};Button("Steer"){Task{await model.mutate{try await $0.steer(queueItemID:item.queueItemID)}}}.disabled(cursor.mailbox?.activeTurnID==nil)}}}}
            if let dialog=pendingDialog{Section("Action required"){Text(dialog.request.formatted);if dialog.requestType=="confirm"{HStack{Button("Confirm"){resolve(dialog,.ok(.bool(true)))};Button("Decline"){resolve(dialog,.ok(.bool(false)))}}}else{TextField("Response",text:$dialogText);Button("Submit"){resolve(dialog,.ok(.string(dialogText)))}.disabled(dialogText.isEmpty)};Button("Cancel",role:.cancel){resolve(dialog,.cancelled)}}}
        }
        MobileDiff(model:model,cursor:cursor,commentTarget:$commentTarget)
    }.sheet(item:$queueEdit){MobileQueueEditor(edit:$0,model:model)}.sheet(item:$commentTarget){MobileCommentEditor(target:$0,model:model)}}
    private func resolve(_ dialog:PluginDialog,_ response:UIResult){Task{await model.mutate{try await $0.resolve(dialog,response:response)}}}
}

struct MobileDiff:View {
    @ObservedObject var model:MobileModel;@ObservedObject var cursor:ProjectionCursor;@Binding var commentTarget:MobileCommentTarget?
    @ViewBuilder var body:some View{if let snapshot=cursor.diff?.snapshot{changes("Unstaged",snapshot.projection.unstaged,snapshot,true);changes("Staged",snapshot.projection.staged,snapshot,false)}else{Section("Diff"){Text("No authoritative snapshot.").foregroundStyle(.secondary)}}}
    @ViewBuilder private func changes(_ title:String,_ changes:[GitChange],_ snapshot:GitSnapshot,_ stage:Bool)->some View{Section(title){ForEach(changes,id:\.id){change in DisclosureGroup(change.path){ForEach(change.hunks,id:\.id){hunk in Text(hunk.patch).font(.caption.monospaced()).textSelection(.enabled)};if model.isDirect{HStack{Button(stage ? "Stage":"Unstage"){Task{await model.mutate{if stage{try await $0.stage(snapshotRevision:snapshot.revision,snapshotDigest:snapshot.digest,unitIDs:[change.id])}else{try await $0.unstage(snapshotRevision:snapshot.revision,snapshotDigest:snapshot.digest,unitIDs:[change.id])}}}}.disabled(change.protected || !change.mutationSupported);Button("Comment"){commentTarget=MobileCommentTarget(snapshot:snapshot,change:change)}}}}}}}
}

struct MobileQueueEdit:Identifiable{let item:QueueItem;var id:String{item.queueItemID}}
struct MobileQueueEditor:View{let edit:MobileQueueEdit;@ObservedObject var model:MobileModel;@Environment(\.dismiss)var dismiss;@State private var text:String;@State private var error:String?;init(edit:MobileQueueEdit,model:MobileModel){self.edit=edit;self.model=model;_text=State(initialValue:edit.item.content.formatted)};var body:some View{NavigationStack{VStack{TextEditor(text:$text).font(.body.monospaced());if let error{Text(error).foregroundStyle(.red)}}.padding().navigationTitle("Edit Queue JSON").toolbar{ToolbarItem(placement:.cancellationAction){Button("Cancel"){dismiss()}};ToolbarItem(placement:.confirmationAction){Button("Save"){do{let value=try JSONValue.parse(text);Task{await model.mutate{try await $0.edit(queueItemID:edit.item.queueItemID,content:value)};dismiss()}}catch{self.error="Invalid JSON"}}}}}}}

struct MobileCommentTarget:Identifiable{let snapshot:GitSnapshot,change:GitChange;var id:String{change.id}}
struct MobileCommentEditor:View{let target:MobileCommentTarget;@ObservedObject var model:MobileModel;@Environment(\.dismiss)var dismiss;@State private var text="";var body:some View{NavigationStack{TextEditor(text:$text).padding().navigationTitle("Request Change").toolbar{ToolbarItem(placement:.cancellationAction){Button("Cancel"){dismiss()}};ToolbarItem(placement:.confirmationAction){Button("Submit"){Task{await model.mutate{try await $0.requestChange(snapshotRevision:target.snapshot.revision,snapshotDigest:target.snapshot.digest,fileID:target.change.id,hunkID:nil,body:text)};dismiss()}}.disabled(text.trimmingCharacters(in:.whitespacesAndNewlines).isEmpty)}}}}}
#else
@main enum AgoMobileUnsupportedHost { static func main() {} }
#endif
