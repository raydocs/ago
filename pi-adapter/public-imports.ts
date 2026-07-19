import { createAgoSidecar, encodeJsonl, decodeCommand } from "./src/index.js";
import type { AgoCommand, AgoEvent, AgoMessage, AgoToolInvocation, AgoToolResult } from "./src/index.js";
void [createAgoSidecar, encodeJsonl, decodeCommand];
type PublicTypes = AgoCommand | AgoEvent | AgoMessage | AgoToolInvocation | AgoToolResult;
declare const publicType: PublicTypes;
void publicType;
