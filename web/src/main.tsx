import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'

import { App } from './App'
import { Toaster } from './components/ui/sonner'
import { initTheme } from './lib/theme'
import './index.css'

// Follow the system theme until the user picks one, then honour the choice.
// Applied before the first paint so the page never flashes the wrong theme.
initTheme()

document.addEventListener('contextmenu', (e) => {
  if (!(e.target instanceof Element) || !e.target.closest('[data-allow-context]')) e.preventDefault()
})

const root = document.getElementById('root')
if (!root) throw new Error('the page has no mount point')

createRoot(root).render(
  <StrictMode>
    <App />
    <Toaster />
  </StrictMode>,
)
