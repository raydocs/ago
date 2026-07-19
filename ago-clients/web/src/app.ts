import {ProjectionStore} from "./store";
import {AgoClient,ConflictError,type ComposerDTO,type ProjectSummary,type ThreadSummary} from "./client";
import {authoritativeRecord,notificationFromEvent,timelineItems,type Dialog,type EventRecord,type Json,type QueueItem,type TimelineItem} from "./projection";
import {createWebTransport,type WebTransportConfig} from "./transport";

export const EVENT_RENDER_LIMIT=200;
export const visibleEventWindow=(events:readonly EventRecord[])=>events.slice(-EVENT_RENDER_LIMIT);
export const visibleTimelineWindow=(items:readonly TimelineItem[])=>items.slice(-EVENT_RENDER_LIMIT);
export const jsonEditorText=(value:Json)=>JSON.stringify(value,null,2);
export function parseJSONEditor(value:string):Json{try{return JSON.parse(value) as Json}catch{throw Error("Queue content must be valid JSON.")}}
export async function runMutation(action:()=>Promise<unknown>,refresh:()=>Promise<unknown>):Promise<string>{
 try{await action();await refresh();return "Connected"}catch(error){
  if(error instanceof ConflictError){await refresh();return "Conflict: State changed on the server; refreshed the latest snapshot."}
  return error instanceof Error?error.message:String(error);
 }
}

type DiffHunk={id:string;header:string;patch?:string;header_occurrence:number;header_occurrences:number};
type DiffUnit={id:string;path:string;status:string;protected:boolean;mutation_supported:boolean;patch?:string;hunks?:DiffHunk[]};
export type VisibleDiff={revision:number;digest:string;staged:DiffUnit[];unstaged:DiffUnit[]};
export function visibleDiff(snapshot:Json|null):VisibleDiff|null{
 if(!snapshot||typeof snapshot!=="object"||Array.isArray(snapshot))return null;
 const s=snapshot as Record<string,Json>,projection=s.projection;
 if(typeof s.revision!=="number"||typeof s.digest!=="string"||!projection||typeof projection!=="object"||Array.isArray(projection))return null;
 const p=projection as Record<string,Json>;
 const units=(value:Json|undefined):DiffUnit[]=>Array.isArray(value)?value.flatMap(item=>{
  if(!item||typeof item!=="object"||Array.isArray(item))return[];
  const unit=item as Record<string,Json>;
  if(typeof unit.id!=="string"||typeof unit.path!=="string"||typeof unit.status!=="string"||typeof unit.protected!=="boolean"||typeof unit.mutation_supported!=="boolean")return[];
  const rawHunks=Array.isArray(unit.hunks)?unit.hunks.flatMap(hunk=>{
   if(!hunk||typeof hunk!=="object"||Array.isArray(hunk))return[];
   const candidate=hunk as Record<string,Json>;
   return typeof candidate.id==="string"&&typeof candidate.header==="string"?[{id:candidate.id,header:candidate.header,...(typeof candidate.patch==="string"?{patch:candidate.patch}:{})}]:[];
  }):[];
  const totals=new Map<string,number>();
  for(const hunk of rawHunks)totals.set(hunk.header,(totals.get(hunk.header)??0)+1);
  const seen=new Map<string,number>();
  const hunks:DiffHunk[]=rawHunks.map(hunk=>{const occurrence=(seen.get(hunk.header)??0)+1;seen.set(hunk.header,occurrence);return{...hunk,header_occurrence:occurrence,header_occurrences:totals.get(hunk.header)!}});
  return[{id:unit.id,path:unit.path,status:unit.status,protected:unit.protected,mutation_supported:unit.mutation_supported,...(typeof unit.patch==="string"?{patch:unit.patch}:{}),...(hunks.length?{hunks}:{})}];
 }):[];
 return{revision:s.revision,digest:s.digest,staged:units(p.staged),unstaged:units(p.unstaged)};
}

