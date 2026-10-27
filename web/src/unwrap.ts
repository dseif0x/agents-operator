// unwrapLines joins the rows a full-screen program wrapped itself. xterm
// marks the rows it soft-wrapped (bufferText already joins those), but TUIs
// like Claude Code (Ink) break long text into rows of their own, so a long
// command copied from the screen comes out with a newline at every row.
//
// A row is taken as wrapped when the first word of the next row would not
// have fitted after it: within the terminal width, or within a box's right
// border when the row has one. A row filled to the edge was cut mid-word and
// is joined without a space. Box borders (│) are dropped: what is copied out
// of a box is its content. It is a guess, so the sheet can show the raw rows.

const BORDER = "│┃║";
// Rows that are only box lines (a rule across the screen fills it too).
const RULE = /^[─━═╭╮╰╯┌┐└┘├┤┬┴┼\s-]+$/;

interface Row {
  indent: string; // leading text kept on the first row of a run
  body: string; // content without borders and surrounding spaces
  end: number; // column after the last character of the content
  limit: number; // last column content may reach on this row
}

function parse(line: string, cols: number): Row {
  let s = line.replace(/\s+$/, "");
  let limit = cols;
  const last = s[s.length - 1];
  if (last && BORDER.includes(last)) {
    limit = s.length - 2; // a space of padding before the border
    s = s.slice(0, -1).replace(/\s+$/, "");
  }
  const m = /^(\s*)([│┃║]\s?)?(\s*)/.exec(s)!;
  const indent = m[2] ? m[1] + m[3] : m[0];
  return { indent, body: s.slice(m[0].length), end: s.length, limit };
}

function wrapped(prev: Row, next: Row): boolean {
  if (prev.body === "" || next.body === "" || RULE.test(prev.body) || RULE.test(next.body)) return false;
  if (prev.end >= prev.limit) return true;
  const word = /^\S+/.exec(next.body)?.[0] ?? "";
  return prev.end + 1 + word.length > prev.limit;
}

export function unwrapLines(text: string, cols: number): string {
  if (cols <= 0) return text;
  const rows = text.split("\n").map((l) => parse(l, cols));
  const out: string[] = [];
  let cur = "";
  rows.forEach((r, i) => {
    const prev = rows[i - 1];
    if (prev && wrapped(prev, r)) {
      cur += (prev.end >= prev.limit ? "" : " ") + r.body;
      return;
    }
    if (i > 0) out.push(cur);
    cur = r.indent + r.body;
  });
  out.push(cur);
  return out.join("\n");
}
