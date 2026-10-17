// Mirror of internal/extract's IsFirstPart: the file extraction starts
// from — part 1 of a multi-part RAR set, a plain .rar, or a zip/tar
// archive (single file). Keep in sync with detect.go.

const partNN = /^(.+)\.part(\d+)\.rar$/i
const plainRar = /^(.+)\.rar$/i
const singleFile = /\.(zip|tar\.gz|tgz|tar\.bz2|tbz2)$/i

export function isFirstArchivePart(filename: string): boolean {
  const m = partNN.exec(filename)
  if (m) return Number(m[2]) === 1
  return plainRar.test(filename) || singleFile.test(filename)
}