const $=<T extends Element>(selector:string)=>document.querySelector<T>(selector)!;
const text=(value:Json|undefined)=>typeof value==="string"?value:JSON.stringify(value,null,2);
const escape=(value:string|undefined)=>String(value??"").replace(/[&<>"']/g,character=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[character]!));
let store:ProjectionStore|undefined,api:AgoClient|undefined,thread="",threads:ThreadSummary[]=[],projects:ProjectSummary[]=[];
const loadedThreads=new Set<string>();
let notificationsLive=false;
let sessionTransport:WebTransportConfig|undefined,activeTransportMode:"direct"|"relay"="direct";
export function configureWebSession(config:WebTransportConfig|undefined){sessionTransport=config}

async function connectThread(nextThread:string){
 if(!store||!api||!nextThread)return;
 thread=nextThread;
 ($<HTMLInputElement>("#thread")).value=thread;
 status("Connecting…");
 notificationsLive=loadedThreads.has(thread);
 try{
  await store.connect(thread);
  loadedThreads.add(thread);
  render();
  status("Connected");
 }catch(error){status(error instanceof Error?error.message:String(error))}finally{notificationsLive=false}
}

async function loadSwitcher(){
 if(!api)return;
 try{
  projects=await api.listProjects();
  const projectSelect=$<HTMLSelectElement>("#project-switch");
  const selected=projectSelect.value;
  projectSelect.replaceChildren(option("","All projects"),...projects.map(project=>option(project.project_id,project.display_name||project.project_id)));
  projectSelect.value=selected;
  await searchThreads();
 }catch(error){status(`Switcher unavailable: ${error instanceof Error?error.message:String(error)}`)}
}

async function searchThreads(){
 if(!api)return;
 try{
  const filter=switcherFilter();
  if(filter.project_id)threads=await api.listThreads(filter);
  else if(projects.length)threads=(await Promise.all(projects.map(project=>api!.listThreads({...filter,project_id:project.project_id})))).flat();
  else threads=await api.listThreads();
  renderThreadOptions();
 }catch(error){status(error instanceof Error?error.message:String(error))}
}

function switcherFilter(){return{project_id:$<HTMLSelectElement>("#project-switch").value||undefined,query:$<HTMLInputElement>("#thread-search").value.trim()||undefined,archived:false}}
function option(value:string,label:string){const node=document.createElement("option");node.value=value;node.textContent=label;return node}
function renderThreadOptions(){const select=$<HTMLSelectElement>("#thread-switch");select.replaceChildren(option("","Choose a thread"),...threads.map(item=>option(item.thread_id,item.title||item.thread_id)));select.value=thread}
function status(value:string){$("#status").textContent=value}

function render(){
 const projection=store?.current;
 if(!projection)return;
 $("#title").textContent=projection.thread.title||projection.thread.thread_id;
 $("#project").textContent=projection.thread.project.display_name||projection.thread.project.project_id;
 $("#activity").textContent=projection.executor.activity;
 const executor=$("#executor");
 executor.textContent=`Executor: ${projection.executor.target.type}${projection.executor.target.runner_id?` · ${projection.executor.target.runner_id}`:""}`;
 if(projection.executor.active_turn_id){const button=document.createElement("button");button.textContent="Interrupt";button.onclick=()=>act(()=>api!.interrupt(thread,projection.executor.active_turn_id!,projection.snapshot_sequence,{reason:"user_requested"}));executor.append(" · ",button)}
 renderFacts();
 renderDiff();
 $("#timeline ol").replaceChildren(...visibleTimelineWindow(timelineItems(projection.events)).map(timelineRow));
 $("#queue div").replaceChildren(...projection.mailbox.queue.map(queueRow));
 $("#dialogs div").replaceChildren(...projection.dialogs.filter(item=>item.state==="pending").map(dialogRow));
}

function renderFacts(){
 const events=store!.current!.events;
 const renderRecord=(selector:string,record:ReturnType<typeof authoritativeRecord>,empty:string)=>{const root=$(selector);if(!record){root.textContent=empty;return}root.replaceChildren(document.createTextNode(`Event #${record.sequence}`),jsonBlock(record.value))};
 renderRecord("#verification div",authoritativeRecord(events,"verification.check-recorded"),"No authoritative verification check event yet.");
 renderRecord("#usage div",authoritativeRecord(events,"provider.usage-recorded"),"No authoritative provider usage event yet.");
}

function timelineRow(item:TimelineItem){
 const row=document.createElement("li");
 row.className=`event ${item.kind}${"failed" in item&&item.failed?" failed":""}`;
 const sequence=document.createElement("small");sequence.textContent=`#${item.sequence}`;
 if(item.kind==="thinking"){
  const details=document.createElement("details"),summary=document.createElement("summary");summary.textContent="Thinking";details.append(summary,jsonBlock(item.text));row.append(sequence,details);
 }else if(item.kind==="text"){
  const label=document.createElement("b");label.textContent=item.streaming?"Response · streaming":"Response";row.append(label,sequence,jsonBlock(item.text));
 }else{
  const label=document.createElement("b");label.textContent=`${item.kind==="tool-call"?"Tool call":"Tool result"} · ${item.title}`;
  const details=document.createElement("details"),summary=document.createElement("summary");summary.textContent=item.kind==="tool-call"?"Input":"Output";details.append(summary,jsonBlock(item.detail));row.append(label,sequence,details);
 }
 return row;
}

function jsonBlock(value:Json){const pre=document.createElement("pre");pre.textContent=text(value);return pre}

type VisibleDiffComment={comment_id:string;file_id:string;hunk_id?:string;actor:string;body:string};
function visibleDiffComments(values:Json[]):VisibleDiffComment[]{return values.flatMap(value=>{
 if(!value||typeof value!=="object"||Array.isArray(value))return[];
 const comment=value as Record<string,Json>;
 if(typeof comment.comment_id!=="string"||typeof comment.file_id!=="string"||typeof comment.actor!=="string"||typeof comment.body!=="string")return[];
 return[{comment_id:comment.comment_id,file_id:comment.file_id,...(typeof comment.hunk_id==="string"&&comment.hunk_id?{hunk_id:comment.hunk_id}:{}),actor:comment.actor,body:comment.body}];
})}

function renderDiff(){
 const projection=store!.current!,snapshot=visibleDiff(projection.diff.snapshot),root=$("#diff div");
 if(!snapshot){root.textContent="No Git snapshot yet.";return}
 const comments=visibleDiffComments(projection.diff.comments);
 const commentForm=(fileID:string,hunkID?:string)=>{
  const form=document.createElement("form");form.className="change-request";
  const existing=comments.filter(comment=>comment.file_id===fileID&&comment.hunk_id===hunkID);
  if(existing.length){const list=document.createElement("ul");list.className="diff-comments";for(const comment of existing){const item=document.createElement("li");item.innerHTML=`<b>${escape(comment.actor)}</b> ${escape(comment.body)}`;list.append(item)}form.append(list)}
  const label=document.createElement("label");label.textContent=hunkID?"Request a change to this hunk":"Request a change to this file";
  const input=document.createElement("textarea");input.rows=2;input.placeholder="Describe the requested change";label.append(input);
  const button=document.createElement("button");button.type="submit";button.textContent="Add change request";form.append(label,button);
  form.onsubmit=async event=>{event.preventDefault();const body=input.value.trim();if(!body)return;await act(()=>api!.requestDiffChange(thread,store!.current!.snapshot_sequence,snapshot.revision,snapshot.digest,{file_id:fileID,...(hunkID?{hunk_id:hunkID}:{})},body))};
  return form;
 };
 const group=(title:string,kind:"stage"|"unstage",units:DiffUnit[])=>{
  const field=document.createElement("fieldset");const legend=document.createElement("legend");legend.textContent=title;field.append(legend);
  if(!units.length){const empty=document.createElement("p");empty.className="muted";empty.textContent="No changes.";field.append(empty)}
  for(const unit of units){
   const file=document.createElement("article");file.className="diff-file";file.dataset.fileId=unit.id;
   const heading=document.createElement("header"),label=document.createElement("label"),checkbox=document.createElement("input");checkbox.type="checkbox";checkbox.value=unit.id;checkbox.disabled=unit.protected||!unit.mutation_supported;
   label.append(checkbox," ");const name=document.createElement("b");name.textContent=unit.path;const state=document.createElement("small");state.textContent=`${unit.status}${unit.protected?" · protected":""}`;label.append(name," ",state);heading.append(label);file.append(heading);
   if(unit.patch){const details=document.createElement("details"),summary=document.createElement("summary");summary.textContent="File patch";details.append(summary,jsonBlock(unit.patch));file.append(details)}
   for(const hunk of unit.hunks??[]){
    const section=document.createElement("section");section.className="diff-hunk";section.dataset.hunkId=hunk.id;
    const hunkLabel=document.createElement("label"),hunkCheckbox=document.createElement("input");hunkCheckbox.type="checkbox";hunkCheckbox.value=hunk.id;hunkCheckbox.disabled=checkbox.disabled;hunkLabel.append(hunkCheckbox," Select hunk");section.append(hunkLabel);
    const details=document.createElement("details");details.open=true;const summary=document.createElement("summary");const duplicate=hunk.header_occurrences>1?` · duplicate header occurrence ${hunk.header_occurrence} of ${hunk.header_occurrences}`:"";summary.textContent=`${hunk.header}${duplicate}`;
    const context=document.createElement("pre");context.className=hunk.patch?"patch":"patch unavailable";context.textContent=hunk.patch??"Patch context unavailable for this snapshot.";details.append(summary,context);section.append(details,commentForm(unit.id,hunk.id));file.append(section);
   }
   file.append(commentForm(unit.id));field.append(file);
  }
  const button=document.createElement("button");button.type="button";button.textContent=kind==="stage"?"Stage selected":"Unstage selected";button.onclick=()=>{const ids=Array.from(field.querySelectorAll<HTMLInputElement>("input[type=checkbox]:checked"),input=>input.value);if(ids.length)act(()=>api![kind](thread,projection.snapshot_sequence,snapshot.revision,snapshot.digest,ids))};field.append(button);return field;
 };
 const receipts=projection.events.flatMap(event=>{if(event.type!=="git.write-receipt-recorded"||!event.payload||typeof event.payload!=="object"||Array.isArray(event.payload))return[];const id=(event.payload as Record<string,Json>).receipt_id;return typeof id==="string"?[id]:[]}).slice(-20).reverse();
 const receiptBox=document.createElement("fieldset");receiptBox.innerHTML="<legend>Ago writes</legend>";for(const id of receipts){const row=document.createElement("p");row.innerHTML=`<code>${escape(id)}</code> `;const button=document.createElement("button");button.textContent="Revert";button.onclick=()=>act(()=>api!.revert(thread,projection.snapshot_sequence,snapshot.revision,snapshot.digest,id));row.append(button);receiptBox.append(row)}
 root.replaceChildren(group("Unstaged","stage",snapshot.unstaged),group("Staged","unstage",snapshot.staged),receiptBox);
}
function queueRow(item:QueueItem){const form=document.createElement("form");form.innerHTML=`<label>${escape(item.class)} #${item.position}<textarea>${escape(jsonEditorText(item.content))}</textarea></label><button name="edit">Save</button><button name="dequeue">Dequeue</button><button name="steer">Steer</button>`;form.onclick=async event=>{const name=(event.target as HTMLButtonElement).name;if(!name)return;event.preventDefault();const projection=store!.current!;if(name==="edit"){let content:Json;try{content=parseJSONEditor(form.querySelector("textarea")!.value)}catch(error){status(error instanceof Error?error.message:String(error));return}await act(()=>api!.editQueue(thread,item.queue_item_id,projection.snapshot_sequence,content));return}await act(()=>name==="dequeue"?api!.dequeue(thread,item.queue_item_id,projection.snapshot_sequence):api!.steer(thread,item.queue_item_id,projection.snapshot_sequence,projection.mailbox.active_turn_id!))};return form}
function dialogRow(dialog:Dialog){const form=document.createElement("form"),request=dialog.request as Record<string,Json>;const options=dialog.request_type==="select"&&Array.isArray(request.options)?`<select>${request.options.map(value=>`<option>${escape(String(value))}</option>`).join("")}</select>`:dialog.request_type==="input"?`<input aria-label="Response">`:`<input type="checkbox" aria-label="Confirm">`;form.innerHTML=`<p><b>${escape(dialog.plugin_id)}</b> asks to ${escape(dialog.request_type)}</p>${options}<button name="resolve">Resolve</button><button name="cancel">Cancel</button>`;form.onsubmit=async event=>{event.preventDefault();const cancelled=(event.submitter as HTMLButtonElement|null)?.name==="cancel",input=form.querySelector<HTMLInputElement|HTMLSelectElement>("input,select")!,value=dialog.request_type==="confirm"?(input as HTMLInputElement).checked:input.value,response=cancelled?{status:"cancelled" as const}:{status:"ok" as const,value};await act(()=>api!.resolveDialog(thread,dialog.dialog_id,dialog.revision,store!.current!.snapshot_sequence,response))};return form}

async function act(action:()=>Promise<unknown>){status(await runMutation(action,()=>connectThread(thread)))}

function composerDTO(form:HTMLFormElement):ComposerDTO{
 const value=(name:string)=>form.querySelector<HTMLInputElement|HTMLTextAreaElement>(`[name="${name}"]`)!.value.trim();
 const attachment=value("attachment"),mention=value("mention");
 return{text:value("message"),attachments:attachment?[{attachment_id:attachment,sha256:value("sha256"),filename:value("filename"),media_type:value("media-type"),size_bytes:Number(value("size"))}]:[],file_mentions:mention?[{path:mention}]:[]};
}

async function enableNotifications(){if(!("Notification" in window)){status("System notifications are unavailable.");return}const permission=await Notification.requestPermission();status(permission==="granted"?"Notifications enabled":"Notifications not enabled")}
function notifyFrom(event:EventRecord){const notification=notificationsLive?notificationFromEvent(event):null;if(notification&&"Notification" in window&&Notification.permission==="granted")new Notification(notification.title,{body:notification.body,tag:notification.tag})}

function start(){
 $("#connect").addEventListener("submit",async event=>{event.preventDefault();const directBase=$<HTMLInputElement>("#base").value.replace(/\/$/,"");const transport=createWebTransport(sessionTransport??{mode:"direct",baseUrl:directBase});activeTransportMode=transport.mode;api=new AgoClient(transport.baseUrl,transport.fetcher);store=new ProjectionStore({baseUrl:transport.baseUrl,fetcher:transport.fetcher,...(transport.mode==="direct"?{storage:localStorage}:{})});const requested=$<HTMLInputElement>("#thread").value.trim();await connectThread(requested);store.subscribe(notifyFrom);if(transport.mode==="direct")await loadSwitcher()});
 $("#thread-switch").addEventListener("change",event=>connectThread((event.target as HTMLSelectElement).value));
 $("#project-switch").addEventListener("change",searchThreads);
 $("#thread-search").addEventListener("input",searchThreads);
 $("#refresh-threads").addEventListener("click",loadSwitcher);
 $("#archive").addEventListener("click",async()=>{if(!store?.current)return;await act(()=>api!.archiveThread(thread,store!.current!.snapshot_sequence));if(activeTransportMode==="direct")await searchThreads()});
 $("#enable-notifications").addEventListener("click",enableNotifications);
 $<HTMLFormElement>("#composer").addEventListener("submit",async event=>{event.preventDefault();const form=event.currentTarget as HTMLFormElement;if(!store?.current)return;const dto=composerDTO(form);if(!dto.text&&!dto.attachments.length&&!dto.file_mentions.length)return;await act(()=>api!.submitMessage(thread,store!.current!.snapshot_sequence,dto));form.reset()});
 window.addEventListener("online",()=>thread&&connectThread(thread));
 window.addEventListener("offline",()=>status("Offline — reconnect will resume from the in-memory transcript"));
 if("serviceWorker"in navigator)navigator.serviceWorker.register("/sw.js");
}
if(typeof document!=="undefined")start();
