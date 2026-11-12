/**
 * Highlight text carries Typst's *strong* and _emphasis_ marks; a backslash
 * escapes the next character. The CV template (templates/classic/1/main.typ,
 * `rich`) reads exactly this and nothing else, so keep the two in step.
 */
export interface Run {
  text: string;
  bold: boolean;
  italic: boolean;
}

/** Splits marked text into runs. Unbalanced marks yield the text as given. */
export function parseRich(src: string): Run[] {
  const runs: Run[] = [];
  let buf = '';
  let bold = false;
  let italic = false;
  let esc = false;
  for (const c of src) {
    if (esc) {
      buf += c;
      esc = false;
    } else if (c === '\\') esc = true;
    else if (c === '*' || c === '_') {
      runs.push({ text: buf, bold, italic });
      buf = '';
      if (c === '*') bold = !bold;
      else italic = !italic;
    } else buf += c;
  }
  if (esc) buf += '\\';
  runs.push({ text: buf, bold, italic });
  if (bold || italic) return src ? [{ text: src, bold: false, italic: false }] : [];
  return runs.filter((r) => r.text !== '');
}

const escape = (s: string) => s.replace(/[\\*_]/g, (c) => '\\' + c);

/** Writes runs back as marked text. Whitespace at a run's edges stays outside its marks. */
export function serializeRich(runs: Run[]): string {
  const merged: Run[] = [];
  for (const r of runs) {
    if (!r.text) continue;
    const last = merged.at(-1);
    if (last && last.bold === r.bold && last.italic === r.italic) last.text += r.text;
    else merged.push({ ...r });
  }
  return merged
    .map((r) => {
      if (!r.bold && !r.italic) return escape(r.text);
      const [, lead, core, trail] = /^(\s*)([\s\S]*?)(\s*)$/.exec(r.text)!;
      if (!core) return r.text;
      const open = (r.bold ? '*' : '') + (r.italic ? '_' : '');
      const close = (r.italic ? '_' : '') + (r.bold ? '*' : '');
      return lead + open + escape(core) + close + trail;
    })
    .join('');
}

/** The text without marks, for lengths and plain contexts. */
export const plainRich = (src: string) =>
  parseRich(src)
    .map((r) => r.text)
    .join('');
