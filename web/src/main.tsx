import { render } from "preact";
import { App } from "./App";
import { applyTheme, currentTheme } from "./theme";
import { registerServiceWorker, restorePath } from "./pwa";
import "@xterm/xterm/css/xterm.css";
import "./styles.css";

applyTheme(currentTheme());
restorePath();
registerServiceWorker();
render(<App />, document.getElementById("app")!);
