/**
 * agent-mem flat-memory hook for oh-my-pi.
 *
 * Mirrors the agent-mem Claude Code plugin (hooks/hooks.json), which wires four
 * lifecycle events to `agent-mem hook <event>`:
 *
 *   Claude Code        agent-mem adapter        oh-my-pi event (here)
 *   ---------------    ---------------------    ---------------------
 *   SessionStart    -> agent-mem hook session-start  -> session_start        (recall)
 *   UserPromptSubmit-> agent-mem hook prompt-submit   -> before_agent_start   (record prompt)
 *   PostToolUse (*) -> agent-mem hook post-tool-use   -> tool_result          (record tool use)
 *   Stop            -> agent-mem hook stop            -> agent_end            (record final message)
 *
 * Like the Claude/Codex/Gemini integrations, we do NOT re-implement the worker's
 * HTTP contract; we shell out to the same `agent-mem hook <event>` binary and
 * feed it the canonical snake_case payload on stdin. The binary owns worker-port
 * resolution, payload normalization, and graceful worker-down handling.
 *
 * Requirements: the `agent-mem` binary on PATH (override with $AGENT_MEM_BIN) and
 * the agent-mem worker running (`agent-mem worker`). Any failure is swallowed —
 * memory capture must never break or block a session.
 *
 * Install: drop this file in ~/.omp/agent/hooks/ (auto-discovered) or load with
 *   omp --hook ~/.omp/agent/hooks/agent-mem.ts
 */
import type { HookAPI } from "@oh-my-pi/pi-coding-agent";

const BIN = process.env.AGENT_MEM_BIN?.trim() || "agent-mem";
const HOOK_TIMEOUT_MS = 25_000;
/** Cap payload text so a huge tool dump or final message never bloats a request. */
const MAX_TOOL_RESPONSE = 20_000;
const MAX_ASSISTANT_MESSAGE = 50_000;

type AgentMemEvent = "session-start" | "prompt-submit" | "post-tool-use" | "stop";

/** A block that may carry text (TextContent, LLM content block, …). */
interface MaybeTextBlock {
	text?: unknown;
}

/** Minimal, union-safe view of an AgentMessage. */
interface MinimalMessage {
	role?: string;
	content?: unknown;
}

function textFromContent(content: unknown): string {
	if (typeof content === "string") return content;
	if (Array.isArray(content)) {
		return content
			.map(block =>
				block && typeof block === "object" && "text" in block ? String((block as MaybeTextBlock).text ?? "") : "",
			)
			.join("");
	}
	return "";
}

function clamp(text: string, max: number): string {
	return text.length > max ? `${text.slice(0, max)}\n…[truncated ${text.length - max} chars]` : text;
}

/**
 * Spawn `agent-mem hook <event>` with the payload on stdin, exactly as Claude
 * Code pipes its hook JSON. When `wantResponse` is true (session-start recall),
 * await stdout; otherwise fire-and-forget so the agent loop is never blocked.
 * Never throws.
 */
async function runAgentMemHook(
	event: AgentMemEvent,
	payload: Record<string, unknown>,
	wantResponse: boolean,
): Promise<string | null> {
	try {
		const proc = Bun.spawn([BIN, "hook", event], {
			stdin: new Blob([JSON.stringify(payload)]),
			stdout: wantResponse ? "pipe" : "ignore",
			stderr: "ignore",
		});

		if (!wantResponse) {
			// Record events: don't await exit (keeps tool calls / turns latency-free),
			// but guard against a hung child.
			const kill = setTimeout(() => {
				try {
					proc.kill();
				} catch {
					/* already gone */
				}
			}, HOOK_TIMEOUT_MS);
			void proc.exited.finally(() => clearTimeout(kill));
			return null;
		}

		const kill = setTimeout(() => {
			try {
				proc.kill();
			} catch {
				/* already gone */
			}
		}, HOOK_TIMEOUT_MS);
		const out = await new Response(proc.stdout).text();
		await proc.exited;
		clearTimeout(kill);
		return out;
	} catch {
		// Binary missing or worker unreachable — degrade silently.
		return null;
	}
}

export default function (pi: HookAPI): void {
	let recalled = false;

	// SessionStart -> recall stored memory and inject it into context.
	pi.on("session_start", async (_event, ctx) => {
		if (recalled) return;
		recalled = true;

		const out = await runAgentMemHook(
			"session-start",
			{ session_id: ctx.sessionManager.getSessionId(), cwd: ctx.cwd, source: "startup" },
			true,
		);
		if (!out) return;

		let context = "";
		try {
			const parsed = JSON.parse(out) as { hookSpecificOutput?: { additionalContext?: unknown } };
			context = String(parsed?.hookSpecificOutput?.additionalContext ?? "");
		} catch {
			context = out.trim();
		}
		if (context.trim().length === 0) return;

		pi.sendMessage({
			customType: "agent-mem",
			content: context,
			display: true,
			attribution: "user",
		});
	});

	// UserPromptSubmit -> record the user's prompt.
	pi.on("before_agent_start", async (event, ctx) => {
		await runAgentMemHook(
			"prompt-submit",
			{ session_id: ctx.sessionManager.getSessionId(), cwd: ctx.cwd, prompt: event.prompt },
			false,
		);
	});

	// PostToolUse (all tools) -> record tool invocation + result.
	pi.on("tool_result", async (event, ctx) => {
		await runAgentMemHook(
			"post-tool-use",
			{
				session_id: ctx.sessionManager.getSessionId(),
				cwd: ctx.cwd,
				tool_name: event.toolName,
				tool_input: event.input,
				tool_response: clamp(textFromContent(event.content), MAX_TOOL_RESPONSE),
				is_error: event.isError ?? false,
			},
			false,
		);
	});

	// Stop -> record the assistant's final message.
	pi.on("agent_end", async (event, ctx) => {
		if (event.willContinue) return; // an auto-continuation is scheduled; not a real settle
		const messages = event.messages as ReadonlyArray<MinimalMessage>;
		const lastAssistant = [...messages].reverse().find(m => m?.role === "assistant");
		const text = lastAssistant ? textFromContent(lastAssistant.content) : "";
		if (text.trim().length === 0) return;

		await runAgentMemHook(
			"stop",
			{
				session_id: ctx.sessionManager.getSessionId(),
				cwd: ctx.cwd,
				last_assistant_message: clamp(text, MAX_ASSISTANT_MESSAGE),
			},
			false,
		);
	});
}
