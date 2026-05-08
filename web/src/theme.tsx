import { useEffect, useState, type ReactNode } from 'react'

export type ThemePref = 'light' | 'dark' | 'system'
export type ResolvedTheme = 'light' | 'dark'

const STORAGE_KEY = 'flowscope-theme'

function readPref(): ThemePref {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    if (v === 'light' || v === 'dark' || v === 'system') return v
  } catch {
    // localStorage unavailable — fall through
  }
  return 'system'
}

function resolve(pref: ThemePref): ResolvedTheme {
  if (pref === 'system') {
    return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark'
  }
  return pref
}

export function useTheme() {
  const [pref, setPrefState] = useState<ThemePref>(readPref)
  const [resolved, setResolved] = useState<ResolvedTheme>(() => resolve(readPref()))

  useEffect(() => {
    const next = resolve(pref)
    setResolved(next)
    document.documentElement.setAttribute('data-theme', next)
    try {
      localStorage.setItem(STORAGE_KEY, pref)
    } catch {
      // ignore — prefs won't persist across reloads
    }
    if (pref !== 'system') return
    const mq = window.matchMedia('(prefers-color-scheme: light)')
    const onChange = () => {
      const r = mq.matches ? 'light' : 'dark'
      setResolved(r)
      document.documentElement.setAttribute('data-theme', r)
    }
    mq.addEventListener('change', onChange)
    return () => mq.removeEventListener('change', onChange)
  }, [pref])

  return { pref, setPref: setPrefState, resolved }
}

export function ThemeToggle() {
  const { pref, setPref } = useTheme()
  const opts: Array<{ id: ThemePref; label: string; icon: ReactNode }> = [
    { id: 'light', label: 'Light', icon: <SunIcon /> },
    { id: 'dark', label: 'Dark', icon: <MoonIcon /> },
    { id: 'system', label: 'System', icon: <MonitorIcon /> },
  ]
  return (
    <div
      role="radiogroup"
      aria-label="Theme"
      className="flex items-center border border-line"
    >
      {opts.map((o) => {
        const selected = pref === o.id
        return (
          <button
            key={o.id}
            role="radio"
            aria-checked={selected}
            aria-label={o.label}
            title={o.label}
            onClick={() => setPref(o.id)}
            className={`flex items-center justify-center w-7 h-6 transition-colors ${
              selected
                ? 'bg-accent-wash text-accent'
                : 'text-faint hover:text-text hover:bg-surface'
            }`}
          >
            {o.icon}
          </button>
        )
      })}
    </div>
  )
}

function SunIcon() {
  return (
    <svg
      width="13"
      height="13"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
    >
      <circle cx="12" cy="12" r="4" />
      <path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M6.34 17.66l-1.41 1.41M19.07 4.93l-1.41 1.41" />
    </svg>
  )
}

function MoonIcon() {
  return (
    <svg
      width="13"
      height="13"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
    >
      <path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z" />
    </svg>
  )
}

function MonitorIcon() {
  return (
    <svg
      width="13"
      height="13"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
    >
      <rect x="2.5" y="3.5" width="19" height="13" rx="1.5" />
      <path d="M8 21h8M12 17v4" />
    </svg>
  )
}
