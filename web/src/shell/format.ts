import type { Session } from "../api";

/** "14:05" today, "Mon" this week, "Aug 19" otherwise. */
export function shortTime(iso?: string | null): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const now = new Date();
  if (d.toDateString() === now.toDateString()) return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  const days = (now.getTime() - d.getTime()) / 86400000;
  if (days < 6) return d.toLocaleDateString([], { weekday: "short" });
  return d.toLocaleDateString([], { month: "short", day: "numeric" });
}

/** "Wed, Aug 19 9:42 AM" for date separators. */
export function longTime(iso: string): string {
  const d = new Date(iso);
  return d.toLocaleString([], { weekday: "short", month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
}

export const agentLabel: Record<string, string> = { claude: "Claude Code", codex: "Codex", opencode: "OpenCode", shell: "Shell" };

/** The short role tag shown next to an agent's name: its persona's first words, else its CLI. */
export function roleTag(s: Session): string {
  const p = (s.persona || "").trim().split("\n")[0].replace(/^(you are|du bist)\s+/i, "");
  if (p) return p.length > 28 ? p.slice(0, 26) + "…" : p;
  return agentLabel[s.agent] ?? s.agent;
}

export type AgentStatus = "working" | "attention" | "stopped" | "error" | null;

export function agentStatus(s: Session): AgentStatus {
  if (s.state === "stopped" || s.state === "deleting") return "stopped";
  if (s.state === "failed") return "error";
  if (s.state !== "running") return null;
  if (s.conversation?.error) return "error";
  const a = s.activity?.state;
  if (a === "needs_permission") return "attention";
  if (s.conversation?.turn || a === "thinking" || a === "tool") return "working";
  if (a === "error") return "error";
  return null;
}

/** Whether a session belongs in "Needs you". */
export function needsYou(s: Session): boolean {
  if (s.state === "failed") return true;
  if (s.state !== "running") return false;
  if (s.conversation?.error) return true;
  const a = s.activity?.state;
  return a === "needs_permission" || a === "error";
}

/** One line on what the agent is doing or did last. */
export function agentLine(s: Session): string {
  if (s.state === "creating") return "Starting…";
  if (s.state === "stopping") return "Stopping…";
  if (s.state === "stopped") return "Stopped";
  if (s.state === "failed") return s.state_reason || "Failed";
  if (s.conversation?.error) return s.conversation.error;
  const a = s.activity;
  if (a?.state === "needs_permission") return `Waiting for you: ${a.detail || "a permission"}`;
  if (a?.state === "tool" && a.detail) return a.detail;
  if (a?.state === "thinking" || s.conversation?.turn) return "Working…";
  if (a?.state === "error") return a.detail || "Error";
  if (a?.message) return a.message.replace(/\s+/g, " ").trim();
  return s.tail?.trim() || (s.interface === "conversation" ? "Ready" : "Terminal");
}

/** When the agent last did something, for sorting and the row time. */
export function lastActive(s: Session): string {
  return s.conversation?.turn_since || s.conversation?.last_turn_at || s.activity?.since || s.last_output_at || s.updated_at || s.created_at;
}
