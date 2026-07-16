import { StrictMode, Suspense } from 'react';
import { createRoot } from 'react-dom/client';
import { RouterProvider } from 'react-router-dom';
import { router } from './App';
import { AuthProvider } from './components/auth';
import './index.css';

// After a new deploy, any still-open tab's old index.html references stale hashed chunk names.
// Navigating to a lazy route that tries to fetch a missing chunk fails with vite:preloadError.
// Auto-reload once to grab the latest index (backend sends no-cache for index.html), then bail
// so a genuinely missing chunk doesn't spin forever.
window.addEventListener('vite:preloadError', () => {
  const KEY = 'vite:preloadError:lastReload';
  const now = Date.now();
  if (now - Number(sessionStorage.getItem(KEY) || '0') > 10_000) {
    sessionStorage.setItem(KEY, String(now));
    window.location.reload();
  }
});

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <AuthProvider>
      <Suspense
        fallback={
          // The root fallback: amber arc on the app background, matching the
          // one in RequireAuth so the boot sequence never changes character.
          <div className="grid min-h-screen place-items-center bg-slate-950">
            <span
              role="status"
              aria-label="Loading"
              className="inline-block size-5 animate-spin rounded-full border-2 border-slate-800 border-t-brand"
            />
          </div>
        }
      >
        <RouterProvider router={router} />
      </Suspense>
    </AuthProvider>
  </StrictMode>,
);
