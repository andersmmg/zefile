import { useState, type ReactNode } from 'react'
import smartcrop from 'smartcrop'

import type { Entry } from '@/api'

const focalPoints = new Map<string, string>()

export function Thumbnail({
  entry,
  className,
  size = 256,
  children,
}: {
  entry: Entry
  className?: string
  size?: number
  children: ReactNode
}) {
  const [loadedSrc, setLoadedSrc] = useState<string | null>(null)
  const [failedSrc, setFailedSrc] = useState<string | null>(null)
  const [focal, setFocal] = useState<{ src: string; pos: string } | null>(null)

  const src = `/api/v1/fs/thumb?path=${encodeURIComponent(entry.path)}&s=${size}`
  const shown = loadedSrc === src
  const failed = failedSrc === src
  const position = focal && focal.src === src ? focal.pos : (focalPoints.get(src) ?? '50% 50%')

  async function locateSubject(img: HTMLImageElement) {
    setLoadedSrc(src)
    if (focalPoints.has(src)) return
    try {
      const { topCrop } = await smartcrop.crop(img, { width: 100, height: 100 })
      const w = img.naturalWidth || 1
      const h = img.naturalHeight || 1
      const clamp = (n: number) => Math.min(100, Math.max(0, n))
      const pos = `${clamp(((topCrop.x + topCrop.width / 2) / w) * 100)}% ${clamp(((topCrop.y + topCrop.height / 2) / h) * 100)}%`
      if (focalPoints.size > 500) focalPoints.clear()
      focalPoints.set(src, pos)
      setFocal({ src, pos })
    } catch {
      // Center crop stands.
    }
  }

  // The image stays mounted with opacity so lazy loading still fetches it;
  // display:none would prevent onLoad from ever firing.
  return (
    <div className={`relative overflow-hidden ${className ?? ''}`}>
      {!shown && <div className="absolute inset-0 grid place-items-center">{children}</div>}
      {!failed && (
        <img
          src={src}
          alt=""
          loading="lazy"
          decoding="async"
          draggable={false}
          onLoad={(e) => void locateSubject(e.currentTarget)}
          onError={() => setFailedSrc(src)}
          style={{ objectPosition: position }}
          className={`absolute inset-0 h-full w-full object-cover transition-opacity ${
            shown ? 'opacity-100' : 'opacity-0'
          }`}
        />
      )}
    </div>
  )
}
