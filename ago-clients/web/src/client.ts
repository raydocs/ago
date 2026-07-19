import type {Fetcher} from "./store";

export interface ProjectSummary {
  project_id: string;
  display_name?: string;
}

export interface ThreadSummary {
  thread_id: string;
  last_sequence: number;
  title: string;
  workspace: string;
  project: ProjectSummary;
  activity?: string;
  archived?: boolean;
}

export interface AttachmentDTO {
  attachment_id: string;
  sha256: string;
  size_bytes: number;
  media_type: string;
  filename: string;
}

export interface FileMentionDTO {
  path: string;
}

export interface ComposerDTO {
  text: string;
  attachments: AttachmentDTO[];
  file_mentions: FileMentionDTO[];
}

export type UIResult=
  | {status:"ok";value:boolean|string}
  | {status:"cancelled"|"unavailable"|"timeout"};

export class ConflictError extends Error {
  constructor(public detail: unknown) {
    super("State changed on the server; refresh before retrying.");
  }
}

export class AgoClient {
  constructor(public baseUrl: string, private fetcher: Fetcher = fetch) {}

  async listProjects(): Promise<ProjectSummary[]> {
    const threads = await this.threadResponse("/v1/threads");
    const projects = new Map<string, ProjectSummary>();
    for (const thread of threads) projects.set(thread.project.project_id, thread.project);
    return [...projects.values()].sort((left, right) => left.project_id.localeCompare(right.project_id));
  }

  async listThreads(filter: {project_id?: string; query?: string; archived?: boolean} = {}): Promise<ThreadSummary[]> {
    const query = new URLSearchParams();
    if (filter.project_id) query.set("project_id", filter.project_id);
    if (filter.query) query.set("search", filter.query);
    if (filter.archived !== undefined) query.set("archive", filter.archived ? "archived" : "active");
    if (query.size) query.set("limit", "100");
    if (query.size && !filter.project_id) throw Error("project selection is required for catalog search");
    return this.threadResponse(`/v1/threads${query.size ? `?${query}` : ""}`);
  }

  submitMessage(thread: string, sequence: number, content: ComposerDTO) {
    return this.mutate(`/v1/threads/${encodeURIComponent(thread)}/messages`, "POST", {...this.envelope(sequence), content, class:"normal"});
  }

  archiveThread(thread: string, sequence: number) {
    return this.mutate(`/v1/threads/${encodeURIComponent(thread)}/archive`, "POST", this.envelope(sequence));
  }

  editQueue(thread: string, queue: string, sequence: number, content: unknown) {
    return this.mutate(`/v1/threads/${encodeURIComponent(thread)}/queue/${encodeURIComponent(queue)}`, "PATCH", {...this.envelope(sequence), content});
  }

  dequeue(thread: string, queue: string, sequence: number) {
    return this.mutate(`/v1/threads/${encodeURIComponent(thread)}/queue/${encodeURIComponent(queue)}`, "DELETE", this.envelope(sequence));
  }

  steer(thread: string, queue: string, sequence: number, turn: string) {
    return this.mutate(`/v1/threads/${encodeURIComponent(thread)}/queue/${encodeURIComponent(queue)}/steer`, "POST", {...this.envelope(sequence), expected_turn_id:turn});
  }

  interrupt(thread: string, turn: string, sequence: number, content: unknown) {
    return this.mutate(`/v1/threads/${encodeURIComponent(thread)}/turns/${encodeURIComponent(turn)}/interrupt`, "POST", {...this.envelope(sequence), content});
  }

  resolveDialog(thread: string, dialog: string, revision: number, expected_sequence: number, response: UIResult) {
    return this.mutate(`/v1/threads/${encodeURIComponent(thread)}/dialogs/${encodeURIComponent(dialog)}/resolve`, "POST", {resolver_id:"web-pwa",expected_revision:revision,expected_sequence,response});
  }

