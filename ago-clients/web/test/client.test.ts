import {expect,test} from "bun:test";
import {AgoClient,ConflictError} from "../src/client";
import {jsonEditorText,parseJSONEditor,runMutation,visibleDiff} from "../src/app";
import type {Json} from "../src/projection";
test("mutation uses one ID for command and idempotency and surfaces conflict",async()=>{let body:any;const client=new AgoClient("https://ago.test",async(_u,init)=>{body=JSON.parse(String(init?.body));return new Response(JSON.stringify({winner:"other"}),{status:409});});await expect(client.interrupt("t","turn",9,{text:"stop"})).rejects.toBeInstanceOf(ConflictError);expect(body).toMatchObject({expected_sequence:9});expect(body.command_id).toBe(body.idempotency_key);});

test("dialog resolution sends only the daemon's strict request fields", async () => {
  let body: Record<string, unknown> = {};
  const client = new AgoClient("https://ago.test", async (_url, init) => {
    body = JSON.parse(String(init?.body));
    return new Response(JSON.stringify({state: "resolved"}), {status: 202});
  });

  await client.resolveDialog("thread", "dialog", 3, 11, {status:"ok",value:true});

  expect(body).toEqual({
    resolver_id: "web-pwa",
    expected_revision: 3,
    expected_sequence: 11,
    response: {status:"ok",value:true},
  });
});
test("cancelled dialog result has no value", async () => {
  let body: Record<string, unknown> = {};
  const client = new AgoClient("https://ago.test", async (_url, init) => {
    body = JSON.parse(String(init?.body));
    return new Response("{}", {status:202});
  });
  await client.resolveDialog("thread", "dialog", 3, 11, {status:"cancelled"});
  expect(body.response).toEqual({status:"cancelled"});
  expect(body.response).not.toHaveProperty("value");
});
test("stage sends only snapshot fences and opaque server unit IDs", async () => {
  let url = "", body: Record<string, unknown> = {};
  const client = new AgoClient("https://ago.test", async (request, init) => {
    url = String(request); body = JSON.parse(String(init?.body));
    return new Response(JSON.stringify({operation:{state:"completed"}}), {status: 200});
  });
  await client.stage("thread/one", 12, 4, "a".repeat(64), ["unit-1"]);
  expect(url).toBe("https://ago.test/v1/threads/thread%2Fone/diff/stage");
  expect(body).toMatchObject({actor_id:"web-pwa", expected_sequence:12, expected_snapshot_revision:4, expected_snapshot_digest:"a".repeat(64), selected_unit_ids:["unit-1"]});
  expect(body.command_id).toBe(body.idempotency_key);
  expect(body).not.toHaveProperty("path"); expect(body).not.toHaveProperty("patch"); expect(body).not.toHaveProperty("workspace");
});
test("revert sends only a durable receipt and snapshot fences", async () => {
  let body: Record<string, unknown> = {};
  const client = new AgoClient("https://ago.test", async (_request, init) => { body=JSON.parse(String(init?.body)); return new Response("{}",{status:200}); });
  await client.revert("thread",12,4,"a".repeat(64),"R-one");
  expect(body).toMatchObject({receipt_id:"R-one",expected_sequence:12,expected_snapshot_revision:4,expected_snapshot_digest:"a".repeat(64)});
  expect(body).not.toHaveProperty("path"); expect(body).not.toHaveProperty("patch"); expect(body).not.toHaveProperty("selected_unit_ids");
});
test("visible diff exposes only authoritative opaque units and fences", () => {
  expect(visibleDiff({revision:7,digest:"d",projection:{staged:[{id:"s1",path:"a.ts",status:"M",protected:false,mutation_supported:true}],unstaged:[{id:"u1",path:"b.ts",status:"A",protected:true,mutation_supported:false}]}})).toEqual({revision:7,digest:"d",staged:[{id:"s1",path:"a.ts",status:"M",protected:false,mutation_supported:true}],unstaged:[{id:"u1",path:"b.ts",status:"A",protected:true,mutation_supported:false}]});
  expect(visibleDiff({revision:7,digest:"d",projection:null})).toBeNull();
});
test("visible diff keeps opaque hunk identities and disambiguates duplicate headers", () => {
  expect(visibleDiff({revision:8,digest:"digest",projection:{staged:[],unstaged:[{
    id:"file:opaque",path:"src/a.ts",status:"modified",protected:false,mutation_supported:true,patch:"file patch",
    hunks:[
      {id:"hunk:first",header:"@@ -1 +1 @@ function same",patch:"@@ -1 +1 @@ function same\n-old one\n+new one"},
      {id:"hunk:second",header:"@@ -1 +1 @@ function same",patch:"@@ -8 +8 @@ function same\n-old two\n+new two"},
    ],
  }]}})?.unstaged[0]).toEqual({
    id:"file:opaque",path:"src/a.ts",status:"modified",protected:false,mutation_supported:true,patch:"file patch",
    hunks:[
      {id:"hunk:first",header:"@@ -1 +1 @@ function same",patch:"@@ -1 +1 @@ function same\n-old one\n+new one",header_occurrence:1,header_occurrences:2},
      {id:"hunk:second",header:"@@ -1 +1 @@ function same",patch:"@@ -8 +8 @@ function same\n-old two\n+new two",header_occurrence:2,header_occurrences:2},
    ],
  });
  expect(visibleDiff({revision:8,digest:"digest",projection:{staged:[],unstaged:[{id:"file",path:"a",status:"M",protected:false,mutation_supported:true,hunks:[{id:"hunk",header:"@@ section"}]}]}})?.unstaged[0]?.hunks?.[0]).toEqual({id:"hunk",header:"@@ section",header_occurrence:1,header_occurrences:1});
});
test("diff change requests send exact snapshot fences and opaque target IDs", async () => {
  let url="", body:Record<string,unknown>={};
  const client=new AgoClient("https://ago.test",async(request,init)=>{url=String(request);body=JSON.parse(String(init?.body));return new Response("{}",{status:202})});
  await client.requestDiffChange("thread/one",13,5,"a".repeat(64),{file_id:"file:opaque",hunk_id:"hunk:second"},"Change this occurrence");
  expect(url).toBe("https://ago.test/v1/threads/thread%2Fone/diff/comments");
  expect(body).toMatchObject({expected_sequence:13,snapshot_revision:5,snapshot_digest:"a".repeat(64),file_id:"file:opaque",hunk_id:"hunk:second",actor_id:"web-pwa",body:"Change this occurrence"});
  expect(typeof body.comment_id).toBe("string");
  expect(body).not.toHaveProperty("path"); expect(body).not.toHaveProperty("patch"); expect(body).not.toHaveProperty("selected_unit_ids");
});
test("stale mutation conflicts refresh before reporting the conflict", async () => {
  const order:string[]=[];
  const result=await runMutation(async()=>{order.push("mutate");throw new ConflictError({current_sequence:14})},async()=>{order.push("refresh")});
  expect(order).toEqual(["mutate","refresh"]);
  expect(result).toBe("Conflict: State changed on the server; refreshed the latest snapshot.");
});
test("queue JSON editor preserves objects, scalars, and strings and rejects invalid JSON", () => {
  for (const value of [{text:"hello"},[1,true],42,false,null,"plain string"] as Json[]) {
    expect(parseJSONEditor(jsonEditorText(value))).toEqual(value);
  }
  expect(() => parseJSONEditor("{not json}" )).toThrow("valid JSON");
});
test("project and thread discovery use explicit query endpoints", async () => {
  const requests: string[] = [];
  const client = new AgoClient("https://ago.test", async request => {
    requests.push(String(request));
    return new Response(requests.length === 1
      ? JSON.stringify({threads:[{thread_id:"t",last_sequence:2,title:"Thread",workspace:"/x",project:{project_id:"p",display_name:"Project"}}]})
      : JSON.stringify({schema_version:1,threads:[{thread_id:"t",project_id:"p/one",last_sequence:2,title:"Thread",workspace:"/x",activity:"idle",archived:false}]}));
  });

  expect(await client.listProjects()).toEqual([{project_id:"p",display_name:"Project"}]);
  expect(await client.listThreads({project_id:"p/one",query:"needle",archived:false})).toHaveLength(1);
  expect(requests).toEqual([
    "https://ago.test/v1/threads",
    "https://ago.test/v1/threads?project_id=p%2Fone&search=needle&archive=active&limit=100",
  ]);
});
test("archive and composed messages preserve strict authoritative DTOs", async () => {
  const calls: Array<{url:string;method?:string;body:Record<string,unknown>}> = [];
  const client = new AgoClient("https://ago.test", async (request, init) => {
    calls.push({url:String(request),method:init?.method,body:JSON.parse(String(init?.body))});
    return new Response("{}", {status:202});
  });
  await client.submitMessage("t/1", 8, {
    text:"Review this",
    attachments:[{attachment_id:"attachment-1",sha256:"a".repeat(64),filename:"report.pdf",media_type:"application/pdf",size_bytes:42}],
    file_mentions:[{path:"src/app.ts"}],
  });
  await client.archiveThread("t/1", 9);

  expect(calls[0]).toMatchObject({url:"https://ago.test/v1/threads/t%2F1/messages",method:"POST",body:{actor_id:"web-pwa",expected_sequence:8,class:"normal",content:{text:"Review this",attachments:[{attachment_id:"attachment-1",sha256:"a".repeat(64),filename:"report.pdf",media_type:"application/pdf",size_bytes:42}],file_mentions:[{path:"src/app.ts"}]}}});
  expect(calls[1]).toMatchObject({url:"https://ago.test/v1/threads/t%2F1/archive",method:"POST",body:{actor_id:"web-pwa",expected_sequence:9}});
});
test("source contains no provider or model selection UI/config",async()=>{
  const app=(await Bun.file(new URL("../src/app.ts",import.meta.url)).text()).toLowerCase();
  const html=(await Bun.file(new URL("../index.html",import.meta.url)).text()).toLowerCase();
  expect(app).not.toMatch(/model|provider[-_](select|switch|config)|provider\s*:/);
  expect(html).not.toMatch(/provider|model/);
});
