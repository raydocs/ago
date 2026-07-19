export type Json = null|boolean|number|string|Json[]|{[key:string]:Json};
export interface EventRecord {schema_version:number;event_id:string;thread_id:string;sequence:number;type:string;visibility:string;provenance?:Json;payload?:Json}
export interface QueueItem {queue_item_id:string;position:number;class:string;state:string;content:Json}
export interface Dialog {dialog_id:string;thread_id:string;turn_id:string;plugin_id:string;generation:number;invocation_id:string;deadline:string;request_type:"confirm"|"input"|"select";request:Json;state:"pending"|"resolved";revision:number;requested_sequence:number;resolved_sequence?:number;resolver_id?:string;response?:Json}
export interface ExecutorTarget {type:string;runner_id?:string}
export interface ToolRegistration {name:string;description:string;inputSchema:Json}
export interface CommandRegistration {id:string;title:string;category?:string;description?:string}
export interface PluginRegistration {pluginId:string;tools:ToolRegistration[];commands:CommandRegistration[];hooks:string[]}
export interface Projection {schema_version:1;thread:{thread_id:string;last_sequence:number;title:string;workspace:string;mode:string;executor:ExecutorTarget;project:{project_id:string;display_name?:string};agent:{definition_id:string;version:string;display_name:string;default_mode:string;capabilities?:string[]};provenance?:Json};mailbox:{thread_id:string;last_sequence:number;activity:string;active_turn_id?:string;cancel_requested:boolean;queue:QueueItem[];events?:EventRecord[]};events:EventRecord[];dialogs:Dialog[];diff:{snapshot:Json|null;comments:Json[]};requested_after_sequence:number;next_after_sequence:number;snapshot_sequence:number;has_more:boolean;plugins:{available:boolean;generation:number;registrations:PluginRegistration[]};executor:{target:ExecutorTarget;activity:string;active_turn_id?:string}}
export type TimelineItem=
 | {kind:"text";sequence:number;text:string;streaming?:true}
 | {kind:"thinking";sequence:number;text:string}
 | {kind:"tool-call";sequence:number;title:string;detail:Json;call_id:string}
 | {kind:"tool-result";sequence:number;title:string;detail:Json;call_id:string;failed:boolean};
