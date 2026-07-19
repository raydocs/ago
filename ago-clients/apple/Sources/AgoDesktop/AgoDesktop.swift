import SwiftUI
import UserNotifications
import UniformTypeIdentifiers
import AppKit
import AgoClientCore

@main struct AgoDesktopApp: App {
    @StateObject private var model=DesktopModel()
    init(){if CommandLine.arguments.dropFirst().first=="--conformance"{let arguments=Array(CommandLine.arguments.dropFirst(2));Task{@MainActor in do{guard arguments.count==1 else{throw ConformanceError.invalidArguments};print(try await ProjectionConformance.run(projectionURL:arguments[0]));Foundation.exit(EXIT_SUCCESS)}catch{FileHandle.standardError.write(Data("\(error)\n".utf8));Foundation.exit(EXIT_FAILURE)}}}}
    var body: some Scene {
        WindowGroup { DesktopView(model:model) }.defaultSize(width: 1180, height: 760)
            .commands {
                CommandMenu("Ago") {
                    Button("Connect to Daemon"){Task{await model.connectDaemon()}}.keyboardShortcut("k",modifiers:[.command,.shift])
                    Button("Refresh Thread"){Task{await model.refreshCurrentThread()}}.keyboardShortcut("r",modifiers:.command).disabled(model.cursor==nil)
                    Divider()
                    Button("Add Attachment…"){model.fileImporterRequested=true}.keyboardShortcut("a",modifiers:[.command,.shift]).disabled(model.cursor==nil)
                    Button("Send Message"){Task{await model.submitDraft()}}.keyboardShortcut(.return,modifiers:.command).disabled(!model.canSubmitDraft)
                }
            }
    }
}

@MainActor final class DesktopModel: ObservableObject {
    @Published var baseURL="http://127.0.0.1:8080/"
    @Published var actorID="ago-desktop"
    @Published var search=""
    @Published var projects:[ProjectIdentity]=[]
    @Published var threads:[ThreadCatalogEntry]=[]
    @Published var selectedProjectID=""
    @Published var selectedThreadID=""
    @Published var status="Connect to Ago"
    @Published var error:String?
    @Published var cursor:ProjectionCursor?
    @Published var latestNotification:EventNotification?
    @Published var draft=""
    @Published var attachments:[ImmutableAttachment]=[]
    @Published var mentions:[RepositoryFileMention]=[]
    @Published var mentionInput=""
    @Published var fileImporterRequested=false
    private var cursors:[String:ProjectionCursor]=[:]
    private var mutations:MutationClient?
    private var messages:MessageClient?
    private var bundledDaemon:BundledDaemonSession?
    var canSubmitDraft:Bool{cursor != nil && (!draft.isEmpty || !attachments.isEmpty || !mentions.isEmpty)}

    func launchBundledDaemon()async {
        guard !CommandLine.arguments.contains("--conformance"),Bundle.main.bundleURL.pathExtension=="app",bundledDaemon==nil else{return}
        do{let plan=try BundleManifestLauncher.load();let session=try await BundleManifestLauncher.start(plan);bundledDaemon=session;baseURL=session.configuration.baseURL.absoluteString;status="Bundled daemon ready";await connectDaemon()}catch{fail(error)}
    }
    func terminateBundledDaemon(){bundledDaemon?.terminate();bundledDaemon=nil}
    private func httpConfiguration()->HTTPClientConfiguration?{guard let url=URL(string:baseURL)else{return nil};if let bundled=bundledDaemon?.configuration,bundled.baseURL==url{return bundled};return HTTPClientConfiguration(baseURL:url)}

