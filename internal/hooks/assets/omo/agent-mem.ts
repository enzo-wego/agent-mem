/** Native OmO 5.1.83 extension. The agent-mem CLI owns the worker protocol. */
import type { ExtensionAPI, ExtensionContext, SessionEntry } from "@earendil-works/pi-coding-agent";

const HOOK_TIMEOUT_MS = 1_500;
const MAX_RECALL_BYTES = 128 * 1024;
const MAX_PAYLOAD_BYTES = 128 * 1024;
const MAX_TOOL_INPUT = 4_000;
const MAX_TOOL_RESPONSE = 4_000;
const MAX_PENDING_INPUTS = 128;
const RECALL_ENTRY = "agent-mem:recall-attempt";
const TRUNCATED = "…[truncated]";

type HookEvent = "session-start" | "prompt-submit" | "post-tool-use" | "stop";

const recallHistory = {
	initial: (): string | undefined => undefined,
	add: (last: string | undefined, entry: SessionEntry): string | undefined =>
		entry.type === "custom" && entry.customType === RECALL_ENTRY && typeof entry.data === "string"
			? entry.data
			: last,
};

function clip(text: string, limit: number): string {
	return text.length > limit ? text.slice(0, limit - TRUNCATED.length) + TRUNCATED : text;
}

function contentText(content: unknown, limit: number): string {
	if (typeof content === "string") return clip(content, limit);
	if (!Array.isArray(content)) return "";
	let text = "";
	for (const block of content) {
		if (block?.type !== "text" || typeof block.text !== "string" || !block.text) continue;
		const separator = text ? "\n" : "";
		const remaining = limit - text.length - separator.length;
		if (block.text.length > remaining) return clip(text + separator + block.text.slice(0, Math.max(0, remaining) + 1), limit);
		text += separator + block.text;
	}
	return text;
}

function boundedToolInput(input: unknown): unknown {
	// Interrupt serialization before copying large strings or descending through large objects.
	let remaining = MAX_TOOL_INPUT;
	try {
		const json = JSON.stringify(input, (key, value) => {
			remaining -= key.length + 1 + (typeof value === "string" ? value.length : 0);
			if (remaining < 0) throw new Error("tool input exceeds budget");
			return value;
		});
		if (json && Buffer.byteLength(json) <= MAX_TOOL_INPUT) return JSON.parse(json);
	} catch {
		// Non-JSON and oversize inputs should not drop the result.
	}
	return { omitted: "tool input is too large or not JSON serializable" };
}

async function runHook(event: HookEvent, ctx: ExtensionContext, fields: Record<string, unknown>): Promise<string | undefined> {
	let timer: ReturnType<typeof setTimeout> | undefined;
	try {
		const session_id = ctx.sessionManager.getSessionId();
		if (!session_id || !ctx.cwd) return;
		const payload = JSON.stringify({ session_id, cwd: ctx.cwd, ...fields });
		if (Buffer.byteLength(payload) > MAX_PAYLOAD_BYTES) return;
		const recall = event === "session-start";
		// OmO still dispatches results from already-aborted tools. Record those,
		// but let a new interrupt cancel an in-flight result hook.
		const signal = event === "post-tool-use" ? ctx.signal : undefined;
		const proc = Bun.spawn([process.env.AGENT_MEM_BIN?.trim() || "agent-mem", "hook", event, "omo"], {
			env: process.env,
			stdin: new Blob([payload]),
			stdout: recall ? "pipe" : "ignore",
			stderr: "ignore",
			timeout: HOOK_TIMEOUT_MS,
			killSignal: "SIGKILL",
			signal: signal?.aborted ? undefined : signal,
		});
		if (!recall) {
			await proc.exited;
			return;
		}
		const reader = proc.stdout?.getReader();
		try {
			const read = async () => {
				if (!reader) return;
				const decoder = new TextDecoder();
				let bytes = 0;
				let text = "";
				while (true) {
					const { value, done } = await reader.read();
					if (done) return text + decoder.decode();
					bytes += value.byteLength;
					if (bytes > MAX_RECALL_BYTES) {
						proc.kill("SIGKILL");
						return;
					}
					text += decoder.decode(value, { stream: true });
				}
			};
			const result = Promise.all([proc.exited, read()]).then(([code, text]) => code === 0 ? text : undefined);
			// Bound the wait for EOF too (e.g. if a wrapper left the pipe open).
			return await Promise.race([
				result,
				new Promise<undefined>(resolve => { timer = setTimeout(resolve, HOOK_TIMEOUT_MS); }),
			]);
		} finally {
			if (reader) void reader.cancel().catch(() => {});
		}
	} catch {
		// Missing CLI, worker, or valid output must not affect the native turn.
		return;
	} finally {
		clearTimeout(timer);
	}
}

