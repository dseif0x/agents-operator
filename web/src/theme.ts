// Dark by default, one light theme, both via CSS variables on <html>.
export type Theme = "dark" | "light";

const KEY = "agents-operator.theme";

export function currentTheme(): Theme {
  try {
    const saved = localStorage.getItem(KEY);
    if (saved === "dark" || saved === "light") return saved;
  } catch {
    /* private mode */
  }
  return matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
}

export function applyTheme(theme: Theme) {
  document.documentElement.dataset.theme = theme;
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.setAttribute("content", theme === "dark" ? "#0f1115" : "#f6f7f9");
  try {
    localStorage.setItem(KEY, theme);
  } catch {
    /* ignore */
  }
  dispatchEvent(new CustomEvent("agents-operator:theme", { detail: theme }));
}

export function toggleTheme() {
  applyTheme(currentTheme() === "dark" ? "light" : "dark");
}

/** Terminal colours follow the page theme. */
export function terminalTheme(theme: Theme) {
  if (theme === "light") {
    return {
      background: "#ffffff",
      foreground: "#1f2328",
      cursor: "#1f2328",
      selectionBackground: "#b6d7ff",
      black: "#24292f",
      red: "#cf222e",
      green: "#116329",
      yellow: "#4d2d00",
      blue: "#0969da",
      magenta: "#8250df",
      cyan: "#1b7c83",
      white: "#6e7781",
      brightBlack: "#57606a",
      brightRed: "#a40e26",
      brightGreen: "#1a7f37",
      brightYellow: "#633c01",
      brightBlue: "#218bff",
      brightMagenta: "#a475f9",
      brightCyan: "#3192aa",
      brightWhite: "#8c959f",
    };
  }
  return {
    background: "#0f1115",
    foreground: "#e6e6e6",
    cursor: "#e6e6e6",
    selectionBackground: "#3b4a6b",
    black: "#1b1e26",
    red: "#ff6b6b",
    green: "#8ce99a",
    yellow: "#ffd43b",
    blue: "#74c0fc",
    magenta: "#da77f2",
    cyan: "#66d9e8",
    white: "#c1c2c5",
    brightBlack: "#5c5f66",
    brightRed: "#ff8787",
    brightGreen: "#b2f2bb",
    brightYellow: "#ffe066",
    brightBlue: "#a5d8ff",
    brightMagenta: "#e599f7",
    brightCyan: "#99e9f2",
    brightWhite: "#f8f9fa",
  };
}