    func connectDaemon()async {
        guard let configuration=httpConfiguration() else{return}; error=nil; status="Loading projects…"
        do {
            projects=try await HTTPCatalogTransport(configuration:configuration).fetchProjects()
            if !projects.contains(where:{$0.projectID==selectedProjectID}) { selectedProjectID=projects.first?.projectID ?? "" }
            try await refreshThreads(); status=projects.isEmpty ? "No projects" : "Connected"
        } catch { fail(error) }
    }
    func refreshThreads()async throws {
        guard let configuration=httpConfiguration(),!selectedProjectID.isEmpty else{threads=[];return}
        let transport=HTTPCatalogTransport(configuration:configuration);var all:[ThreadCatalogEntry]=[],cursor:String?
        repeat{let page=try await transport.fetchCatalog(ThreadCatalogRequest(projectID:selectedProjectID,search:search,cursor:cursor));all.append(contentsOf:page.threads);cursor=page.nextCursor}while cursor != nil
        threads=all
    }
    func selectProject(_ id:String)async { selectedProjectID=id; do{try await refreshThreads()}catch{fail(error)} }
    func open(_ threadID:String)async {
        guard let configuration=httpConfiguration(),!threadID.isEmpty else{return}
        selectedThreadID=threadID;latestNotification=nil;let cached=cursors[threadID] ?? ProjectionCursor(threadID:threadID); cursors[threadID]=cached; cursor=cached
        let previous=cached.sequence,projection=HTTPProjectionTransport(configuration:configuration)
        mutations=MutationClient(cursor:cached,mutationTransport:HTTPMutationTransport(configuration:configuration),projectionTransport:projection,actorID:actorID)
        messages=MessageClient(cursor:cached,transport:HTTPMessageTransport(configuration:configuration),projectionTransport:projection,actorID:actorID)
        status="Reconnecting…"; error=nil
        do {
            try await ReconnectController(cursor:cached,transport:projection).refresh()
            if previous>0 { for event in cached.events where event.sequence>previous { if let notice=EventProjection.notification(event){latestNotification=notice;await present(notice)} } }
            status="Connected · sequence \(cached.sequence)"
        } catch { fail(error) }
    }
    func mutate(_ action:@escaping (MutationClient)async throws->Void)async {
        guard let mutations else{return}; error=nil
        do { try await action(mutations); status="Connected · sequence \(cursor?.sequence ?? 0)"; try await refreshThreads() }
        catch MutationError.conflict { if let id=cursor?.threadID{await open(id)}; error="State changed on the daemon; refreshed the authoritative projection." }
        catch { fail(error) }
    }
    func archive()async { await mutate{try await $0.archive()}; if error==nil{cursor=nil;selectedThreadID=""} }
    func refreshCurrentThread()async { if let id=cursor?.threadID{await open(id)}else{await connectDaemon()} }
    func addAttachment(_ url:URL) {
        let accessed=url.startAccessingSecurityScopedResource()
        do {
            let type=UTType(filenameExtension:url.pathExtension)?.preferredMIMEType ?? "application/octet-stream"
            let attachment=try ImmutableAttachment.capture(url:url,mediaType:type)
            guard !attachments.contains(where:{$0.sourceURL==url}) else{if accessed{url.stopAccessingSecurityScopedResource()};return}
            attachments.append(attachment)
        } catch { if accessed{url.stopAccessingSecurityScopedResource()};fail(error) }
    }
    func removeAttachment(_ attachment:ImmutableAttachment){attachments.removeAll{$0.id==attachment.id};attachment.sourceURL.stopAccessingSecurityScopedResource()}
    func addMention(){do{let mention=try RepositoryFileMention(path:mentionInput);guard !mentions.contains(mention)else{return};mentions.append(mention);mentionInput=""}catch{fail(error)}}
    func submitDraft()async {
        guard let messages,canSubmitDraft else{return};error=nil
        do {
            try await messages.submit(text:draft,attachments:attachments,fileMentions:mentions)
            attachments.forEach{$0.sourceURL.stopAccessingSecurityScopedResource()};attachments=[];mentions=[];draft="";status="Connected · sequence \(cursor?.sequence ?? 0)";try await refreshThreads()
        } catch MutationError.conflict { if let id=cursor?.threadID{await open(id)};error="State changed on the daemon; refreshed the authoritative projection." }
        catch { fail(error) }
    }
    func enableNotifications()async { do{let granted=try await UNUserNotificationCenter.current().requestAuthorization(options:[.alert,.sound]);status=granted ? "Notifications enabled":"Notifications not enabled"}catch{fail(error)} }
    private func present(_ notice:EventNotification)async { let content=UNMutableNotificationContent();content.title=notice.title;content.body=notice.body;do{try await UNUserNotificationCenter.current().add(UNNotificationRequest(identifier:notice.tag,content:content,trigger:nil))}catch{} }
    private func fail(_ value:Error){error=String(describing:value);status="Disconnected"}
}

