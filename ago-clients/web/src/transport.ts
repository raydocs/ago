import type {Fetcher} from "./store";

type RelayMutationAction="thread.submit"|"thread.archive";
type RelayAction="thread.projection"|RelayMutationAction|"auth.challenge"|"auth.assertion";
export interface RelayActionContext {projectId:string;threadId:string;action:RelayMutationAction}
export type RelayAuthorizationProvider=(context:RelayActionContext)=>Promise<string|undefined>;
export type PasskeyCredentialGetter=(options:PublicKeyCredentialRequestOptions)=>Promise<PublicKeyCredential|null>;
export interface RelayPasskeyConfig {rpId:string;getCredential?:PasskeyCredentialGetter}
type PollingConfig={maxAttempts:number;minDelayMs:number;maxDelayMs:number};
export type WebTransportConfig=
  | {mode:"direct";baseUrl:string;fetcher?:Fetcher}
  | {mode:"relay";relayUrl:string;bearerToken:string;projectId:string;fetcher?:Fetcher;polling?:Partial<PollingConfig>;enqueueAttempts?:number;requestTimeoutMs?:number;nonce?:()=>string;authorization?:RelayAuthorizationProvider;passkey?:RelayPasskeyConfig};

export type WebTransport={mode:"direct"|"relay";baseUrl:string;fetcher:Fetcher};
const relayBaseUrl="https://ago-relay-transport.invalid";
const record=(value:unknown):Record<string,unknown>|null=>!!value&&typeof value==="object"&&!Array.isArray(value)?value as Record<string,unknown>:null;
const exact=(value:Record<string,unknown>,keys:string[])=>Object.keys(value).every(key=>keys.includes(key));
const positiveInteger=(value:unknown):value is number=>Number.isSafeInteger(value)&&Number(value)>0;
const jsonResponse=(value:unknown,status=200)=>new Response(JSON.stringify(value),{status,headers:{"content-type":"application/json","cache-control":"no-store"}});
const protocolError=()=>Error("Relay protocol error.");
const delay=(milliseconds:number)=>milliseconds>0?new Promise(resolve=>setTimeout(resolve,milliseconds)):Promise.resolve();
const mutationAction=(action:RelayAction):action is RelayMutationAction=>action==="thread.submit"||action==="thread.archive";

function decodeBase64URL(value:string,maximum:number):ArrayBuffer{
 if(!value||value.length>maximum*2||!/^[A-Za-z0-9_-]+$/.test(value)||value.length%4===1)throw protocolError();
 let binary:string;try{binary=atob(value.replace(/-/g,"+").replace(/_/g,"/")+"===".slice((value.length+3)%4))}catch{throw protocolError()}
 if(binary.length>maximum)throw protocolError();const bytes=new Uint8Array(binary.length);for(let index=0;index<binary.length;index++)bytes[index]=binary.charCodeAt(index);return bytes.buffer;
}

