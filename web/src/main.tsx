import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { App } from './App'
import { AppConfirmProvider } from './components/ui/appConfirm'
import './index.css'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // FlowScope is a near-real-time tool: aggressive refresh,
      // no stale-while-revalidate cleverness, errors visible.
      refetchInterval: 2000,
      refetchOnWindowFocus: true,
      retry: 1,
      staleTime: 0,
    },
  },
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <AppConfirmProvider>
        <App />
      </AppConfirmProvider>
    </QueryClientProvider>
  </StrictMode>,
)
