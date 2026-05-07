import { Overview } from './components/Overview'

// Single-page shell for slice 8. Tabs (Flows, Devices, Alerts) and
// routing land in follow-up slices.
export function App() {
  return (
    <div className="min-h-screen flex flex-col">
      <Topbar />
      <main className="flex-1 px-7 py-6 max-w-[1280px] w-full mx-auto">
        <Overview />
      </main>
      <Footer />
    </div>
  )
}

function Topbar() {
  return (
    <header className="flex items-center gap-6 px-7 h-14 bg-surf border-b border-line">
      <div className="font-bold tracking-tight text-[15px]">
        FlowScope<span className="text-accent">.</span>
      </div>
      <nav className="flex items-center gap-5 text-[12px] uppercase tracking-[0.16em] font-mono text-dim">
        <span className="text-text border-b-2 border-accent pb-3 -mb-3">Overview</span>
        <span className="opacity-50">Flows</span>
        <span className="opacity-50">Devices</span>
        <span className="opacity-50">Alerts</span>
      </nav>
      <div className="ml-auto flex items-center gap-4 text-[11px] font-mono text-dim">
        <span className="flex items-center gap-2">
          <span className="w-1.5 h-1.5 rounded-full bg-ok animate-pulse" />
          <span className="text-text">live</span>
        </span>
        <span>FlowScope v1</span>
      </div>
    </header>
  )
}

function Footer() {
  return (
    <footer className="px-7 py-4 border-t border-line text-[11px] font-mono text-faint flex justify-between">
      <span>FlowScope · v1 · React</span>
      <span className="space-x-3">
        <a className="text-accent hover:underline" href="/api/summary">/api/summary</a>
        <a className="text-accent hover:underline" href="/api/interfaces">/api/interfaces</a>
        <a className="text-accent hover:underline" href="/metrics">/metrics</a>
      </span>
    </footer>
  )
}
