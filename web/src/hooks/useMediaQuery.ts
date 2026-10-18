import { useEffect, useState } from 'react'

// NARROW_SCREEN matches the phone layout's breakpoint in styles.css.
export const NARROW_SCREEN = '(max-width: 760px)'

// useMediaQuery reports whether a CSS media query matches, updating when
// it changes (rotating the phone, resizing the window). For layout that
// CSS alone can't express, e.g. rendering a different structure.
export function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState(() => window.matchMedia(query).matches)
  useEffect(() => {
    const mq = window.matchMedia(query)
    const onChange = () => setMatches(mq.matches)
    onChange()
    mq.addEventListener('change', onChange)
    return () => mq.removeEventListener('change', onChange)
  }, [query])
  return matches
}