struct DesktopView:View {
    @ObservedObject var model:DesktopModel
    var body:some View {
        NavigationSplitView {
            VStack(spacing:10) {
                TextField("Daemon URL",text:$model.baseURL).textFieldStyle(.roundedBorder)
                TextField("Actor ID",text:$model.actorID).textFieldStyle(.roundedBorder)
                Button("Connect"){Task{await model.connectDaemon()}}.buttonStyle(.borderedProminent).frame(maxWidth:.infinity,alignment:.trailing)
                Picker("Project",selection:$model.selectedProjectID){Text("Choose a project").tag("");ForEach(model.projects,id:\.projectID){Text($0.displayName ?? $0.projectID).tag($0.projectID)}}.onChange(of:model.selectedProjectID){_,id in Task{await model.selectProject(id)}}
                TextField("Search threads",text:$model.search).textFieldStyle(.roundedBorder).onSubmit{Task{try? await model.refreshThreads()}}
                List(model.threads,selection:$model.selectedThreadID) { thread in Button{Task{await model.open(thread.threadID)}}label:{ThreadRow(thread:thread,selected:thread.threadID==model.selectedThreadID)}.buttonStyle(.plain).tag(thread.threadID) }
                HStack { Label(model.status,systemImage:model.error==nil ? "circle.fill":"exclamationmark.triangle").foregroundStyle(model.error==nil ? Color.secondary:Color.red);Spacer();Button("Alerts"){Task{await model.enableNotifications()}}.labelStyle(.iconOnly) }
                if let error=model.error{Text(error).font(.caption).foregroundStyle(.red).textSelection(.enabled)}
            }.padding().navigationTitle("Threads").navigationSplitViewColumnWidth(min:250,ideal:290)
        } detail: {
            if let cursor=model.cursor { ThreadDetail(cursor:cursor,model:model) }
            else { ContentUnavailableView("No Thread",systemImage:"text.bubble",description:Text("Choose a project thread from the daemon catalog.")) }
        }.task{await model.launchBundledDaemon()}.onReceive(NotificationCenter.default.publisher(for:NSApplication.willTerminateNotification)){_ in model.terminateBundledDaemon()}
    }
}

struct ThreadRow:View { let thread:ThreadCatalogEntry,selected:Bool; var body:some View { HStack { Circle().fill(activityColor).frame(width:8,height:8);VStack(alignment:.leading){Text(thread.title.isEmpty ? thread.threadID:thread.title).lineLimit(1);Text("\(selected ? "Active view":"Background") · \(thread.activity.rawValue)").font(.caption).foregroundStyle(.secondary)};Spacer();Text("#\(thread.lastSequence)").font(.caption).monospacedDigit() } }; var activityColor:Color{switch thread.activity{case .running:return .green;case .awaitingApproval:return .orange;case .error:return .red;case .idle:return .gray}} }

