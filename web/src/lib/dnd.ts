import { createElement } from 'react'
import { flushSync } from 'react-dom'
import { createRoot, type Root } from 'react-dom/client'
import { FilesIcon } from '@phosphor-icons/react'

// Drag-and-drop, both directions: the preview shown while entries are dragged
// around inside the app, and reading a folder dropped onto it.
//
// A folder picked through <input webkitdirectory> triggers the browser's own
// "upload everything from this folder?" confirmation, which a page cannot style
// or suppress. A folder dropped onto the page does not: the entries API lets us
// walk it directly. This is the difference that makes drag-and-drop the calmer
// way to bring a tree in.

/** DroppedFile is one file pulled out of a drop, with its path relative to what
 *  was dropped — so a folder's shape can be recreated on the server. */
export type DroppedFile = { file: File; path: string }

/**
 * dropEntries pulls the filesystem entries out of a drop.
 *
 * It must run synchronously inside the drop handler: a DataTransfer's items are
 * only valid while the event is being dispatched, so the entries are captured
 * now and walked later.
 */
export function dropEntries(dt: DataTransfer): FileSystemEntry[] {
  const items = dt.items ? Array.from(dt.items) : []
  return items
    .filter((item) => item.kind === 'file')
    .map((item) => item.webkitGetAsEntry?.() ?? null)
    .filter((entry): entry is FileSystemEntry => entry !== null)
}

/** readEntries walks the captured entries, descending into folders, and returns
 *  every file with its relative path. */
export async function readEntries(entries: FileSystemEntry[]): Promise<DroppedFile[]> {
  const out: DroppedFile[] = []
  for (const entry of entries) await walkEntry(entry, '', out)
  return out
}

async function walkEntry(entry: FileSystemEntry, prefix: string, out: DroppedFile[]): Promise<void> {
  if (entry.isFile) {
    out.push({ file: await fileOf(entry as FileSystemFileEntry), path: prefix + entry.name })
    return
  }
  if (entry.isDirectory) {
    const children = await drain((entry as FileSystemDirectoryEntry).createReader())
    for (const child of children) await walkEntry(child, prefix + entry.name + '/', out)
  }
}

function fileOf(entry: FileSystemFileEntry): Promise<File> {
  return new Promise((resolve, reject) => entry.file(resolve, reject))
}

/** drain reads a directory fully: readEntries returns entries in batches and
 *  must be called again until it yields an empty one. */
function drain(reader: FileSystemDirectoryReader): Promise<FileSystemEntry[]> {
  const all: FileSystemEntry[] = []
  return new Promise((resolve, reject) => {
    const step = () =>
      reader.readEntries((batch) => {
        if (batch.length === 0) {
          resolve(all)
          return
        }
        all.push(...batch)
        step()
      }, reject)
    step()
  })
}

// The drag preview.
//
// When several entries are dragged at once, the browser's default preview
// repeats whichever row the pointer happened to start on and says nothing
// about the rest of the selection, so a batch looks exactly like one file.
// The preview below is instead the tile the browser would have photographed —
// same card, same square icon over a caption, in its selected colours — with
// two more cards fanned behind it, and the caption counting the selection
// instead of naming one file.

/** iconSurface is the glyph's own rendering surface: the preview lives in
 *  plain DOM, so the icon has to become a markup string rather than an element.
 *  One detached root is kept and flushed synchronously — this gets the markup
 *  inside the dragstart handler without pulling react-dom's server renderer
 *  into the bundle for a single icon. */
let iconSurface: { host: HTMLElement; root: Root } | null = null

function iconSurfaceOf(): { host: HTMLElement; root: Root } {
  if (!iconSurface) {
    const host = document.createElement('div')
    iconSurface = { host, root: createRoot(host) }
  }
  return iconSurface
}

/** filesIcon renders Phosphor's Files icon — the app's own icon set — at the
 *  pixel size it is given. Nothing is copied into this file: the artwork comes
 *  straight from the component. `regular` is the weight the listing draws with
 *  (duotone belongs to the sidebar alone), so the preview matches the tiles it
 *  stands in for. */
function filesIcon(px: number): string {
  const { host, root } = iconSurfaceOf()
  flushSync(() => root.render(createElement(FilesIcon, { size: px, weight: 'regular', 'aria-hidden': true })))
  return host.innerHTML
}

/** iconPx reads a Tailwind size-* class the way the grid's icon sizes work:
 *  size-8/12/16 are 32/48/64px (0.25rem per step, 16px root). Unrecognised
 *  classes fall back to the medium tile's 48px. */
function iconPx(sizeClass: string): number {
  const n = Number(/^size-(\d+(?:\.\d+)?)$/.exec(sizeClass)?.[1])
  return Number.isFinite(n) && n > 0 ? n * 4 : 48
}

