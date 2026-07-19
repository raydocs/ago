import {describe,expect,test} from "bun:test";
import {AgoClient,ConflictError} from "../src/client";
import {ProjectionStore,type Fetcher} from "../src/store";
import {createWebTransport} from "../src/transport";

const projection=()=>({schema_version:1,thread:{thread_id:"thread-1",last_sequence:1,title:"Relay",workspace:"remote",mode:"default",executor:{type:"remote"},project:{project_id:"project-1"},agent:{definition_id:"agent",version:"1",display_name:"Agent",default_mode:"default"}},mailbox:{thread_id:"thread-1",last_sequence:1,activity:"running",active_turn_id:"turn-1",cancel_requested:false,queue:[]},events:[{schema_version:1,event_id:"event-1",thread_id:"thread-1",sequence:1,type:"message.accepted",visibility:"shared",payload:{content:{text:"hello"}}}],dialogs:[],diff:{snapshot:null,comments:[]},requested_after_sequence:0,next_after_sequence:1,snapshot_sequence:1,has_more:false,plugins:{available:false,generation:0,registrations:[]},executor:{target:{type:"remote"},activity:"running",active_turn_id:"turn-1"}});

const json=(value:unknown,status=200)=>new Response(JSON.stringify(value),{status,headers:{"content-type":"application/json"}});

