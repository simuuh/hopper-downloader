import { useEffect, useRef, useState } from 'react'
import { getToken } from '../lib/api'
import type { QueueEvent } from '../lib/types'

export type ConnectionState = 'connecting' | 'open' | 'closed'

/**
 * Subscribes to /ws and invokes onEvent for every queue.Event received,
 * reconnecting with a fixed backoff if the connection drops. Returns the
 * current connection state for a status indicator.
 *
 * `enabled` must flip to true only once a token actually exists (i.e.
 * after login): the effect re-runs whenever it changes, but a stale
 * closure would otherwise keep reusing the token captured at mount, which
 * is null before the user has logged in.
 *
 * `onOpen`, if given, fires on every successful connection — including a
 * reconnect after a dropped socket, not just the very first one — so a
 * caller can force a full state refresh (GET /api/v1/queue) to catch
 * whatever changed while disconnected. Queue item events stream live once
 * connected, but nothing replays what was missed during the gap itself
 * (e.g. an item's extract_state moving to extract_failed because hopperd
 * restarted mid-extraction while this client was reconnecting).
 */
export function useQueueSocket(
  enabled: boolean,
  onEvent: (ev: QueueEvent) => void,
  onOpen?: () => void,
): ConnectionState {
  const [state, setState] = useState<ConnectionState>('connecting')
  const onEventRef = useRef(onEvent)
  const onOpenRef = useRef(onOpen)
  useEffect(() => {
    onEventRef.current = onEvent
    onOpenRef.current = onOpen
  })

  useEffect(() => {
    const token = enabled ? getToken() : null
    if (!token) return

    let closedByUs = false
    let ws: WebSocket | null = null
    let retryTimer: ReturnType<typeof setTimeout> | undefined

    function connect() {
      const proto = window.location.protocol === 'https:' ? 'wss' : 'ws'
      // The token travels in Sec-WebSocket-Protocol, not the URL, so it never
      // shows up in reverse-proxy access logs (see api/auth.go wsAuth).
      ws = new WebSocket(`${proto}://${window.location.host}/ws`, ['hopper.bearer', token!])

      ws.onopen = () => {
        setState('open')
        onOpenRef.current?.()
      }
      ws.onmessage = (msg) => {
        try {
          const ev = JSON.parse(msg.data as string) as QueueEvent
          onEventRef.current(ev)
        } catch {
          // ignore malformed frames
        }
      }
      ws.onclose = () => {
        setState('closed')
        if (!closedByUs) {
          retryTimer = setTimeout(() => {
            setState('connecting')
            connect()
          }, 2000)
        }
      }
      ws.onerror = () => ws?.close()
    }
    connect()

    return () => {
      closedByUs = true
      if (retryTimer) clearTimeout(retryTimer)
      ws?.close()
    }
  }, [enabled])

  return enabled && getToken() ? state : 'closed'
}
