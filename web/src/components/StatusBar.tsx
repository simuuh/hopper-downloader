import type { ConnectionState } from '../hooks/useQueueSocket'
import { formatSpeed } from '../lib/format'
import type { QueueItem } from '../lib/types'

export function StatusBar({ items, connection }: { items: QueueItem[]; connection: ConnectionState }) {
  const downloading = items.filter((i) => i.state === 'downloading')
  const totalSpeed = downloading.reduce((sum, i) => sum + i.speed, 0)
  const queued = items.filter((i) => i.state === 'queued').length

  return (
    <div className="status" title={`WebSocket: ${connection}`}>
      <span className="status-conn">
        <span className={`dot ${connection}`} />
        {connection === 'open' ? 'live' : connection}
      </span>
      <span className="status-num">{downloading.length} active</span>
      <span className="status-num">{queued} queued</span>
      <span className="status-num status-speed">{formatSpeed(totalSpeed)}</span>
    </div>
  )
}
