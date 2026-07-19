import {describe,expect,test} from "bun:test";
import {authoritativeRecord, notificationFromEvent, parseProjection, timelineItems} from "../src/projection";
import type {EventRecord,Json} from "../src/projection";
import {ProjectionStore,type CursorStorage} from "../src/store";

const event=(sequence:number, payload:Json={x:1}):EventRecord=>({schema_version:1,event_id:`e${sequence}`,thread_id:"t",sequence,type:"future.event",visibility:"shared",payload});
const page=(after:number,events=after<2?[event(after+1)]:[],more=after<1)=>({schema_version:1,thread:{thread_id:"t",last_sequence:2,title:"Thread",workspace:"/x",mode:"default",executor:{type:"local",runner_id:"r"},project:{project_id:"p",display_name:"Project"},agent:{definition_id:"a",version:"1",display_name:"Agent",default_mode:"default"}},mailbox:{thread_id:"t",last_sequence:2,activity:"idle",cancel_requested:false,queue:[]},events,dialogs:[],diff:{snapshot:null,comments:[]},requested_after_sequence:after,next_after_sequence:events.at(-1)?.sequence??after,snapshot_sequence:2,has_more:more,plugins:{available:true,generation:3,registrations:[{pluginId:"plug",tools:[{name:"search",description:"Search",inputSchema:{type:"object"}}],commands:[{id:"run",title:"Run"}],hooks:[]}]},executor:{target:{type:"local",runner_id:"r"},activity:"idle"}});

describe("schema-v1 projection",()=>{
 test("preserves opaque event type and payload",()=>expect(parseProjection(page(0)).events[0]).toMatchObject({type:"future.event",payload:{x:1}}));
 test("accepts production plugin and executor projections with deterministic optional arrays",()=>{
  const parsed=parseProjection({...page(0),plugins:{available:true,generation:7,registrations:[{pluginId:"acme"}]}});
  expect(parsed.plugins.registrations[0]).toEqual({pluginId:"acme",tools:[],commands:[],hooks:[]});
  expect(parsed.executor).toEqual({target:{type:"local",runner_id:"r"},activity:"idle"});
 });
 test("rejects unknown top-level, wrong schema, malformed state, and cursor contradictions",()=>{
  expect(()=>parseProjection({...page(0),surprise:true})).toThrow();
  expect(()=>parseProjection({...page(0),schema_version:2})).toThrow();
  expect(()=>parseProjection({...page(0),mailbox:{...page(0).mailbox,queue:[{queue_item_id:"q"}]}})).toThrow();
  expect(()=>parseProjection({...page(0),dialogs:[{dialog_id:"d",state:"pending"}]})).toThrow();
  expect(()=>parseProjection({...page(0),next_after_sequence:9})).toThrow();
  expect(()=>parseProjection({...page(0),plugins:{available:true,generation:1,registrations:[{plugin_id:"bad"}]}})).toThrow();
  expect(()=>parseProjection({...page(0),executor:{target:{type:"other"},activity:"idle"}})).toThrow();
  expect(()=>parseProjection({...page(0),executor:{target:{type:"local",runner_id:"r"},activity:"running",active_turn_id:"turn"}})).toThrow();
 });
});

describe("projection store",()=>{
 test("drains pagination and emits each sequence once despite stale/out-of-order pages",async()=>{
  const calls:number[]=[]; const seen:number[]=[];
  const fetcher=async (input:string|URL|Request)=>{const after=Number(new URL(String(input)).searchParams.get("after"));calls.push(after);return new Response(JSON.stringify(page(after)),{status:200});};
  const store=new ProjectionStore({baseUrl:"https://ago.test",fetcher,storage:{getItem:()=>null,setItem:()=>{}}});store.subscribe(e=>seen.push(e.sequence));
  await store.connect("t"); await store.connect("t");
  expect(calls).toEqual([0,1,2]); expect(seen).toEqual([1,2]); expect(store.current?.events.map(e=>e.sequence)).toEqual([1,2]);
 });
 test("ignores a bare persisted cursor on cold reload",async()=>{
  const memory=new Map<string,string>(); const storage:CursorStorage={getItem:k=>memory.get(k)??null,setItem:(k,v)=>{memory.set(k,v)}};
  const starts:number[]=[]; const fetcher=async (input:string|URL|Request)=>{const u=new URL(String(input));const after=Number(u.searchParams.get("after"));starts.push(after);return new Response(JSON.stringify(page(after)),{status:200});};
  memory.set("ago:cursor:t","1"); await new ProjectionStore({baseUrl:"",fetcher,storage}).connect("t");
  expect(starts[0]).toBe(0);
 });
});

describe("authoritative UX event projection", () => {
 test("renders completed text and thinking once while keeping tool calls and results distinct", () => {
  const events:EventRecord[] = [
   {...event(1),type:"assistant.text-delta",payload:{turn_id:"turn",event:{type:"text",delta:"draft answer"}}},
   {...event(2),type:"assistant.completed",payload:{turn_id:"turn",event:{type:"assistant_completed",message:{content:[{type:"thinking",thinking:"checked constraints"},{type:"text",text:"final answer"},{type:"toolCall",callId:"call",name:"read",input:{path:"README.md"}}]}}}},
   {...event(3),type:"tool.requested",payload:{turn_id:"turn",event:{type:"tool_invocation",callId:"call",name:"read",input:{path:"README.md"}}}},
   {...event(4),type:"tool.completed",payload:{turn_id:"turn",call_id:"call",name:"read",output:"contents",error:false}},
  ];
  expect(timelineItems(events)).toEqual([
   {kind:"thinking",sequence:2,text:"checked constraints"},
   {kind:"text",sequence:2,text:"final answer"},
   {kind:"tool-call",sequence:3,title:"read",detail:{path:"README.md"},call_id:"call"},
   {kind:"tool-result",sequence:4,title:"read",detail:"contents",call_id:"call",failed:false},
  ]);
 });
 test("coalesces live text deltas until an authoritative completion arrives", () => {
  expect(timelineItems([
   {...event(1),type:"assistant.text-delta",payload:{turn_id:"turn",event:{delta:"hel"}}},
   {...event(2),type:"assistant.text-delta",payload:{turn_id:"turn",event:{delta:"lo"}}},
  ])).toEqual([{kind:"text",sequence:2,text:"hello",streaming:true}]);
 });
 test("provider usage, verification checks, and notifications are sourced only from exact durable event names", () => {
  const ordinary={...event(1),type:"assistant.completed",payload:{usage:{input:99,cost:{total:9}},verification:{status:"passed"},title:"ignore",body:"ignore"}};
  const usage={...event(2),type:"provider.usage-recorded",payload:{usage:{input:11,output:7,totalTokens:18,cost:{total:0.0123,currency:"USD"}}}};
  const verification={...event(3),type:"verification.check-recorded",payload:{status:"passed",command:"bun test",summary:"24 pass"}};
  const notification={...event(4),type:"notification.created",payload:{title:"Input needed",body:"Confirm the operation"}};
  expect(authoritativeRecord([ordinary,usage],"provider.usage-recorded")).toEqual({sequence:2,value:{usage:{input:11,output:7,totalTokens:18,cost:{total:0.0123,currency:"USD"}}}});
  expect(authoritativeRecord([ordinary,verification],"verification.check-recorded")).toEqual({sequence:3,value:{status:"passed",command:"bun test",summary:"24 pass"}});
  expect(notificationFromEvent(ordinary)).toBeNull();
  expect(notificationFromEvent(notification)).toEqual({title:"Input needed",body:"Confirm the operation",tag:"e4"});
 });
});
