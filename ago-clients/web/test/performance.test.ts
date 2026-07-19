import {describe,expect,test} from "bun:test";
import type {EventRecord,Projection} from "../src/projection";
import {ProjectionStore} from "../src/store";

const BUDGET={initialMs:2_000,reconnectMs:250,accumulationMs:4_000,cpuMs:3_000,heapBytes:64*1024*1024,linearRatio:3.25,visibleEvents:200} as const;
const event=(sequence:number):EventRecord=>({schema_version:1,event_id:`event-${sequence}`,thread_id:"thread",sequence,type:"message",visibility:"shared",payload:{text:`message ${sequence}`}});
const projection=(after:number,next:number,snapshot:number,events:EventRecord[],hasMore:boolean):Projection=>({
 schema_version:1,
 thread:{thread_id:"thread",last_sequence:snapshot,title:"Performance",workspace:"/x",mode:"default",executor:{type:"local"},project:{project_id:"project",display_name:"Project"},agent:{definition_id:"agent",version:"1",display_name:"Agent",default_mode:"default"}},
 mailbox:{thread_id:"thread",last_sequence:snapshot,activity:"idle",cancel_requested:false,queue:[]},
 events,dialogs:[],diff:{snapshot:null,comments:[]},requested_after_sequence:after,next_after_sequence:next,snapshot_sequence:snapshot,has_more:hasMore,
 plugins:{available:false,generation:0,registrations:[]},executor:{target:{type:"local"},activity:"idle"},
});
const response=(value:Projection)=>({ok:true,status:200,json:async()=>value}) as Response;
const elapsedMs=(start:number)=>performance.now()-start;
const cpuMs=(start:NodeJS.CpuUsage)=>{const used=process.cpuUsage(start);return (used.user+used.system)/1_000};

describe("5,000-message projection performance",()=>{
 test("drains five 1,000-event pages within frozen time, CPU, and heap budgets",async()=>{
  Bun.gc(true);
  const heapStart=process.memoryUsage().heapUsed,cpuStart=process.cpuUsage(),start=performance.now(),afters:number[]=[],heard:number[]=[];
  const store=new ProjectionStore({baseUrl:"https://ago.test",limit:1_000,fetcher:async input=>{
   const after=Number(new URL(String(input)).searchParams.get("after"));
   afters.push(after);
   const next=Math.min(after+1_000,5_000),events=Array.from({length:next-after},(_,index)=>event(after+index+1));
   return response(projection(after,next,5_000,events,next<5_000));
  }});
  store.subscribe(value=>heard.push(value.sequence));

  await store.connect("thread");
  const elapsed=elapsedMs(start),cpu=cpuMs(cpuStart);
  Bun.gc(true);
  const heap=process.memoryUsage().heapUsed-heapStart;
  console.info(`[performance] initial5000=${elapsed.toFixed(2)}ms cpu=${cpu.toFixed(2)}ms heap=${heap}B budgets=${BUDGET.initialMs}ms/${BUDGET.cpuMs}ms/${BUDGET.heapBytes}B`);

  expect(afters).toEqual([0,1_000,2_000,3_000,4_000]);
  expect(store.current?.events).toHaveLength(5_000);
  expect(heard).toHaveLength(5_000);
  expect(new Set(heard).size).toBe(5_000);
  expect(elapsed).toBeLessThan(BUDGET.initialMs);
  expect(cpu).toBeLessThan(BUDGET.cpuMs);
  expect(heap).toBeLessThan(BUDGET.heapBytes);
 });

 test("reconnects from 5,000 without copying data, duplicating events, or notifying listeners",async()=>{
  const afters:number[]=[],heard:number[]=[],writes:string[]=[];
  const store=new ProjectionStore({baseUrl:"https://ago.test",limit:1_000,storage:{getItem:()=>null,setItem:(_key,value)=>writes.push(value)},fetcher:async input=>{
   const after=Number(new URL(String(input)).searchParams.get("after"));
   afters.push(after);
   const next=Math.min(after+1_000,5_000),events=Array.from({length:next-after},(_,index)=>event(after+index+1));
   return response(projection(after,next,5_000,events,next<5_000));
  }});
  store.subscribe(value=>heard.push(value.sequence));
  await store.connect("thread");
  const transcript=store.current!.events,start=performance.now();

  await store.connect("thread");
  const reconnect=elapsedMs(start);
  console.info(`[performance] reconnect5000=${reconnect.toFixed(2)}ms budget=${BUDGET.reconnectMs}ms`);

  expect(reconnect).toBeLessThan(BUDGET.reconnectMs);
  expect(afters.at(-1)).toBe(5_000);
  expect(store.current!.events).toBe(transcript);
  expect(store.current!.events).toHaveLength(5_000);
  expect(heard).toHaveLength(5_000);
  expect(writes.at(-1)).toBe("5000");
 });

 test("retains the complete transcript while exposing only a bounded visible tail",async()=>{
  const app=await import("../src/app").catch(()=>({}));
  const visible=(app as {visibleEventWindow?:(events:EventRecord[])=>EventRecord[]}).visibleEventWindow;
  const events=Array.from({length:5_000},(_,index)=>event(index+1));

  expect(visible).toBeFunction();
  const window=visible!(events);
  expect(events).toHaveLength(5_000);
  expect(window).toHaveLength(BUDGET.visibleEvents);
  expect(window[0]?.sequence).toBe(4_801);
  expect(window.at(-1)?.sequence).toBe(5_000);
 });

 test("incremental accumulation scales linearly rather than quadratically",async()=>{
  const run=async(count:number)=>{
   const store=new ProjectionStore({baseUrl:"https://ago.test",fetcher:async input=>{
    const after=Number(new URL(String(input)).searchParams.get("after")),next=Math.min(after+20,count);
    return response(projection(after,next,next,Array.from({length:next-after},(_,index)=>event(after+index+1)),false));
   }});
   const start=performance.now();
   for(let index=0;index<count;index+=20)await store.connect("thread");
   expect(store.current?.events).toHaveLength(count);
   return elapsedMs(start);
  };
  await run(250);
  const small=await run(2_500),large=await run(5_000);
  const ratio=large/Math.max(small,1);
  console.info(`[performance] accumulation2500=${small.toFixed(2)}ms accumulation5000=${large.toFixed(2)}ms ratio=${ratio.toFixed(2)} budgets=${BUDGET.accumulationMs}ms/${BUDGET.linearRatio}x`);

  expect(large).toBeLessThan(BUDGET.accumulationMs);
  expect(ratio).toBeLessThan(BUDGET.linearRatio);
 });
});