const obj=(v:unknown,n:string):Record<string,unknown>=>{if(!v||typeof v!=="object"||Array.isArray(v))throw Error(`${n} must be an object`);return v as Record<string,unknown>};
const exact=(o:Record<string,unknown>,keys:string[],n:string)=>{if(Object.keys(o).some(k=>!keys.includes(k)))throw Error(`unknown ${n} field`)};
const str=(o:Record<string,unknown>,k:string,optional=false)=>{const v=o[k];if(optional&&v===undefined)return undefined;if(typeof v!=="string"||(!optional&&!v))throw Error(`${k} must be a non-empty string`);return v};
const num=(o:Record<string,unknown>,k:string)=>{const v=o[k];if(!Number.isSafeInteger(v)||Number(v)<0)throw Error(`${k} must be an unsigned integer`);return Number(v)};
const json=(v:unknown,n:string):Json=>{if(v===undefined)throw Error(`${n} missing`);try{JSON.stringify(v)}catch{throw Error(`${n} invalid`)}return v as Json};
const strings=(v:unknown,n:string)=>{if(!Array.isArray(v)||!v.every(x=>typeof x==="string"))throw Error(`${n} must be an array of strings`);return [...v] as string[]};
const target=(v:unknown)=>{const o=obj(v,"executor target");exact(o,["type","runner_id"],"executor target");return {type:str(o,"type")!,...(o.runner_id===undefined?{}:{runner_id:str(o,"runner_id",true)})}};
const sameTarget=(a:ExecutorTarget,b:ExecutorTarget)=>a.type===b.type&&a.runner_id===b.runner_id;
const event=(v:unknown):EventRecord=>{const o=obj(v,"event");if(num(o,"schema_version")!==1)throw Error("event schema");return {schema_version:1,event_id:str(o,"event_id")!,thread_id:str(o,"thread_id")!,sequence:num(o,"sequence"),type:str(o,"type")!,visibility:str(o,"visibility")!,...(o.provenance===undefined?{}:{provenance:json(o.provenance,"provenance")}),...(o.payload===undefined?{}:{payload:json(o.payload,"payload")})}};
export function parseProjection(value:unknown):Projection {
 const o=obj(value,"projection");exact(o,["schema_version","thread","mailbox","events","dialogs","diff","requested_after_sequence","next_after_sequence","snapshot_sequence","has_more","plugins","executor"],"projection");if(o.schema_version!==1)throw Error("unsupported projection schema");
 const t=obj(o.thread,"thread"),threadTarget=target(t.executor),pr=obj(t.project,"project"),ag=obj(t.agent,"agent");const thread:Projection["thread"]={thread_id:str(t,"thread_id")!,last_sequence:num(t,"last_sequence"),title:str(t,"title",true)??"",workspace:str(t,"workspace",true)??"",mode:str(t,"mode")!,executor:threadTarget,project:{project_id:str(pr,"project_id")!,...(pr.display_name===undefined?{}:{display_name:str(pr,"display_name",true)})},agent:{definition_id:str(ag,"definition_id")!,version:str(ag,"version")!,display_name:str(ag,"display_name")!,default_mode:str(ag,"default_mode")!,...(ag.capabilities===undefined?{}:{capabilities:strings(ag.capabilities,"capabilities")})},...(t.provenance===undefined?{}:{provenance:json(t.provenance,"provenance")})};
 const m=obj(o.mailbox,"mailbox");if(!Array.isArray(m.queue)||typeof m.cancel_requested!=="boolean")throw Error("malformed mailbox");const queue=m.queue.map(v=>{const q=obj(v,"queue item");return {queue_item_id:str(q,"queue_item_id")!,position:num(q,"position"),class:str(q,"class")!,state:str(q,"state")!,content:json(q.content,"queue content")}});const mailbox:Projection["mailbox"]={thread_id:str(m,"thread_id")!,last_sequence:num(m,"last_sequence"),activity:str(m,"activity")!,cancel_requested:m.cancel_requested,queue,...(m.active_turn_id===undefined?{}:{active_turn_id:str(m,"active_turn_id",true)}),...(m.events===undefined?{}:{events:Array.isArray(m.events)?m.events.map(event):(()=>{throw Error("mailbox events")})()})};
 if(!Array.isArray(o.events)||!Array.isArray(o.dialogs)||typeof o.has_more!=="boolean")throw Error("malformed projection collections");const events=o.events.map(event);const dialogs=o.dialogs.map(v=>{const d=obj(v,"dialog"),rt=str(d,"request_type")!,state=str(d,"state")!;if(!["confirm","input","select"].includes(rt)||!["pending","resolved"].includes(state))throw Error("malformed dialog");return {dialog_id:str(d,"dialog_id")!,thread_id:str(d,"thread_id")!,turn_id:str(d,"turn_id")!,plugin_id:str(d,"plugin_id")!,generation:num(d,"generation"),invocation_id:str(d,"invocation_id")!,deadline:str(d,"deadline")!,request_type:rt,state,request:json(d.request,"dialog request"),revision:num(d,"revision"),requested_sequence:num(d,"requested_sequence"),...(d.resolved_sequence===undefined?{}:{resolved_sequence:num(d,"resolved_sequence")}),...(d.resolver_id===undefined?{}:{resolver_id:str(d,"resolver_id",true)}),...(d.response===undefined?{}:{response:json(d.response,"dialog response")})} as Dialog});
 const diffObject=obj(o.diff,"diff");exact(diffObject,["snapshot","comments"],"diff");if(diffObject.snapshot!==null&&(typeof diffObject.snapshot!=="object"||Array.isArray(diffObject.snapshot))||!Array.isArray(diffObject.comments))throw Error("malformed diff");const diff={snapshot:json(diffObject.snapshot,"diff snapshot"),comments:diffObject.comments.map((value,index)=>json(value,`diff comment ${index}`))};
 const po=obj(o.plugins,"plugins");exact(po,["available","generation","registrations"],"plugins");if(typeof po.available!=="boolean"||!Array.isArray(po.registrations))throw Error("malformed plugins");const registrations=po.registrations.map(v=>{const r=obj(v,"plugin registration");exact(r,["pluginId","tools","commands","hooks"],"plugin registration");const tools=(r.tools===undefined?[]:Array.isArray(r.tools)?r.tools:(()=>{throw Error("tools must be an array")})()).map(v=>{const x=obj(v,"tool");exact(x,["name","description","inputSchema"],"tool");return {name:str(x,"name")!,description:str(x,"description",true)??"",inputSchema:json(x.inputSchema,"inputSchema")}});const commands=(r.commands===undefined?[]:Array.isArray(r.commands)?r.commands:(()=>{throw Error("commands must be an array")})()).map(v=>{const x=obj(v,"command");exact(x,["id","title","category","description"],"command");return {id:str(x,"id")!,title:str(x,"title")!,...(x.category===undefined?{}:{category:str(x,"category",true)}),...(x.description===undefined?{}:{description:str(x,"description",true)})}});return {pluginId:str(r,"pluginId")!,tools,commands,hooks:r.hooks===undefined?[]:strings(r.hooks,"hooks")}});
 const eo=obj(o.executor,"executor");exact(eo,["target","activity","active_turn_id"],"executor");const executor:Projection["executor"]={target:target(eo.target),activity:str(eo,"activity")!,...(eo.active_turn_id===undefined?{}:{active_turn_id:str(eo,"active_turn_id",true)})};
 const requested=num(o,"requested_after_sequence"),next=num(o,"next_after_sequence"),snapshot=num(o,"snapshot_sequence");if(next<requested||next>snapshot||thread.last_sequence!==snapshot||mailbox.last_sequence!==snapshot||mailbox.thread_id!==thread.thread_id||!sameTarget(executor.target,thread.executor)||executor.activity!==mailbox.activity||executor.active_turn_id!==mailbox.active_turn_id||events.some((e,i)=>e.thread_id!==thread.thread_id||e.sequence<=requested||e.sequence>next||(i>0&&events[i-1]!.sequence>=e.sequence))||(events.length?events.at(-1)!.sequence!==next:next!==requested)||o.has_more&&next>=snapshot)throw Error("projection contradiction");
 return {schema_version:1,thread,mailbox,events,dialogs,diff,requested_after_sequence:requested,next_after_sequence:next,snapshot_sequence:snapshot,has_more:o.has_more,plugins:{available:po.available,generation:num(po,"generation"),registrations},executor};
}