describe("browser relay transport",()=>{
  test("retries an uncertain enqueue with the exact nonce/body then polls pending result into ProjectionStore",async()=>{
    const enqueues:string[]=[],requests:string[]=[];let enqueueCalls=0,polls=0;
    const relay:Fetcher=async(input,init)=>{
      requests.push(String(input));
      expect(new Headers(init?.headers).get("authorization")).toBe("Bearer session-secret");
      if(init?.method==="POST"){
        const body=String(init.body);enqueues.push(body);enqueueCalls++;
        if(enqueueCalls===1)throw Error("connection reset after relay accepted request");
        return json({sequence:7,nonce:JSON.parse(body).nonce},202);
      }
      polls++;
      if(polls<3)return json({sequence:7,pending:true},202);
      const nonce=JSON.parse(enqueues[0]!).nonce;
      return json({sequence:7,nonce,account_id:"account-1",device_id:"device-1",payload:projection()});
    };
    const transport=createWebTransport({mode:"relay",relayUrl:"https://relay.test",bearerToken:"session-secret",projectId:"project-1",fetcher:relay,polling:{maxAttempts:4,minDelayMs:0,maxDelayMs:0},nonce:()=>"nonce-fixed"});
    const store=new ProjectionStore({baseUrl:transport.baseUrl,fetcher:transport.fetcher});

    await store.connect("thread-1");

    expect(store.current?.snapshot_sequence).toBe(1);
    expect(enqueues).toHaveLength(2);
    expect(enqueues[0]).toBe(enqueues[1]);
    expect(JSON.parse(enqueues[0]!)).toEqual({nonce:"nonce-fixed",project_id:"project-1",thread_id:"thread-1",action:"thread.projection",payload:{after_sequence:0,limit:200}});
    expect(requests.filter(value=>value.includes("/v1/relay/results?sequence=7"))).toHaveLength(3);
  });

  test("maps submit and archive to exact production bridge actions",async()=>{
    const bodies:Record<string,unknown>[]=[];
    const relay:Fetcher=async(_input,init)=>{
      if(init?.method==="POST"){const body=JSON.parse(String(init.body));bodies.push(body);return json({sequence:bodies.length,nonce:body.nonce},202)}
      const sequence=Number(new URL(String(_input)).searchParams.get("sequence")),body=bodies[sequence-1]!;
      return json({sequence,nonce:body.nonce,account_id:"account",device_id:"device",payload:{accepted:true}});
    };
    let nonce=0;const transport=createWebTransport({mode:"relay",relayUrl:"https://relay.test",bearerToken:"runtime-only",projectId:"project-1",fetcher:relay,polling:{maxAttempts:1,minDelayMs:0,maxDelayMs:0},nonce:()=>`nonce-${++nonce}`,authorization:async context=>`grant:${context.action}`});
    const client=new AgoClient(transport.baseUrl,transport.fetcher);
    await client.submitMessage("thread-1",8,{text:"Review",attachments:[],file_mentions:[]});
    await client.archiveThread("thread-1",9);
    expect(bodies).toEqual([
      {nonce:"nonce-1",project_id:"project-1",thread_id:"thread-1",action:"thread.submit",authorization_token:"grant:thread.submit",payload:{expected_sequence:8,content:{text:"Review",attachments:[],file_mentions:[]},class:"normal"}},
      {nonce:"nonce-2",project_id:"project-1",thread_id:"thread-1",action:"thread.archive",authorization_token:"grant:thread.archive",payload:{expected_sequence:9}},
    ]);
  });

  test("acquires a fresh one-mutation passkey grant through exact challenge and assertion DTOs",async()=>{
    const bodies:Record<string,any>[]=[],challenge="Y2hhbGxlbmdlLWJ5dGVz";
    const relay:Fetcher=async(input,init)=>{
      if(init?.method==="POST"){const body=JSON.parse(String(init.body));bodies.push(body);return json({sequence:bodies.length,nonce:body.nonce},202)}
      const sequence=Number(new URL(String(input)).searchParams.get("sequence")),body=bodies[sequence-1]!;
      const payload=body.action==="auth.challenge"?{challenge,rp_id:"example.com",expires_at:"2099-01-01T00:00:00Z"}:body.action==="auth.assertion"?{authorization_token:"grant-one",expires_at:"2099-01-01T00:00:00Z"}:{accepted:true};
      return json({sequence,nonce:body.nonce,account_id:"account",device_id:"device",payload});
    };
    const clientData=new TextEncoder().encode('{"type":"webauthn.get"}'),authenticatorData=new Uint8Array([3,4]),signature=new Uint8Array([5,6]);
    let credentialRequests=0;const getCredential=async(options:PublicKeyCredentialRequestOptions)=>{
      credentialRequests++;
      expect(new TextDecoder().decode(options.challenge)).toBe("challenge-bytes");
      expect(options).toMatchObject({rpId:"example.com",userVerification:"required"});
      return{rawId:new Uint8Array([1,2]).buffer,response:{clientDataJSON:clientData.buffer,authenticatorData:authenticatorData.buffer,signature:signature.buffer}} as unknown as PublicKeyCredential;
    };
    let nonce=0;const transport=createWebTransport({mode:"relay",relayUrl:"https://relay.test",bearerToken:"browser-session",projectId:"project-1",fetcher:relay,polling:{maxAttempts:1,minDelayMs:0,maxDelayMs:0},nonce:()=>`passkey-${++nonce}`,passkey:{rpId:"example.com",getCredential}});
    const client=new AgoClient(transport.baseUrl,transport.fetcher);
    await client.submitMessage("thread-1",8,{text:"Authorized",attachments:[],file_mentions:[]});
    await client.archiveThread("thread-1",9);
    expect(bodies.map(body=>body.action)).toEqual(["auth.challenge","auth.assertion","thread.submit","auth.challenge","auth.assertion","thread.archive"]);
    expect(bodies[0]).toEqual({nonce:"passkey-1",project_id:"project-1",thread_id:"thread-1",action:"auth.challenge",payload:{rp_id:"example.com"}});
    expect(bodies[1]).toEqual({nonce:"passkey-2",project_id:"project-1",thread_id:"thread-1",action:"auth.assertion",payload:{credential_id:"AQI",rp_id:"example.com",client_data_json:Buffer.from(clientData).toString("base64url"),authenticator_data:"AwQ",signature:"BQY"}});
    expect(bodies[2].authorization_token).toBe("grant-one");
    expect(bodies[5].authorization_token).toBe("grant-one");
    expect(credentialRequests).toBe(2);
    expect(JSON.stringify(bodies)).not.toContain("browser-session");
  });

  test("bounds pending polling and redacts relay authentication failures",async()=>{
    const pending=createWebTransport({mode:"relay",relayUrl:"https://relay.test",bearerToken:"do-not-leak",projectId:"project-1",fetcher:async(_input,init)=>init?.method==="POST"?json({sequence:1,nonce:"n"},202):json({sequence:1,pending:true},202),polling:{maxAttempts:2,minDelayMs:0,maxDelayMs:0},nonce:()=>"n"});
    await expect(new ProjectionStore({baseUrl:pending.baseUrl,fetcher:pending.fetcher}).connect("thread-1")).rejects.toThrow("Relay result timed out");
    const denied=createWebTransport({mode:"relay",relayUrl:"https://relay.test",bearerToken:"do-not-leak",projectId:"project-1",fetcher:async()=>new Response("token=do-not-leak internal detail",{status:401}),nonce:()=>"n"});
    const error=await new ProjectionStore({baseUrl:denied.baseUrl,fetcher:denied.fetcher}).connect("thread-1").then(()=>Error("expected rejection"),value=>value as Error);
    expect(error.message).toBe("Relay authorization failed.");
    expect(error.message).not.toContain("do-not-leak");
  });

  test("surfaces bridge conflicts and rejects changed duplicate identities",async()=>{
    const conflict=createWebTransport({mode:"relay",relayUrl:"https://relay.test",bearerToken:"token",projectId:"project-1",fetcher:async(input,init)=>init?.method==="POST"?json({sequence:3,nonce:"same"},202):json({sequence:3,nonce:"same",account_id:"account",device_id:"device",error:{code:"conflict",message:"sequence changed"}}),polling:{maxAttempts:1,minDelayMs:0,maxDelayMs:0},nonce:()=>"same"});
    await expect(new AgoClient(conflict.baseUrl,conflict.fetcher).archiveThread("thread-1",4)).rejects.toBeInstanceOf(ConflictError);
    const changed=createWebTransport({mode:"relay",relayUrl:"https://relay.test",bearerToken:"token",projectId:"project-1",fetcher:async(_input,init)=>init?.method==="POST"?json({sequence:3,nonce:"different"},202):json({}),nonce:()=>"same"});
    await expect(new ProjectionStore({baseUrl:changed.baseUrl,fetcher:changed.fetcher}).connect("thread-1")).rejects.toThrow("Relay protocol error.");
  });

  test("direct mode preserves the supplied local fetch transport",async()=>{
    const fetcher:Fetcher=async()=>json({ok:true});
    const transport=createWebTransport({mode:"direct",baseUrl:"http://127.0.0.1:8080",fetcher});
    expect(transport).toEqual({mode:"direct",baseUrl:"http://127.0.0.1:8080",fetcher});
  });
});
