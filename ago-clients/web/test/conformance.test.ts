import {describe,expect,test} from "bun:test";
import {conformanceEndpoint,projectionConformance} from "../src/conformance";

const page=(overrides:Record<string,unknown>={})=>({
  schema_version:1,
  thread:{thread_id:"thread/one",last_sequence:2,title:"Thread",workspace:"/x",mode:"default",executor:{type:"local"},project:{project_id:"project"},agent:{definition_id:"agent",version:"1",display_name:"Agent",default_mode:"default"}},
  mailbox:{thread_id:"thread/one",last_sequence:2,activity:"idle",cancel_requested:false,queue:[{queue_item_id:"queue-1",position:0,class:"normal",state:"pending",content:{text:"next"}}]},
  events:[{schema_version:1,event_id:"event-1",thread_id:"thread/one",sequence:1,type:"message.accepted",visibility:"shared",payload:{content:{text:"hello"}}},{schema_version:1,event_id:"event-2",thread_id:"thread/one",sequence:2,type:"provider.usage-recorded",visibility:"shared",payload:{input_tokens:4}}],
  dialogs:[{dialog_id:"dialog-1",thread_id:"thread/one",turn_id:"turn-1",plugin_id:"plugin",generation:1,invocation_id:"invocation",deadline:"2026-07-19T00:00:00Z",request_type:"confirm",request:{prompt:"Continue?"},state:"pending",revision:1,requested_sequence:2}],
  diff:{snapshot:null,comments:[]},requested_after_sequence:0,next_after_sequence:2,snapshot_sequence:2,has_more:false,
  plugins:{available:false,generation:0,registrations:[]},executor:{target:{type:"local"},activity:"idle"},
  ...overrides,
});

describe("Web projection conformance entrypoint",()=>{
  test("accepts one credential-free daemon projection URL and derives store identity",()=>{
    expect(conformanceEndpoint(["https://daemon.test/api/v1/threads/thread%2Fone/projection"])).toEqual({baseUrl:"https://daemon.test/api",threadId:"thread/one"});
    for(const args of [[],["https://daemon.test/v1/threads/t/projection","extra"],["https://user:secret@daemon.test/v1/threads/t/projection"],["file:///v1/threads/t/projection"],["https://daemon.test/v1/threads/t/projection?after=2"],["https://daemon.test/v1/threads/t/messages"]])expect(()=>conformanceEndpoint(args)).toThrow();
  });

  test("uses ProjectionStore and emits deterministic component and whole-projection digests",async()=>{
    const requests:string[]=[];
    const fetcher=async(input:string|URL|Request)=>{requests.push(String(input));return new Response(JSON.stringify(page()),{status:200})};
    const first=await projectionConformance("https://daemon.test/api/v1/threads/thread%2Fone/projection",fetcher);
    const second=await projectionConformance("https://daemon.test/api/v1/threads/thread%2Fone/projection",fetcher);

    expect(requests[0]).toBe("https://daemon.test/api/v1/threads/thread%2Fone/projection?after=0&limit=200");
    expect(first).toEqual(second);
    expect(first.snapshot_sequence).toBe(2);
    expect(first.mailbox).toMatchObject({thread_id:"thread/one",last_sequence:2,activity:"idle",cancel_requested:false});
    expect(first.queue).toMatchObject({count:1});
    expect(first.dialogs).toMatchObject({count:1});
    expect(first.diff).toMatchObject({has_snapshot:false,comment_count:0});
    expect(first.events).toMatchObject({count:2,first_sequence:1,last_sequence:2});
    for(const digest of [first.digest,first.queue.digest,first.dialogs.digest,first.diff.digest,first.events.digest])expect(digest).toMatch(/^[a-f0-9]{64}$/);
  });

  test("rejects malformed daemon projections through the production parser",async()=>{
    await expect(projectionConformance("https://daemon.test/v1/threads/thread%2Fone/projection",async()=>new Response(JSON.stringify({...page(),snapshot_sequence:99}),{status:200}))).rejects.toThrow("projection contradiction");
  });
});
