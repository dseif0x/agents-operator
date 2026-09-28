import type { User } from "../api";
import { Link } from "../router";
import { toggleTheme } from "../theme";

export function Nav(props: { user: User; onLogout: () => void; right?: preact.ComponentChildren }) {
  return (
    <nav class="nav">
      <Link href="/" class="brand">
        agents-operator
      </Link>
      <span class="spacer" />
      {props.right}
      <Link href="/account" class="btn small" title="Account and credentials">
        {props.user.username}
      </Link>
      <button class="btn small" onClick={toggleTheme} title="Toggle theme" aria-label="Toggle theme">
        ◐
      </button>
      <button class="btn small" onClick={props.onLogout}>
        Log out
      </button>
    </nav>
  );
}