struct ThreadDetail:View {
    @ObservedObject var cursor:ProjectionCursor;@ObservedObject var model:DesktopModel
    @State private var interruptText=""
    @State private var queueEditor:QueueEditorItem?
    @State private var changeTarget:ChangeTarget?
    var pending:PluginDialog?{cursor.dialogs.first{$0.state=="pending"&&$0.requestedSequence<=cursor.sequence}}
    var body:some View { VStack(spacing:0) {
        HStack { VStack(alignment:.leading){Text(cursor.thread?.title ?? cursor.threadID).font(.title2).fontWeight(.semibold);Text(cursor.thread?.project.displayName ?? cursor.thread?.project.projectID ?? "").foregroundStyle(.secondary)};Spacer();Text(cursor.executor?.activity.capitalized ?? "Loading");Text("Sequence \(cursor.sequence)").monospacedDigit().foregroundStyle(.secondary);Button("Archive",role:.destructive){Task{await model.archive()}} }.padding()
        if let notice=model.latestNotification{HStack{Image(systemName:"bell.fill");VStack(alignment:.leading){Text(notice.title).bold();Text(notice.body)};Spacer()}.padding(10).background(.yellow.opacity(0.12))}
        Divider();HSplitView {
            VStack(spacing:0){List(EventProjection.timeline(cursor.events)){block in TimelineBlockView(block:block)}.navigationTitle("Transcript");Divider();MessageComposer(model:model)}
            List {
                UsagePanel(record:EventProjection.providerUsage(cursor.events))
                VerificationPanel(check:EventProjection.verification(cursor.events))
                DiffSections(cursor:cursor,model:model,changeTarget:$changeTarget)
                Section("Queue") { ForEach(cursor.mailbox?.queue ?? [],id:\.queueItemID){item in VStack(alignment:.leading,spacing:6){Text("\(item.class.capitalized) #\(item.position)").bold();Text(item.content.formatted).font(.caption.monospaced()).lineLimit(4);HStack{Button("Edit JSON"){queueEditor=QueueEditorItem(item:item)};Button("Dequeue",role:.destructive){Task{await model.mutate{try await $0.dequeue(queueItemID:item.queueItemID)}}};Button("Steer"){Task{await model.mutate{try await $0.steer(queueItemID:item.queueItemID)}}}.disabled(cursor.mailbox?.activeTurnID==nil)}}} }
                Section("Active turn") { TextField("Interrupt JSON text",text:$interruptText);Button("Interrupt",role:.destructive){let text=interruptText;Task{await model.mutate{try await $0.interrupt(content:.object(["text":.string(text)]))}}}.disabled(interruptText.isEmpty||cursor.mailbox?.activeTurnID==nil) }
            }.frame(minWidth:390,idealWidth:460)
        }
    }.sheet(item:$queueEditor){QueueEditorView(item:$0,model:model)}.sheet(item:$changeTarget){ChangeRequestView(target:$0,model:model)}.sheet(item:Binding(get:{pending.map(DialogPresentation.init)},set:{_ in})){DialogResolutionView(item:$0,model:model)}.fileImporter(isPresented:$model.fileImporterRequested,allowedContentTypes:[.data]){result in if case .success(let url)=result{model.addAttachment(url)}else if case .failure(let error)=result{model.error=String(describing:error)}} }
}

struct MessageComposer:View {
    @ObservedObject var model:DesktopModel
    var body:some View{VStack(alignment:.leading,spacing:8){
        if !model.attachments.isEmpty{ScrollView(.horizontal){HStack{ForEach(model.attachments){attachment in HStack{Image(systemName:"paperclip");Text(attachment.ref.filename);Text(ByteCountFormatter.string(fromByteCount:Int64(attachment.ref.sizeBytes),countStyle:.file)).foregroundStyle(.secondary);Button{model.removeAttachment(attachment)}label:{Image(systemName:"xmark.circle.fill")}.buttonStyle(.plain)}}}}}
        if !model.mentions.isEmpty{ScrollView(.horizontal){HStack{ForEach(model.mentions,id:\.path){mention in HStack{Image(systemName:"doc.text");Text(mention.path);Button{model.mentions.removeAll{$0==mention}}label:{Image(systemName:"xmark.circle.fill")}.buttonStyle(.plain)}}}}}
        HStack{TextField("Repository-relative file mention",text:$model.mentionInput).onSubmit{model.addMention()};Button("Mention"){model.addMention()}.disabled(model.mentionInput.isEmpty);Button("Attach…"){model.fileImporterRequested=true}}
        HStack(alignment:.bottom){TextEditor(text:$model.draft).frame(minHeight:54,maxHeight:120).overlay(RoundedRectangle(cornerRadius:6).stroke(.separator));Button("Send"){Task{await model.submitDraft()}}.keyboardShortcut(.return,modifiers:.command).buttonStyle(.borderedProminent).disabled(!model.canSubmitDraft)}
    }.padding(10)}
}

