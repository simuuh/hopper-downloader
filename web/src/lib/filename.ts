// splitName splits a file name into a head that may be shortened with an
// ellipsis and a tail that always stays visible: the archive part
// (".part3.rar", ".r07") or the extension. Releases' parts share the whole
// head, so cutting at the end would hide the only difference between them.
// head + tail is always the full name; short names stay whole.

const TAIL_PATTERNS = [/\.part\d+\.rar$/i, /\.r\d+$/i, /\.tar\.(gz|bz2)$/i, /\.[a-z0-9]{1,5}$/i]

export function splitName(name: string): { head: string; tail: string } {
  if (name.length > 16) {
    for (const re of TAIL_PATTERNS) {
      const m = re.exec(name)
      if (m && m.index > 0) {
        return { head: name.slice(0, m.index), tail: name.slice(m.index) }
      }
    }
  }
  return { head: name, tail: '' }
}