  stage(thread: string, sequence: number, revision: number, digest: string, ids: string[]) {
    return this.gitMutation(thread, "stage", sequence, revision, digest, ids);
  }

  unstage(thread: string, sequence: number, revision: number, digest: string, ids: string[]) {
    return this.gitMutation(thread, "unstage", sequence, revision, digest, ids);
  }

  revert(thread: string, sequence: number, revision: number, digest: string, receipt: string) {
    return this.mutate(`/v1/threads/${encodeURIComponent(thread)}/diff/revert`, "POST", {...this.gitEnvelope(sequence),expected_snapshot_revision:revision,expected_snapshot_digest:digest,receipt_id:receipt});
  }

  requestDiffChange(thread: string, sequence: number, revision: number, digest: string, target: {file_id:string;hunk_id?:string}, body: string) {
    return this.mutate(`/v1/threads/${encodeURIComponent(thread)}/diff/comments`, "POST", {
      comment_id:crypto.randomUUID(),expected_sequence:sequence,snapshot_revision:revision,snapshot_digest:digest,
      file_id:target.file_id,...(target.hunk_id?{hunk_id:target.hunk_id}:{}),actor_id:"web-pwa",body,
    });
  }

  private async read(path: string): Promise<unknown> {
    const response = await this.fetcher(`${this.baseUrl}${path}`);
    if (!response.ok) throw Error(`read HTTP ${response.status}`);
    return response.json();
  }

  private async threadResponse(path: string): Promise<ThreadSummary[]> {
    const body = await this.read(path);
    if (!isRecord(body) || !Array.isArray(body.threads)) throw Error("malformed threads response");
    return body.threads.map(value => {
      if (!isRecord(value) || typeof value.thread_id !== "string" || !Number.isSafeInteger(value.last_sequence) || typeof value.title !== "string" || typeof value.workspace !== "string") throw Error("malformed thread");
      const nested = isRecord(value.project) ? value.project : undefined;
      const projectID = typeof nested?.project_id === "string" ? nested.project_id : value.project_id;
      if (typeof projectID !== "string" || !projectID) throw Error("malformed thread project");
      return {
        thread_id:value.thread_id,last_sequence:Number(value.last_sequence),title:value.title,workspace:value.workspace,
        project:{project_id:projectID,...(typeof nested?.display_name === "string" ? {display_name:nested.display_name} : {})},
        ...(typeof value.activity === "string" ? {activity:value.activity} : {}),
        ...(typeof value.archived === "boolean" ? {archived:value.archived} : {}),
      };
    });
  }

  private async mutate(path: string, method: string, body: Record<string, unknown>) {
    const response = await this.fetcher(`${this.baseUrl}${path}`, {method,headers:{"content-type":"application/json"},body:JSON.stringify(body)});
    const detail = await response.json().catch(() => null);
    if (response.status === 409) throw new ConflictError(detail);
    if (!response.ok) throw Error(`mutation HTTP ${response.status}`);
    return detail;
  }

  private envelope(expected_sequence: number) {
    const id = crypto.randomUUID();
    return {command_id:id,idempotency_key:id,actor_id:"web-pwa",expected_sequence};
  }

  private gitEnvelope(expected_sequence: number) {
    const id = `git:${crypto.randomUUID()}`;
    return {command_id:id,idempotency_key:id,actor_id:"web-pwa",expected_sequence};
  }

  private gitMutation(thread: string, kind: "stage"|"unstage", sequence: number, revision: number, digest: string, ids: string[]) {
    return this.mutate(`/v1/threads/${encodeURIComponent(thread)}/diff/${kind}`, "POST", {...this.gitEnvelope(sequence),expected_snapshot_revision:revision,expected_snapshot_digest:digest,selected_unit_ids:ids});
  }
}

const isRecord = (value: unknown): value is Record<string, unknown> => !!value && typeof value === "object" && !Array.isArray(value);