function encodeBase64URL(value:ArrayBuffer):string{
 const bytes=new Uint8Array(value);let binary="";for(let offset=0;offset<bytes.length;offset+=8192)binary+=String.fromCharCode(...bytes.subarray(offset,offset+8192));return btoa(binary).replace(/\+/g,"-").replace(/\//g,"_").replace(/=+$/,"");
}

class RelayTransportError extends Error {constructor(message:string,public status:number){super(message)}}

function relayURL(value:string):string{
 let url:URL;try{url=new URL(value)}catch{throw Error("Relay URL must be an absolute HTTPS URL.")}
 if(url.protocol!=="https:"||url.username||url.password||url.search||url.hash)throw Error("Relay URL must be credential-free HTTPS without query or fragment.");
 return url.href.replace(/\/$/,"");
}

function decodeAction(input:string|URL|Request,init?:RequestInit):{threadId:string;action:RelayAction;payload:Record<string,unknown>}|null{
 const url=new URL(String(input),relayBaseUrl),method=(init?.method??"GET").toUpperCase();
 const match=/^\/v1\/threads\/([^/]+)\/(projection|messages|archive)$/.exec(url.pathname);if(!match)return null;
 let threadId:string;try{threadId=decodeURIComponent(match[1]!)}catch{return null}if(!threadId)return null;
 if(method==="GET"&&match[2]==="projection"){
  const after=Number(url.searchParams.get("after")),limit=Number(url.searchParams.get("limit"));
  if(!Number.isSafeInteger(after)||after<0||!Number.isSafeInteger(limit)||limit<1||limit>1000)return null;
  return{threadId,action:"thread.projection",payload:{after_sequence:after,limit}};
 }
 if(method!=="POST")return null;
 let body:Record<string,unknown>|null=null;try{body=record(JSON.parse(String(init?.body)))}catch{return null}if(!body)return null;
 if(match[2]==="messages"&&positiveInteger(body.expected_sequence)&&body.content!==undefined&&(body.class==="normal"||body.class==="steer"))return{threadId,action:"thread.submit",payload:{expected_sequence:body.expected_sequence,content:body.content,class:body.class}};
 if(match[2]==="archive"&&positiveInteger(body.expected_sequence))return{threadId,action:"thread.archive",payload:{expected_sequence:body.expected_sequence}};
 return null;
}

function pollingConfig(value:Partial<PollingConfig>|undefined):PollingConfig{
 const result={maxAttempts:value?.maxAttempts??8,minDelayMs:value?.minDelayMs??100,maxDelayMs:value?.maxDelayMs??2_000};
 if(!Number.isSafeInteger(result.maxAttempts)||result.maxAttempts<1||result.maxAttempts>20||!Number.isFinite(result.minDelayMs)||result.minDelayMs<0||!Number.isFinite(result.maxDelayMs)||result.maxDelayMs<result.minDelayMs||result.maxDelayMs>30_000)throw Error("Invalid relay polling configuration.");
 return result;
}

export function createWebTransport(config:WebTransportConfig):WebTransport{
 if(config.mode==="direct")return{mode:"direct",baseUrl:config.baseUrl,fetcher:config.fetcher??fetch};
 const {bearerToken,projectId,authorization,passkey}=config,base=relayURL(config.relayUrl),relayFetcher=config.fetcher??fetch,polling=pollingConfig(config.polling),enqueueAttempts=config.enqueueAttempts??3,requestTimeoutMs=config.requestTimeoutMs??15_000,nonceFactory=config.nonce??(()=>crypto.randomUUID());
 if(!bearerToken||!projectId||!Number.isSafeInteger(enqueueAttempts)||enqueueAttempts<1||enqueueAttempts>5||!Number.isFinite(requestTimeoutMs)||requestTimeoutMs<1||requestTimeoutMs>60_000)throw Error("Complete in-memory relay configuration is required.");
 if(passkey&&(!passkey.rpId||passkey.rpId.trim()!==passkey.rpId))throw Error("Passkey relying party ID is required.");
 const relayFetch=async(path:string,init?:RequestInit)=>{
  const headers=new Headers(init?.headers);headers.set("authorization",`Bearer ${bearerToken}`);
  const controller=new AbortController(),timer=setTimeout(()=>controller.abort(),requestTimeoutMs);let response:Response;
  try{response=await relayFetcher(`${base}${path}`,{...init,headers,signal:controller.signal,cache:"no-store",credentials:"omit",referrerPolicy:"no-referrer"})}catch(error){throw error}finally{clearTimeout(timer)}
  if(response.status===401||response.status===403)throw new RelayTransportError("Relay authorization failed.",response.status);
  if(response.status===409)throw new RelayTransportError("Relay request conflicted.",409);
  if(!response.ok&&response.status<500)throw new RelayTransportError("Relay request was rejected.",response.status);
  return response;
 };
 let acquireRecentPasskey:((threadId:string)=>Promise<string>)|undefined;
 const execute=async(target:{threadId:string;action:RelayAction;payload:Record<string,unknown>})=>{
  let authorizationToken:string|undefined;
  if(mutationAction(target.action))authorizationToken=await authorization?.({projectId,threadId:target.threadId,action:target.action})??await acquireRecentPasskey?.(target.threadId);
  const nonce=nonceFactory();if(!nonce)throw Error("Relay nonce generation failed.");
  const requestBody=JSON.stringify({nonce,project_id:projectId,thread_id:target.threadId,action:target.action,...(authorizationToken?{authorization_token:authorizationToken}:{}),payload:target.payload});
  let accepted:Record<string,unknown>|null=null;
  for(let attempt=0;attempt<enqueueAttempts;attempt++){
   try{
    const response=await relayFetch("/v1/relay/requests",{method:"POST",headers:{"content-type":"application/json"},body:requestBody});
    if(response.status>=500){if(attempt+1<enqueueAttempts){await delay(Math.min(polling.maxDelayMs,polling.minDelayMs*2**attempt));continue}throw new RelayTransportError("Relay is unavailable.",502)}
    try{accepted=record(await response.json())}catch{throw protocolError()}
    if(!accepted||!exact(accepted,["sequence","nonce"])||!positiveInteger(accepted.sequence)||accepted.nonce!==nonce)throw protocolError();
    break;
   }catch(error){
    if(error instanceof RelayTransportError||error instanceof Error&&error.message==="Relay protocol error.")throw error;
    if(attempt+1>=enqueueAttempts)throw Error("Relay is unavailable.");
    await delay(Math.min(polling.maxDelayMs,polling.minDelayMs*2**attempt));
   }
  }
  if(!accepted)throw Error("Relay is unavailable.");const sequence=Number(accepted.sequence);
  for(let attempt=0;attempt<polling.maxAttempts;attempt++){
   let response:Response;
   try{response=await relayFetch(`/v1/relay/results?sequence=${sequence}`)}catch(error){if(error instanceof RelayTransportError)throw error;if(attempt+1>=polling.maxAttempts)throw Error("Relay result timed out.");await delay(Math.min(polling.maxDelayMs,polling.minDelayMs*2**attempt));continue}
   if(response.status>=500){if(attempt+1>=polling.maxAttempts)throw Error("Relay result timed out.");await delay(Math.min(polling.maxDelayMs,polling.minDelayMs*2**attempt));continue}
   let result:Record<string,unknown>|null;try{result=record(await response.json())}catch{throw protocolError()}
   if(response.status===202){if(!result||!exact(result,["sequence","pending"])||result.sequence!==sequence||result.pending!==true)throw protocolError();if(attempt+1>=polling.maxAttempts)throw Error("Relay result timed out.");await delay(Math.min(polling.maxDelayMs,polling.minDelayMs*2**attempt));continue}
   if(!result||!exact(result,["sequence","nonce","account_id","device_id","payload","error"])||result.sequence!==sequence||result.nonce!==nonce||typeof result.account_id!=="string"||!result.account_id||typeof result.device_id!=="string"||!result.device_id)throw protocolError();
   const responseError=record(result.error);
   if(responseError){if(!exact(responseError,["code","message"])||typeof responseError.code!=="string"||typeof responseError.message!=="string"||result.payload!==undefined)throw protocolError();if(responseError.code==="conflict"||responseError.code==="replay")throw new RelayTransportError("Relay action conflicted.",409);if(responseError.code==="unauthorized"||responseError.code==="authorization_required")throw new RelayTransportError("Relay authorization failed.",403);throw new RelayTransportError("Relay action failed.",502)}
   if(result.payload===undefined)throw protocolError();return result.payload;
  }
  throw Error("Relay result timed out.");
 };
 if(passkey)acquireRecentPasskey=async threadId=>{
  const challengeValue=record(await execute({threadId,action:"auth.challenge",payload:{rp_id:passkey.rpId}}));
  if(!challengeValue||!exact(challengeValue,["challenge","rp_id","expires_at"])||typeof challengeValue.challenge!=="string"||challengeValue.rp_id!==passkey.rpId||typeof challengeValue.expires_at!=="string")throw protocolError();
  const expiresAt=Date.parse(challengeValue.expires_at),remaining=expiresAt-Date.now();if(!Number.isFinite(expiresAt)||remaining<=0)throw Error("Passkey challenge expired.");
  const options:PublicKeyCredentialRequestOptions={challenge:decodeBase64URL(challengeValue.challenge,64<<10),rpId:passkey.rpId,userVerification:"required",timeout:Math.min(remaining,120_000)};
  const getCredential=passkey.getCredential??(async request=>{if(typeof navigator==="undefined"||!navigator.credentials?.get)throw Error("Platform passkey authentication is unavailable.");return await navigator.credentials.get({publicKey:request}) as PublicKeyCredential|null});
  const credential=await getCredential(options);if(!credential)throw Error("Passkey authentication was cancelled.");
  const assertion=credential.response as AuthenticatorAssertionResponse;
  if(!(credential.rawId instanceof ArrayBuffer)||!(assertion.clientDataJSON instanceof ArrayBuffer)||!(assertion.authenticatorData instanceof ArrayBuffer)||!(assertion.signature instanceof ArrayBuffer))throw Error("Platform passkey assertion was malformed.");
  const grantValue=record(await execute({threadId,action:"auth.assertion",payload:{credential_id:encodeBase64URL(credential.rawId),rp_id:passkey.rpId,client_data_json:encodeBase64URL(assertion.clientDataJSON),authenticator_data:encodeBase64URL(assertion.authenticatorData),signature:encodeBase64URL(assertion.signature)}}));
  if(!grantValue||!exact(grantValue,["authorization_token","expires_at"])||typeof grantValue.authorization_token!=="string"||!grantValue.authorization_token||typeof grantValue.expires_at!=="string"||!Number.isFinite(Date.parse(grantValue.expires_at))||Date.parse(grantValue.expires_at)<=Date.now())throw protocolError();
  return grantValue.authorization_token;
 };
 const bridgeFetcher:Fetcher=async(input,init)=>{
  const target=decodeAction(input,init);if(!target)return jsonResponse({error:"Action is unavailable through relay mode."},405);
  try{return jsonResponse(await execute(target),target.action==="thread.projection"?200:202)}catch(error){if(error instanceof RelayTransportError&&error.status===409)return jsonResponse({error:error.message},409);throw error instanceof RelayTransportError?Error(error.message):error}
 };
 return{mode:"relay",baseUrl:relayBaseUrl,fetcher:bridgeFetcher};
}