const record=(value:Json|undefined):Record<string,Json>|undefined=>value!==null&&typeof value==="object"&&!Array.isArray(value)?value:undefined;
const turnID=(event:EventRecord)=>{const payload=record(event.payload);return typeof payload?.turn_id==="string"?payload.turn_id:undefined};

export function timelineItems(events:readonly EventRecord[]):TimelineItem[]{
 const completedTurns=new Set(events.filter(event=>event.type==="assistant.completed").map(turnID).filter((value):value is string=>!!value));
 const items:TimelineItem[]=[];
 const streams=new Map<string,{sequence:number;text:string}>();
 for(const event of events){
  const payload=record(event.payload);
  if(!payload)continue;
  if(event.type==="assistant.text-delta"){
   const turn=turnID(event)??event.event_id;
   if(completedTurns.has(turn))continue;
   const nested=record(payload.event),delta=nested?.delta;
   if(typeof delta!=="string")continue;
   const current=streams.get(turn);
   streams.set(turn,{sequence:event.sequence,text:(current?.text??"")+delta});
   continue;
  }
  if(event.type==="assistant.completed"){
   const nested=record(payload.event),message=record(nested?.message),content=message?.content;
   if(!Array.isArray(content))continue;
   for(const value of content){
    const block=record(value);
    if(!block||typeof block.type!=="string")continue;
    if(block.type==="text"&&typeof block.text==="string")items.push({kind:"text",sequence:event.sequence,text:block.text});
    if(block.type==="thinking"&&typeof block.thinking==="string")items.push({kind:"thinking",sequence:event.sequence,text:block.thinking});
   }
   continue;
  }
  if(event.type==="tool.requested"){
   const nested=record(payload.event);
   if(nested&&typeof nested.callId==="string"&&typeof nested.name==="string"&&nested.input!==undefined)items.push({kind:"tool-call",sequence:event.sequence,title:nested.name,detail:nested.input,call_id:nested.callId});
   continue;
  }
  if(event.type==="tool.completed"||event.type==="tool.failed"){
   if(typeof payload.call_id==="string"&&typeof payload.name==="string"&&payload.output!==undefined)items.push({kind:"tool-result",sequence:event.sequence,title:payload.name,detail:payload.output,call_id:payload.call_id,failed:event.type==="tool.failed"||payload.error===true});
   continue;
  }
  if(event.type==="message.accepted"){
   const content=record(payload.content);
   if(typeof content?.text==="string")items.push({kind:"text",sequence:event.sequence,text:content.text});
  }
 }
 for(const stream of streams.values())if(stream.text)items.push({kind:"text",sequence:stream.sequence,text:stream.text,streaming:true});
 items.sort((a,b)=>a.sequence-b.sequence);
 return items;
}

export function authoritativeRecord(events:readonly EventRecord[],type:"provider.usage-recorded"|"verification.check-recorded"):{sequence:number;value:Json}|null{
 for(let index=events.length-1;index>=0;index--){const event=events[index]!;if(event.type===type&&event.payload!==undefined)return{sequence:event.sequence,value:event.payload}}
 return null;
}

export function notificationFromEvent(event:EventRecord):{title:string;body:string;tag:string}|null{
 if(!["notification.created","plugin.notification.created","plugin.notification.requested"].includes(event.type))return null;
 const payload=record(event.payload);
 if(!payload||typeof payload.title!=="string"||typeof payload.body!=="string")return null;
 return{title:payload.title,body:payload.body,tag:event.event_id};
}