struct TimelineBlockView:View { let block:TimelineBlock; var body:some View { VStack(alignment:.leading,spacing:6){HStack{Label(label,systemImage:icon).font(.headline);Spacer();Text("#\(block.sequence)").font(.caption).monospacedDigit()};if block.kind == .thinking{DisclosureGroup("Thinking"){Text(block.text ?? "").textSelection(.enabled)}}else if let text=block.text{Text(text).textSelection(.enabled)}else if let detail=block.detail{Text(detail.formatted).font(.caption.monospaced()).textSelection(.enabled)}}.padding(.vertical,5) }; var label:String{switch block.kind{case .text:return block.streaming ? "Response · streaming":"Response";case .thinking:return "Thinking";case .toolCall:return "Tool call · \(block.title ?? "")";case .toolResult:return "Tool result · \(block.title ?? "")"}};var icon:String{switch block.kind{case .text:return "text.bubble";case .thinking:return "brain";case .toolCall:return "hammer";case .toolResult:return block.failed ? "xmark.circle":"checkmark.circle"}} }

struct UsagePanel:View { let record:ProviderUsageRecord?;var body:some View{Section("Usage"){if let record{LabeledContent("Provider",value:record.provider);LabeledContent("Model",value:record.model);LabeledContent("Status",value:record.status.rawValue);LabeledContent("Input tokens",value:String(describing:record.usage.inputTokens));LabeledContent("Output tokens",value:String(describing:record.usage.outputTokens));LabeledContent("Cache read",value:String(describing:record.usage.cacheReadTokens));LabeledContent("Cache write",value:String(describing:record.usage.cacheWriteTokens));LabeledContent("Total tokens",value:String(describing:record.usage.totalTokens));LabeledContent("Total cost",value:String(describing:record.cost.total))}else{Text("No authoritative provider usage event yet.").foregroundStyle(.secondary)}}} }

struct VerificationPanel:View { let check:VerificationCheck?;var body:some View{Section("Verification"){if let check{LabeledContent("Check",value:check.checkID);LabeledContent("Status",value:check.status.rawValue);Text(check.command).font(.caption.monospaced()).textSelection(.enabled);Text(check.outputSummary).foregroundStyle(.secondary).textSelection(.enabled)}else{Text("No authoritative verification check event yet.").foregroundStyle(.secondary)}}} }

struct DiffSections:View {
    @ObservedObject var cursor:ProjectionCursor;@ObservedObject var model:DesktopModel;@Binding var changeTarget:ChangeTarget?
    @ViewBuilder var body:some View {
        if let snapshot=cursor.diff?.snapshot {
            changes("Unstaged changes",snapshot.projection.unstaged,snapshot:snapshot,stage:true)
            changes("Staged changes",snapshot.projection.staged,snapshot:snapshot,stage:false)
            Section("Review comments") {
                ForEach(cursor.diff?.comments ?? [],id:\.commentID) { comment in
                    VStack(alignment:.leading) {
                        Text(comment.body)
                        Text("\(comment.actor) · \(comment.fileID)\(comment.hunkID.map { " · \($0)" } ?? "")").font(.caption).foregroundStyle(.secondary)
                    }
                }
            }
            Section("Ago writes") {
                ForEach(receiptIDs(cursor.events),id:\.self) { receipt in
                    HStack { Text(receipt).font(.caption).textSelection(.enabled);Spacer();Button("Revert",role:.destructive){Task{await model.mutate{try await $0.revert(snapshotRevision:snapshot.revision,snapshotDigest:snapshot.digest,receiptID:receipt)}}} }
                }
            }
        } else {
            Section("Diff") { Text("No authoritative Git snapshot yet.").foregroundStyle(.secondary) }
        }
    }
    @ViewBuilder func changes(_ title:String,_ changes:[GitChange],snapshot:GitSnapshot,stage:Bool)->some View { Section(title){ForEach(changes,id:\.id){change in VStack(alignment:.leading,spacing:6){HStack{VStack(alignment:.leading){Text(change.path).bold();Text(change.status).foregroundStyle(.secondary)};Spacer();Button(stage ? "Stage":"Unstage"){Task{await model.mutate{if stage{try await $0.stage(snapshotRevision:snapshot.revision,snapshotDigest:snapshot.digest,unitIDs:[change.id])}else{try await $0.unstage(snapshotRevision:snapshot.revision,snapshotDigest:snapshot.digest,unitIDs:[change.id])}}}}.disabled(change.protected || !change.mutationSupported);Button("Request change"){changeTarget=ChangeTarget(snapshot:snapshot,file:change,hunk:nil)}};ForEach(change.hunks,id:\.id){hunk in DisclosureGroup("\(hunk.header) · occurrence \(hunk.occurrence)"){Text(hunk.patch).font(.caption.monospaced()).textSelection(.enabled);Button("Request change on hunk"){changeTarget=ChangeTarget(snapshot:snapshot,file:change,hunk:hunk)}}}}}} }
}

