import { render } from "preact";
import { App } from "./App";
import { applyTheme, currentTheme } from "./theme";
import "@xterm/xterm/css/xterm.css";
import "./styles.css";

applyTheme(currentTheme());
render(<App />, document.getElementById("app")!);
