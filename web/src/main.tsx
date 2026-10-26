import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import { PreferencesMotion } from './utils/preferences';
import './index.css';
import App from './App';

// reducedMotion="user" is the global motion kill-switch: every framer
// animation in the app (including the ones in src/motion.tsx and charts.tsx)
// collapses to a no-op transform when the OS asks for reduced motion. The CSS
// keyframes are covered by the matching media query in index.css. An operator
// who set the OS preference must never have to hunt for an in-app toggle.
createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <PreferencesMotion>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </PreferencesMotion>
  </StrictMode>,
);
