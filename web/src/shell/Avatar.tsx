// A small creature per agent: a colour, a shape and a face, all picked
// from its id, so the same agent always looks the same.

export function hashOf(id: string): number {
  let h = 2166136261;
  for (const c of id) h = Math.imul(h ^ c.charCodeAt(0), 16777619) >>> 0;
  return h;
}

const palette = ["#7c5cff", "#22b07d", "#f2a516", "#ef6a3a", "#e8457c", "#3b9cf5", "#9d5cf0", "#14b8a6"];

export function agentColor(id: string): string {
  return palette[hashOf(id) % palette.length];
}

const shapes = [
  // circle
  <circle cx="20" cy="20" r="17" />,
  // rounded square
  <rect x="4" y="4" width="32" height="32" rx="10" />,
  // hexagon
  <path d="M20 2.5 35.2 11.2v17.6L20 37.5 4.8 28.8V11.2z" stroke-linejoin="round" stroke-width="3" />,
  // pill
  <rect x="2.5" y="9" width="35" height="22" rx="11" />,
  // soft triangle
  <path d="M20 4c1.4 0 2.4.8 3.2 2.2l12.2 23.2c1.2 2.4-.2 4.6-2.8 4.6H7.4c-2.6 0-4-2.2-2.8-4.6L16.8 6.2C17.6 4.8 18.6 4 20 4z" />,
];

const faces = [
  // dots
  <>
    <circle cx="15" cy="20" r="2.4" />
    <circle cx="25" cy="20" r="2.4" />
  </>,
  // dashes
  <>
    <rect x="12" y="19" width="6" height="2.6" rx="1.3" />
    <rect x="22" y="19" width="6" height="2.6" rx="1.3" />
  </>,
  // tall
  <>
    <rect x="14" y="15" width="3" height="9" rx="1.5" />
    <rect x="23" y="15" width="3" height="9" rx="1.5" />
  </>,
  // wink
  <>
    <circle cx="15" cy="20" r="2.4" />
    <rect x="22" y="19" width="6" height="2.6" rx="1.3" />
  </>,
];

export function Avatar(props: { id: string; size?: number; status?: "working" | "attention" | "stopped" | "error" | null; owner?: boolean }) {
  const size = props.size ?? 36;
  if (props.owner) {
    return (
      <span class="sh-avatar owner" style={{ width: size, height: size }}>
        <svg viewBox="0 0 40 40" width={size} height={size}>
          <circle cx="20" cy="20" r="18" fill="var(--sh-bubble)" />
          <circle cx="20" cy="16" r="6" fill="var(--sh-muted)" />
          <path d="M8 33a12 12 0 0 1 24 0" fill="var(--sh-muted)" />
        </svg>
      </span>
    );
  }
  const h = hashOf(props.id);
  const color = palette[h % palette.length];
  const shape = shapes[(h >>> 3) % shapes.length];
  const face = faces[(h >>> 7) % faces.length];
  return (
    <span class={`sh-avatar ${props.status ?? ""}`} style={{ width: size, height: size }}>
      <svg viewBox="0 0 40 40" width={size} height={size}>
        <g fill={color} stroke={color}>
          {shape}
        </g>
        <g fill="#fff" opacity="0.95">
          {face}
        </g>
      </svg>
      {props.status && <span class={`sh-status ${props.status}`} />}
    </span>
  );
}
