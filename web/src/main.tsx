import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { App } from './App'
import { AppConfirmProvider } from './components/ui/appConfirm'
import { hydrateConfig } from './config'
import { TimeRangeProvider, isLive } from './timeRange'
import './index.css'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // Live-vs-fixed contract: trailing-window presets stream;
      // absolute ranges freeze. The function form is evaluated each
      // time TanStack schedules an interval, so a brush (which flips
      // the range to absolute and the live-mode flag to false) takes
      // effect on the next evaluation. Per-query overrides should
      // wrap their cadence in useLiveInterval(ms) for the same
      // behaviour with React-reactive updates.
      refetchInterval: () => (isLive() ? 2000 : false),
      refetchOnWindowFocus: () => isLive(),
      retry: 1,
      staleTime: 0,
    },
  },
})

export { queryClient }

// Hydrate effective config (display name, default theme, default
// time range) before mounting so the first paint matches the
// admin-configured defaults. The hydrator has its own timeout — if
// the api is slow, fall through to hard-coded defaults.
hydrateConfig().finally(() => {
  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <TimeRangeProvider>
          <AppConfirmProvider>
            <App />
          </AppConfirmProvider>
        </TimeRangeProvider>
      </QueryClientProvider>
    </StrictMode>,
  )
})
