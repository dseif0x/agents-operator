export function timeAgo(iso: string | null | undefined): string {
  if (!iso) return "never";
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return "?";
  const s = Math.max(0, Math.round((Date.now() - t) / 1000));
  if (s < 5) return "just now";
  if (s < 60) return `${s}s ago`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.round(m / 60);
  if (h < 48) return `${h}h ago`;
  return `${Math.round(h / 24)}d ago`;
}

export function repoShort(url: string): string {
  if (!url) return "no repo";
  return url
    .replace(/^git@[^:]+:/, "")
    .replace(/^(https?|ssh):\/\/[^/]+\//, "")
    .replace(/\.git\/?$/, "");
}

/** "owner/repo@branch (+2 more)" for a session's repo list. */
export function reposSummary(repos: { url: string; branch?: string }[] | undefined): string {
  if (!repos || repos.length === 0) return "no repo";
  const first = repoShort(repos[0].url) + (repos[0].branch ? `@${repos[0].branch}` : "");
  return repos.length > 1 ? `${first} +${repos.length - 1}` : first;
}

export function stateLabel(state: string): string {
  return state.charAt(0).toUpperCase() + state.slice(1);
}

/** One line for an agent's hook-reported activity. */
export function activityLabel(a: { state: string; detail?: string } | undefined): string {
  if (!a) return "";
  switch (a.state) {
    case "thinking":
      return "Thinking…";
    case "tool":
      return a.detail ? `Running ${a.detail}` : "Running a tool";
    case "needs_permission":
      return a.detail ? `Needs permission: ${a.detail}` : "Needs permission";
    case "waiting_input":
      return a.detail === "ready" ? "Ready" : "Waiting for you";
    case "error":
      return a.detail ? `Error: ${a.detail}` : "Error";
    case "exited":
      return "Agent exited";
    default:
      return a.state;
  }
}
