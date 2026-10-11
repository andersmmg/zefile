import { useCallback, useRef, useState, type MouseEvent as ReactMouseEvent } from 'react'

export const INTERACTIVE_SELECTOR = '[data-entry-path], button, a, input, textarea, select'

/** how far a press must travel before it becomes a box */
const MIN_MOVE = 4

/** minimum hold time for the box to stay */
const SWALLOW_MS = 300

export type BoxRect = { left: number; top: number; width: number; height: number }

type Box = { left: number; top: number; right: number; bottom: number }

type Drag = {
  startX: number
  startY: number
  x: number
  y: number
  container: HTMLElement
  base: string[]
  active: boolean
  frame: number
}

type Options = {
  /** selection as it stands when starting */
  snapshot: () => string[]
  apply: (paths: string[]) => void
}

export type BoxSelect = {
  rect: BoxRect | null
  onMouseDown: (e: ReactMouseEvent) => void
  click: (then: () => void) => void
}

function touched(container: HTMLElement, box: Box): string[] {
  const out: string[] = []
  for (const el of container.querySelectorAll('[data-entry-path]')) {
    const path = el.getAttribute('data-entry-path')
    if (!path) continue
    const r = el.getBoundingClientRect()
    if (r.left <= box.right && r.right >= box.left && r.top <= box.bottom && r.bottom >= box.top) {
      out.push(path)
    }
  }
  return out
}

/**
 * wires box selection onto a listing container
 */
export function useBoxSelect({ snapshot, apply }: Options): BoxSelect {
  const opts = useRef({ snapshot, apply })
  opts.current = { snapshot, apply }

  const [rect, setRect] = useState<BoxRect | null>(null)
  const drag = useRef<Drag | null>(null)
  const swallowUntil = useRef(0)

  const paint = useCallback((d: Drag) => {
    const box: Box = {
      left: Math.min(d.startX, d.x),
      top: Math.min(d.startY, d.y),
      right: Math.max(d.startX, d.x),
      bottom: Math.max(d.startY, d.y),
    }
    setRect({ left: box.left, top: box.top, width: box.right - box.left, height: box.bottom - box.top })
    opts.current.apply([...d.base, ...touched(d.container, box)])
  }, [])

  const move = useCallback(
    (e: globalThis.MouseEvent) => {
      const d = drag.current
      if (!d) return
      d.x = e.clientX
      d.y = e.clientY
      if (!d.active && Math.hypot(d.x - d.startX, d.y - d.startY) < MIN_MOVE) return
      d.active = true
      if (d.frame) return
      d.frame = requestAnimationFrame(() => {
        d.frame = 0
        const cur = drag.current
        if (cur) paint(cur)
      })
    },
    [paint],
  )

  const end = useCallback(() => {
    const d = drag.current
    if (!d) return
    window.removeEventListener('mousemove', move)
    window.removeEventListener('mouseup', end)
    window.removeEventListener('blur', end)
    if (d.frame) cancelAnimationFrame(d.frame)
    d.frame = 0
    drag.current = null
    if (!d.active) return
    paint(d) // update the box
    setRect(null)
    swallowUntil.current = Date.now() + SWALLOW_MS
  }, [move, paint])

  const onMouseDown = useCallback(
    (e: ReactMouseEvent) => {
      if (e.button !== 0) return
      const target = e.target instanceof Element ? e.target : null
      if (target?.closest(INTERACTIVE_SELECTOR)) return
      const container = e.currentTarget
      if (!(container instanceof HTMLElement)) return
      // ignore scrollbar etc of browser
      const area = container.getBoundingClientRect()
      if (e.clientX - area.left >= container.clientWidth || e.clientY - area.top >= container.clientHeight) return
      e.preventDefault()
      drag.current = {
        startX: e.clientX,
        startY: e.clientY,
        x: e.clientX,
        y: e.clientY,
        container,
        base: e.shiftKey ? opts.current.snapshot() : [],
        active: false,
        frame: 0,
      }
      window.addEventListener('mousemove', move)
      window.addEventListener('mouseup', end)
      window.addEventListener('blur', end)
    },
    [move, end],
  )

  const click = useCallback((then: () => void) => {
    if (Date.now() < swallowUntil.current) {
      swallowUntil.current = 0
      return
    }
    then()
  }, [])

  return { rect, onMouseDown, click }
}