/** CARD_PAD is the room kept around the card so the cards fanned behind it are
 *  not clipped by the bounds the browser photographs. */
const CARD_PAD = 20

/** cardSheet is one of the cards behind the one being dragged: the same box,
 *  border and radius as the tile, a little askew — a stack of the preview
 *  itself rather than a separate graphic. The inset matches the wrapper's
 *  padding on every side, because an absolutely positioned box is sized
 *  against the padding box: left/top only would make the sheet 20px larger
 *  than the card it copies. */
function cardSheet(transform: string): HTMLElement {
  const sheet = document.createElement('div')
  sheet.style.cssText =
    `position:absolute;inset:${CARD_PAD}px;border:1px solid ` +
    'color-mix(in srgb, var(--primary) 40%, var(--border));border-radius:10px;' +
    `background:color-mix(in srgb, var(--primary) 10%, var(--background));transform:${transform}`
  return sheet
}

/** dragGhost is the off-screen node the browser photographs for the preview.
 *  It must be in the document when setDragImage is called, and the browser
 *  takes its snapshot a moment later, so it is removed on dragend rather than
 *  immediately — and a new drag clears any that outlived one. */
let dragGhost: HTMLElement | null = null

/** clearDragGhost drops the preview node, once the drag it belongs to is over. */
export function clearDragGhost() {
  dragGhost?.remove()
  dragGhost = null
}

/**
 * installDragGhost sets the preview for a drag carrying several entries: the
 * grid tile's own shape — square icon over a caption, in the selected colours
 * — stacked twice behind, with the caption reading "N items" where a single
 * tile shows its file name. `tile` is the grid's current icon size (Small,
 * Medium, Large), so the card and its glyph are exactly the size of the tiles
 * being dragged. `row` switches to the list view's shape: a small horizontal
 * bar, rounded like the grid drag. A single entry is left alone, where the
 * browser's own preview is already the right picture.
 *
 * Colours come from the theme's variables, so the ghost follows light and dark
 * mode without the page having to tell it which is in use.
 */
export function installDragGhost(
  dt: DataTransfer,
  count: number,
  tile: { min: string; icon: string },
  variant: 'tile' | 'row' = 'tile',
) {
  if (count < 2) return
  clearDragGhost()

  const ghost = document.createElement('div')
  ghost.style.cssText =
    `position:fixed;left:-10000px;top:0;padding:${CARD_PAD}px;pointer-events:none;user-select:none`
  ghost.append(
    cardSheet('translate(-6px, 4px) rotate(-6deg)'),
    cardSheet('translate(-3px, 2px) rotate(-3deg)'),
  )

  const card = document.createElement('div')
  const shell =
    'border:1px solid color-mix(in srgb, var(--primary) 40%, var(--border));border-radius:10px;' +
    'background:color-mix(in srgb, var(--primary) 10%, var(--background));' +
    'font-family:var(--font-sans);box-shadow:0 1px 2px rgb(0 0 0 / 0.12)'
  const label = document.createElement('span')
  label.style.cssText = 'font-size:14px;line-height:20px;color:var(--foreground)'
  label.textContent = `${count} items`

  if (variant === 'row') {
    // A list row is a horizontal bar: icon on the left, text beside it. The
    // fallback keeps that shape but shrinks it and rounds it like the grid
    // drag — rows have no square icon panel of their own.
    card.style.cssText =
      `position:relative;display:flex;align-items:center;gap:8px;padding:8px 12px;white-space:nowrap;${shell}`
    const glyph = document.createElement('span')
    glyph.style.cssText = 'display:grid;place-items:center;color:var(--muted-foreground)'
    glyph.innerHTML = filesIcon(iconPx('size-5'))
    card.append(glyph, label)
  } else {
    card.style.cssText =
      `position:relative;display:flex;flex-direction:column;gap:8px;width:${tile.min};padding:8px;${shell}`
    // The tile centres its caption under the icon panel and wraps long names.
    label.style.textAlign = 'center'
    label.style.overflowWrap = 'anywhere'
    const iconArea = document.createElement('div')
    iconArea.style.cssText =
      'display:grid;place-items:center;aspect-ratio:1;width:100%;border-radius:8px;' +
      'background:color-mix(in srgb, var(--muted) 60%, transparent);color:var(--muted-foreground)'
    iconArea.innerHTML = filesIcon(iconPx(tile.icon))
    card.append(iconArea, label)
  }
  ghost.append(card)
  document.body.appendChild(ghost)
  dragGhost = ghost
  // The cursor rides the middle of the top card, so the stack trails behind it.
  dt.setDragImage(
    ghost,
    Math.round(card.offsetLeft + card.offsetWidth / 2),
    Math.round(card.offsetTop + card.offsetHeight / 2),
  )
}