struct QueueEditorItem:Identifiable { let item:QueueItem;var id:String{item.queueItemID} }
struct QueueEditorView:View { let item:QueueEditorItem;@ObservedObject var model:DesktopModel;@Environment(\.dismiss) var dismiss;@State private var text:String;@State private var error:String?;init(item:QueueEditorItem,model:DesktopModel){self.item=item;self.model=model;_text=State(initialValue:item.item.content.formatted)};var body:some View{VStack(alignment:.leading,spacing:12){Text("Edit Queue JSON").font(.title2);TextEditor(text:$text).font(.body.monospaced()).frame(minHeight:260).border(.separator);if let error{Text(error).foregroundStyle(.red)};HStack{Spacer();Button("Cancel"){dismiss()};Button("Save"){do{let content=try JSONValue.parse(text);Task{await model.mutate{try await $0.edit(queueItemID:item.item.queueItemID,content:content)};if model.error==nil{dismiss()}}}catch{self.error="Queue content must be valid JSON."}}.buttonStyle(.borderedProminent)}}.padding().frame(minWidth:560,minHeight:380)} }

struct ChangeTarget:Identifiable { let snapshot:GitSnapshot,file:GitChange,hunk:GitHunk?;var id:String{"\(file.id)-\(hunk?.id ?? "file")"} }
struct ChangeRequestView:View { let target:ChangeTarget;@ObservedObject var model:DesktopModel;@Environment(\.dismiss) var dismiss;@State private var bodyText="";var body:some View{VStack(alignment:.leading,spacing:14){Text("Request Change").font(.title2);Text(target.file.path).bold();if let hunk=target.hunk{Text("\(hunk.header) · occurrence \(hunk.occurrence)").font(.caption);Text(hunk.patch).font(.caption.monospaced()).frame(maxHeight:180).textSelection(.enabled)};TextEditor(text:$bodyText).frame(minHeight:100).border(.separator);HStack{Spacer();Button("Cancel"){dismiss()};Button("Submit"){Task{await model.mutate{try await $0.requestChange(snapshotRevision:target.snapshot.revision,snapshotDigest:target.snapshot.digest,fileID:target.file.id,hunkID:target.hunk?.id,body:bodyText)};if model.error==nil{dismiss()}}}.disabled(bodyText.trimmingCharacters(in:.whitespacesAndNewlines).isEmpty).buttonStyle(.borderedProminent)}}.padding().frame(minWidth:600)} }

struct DialogPresentation:Identifiable { let dialog:PluginDialog;var id:String{"\(dialog.dialogID)-\(dialog.revision)-\(dialog.requestedSequence)"} }
struct DialogResolutionView:View { let item:DialogPresentation;@ObservedObject var model:DesktopModel;@State private var value="";var body:some View{VStack(alignment:.leading,spacing:16){Text("Action Required").font(.title2);Text(item.dialog.request.formatted);if item.dialog.requestType=="confirm"{HStack{Button("Confirm"){resolve(.ok(.bool(true)))};Button("Decline"){resolve(.ok(.bool(false)))}}}else{TextField(item.dialog.requestType=="select" ? "Selection":"Response",text:$value);Button(item.dialog.requestType=="select" ? "Select":"Submit"){resolve(.ok(.string(value)))}.disabled(value.isEmpty)};Button("Cancel",role:.cancel){resolve(.cancelled)};Text("Revision \(item.dialog.revision) · sequence \(item.dialog.requestedSequence)").font(.caption).foregroundStyle(.secondary)}.padding().frame(minWidth:420)};private func resolve(_ response:UIResult){Task{await model.mutate{try await $0.resolve(item.dialog,response:response)}}} }

private func receiptIDs(_ events:[AgoEvent])->[String]{Array(events.compactMap{event in guard event.type=="git.write-receipt-recorded",let id=event.payload.objectValue?["receipt_id"]?.stringValue else{return nil};return id}.suffix(20).reversed())}
