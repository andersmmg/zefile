/** Palette is one selectable colour theme. */
export type Palette = {
  id: string
  label: string
  seed: string
}

export const DEFAULT_PALETTE = 'pine'

export const PALETTES: Palette[] = [
  { id: 'pine', label: 'Pine', seed: '#7fd0bb' }, // an earthy green
  { id: 'green', label: 'Green', seed: '#1aff80' }, // soft green
  { id: 'aqua', label: 'Aqua', seed: '#50d8ed' }, // clear teal
  { id: 'azure', label: 'Azure', seed: '#80a3d0' }, // purplish blue
  { id: 'violet', label: 'Violet', seed: '#a080d0' }, // violet duh
  { id: 'rose', label: 'Rose', seed: '#d08096' }, // pinky rose red
  { id: 'amber', label: 'Amber', seed: '#ffb642' }, // deep yellow <3
]

const STORAGE_KEY = 'zefile-palette'

export function paletteById(id?: string | null): Palette {
  return PALETTES.find((p) => p.id === id) ?? paletteById(DEFAULT_PALETTE)
}

export function currentPaletteId(): string {
  if (typeof localStorage === 'undefined') return DEFAULT_PALETTE
  return localStorage.getItem(STORAGE_KEY) ?? DEFAULT_PALETTE
}

export function applyPalette(id?: string | null): Palette {
  const palette = paletteById(id)
  if (typeof document !== 'undefined') {
    document.documentElement.style.setProperty('--seed', palette.seed)
  }
  if (typeof localStorage !== 'undefined') {
    if (palette.id === DEFAULT_PALETTE) localStorage.removeItem(STORAGE_KEY)
    else localStorage.setItem(STORAGE_KEY, palette.id)
  }
  return palette
}
