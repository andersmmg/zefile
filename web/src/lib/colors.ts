export type ColorPreset = {
  label: string
  hex: string
  panelClasses: string
  frameClasses: string
}

export const COLORS: Record<
  string,
  ColorPreset
> = {
  red: {
    label: "Red",
    hex: "#ef4444",
    panelClasses: "bg-red-100 text-red-600 dark:bg-red-950 dark:text-red-400",
    frameClasses: "outline-solid outline-red-100 dark:outline-red-950",
  },
  orange: {
    label: "Orange",
    hex: "#f97316",
    panelClasses: "bg-orange-100 text-orange-600 dark:bg-orange-950 dark:text-orange-400",
    frameClasses: "outline-solid outline-orange-100 dark:outline-orange-950",
  },
  yellow: {
    label: "Yellow",
    hex: "#eab308",
    panelClasses: "bg-yellow-100 text-yellow-600 dark:bg-yellow-950 dark:text-yellow-400",
    frameClasses: "outline-solid outline-yellow-100 dark:outline-yellow-950",
  },
  green: {
    label: "Green",
    hex: "#22c55e",
    panelClasses: "bg-emerald-100 text-emerald-600 dark:bg-emerald-950 dark:text-emerald-400",
    frameClasses: "outline-solid outline-emerald-100 dark:outline-emerald-950",
  },
  teal: {
    label: "Teal",
    hex: "#14b8a6",
    panelClasses: "bg-teal-100 text-teal-600 dark:bg-teal-950 dark:text-teal-400",
    frameClasses: "outline-solid outline-teal-100 dark:outline-teal-950",
  },
  cyan: {
    label: "Cyan",
    hex: "#06b6d4",
    panelClasses: "bg-cyan-100 text-cyan-600 dark:bg-cyan-950 dark:text-cyan-400",
    frameClasses: "outline-solid outline-cyan-100 dark:outline-cyan-950",
  },
  blue: {
    label: "Blue",
    hex: "#3b82f6",
    panelClasses: "bg-blue-100 text-blue-600 dark:bg-blue-950 dark:text-blue-400",
    frameClasses: "outline-solid outline-blue-100 dark:outline-blue-950",
  },
  purple: {
    label: "Purple",
    hex: "#a855f7",
    panelClasses: "bg-purple-100 text-purple-600 dark:bg-purple-950 dark:text-purple-400",
    frameClasses: "outline-solid outline-purple-100 dark:outline-purple-950",
  },
  pink: {
    label: "Pink",
    hex: "#ec4899",
    panelClasses: "bg-pink-100 text-pink-600 dark:bg-pink-950 dark:text-pink-400",
    frameClasses: "outline-solid outline-pink-100 dark:outline-pink-950",
  },
};

export type ColorId = keyof typeof COLORS

function preset(id?: string | null): ColorPreset | undefined {
  return id ? COLORS[id] : undefined
}

export function colorHex(id?: string | null): string | undefined {
  return preset(id)?.hex
}

export function colorLabel(id?: string | null): string {
  return preset(id)?.label ?? 'Colour'
}

export function panelClasses(id?: string | null): string | undefined {
  return preset(id)?.panelClasses;
}

export function frameClasses(id?: string | null): string | undefined {
  return preset(id)?.frameClasses;
}
