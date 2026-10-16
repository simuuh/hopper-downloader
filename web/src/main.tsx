import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import { captureSharedLinks } from './lib/share'
import { applyTheme } from './lib/theme'
import './styles.css'

applyTheme()
captureSharedLinks()

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
