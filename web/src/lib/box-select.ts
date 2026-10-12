import { useCallback, useRef, useState, type MouseEvent as ReactMouseEvent } from 'react'

// data-control marks a bare control outside any button
export const INTERACTIVE_SELECTOR = '[data-entry-path], button, a, input, textarea, select, [data-control]'

/** how far a press must travel before it becomes a box */
const MIN_MOVE = 4

/** minimum hold time for the box to stay */
const SWALLOW_MS = 300

/** fade-out time after release */
export const FADE_MS = 150

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
  fading: boolean
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

/** wires box selection onto a container */
export function useBoxSelect({ snapshot, apply }: Options): BoxSelect {
  const opts = useRef({ snapshot, apply })
  opts.current = { snapshot, apply }

  const [rect, setRect] = useState<BoxRect | null>(null)
  const [fading, setFading] = useState(false)
  const drag = useRef<Drag | null>(null)
  const swallowUntil = useRef(0)
  const fadeTimer = useRef(0)

  const paint = useCallback((d: Drag) => {
    const box: Box = {
      left: Math.min(d.startX, d.x),
      top: Math.min(d.startY, d.y),
      right: Math.max(d.startX, d.x),
      bottom: Math.max(d.startY, d.y),
    }
    // drawn clipped to the browse area so it never paints over the sidebar/header
    const area = d.container.getBoundingClientRect()
    const vis: Box = {
      left: Math.max(box.left, area.left),
      top: Math.max(box.top, area.top),
      right: Math.min(box.right, area.right),
      bottom: Math.min(box.bottom, area.bottom),
    }
    if (vis.left < vis.right && vis.top < vis.bottom)
      setRect({ left: vis.left, top: vis.top, width: vis.right - vis.left, height: vis.bottom - vis.top })
    else setRect(null)
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
    paint(d)
    // keep the last rect mounted and let it fade out rather than vanishing
    setFading(true)
    fadeTimer.current = window.setTimeout(() => {
      setRect(null)
      setFading(false)
      fadeTimer.current = 0
    }, FADE_MS)
    swallowUntil.current = Date.now() + SWALLOW_MS
  }, [move, paint])

  const onMouseDown = useCallback(
    (e: ReactMouseEvent) => {
      if (e.button !== 0) return
      // a press during the fade cancels it
      if (fadeTimer.current) {
        window.clearTimeout(fadeTimer.current)
        fadeTimer.current = 0
        setRect(null)
        setFading(false)
      }
      const target = e.target instanceof Element ? e.target : null
      if (target?.closest(INTERACTIVE_SELECTOR)) return
      const container = e.currentTarget
      if (!(container instanceof HTMLElement)) return
      // scrollbars dispatch to the scroller itself, so test the gutter all the way up
      for (let el: Element | null = target; el; el = el.parentElement) {
        const r = el.getBoundingClientRect()
        if (e.clientX - r.left >= el.clientWidth || e.clientY - r.top >= el.clientHeight) return
        if (el === container) break
      }
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

  return { rect, fading, onMouseDown, click }
}