export default function (pi: ExtensionAPI): void {
	const pending = new Map<string, string>();
	let sessionId: string | undefined;
	let userTurn = false;
	let finalText = "";

	pi.on("session_start", async (event, ctx) => {
		try {
			if (event.sizesOnly) return;
			const id = ctx.sessionManager.getSessionId();
			if (!id || sessionId === id) return;
			sessionId = id;
			pending.clear();
			userTurn = false;
			finalText = "";
			if (ctx.sessionManager.reduceHistory(recallHistory) === id) return;
			// The session ID distinguishes real reload/resume from a fork that inherited the marker.
			pi.appendEntry(RECALL_ENTRY, id, { startupBookkeeping: true });
			const output = await runHook("session-start", ctx, { source: event.reason });
			if (!output || sessionId !== id) return;
			const context: unknown = JSON.parse(output)?.hookSpecificOutput?.additionalContext;
			if (typeof context === "string" && context.trim()) {
				pi.sendMessage({ customType: "agent-mem", content: clip(context, 32_000), display: false }, { triggerTurn: false });
			}
		} catch {
			// Session history may be unavailable, or CLI stdout may not be valid JSON.
		}
	});

	pi.on("input", event => {
		if (event.attribution !== "user") return;
		const oldest = pending.keys().next().value;
		if (pending.size >= MAX_PENDING_INPUTS && oldest !== undefined) pending.delete(oldest);
		pending.set(event.inputId, clip(event.text, 16_000));
	});

	pi.on("input_disposition", async (event, ctx) => {
		if (event.disposition === "queued") return;
		const prompt = pending.get(event.inputId);
		pending.delete(event.inputId);
		if (prompt === undefined || event.disposition !== "started") return;
		userTurn = true;
		finalText = "";
		await runHook("prompt-submit", ctx, { prompt });
	});

	pi.on("tool_result", async (event, ctx) => {
		// The worker queues tool_response but discards a top-level is_error field.
		const status = event.isError ? "[tool failed]\n" : "";
		await runHook("post-tool-use", ctx, {
			tool_name: clip(event.toolName, 256),
			tool_input: boundedToolInput(event.input),
			tool_response: status + contentText(event.content, MAX_TOOL_RESPONSE - status.length),
		});
	});

	pi.on("agent_end", event => {
		if (!userTurn) return;
		const assistant = event.messages.findLast(message => message.role === "assistant");
		finalText = !event.aborted && !event.willRetry && assistant?.role === "assistant" && assistant.stopReason === "stop"
			? contentText(assistant.content, 16_000)
			: "";
	});

	pi.on("agent_settled", async (_event, ctx) => {
		// agent_end is per low-level run; retries/compaction/queued continuations can follow it.
		if (!userTurn) return;
		const text = finalText;
		userTurn = false;
		finalText = "";
		// With no final answer the worker can use stored observations and still close the turn.
		await runHook("stop", ctx, text.trim() ? { last_assistant_message: text } : {});
	});
}
