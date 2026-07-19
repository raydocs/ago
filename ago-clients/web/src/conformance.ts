import {ProjectionStore,type Fetcher} from "./store";

export interface ConformanceSummary {
  digest:string;
  snapshot_sequence:number;
  mailbox:{thread_id:string;last_sequence:number;activity:string;active_turn_id?:string;cancel_requested:boolean;events?:unknown;digest:string};
  queue:{count:number;digest:string};
  dialogs:{count:number;digest:string};
  diff:{has_snapshot:boolean;comment_count:number;digest:string};
  events:{count:number;first_sequence:number|null;last_sequence:number|null;digest:string};
}

export function conformanceEndpoint(args:string[]):{baseUrl:string;threadId:string}{
  if(args.length!==1)throw Error("usage: bun src/conformance.ts <daemon-projection-url>");
  let url:URL;
  try{url=new URL(args[0]!)}catch{throw Error("projection URL must be an absolute HTTP(S) URL")}
  if(!["http:","https:"].includes(url.protocol)||url.username||url.password||url.search||url.hash)throw Error("projection URL must be credential-free HTTP(S) without query or fragment");
  const match=/^(.*)\/v1\/threads\/([^/]+)\/projection$/.exec(url.pathname);
  if(!match)throw Error("projection URL must end in /v1/threads/{id}/projection");
  let threadId:string;
  try{threadId=decodeURIComponent(match[2]!)}catch{throw Error("projection URL contains an invalid thread ID")}
  if(!threadId)throw Error("projection URL requires a thread ID");
  return{baseUrl:`${url.origin}${match[1]}`,threadId};
}

function canonicalJSON(value:unknown):string{
  if(value===null||typeof value==="boolean"||typeof value==="string")return JSON.stringify(value);
  if(typeof value==="number"){if(!Number.isFinite(value))throw Error("non-finite conformance value");return JSON.stringify(value)}
  if(Array.isArray(value))return`[${value.map(canonicalJSON).join(",")}]`;
  if(typeof value==="object"){
    const object=value as Record<string,unknown>;
    return`{${Object.keys(object).filter(key=>object[key]!==undefined).sort().map(key=>`${JSON.stringify(key)}:${canonicalJSON(object[key])}`).join(",")}}`;
  }
  throw Error("non-JSON conformance value");
}

function digest(value:unknown):string{return new Bun.CryptoHasher("sha256").update(canonicalJSON(value)).digest("hex")}

export async function projectionConformance(projectionUrl:string,fetcher:Fetcher=fetch):Promise<ConformanceSummary>{
  const endpoint=conformanceEndpoint([projectionUrl]);
  const store=new ProjectionStore({baseUrl:endpoint.baseUrl,fetcher});
  await store.connect(endpoint.threadId);
  const projection=store.current!;
  const {queue,...mailbox}=projection.mailbox;
  const canonical={snapshot_sequence:projection.snapshot_sequence,mailbox,queue,dialogs:projection.dialogs,diff:projection.diff,events:projection.events};
  return{
    digest:digest(canonical),snapshot_sequence:projection.snapshot_sequence,
    mailbox:{...mailbox,digest:digest(mailbox)},
    queue:{count:queue.length,digest:digest(queue)},
    dialogs:{count:projection.dialogs.length,digest:digest(projection.dialogs)},
    diff:{has_snapshot:projection.diff.snapshot!==null,comment_count:projection.diff.comments.length,digest:digest(projection.diff)},
    events:{count:projection.events.length,first_sequence:projection.events[0]?.sequence??null,last_sequence:projection.events.at(-1)?.sequence??null,digest:digest(projection.events)},
  };
}

async function main(){
  try{const args=Bun.argv.slice(2);conformanceEndpoint(args);console.log(canonicalJSON(await projectionConformance(args[0]!)))}catch(error){console.error(error instanceof Error?error.message:String(error));process.exitCode=1}
}

if(import.meta.main)await main();
