import type { Priority, QueueItem } from './types'

// Mirror of internal/queue/order.go (queueOrder): the dispatcher starts
// items in exactly this order, so the queue is displayed in it too —
// top to bottom = start order. Keep the two in sync.

const PRIO: Record<Priority, number> = { low: 0, normal: 1, high: 2 }

const partNN = /^(.+)\.part(\d+)\.rar$/i
const rNN = /^(.+)\.r(\d+)$/i
const plainRar = /^(.+)\.rar$/i

// partOf returns an item's archive set (lowercased base name) and part
// number, like extract.GroupBase/PartNumber.
function partOf(filename: string): { set: string; n: number } | null {
  let m = partNN.exec(filename)
  if (m) return { set: m[1].toLowerCase(), n: Number(m[2]) }
  m = rNN.exec(filename)
  if (m) return { set: m[1].toLowerCase(), n: Number(m[2]) + 2 }
  m = plainRar.exec(filename)
  if (m) return { set: m[1].toLowerCase(), n: 1 }
  return null
}

export function groupKeyOf(item: QueueItem): string {
  return item.package_id ? item.package_id : `item:${item.id}`
}

// sortQueue returns items in queue order: groups (packages) by their
// highest unfinished priority, then where they were first queued; within
// a group by priority, archive part number, position.
export function sortQueue(items: QueueItem[]): QueueItem[] {
  const rank = new Map<string, number>()
  const openPrio = new Map<string, number>()
  const allPrio = new Map<string, number>()
  for (const it of items) {
    const g = groupKeyOf(it)
    rank.set(g, Math.min(rank.get(g) ?? Infinity, it.position))
    allPrio.set(g, Math.max(allPrio.get(g) ?? -1, PRIO[it.priority]))
    if (it.state !== 'done') openPrio.set(g, Math.max(openPrio.get(g) ?? -1, PRIO[it.priority]))
  }
  const groupPrio = (g: string) => openPrio.get(g) ?? allPrio.get(g) ?? 0

  return [...items].sort((a, b) => {
    const ga = groupKeyOf(a)
    const gb = groupKeyOf(b)
    if (ga !== gb) {
      const pd = groupPrio(gb) - groupPrio(ga)
      if (pd !== 0) return pd
      const rd = (rank.get(ga) ?? 0) - (rank.get(gb) ?? 0)
      if (rd !== 0) return rd
      return ga < gb ? -1 : 1
    }
    const pd = PRIO[b.priority] - PRIO[a.priority]
    if (pd !== 0) return pd
    const pa = a.filename ? partOf(a.filename) : null
    const pb = b.filename ? partOf(b.filename) : null
    if (pa && pb && pa.set === pb.set && pa.n !== pb.n) return pa.n - pb.n
    if (a.position !== b.position) return a.position - b.position
    return a.created_at < b.created_at ? -1 : a.created_at > b.created_at ? 1 : 0
  })
}
